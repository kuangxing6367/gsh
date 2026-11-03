package tools

import (
	"context"
	"strings"
	"testing"

	"gsh/internal/api"
)

// stubTool 测试用工具：只回报预设的输出/错误，不做真实 IO。
type stubTool struct {
	name string
	out  string
	err  error
}

func (f *stubTool) Spec() api.ToolSpec { return api.ToolSpec{Name: f.name} }
func (f *stubTool) Run(_ context.Context, _ string) (string, error) {
	return f.out, f.err
}

func TestRedactArgs(t *testing.T) {
	in := `{"command":"curl -H 'Authorization: Bearer abc123XYZ' https://x","path":"f.txt","api_key":"sk-1234567890abcdefghij","argv":["git","clone"]}`
	out := redactArgs(in)
	if strings.Contains(out, "abc123XYZ") {
		t.Fatal("bearer token not redacted")
	}
	if strings.Contains(out, "sk-1234567890abcdefghij") {
		t.Fatal("api_key value not redacted")
	}
	if !strings.Contains(out, "***REDACTED***") {
		t.Fatal("expected at least one redaction marker")
	}
	if !strings.Contains(out, "f.txt") {
		t.Fatal("non-sensitive path should be preserved")
	}
	if !strings.Contains(out, "git") {
		t.Fatal("argv entries should be preserved")
	}
}

func TestRedactSecretsInCommand(t *testing.T) {
	cmd := `export TOKEN=supersecret123 && echo hi`
	out := redactArgs(`{"command":` + "`"+cmd+"`" + `}`)
	if strings.Contains(out, "supersecret123") {
		t.Fatal("token=... in command not redacted")
	}
}

func TestAuditRecordAndRecent(t *testing.T) {
	a, err := NewAuditLog(t.TempDir(), true)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	a.Record(AuditEntry{Tool: "bash", Action: "execute", Ok: true})
	a.Record(AuditEntry{Tool: "read_file", Action: "execute", Ok: true})
	es, err := a.Recent(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(es) != 2 {
		t.Fatalf("want 2 entries, got %d", len(es))
	}
	// 新→旧：最后写的最先返回。
	if es[0].Tool != "read_file" {
		t.Fatalf("newest should be read_file, got %s", es[0].Tool)
	}
}

func TestAuditDisabled(t *testing.T) {
	a, err := NewAuditLog(t.TempDir(), false)
	if err != nil {
		t.Fatal(err)
	}
	a.Record(AuditEntry{Tool: "bash", Action: "execute"})
	es, err := a.Recent(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(es) != 0 {
		t.Fatalf("disabled audit must record nothing, got %d", len(es))
	}
	if a.Enabled() {
		t.Fatal("Enabled() should be false when disabled")
	}
}

func TestRegistryExecuteAudit(t *testing.T) {
	dir := t.TempDir()
	a, err := NewAuditLog(dir, true)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	reg := &registry{guard: NewGuard(PermStandard), audit: a}
	reg.Register(&stubTool{name: "read_file", out: "ok"})
	reg.Register(&stubTool{name: "write_file", out: "wrote"})

	// 1) 真实执行（含敏感参数，应被脱敏）
	reg.Execute(context.Background(), api.ToolCall{ID: "c1", Name: "read_file", Args: `{"path":"f","api_key":"sk-secret"}`})
	// 2) 不存在的工具 → denied
	reg.Execute(context.Background(), api.ToolCall{ID: "c2", Name: "ghost", Args: "{}"})
	// 3) 只读模式下写工具 → blocked
	reg.guard.Set(PermReadOnly)
	reg.Execute(context.Background(), api.ToolCall{ID: "c3", Name: "write_file", Args: `{"path":"f"}`})

	es, err := a.Recent(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(es) != 3 {
		t.Fatalf("want 3 entries, got %d", len(es))
	}
	// 新→旧: [blocked(write_file), denied(ghost), execute(read_file)]
	if es[0].Action != "blocked" || es[0].Tool != "write_file" {
		t.Fatalf("entry0 want blocked/write_file, got %s/%s", es[0].Action, es[0].Tool)
	}
	if es[0].Perm != "read_only" {
		t.Fatalf("blocked perm want read_only, got %s", es[0].Perm)
	}
	if es[1].Action != "denied" || es[1].Tool != "ghost" {
		t.Fatalf("entry1 want denied/ghost, got %s/%s", es[1].Action, es[1].Tool)
	}
	if es[2].Action != "execute" || !es[2].Ok {
		t.Fatalf("entry2 want execute/ok, got %s/ok=%v", es[2].Action, es[2].Ok)
	}
	if es[2].Perm != "standard" {
		t.Fatalf("execute perm want standard, got %s", es[2].Perm)
	}
	if es[2].ResultLen != 2 {
		t.Fatalf("result_len want 2, got %d", es[2].ResultLen)
	}
	if !strings.Contains(es[2].Args, "***REDACTED***") {
		t.Fatal("sensitive arg value should be redacted in audit")
	}
	if strings.Contains(es[2].Args, "sk-secret") {
		t.Fatal("raw secret leaked into audit log")
	}
}
