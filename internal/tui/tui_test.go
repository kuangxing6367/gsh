package tui

import (
	"context"
	"strings"
	"testing"

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

// TestTUIView 验证 opencode 风格的关键视觉元素都正确渲染且不崩溃。
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
		"session label":   "session",
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
		`{"command":"uptime"}`:      "uptime",
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

func TestResultNote(t *testing.T) {
	if got := resultNote("a\n\nb\n", false); got != "(2)" {
		t.Errorf("resultNote lines=%q", got)
	}
	if got := resultNote("x", true); got != "failed" {
		t.Errorf("resultNote err=%q", got)
	}
	if got := resultNote("", false); got != "" {
		t.Errorf("resultNote empty=%q", got)
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
