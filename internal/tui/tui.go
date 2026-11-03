// Package tui 是 gsh 的终端 UI（TUI）前端，用 bubbletea 实现，
// 视觉语言 1:1 对齐 opencode：近黑画布、磷光绿强调、等宽硬边、Dracula 系配色。
//
// 复用 gsh 内核的 loop 服务：用户输入 → loop.RunSession（流式回调）→ 通过 channel
// 把 delta / tool_call / tool_result 事件回流到 bubbletea 的 Update，实现逐字流与
// 工具行实时状态。不引入新的 agent 逻辑——TUI 只是 loop 的一个新的展示面，
// 与既有的 Web UI、REPL 完全对等。
package tui

import (
	"context"
	"fmt"
	"strings"

	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/bubbles/textarea"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"gsh/internal/api"
	"gsh/internal/kernel"
)

// opencode 暗色视觉语言（Dracula 系，来自 opencode 官方主题与 DESIGN.md 规格）。
var (
	cBg      = lipgloss.Color("#080808") // 近黑画布
	cBgAlt   = lipgloss.Color("#0f0f0f") // 输入框 / 代码块底
	cText    = lipgloss.Color("#d2d2d2") // 米白主文字
	cMuted   = lipgloss.Color("#8a8a8a") // 次级文字
	cDim     = lipgloss.Color("#5a5a5a") // 暗文字
	cBorder  = lipgloss.Color("#2a2a2a") // 边框
	cAccent  = lipgloss.Color("#50fa7b") // 磷光绿：主要行动 / 激活
	cCyan    = lipgloss.Color("#8be9fd")
	cPurple  = lipgloss.Color("#bd93f9")
	cRed     = lipgloss.Color("#ff5555")

	styleHeader   = lipgloss.NewStyle().Foreground(cAccent).Bold(true)
	styleUser     = lipgloss.NewStyle().Foreground(cAccent)
	styleAsst     = lipgloss.NewStyle().Foreground(cText)
	styleToolOK   = lipgloss.NewStyle().Foreground(cAccent)
	styleToolErr  = lipgloss.NewStyle().Foreground(cRed)
	stylePending  = lipgloss.NewStyle().Foreground(cPurple)
	styleMuted    = lipgloss.NewStyle().Foreground(cMuted)
	styleErr      = lipgloss.NewStyle().Foreground(cRed)
	styleDivider  = lipgloss.NewStyle().Foreground(cBorder)
	stylePrompt   = lipgloss.NewStyle().Foreground(cAccent)
	styleInputBox = lipgloss.NewStyle().Background(cBgAlt).Foreground(cText)
)

type roleType string

const (
	roleUser roleType = "user"
	roleAsst roleType = "assistant"
	roleTool roleType = "tool"
)

// displayMsg 渲染层的一条消息。
type displayMsg struct {
	role    roleType
	text    string // user / assistant 文本
	id      string // tool 调用 id（tool_call 与 tool_result 配对用）
	name    string // tool 名
	state   string // tool: pending / done / error
	summary string // tool 结果摘要
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
	width    int
	height   int
	ready    bool
	messages []displayMsg
	vp       viewport.Model
	ta       textarea.Model
	sp       spinner.Model
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
	ta.Placeholder = "询问 gsh …  Enter 发送 · Esc 停止 · Ctrl+C 退出"
	ta.ShowLineNumbers = false
	ta.Prompt = stylePrompt.Render("› ") // opencode 风格输入框提示符
	ta.SetWidth(80)
	ta.SetHeight(3)
	ta.Focus()

	sp := spinner.New()
	sp.Spinner = spinner.Dot

	vp := viewport.New(80, 20)

	return &Model{
		k:        k,
		loop:     lp,
		sessID:   sessID,
		ta:       ta,
		sp:       sp,
		vp:       vp,
		streamCh: make(chan streamEvent, 1024),
	}, nil
}

// Init 进入时聚焦输入框、启动 spinner，并开始监听流式事件。
func (m *Model) Init() tea.Cmd {
	return tea.Batch(m.ta.Focus(), m.sp.Tick, listenCmd(m.streamCh))
}

// listenCmd 把 channel 里的事件转成 bubbletea 消息（持续监听）。
func listenCmd(ch chan streamEvent) tea.Cmd {
	return func() tea.Msg { return <-ch }
}

// Update 处理按键、窗口尺寸、流式事件、spinner tick。
func (m *Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		m.vp.Width = msg.Width
		m.vp.Height = msg.Height - 3 /* header */ - 5 /* footer */
		m.ta.SetWidth(msg.Width - 4)
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

	case spinner.TickMsg:
		var cmd tea.Cmd
		m.sp, cmd = m.sp.Update(msg)
		return m, cmd

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
		m.messages = append(m.messages, displayMsg{role: roleTool, id: se.id, name: se.name, state: "pending"})
	case "tool_result":
		for i := range m.messages {
			if m.messages[i].role == roleTool && m.messages[i].id == se.id {
				if se.err {
					m.messages[i].state = "error"
				} else {
					m.messages[i].state = "done"
				}
				m.messages[i].summary = summarizeResult(se.data)
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
	var b strings.Builder
	for _, dm := range m.messages {
		switch dm.role {
		case roleUser:
			b.WriteString(styleUser.Render("› "+dm.text) + "\n\n")
		case roleAsst:
			txt := dm.text
			if m.running && txt == "" {
				txt = m.sp.View() + " 思考中…"
			}
			b.WriteString(styleAsst.Render(txt) + "\n\n")
		case roleTool:
			b.WriteString(renderTool(dm, m.sp.View()) + "\n")
		}
	}
	if m.errStr != "" {
		b.WriteString(styleErr.Render("错误: "+m.errStr) + "\n")
	}
	m.vp.SetContent(b.String())
	m.vp.GotoBottom()
}

// renderTool opencode 风格的工具行：图标 + 工具名 + 结果摘要。
func renderTool(dm displayMsg, spin string) string {
	var icon string
	var nameStyle lipgloss.Style
	switch dm.state {
	case "pending":
		icon = spin
		nameStyle = stylePending
	case "done":
		icon = "✓"
		nameStyle = styleToolOK
	case "error":
		icon = "✗"
		nameStyle = styleToolErr
	default:
		icon = "•"
		nameStyle = styleMuted
	}
	line := nameStyle.Render(fmt.Sprintf("%s %s", icon, dm.name))
	if dm.summary != "" {
		line += styleMuted.Render(dm.summary)
	}
	return line
}

// summarizeResult 把工具结果压成一行摘要（去换行、截断）。
func summarizeResult(s string) string {
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.Join(strings.Fields(s), " ")
	s = strings.TrimSpace(s)
	const max = 90
	if r := []rune(s); len(r) > max {
		s = string(r[:max]) + "…"
	}
	if s == "" {
		return "  （空）"
	}
	return "  " + s
}

// View 渲染整屏：顶栏 + 消息区 + 底栏。
func (m *Model) View() string {
	if !m.ready {
		return styleMuted.Render("loading…")
	}
	header := m.renderHeader()
	divider := styleDivider.Render(strings.Repeat("─", maxInt(m.width, 1)))
	return header + "\n" + m.vp.View() + "\n" + divider + "\n" + m.ta.View()
}

// renderHeader opencode 风格顶栏：品牌 + 会话 id + 运行状态。
func (m *Model) renderHeader() string {
	brand := styleHeader.Render("⌬ gsh")
	sess := styleMuted.Render("  session " + shortID(m.sessID))
	status := ""
	if m.running {
		status = stylePending.Render(m.sp.View() + " running")
	}
	pad := m.width - lipgloss.Width(brand) - lipgloss.Width(sess) - lipgloss.Width(status) - 2
	if pad < 0 {
		pad = 0
	}
	return brand + sess + strings.Repeat(" ", pad) + status
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
