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
	"time"

	"github.com/charmbracelet/bubbles/textarea"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"gsh/internal/api"
	"gsh/internal/kernel"
	"gsh/internal/tools"
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
	styleDiffAdd = lipgloss.NewStyle().Foreground(lipgloss.Color("#6fce8c"))
	styleDiffDel = lipgloss.NewStyle().Foreground(lipgloss.Color("#ff6b6b"))
)

type roleType string

const (
	roleUser roleType = "user"
	roleAsst roleType = "assistant"
	roleTool roleType = "tool"
)

// displayMsg 渲染层的一条消息。
type displayMsg struct {
	role   roleType
	text   string // user / assistant 文本
	id     string // tool 调用 id（tool_call 与 tool_result 配对）
	name   string // tool 名
	args   string // 调用参数摘要（如 command / pattern / path）
	state  string // tool: pending / done / error
	note   string // 结果摘要（如 "(12)" / "diff"）
	diff   string // 文件改动统一 diff（write/edit 工具产出）
	detail string // 工具结果正文（去除 diff 后的剩余输出）
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

	// 浮层与命令
	overlay   string // ""=无, "help", "sessions"
	sessList  []api.SessionMeta
	sessIdx   int
	// 计时与计数（顶栏实时显示，不编 token/cost）
	runStart time.Time
	lastDur  time.Duration

	// 交互式权限确认：标准档位下高风险工具执行前先问用户。
	permCh chan *permRequest // 审批器 → Model
	perm   *permRequest      // 当前待确认请求（nil=无）
}

// permRequest 一次待确认的工具调用（审批器 → Model 的跨 goroutine 消息）。
type permRequest struct {
	name string // 工具名
	args string // 参数 JSON（用于摘要展示）
	resp chan permDecision
}

// permDecision 用户对一次确认的选择。
type permDecision struct {
	allow  bool
	always bool
}

// tuiApprover 把 registry 的 Approve 调用桥接到 bubbletea 事件循环：
// 阻塞等待 Model 把决定写回 resp。loop 在独立 goroutine 中调用，不卡 UI 主线程。
type tuiApprover struct {
	permCh chan *permRequest
}

func (a *tuiApprover) Approve(ctx context.Context, call api.ToolCall) (api.ApprovalDecision, error) {
	pr := &permRequest{name: call.Name, args: call.Args, resp: make(chan permDecision, 1)}
	select {
	case a.permCh <- pr:
	case <-ctx.Done():
		return api.ApprovalDecision{}, ctx.Err()
	}
	select {
	case d := <-pr.resp:
		return api.ApprovalDecision{Allow: d.allow, Always: d.always}, nil
	case <-ctx.Done():
		return api.ApprovalDecision{}, ctx.Err()
	}
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

	permCh := make(chan *permRequest, 1)
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
		overlay:  "",
		permCh:   permCh,
	}
	// 注入交互式审批器（仅当 registry 存在；测试裸内核无 tools 插件则跳过）。
	if reg, err := kernel.Use[api.ToolRegistry](k, "toolRegistry"); err == nil {
		reg.SetApprover(&tuiApprover{permCh: permCh})
	}
	return m, nil
}

// Init 进入时聚焦输入框并开始监听流式事件。
func (m *Model) Init() tea.Cmd {
	return tea.Batch(m.ta.Focus(), m.listen())
}

// listen 同时监听流式事件与权限确认请求，转成 bubbletea 消息（持续监听）。
func (m *Model) listen() tea.Cmd {
	return func() tea.Msg {
		select {
		case ev := <-m.streamCh:
			return ev
		case pr := <-m.permCh:
			return pr
		}
	}
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
		case m.overlay != "":
			return m.handleOverlayKey(msg)
		case m.perm != nil:
			return m.handlePermKey(msg)
		case msg.Type == tea.KeyCtrlP:
			if !m.running {
				m.openSessions()
			}
			return m, nil
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
		return m, m.listen()

	case *permRequest:
		m.perm = msg
		m.renderContent()
		return m, m.listen()

	case error:
		m.errStr = msg.Error()
	}

	return m, nil
}

// send 把当前输入框内容作为一轮用户输入发给 loop（后台 goroutine 跑，事件回流）。
// 以 "/" 开头的输入按斜杠命令处理，不发给模型。
func (m *Model) send() {
	text := strings.TrimSpace(m.ta.Value())
	if text == "" || m.running {
		return
	}
	if strings.HasPrefix(text, "/") {
		m.ta.Reset()
		m.runCommand(text)
		return
	}
	if m.title == "新会话" {
		m.title = truncate(text, 48)
	}
	m.ta.Reset()
	m.running = true
	m.runStart = time.Now()
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
					rest, diff := tools.ExtractDiff(se.data)
					m.messages[i].diff = diff
					m.messages[i].detail = rest
					m.messages[i].note = toolNote(se.err, diff, rest)
					break
				}
			}
		case "done":
			m.running = false
			if !m.runStart.IsZero() {
				m.lastDur = time.Since(m.runStart)
				m.runStart = time.Time{}
			}
			m.ta.Focus()
		case "error":
			m.errStr = se.text
			m.running = false
			if !m.runStart.IsZero() {
				m.lastDur = time.Since(m.runStart)
				m.runStart = time.Time{}
			}
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
			b.WriteString(renderToolLine(dm, w))
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

// renderToolLine 工具行：`* Tool args` / 读类工具用 `→`，下方渲染 diff 与输出详情。
func renderToolLine(dm displayMsg, w int) string {
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

	var extra strings.Builder
	if dm.diff != "" {
		extra.WriteString("\n")
		extra.WriteString(renderDiffBlock(dm.diff, w))
	}
	if dm.detail != "" {
		if d := renderToolDetail(dm.detail, w); d != "" {
			extra.WriteString("\n")
			extra.WriteString(d)
		}
	}
	return line + extra.String()
}

// renderDiffBlock 把统一 diff 渲染成彩色（+/绿、-/红、块头/文件头/灰），按宽度折行。
func renderDiffBlock(diff string, w int) string {
	var b strings.Builder
	for _, ln := range strings.Split(diff, "\n") {
		if ln == "" {
			continue
		}
		var st lipgloss.Style
		switch {
		case strings.HasPrefix(ln, "+"):
			st = styleDiffAdd
		case strings.HasPrefix(ln, "-"):
			st = styleDiffDel
		case strings.HasPrefix(ln, "@@"):
			st = styleDim
		case strings.HasPrefix(ln, "--- "), strings.HasPrefix(ln, "+++ "):
			st = styleDim
		default:
			st = styleText
		}
		wrapped := wrapText(st.Render(ln), w-3)
		parts := strings.Split(wrapped, "\n")
		for i, p := range parts {
			if i == 0 {
				b.WriteString("   ")
			} else {
				b.WriteString("     ")
			}
			b.WriteString(p)
			b.WriteString("\n")
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

// renderToolDetail 工具输出详情（bash/grep/read 等）：暗色左条 + 折行，超长截断。
func renderToolDetail(content string, w int) string {
	content = strings.TrimRight(content, "\n")
	if strings.TrimSpace(content) == "" {
		return ""
	}
	lines := strings.Split(content, "\n")
	const maxDetail = 40
	more := 0
	if len(lines) > maxDetail {
		more = len(lines) - maxDetail
		lines = lines[:maxDetail]
	}
	body := strings.Join(lines, "\n")
	if more > 0 {
		body += fmt.Sprintf("\n… [+%d 行]", more)
	}
	wrapped := wrapText(body, w-3)
	return lipgloss.NewStyle().
		Border(lipgloss.Border{Left: "┃"}, false, false, false, true).
		BorderForeground(cDim).
		Padding(0, 1).
		Width(maxInt(w-3, 6)).
		Render(wrapped)
}

// View 渲染整屏：标题条 + 消息区（或浮层）+ 底栏。
func (m *Model) View() string {
	if !m.ready {
		return styleMuted.Render("loading…")
	}
	w := maxInt(m.width, 20)
	if m.overlay != "" {
		return m.renderTitleBar(w) + "\n" + m.renderOverlay(w) + "\n" + m.renderFooter(w)
	}
	footer := m.renderFooter(w)
	if m.perm != nil {
		footer = m.renderPermFooter(w)
	}
	return m.renderTitleBar(w) + "\n" + m.vp.View() + "\n" + footer
}

// renderTitleBar 顶部圆角标题条：`# 标题` + 右对齐 耗时 · 条数 · session。
func (m *Model) renderTitleBar(w int) string {
	left := "# " + m.title
	right := shortID(m.sessID)
	if m.running && !m.runStart.IsZero() {
		right = fmt.Sprintf("运行中 %s · %s", fmtDur(time.Since(m.runStart)), shortID(m.sessID))
	} else if m.lastDur > 0 {
		right = fmt.Sprintf("%s · %d 条 · %s", fmtDur(m.lastDur), len(m.messages), shortID(m.sessID))
	} else {
		right = fmt.Sprintf("%d 条 · %s", len(m.messages), shortID(m.sessID))
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

// renderPermFooter 权限确认底栏：替换输入框，给出 allow / always / deny 选项。
// 故意不用 emoji（✻/⚠ 在 Cascadia Mono、Consolas 无字形会变豆腐块），只用文字标记。
func (m *Model) renderPermFooter(w int) string {
	badge := "  " + styleAccent.Render("◆") + " " + styleAccent.Render(m.mode) + styleMuted.Render(" · "+m.model)

	pr := m.perm
	label := styleAccent.Render("确认 ") + styleText.Render(pr.name)
	if a := argSummary(pr.args); a != "" {
		label += styleDim.Render("  → ") + styleText.Render(a)
	}
	keys := styleDim.Render("  [a] 允许    [A] 总是允许    [d / esc] 拒绝")
	inner := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(cAccent).
		Background(cInput).
		Width(maxInt(w-2, 10)).
		Padding(0, 1).
		Render(label + "\n" + keys)

	modeRow := "  " + styleAccent.Render(m.mode) + "  " + styleText.Render(m.model)
	if m.provider != "" {
		modeRow += "  " + styleDim.Render(m.provider)
	}
	hint := padStyled(
		styleDim.Render("  权限确认中…"),
		styleDim.Render("  ctrl+c 退出  "),
		w,
	)

	return badge + "\n" + inner + "\n" + modeRow + "\n" + hint
}

// ---------- 浮层与命令 ----------

// runCommand 处理斜杠命令（输入以 / 开头，已在 send 里分流）。
func (m *Model) runCommand(text string) {
	switch strings.Fields(text)[0] {
	case "/help", "/?":
		m.overlay = "help"
	case "/sessions", "/s":
		m.openSessions()
	case "/new":
		m.newSession()
	case "/clear":
		m.newSession()
	default:
		m.errStr = "未知命令：" + strings.Fields(text)[0] + "（输入 /help 查看可用命令）"
	}
}

// newSession 开一个新会话并清屏（/new 与 /clear 都走这里）。
func (m *Model) newSession() {
	store, err := kernel.Use[api.SessionStore](m.k, "sessionStore")
	if err != nil {
		m.errStr = err.Error()
		return
	}
	s, err := store.New()
	if err != nil {
		m.errStr = err.Error()
		return
	}
	m.sessID = s.ID()
	m.title = "新会话"
	m.messages = nil
	m.lastDur = 0
	m.runStart = time.Time{}
	m.renderContent()
}

// openSessions 载入会话列表并打开选择浮层。
func (m *Model) openSessions() {
	store, err := kernel.Use[api.SessionStore](m.k, "sessionStore")
	if err != nil {
		m.errStr = err.Error()
		return
	}
	list, err := store.ListMeta()
	if err != nil {
		m.errStr = err.Error()
		return
	}
	m.sessList = list
	if m.sessIdx >= len(list) {
		m.sessIdx = 0
	}
	m.overlay = "sessions"
}

// handleOverlayKey 浮层键盘路由（help / sessions）。
func (m *Model) handleOverlayKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch m.overlay {
	case "help":
		m.overlay = ""
		return m, nil
	case "sessions":
		return m.handleSessionsKey(msg)
	}
	m.overlay = ""
	return m, nil
}

// handlePermKey 权限确认键位：a 允许 / A 总是允许 / d 或 esc 拒绝。
// 决定写回审批器的 resp 通道，loop goroutine 据此放行或拒止本次工具调用。
func (m *Model) handlePermKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	var d permDecision
	switch msg.String() {
	case "a", "y", "enter":
		d = permDecision{allow: true}
	case "A":
		d = permDecision{allow: true, always: true}
	case "d", "n", "esc":
		d = permDecision{allow: false}
	default:
		return m, m.listen()
	}
	m.perm.resp <- d
	m.perm = nil
	return m, m.listen()
}

// handleSessionsKey 会话浮层导航：↑/↓ 选择，enter 打开，d 删除，esc 取消。
func (m *Model) handleSessionsKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch {
	case msg.Type == tea.KeyEsc:
		m.overlay = ""
	case msg.Type == tea.KeyUp || msg.String() == "k":
		if m.sessIdx > 0 {
			m.sessIdx--
		}
	case msg.Type == tea.KeyDown || msg.String() == "j":
		if m.sessIdx < len(m.sessList)-1 {
			m.sessIdx++
		}
	case msg.Type == tea.KeyEnter:
		if m.sessIdx < len(m.sessList) {
			m.switchSession(m.sessList[m.sessIdx].ID, m.sessList[m.sessIdx].Title)
		}
		m.overlay = ""
	case msg.String() == "d":
		if m.sessIdx < len(m.sessList) {
			id := m.sessList[m.sessIdx].ID
			if store, err := kernel.Use[api.SessionStore](m.k, "sessionStore"); err == nil {
				_ = store.Delete(id)
				m.sessList = append(m.sessList[:m.sessIdx], m.sessList[m.sessIdx+1:]...)
				if m.sessIdx >= len(m.sessList) {
					m.sessIdx = maxInt(len(m.sessList)-1, 0)
				}
			}
		}
	}
	return m, nil
}

// switchSession 切到指定会话并回放其消息到 viewport。
func (m *Model) switchSession(id, title string) {
	store, err := kernel.Use[api.SessionStore](m.k, "sessionStore")
	if err != nil {
		m.errStr = err.Error()
		return
	}
	sess, err := store.Open(id)
	if err != nil {
		m.errStr = err.Error()
		return
	}
	m.sessID = id
	if title != "" {
		m.title = title
	}
	m.replaySession(sess)
}

// replaySession 把落盘会话消息重建成展示消息（用户块 / 助手 markdown / 工具行+diff）。
func (m *Model) replaySession(sess api.Session) {
	msgs := sess.Messages()
	toolRes := map[string]api.Message{}
	for _, mm := range msgs {
		if mm.Role == api.RoleTool && mm.ToolCallID != "" {
			toolRes[mm.ToolCallID] = mm
		}
	}
	out := make([]displayMsg, 0, len(msgs))
	for _, mm := range msgs {
		switch mm.Role {
		case api.RoleUser:
			if strings.TrimSpace(mm.Content) != "" {
				out = append(out, displayMsg{role: roleUser, text: mm.Content})
			}
		case api.RoleAssistant:
			if len(mm.ToolCalls) > 0 {
				for _, tc := range mm.ToolCalls {
					dm := displayMsg{role: roleTool, id: tc.ID, name: tc.Name, args: argSummary(tc.Args), state: "done"}
					if res, ok := toolRes[tc.ID]; ok {
						rest, diff := tools.ExtractDiff(res.Content)
						dm.diff, dm.detail, dm.note = diff, rest, toolNote(false, diff, rest)
					}
					out = append(out, dm)
				}
			}
			if strings.TrimSpace(mm.Content) != "" {
				out = append(out, displayMsg{role: roleAsst, text: mm.Content})
			}
		}
	}
	m.messages = out
	m.lastDur = 0
	m.runStart = time.Time{}
	m.renderContent()
}

// renderOverlay 浮层主体（标题条与底栏之间的区域）。
func (m *Model) renderOverlay(w int) string {
	switch m.overlay {
	case "help":
		return m.renderHelp(w)
	case "sessions":
		return m.renderSessions(w)
	}
	return ""
}

// renderHelp 帮助浮层：命令与键位说明。
func (m *Model) renderHelp(w int) string {
	lines := []string{
		styleAccent.Render("  命令"),
		"    /help      显示本帮助",
		"    /sessions  打开会话列表（也可按 Ctrl+P）",
		"    /new       开新会话",
		"    /clear     清空当前会话",
		"",
		styleAccent.Render("  键位"),
		"    Enter      发送（Shift+Enter 换行）",
		"    Esc       运行中停止 / 关闭浮层",
		"    Ctrl+C     退出",
		"    Ctrl+P     会话列表",
		"    ↑/↓ 或 j/k 会话列表内移动",
		"    d         会话列表内删除选中项",
		"",
		styleDim.Render("  按任意键关闭"),
	}
	body := strings.Join(lines, "\n")
	box := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(cBorder).
		Background(cElem).
		Padding(0, 1).
		Width(maxInt(w-2, 20)).
		Render(body)
	return lipgloss.Place(w, maxInt(w/2, 12), lipgloss.Center, lipgloss.Center, box)
}

// renderSessions 会话浮层：列出会话，高亮当前选中项。
func (m *Model) renderSessions(w int) string {
	var b strings.Builder
	b.WriteString(styleAccent.Render("  会话（↑/↓ 选择 · enter 打开 · d 删除 · esc 取消）") + "\n\n")
	if len(m.sessList) == 0 {
		b.WriteString(styleDim.Render("    （暂无历史会话）"))
	}
	for i, s := range m.sessList {
		mark := "  "
		line := "  " + truncate(s.Title, maxInt(w-12, 10))
		if i == m.sessIdx {
			mark = styleAccent.Render("▶ ")
			line = styleAccent.Render(line)
		}
		when := s.Updated.Format("01-02 15:04")
		tail := styleDim.Render(fmt.Sprintf("  %s  %s", when, shortID(s.ID)))
		// 让标题与尾部之间用空格补齐到宽度
		gap := maxInt(w-4, 8) - lipgloss.Width(line) - lipgloss.Width(tail)
		if gap < 1 {
			gap = 1
		}
		b.WriteString(mark + line + strings.Repeat(" ", gap) + tail + "\n")
	}
	return b.String()
}

// fmtDur 把时长格式化为 m:ss 或 s（顶栏真实耗时，不编 token/cost）。
func fmtDur(d time.Duration) string {
	d = d.Round(time.Second)
	if d < 0 {
		d = 0
	}
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	mm := int(d.Minutes())
	ss := int(d.Seconds()) % 60
	return fmt.Sprintf("%dm%02ds", mm, ss)
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

// toolNote 工具结果摘要：diff 标 "diff"，否则标输出行数；失败显式标 failed。
func toolNote(isErr bool, diff, detail string) string {
	if isErr {
		return "failed"
	}
	if strings.TrimSpace(detail) == "" && diff == "" {
		return ""
	}
	if diff != "" {
		return "diff"
	}
	n := 0
	for _, ln := range strings.Split(detail, "\n") {
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
