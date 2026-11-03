package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// argsJSON 构造工具参数（路径含反斜杠时必须走 JSON 编码，不能手拼）。
func argsJSON(t *testing.T, v map[string]any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestParseShellConfig(t *testing.T) {
	// 缺省 → 平台默认链，非显式
	chain, explicit, err := parseShellConfig(nil)
	if err != nil || explicit || len(chain) == 0 {
		t.Fatalf("nil: chain=%v explicit=%v err=%v", chain, explicit, err)
	}
	// auto 等同缺省
	chain, explicit, err = parseShellConfig("auto")
	if err != nil || explicit || len(chain) == 0 {
		t.Fatalf("auto: chain=%v explicit=%v err=%v", chain, explicit, err)
	}
	// 单个字符串 → 严格单元素
	chain, explicit, err = parseShellConfig("bash")
	if err != nil || !explicit || len(chain) != 1 || chain[0].Bin != "bash" {
		t.Fatalf("string: %v %v %v", chain, explicit, err)
	}
	if len(chain[0].Args) == 0 || chain[0].Args[0] != "-c" {
		t.Fatalf("bash args should default to -c, got %v", chain[0].Args)
	}
	// 数组 → 按序，各自补默认参数
	chain, explicit, err = parseShellConfig([]any{"bash", "sh", "cmd"})
	if err != nil || !explicit || len(chain) != 3 {
		t.Fatalf("array: %v %v %v", chain, explicit, err)
	}
	if chain[2].Bin != "cmd" || len(chain[2].Args) != 1 || chain[2].Args[0] != "/c" {
		t.Fatalf("cmd should default to /c, got %v", chain[2])
	}
	// 对象 → 精确参数
	chain, _, err = parseShellConfig(map[string]any{"bin": "powershell", "args": []any{"-NoProfile", "-Command"}})
	if err != nil || len(chain) != 1 || len(chain[0].Args) != 2 {
		t.Fatalf("object: %v %v", chain, err)
	}
	// 不支持的类型要报错，不能静默
	if _, _, err = parseShellConfig(42); err == nil {
		t.Fatal("numeric config should error")
	}
	// 空数组要报错
	if _, _, err = parseShellConfig([]any{}); err == nil {
		t.Fatal("empty chain should error")
	}
}

func TestResolveShellReportsTried(t *testing.T) {
	_, err := resolveShell([]ShellSpec{{Bin: "definitely-not-a-shell-xyz"}})
	if err == nil {
		t.Fatal("expected error for missing shell")
	}
	if !strings.Contains(err.Error(), "definitely-not-a-shell-xyz") {
		t.Fatalf("error should list what was tried, got: %v", err)
	}
}

// TestBashToolExplicitMissingShell 显式配了不存在的 shell 时必须硬报错，
// 而不是悄悄换一种方言继续跑。
func TestBashToolExplicitMissingShell(t *testing.T) {
	bt := &BashTool{
		shellChain:   []ShellSpec{{Bin: "definitely-not-a-shell-xyz", Args: []string{"-c"}}},
		requireShell: true,
	}
	_, err := bt.Run(context.Background(), `{"command":"echo hi"}`)
	if err == nil || !strings.Contains(err.Error(), "找不到可用的 shell") {
		t.Fatalf("explicit missing shell must fail loudly, got %v", err)
	}
}

// TestBashToolDegradesWithoutShell 没有 shell 且未强制要求时，
// 单条命令应降级为直连执行，而不是让整个工具报废。
func TestBashToolDegradesWithoutShell(t *testing.T) {
	bt := &BashTool{shellChain: []ShellSpec{{Bin: "definitely-not-a-shell-xyz", Args: []string{"-c"}}}}
	got, err := bt.Run(context.Background(), `{"command":"echo degraded-ok"}`)
	if err != nil {
		t.Fatalf("expected degraded execution to succeed, got %v", err)
	}
	if !strings.Contains(got, "degraded-ok") {
		t.Fatalf("degraded run produced no stdout: %q", got)
	}
	if !strings.Contains(got, "无可用 shell") {
		t.Fatalf("degraded run must state the limitation, got %q", got)
	}
}

// TestBashToolDegradeRejectsShellSyntax 用到管道/重定向时不能半执行。
func TestBashToolDegradeRejectsShellSyntax(t *testing.T) {
	bt := &BashTool{shellChain: []ShellSpec{{Bin: "definitely-not-a-shell-xyz", Args: []string{"-c"}}}}
	if _, err := bt.Run(context.Background(), `{"command":"echo a | cat"}`); err == nil {
		t.Fatal("pipeline must be rejected when no shell is available")
	}
}

// TestBashToolArgvBypassesShell argv 形式不依赖任何 shell。
func TestBashToolArgvBypassesShell(t *testing.T) {
	bt := &BashTool{shellChain: []ShellSpec{{Bin: "definitely-not-a-shell-xyz"}}, requireShell: true}
	got, err := bt.Run(context.Background(), `{"argv":["echo","argv-ok"]}`)
	if err != nil {
		t.Fatalf("argv form must not need a shell: %v", err)
	}
	if !strings.Contains(got, "argv-ok") {
		t.Fatalf("got %q", got)
	}
}

func TestShellHint(t *testing.T) {
	if h := shellHint(ShellSpec{Bin: `C:\Windows\System32\cmd.exe`}); !strings.Contains(h, "cmd 语法") {
		t.Fatalf("cmd hint missing: %q", h)
	}
	if h := shellHint(ShellSpec{Bin: "/usr/bin/bash"}); h != "" {
		t.Fatalf("posix shell should need no hint: %q", h)
	}
}

func TestLoadSkills(t *testing.T) {
	dir := t.TempDir()
	// 目录式技能（带 frontmatter）
	sub := filepath.Join(dir, "deploy")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	content := "---\nname: deploy\ndescription: 部署到生产\n---\n# 步骤\n1. 构建\n"
	if err := os.WriteFile(filepath.Join(sub, "SKILL.md"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	// 单文件技能（无 frontmatter，名字取文件名，描述取首个标题）
	if err := os.WriteFile(filepath.Join(dir, "review.md"), []byte("# Code Review\n看 diff\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	reg, err := LoadSkills(dir)
	if err != nil {
		t.Fatal(err)
	}
	if reg.Len() != 2 {
		t.Fatalf("want 2 skills, got %d", reg.Len())
	}
	sk, ok := reg.Find("deploy")
	if !ok || sk.Description != "部署到生产" {
		t.Fatalf("deploy skill wrong: %+v ok=%v", sk, ok)
	}
	rv, ok := reg.Find("review")
	if !ok || !strings.Contains(rv.Description, "Code Review") {
		t.Fatalf("review skill wrong: %+v ok=%v", rv, ok)
	}
	// use_skill 返回正文，且不含 frontmatter
	out, err := (&UseSkillTool{reg: reg}).Run(context.Background(), `{"name":"deploy"}`)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "description:") || !strings.Contains(out, "1. 构建") {
		t.Fatalf("use_skill body wrong: %s", out)
	}
	// 未知技能要报错并列出可用项
	if _, err := (&UseSkillTool{reg: reg}).Run(context.Background(), `{"name":"nope"}`); err == nil {
		t.Fatal("unknown skill should error")
	}
}

func TestLoadSkillsMissingDir(t *testing.T) {
	reg, err := LoadSkills(filepath.Join(t.TempDir(), "nope"))
	if err != nil {
		t.Fatalf("missing skill dir is not an error, got %v", err)
	}
	if reg.Len() != 0 {
		t.Fatal("expected empty registry")
	}
}

func TestDeletePathGuards(t *testing.T) {
	d := &DeletePathTool{allowRecursive: true}
	for _, p := range []string{"/", ".", "..", os.Getenv("HOME")} {
		if p == "" {
			continue
		}
		if _, err := d.Run(context.Background(), argsJSON(t, map[string]any{"path": p, "recursive": true})); err == nil {
			t.Fatalf("delete %q must be refused", p)
		}
	}
}

func TestDeletePathRecursiveNeedsPermission(t *testing.T) {
	dir := t.TempDir()
	victim := filepath.Join(dir, "victim")
	if err := os.MkdirAll(victim, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(victim, "a.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	// 未授权：非空目录拒绝
	if _, err := (&DeletePathTool{}).Run(context.Background(), argsJSON(t, map[string]any{"path": victim, "recursive": true})); err == nil {
		t.Fatal("recursive delete without permission should fail")
	}
	if _, err := os.Stat(victim); err != nil {
		t.Fatal("victim must still exist")
	}
	// 授权后可删
	if _, err := (&DeletePathTool{allowRecursive: true}).Run(context.Background(), argsJSON(t, map[string]any{"path": victim, "recursive": true})); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(victim); !os.IsNotExist(err) {
		t.Fatal("victim should be gone")
	}
}

func TestListDirAndCopyMove(t *testing.T) {
	dir := t.TempDir()
	if _, err := (&MakeDirTool{}).Run(context.Background(), argsJSON(t, map[string]any{"path": filepath.Join(dir, "a/b")})); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "a/b/f.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := (&ListDirTool{}).Run(context.Background(), argsJSON(t, map[string]any{"path": dir, "recursive": true, "depth": 3}))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(filepath.ToSlash(out), "a/b/f.txt") {
		t.Fatalf("list_dir recursive output missing nested file:\n%s", out)
	}
	dst := filepath.Join(dir, "copy")
	if _, err := (&CopyPathTool{}).Run(context.Background(), argsJSON(t, map[string]any{"from": filepath.Join(dir, "a"), "to": dst})); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(filepath.Join(dst, "b/f.txt")); err != nil || string(b) != "hello" {
		t.Fatalf("copy content wrong: %q err=%v", b, err)
	}
	moved := filepath.Join(dir, "moved")
	if _, err := (&MovePathTool{}).Run(context.Background(), argsJSON(t, map[string]any{"from": dst, "to": moved})); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(moved, "b/f.txt")); err != nil {
		t.Fatal("move lost the file")
	}
}

func TestHTMLToText(t *testing.T) {
	h := `<html><head><style>body{color:red}</style><script>var x=1</script></head>
<body><h1>Title</h1><p>Hello &amp; <b>world</b></p><ul><li>one</li><li>two</li></ul></body></html>`
	out := htmlToText(h)
	for _, want := range []string{"Title", "Hello &", "world", "one", "two"} {
		if !strings.Contains(out, want) {
			t.Fatalf("htmlToText missing %q in:\n%s", want, out)
		}
	}
	for _, bad := range []string{"<p>", "var x=1", "color:red", "&amp;"} {
		if strings.Contains(out, bad) {
			t.Fatalf("htmlToText left %q in:\n%s", bad, out)
		}
	}
}

// TestSplitCommand 引号与转义规则。
func TestSplitCommand(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{`echo hi`, []string{"echo", "hi"}},
		{`echo "a b"`, []string{"echo", "a b"}},
		{`echo 'a "b" c'`, []string{"echo", `a "b" c`}},
		{`echo a\ b`, []string{"echo", "a b"}},
		{`echo ""`, []string{"echo", ""}},
		{`  ls   -la  `, []string{"ls", "-la"}},
	}
	for _, c := range cases {
		got, err := splitCommand(c.in)
		if err != nil {
			t.Fatalf("%q: %v", c.in, err)
		}
		if strings.Join(got, "\x00") != strings.Join(c.want, "\x00") {
			t.Fatalf("%q → %#v, want %#v", c.in, got, c.want)
		}
	}
	if _, err := splitCommand(`echo "unclosed`); err == nil {
		t.Fatal("unclosed quote must error")
	}
	if _, err := splitCommand(`   `); err == nil {
		t.Fatal("empty command must error")
	}
}

// TestNeedsShell shell 语义检测必须忽略引号内的字符。
func TestNeedsShell(t *testing.T) {
	if _, ok := needsShell(`echo "a|b"`); ok {
		t.Fatal("pipe inside quotes is not a pipe")
	}
	if _, ok := needsShell(`echo a | cat`); !ok {
		t.Fatal("real pipe must be detected")
	}
	if _, ok := needsShell(`echo "a > b"`); ok {
		t.Fatal("redirect inside quotes must be ignored")
	}
}

// TestShellQuote 还原成命令行后必须能被 splitCommand 拆回原样。
func TestShellQuote(t *testing.T) {
	in := []string{"echo", "a b", "it's", `quote"d`}
	back, err := splitCommand(shellQuote(in))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(back, "\x00") != strings.Join(in, "\x00") {
		t.Fatalf("round trip mismatch: %#v vs %#v", back, in)
	}
}
