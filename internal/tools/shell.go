package tools

import (
	"context"
	"fmt"
	"os/exec"
	"runtime"
	"strings"
	"time"
	"unicode/utf8"
)

// ShellFamily 命令方言族。模型必须知道这个才能写对命令——
// 同样是「列目录」，posix 是 ls、cmd 是 dir、powershell 是 Get-ChildItem。
type ShellFamily string

const (
	FamilyPOSIX      ShellFamily = "posix"
	FamilyCmd        ShellFamily = "cmd"
	FamilyPowerShell ShellFamily = "powershell"
)

// ShellSpec 一个候选 shell：可执行文件 + 固定前置参数。
//
//	{"bin": "bash", "args": ["-c"]}
//
// 为什么不把 shell 写死：机器上有什么是环境事实，不是程序能假定的事。
// 但「探测到什么就用什么」也不够——候选之间是**优先级关系**，
// 且探测不能只看文件在不在：WSL 的 bash stub、被截断的二进制、
// 权限不对的 sh 都存在于 PATH 却跑不起来。
type ShellSpec struct {
	Bin    string      `json:"bin"`
	Args   []string    `json:"args"`
	Family ShellFamily `json:"family,omitempty"`
}

// String 便于错误信息与诊断展示。
func (s ShellSpec) String() string {
	if len(s.Args) == 0 {
		return s.Bin
	}
	return s.Bin + " " + strings.Join(s.Args, " ")
}

// familyOf 从可执行文件名推断方言族；显式指定优先。
func familyOf(bin string) ShellFamily {
	// 不能只用 filepath.Base：它按当前平台的分隔符切分，
	// 在 Linux 上拿到 "C:\Windows\System32\cmd.exe" 会原样返回整串，
	// 于是 Windows 路径被判成 POSIX。配置常常是跨平台共用的，两种分隔符都要认。
	base := strings.ToLower(bin)
	if i := strings.LastIndexAny(base, `/\`); i >= 0 {
		base = base[i+1:]
	}
	base = strings.TrimSuffix(base, ".exe")
	switch base {
	case "cmd":
		return FamilyCmd
	case "powershell", "pwsh":
		return FamilyPowerShell
	}
	return FamilyPOSIX
}

// withFamily 补齐 Family 字段。
func (s ShellSpec) withFamily() ShellSpec {
	if s.Family == "" {
		s.Family = familyOf(s.Bin)
	}
	return s
}

// normalizeOutput 把子进程输出规整成合法 UTF-8。
// 已经是 UTF-8 就原样返回；否则按平台本地编码解码（Windows 走 CP936）。
func normalizeOutput(s string) string {
	if s == "" || utf8.ValidString(s) {
		return s
	}
	return string(decodeConsole([]byte(s)))
}

// defaultShellChain 平台默认候选链（按优先级从高到低）。
//
// 排序依据是「模型最可能写哪种方言」而不是「哪种更古老」：
// 绝大多数命令是 POSIX 语法，所以 POSIX 族全体排前面，且功能更全的
// bash 先于 sh、sh 先于 dash。busybox 单列——精简镜像常只留它，
// 它自己的 sh 能跑 POSIX，但 PATH 里没有 sh 这个文件。
// 非 POSIX 只作为最后兜底，并且一旦选中会告诉模型换语法。
func defaultShellChain() []ShellSpec {
	if runtime.GOOS == "windows" {
		return []ShellSpec{
			{Bin: "bash", Args: []string{"-c"}, Family: FamilyPOSIX},          // Git for Windows
			{Bin: "sh", Args: []string{"-c"}, Family: FamilyPOSIX},            // 别的 POSIX 实现
			{Bin: "busybox", Args: []string{"sh", "-c"}, Family: FamilyPOSIX}, // 精简环境
			{Bin: "pwsh", Args: []string{"-NoProfile", "-NonInteractive", "-Command"}, Family: FamilyPowerShell},
			{Bin: "powershell", Args: []string{"-NoProfile", "-NonInteractive", "-Command"}, Family: FamilyPowerShell},
			{Bin: "cmd", Args: []string{"/c"}, Family: FamilyCmd},
		}
	}
	return []ShellSpec{
		{Bin: "bash", Args: []string{"-c"}, Family: FamilyPOSIX},
		{Bin: "sh", Args: []string{"-c"}, Family: FamilyPOSIX},
		{Bin: "zsh", Args: []string{"-c"}, Family: FamilyPOSIX},
		{Bin: "busybox", Args: []string{"sh", "-c"}, Family: FamilyPOSIX},
		{Bin: "dash", Args: []string{"-c"}, Family: FamilyPOSIX},
		{Bin: "ksh", Args: []string{"-c"}, Family: FamilyPOSIX},
		{Bin: "ash", Args: []string{"-c"}, Family: FamilyPOSIX},
	}
}

// knownArgs 给只写了名字的候选补默认参数。
func knownArgs(bin string) []string {
	switch familyOf(bin) {
	case FamilyCmd:
		return []string{"/c"}
	case FamilyPowerShell:
		return []string{"-NoProfile", "-NonInteractive", "-Command"}
	default:
		return []string{"-c"}
	}
}

// parseShellConfig 解析 plugins_cfg.tools.shell。
//
// 接受的形态：
//
//	缺省 / "auto"        → 平台默认链（探测优先，逐个验证）
//	"bash"               → 单元素链（严格：找不到/跑不通就报错）
//	["bash","sh","cmd"]  → 按序探测
//	{"bin":"bash","args":["-c"]} → 精确指定
//
// 返回 explicit 表示用户是否显式指定（显式指定时探测失败要硬报错而非退化）。
func parseShellConfig(v any) (chain []ShellSpec, explicit bool, err error) {
	switch t := v.(type) {
	case nil:
		return defaultShellChain(), false, nil
	case string:
		s := strings.TrimSpace(t)
		if s == "" || strings.EqualFold(s, "auto") {
			return defaultShellChain(), false, nil
		}
		return []ShellSpec{{Bin: s, Args: knownArgs(s), Family: familyOf(s)}}, true, nil
	case []any:
		var out []ShellSpec
		for _, item := range t {
			switch it := item.(type) {
			case string:
				if it = strings.TrimSpace(it); it != "" {
					out = append(out, ShellSpec{Bin: it, Args: knownArgs(it), Family: familyOf(it)})
				}
			case map[string]any:
				sp, ok := specFromMap(it)
				if ok {
					out = append(out, sp)
				}
			}
		}
		if len(out) == 0 {
			return nil, false, fmt.Errorf("tools.shell: 空候选链")
		}
		return out, true, nil
	case map[string]any:
		sp, ok := specFromMap(t)
		if !ok {
			return nil, false, fmt.Errorf("tools.shell: 对象形式必须带 bin")
		}
		return []ShellSpec{sp}, true, nil
	default:
		return nil, false, fmt.Errorf("tools.shell: 不支持的配置类型 %T（要 string / []string / object）", v)
	}
}

// specFromMap 解析候选对象。args 缺省时按 bin 补默认参数；
// family 可显式覆盖（例如把 busybox 当 posix 用，默认推断已经如此）。
func specFromMap(m map[string]any) (ShellSpec, bool) {
	bin, _ := m["bin"].(string)
	bin = strings.TrimSpace(bin)
	if bin == "" {
		return ShellSpec{}, false
	}
	sp := ShellSpec{Bin: bin}
	if args, ok := m["args"].([]any); ok {
		for _, a := range args {
			if s, ok := a.(string); ok {
				sp.Args = append(sp.Args, s)
			}
		}
	} else {
		sp.Args = knownArgs(bin)
	}
	if fam, ok := m["family"].(string); ok && fam != "" {
		sp.Family = ShellFamily(strings.ToLower(fam))
	}
	sp.Family = familyOf(sp.Bin).familyOr(sp.Family)
	return sp, true
}

// familyOr 显式 family 优先，否则用推断值。
func (f ShellFamily) familyOr(explicit ShellFamily) ShellFamily {
	if explicit != "" {
		return explicit
	}
	return f
}

// ShellCandidate 一个候选的探测结果，用于诊断（env_info / -tools）。
type ShellCandidate struct {
	Spec   ShellSpec
	Path   string // LookPath 命中的路径，空表示不在 PATH
	OK     bool
	Reason string // 失败原因
}

// probeTimeout 单次探针超时。探针只是 `echo`，正常在毫秒级；
// 给 3 秒是为了容忍冷启动慢的 WSL/pwsh，同时不至于让启动卡很久。
const probeTimeout = 3 * time.Second

// probeShell 真实执行一次探针来确认「这个 shell 能用」。
//
// 只看 LookPath 是不够的：Windows 上 WindowsApps 目录里的 bash.exe 是
// 一个跳转到 Microsoft Store 的 stub，存在、可执行、返回码还可能是 0，
// 但它不会执行你的命令。所以必须跑一条已知输出，验证回显。
func probeShell(ctx context.Context, sp ShellSpec) ShellCandidate {
	c := ShellCandidate{Spec: sp}
	path, err := exec.LookPath(sp.Bin)
	if err != nil {
		c.Reason = "not in PATH"
		return c
	}
	c.Path = path

	probe := "echo gsh-probe-ok"
	if sp.Family == FamilyPowerShell {
		probe = "Write-Output gsh-probe-ok"
	}
	argv := append(append([]string{}, sp.Args...), probe)
	cctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	out, err := exec.CommandContext(cctx, path, argv...).CombinedOutput()
	text := string(out)
	if err != nil {
		if cctx.Err() == context.DeadlineExceeded {
			c.Reason = "probe timeout"
		} else {
			c.Reason = "probe failed: " + firstLine(text)
		}
		return c
	}
	if !strings.Contains(text, "gsh-probe-ok") {
		c.Reason = "probe produced no marker (可能是 stub 或包装脚本)"
		return c
	}
	c.OK = true
	return c
}

// firstLine 取首行非空文本，用于精简错误信息。
func firstLine(s string) string {
	for _, ln := range strings.Split(s, "\n") {
		if ln = strings.TrimSpace(ln); ln != "" {
			if len(ln) > 200 {
				ln = ln[:200]
			}
			return ln
		}
	}
	return "no output"
}

// probeChain 按优先级逐個探测，返回第一个真正可用的与全部探测记录。
func probeChain(ctx context.Context, chain []ShellSpec) (ShellSpec, []ShellCandidate, error) {
	if len(chain) == 0 {
		chain = defaultShellChain()
	}
	cands := make([]ShellCandidate, 0, len(chain))
	for _, raw := range chain {
		sp := raw.withFamily()
		c := probeShell(ctx, sp)
		cands = append(cands, c)
		if c.OK {
			return ShellSpec{Bin: c.Path, Args: sp.Args, Family: sp.Family}, cands, nil
		}
	}
	var tried []string
	for _, c := range cands {
		tried = append(tried, fmt.Sprintf("%s(%s)", c.Spec.Bin, c.Reason))
	}
	return ShellSpec{}, cands, fmt.Errorf(
		"找不到可用的 shell（已尝试：%s）。可在 plugins_cfg.tools.shell 显式指定可执行文件路径；"+
			"不配也能跑——工具会降级为直接执行单个程序（不支持管道/重定向）",
		strings.Join(tried, ", "))
}

// resolveShell 兼容旧调用点：不关心探测明细时用它。
func resolveShell(chain []ShellSpec) (ShellSpec, error) {
	sp, _, err := probeChain(context.Background(), chain)
	return sp, err
}
