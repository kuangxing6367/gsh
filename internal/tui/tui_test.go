package tui

import (
	"context"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"gsh/internal/api"
	"gsh/internal/kernel"
)

// fakeLoop 测试用 loop：不真正调用模型，只满足 api.Loop 接口。
type fakeLoop struct{}

func (fakeLoop) Run(_ context.Context, _ string, _ api.StreamDelta) (string, error) {
	return "", nil
}
func (fakeLoop) RunTraced(_ context.Context, _ string, _ api.StreamDelta, _ api.EventHandler) (string, error) {
	return "", nil
}
func (fakeLoop) RunSession(_ context.Context, _ string, _ string, _ api.StreamDelta, _ api.EventHandler) (string, error) {
	return "", nil
}
func (fakeLoop) RunWithParts(_ context.Context, _, _ string, _ []api.ContentPart, _ api.StreamDelta, _ api.EventHandler) (string, error) {
	return "", nil
}

func newTestModel(t *testing.T, w, h int) *Model {
	t.Helper()
	k := kernel.New(&kernel.Config{DataDir: t.TempDir()})
	m, err := NewModel(k, "testsession123", fakeLoop{}, false)
	if err != nil {
		t.Fatal(err)
	}
	m.Update(tea.WindowSizeMsg{Width: w, Height: h})
	return m
}

// TestTUIView 验证终端 UI 的关键视觉元素都正确渲染且不崩溃。
func TestTUIView(t *testing.T) {
	m := newTestModel(t, 100, 32)

	m.messages = []displayMsg{
		{role: roleUser, text: "你好 gsh"},
		{role: roleTool, id: "c1", name: "bash", args: "uptime", state: "done", note: "(1)"},
		{role: roleTool, id: "c2", name: "read_file", args: "a.go", state: "error", note: "failed"},
		{role: roleAsst, text: "这是助手回复"},
	}
	m.renderContent()
	view := m.View()

	checks := map[string]string{
		"title bar":       "# 新会话",
		"session label":   "testsess",
		"user text":       "你好 gsh",
		"user accent bar": "┃",
		"exec tool line":  "* bash",
		"read tool arrow": "→ read_file",
		"assistant":       "这是助手回复",
		"model badge":     "◆",
		"mode label":      "标准",
		"hint":            "enter 发送",
	}
	for label, want := range checks {
		if !strings.Contains(view, want) {
			t.Errorf("%s: view 缺少 %q\nview=\n%s", label, want, view)
		}
	}

	// 布局不应明显超出终端宽度（lipgloss.Width 会剥离 ANSI）。
	for i, line := range strings.Split(view, "\n") {
		if w := lipgloss.Width(line); w > 102 {
			t.Errorf("第 %d 行宽度 %d 超出终端 100\n%s", i, w, line)
		}
	}
}

// TestTUIStreamFlow 验证流式事件能正确更新消息列表（delta 累积 / 工具配对 / done 复位）。
func TestTUIStreamFlow(t *testing.T) {
	m := newTestModel(t, 80, 24)

	m.handleStream(streamEvent{kind: "delta", text: "你"})
	m.handleStream(streamEvent{kind: "delta", text: "好"})
	if len(m.messages) != 1 || m.messages[0].role != roleAsst || m.messages[0].text != "你好" {
		t.Fatalf("delta 累积错误: %+v", m.messages)
	}

	m.handleStream(streamEvent{kind: "tool_call", id: "t1", name: "bash", text: `{"command":"ls -la"}`})
	if len(m.messages) != 2 || m.messages[1].state != "pending" || m.messages[1].args != "ls -la" {
		t.Fatalf("tool_call 状态/参数错误: %+v", m.messages[1])
	}
	m.handleStream(streamEvent{kind: "tool_result", id: "t1", name: "bash", data: "a\nb\nc", err: false})
	if m.messages[1].state != "done" || m.messages[1].note != "(3)" {
		t.Fatalf("tool_result 更新错误: %+v", m.messages[1])
	}

	m.running = true
	m.handleStream(streamEvent{kind: "done"})
	if m.running {
		t.Fatal("done 后 running 应复位")
	}
}

func TestArgSummary(t *testing.T) {
	cases := map[string]string{
		`{"command":"uptime"}`:       "uptime",
		`{"pattern":"Home.*button"}`: "Home.*button",
		`{"path":"a/b.go"}`:          "a/b.go",
		``:                           "",
	}
	for in, want := range cases {
		if got := argSummary(in); got != want {
			t.Errorf("argSummary(%q)=%q, want %q", in, got, want)
		}
	}
}

func TestToolNote(t *testing.T) {
	if got := toolNote(false, "", "a\n\nb\n"); got != "(2)" {
		t.Errorf("toolNote lines=%q", got)
	}
	if got := toolNote(true, "", "x"); got != "failed" {
		t.Errorf("toolNote err=%q", got)
	}
	if got := toolNote(false, "", ""); got != "" {
		t.Errorf("toolNote empty=%q", got)
	}
	if got := toolNote(false, "--- a\n+++ b\n@@ -1 +1 @@\n-x\n+y\n", ""); got != "diff" {
		t.Errorf("toolNote diff=%q", got)
	}
	if got := toolNote(false, "", "x\ny\nz"); got != "(3)" {
		t.Errorf("toolNote detail=%q", got)
	}
}

func TestPermLabel(t *testing.T) {
	cases := map[string]string{"read_only": "只读", "standard": "标准", "full": "完全权限", "": "标准"}
	for in, want := range cases {
		if got := permLabel(in); got != want {
			t.Errorf("permLabel(%q)=%q, want %q", in, got, want)
		}
	}
}

// feedKey 向模型投一个字符按键（如 'a' / 'A' / 'd'），返回更新后的模型。
func feedKey(m *Model, r rune) *Model {
	mm, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
	return mm.(*Model)
}

// TestTUIPermissionPrompt 验证标准档位下高风险工具会弹出确认浮层，
// 且 a/A/d 三种按键能把正确的裁决写回审批器（否则 loop goroutine 会一直阻塞）。
func TestTUIPermissionPrompt(t *testing.T) {
	// 允许：按 a
	m := newTestModel(t, 100, 32)
	ch := make(chan permDecision, 1)
	m.perm = &permRequest{name: "bash", args: `{"command":"ls -la"}`, resp: ch}
	view := m.View()
	for _, want := range []string{"确认", "bash", "[a] 允许", "[A] 总是允许", "[d / esc] 拒绝", "ls -la"} {
		if !strings.Contains(view, want) {
			t.Errorf("确认浮层缺少 %q\nview=\n%s", want, view)
		}
	}
	feedKey(m, 'a')
	select {
	case d := <-ch:
		if !d.allow || d.always {
			t.Errorf("按 a → 应 allow 且非 always，got %+v", d)
		}
	case <-time.After(time.Second):
		t.Fatal("审批器未收到裁决（loop 会阻塞）")
	}
	if m.perm != nil {
		t.Error("确认后 m.perm 应清空")
	}

	// 总是允许：按 A
	m2 := newTestModel(t, 100, 32)
	ch2 := make(chan permDecision, 1)
	m2.perm = &permRequest{name: "write_file", args: `{"path":"a.go"}`, resp: ch2}
	feedKey(m2, 'A')
	select {
	case d := <-ch2:
		if !d.allow || !d.always {
			t.Errorf("按 A → 应 allow 且 always，got %+v", d)
		}
	case <-time.After(time.Second):
		t.Fatal("审批器未收到裁决")
	}

	// 拒绝：按 d
	m3 := newTestModel(t, 100, 32)
	ch3 := make(chan permDecision, 1)
	m3.perm = &permRequest{name: "delete_path", args: `{"path":"x"}`, resp: ch3}
	feedKey(m3, 'd')
	select {
	case d := <-ch3:
		if d.allow {
			t.Errorf("按 d → 应 deny，got %+v", d)
		}
	case <-time.After(time.Second):
		t.Fatal("审批器未收到裁决")
	}
}

// TestReadOnlyBlocksSend 验证只读模式下 send 不发送新消息、不进入 running。
func TestReadOnlyBlocksSend(t *testing.T) {
	m := newTestModel(t, 100, 32)
	m.readOnly = true
	m.ta.SetValue("这条不该发出去")
	m.send()
	if m.running {
		t.Error("只读模式 send 不应进入 running")
	}
	if len(m.messages) != 0 {
		t.Errorf("只读模式 send 不应追加用户消息，got %d", len(m.messages))
	}
}

// TestToggleReadOnly 验证 /readonly 在只读与可编辑之间切换。
func TestToggleReadOnly(t *testing.T) {
	m := newTestModel(t, 100, 32)
	if m.readOnly {
		t.Fatal("默认应为可编辑")
	}
	m.runCommand("/readonly")
	if !m.readOnly {
		t.Error("/readonly 后应进入只读")
	}
	m.runCommand("/readonly")
	if m.readOnly {
		t.Error("再次 /readonly 应切回可编辑")
	}
}

// TestReadOnlyTitleBar 验证只读模式顶栏显示「只读浏览」标记。
func TestReadOnlyTitleBar(t *testing.T) {
	m := newTestModel(t, 100, 32)
	m.readOnly = true
	m.messages = append(m.messages, displayMsg{role: roleUser, text: "hi"})
	v := m.View()
	if !strings.Contains(v, "只读浏览") {
		t.Errorf("只读顶栏应含「只读浏览」，got:\n%s", v)
	}
}

// TestCycleDetail 展示档按 0→1→2→3→0 循环，超出范围不会越界。
func TestCycleDetail(t *testing.T) {
	m := newTestModel(t, 100, 30)
	m.detail = 0
	for _, want := range []int{1, 2, 3, 0} {
		m.cycleDetail()
		if m.detail != want {
			t.Fatalf("档位应为 %d，实际 %d", want, m.detail)
		}
	}
}

// TestDetailCommand 斜杠命令能直接指定展示档，非法值只提示不改状态。
func TestDetailCommand(t *testing.T) {
	m := newTestModel(t, 100, 30)
	m.runCommand("/detail 0")
	if m.detail != 0 {
		t.Fatalf("应为简洁档，实际 %d", m.detail)
	}
	m.runCommand("/detail 3")
	if m.detail != 3 {
		t.Fatalf("应为完全档，实际 %d", m.detail)
	}
	m.runCommand("/detail 9")
	if m.detail != 3 {
		t.Fatalf("非法参数不应改档，实际 %d", m.detail)
	}
	if m.errStr == "" {
		t.Fatal("非法参数应给出错误提示")
	}
}

// TestToolLineHiddenInSimpleMode 简洁档不渲染参数与结果正文。
func TestToolLineHiddenInSimpleMode(t *testing.T) {
	dm := displayMsg{
		role:   roleTool,
		name:   "bash",
		args:   "go test ./...",
		state:  "done",
		detail: "ok  gsh/internal/tools",
		note:   "(20)",
	}
	simple := renderToolLine(dm, 100, 0)
	if strings.Contains(simple, "go test") || strings.Contains(simple, "ok  gsh") {
		t.Fatalf("简洁档不应显示参数/正文: %q", simple)
	}
	standard := renderToolLine(dm, 100, 1)
	if !strings.Contains(standard, "go test") {
		t.Fatalf("标准档应显示参数: %q", standard)
	}
	if !strings.Contains(standard, "ok  gsh") {
		t.Fatalf("标准档应显示结果正文: %q", standard)
	}
}

// TestReasonHiddenInSimpleMode 简洁档不显示思考行。
func TestReasonHiddenInSimpleMode(t *testing.T) {
	if got := renderReasonLine("想一想", 80, 0); got != "" {
		t.Fatalf("简洁档应隐藏思考: %q", got)
	}
	if got := renderReasonLine("想一想", 80, 1); !strings.Contains(got, "思考") {
		t.Fatalf("标准档应显示折叠思考: %q", got)
	}
}

// TestTodoProgress 底栏进度只统计 done。
func TestTodoProgress(t *testing.T) {
	items := []api.TodoItem{
		{Text: "a", Status: "done"},
		{Text: "b", Status: "in_progress"},
		{Text: "c", Status: "pending"},
	}
	if got := todoProgress(items); got != "计划 1/3" {
		t.Fatalf("进度文案错误: %q", got)
	}
	if got := todoProgress(nil); got != "" {
		t.Fatalf("空清单不应有进度: %q", got)
	}
}

// TestTodoEventUpdatesModel todo 事件到达后模型持有清单（底栏据此显示进度）。
func TestTodoEventUpdatesModel(t *testing.T) {
	m := newTestModel(t, 100, 30)
	m.handleStream(streamEvent{kind: "todo", todo: []api.TodoItem{{Text: "写代码", Status: "in_progress"}}})
	if len(m.todo) != 1 {
		t.Fatalf("清单未更新: %d", len(m.todo))
	}
	if !strings.Contains(m.renderFooter(100), "计划 0/1") {
		t.Fatal("底栏应显示计划进度")
	}
}
