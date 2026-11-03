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
// 读类工具不受影响。
func TestGuardReadOnlyBlocks(t *testing.T) {
	g := NewGuard(PermReadOnly)
	for _, name := range []string{"bash", "write_file", "edit_file", "delete_path", "move_path"} {
		if reason := g.Check(name); reason == "" {
			t.Fatalf("%s must be blocked in read_only", name)
		}
	}
	for _, name := range []string{"read_file", "glob", "grep", "list_dir", "web_fetch"} {
		if reason := g.Check(name); reason != "" {
			t.Fatalf("%s must be allowed in read_only, got %q", name, reason)
		}
	}

	// 切档后立刻放行
	g.Set(PermStandard)
	if reason := g.Check("bash"); reason != "" {
		t.Fatalf("bash must be allowed in standard: %q", reason)
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
