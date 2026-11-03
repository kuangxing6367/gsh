package tui

import (
	"context"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
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

// TestTUIView 验证 opencode 风格的关键视觉元素都正确渲染且不崩溃。
func TestTUIView(t *testing.T) {
	k := kernel.New(&kernel.Config{DataDir: t.TempDir()})
	m, err := NewModel(k, "testsession123", fakeLoop{})
	if err != nil {
		t.Fatal(err)
	}
	// 触发窗口尺寸初始化（设置 viewport / 输入框尺寸并标记 ready）。
	m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})

	m.messages = []displayMsg{
		{role: roleUser, text: "你好 gsh"},
		{role: roleTool, id: "c1", name: "bash", state: "done", summary: "  uptime 12:00"},
		{role: roleTool, id: "c2", name: "read_file", state: "error", summary: "  文件不存在"},
		{role: roleAsst, text: "这是助手回复"},
	}
	m.renderContent()
	view := m.View()

	checks := map[string]string{
		"brand ⌬ gsh":         "⌬ gsh",
		"header session":      "session",
		"ok tool line":        "✓ bash",
		"error tool line":     "✗ read_file",
		"assistant content":   "这是助手回复",
		"user line":           "› 你好 gsh",
		"input prompt":        "› ",
	}
	for label, want := range checks {
		if !strings.Contains(view, want) {
			t.Errorf("%s: view 缺少 %q\nview=\n%s", label, want, view)
		}
	}
}

// TestTUIStreamFlow 验证流式事件能正确更新消息列表（delta 累积 / 工具配对 / done 复位）。
func TestTUIStreamFlow(t *testing.T) {
	k := kernel.New(&kernel.Config{DataDir: t.TempDir()})
	m, err := NewModel(k, "sess", fakeLoop{})
	if err != nil {
		t.Fatal(err)
	}
	m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})

	// delta 累积成 assistant 消息
	m.handleStream(streamEvent{kind: "delta", text: "你"})
	m.handleStream(streamEvent{kind: "delta", text: "好"})
	if len(m.messages) != 1 || m.messages[0].role != roleAsst || m.messages[0].text != "你好" {
		t.Fatalf("delta 累积错误: %+v", m.messages)
	}
	// tool_call → tool_result 配对
	m.handleStream(streamEvent{kind: "tool_call", id: "t1", name: "bash"})
	if len(m.messages) != 2 || m.messages[1].state != "pending" {
		t.Fatalf("tool_call 状态错误: %+v", m.messages)
	}
	m.handleStream(streamEvent{kind: "tool_result", id: "t1", name: "bash", data: "hello\nworld", err: false})
	if m.messages[1].state != "done" || m.messages[1].summary == "" {
		t.Fatalf("tool_result 更新错误: %+v", m.messages[1])
	}
	// done 复位 running
	m.running = true
	m.handleStream(streamEvent{kind: "done"})
	if m.running {
		t.Fatal("done 后 running 应复位")
	}
}
