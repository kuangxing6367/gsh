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
func (t *BashTool) pickShell() (ShellSpec, error) {
	t.shellOnce.Do(func() {
		chain := t.shellChain
		if len(chain) == 0 {
			chain = defaultShellChain()
		}
		t.shellPick, t.shellErr = resolveShell(chain)
	})
	return t.shellPick, t.shellErr
}

// shellHint 当选中的 shell 不是 POSIX 方言时，给模型的纠偏提示。
// 这不是"兜底偷偷换一种跑法"，而是把环境事实告诉模型，让它自己改写命令。
func shellHint(sp ShellSpec) string {
	switch strings.ToLower(filepath.Base(sp.Bin)) {
	case "cmd", "cmd.exe":
		return "\n[hint] 当前 shell 是 cmd，命令需用 cmd 语法；若想要 POSIX 语法，在 plugins_cfg.tools.shell 指定 bash/sh"
	case "powershell", "powershell.exe", "pwsh", "pwsh.exe":
		return "\n[hint] 当前 shell 是 PowerShell，命令需用 PowerShell 语法"
	}
	return ""
}

// shellPrelude 让非 POSIX shell 输出 UTF-8。
// 注意 cmd 不加 chcp：实测在管道下 chcp 65001 反而产出 GBK，
// 编码统一交给 normalizeOutput 兜底。
func shellPrelude(sp ShellSpec) string {
	switch strings.ToLower(filepath.Base(sp.Bin)) {
	case "cmd", "cmd.exe":
		return ""
	case "powershell", "powershell.exe", "pwsh", "pwsh.exe":
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
// 本地 shell 由 shellChain 决定（配置驱动，见 shell.go），不再写死优先级。
// 远程：command 带 "node":"<节点名>" 时，经 SSH 把命令发到该节点执行（见 NodeSpec）。
// allowDestructive=false 时，破坏性/灾难性命令被安全策略拦截（见 safety.go），远程同样先本地扫描再下发。
// workspace 非空时，本地命令在该目录内执行。
type BashTool struct {
	allowDestructive bool
	workspace        string
	nodes            map[string]NodeSpec
	sshBin           string
	shellChain       []ShellSpec
	shellOnce        sync.Once
	shellPick        ShellSpec
	shellErr         error
}

// Spec 工具描述。
func (t *BashTool) Spec() api.ToolSpec {
	return api.ToolSpec{
		Name:        "bash",
		Description: "Run a shell command on the local machine and return combined stdout+stderr. Destructive commands (rm -r, git reset --hard, del /s, pipe-to-shell...) are blocked by default safety policy unless allow_destructive is set. Prefer read_file/grep/glob for inspection.",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"command": map[string]any{"type": "string", "description": "The shell command to run"},
				"node":    map[string]any{"type": "string", "description": "Remote node name (configured under plugins_cfg.tools.remote.nodes). When set, the command runs on that node over SSH instead of locally"},
				"timeout": map[string]any{"type": "integer", "description": "Timeout seconds, default 120"},
			},
			"required": []string{"command"},
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
	if command == "" {
		return "", fmt.Errorf("command is required")
	}

	// 安全策略：致命命令永远拦截；受限命令在 allowDestructive=false 时拦截。
	if always, gated := scanCommand(command); always != "" {
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

	// 远程节点执行：node 参数非空时走 SSH。
	if node := argStr(m, "node"); node != "" {
		spec, ok := t.nodes[node]
		if !ok {
			return "", fmt.Errorf("未知节点 %q（在 config 的 plugins_cfg.tools.remote.nodes 中未配置）", node)
		}
		return t.runRemote(cctx, command, spec)
	}

	sh, err := t.pickShell()
	if err != nil {
		return "", err
	}
	argv := append(append([]string{}, sh.Args...), shellPrelude(sh)+command)
	cmd := exec.CommandContext(cctx, sh.Bin, argv...)
	if t.workspace != "" {
		cmd.Dir = t.workspace
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
		return out, fmt.Errorf("exit: %v%s", runErr, shellHint(sh))
	}
	return out, nil
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
type ReadFileTool struct{}

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
type WriteFileTool struct{}

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
type EditFileTool struct{}

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
type GlobTool struct{}

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
	matches, err := filepath.Glob(pattern)
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
type GrepTool struct{}

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
