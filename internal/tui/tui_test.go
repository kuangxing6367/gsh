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

func newTestModel(t *testing.T, w, h int) *Model {
	t.Helper()
	k := kernel.New(&kernel.Config{DataDir: t.TempDir()})
	m, err := NewModel(k, "testsession123", fakeLoop{})
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
