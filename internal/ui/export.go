// Package ui 提供非交互展示能力：终端回放 / Markdown 导出。
package ui

import (
	"encoding/json"
	"fmt"
	"strings"

	"gsh/internal/api"
	"gsh/internal/tools"
)

// SessionToMarkdown 把一段会话渲染为 Markdown 文档，便于归档或分享。
// 用户/助手正文原样保留（可能含 Markdown），工具调用展开为 JSON 代码块，
// 工具结果中的统一 diff 单独抽取为 diff 代码块。
func SessionToMarkdown(msgs []api.Message, title, id string) string {
	var b strings.Builder
	b.WriteString("# " + title + "\n\n")
	b.WriteString(fmt.Sprintf("> 会话 ID：`%s`  \n", id))
	b.WriteString("> 由 gsh 导出\n\n")
	b.WriteString("---\n\n")
	for _, m := range msgs {
		switch m.Role {
		case api.RoleUser:
			if strings.TrimSpace(m.Content) == "" {
				continue
			}
			b.WriteString("## 你\n\n")
			b.WriteString(m.Content + "\n\n")
		case api.RoleAssistant:
			if strings.TrimSpace(m.Content) != "" {
				b.WriteString("## 助手\n\n")
				b.WriteString(m.Content + "\n\n")
			}
			for _, tc := range m.ToolCalls {
				b.WriteString(fmt.Sprintf("### 调用 `%s`\n\n", tc.Name))
				b.WriteString("```json\n" + prettyJSON(tc.Args) + "\n```\n\n")
			}
		case api.RoleTool:
			b.WriteString("## 工具结果\n\n")
			rest, diff := tools.ExtractDiff(m.Content)
			if strings.TrimSpace(rest) != "" {
				b.WriteString(rest + "\n\n")
			}
			if diff != "" {
				b.WriteString("```diff\n" + diff + "\n```\n\n")
			}
		}
	}
	return b.String()
}

// prettyJSON 尽量把 JSON 字符串美化；解析失败则原样返回（不编造格式）。
func prettyJSON(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	var v interface{}
	if err := json.Unmarshal([]byte(s), &v); err != nil {
		return s
	}
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return s
	}
	return string(b)
}
