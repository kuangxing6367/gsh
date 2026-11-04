package ui

import (
	"strings"
	"testing"

	"gsh/internal/api"
)

// fakeSess 实现 api.Session，便于脱离磁盘测试轨迹渲染。
type fakeSess struct {
	id   string
	msgs []api.Message
}

func (f fakeSess) ID() string                       { return f.id }
func (f fakeSess) Append(api.Message) error         { return nil }
func (f fakeSess) Messages() []api.Message          { return f.msgs }

func TestRenderTrajectory(t *testing.T) {
	sess := fakeSess{
		id: "20260101-000000-ab12",
		msgs: []api.Message{
			{Role: api.RoleUser, Content: "帮我写个文件"},
			{Role: api.RoleAssistant, ToolCalls: []api.ToolCall{
				{ID: "c1", Name: "writetest", Args: `{"path":"a.txt","content":"hi"}`},
			}},
			{Role: api.RoleTool, ToolCallID: "c1", Content: "wrote a.txt (2 bytes)\n<gsh:diff>\n--- a.txt\n+++ a.txt\n@@ -0,0 +1,1 @@\n+hi\n</gsh:diff>"},
			{Role: api.RoleAssistant, Content: "已写好 a.txt"},
		},
	}
	out := RenderTrajectory(sess)

	checks := map[string]string{
		"会话 id 标题": "20260101-000000-ab12",
		"用户块标记":   "◆ 你",
		"用户问题":     "帮我写个文件",
		"工具调用名":   "writetest",
		"diff 新增行":  "+hi",
		"助手标记":     "◇ 助手",
		"助手文本":     "已写好 a.txt",
	}
	for label, want := range checks {
		if !strings.Contains(out, want) {
			t.Errorf("RenderTrajectory 缺少 %s：期望含 %q\n--- 输出 ---\n%s", label, want, out)
		}
	}
}

func TestRenderTrajectoryEmpty(t *testing.T) {
	out := RenderTrajectory(fakeSess{id: "x", msgs: nil})
	if !strings.Contains(out, "x") {
		t.Errorf("空会话也应输出 id 标题，得到 %q", out)
	}
}
