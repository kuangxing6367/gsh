// Package api 定义跨插件共享的消息与服务接口。
// 各插件只依赖本包 + kernel，互不直接 import（对齐 DSH 的 typed service 思路）。
package api

import "context"

// Role 消息角色。
type Role string

const (
	RoleSystem    Role = "system"
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleTool      Role = "tool"
)

// Message OpenAI 兼容消息结构。
type Message struct {
	Role       Role       `json:"role"`
	Content    string     `json:"content,omitempty"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
	Name       string     `json:"name,omitempty"`
}

// ToolCall 模型发起的一次工具调用。
type ToolCall struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Args string `json:"arguments"` // 原始 JSON 字符串
}

// ToolSpec 工具描述（发给模型的 JSON Schema）。
type ToolSpec struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Parameters  map[string]any `json:"parameters,omitempty"`
}

// ToolResult 工具执行结果。
type ToolResult struct {
	CallID  string `json:"call_id"`
	Name    string `json:"name"`
	Content string `json:"content"`
	IsError bool   `json:"is_error"`
}

// Tool 工具插件向 registry 注册的能力。
type Tool interface {
	Spec() ToolSpec
	Run(ctx context.Context, argsJSON string) (string, error)
}

// Session 会话服务：append-only 消息流。
type Session interface {
	ID() string
	Append(msg Message) error
	Messages() []Message
}

// SessionStore 会话管理：新建 / 列出 / 恢复 / 分叉。
type SessionStore interface {
	New() (Session, error)
	List() ([]string, error) // 会话 ID 列表，新→旧
	Open(id string) (Session, error)
	Fork(id string) (Session, error)
}

// StreamDelta 流式文本回调。
type StreamDelta func(text string)

// LLM 模型服务：一轮对话补全（OpenAI 兼容）。
type LLM interface {
	Chat(ctx context.Context, msgs []Message, tools []ToolSpec, onDelta StreamDelta) (Message, error)
}

// Loop agent 主循环：用户输入 → 模型 ↔ 工具 → 最终回复。
type Loop interface {
	Run(ctx context.Context, userText string, onDelta StreamDelta) (string, error)
	RunTraced(ctx context.Context, userText string, onDelta StreamDelta, onEvent EventHandler) (string, error)
}

// EventKind 运行事件类型（Trajectory 粒度）。
type EventKind string

const (
	EventToolCall   EventKind = "tool_call"
	EventToolResult EventKind = "tool_result"
	EventFinal      EventKind = "final"
)

// Event 一次运行的事件记录（对齐 DSH 的 append-only trajectory 可观测性）。
type Event struct {
	Kind EventKind `json:"kind"`
	Name string    `json:"name,omitempty"` // 工具名
	Args string    `json:"args,omitempty"` // 工具参数 JSON
	Data string    `json:"data,omitempty"` // 结果内容 / 最终回复
}

// EventHandler 运行事件回调，可为 nil。
type EventHandler func(Event)

// ToolRegistry 工具注册表服务。
type ToolRegistry interface {
	Register(t Tool) error
	Execute(ctx context.Context, call ToolCall) ToolResult
	Specs() []ToolSpec
	Names() []string
}
