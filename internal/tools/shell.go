package tools

import (
	"fmt"
	"os/exec"
	"runtime"
	"strings"
	"unicode/utf8"
)

// ShellSpec 一个候选 shell：可执行文件 + 固定前置参数。
//
//	{"bin": "bash", "args": ["-c"]}
//
// 不把 shell 选择写死在代码里的原因：不同机器上有什么是环境事实，
// 不是程序能假定的事。显式配置优先于探测，探测失败就报错，绝不静默退化
// 到某种模型不认识的方言（那会让模型写的命令一半跑不通，且看起来像"工具坏了"）。
type ShellSpec struct {
	Bin  string   `json:"bin"`
	Args []string `json:"args"`
}

// String 便于错误信息展示。
func (s ShellSpec) String() string {
	if len(s.Args) == 0 {
		return s.Bin
	}
	return s.Bin + " " + strings.Join(s.Args, " ")
}

// normalizeOutput 把子进程输出规整成合法 UTF-8。
// 已经是 UTF-8 就原样返回；否则按平台本地编码解码（Windows 走 CP936）。
func normalizeOutput(s string) string {
	if s == "" || utf8.ValidString(s) {
		return s
	}
	return string(decodeConsole([]byte(s)))
}

// defaultShellChain 平台默认候选链（按优先级）。
//
// Windows 故意把 bash/sh 排在 cmd 前面：Git for Windows 自带 POSIX shell，
// 而模型生成的命令绝大多数是 POSIX 语法。cmd 只是最后兜底，不是首选。
func defaultShellChain() []ShellSpec {
	if runtime.GOOS == "windows" {
		return []ShellSpec{
			{Bin: "bash", Args: []string{"-c"}},
			{Bin: "sh", Args: []string{"-c"}},
			{Bin: "cmd", Args: []string{"/c"}},
		}
	}
	return []ShellSpec{
		{Bin: "bash", Args: []string{"-c"}},
		{Bin: "sh", Args: []string{"-c"}},
	}
}

// knownShellArgs 给只写了名字的候选补默认参数。
func knownArgs(bin string) []string {
	switch strings.ToLower(bin) {
	case "cmd", "cmd.exe":
		return []string{"/c"}
	case "powershell", "powershell.exe", "pwsh", "pwsh.exe":
		return []string{"-NoProfile", "-NonInteractive", "-Command"}
	default:
		return []string{"-c"}
	}
}

// parseShellConfig 解析 plugins_cfg.tools.shell。
//
// 接受的形态：
//
//	缺省 / "auto"        → 平台默认链
//	"bash"               → 单元素链（严格：找不到就报错）
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
		return []ShellSpec{{Bin: s, Args: knownArgs(s)}}, true, nil
	case []any:
		var out []ShellSpec
		for _, item := range t {
			switch it := item.(type) {
			case string:
				if it = strings.TrimSpace(it); it != "" {
					out = append(out, ShellSpec{Bin: it, Args: knownArgs(it)})
				}
			case map[string]any:
				var sp ShellSpec
				if bin, ok := it["bin"].(string); ok {
					sp.Bin = strings.TrimSpace(bin)
				}
				if sp.Bin == "" {
					continue
				}
				if args, ok := it["args"].([]any); ok {
					for _, a := range args {
						if s, ok := a.(string); ok {
							sp.Args = append(sp.Args, s)
						}
					}
				} else {
					sp.Args = knownArgs(sp.Bin)
				}
				out = append(out, sp)
			}
		}
		if len(out) == 0 {
			return nil, false, fmt.Errorf("tools.shell: 空候选链")
		}
		return out, true, nil
	case map[string]any:
		bin, _ := t["bin"].(string)
		bin = strings.TrimSpace(bin)
		if bin == "" {
			return nil, false, fmt.Errorf("tools.shell: 对象形式必须带 bin")
		}
		sp := ShellSpec{Bin: bin}
		if args, ok := t["args"].([]any); ok {
			for _, a := range args {
				if s, ok := a.(string); ok {
					sp.Args = append(sp.Args, s)
				}
			}
		} else {
			sp.Args = knownArgs(bin)
		}
		return []ShellSpec{sp}, true, nil
	default:
		return nil, false, fmt.Errorf("tools.shell: 不支持的配置类型 %T（要 string / []string / object）", v)
	}
}

// resolveShell 在候选链里找出第一个可执行的 shell。
// 全都不可用时返回错误，并把试过哪些列出来——让用户知道该配什么。
func resolveShell(chain []ShellSpec) (ShellSpec, error) {
	var tried []string
	for _, sp := range chain {
		if p, err := exec.LookPath(sp.Bin); err == nil {
			return ShellSpec{Bin: p, Args: sp.Args}, nil
		}
		tried = append(tried, sp.Bin)
	}
	return ShellSpec{}, fmt.Errorf(
		"找不到可用的 shell（已尝试：%s）。请在 plugins_cfg.tools.shell 显式指定可执行文件路径",
		strings.Join(tried, ", "))
}
