package subagent

import (
	"context"
	"strings"
	"testing"

	"gsh/internal/api"
	"gsh/internal/kernel"
)

// fakeModel 按脚本返回：第一次发起工具调用，第二次给结论。
type fakeModel struct {
	calls  int
	specs  [][]api.ToolSpec
	script []api.Message
}

func (m *fakeModel) Chat(_ context.Context, _ []api.Message, tools []api.ToolSpec, _ api.StreamHandlers) (api.Message, error) {
	m.specs = append(m.specs, tools)
	if m.calls >= len(m.script) {
		return api.Message{Role: api.RoleAssistant, Content: "done"}, nil
	}
	msg := m.script[m.calls]
	m.calls++
	return msg, nil
}

// fakeRegistry 只记执行了哪些工具。
type fakeRegistry struct {
	ran []string
}

func (r *fakeRegistry) Register(api.Tool) error { return nil }
func (r *fakeRegistry) Specs() []api.ToolSpec {
	return []api.ToolSpec{
		{Name: "read_file", Description: "读"},
		{Name: "bash", Description: "跑命令"},
	}
}
func (r *fakeRegistry) Names() []string              { return []string{"read_file", "bash"} }
func (r *fakeRegistry) SetApprover(api.ToolApprover) {}
func (r *fakeRegistry) HasApprover() bool            { return false }
func (r *fakeRegistry) Execute(_ context.Context, call api.ToolCall) api.ToolResult {
	r.ran = append(r.ran, call.Name)
	return api.ToolResult{CallID: call.ID, Name: call.Name, Content: "ok"}
}

type provider struct {
	model *fakeModel
	reg   *fakeRegistry
}

func (p *provider) Name() string { return "stub" }
func (p *provider) Apply(ctx *kernel.Ctx) error {
	ctx.Provide("llm", api.LLM(p.model))
	ctx.Provide("toolRegistry", api.ToolRegistry(p.reg))
	return nil
}

// setup 造一个只有假模型与假注册表的内核。
func setup(t *testing.T, script []api.Message) (*fakeModel, *fakeRegistry) {
	t.Helper()
	model := &fakeModel{script: script}
	reg := &fakeRegistry{}
	k := kernel.New(&kernel.Config{})
	prev := kernel.Current
	kernel.Current = k
	t.Cleanup(func() { kernel.Current = prev })
	if err := k.Load(&provider{model: model, reg: reg}); err != nil {
		t.Fatalf("装载桩插件失败: %v", err)
	}
	return model, reg
}

// hasTool 判断模型看到的工具表里有没有某个名字。
func hasTool(specs []api.ToolSpec, name string) bool {
	for _, s := range specs {
		if s.Name == name {
			return true
		}
	}
	return false
}

func TestReadOnlyByDefault(t *testing.T) {
	model, reg := setup(t, nil)
	tool := &TaskTool{reg: reg, maxTurns: 3}
	if _, err := tool.Run(context.Background(), `{"prompt":"看看项目结构"}`); err != nil {
		t.Fatalf("Run 失败: %v", err)
	}
	if len(model.specs) == 0 {
		t.Fatal("模型没被调用")
	}
	seen := model.specs[0]
	if hasTool(seen, "bash") {
		t.Fatal("默认只读时不该看到 bash")
	}
	if !hasTool(seen, "read_file") {
		t.Fatal("只读工具应当保留")
	}
	if hasTool(seen, "task") {
		t.Fatal("子 agent 不该能继续派活")
	}
}

func TestAllowWriteExpandsTools(t *testing.T) {
	model, reg := setup(t, nil)
	tool := &TaskTool{reg: reg, maxTurns: 3}
	if _, err := tool.Run(context.Background(), `{"prompt":"改一下","allow_write":true}`); err != nil {
		t.Fatalf("Run 失败: %v", err)
	}
	if !hasTool(model.specs[0], "bash") {
		t.Fatal("allow_write 后应当能看到 bash")
	}
	if hasTool(model.specs[0], "task") {
		t.Fatal("无论如何都不该能递归派活")
	}
}

func TestBlockedCallNeverReachesRegistry(t *testing.T) {
	script := []api.Message{
		{
			Role: api.RoleAssistant,
			ToolCalls: []api.ToolCall{
				{ID: "c1", Name: "bash", Args: `{"command":"rm -rf /"}`},
			},
		},
		{Role: api.RoleAssistant, Content: "只有只读权限，删不了。"},
	}
	_, reg := setup(t, script)
	tool := &TaskTool{reg: reg, maxTurns: 3}
	out, err := tool.Run(context.Background(), `{"prompt":"删点东西"}`)
	if err != nil {
		t.Fatalf("Run 失败: %v", err)
	}
	if len(reg.ran) != 0 {
		t.Fatalf("越权调用不应落到真实注册表，实际执行了 %v", reg.ran)
	}
	if !strings.Contains(out, "只读权限") {
		t.Fatalf("应带回子 agent 的结论，实际 %q", out)
	}
}

func TestEmptyPromptRejected(t *testing.T) {
	_, reg := setup(t, nil)
	tool := &TaskTool{reg: reg, maxTurns: 2}
	if _, err := tool.Run(context.Background(), `{"prompt":"  "}`); err == nil {
		t.Fatal("空 prompt 应当报错")
	}
	if _, err := tool.Run(context.Background(), `{`); err == nil {
		t.Fatal("坏 JSON 应当报错")
	}
}

func TestTurnBudgetExhausted(t *testing.T) {
	// 模型一直要工具，永不收口。
	loop := api.Message{Role: api.RoleAssistant, ToolCalls: []api.ToolCall{{ID: "c", Name: "read_file", Args: "{}"}}}
	model, reg := setup(t, nil)
	model.script = []api.Message{loop, loop, loop, loop, loop}
	tool := &TaskTool{reg: reg, maxTurns: 2}
	if _, err := tool.Run(context.Background(), `{"prompt":"读个够"}`); err == nil {
		t.Fatal("超轮数应当报错")
	}
	if model.calls != 2 {
		t.Fatalf("轮数预算应为 2，实际调了 %d 次", model.calls)
	}
}
