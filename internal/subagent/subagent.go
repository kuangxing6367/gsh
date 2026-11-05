// Package subagent 让主 agent 把一段独立的调研/改造外包出去。
//
// 子 agent 有自己的上下文、自己的轮数预算，跑完只回一句话结论。
// 这样主会话的窗口里不会堆满子任务翻看的一堆文件内容——
// 「派活」比「自己干」省上下文，是长任务能撑住的关键。
package subagent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"gsh/internal/api"
	"gsh/internal/kernel"
)

// 单个子任务的默认轮数上限。比主循环小：子任务应当聚焦，
// 跑偏了就该失败返回，而不是继续消耗。
const defaultMaxTurns = 12

// writeTools 是子 agent 默认拿不到的工具。
// 默认只读不是保守，是因为主 agent 看不见子 agent 干了什么——
// 让它在背后改文件，出了问题连回滚点都找不到。
var writeTools = map[string]bool{
	"bash":        true,
	"write_file":  true,
	"edit_file":   true,
	"delete_path": true,
	"move_path":   true,
	"copy_path":   true,
	"make_dir":    true,
	"todo_write":  true,
	"task":        true, // 不允许递归派活，否则一次误调能炸出指数级子任务
}

// RegisterKernel 挂载子 agent 插件。必须排在 loop 与 tools 之后。
func RegisterKernel(k *kernel.Kernel) error { return k.Load(&Plugin{}) }

// Plugin 子 agent 插件。
type Plugin struct{}

// Name 实现 kernel.Plugin。
func (p *Plugin) Name() string { return "subagent" }

// Apply 把 task 工具挂到主工具注册表上。
func (p *Plugin) Apply(ctx *kernel.Ctx) error {
	reg, err := kernel.Use[api.ToolRegistry](kernel.Current, "toolRegistry")
	if err != nil {
		return nil // 没有工具注册表就不提供派活能力，不算错误
	}
	maxTurns := defaultMaxTurns
	if cfg := ctx.Config(); cfg != nil {
		if f, ok := cfg["max_turns"].(float64); ok && f >= 2 {
			maxTurns = int(f)
		}
	}
	return reg.Register(&TaskTool{reg: reg, maxTurns: maxTurns})
}

// TaskTool 派活工具。
type TaskTool struct {
	reg      api.ToolRegistry
	maxTurns int
}

// Spec 实现 api.Tool。
func (t *TaskTool) Spec() api.ToolSpec {
	return api.ToolSpec{
		Name: "task",
		// 派活本身不碰磁盘：子 agent 用的仍是主注册表，写入一样过全局权限闸门。
		// 所以只读模式下允许派活——那正是最需要「只调研不改动」的场景。
		ReadOnly: true,
		Description: "把一段独立的调研或改造交给子 agent 执行，只收回最终结论。" +
			"子任务看不到当前会话历史，prompt 必须自包含。" +
			"子 agent 默认只有只读工具；确需写入时把 allow_write 设为 true。",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"prompt": map[string]any{
					"type":        "string",
					"description": "交给子 agent 的完整任务描述，包含背景、目标和验收标准",
				},
				"allow_write": map[string]any{
					"type":        "boolean",
					"description": "是否授予写文件与执行命令的权限，默认 false",
				},
			},
			"required": []string{"prompt"},
		},
	}
}

// taskArgs 派活参数。
type taskArgs struct {
	Prompt     string `json:"prompt"`
	AllowWrite bool   `json:"allow_write"`
}

// Run 实现 api.Tool：起一个独立上下文的嵌套循环。
func (t *TaskTool) Run(ctx context.Context, argsJSON string) (string, error) {
	var a taskArgs
	if err := json.Unmarshal([]byte(argsJSON), &a); err != nil {
		return "", fmt.Errorf("参数解析失败: %w", err)
	}
	if strings.TrimSpace(a.Prompt) == "" {
		return "", fmt.Errorf("prompt 不能为空")
	}
	return t.run(ctx, a)
}

// filtered 是子 agent 看到的工具视图：同一批工具，少几个名字。
type filtered struct {
	api.ToolRegistry
	drop map[string]bool
}

// Specs 只暴露允许的工具。
func (f filtered) Specs() []api.ToolSpec {
	all := f.ToolRegistry.Specs()
	out := make([]api.ToolSpec, 0, len(all))
	for _, s := range all {
		if f.drop[s.Name] {
			continue
		}
		out = append(out, s)
	}
	return out
}

// Names 与 Specs 保持一致。
func (f filtered) Names() []string {
	out := make([]string, 0, len(f.Specs()))
	for _, s := range f.Specs() {
		out = append(out, s.Name)
	}
	return out
}

// Execute 兜住越权调用：模型可能凭记忆调一个已经被藏起来的工具，
// 这里必须再挡一次，不能只靠「提示里没列出来」。
func (f filtered) Execute(ctx context.Context, call api.ToolCall) api.ToolResult {
	if f.drop[call.Name] {
		return api.ToolResult{
			CallID:  call.ID,
			Name:    call.Name,
			Content: "该工具在子 agent 中不可用；如需写入，请由主 agent 显式授予 allow_write。",
			IsError: true,
		}
	}
	return f.ToolRegistry.Execute(ctx, call)
}

// run 执行嵌套循环。
func (t *TaskTool) run(ctx context.Context, a taskArgs) (string, error) {
	k := kernel.Current
	if k == nil {
		return "", fmt.Errorf("subagent: kernel not bound")
	}
	model, err := kernel.Use[api.LLM](k, "llm")
	if err != nil {
		return "", err
	}
	drop := map[string]bool{}
	if !a.AllowWrite {
		for name := range writeTools {
			drop[name] = true
		}
	} else {
		drop["task"] = true
	}
	view := filtered{ToolRegistry: t.reg, drop: drop}
	specs := view.Specs()

	system := "你是 gsh 派出的子 agent，独立完成一项子任务后返回结论。"
	if b, berr := kernel.Use[api.PromptBuilder](k, "promptBuilder"); berr == nil && b != nil {
		system = b.Build(specs)
	}
	system += "\n\n# 子任务\n你被主 agent 派来单独完成一项任务，看不到主会话的历史。\n"
	system += "直接动手做，不要反问、不要请求确认——把结论写清楚返回即可。\n"
	if !a.AllowWrite {
		system += "本次只有只读权限：不要尝试写入或执行命令，遇到必须改动的地方就指出该改哪里。\n"
	}

	msgs := []api.Message{
		{Role: api.RoleSystem, Content: system},
		{Role: api.RoleUser, Content: a.Prompt},
	}
	for turn := 0; turn < t.maxTurns; turn++ {
		assistant, cerr := model.Chat(ctx, msgs, specs, api.StreamHandlers{})
		if cerr != nil {
			return "", cerr
		}
		assistant.Reasoning = ""
		msgs = append(msgs, assistant)
		if len(assistant.ToolCalls) == 0 {
			return strings.TrimSpace(assistant.Content), nil
		}
		for _, call := range assistant.ToolCalls {
			res := view.Execute(ctx, call)
			msgs = append(msgs, api.Message{
				Role:       api.RoleTool,
				Content:    res.Content,
				ToolCallID: res.CallID,
				Name:       res.Name,
			})
		}
	}
	// 超轮数不算彻底失败：把最后一段内容带回去，主 agent 自己决定怎么补。
	if last := msgs[len(msgs)-1]; last.Role == api.RoleAssistant && strings.TrimSpace(last.Content) != "" {
		return strings.TrimSpace(last.Content), nil
	}
	return "", fmt.Errorf("子任务超过 %d 轮仍未收敛", t.maxTurns)
}
