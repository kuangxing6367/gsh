// Package tui 是 gsh 的终端 UI（TUI）前端，用 bubbletea + lipgloss 实现。
//
// 视觉对齐 opencode 的真实 TUI（取其 dev 分支落地页截图为参照）：
//   - 顶部：圆角「# 会话标题」条，右对齐 session 信息
//   - 用户消息：左侧蓝色竖条 + 浅色底块（不是行内气泡）
//   - 工具行：`* Tool args` / `→ Read path` 的等宽行，无 ✓/✗ 图标
//   - 底部：◆ 模式 · 模型徽标行 → 带左边条的圆角输入框 → 模式/模型/provider 行 → 键位提示条
//   - 配色：近黑画布 #0a0a0a、元素 #141414、输入框 #1e1e1e、正文 #e8e8e8、
//     次级 #909090、边框 #2e2e2e、强调蓝 #5898f0（取自截图采样，不是绿）
//
// 复用 gsh 内核的 loop 服务：用户输入 → loop.RunSession（流式回调）→ 通过 channel
// 把 delta / tool_call / tool_result 事件回流到 bubbletea 的 Update，实现逐字流与
// 工具行实时状态。不引入新的 agent 逻辑——TUI 只是 loop 的一个展示面，
// 与既有的 Web UI、REPL 完全对等。
package tui

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/charmbracelet/bubbles/textarea"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"gsh/internal/api"
	"gsh/internal/kernel"
)

// opencode 视觉 token（取自 dev 分支截图采样）。
var (
	cBg     = lipgloss.Color("#0a0a0a") // 画布
	cElem   = lipgloss.Color("#141414") // 标题条 / 用户块
	cInput  = lipgloss.Color("#1e1e1e") // 输入框
	cText   = lipgloss.Color("#e8e8e8") // 正文
	cMuted  = lipgloss.Color("#909090") // 次级
	cDim    = lipgloss.Color("#5a5a5a") // 弱化（符号/摘要）
	cBorder = lipgloss.Color("#3a3a3a") // 边框（比截图略亮，避免 256 色被量化成纯黑）
	cAccent = lipgloss.Color("#5898f0") // 强调蓝（opencode 截图主色）
	cRed    = lipgloss.Color("#ff5555")

	styleText   = lipgloss.NewStyle().Foreground(cText)
	styleMuted  = lipgloss.NewStyle().Foreground(cMuted)
	styleDim    = lipgloss.NewStyle().Foreground(cDim)
	styleAccent = lipgloss.NewStyle().Foreground(cAccent)
	styleErr    = lipgloss.NewStyle().Foreground(cRed)
)

type roleType string

const (
	roleUser roleType = "user"
	roleAsst roleType = "assistant"
	roleTool roleType = "tool"
)

// displayMsg 渲染层的一条消息。
type displayMsg struct {
	role  roleType
	text  string // user / assistant 文本
	id    string // tool 调用 id（tool_call 与 tool_result 配对）
	name  string // tool 名
	args  string // 调用参数摘要（如 command / pattern / path）
	state string // tool: pending / done / error
	note  string // 结果摘要（如 "(12)"）
}

// streamEvent 从 loop 的流式 goroutine 回流到 bubbletea 的事件。
type streamEvent struct {
	kind string // delta | tool_call | tool_result | final | done | error
	id   string
	name string
	text string
	data string
	err  bool
}

// Model 是 bubbletea 的根模型。
type Model struct {
	k        *kernel.Kernel
	loop     api.Loop
	sessID   string
	title    string
	model    string
	provider string
	mode     string // 权限档位的中文标签
	width    int
	height   int
	ready    bool
	messages []displayMsg
	vp       viewport.Model
	ta       textarea.Model
	running  bool
	cancel   context.CancelFunc
	streamCh chan streamEvent
	errStr   string
}

// Run 启动 TUI（阻塞）。新建一个会话作为本次 TUI 的对话上下文。
func Run(k *kernel.Kernel) error {
	store, err := kernel.Use[api.SessionStore](k, "sessionStore")
	if err != nil {
		return err
	}
	sess, err := store.New()
	if err != nil {
		return err
	}
	lp, err := kernel.Use[api.Loop](k, "loop")
	if err != nil {
		return err
	}
	m, err := NewModel(k, sess.ID(), lp)
	if err != nil {
		return err
	}
	p := tea.NewProgram(m, tea.WithAltScreen())
	_, err = p.Run()
	return err
}

// NewModel 构造 TUI 模型。lp 由调用方从内核取出后传入（便于测试注入假实现）。
func NewModel(k *kernel.Kernel, sessID string, lp api.Loop) (*Model, error) {
	if lp == nil {
		return nil, fmt.Errorf("tui: nil loop")
	}
	ta := textarea.New()
	ta.Placeholder = "输入消息…"
	ta.ShowLineNumbers = false
	ta.Prompt = ""
	ta.SetWidth(80)
	ta.SetHeight(2)
	ta.Focus()

	vp := viewport.New(80, 20)

	m := &Model{
		k:        k,
		loop:     lp,
		sessID:   sessID,
		title:    "新会话",
		model:    optStr(k, "llm", "model"),
		provider: optStr(k, "llm", "provider"),
		mode:     permLabel(optStr(k, "tools", "permission")),
		ta:       ta,
		vp:       vp,
		streamCh: make(chan streamEvent, 1024),
	}
	return m, nil
}

// Init 进入时聚焦输入框并开始监听流式事件。
func (m *Model) Init() tea.Cmd {
	return tea.Batch(m.ta.Focus(), listenCmd(m.streamCh))
}

// listenCmd 把 channel 里的事件转成 bubbletea 消息（持续监听）。
func listenCmd(ch chan streamEvent) tea.Cmd {
	return func() tea.Msg { return <-ch }
}

// Update 处理按键、窗口尺寸与流式事件。
func (m *Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		m.vp.Width = maxInt(msg.Width, 20)
		// 固定占位：标题条 3 + 徽标 1 + 输入框 4 + 模式行 1 + 提示条 1 + 两条分隔 = 12。
		m.vp.Height = maxInt(msg.Height-12, 3)
		m.ta.SetWidth(maxInt(msg.Width-5, 10))
		m.ready = true
		m.renderContent()

	case tea.KeyMsg:
		switch {
		case msg.Type == tea.KeyCtrlC:
			return m, tea.Quit
		case m.running:
			if msg.Type == tea.KeyEsc {
				if m.cancel != nil {
					m.cancel()
				}
				m.running = false
				m.ta.Focus()
			}
			return m, nil
		case msg.Type == tea.KeyEnter && msg.String() == "enter":
			m.send()
			return m, nil
		default:
			var cmd tea.Cmd
			m.ta, cmd = m.ta.Update(msg)
			return m, cmd
		}

	case streamEvent:
		m.handleStream(msg)
		m.renderContent()
		return m, listenCmd(m.streamCh)

	case error:
		m.errStr = msg.Error()
	}

	return m, nil
}

// send 把当前输入框内容作为一轮用户输入发给 loop（后台 goroutine 跑，事件回流）。
func (m *Model) send() {
	text := strings.TrimSpace(m.ta.Value())
	if text == "" || m.running {
		return
	}
	if m.title == "新会话" {
		m.title = truncate(text, 48)
	}
	m.ta.Reset()
	m.running = true
	m.ta.Blur()
	m.messages = append(m.messages, displayMsg{role: roleUser, text: text})
	m.renderContent()

	ctx, cancel := context.WithCancel(context.Background())
	m.cancel = cancel
	go func() {
		defer func() { m.streamCh <- streamEvent{kind: "done"} }()
		_, _ = m.loop.RunSession(ctx, m.sessID, text,
			func(delta string) {
				m.streamCh <- streamEvent{kind: "delta", text: delta}
			},
			func(ev api.Event) {
				switch ev.Kind {
				case api.EventToolCall:
					m.streamCh <- streamEvent{kind: "tool_call", id: ev.ID, name: ev.Name, text: ev.Args}
				case api.EventToolResult:
					m.streamCh <- streamEvent{kind: "tool_result", id: ev.ID, name: ev.Name, data: ev.Data, err: ev.Err}
				case api.EventFinal:
					m.streamCh <- streamEvent{kind: "final", text: ev.Data}
				}
			},
		)
	}()
}

// handleStream 处理回流的流式事件，更新消息列表。
func (m *Model) handleStream(se streamEvent) {
	switch se.kind {
	case "delta":
		if n := len(m.messages); n > 0 && m.messages[n-1].role == roleAsst {
			m.messages[n-1].text += se.text
		} else {
			m.messages = append(m.messages, displayMsg{role: roleAsst, text: se.text})
		}
	case "final":
		if n := len(m.messages); n > 0 && m.messages[n-1].role == roleAsst && m.messages[n-1].text == "" {
			m.messages[n-1].text = se.text
		}
	case "tool_call":
		m.messages = append(m.messages, displayMsg{
			role:  roleTool,
			id:    se.id,
			name:  se.name,
			args:  argSummary(se.text),
			state: "pending",
		})
	case "tool_result":
		for i := range m.messages {
			if m.messages[i].role == roleTool && m.messages[i].id == se.id {
				if se.err {
					m.messages[i].state = "error"
				} else {
					m.messages[i].state = "done"
				}
				m.messages[i].note = resultNote(se.data, se.err)
				break
			}
		}
	case "done":
		m.running = false
		m.ta.Focus()
	case "error":
		m.errStr = se.text
		m.running = false
		m.ta.Focus()
	}
}

// renderContent 把消息列表拼成 viewport 内容并滚到底。
func (m *Model) renderContent() {
	w := maxInt(m.width, 20)
	var b strings.Builder
	for _, dm := range m.messages {
		switch dm.role {
		case roleUser:
			b.WriteString(m.renderUserBlock(dm.text, w))
			b.WriteString("\n\n")
		case roleAsst:
			txt := dm.text
			if m.running && strings.TrimSpace(txt) == "" {
				b.WriteString(styleMuted.Render("  ~ 思考中…"))
			} else {
				// 助手回复按 markdown 渲染（标题/粗体/行内码/代码块/列表/引用）。
				b.WriteString(renderMarkdown(txt, w-2))
			}
			b.WriteString("\n")
		case roleTool:
			b.WriteString(renderToolLine(dm))
			b.WriteString("\n")
		}
	}
	if m.errStr != "" {
		b.WriteString(styleErr.Width(w - 2).Render("错误: " + m.errStr))
		b.WriteString("\n")
	}
	m.vp.SetContent(b.String())
	m.vp.GotoBottom()
}

// renderUserBlock 用户消息：左侧蓝色竖条 + 浅色底块（对齐 opencode）。
func (m *Model) renderUserBlock(text string, w int) string {
	return lipgloss.NewStyle().
		Border(lipgloss.Border{Left: "┃"}, false, false, false, true).
		BorderForeground(cAccent).
		Background(cElem).
		Foreground(cText).
		Padding(0, 1).
		Width(maxInt(w-5, 10)).
		Render(text)
}

// renderToolLine 工具行：`* Tool args (n)` / 读类工具用 `→`。
func renderToolLine(dm displayMsg) string {
	sym := "*"
	if isReadTool(dm.name) {
		sym = "→"
	}
	line := "  " + styleDim.Render(sym) + " " + styleMuted.Render(dm.name)
	if dm.args != "" {
		line += " " + styleDim.Render(dm.args)
	}
	if dm.state == "error" {
		line += " " + styleErr.Render("failed")
	} else if dm.note != "" {
		line += " " + styleDim.Render(dm.note)
	}
	return line
}

// View 渲染整屏：标题条 + 消息区 + 底栏。
func (m *Model) View() string {
	if !m.ready {
		return styleMuted.Render("loading…")
	}
	w := maxInt(m.width, 20)
	return m.renderTitleBar(w) + "\n" + m.vp.View() + "\n" + m.renderFooter(w)
}

// renderTitleBar 顶部圆角标题条：`# 标题` + 右对齐 session。
func (m *Model) renderTitleBar(w int) string {
	left := "# " + m.title
	right := "session " + shortID(m.sessID)
	if m.running {
		right = "running · " + right
	}
	// Width 含 padding、不含 border：文字区 = w-4，总宽 = w。
	content := maxInt(w-4, 10)
	if lipgloss.Width(left)+lipgloss.Width(right)+1 > content {
		left = truncate(left, maxInt(content-lipgloss.Width(right)-2, 6))
	}
	line := padStyled(styleText.Render(left), styleMuted.Render(right), content)
	return lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(cBorder).
		Background(cElem).
		Width(maxInt(w-2, 12)).
		Padding(0, 1).
		Render(line)
}

// renderFooter 底栏：✻ 模式·模型徽标 → 输入框 → 模式/模型/provider → 键位提示。
func (m *Model) renderFooter(w int) string {
	model := m.model
	if model == "" {
		model = "gsh"
	}

	// 用 ◆ 而不用 opencode 的 ✻：✻/✦ 在 Cascadia Mono、Consolas 都没有字形，会变豆腐块。
	badge := "  " + styleAccent.Render("◆") + " " + styleAccent.Render(m.mode) + styleMuted.Render(" · "+model)

	inner := lipgloss.NewStyle().
		Border(lipgloss.Border{Left: "┃"}, false, false, false, true).
		BorderForeground(cAccent).
		Padding(0, 1).
		Render(m.ta.View())
	box := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(cBorder).
		Background(cInput).
		Width(maxInt(w-2, 10)).
		Render(inner)

	modeRow := "  " + styleAccent.Render(m.mode) + "  " + styleText.Render(model)
	if m.provider != "" {
		modeRow += "  " + styleDim.Render(m.provider)
	}

	leftHint := "enter 发送"
	if m.running {
		leftHint = "esc 停止"
	}
	hint := padStyled(
		styleDim.Render("  "+leftHint),
		styleDim.Render("shift+enter 换行   ctrl+c 退出  "),
		w,
	)

	return badge + "\n" + box + "\n" + modeRow + "\n" + hint
}

// ---------- helpers ----------

// optStr 读取某个插件配置节的字符串字段。
func optStr(k *kernel.Kernel, plugin, key string) string {
	if k == nil {
		return ""
	}
	cfg, ok := k.Options(plugin)
	if !ok {
		return ""
	}
	v, _ := cfg[key].(string)
	return v
}

func permLabel(mode string) string {
	switch mode {
	case "read_only":
		return "只读"
	case "full":
		return "完全权限"
	default:
		return "标准"
	}
}

// argSummary 从工具参数 JSON 里抽一个最像「人类可读参数」的字段。
func argSummary(argsJSON string) string {
	s := strings.TrimSpace(argsJSON)
	if s == "" {
		return ""
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		return truncate(s, 60)
	}
	for _, key := range []string{"command", "cmd", "pattern", "path", "file_path", "query", "url", "glob", "name"} {
		if v, ok := m[key].(string); ok && v != "" {
			return truncate(collapse(v), 60)
		}
	}
	for _, v := range m {
		if str, ok := v.(string); ok && str != "" {
			return truncate(collapse(str), 60)
		}
	}
	return ""
}

// resultNote 结果摘要：条数（opencode 风格 `(18)`），失败显式标 failed。
func resultNote(data string, isErr bool) string {
	if isErr {
		return "failed"
	}
	n := 0
	for _, ln := range strings.Split(data, "\n") {
		if strings.TrimSpace(ln) != "" {
			n++
		}
	}
	if n == 0 {
		return ""
	}
	return fmt.Sprintf("(%d)", n)
}

func isReadTool(name string) bool {
	n := strings.ToLower(name)
	return strings.Contains(n, "read") || strings.Contains(n, "view") || n == "cat"
}

func collapse(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

func truncate(s string, n int) string {
	if n <= 0 {
		return ""
	}
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	if n == 1 {
		return string(r[:1])
	}
	return string(r[:n-1]) + "…"
}

// padStyled 把 left/right 之间补空格到指定显示宽度。
func padStyled(left, right string, width int) string {
	gap := width - lipgloss.Width(left) - lipgloss.Width(right)
	if gap < 1 {
		gap = 1
	}
	return left + strings.Repeat(" ", gap) + right
}

func shortID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
