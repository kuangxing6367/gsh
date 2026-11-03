package tui

import (
	"regexp"
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"
)

var ansiRe = regexp.MustCompile("\x1b\\[[0-9;]*m")

func stripANSI(s string) string { return ansiRe.ReplaceAllString(s, "") }

// TestMarkdownBlocks 验证块级语法：标记被吃掉、内容仍在。
func TestMarkdownBlocks(t *testing.T) {
	src := `# 一级标题

## 二级标题

这是 **粗体** 和 *斜体* 以及 ` + "`行内码`" + ` 的段落。

- 第一项
- 第二项

1. 有序一
2. 有序二

> 这是一段引用

[文档](https://example.com/doc)

---

` + "```go" + `
func main() {
	println("hi")
}
` + "```"

	out := stripANSI(renderMarkdown(src, 80))

	for _, want := range []string{
		"一级标题", "二级标题", "粗体", "斜体", "行内码",
		"• 第一项", "• 第二项", "1. 有序一", "2. 有序二",
		"这是一段引用", "文档", "https://example.com/doc",
		"func main() {", `println("hi")`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("渲染结果缺少 %q\n---\n%s", want, out)
		}
	}

	// 原始标记不应残留。
	for _, banned := range []string{"**", "```", "## ", "# 一级", "> 这是"} {
		if strings.Contains(out, banned) {
			t.Errorf("渲染结果残留原始标记 %q\n---\n%s", banned, out)
		}
	}
	if strings.Count(out, "─") < 3 {
		t.Errorf("分割线未渲染: %q", out)
	}
}

// TestMarkdownUnderscoreNotEmphasis 确认 snake_case / 文件名不被当成斜体。
func TestMarkdownUnderscoreNotEmphasis(t *testing.T) {
	out := stripANSI(inline("call tool_name_here and read my_file.go"))
	if out != "call tool_name_here and read my_file.go" {
		t.Errorf("下划线误判: %q", out)
	}
	// 两侧为词边界的 _x_ 仍应作为斜体（标记被吃掉）。
	if got := stripANSI(inline("an _italic_ word")); got != "an italic word" {
		t.Errorf("行内斜体渲染错误: %q", got)
	}
}

// TestMarkdownUnclosedCodeFence 未闭合代码块不能丢内容。
func TestMarkdownUnclosedCodeFence(t *testing.T) {
	out := stripANSI(renderMarkdown("```\nline one\nline two", 60))
	if !strings.Contains(out, "line one") || !strings.Contains(out, "line two") {
		t.Errorf("未闭合代码块丢内容: %q", out)
	}
}

// TestMarkdownWidth 中文长段落与长代码行都不能超出给定宽度。
func TestMarkdownWidth(t *testing.T) {
	src := strings.Repeat("这是一段很长的中文描述，用来验证折行是否正确处理全角字符。", 3) +
		"\n\n- " + strings.Repeat("列表项内容很长", 6) +
		"\n\n```\n" + strings.Repeat("x", 200) + "\n```"
	out := renderMarkdown(src, 50)
	for i, line := range strings.Split(out, "\n") {
		if w := lipgloss.Width(line); w > 50 {
			t.Errorf("第 %d 行宽度 %d 超过 50: %q", i, w, stripANSI(line))
		}
	}
}

// TestMarkdownInTUI 确认 TUI 的助手消息走 markdown 渲染。
func TestMarkdownInTUI(t *testing.T) {
	m := newTestModel(t, 100, 32)
	m.messages = []displayMsg{{role: roleAsst, text: "## 结论\n\n用 **make build** 即可。"}}
	m.renderContent()
	view := stripANSI(m.View())
	if strings.Contains(view, "## ") || strings.Contains(view, "**") {
		t.Errorf("TUI 未渲染 markdown:\n%s", view)
	}
	if !strings.Contains(view, "结论") || !strings.Contains(view, "make build") {
		t.Errorf("TUI 丢失 markdown 内容:\n%s", view)
	}
}
