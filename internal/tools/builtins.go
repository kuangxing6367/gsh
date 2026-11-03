package tools

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"gsh/internal/api"
)

// maxToolOutput 工具输出上限（超长截断，避免撑爆模型上下文）。
const maxToolOutput = 60_000

func osGetwd() (string, error) { return os.Getwd() }

var _ = osGetwd // 预留：工具工作目录白名单化时使用

// pickShell 按候选链解析出可用的 shell（每个 BashTool 实例只解析一次）。
// 一次解析会逐个跑探针，结果连同失败原因一起缓存下来供诊断使用。
func (t *BashTool) pickShell(ctx context.Context) (ShellSpec, []ShellCandidate, error) {
	t.shellOnce.Do(func() {
		chain := t.shellChain
		if len(chain) == 0 {
			chain = defaultShellChain()
		}
		t.shellPick, t.shellCands, t.shellErr = probeChain(ctx, chain)
	})
	return t.shellPick, t.shellCands, t.shellErr
}

// shellChainOr 返回实际生效的候选链。
func (t *BashTool) shellChainOr() []ShellSpec {
	if len(t.shellChain) > 0 {
		return t.shellChain
	}
	return defaultShellChain()
}

// shellHint 当选中的 shell 不是 POSIX 方言时，给模型的纠偏提示。
// 这不是"兜底偷偷换一种跑法"，而是把环境事实告诉模型，让它自己改写命令。
func shellHint(sp ShellSpec) string {
	switch sp.withFamily().Family {
	case FamilyCmd:
		return "\n[hint] 当前 shell 是 cmd，命令需用 cmd 语法；若想要 POSIX 语法，在 plugins_cfg.tools.shell 指定 bash/sh"
	case FamilyPowerShell:
		return "\n[hint] 当前 shell 是 PowerShell，命令需用 PowerShell 语法"
	}
	return ""
}

// shellPrelude 让非 POSIX shell 输出 UTF-8。
// 注意 cmd 不加 chcp：实测在管道下 chcp 65001 反而产出 GBK，
// 编码统一交给 normalizeOutput 兜底。
func shellPrelude(sp ShellSpec) string {
	if sp.Family == FamilyPowerShell {
		return "[Console]::OutputEncoding=[Text.Encoding]::UTF8; "
	}
	return ""
}

// NodeSpec 一个远程节点。
// 两种后端：
//   - SSH 直连：Host/User/Port/Identity 齐全时，经系统 ssh 命令行执行。
//   - 外部命令：Exec 非空时，把命令交给一个本地可执行程序处理（argv 模板，
//     占位符 {cmd} 会被替换为待执行命令，{cmd64} 为 base64 编码）。
//     典型用途是把已有的远程 shell 工具（如 rzsh）接进来，复用它自己的
//     主机清单、密钥与登录流程，gsh 只负责调度与安全策略。
type NodeSpec struct {
	Host     string `json:"host"`
	Port     int    `json:"port"`
	User     string `json:"user"`
	Identity string `json:"identity"` // 私钥路径；空则使用 ssh-agent / 默认

	// Exec 外部命令后端：可执行程序名或绝对路径（如 "rzsh"）。
	Exec string `json:"exec"`
	// Args argv 模板，支持 {cmd}（命令原文）与 {cmd64}（base64 命令）。
	Args []string `json:"args"`
}

// ---------- bash ----------

// BashTool 执行 shell 命令。
// 本地 shell 由 shellChain 决定（配置驱动，见 shell.go），不再写死优先级；
// 候选链会逐个真跑探针验证，而不是查一下 PATH 就当能用。
//
// 三种执行方式，按显式程度排序：
//  1. argv 参数：直接 exec，不经过任何 shell。管道/重定向不支持，但也不需要 shell 存在。
//  2. command + 可用 shell：交给 shell 执行（唯一支持管道/重定向/变量的方式）。
//  3. command + 无可用 shell：降级为按引号规则拆分后直接 exec，并明确告知限制。
//
// 第 3 条是关键：环境里可能真的没有 sh（精简容器、scratch 镜像）。
// 此时直接报「找不到 shell」等于把整个执行能力废掉，而模型要跑的
// 大概率只是 `git status` 这种单条命令。
//
// 远程：command 带 "node":"<节点名>" 时，经 SSH 或外部命令把命令发到该节点执行（见 NodeSpec）。
// allowDestructive=false 时，破坏性/灾难性命令被安全策略拦截（见 safety.go），远程同样先本地扫描再下发。
// workspace 非空时，本地命令在该目录内执行。
type BashTool struct {
	allowDestructive bool
	requireShell     bool // true：无 shell 直接报错，不允许降级为直连执行
	ws               *Workspace
	nodes            map[string]NodeSpec
	sshBin           string
	shellChain       []ShellSpec
	shellOnce        sync.Once
	shellPick        ShellSpec
	shellCands       []ShellCandidate
	shellErr         error
}

// Spec 工具描述。
func (t *BashTool) Spec() api.ToolSpec {
	return api.ToolSpec{
		Name: "bash",
		Description: "Run a command and return combined stdout+stderr. " +
			"Prefer `argv` (an array like [\"git\",\"status\"]) when the command is a single program: it runs without any shell, so it works even on systems with no sh, and it avoids quoting bugs. " +
			"Use `command` (a string) only when you need shell features: pipes, redirection, globbing, variables, &&. " +
			"Destructive commands (rm -r, git reset --hard, del /s, pipe-to-shell...) are blocked by default safety policy unless allow_destructive is set. " +
			"Prefer read_file/grep/glob/stat for inspection.",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"command": map[string]any{"type": "string", "description": "Shell command line (needs a shell for pipes/redirection/variables)"},
				"argv": map[string]any{
					"type":        "array",
					"items":       map[string]any{"type": "string"},
					"description": "Program and arguments, executed directly without a shell. Takes precedence over command",
				},
				"node":    map[string]any{"type": "string", "description": "Remote node name (configured under plugins_cfg.tools.remote.nodes). When set, the command runs on that node instead of locally"},
				"timeout": map[string]any{"type": "integer", "description": "Timeout seconds, default 120"},
			},
		},
	}
}

// Run 执行命令。
func (t *BashTool) Run(ctx context.Context, argsJSON string) (string, error) {
	m, err := argObj(argsJSON)
	if err != nil {
		return "", err
	}
	command := argStr(m, "command")
	argv := argStrs(m, "argv")
	if command == "" && len(argv) == 0 {
		return "", fmt.Errorf("either command or argv is required")
	}

	// 安全策略：致命命令永远拦截；受限命令在 allowDestructive=false 时拦截。
	// argv 形式也要扫——否则把 `rm -rf /` 拆成 ["rm","-rf","/"] 就绕过了策略。
	scanTarget := command
	if scanTarget == "" {
		scanTarget = strings.Join(argv, " ")
	}
	if always, gated := scanCommand(scanTarget); always != "" {
		return "", fmt.Errorf("命令被安全策略拦截（致命）：%s", always)
	} else if gated != "" && !t.allowDestructive {
		return "", fmt.Errorf("命令被安全策略拦截（受限）：%s\n（如需放行，在 config.json 的 plugins_cfg.tools 设 allow_destructive: true）", gated)
	}

	timeout := 120 * time.Second
	if v, ok := m["timeout"].(float64); ok && v > 0 && v < 3600 {
		timeout = time.Duration(v) * time.Second
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	// 远程节点执行：node 参数非空时走远程后端。
	if node := argStr(m, "node"); node != "" {
		spec, ok := t.nodes[node]
		if !ok {
			return "", fmt.Errorf("未知节点 %q（在 config 的 plugins_cfg.tools.remote.nodes 中未配置）", node)
		}
		if len(argv) > 0 {
			// 远程后端只吃命令行字符串：做一层转义后下发。
			return t.runRemote(cctx, shellQuote(argv), spec)
		}
		return t.runRemote(cctx, command, spec)
	}

	// 1) 显式 argv：不经过 shell。
	if len(argv) > 0 {
		return t.execArgv(cctx, argv)
	}

	// 2) command + shell。
	sh, _, shErr := t.pickShell(cctx)
	if shErr == nil {
		return t.runViaShell(cctx, sh, command)
	}

	// 3) 无 shell：降级直连执行。
	if t.requireShell {
		return "", shErr
	}
	return t.execDegraded(cctx, command, shErr)
}

// runViaShell 交给选中的 shell 执行。
func (t *BashTool) runViaShell(ctx context.Context, sh ShellSpec, command string) (string, error) {
	argv := append(append([]string{}, sh.Args...), shellPrelude(sh)+command)
	cmd := exec.CommandContext(ctx, sh.Bin, argv...)
	if root := t.ws.Root(); root != "" {
		cmd.Dir = root
	}
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	runErr := cmd.Run()
	out := t.clip(normalizeOutput(buf.String()))
	if runErr != nil {
		return out, fmt.Errorf("exit: %v%s", runErr, shellHint(sh))
	}
	return out, nil
}

// execArgv 直接执行 argv[0]，不经过任何 shell。
func (t *BashTool) execArgv(ctx context.Context, argv []string) (string, error) {
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	if root := t.ws.Root(); root != "" {
		cmd.Dir = root
	}
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	runErr := cmd.Run()
	out := t.clip(normalizeOutput(buf.String()))
	if runErr != nil {
		return out, fmt.Errorf("exit: %v（argv 直连执行；不含 shell，管道/重定向/变量不生效）", runErr)
	}
	return out, nil
}

// execDegraded 无 shell 时的降级路径：按引号规则拆成 argv 后直接执行。
//
// 命令里用到了 shell 语义（管道、重定向、变量、&&）时**不执行**，直接说明原因。
// 半执行比不执行更危险：`cd /tmp && rm -rf x` 只跑前半段会被模型当成成功。
func (t *BashTool) execDegraded(ctx context.Context, command string, shErr error) (string, error) {
	if feat, ok := needsShell(command); ok {
		return "", fmt.Errorf(
			"当前环境没有可用 shell（%v），而这条命令用到了 %s，无法在直连模式下执行。\n"+
				"改用 argv 形式执行单个程序，或在 plugins_cfg.tools.shell 指定一个可用的 shell",
			shErr, feat)
	}
	argv, err := splitCommand(command)
	if err != nil {
		return "", fmt.Errorf("当前环境没有可用 shell，且命令无法拆分（%v）：%w", err, shErr)
	}
	out, runErr := t.execArgv(ctx, argv)
	note := "\n[note] 环境无可用 shell，本命令按 argv 直连执行（不支持管道/重定向/变量）。"
	if runErr != nil {
		if runErr != nil && strings.Contains(runErr.Error(), "executable file not found") {
			note += "\n当前 PATH 里找不到 " + argv[0] + "；可用 which 工具确认程序是否存在。"
		}
		return out, fmt.Errorf("%v%s", runErr, note)
	}
	return out + note, nil
}

// clip 截断过长输出，避免撑爆上下文。
func (t *BashTool) clip(s string) string {
	if len(s) > maxToolOutput {
		return s[:maxToolOutput] + "\n...[truncated]"
	}
	return s
}

// runRemote 分派到对应的后端。
func (t *BashTool) runRemote(ctx context.Context, command string, spec NodeSpec) (string, error) {
	if spec.Exec != "" {
		return t.runViaExec(ctx, command, spec)
	}
	return t.runViaSSH(ctx, command, spec)
}

// runViaSSH 经系统 ssh 命令行把命令发到远程节点执行。
// 命令通过 stdin 传给远程 `bash -s`，避免在本地/远程 shell 参数层产生注入。
// BatchMode=yes 保证无密钥/需密码时立即失败而非卡死；ConnectTimeout 防止无限等待。
func (t *BashTool) runViaSSH(ctx context.Context, command string, spec NodeSpec) (string, error) {
	bin := t.sshBin
	if bin == "" {
		bin = "ssh"
	}
	args := []string{"-o", "StrictHostKeyChecking=no", "-o", "BatchMode=yes", "-o", "ConnectTimeout=10"}
	if spec.Identity != "" {
		args = append(args, "-i", spec.Identity)
	}
	if spec.Port != 0 {
		args = append(args, "-p", strconv.Itoa(spec.Port))
	}
	args = append(args, spec.User+"@"+spec.Host, "bash", "-s")
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Stdin = strings.NewReader(command)
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	runErr := cmd.Run()
	out := normalizeOutput(buf.String())
	if len(out) > maxToolOutput {
		out = out[:maxToolOutput] + "\n...[truncated]"
	}
	if runErr != nil {
		return out, fmt.Errorf("remote exec on %s@%s failed: %w", spec.User, spec.Host, runErr)
	}
	return out, nil
}

// runViaExec 把命令交给外部可执行程序（argv 模板）。
// 占位符：{cmd} 命令原文（引号敏感）、{cmd64} base64 编码（推荐，避免空格/引号问题）。
// 模板里没有 {cmd} 时，命令通过 stdin 传入。
func (t *BashTool) runViaExec(ctx context.Context, command string, spec NodeSpec) (string, error) {
	args := make([]string, 0, len(spec.Args))
	hasPlaceholder := false
	for _, a := range spec.Args {
		switch {
		case strings.Contains(a, "{cmd64}"):
			hasPlaceholder = true
			args = append(args, strings.ReplaceAll(a, "{cmd64}", base64.StdEncoding.EncodeToString([]byte(command))))
		case strings.Contains(a, "{cmd}"):
			hasPlaceholder = true
			args = append(args, strings.ReplaceAll(a, "{cmd}", command))
		default:
			args = append(args, a)
		}
	}
	cmd := exec.CommandContext(ctx, spec.Exec, args...)
	if !hasPlaceholder {
		cmd.Stdin = strings.NewReader(command)
	}
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	runErr := cmd.Run()
	out := normalizeOutput(buf.String())
	if len(out) > maxToolOutput {
		out = out[:maxToolOutput] + "\n...[truncated]"
	}
	if runErr != nil {
		return out, fmt.Errorf("remote exec via %s failed: %w", spec.Exec, runErr)
	}
	return out, nil
}

// ---------- read_file ----------

// ReadFileTool 读文件。
type ReadFileTool struct{ ws *Workspace }

// Spec 工具描述。
func (t *ReadFileTool) Spec() api.ToolSpec {
	return api.ToolSpec{
		Name:        "read_file",
		Description: "Read a text file. Optional offset(1-based)/limit lines.",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"path":   map[string]any{"type": "string", "description": "File path"},
				"offset": map[string]any{"type": "integer", "description": "Start line (1-based)"},
				"limit":  map[string]any{"type": "integer", "description": "Max lines to read"},
			},
			"required": []string{"path"},
		},
	}
}

// Run 读文件内容。
func (t *ReadFileTool) Run(_ context.Context, argsJSON string) (string, error) {
	m, err := argObj(argsJSON)
	if err != nil {
		return "", err
	}
	path := argStr(m, "path")
	if path == "" {
		return "", fmt.Errorf("path is required")
	}
	path = t.ws.Resolve(path)
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	offset := 1
	if v, ok := m["offset"].(float64); ok && v > 0 {
		offset = int(v)
	}
	limit := 2000
	if v, ok := m["limit"].(float64); ok && v > 0 {
		limit = int(v)
	}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 1024*1024), 8*1024*1024)
	var b strings.Builder
	line := 0
	for sc.Scan() {
		line++
		if line < offset {
			continue
		}
		if line >= offset+limit {
			b.WriteString(fmt.Sprintf("...[more lines: use offset=%d]\n", line))
			break
		}
		text := sc.Text()
		if len(text) > 2000 {
			text = text[:2000] + "...[line truncated]"
		}
		fmt.Fprintf(&b, "%6d\t%s\n", line, text)
	}
	if err := sc.Err(); err != nil {
		return b.String(), err
	}
	return b.String(), nil
}

// ---------- write_file ----------

// WriteFileTool 写文件（整文件覆盖）。
type WriteFileTool struct{ ws *Workspace }

// Spec 工具描述。
func (t *WriteFileTool) Spec() api.ToolSpec {
	return api.ToolSpec{
		Name:        "write_file",
		Description: "Write content to a file (overwrite whole file). Creates parent dirs.",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"path":    map[string]any{"type": "string"},
				"content": map[string]any{"type": "string"},
			},
			"required": []string{"path", "content"},
		},
	}
}

// Run 写文件。
func (t *WriteFileTool) Run(_ context.Context, argsJSON string) (string, error) {
	m, err := argObj(argsJSON)
	if err != nil {
		return "", err
	}
	path := argStr(m, "path")
	if path == "" {
		return "", fmt.Errorf("path is required")
	}
	path = t.ws.Resolve(path)
	content, _ := m["content"].(string)
	if dir := filepath.Dir(path); dir != "." && dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return "", err
		}
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		return "", err
	}
	return fmt.Sprintf("wrote %s (%d bytes)", path, len(content)), nil
}

// ---------- edit_file ----------

// EditFileTool 精确字符串替换。
type EditFileTool struct{ ws *Workspace }

// Spec 工具描述。
func (t *EditFileTool) Spec() api.ToolSpec {
	return api.ToolSpec{
		Name:        "edit_file",
		Description: "Replace an exact substring in a file. old_string must match exactly and be unique (or set replace_all).",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"path":        map[string]any{"type": "string"},
				"old_string":  map[string]any{"type": "string"},
				"new_string":  map[string]any{"type": "string"},
				"replace_all": map[string]any{"type": "boolean", "description": "Replace every occurrence"},
			},
			"required": []string{"path", "old_string", "new_string"},
		},
	}
}

// Run 执行替换。
func (t *EditFileTool) Run(_ context.Context, argsJSON string) (string, error) {
	m, err := argObj(argsJSON)
	if err != nil {
		return "", err
	}
	path := argStr(m, "path")
	oldS := argStr(m, "old_string")
	newS := argStr(m, "new_string")
	replaceAll := false
	if v, ok := m["replace_all"].(bool); ok {
		replaceAll = v
	}
	if path == "" || oldS == "" {
		return "", fmt.Errorf("path and old_string are required")
	}
	path = t.ws.Resolve(path)
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	src := string(data)
	if oldS == newS {
		return "", fmt.Errorf("old_string equals new_string")
	}
	count := strings.Count(src, oldS)
	if count == 0 {
		return "", fmt.Errorf("old_string not found in %s", path)
	}
	if count > 1 && !replaceAll {
		return "", fmt.Errorf("old_string occurs %d times; provide more context or set replace_all", count)
	}
	if replaceAll {
		src = strings.ReplaceAll(src, oldS, newS)
	} else {
		src = strings.Replace(src, oldS, newS, 1)
	}
	if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
		return "", err
	}
	return fmt.Sprintf("edited %s (%d replacement(s))", path, min(count, 1<<30)), nil
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// ---------- glob ----------

// GlobTool 文件名模式匹配。
type GlobTool struct{ ws *Workspace }

// Spec 工具描述。
func (t *GlobTool) Spec() api.ToolSpec {
	return api.ToolSpec{
		Name:        "glob",
		Description: "Find files by glob pattern, e.g. src/**/*.go. Returns paths sorted by mtime (newest first).",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"pattern": map[string]any{"type": "string"},
			},
			"required": []string{"pattern"},
		},
	}
}

// Run 执行匹配。
func (t *GlobTool) Run(_ context.Context, argsJSON string) (string, error) {
	m, err := argObj(argsJSON)
	if err != nil {
		return "", err
	}
	pattern := argStr(m, "pattern")
	if pattern == "" {
		return "", fmt.Errorf("pattern is required")
	}
	matches, err := filepath.Glob(t.ws.Resolve(pattern))
	if err != nil {
		return "", err
	}
	if len(matches) == 0 {
		return "(no matches)", nil
	}
	if len(matches) > 500 {
		matches = matches[:500]
	}
	return strings.Join(matches, "\n"), nil
}

// ---------- grep ----------

// GrepTool 内容搜索。
type GrepTool struct{ ws *Workspace }

// Spec 工具描述。
func (t *GrepTool) Spec() api.ToolSpec {
	return api.ToolSpec{
		Name:        "grep",
		Description: "Search a literal substring inside files under a directory (recursive, skips .git and binaries). Returns file:line:text (max 200 hits).",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"needle":   map[string]any{"type": "string", "description": "Literal text to find"},
				"dir":      map[string]any{"type": "string", "description": "Directory, default ."},
				"glob_ext": map[string]any{"type": "string", "description": "Only files with this extension, e.g. .go"},
			},
			"required": []string{"needle"},
		},
	}
}

// Run 执行搜索。
func (t *GrepTool) Run(_ context.Context, argsJSON string) (string, error) {
	m, err := argObj(argsJSON)
	if err != nil {
		return "", err
	}
	needle := argStr(m, "needle")
	if needle == "" {
		return "", fmt.Errorf("needle is required")
	}
	dir := argStr(m, "dir")
	if dir == "" {
		dir = "."
	}
	dir = t.ws.Resolve(dir)
	ext := argStr(m, "glob_ext")

	var b strings.Builder
	hits := 0
	err = filepath.WalkDir(dir, func(path string, d os.DirEntry, werr error) error {
		if werr != nil || hits >= 200 {
			return nil
		}
		name := d.Name()
		if d.IsDir() {
			if name == ".git" || name == "node_modules" {
				return filepath.SkipDir
			}
			return nil
		}
		if ext != "" && filepath.Ext(name) != ext {
			return nil
		}
		info, ierr := d.Info()
		if ierr != nil || info.Size() > 2*1024*1024 {
			return nil
		}
		data, rerr := os.ReadFile(path)
		if rerr != nil || bytes.IndexByte(data, 0) >= 0 {
			return nil // 二进制跳过
		}
		for i, line := range strings.Split(string(data), "\n") {
			if strings.Contains(line, needle) {
				fmt.Fprintf(&b, "%s:%d:%s\n", path, i+1, strings.TrimSpace(line))
				hits++
				if hits >= 200 {
					b.WriteString("...[max 200 hits]\n")
					break
				}
			}
		}
		return nil
	})
	if err != nil {
		return b.String(), err
	}
	if hits == 0 {
		return "(no matches)", nil
	}
	return b.String(), nil
}
