package ui

import (
	"strings"
	"testing"

	"gsh/internal/api"
)

func TestSessionToMarkdown(t *testing.T) {
	msgs := []api.Message{
		{Role: api.RoleUser, Content: "帮我看目录"},
		{Role: api.RoleAssistant, Content: "好的", ToolCalls: []api.ToolCall{{ID: "c1", Name: "bash", Args: `{"command":"ls"}`}}},
		{Role: api.RoleTool, ToolCallID: "c1", Content: "LICENSE\nREADME.md\n<gsh:diff>\n--- a\n+++ b\n@@ -1 +1,2 @@\n line\n+new\n</gsh:diff>"},
		{Role: api.RoleAssistant, Content: "目录里有这些文件。"},
	}
	md := SessionToMarkdown(msgs, "测试会话", "20260101-120000-ab12")
	checks := []string{
		"# 测试会话",
		"20260101-120000-ab12",
		"## 你",
		"帮我看目录",
		"### 调用 `bash`",
		"```json",
		`"command": "ls"`,
		"## 工具结果",
		"```diff",
		"+new",
		"目录里有这些文件。",
	}
	for _, c := range checks {
		if !strings.Contains(md, c) {
			t.Errorf("SessionToMarkdown 输出缺少 %q\n--- 实际输出 ---\n%s", c, md)
		}
	}
}
