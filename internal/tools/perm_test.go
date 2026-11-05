package tools

import (
	"context"
	"strings"
	"testing"

	"gsh/internal/api"
)

// fakeTool 只为了验证「注册表层拦截」优先于执行。
type fakeTool struct{ name string }

func (f fakeTool) Spec() api.ToolSpec { return api.ToolSpec{Name: f.name} }
func (f fakeTool) Run(context.Context, string) (string, error) {
	return "SHOULD-NOT-RUN", nil
}

func apiCall(name, args string) api.ToolCall {
	return api.ToolCall{ID: "t1", Name: name, Args: args}
}

// TestParsePerm 档位解析必须拒绝未知值，不能静默按标准执行。
func TestParsePerm(t *testing.T) {
	for _, c := range []struct {
		in   string
		want Perm
		ok   bool
	}{
		{"", PermStandard, true},
		{"standard", PermStandard, true},
		{"read_only", PermReadOnly, true},
		{"readonly", PermReadOnly, true},
		{"full", PermFull, true},
		{"yolo", PermFull, true},
		{"admin", PermStandard, false},
	} {
		got, err := ParsePerm(c.in)
		if c.ok && err != nil {
			t.Fatalf("%q: unexpected err %v", c.in, err)
		}
		if !c.ok && err == nil {
			t.Fatalf("%q: must error", c.in)
		}
		if c.ok && got != c.want {
			t.Fatalf("%q → %v, want %v", c.in, got, c.want)
		}
	}
}

// TestGuardReadOnlyBlocks 只读模式下写类工具在执行入口被拒，
// 读类工具（自己声明了 ReadOnly 的）不受影响。
func TestGuardReadOnlyBlocks(t *testing.T) {
	g := NewGuard(PermReadOnly)
	for _, name := range []string{"bash", "write_file", "edit_file", "delete_path", "move_path"} {
		// 即使误声明成只读，黑名单名字也必须拦。
		if reason := g.CheckTool(name, true); reason == "" {
			t.Fatalf("%s must be blocked in read_only", name)
		}
	}
	for _, name := range []string{"read_file", "glob", "grep", "list_dir", "web_fetch"} {
		if reason := g.CheckTool(name, true); reason != "" {
			t.Fatalf("%s must be allowed in read_only, got %q", name, reason)
		}
	}

	// 切档后立刻放行
	g.Set(PermStandard)
	if reason := g.CheckTool("bash", false); reason != "" {
		t.Fatalf("bash must be allowed in standard: %q", reason)
	}
}

// TestGuardBlocksUndeclaredTools 没声明 ReadOnly 的工具一律按「可能写」处理。
//
// 这是只读档与计划模式的安全边界：新加的工具、以及 MCP 接进来的工具
// 都走默认拒绝，漏声明只会让它「用不了」，不会让它「能写」。
func TestGuardBlocksUndeclaredTools(t *testing.T) {
	g := NewGuard(PermReadOnly)
	for _, name := range []string{"mcp_fs_writer", "some_new_tool", "remote_shell"} {
		if reason := g.CheckTool(name, false); reason == "" {
			t.Fatalf("%s 未声明只读，必须被拦", name)
		}
	}
}

// TestGuardDestructiveGate 完全权限才放行破坏性命令。
func TestGuardDestructiveGate(t *testing.T) {
	g := NewGuard(PermStandard)
	if g.AllowDestructive() {
		t.Fatal("standard must not allow destructive")
	}
	bt := &BashTool{guard: g}
	_, err := bt.Run(context.Background(), argsJSON(t, map[string]any{"command": "rm -rf build-tmp"}))
	if err == nil || !strings.Contains(err.Error(), "安全策略拦截") {
		t.Fatalf("gated command must be refused in standard, got %v", err)
	}

	g.Set(PermFull)
	if !g.AllowDestructive() {
		t.Fatal("full must allow destructive")
	}
}

// TestRegistryEnforcesGuard 拦截必须发生在注册表这个唯一执行入口，
// 而不是靠每个工具自己记得检查。
func TestRegistryEnforcesGuard(t *testing.T) {
	g := NewGuard(PermReadOnly)
	reg := &registry{guard: g}
	if err := reg.Register(&ListDirTool{ws: NewWorkspace(".")}); err != nil {
		t.Fatal(err)
	}
	// 只读下 list_dir 可用
	if res := reg.Execute(context.Background(), apiCall("list_dir", `{}`)); res.IsError {
		t.Fatalf("list_dir should pass, got %q", res.Content)
	}
	// 用一个「名字在黑名单里」的假工具验证拦截优先于执行
	if err := reg.Register(fakeTool{name: "bash"}); err != nil {
		t.Fatal(err)
	}
	res := reg.Execute(context.Background(), apiCall("bash", `{}`))
	if !res.IsError || !strings.Contains(res.Content, "只读") {
		t.Fatalf("bash must be blocked by registry, got %q", res.Content)
	}
}

// TestBuiltinToolsDeclareReadOnly 内置工具的只读声明必须与真实行为一致。
//
// 漏标的只读工具会在只读档里「用不了」，误标的写工具会变成一条写入通道。
// 这张表是唯一需要人手动维护的地方，所以用测试钉住。
func TestBuiltinToolsDeclareReadOnly(t *testing.T) {
	cases := []struct {
		tool api.Tool
		want bool
	}{
		{&ReadFileTool{}, true},
		{&GlobTool{}, true},
		{&GrepTool{}, true},
		{&ListDirTool{}, true},
		{&WebFetchTool{}, true},
		{&TodoTool{}, true},
		{&BashTool{}, false},
		{&WriteFileTool{}, false},
		{&EditFileTool{}, false},
		{&MakeDirTool{}, false},
		{&MovePathTool{}, false},
		{&CopyPathTool{}, false},
		{&DeletePathTool{}, false},
		{&HTTPRequestTool{}, false},
	}
	for _, c := range cases {
		spec := c.tool.Spec()
		if spec.ReadOnly != c.want {
			t.Fatalf("%s ReadOnly=%v，应为 %v", spec.Name, spec.ReadOnly, c.want)
		}
	}
}
