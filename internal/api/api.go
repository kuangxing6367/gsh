// Package api 定义跨插件共享的消息与服务接口。
// 各插件只依赖本包 + kernel，互不直接 import（typed service 思路，按类型取用）。
package api

import (
	"context"
	"sync/atomic"
	"time"
)

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
	// Parts 是多模态附件（图片等）。只存本地路径不存 base64：
	// 会话历史要长期保留，塞进几百 KB 的 base64 会让 JSONL 迅速失控。
	Parts []ContentPart `json:"parts,omitempty"`
	// Reasoning 是模型本轮产生的思考内容。仅用于当轮展示，
	// 不参与序列化——推理过程绝不能回流进会话历史（会污染上下文并白烧 token）。
	Reasoning string `json:"-"`
}

// ContentPart 是消息里的一段非文本内容。
type ContentPart struct {
	Type      string `json:"type"`                 // image
	Path      string `json:"path"`                 // 本地文件路径，发送时才读取
	MediaType string `json:"media_type,omitempty"` // 未填时按扩展名推断
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
	// ReadOnly 声明这个工具不会改动磁盘、不会产生外部副作用。
	//
	// 只读档与计划模式靠它判断能不能放行：默认 false 表示「可能写」，
	// 新增工具忘了声明的后果是被挡在只读模式外面，而不是悄悄把写操作放过去。
	// 不参与序列化——这是本地权限判定用的，provider 不需要知道。
	ReadOnly bool `json:"-"`
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
// Replace 是上下文压缩的落盘口：整体重写为「摘要 + 最近若干轮」。
// 实现必须保证原子性（临时文件 + rename），不能让压缩中途断电留下半截会话。
type Session interface {
	ID() string
	Append(msg Message) error
	Messages() []Message
	Replace(msgs []Message) error
}

// SessionStore 会话管理：新建 / 列出 / 恢复 / 分叉 / 删除。
type SessionStore interface {
	New() (Session, error)
	List() ([]string, error) // 会话 ID 列表，新→旧
	Open(id string) (Session, error)
	Fork(id string) (Session, error)
	// ListMeta 带标题/时间/消息数的会话列表（UI 用），新→旧。
	ListMeta() ([]SessionMeta, error)
	// Delete 物理删除会话文件。
	Delete(id string) error
	// Rename 设置自定义标题（空串清除，回到自动摘要）。
	Rename(id, title string) error
	// Search 在会话正文里做子串检索（大小写不敏感），返回命中会话与上下文片段。
	Search(query string, limit int) ([]SessionHit, error)
}

// SessionHit 会话检索命中项。
// Snippet 是命中处前后若干字符，避免把整个会话读进 UI 才让用户自己找。
type SessionHit struct {
	ID      string    `json:"id"`
	Title   string    `json:"title"`
	Updated time.Time `json:"updated"`
	Snippet string    `json:"snippet"`
	Count   int       `json:"count"` // 同一会话内命中次数
}

// SessionMeta 会话摘要。
type SessionMeta struct {
	ID       string    `json:"id"`
	Title    string    `json:"title"`
	Updated  time.Time `json:"updated"`
	Messages int       `json:"messages"`
}

// StreamDelta 流式文本回调。
type StreamDelta func(text string)

// StreamHandlers 一轮补全的流式出口。
// OnText 是正式回复增量；OnReason 是思考增量，两者物理隔离：
// 推理内容只走展示通道，绝不能混进 Content 写回会话。
type StreamHandlers struct {
	OnText   StreamDelta
	OnReason StreamDelta
}

// Usage 单轮模型用量。provider 未返回时由本地估算填充（Estimated=true）。
type Usage struct {
	PromptTokens     int  `json:"prompt_tokens"`
	CompletionTokens int  `json:"completion_tokens"`
	TotalTokens      int  `json:"total_tokens"`
	Estimated        bool `json:"estimated,omitempty"`
}

// LLM 模型服务：一轮对话补全（OpenAI 兼容）。
type LLM interface {
	Chat(ctx context.Context, msgs []Message, tools []ToolSpec, h StreamHandlers) (Message, error)
}

// UsageReporter 可选能力：provider 上报上一轮用量（含本地估算）。
// 与 LLM 解耦，避免为了计量去改所有 provider 的返回签名。
type UsageReporter interface {
	LastUsage() Usage
}

// ContextWindowReporter 可选能力：报告当前模型的上下文窗口（token 数）。
// 返回 0 表示未知——此时只能退回字符阈值做压缩判定，不能凭空假设窗口大小。
type ContextWindowReporter interface {
	ContextWindow() int
}

// TokenEstimator 可选能力：按当前模型的计费口径粗估一段消息的 token 数。
// loop 用它做窗口判定；实现方负责拼接 system 与历史后再估算。
type TokenEstimator interface {
	EstimateTokens(msgs []Message) int
}

// PromptBuilder 提示词服务：按当前工具集拼出系统提示。
// 每轮调用一次（工具集可能因权限档位变化），返回内容直接作为首条 system 消息。
type PromptBuilder interface {
	Build(tools []ToolSpec) string
}

// WorkspaceInfo 工作区状态查询。tools 插件实现，提示词层据此读实时路径，
// 避免 Web 端切换工作区后系统提示里仍写着旧目录。
type WorkspaceInfo interface {
	Root() string
}

// ModelManager 具名模型表。主模型跑对话、辅助模型跑摘要，
// 切换在下一轮请求生效，不打断正在进行的补全。
type ModelManager interface {
	Names() []string
	ActiveName() string
	AuxName() string
	Use(name string) error
	SetAux(name string) error
}

// Configurable 可选能力：服务支持运行时改配置（Web 设置面板用）。
// 不是所有实现都必须热更新，但实现了就必须保证「改完下一轮立即生效」。
type Configurable interface {
	Configure(cfg map[string]any) error
}

// Loop agent 主循环：用户输入 → 模型 ↔ 工具 → 最终回复。
type Loop interface {
	Run(ctx context.Context, userText string, onDelta StreamDelta) (string, error)
	RunTraced(ctx context.Context, userText string, onDelta StreamDelta, onEvent EventHandler) (string, error)
	// RunSession 在指定会话里跑一轮。sessionID 为空时沿用进程内默认会话。
	// 存在的理由：Web UI 同时开着多个会话，不能靠「全局 activeSession」串台。
	RunSession(ctx context.Context, sessionID string, userText string, onDelta StreamDelta, onEvent EventHandler) (string, error)
	// RunWithParts 带附件执行一轮（图片等多模态内容）。parts 为空时等同 RunSession。
	RunWithParts(ctx context.Context, sessionID, userText string, parts []ContentPart, onDelta StreamDelta, onEvent EventHandler) (string, error)
}

// EventKind 运行事件类型（Trajectory 粒度）。
type EventKind string

const (
	EventToolCall   EventKind = "tool_call"
	EventToolResult EventKind = "tool_result"
	EventReason     EventKind = "reason"
	EventUsage      EventKind = "usage"
	EventCompacted  EventKind = "compacted"
	EventTodo       EventKind = "todo"
	EventFinal      EventKind = "final"
)

// Event 一次运行的事件记录（append-only trajectory 可观测性）。
type Event struct {
	Kind EventKind `json:"kind"`
	ID   string    `json:"id,omitempty"`   // 工具调用 ID（tool_call 与 tool_result 配对用）
	Name string    `json:"name,omitempty"` // 工具名
	Args string    `json:"args,omitempty"` // 工具参数 JSON
	Data string    `json:"data,omitempty"` // 结果内容 / 最终回复
	Err  bool      `json:"err,omitempty"`  // 工具执行是否失败
	// Usage 仅 usage 事件填充；Compacted 仅 compacted 事件填充（压缩后保留的消息数）。
	Usage     *Usage     `json:"usage,omitempty"`
	Compacted int        `json:"compacted,omitempty"`
	Todo      []TodoItem `json:"todo,omitempty"`
}

// EventHandler 运行事件回调，可为 nil。
type EventHandler func(Event)

// ApprovalDecision 审批器对一次工具调用的裁决。
type ApprovalDecision struct {
	Allow  bool // 是否放行本次调用
	Always bool // 是否把该工具名加入「本进程总是放行」集合
}

// ToolApprover 交互式权限审批器（TUI 实现）。
// 工具处于「需要确认」档位时，registry 在其唯一执行入口调用它；
// 返回 Allow=false 即拒绝执行，Always=true 表示后续同类工具不再询问。
// 非交互场景（Web/REPL 或审批器为 nil）跳过确认，沿用既有权限档位。
type ToolApprover interface {
	Approve(ctx context.Context, call ToolCall) (ApprovalDecision, error)
}

// ToolRegistry 工具注册表服务。
type ToolRegistry interface {
	Register(t Tool) error
	Execute(ctx context.Context, call ToolCall) ToolResult
	Specs() []ToolSpec
	Names() []string
	// SetApprover 注入交互式审批器（TUI 用）。传 nil 关闭交互确认。
	SetApprover(a ToolApprover)
	// HasApprover 是否存在交互式审批器。有审批器时必须串行执行工具调用：
	// 多个确认弹窗并发会互相抢同一路输入，用户看到的顺序也会乱。
	HasApprover() bool
}

// TodoItem 计划清单的一项。status 取值 pending / in_progress / done。
type TodoItem struct {
	Text   string `json:"text"`
	Status string `json:"status"`
}

// PermissionSwitch 运行期权限档位。
// 界面切档后下一次工具调用立即按新档位执行，不需要重启进程。
type PermissionSwitch interface {
	Mode() string
	SetMode(name string) error
}

// MemoryStore 跨会话长期记忆。
// Text 返回注入系统提示的内容（可能已按长度截断）；Append 追加一条带时间的记录。
type MemoryStore interface {
	Text() string
	Append(note string) error
	Path() string
}

// TodoStore 计划清单服务：agent 通过 todo_write 工具维护，界面据此显示进度。
// 长任务里「模型知道自己在第几步」比「模型很聪明」更有用。
type TodoStore interface {
	Items() []TodoItem
	Set(items []TodoItem) error
}

// 钩子阶段。before_tool 可以拦下这次调用，after_tool 可以给结果补注。
const (
	HookBeforeTool = "before_tool"
	HookAfterTool  = "after_tool"
)

// HookEvent 是一次钩子调用的上下文，交给外部进程做判断。
type HookEvent struct {
	Phase     string // HookBeforeTool / HookAfterTool
	Tool      string // 工具名
	Args      string // 工具参数（原始 JSON 文本）
	SessionID string
	Result    string // 仅 after_tool：工具返回内容
	IsError   bool   // 仅 after_tool：工具是否失败
}

// HookResult 是钩子的裁决。
type HookResult struct {
	Blocked bool   // before_tool 专用：拦下这次调用
	Message string // 拦截原因，或要补注到结果里的文本
}

// HookRunner 外部钩子执行器。没装这个服务的进程里所有钩子静默跳过，
// 不改变默认行为。
type HookRunner interface {
	Run(ctx context.Context, ev HookEvent) HookResult
}

// Symbol 是索引里的一条定义。
type Symbol struct {
	Name string `json:"name"`
	Line int    `json:"line"`
	Kind string `json:"kind,omitempty"`
}

// IndexEntry 是索引里的一个文件。
type IndexEntry struct {
	Path    string   `json:"path"`
	Lang    string   `json:"lang,omitempty"`
	Lines   int      `json:"lines,omitempty"`
	Symbols []Symbol `json:"symbols,omitempty"`
}

// CodeIndex 代码库索引服务：把「翻仓库」从逐文件 grep 变成一次查表。
type CodeIndex interface {
	Search(query string, limit int) []IndexEntry
	Refresh() error
	Stats() (files int, symbols int)
	Root() string
}

// ctxKey 是 gsh 内部 context 键的类型（避免与第三方包字符串键冲突）。
type ctxKey string

// AuditSessionKey 工具执行时携带 session id 的 context 键。
// loop 在调用 ToolRegistry.Execute 前把当前会话 id 注入 ctx，
// 审计服务据此把「某次工具调用」关联到「哪个会话」，而不是只记一坨匿名事件。
const AuditSessionKey ctxKey = "audit.session"

// imageTokens 单张图片的粗略 token 当量。
const imageTokens = 800

// EstimateTokensOf 粗略估算一组消息的 token 数：ASCII 约 4 字符/token，
// CJK 约 1 字符/token。只用于窗口判定与缺失 usage 时的兜底，
// 不作为计费依据。
//
// 结果会乘上由真实 usage 校准出来的系数（见 CalibrateTokens）：
// 各家 tokenizer 对中文的切分差异很大，写死一个常数必然在某些模型上偏离。
func EstimateTokensOf(msgs []Message) int {
	n := 0
	for _, m := range msgs {
		n += 4 + estimateText(m.Content)
		// 图片按固定值计：各家视觉编码差异很大，按字节算会严重高估，
		// 按「一张图约等于多少 token」估更接近实际账单量级。
		n += len(m.Parts) * imageTokens
		for _, tc := range m.ToolCalls {
			n += estimateText(tc.Args) + 8
		}
	}
	return scaled(n)
}

// estimateText 单条文本的 token 当量。
//
// CJK 取 1 字符/token：主流 tokenizer（cl100k、DeepSeek、Qwen）实测落在
// 1~2 token/汉字，取 1 是偏低估的下界——估低了会晚压缩、直接撞窗口报 400，
// 比估高了浪费一点上下文严重得多。
func estimateText(s string) int {
	if s == "" {
		return 0
	}
	ascii, cjk := 0, 0
	for _, r := range s {
		if r < 0x80 {
			ascii++
		} else {
			cjk++
		}
	}
	return ascii/4 + cjk + 1
}

// tokenScale 是由真实 usage 反推出来的修正系数。
//
// 估算永远只能是估算，但只要 provider 报了 prompt_tokens，就能知道
// 「我猜的和实际的差多少」，把这个比例记住，下一次就准得多。
var tokenScale atomic.Value // float64

// scaled 把原始估算值按校准系数放大或缩小。
func scaled(n int) int {
	f, _ := tokenScale.Load().(float64)
	if f <= 0 {
		return n
	}
	return int(float64(n) * f)
}

// CalibrateTokens 用一次真实用量修正估算系数。
//
// estimated 是本轮实际发出去的消息的估算量（已含当前系数），actual 是
// provider 报的 prompt_tokens。样本太小时比例失真严重（一条短消息估差
// 5 个 token 就是几倍误差），所以设了门槛。系数做滑动平均，
// 避免单轮异常值把比例带偏。
func CalibrateTokens(estimated, actual int) {
	if estimated < 200 || actual < 200 {
		return
	}
	cur, _ := tokenScale.Load().(float64)
	if cur <= 0 {
		cur = 1
	}
	// estimated 已经是乘过 cur 的值，先除回去才能得到「真实需要的系数」，
	// 否则每轮都会把系数乘一遍，越走越偏。
	raw := float64(estimated) / cur
	r := float64(actual) / raw
	if r < 0.5 {
		r = 0.5
	}
	if r > 3 {
		r = 3
	}
	// 单轮样本波动大，取 30% 权重慢慢靠。
	tokenScale.Store(cur*0.7 + r*0.3)
}
