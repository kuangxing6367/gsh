package tools

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"gsh/internal/api"
)

// todoStore 进程内计划清单。
//
// 为什么不落盘到会话里：清单是「当前这一轮任务的进度」，不是对话内容。
// 写进历史既占上下文，又会在下一轮请求里变成模型眼里的旧事实。
// 需要跨会话留存的长期信息走 memory 插件，两者职责不同。
type todoStore struct {
	mu    sync.RWMutex
	items []api.TodoItem
}

// Items 返回副本，避免调用方直接改内部切片。
func (s *todoStore) Items() []api.TodoItem {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return append([]api.TodoItem(nil), s.items...)
}

// Set 整体替换清单。status 非法时归一为 pending（模型偶尔会写 running / doing）。
func (s *todoStore) Set(items []api.TodoItem) error {
	out := make([]api.TodoItem, 0, len(items))
	for _, it := range items {
		text := strings.TrimSpace(it.Text)
		if text == "" {
			continue
		}
		status := strings.ToLower(strings.TrimSpace(it.Status))
		switch status {
		case "done", "completed", "complete":
			status = "done"
		case "in_progress", "doing", "active", "running":
			status = "in_progress"
		default:
			status = "pending"
		}
		out = append(out, api.TodoItem{Text: text, Status: status})
	}
	s.mu.Lock()
	s.items = out
	s.mu.Unlock()
	return nil
}

// TodoTool 让模型显式维护任务清单。
//
// 长任务里最常见的问题不是模型不够聪明，而是它做到第 5 步时忘掉了第 2 步的要求。
// 把计划写在可回读的清单上，模型下一步能看到自己的进度，用户也能看到。
type TodoTool struct {
	store *todoStore
}

// Spec 实现 api.Tool。
func (t *TodoTool) Spec() api.ToolSpec {
	return api.ToolSpec{
		Name:     "todo_write",
		ReadOnly: true,
		Description: "创建或更新当前任务的计划清单。多步骤任务开始时先列清单，" +
			"每完成一步就更新对应项状态。整份清单每次全量提交（不是增量追加）。",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"items": map[string]any{
					"type":        "array",
					"description": "完整任务清单，按执行顺序排列",
					"items": map[string]any{
						"type": "object",
						"properties": map[string]any{
							"text":   map[string]any{"type": "string", "description": "任务描述"},
							"status": map[string]any{"type": "string", "enum": []string{"pending", "in_progress", "done"}},
						},
						"required": []string{"text", "status"},
					},
				},
			},
			"required": []string{"items"},
		},
	}
}

// Run 实现 api.Tool。
func (t *TodoTool) Run(_ context.Context, argsJSON string) (string, error) {
	m, err := argObj(argsJSON)
	if err != nil {
		return "", err
	}
	raw, ok := m["items"].([]any)
	if !ok {
		return "", fmt.Errorf("items 必须是数组")
	}
	items := make([]api.TodoItem, 0, len(raw))
	for _, it := range raw {
		obj, ok := it.(map[string]any)
		if !ok {
			continue
		}
		items = append(items, api.TodoItem{
			Text:   argStr(obj, "text"),
			Status: argStr(obj, "status"),
		})
	}
	if err := t.store.Set(items); err != nil {
		return "", err
	}
	cur := t.store.Items()
	var done, doing, pending int
	for _, it := range cur {
		switch it.Status {
		case "done":
			done++
		case "in_progress":
			doing++
		default:
			pending++
		}
	}
	if len(cur) == 0 {
		return "清单已清空。", nil
	}
	return fmt.Sprintf("清单已更新：共 %d 项（完成 %d / 进行中 %d / 待办 %d）\n%s",
		len(cur), done, doing, pending, renderTodo(cur)), nil
}

// renderTodo 渲染成可直接读的文本，同时用于工具返回与事件负载。
func renderTodo(items []api.TodoItem) string {
	var b strings.Builder
	for _, it := range items {
		mark := "[ ]"
		switch it.Status {
		case "done":
			mark = "[x]"
		case "in_progress":
			mark = "[~]"
		}
		fmt.Fprintf(&b, "%s %s\n", mark, it.Text)
	}
	return strings.TrimRight(b.String(), "\n")
}

// todoItemsFromArgs 从 tool_call 参数里取出清单，供事件广播使用。
// 解析失败返回 nil，事件层据此跳过。
func todoItemsFromArgs(argsJSON string) []api.TodoItem {
	m, err := argObj(argsJSON)
	if err != nil {
		return nil
	}
	raw, ok := m["items"].([]any)
	if !ok {
		return nil
	}
	out := make([]api.TodoItem, 0, len(raw))
	for _, it := range raw {
		obj, ok := it.(map[string]any)
		if !ok {
			continue
		}
		out = append(out, api.TodoItem{Text: argStr(obj, "text"), Status: argStr(obj, "status")})
	}
	return out
}
