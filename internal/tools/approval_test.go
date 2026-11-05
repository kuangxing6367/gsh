package tools

import (
	"context"
	"testing"

	"gsh/internal/api"
)

// recorderTool 测试用工具：Run 被调用时累加计数。
type recorderTool struct {
	name string
	ran  *int
}

func (r recorderTool) Spec() api.ToolSpec { return api.ToolSpec{Name: r.name} }
func (r recorderTool) Run(_ context.Context, _ string) (string, error) {
	*r.ran++
	return "ok", nil
}

// countingApprover 测试用审批器：记录被调用次数，按预设返回裁决。
type countingApprover struct {
	dec   api.ApprovalDecision
	calls int
}

func (c *countingApprover) Approve(_ context.Context, _ api.ToolCall) (api.ApprovalDecision, error) {
	c.calls++
	return c.dec, nil
}

// newApproveReg 构造带 no-op 审计与指定档位的注册表，注册一个 risky 名工具。
func newApproveReg(t *testing.T, perm Perm) (*registry, *int) {
	t.Helper()
	audit, err := NewAuditLog(t.TempDir(), false)
	if err != nil {
		t.Fatal(err)
	}
	ran := 0
	reg := &registry{
		tools: map[string]api.Tool{"bash": recorderTool{name: "bash", ran: &ran}},
		guard: NewGuard(perm),
		audit: audit,
	}
	return reg, &ran
}

func TestRegistryApprovalDeny(t *testing.T) {
	reg, ran := newApproveReg(t, PermStandard)
	reg.SetApprover(&countingApprover{dec: api.ApprovalDecision{Allow: false}})
	res := reg.Execute(context.Background(), api.ToolCall{ID: "1", Name: "bash", Args: `{"command":"x"}`})
	if !res.IsError {
		t.Fatal("拒绝时应返回错误")
	}
	if *ran != 0 {
		t.Errorf("拒绝时工具不应执行，ran=%d", *ran)
	}
}

func TestRegistryApprovalAllow(t *testing.T) {
	reg, ran := newApproveReg(t, PermStandard)
	a := &countingApprover{dec: api.ApprovalDecision{Allow: true}}
	reg.SetApprover(a)
	res := reg.Execute(context.Background(), api.ToolCall{ID: "1", Name: "bash"})
	if res.IsError {
		t.Fatalf("放行时不应返回错误: %s", res.Content)
	}
	if *ran != 1 {
		t.Errorf("应执行一次，ran=%d", *ran)
	}
	if a.calls != 1 {
		t.Errorf("应询问一次，calls=%d", a.calls)
	}
}

func TestRegistryApprovalAlways(t *testing.T) {
	reg, ran := newApproveReg(t, PermStandard)
	a := &countingApprover{dec: api.ApprovalDecision{Allow: true, Always: true}}
	reg.SetApprover(a)
	reg.Execute(context.Background(), api.ToolCall{ID: "1", Name: "bash"})
	reg.Execute(context.Background(), api.ToolCall{ID: "2", Name: "bash"})
	if a.calls != 1 {
		t.Errorf("选 always 后第二次不应再询问，calls=%d", a.calls)
	}
	if *ran != 2 {
		t.Errorf("两次都应执行，ran=%d", *ran)
	}
}

func TestRegistryApprovalSkippedInFull(t *testing.T) {
	reg, ran := newApproveReg(t, PermFull)
	a := &countingApprover{dec: api.ApprovalDecision{Allow: false}}
	reg.SetApprover(a)
	res := reg.Execute(context.Background(), api.ToolCall{ID: "1", Name: "bash"})
	if res.IsError {
		t.Fatalf("完全权限不应询问，却返回错误: %s", res.Content)
	}
	if a.calls != 0 {
		t.Errorf("完全权限不应调用审批器，calls=%d", a.calls)
	}
	if *ran != 1 {
		t.Errorf("应执行一次，ran=%d", *ran)
	}
}

func TestRegistryApprovalBlockedInReadOnly(t *testing.T) {
	reg, ran := newApproveReg(t, PermReadOnly)
	a := &countingApprover{dec: api.ApprovalDecision{Allow: true}}
	reg.SetApprover(a)
	res := reg.Execute(context.Background(), api.ToolCall{ID: "1", Name: "bash"})
	if !res.IsError {
		t.Fatal("只读档位应直接拦截写工具，不应放行")
	}
	if a.calls != 0 {
		t.Errorf("只读档位应在 guard 处拦截、不调用审批器，calls=%d", a.calls)
	}
	if *ran != 0 {
		t.Errorf("工具不应执行，ran=%d", *ran)
	}
}
