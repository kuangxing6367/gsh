package tui

import (
	"strings"

	"github.com/charmbracelet/lipgloss"
)

// 轻量 markdown 渲染：只覆盖 LLM 回复里真正会用到的子集——标题、粗体、斜体、
// 行内码、代码块、有序/无序列表、引用、分割线、链接。
//
// 不引第三方 markdown 库：渲染目标不是 HTML 而是终端单元格，样式必须落到
// lipgloss 的 ANSI 上；自己走一遍行级状态机比给通用库写 renderer 更直接，
// 也避免把 TUI 的依赖面撑大（内核保持 stdlib + 两个 UI 库）。
//
// 不支持表格与嵌套（嵌套强调只做一层），见 README「已知差距」。

var (
	styleCodeInline = lipgloss.NewStyle().
			Foreground(lipgloss.Color("#f0f0f0")).
			Background(lipgloss.Color("#232323"))
	styleCodeLine = lipgloss.NewStyle().
			Foreground(lipgloss.Color("#d9d9d9"))
	styleLink = lipgloss.NewStyle().
			Foreground(cAccent).
			Underline(true)
	styleH1 = lipgloss.NewStyle().Foreground(cText).Bold(true)
	styleH2 = lipgloss.NewStyle().Foreground(cText).Bold(true)
	styleH3 = lipgloss.NewStyle().Foreground(cMuted).Bold(true)
)

// renderMarkdown 把 markdown 源文本渲染成带 ANSI 样式的终端文本，宽度不超过 width。
func renderMarkdown(src string, width int) string {
	w := maxInt(width, 10)
	lines := strings.Split(strings.ReplaceAll(src, "\r\n", "\n"), "\n")

	var (
		b       strings.Builder
		inCode  bool
		lang    string
		code    []string
		blank   bool // 上一条输出是否已是空行（连续空行合并）
		flushed bool
	)

	flushCode := func() {
		b.WriteString(renderCodeBlock(lang, strings.Join(code, "\n"), w))
		b.WriteString("\n")
		code = nil
		lang = ""
		blank = false // 代码块后紧跟的空行要保留，否则与下一段贴死
	}

	for _, raw := range lines {
		line := strings.TrimRight(raw, " \t")
		fence := strings.HasPrefix(strings.TrimSpace(line), "```")

		if inCode {
			if fence {
				inCode = false
				flushCode()
				continue
			}
			code = append(code, line)
			continue
		}
		if fence {
			inCode = true
			lang = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "```"))
			continue
		}
		if strings.TrimSpace(line) == "" {
			if !blank && flushed {
				b.WriteString("\n")
			}
			blank = true
			continue
		}
		blank = false
		flushed = true
		b.WriteString(renderMarkdownLine(line, w))
		b.WriteString("\n")
	}
	if inCode && len(code) > 0 {
		flushCode()
	}
	return b.String()
}

// renderMarkdownLine 渲染单行块级语法（标题/引用/列表/分割线/段落）。
func renderMarkdownLine(line string, w int) string {
	indent := leadingSpaces(line)
	body := line[indent:]
	pad := strings.Repeat(" ", indent)

	if isHR(body) {
		return pad + styleDim.Render(strings.Repeat("─", maxInt(w-indent-pad0, 4)))
	}
	if lvl, txt, ok := heading(body); ok {
		switch lvl {
		case 1:
			return styleH1.Render(inline(txt))
		case 2:
			return styleH2.Render(inline(txt))
		default:
			return pad + styleH3.Render(inline(txt))
		}
	}
	if strings.HasPrefix(body, ">") {
		qt := strings.TrimSpace(strings.TrimPrefix(body, ">"))
		return lipgloss.NewStyle().
			Border(lipgloss.Border{Left: "┃"}, false, false, false, true).
			BorderForeground(cDim).
			Padding(0, 1).
			Width(maxInt(w-indent-2, 6)).
			Render(styleMuted.Render(inline(qt)))
	}
	if marker, rest, ok := splitList(body); ok {
		content := inline(rest)
		wrapW := maxInt(w-indent-lipgloss.Width(marker), 6)
		parts := strings.Split(wrapText(content, wrapW), "\n")
		var sb strings.Builder
		sb.WriteString(pad + styleMuted.Render(marker) + parts[0])
		for _, p := range parts[1:] {
			sb.WriteString("\n" + pad + strings.Repeat(" ", lipgloss.Width(marker)) + p)
		}
		return sb.String()
	}
	return pad + wrapText(inline(body), maxInt(w-indent, 8))
}

// renderCodeBlock 代码块：左侧竖条 + 浅底块，保留原始缩进（制表符展开成 4 空格）。
func renderCodeBlock(lang, body string, w int) string {
	body = strings.ReplaceAll(body, "\t", "    ")
	if strings.TrimRight(body, "\n") == "" {
		return ""
	}
	out := lipgloss.NewStyle().
		Border(lipgloss.Border{Left: "┃"}, false, false, false, true).
		BorderForeground(cDim).
		Background(cElem).
		Foreground(styleCodeLine.GetForeground()).
		Padding(0, 1).
		Width(maxInt(w-4, 8)).
		Render(body)
	if lang != "" {
		out = styleDim.Render("  "+lang) + "\n" + out
	}
	return out
}

// ---------- inline ----------

// inline 处理行内语法：行内码 `x`、粗体 **x**/__x__、斜体 *x*/_x_、链接 [t](u)。
func inline(s string) string {
	var b strings.Builder
	i := 0
	for i < len(s) {
		c := s[i]
		switch c {
		case '`':
			if j := strings.IndexByte(s[i+1:], '`'); j >= 0 {
				b.WriteString(styleCodeInline.Render(s[i+1 : i+1+j]))
				i += j + 2
				continue
			}
			b.WriteByte(c)
			i++

		case '*':
			if strings.HasPrefix(s[i:], "**") {
				if j := strings.Index(s[i+2:], "**"); j > 0 {
					b.WriteString(lipgloss.NewStyle().Bold(true).Render(inline(s[i+2 : i+2+j])))
					i += j + 4
					continue
				}
			} else if j := strings.IndexByte(s[i+1:], '*'); j > 0 {
				b.WriteString(lipgloss.NewStyle().Italic(true).Render(s[i+1 : i+1+j]))
				i += j + 2
				continue
			}
			b.WriteByte(c)
			i++

		case '_':
			// 只在两侧都是词边界时当强调，避免打断 snake_case / 文件名。
			prevWord := i > 0 && isWordByte(s[i-1])
			if !prevWord {
				if strings.HasPrefix(s[i:], "__") {
					if j := strings.Index(s[i+2:], "__"); j > 0 && !nextWordByte(s, i+2+j+2) {
						b.WriteString(lipgloss.NewStyle().Bold(true).Render(inline(s[i+2 : i+2+j])))
						i += j + 4
						continue
					}
				} else if j := strings.IndexByte(s[i+1:], '_'); j > 0 && !nextWordByte(s, i+1+j+1) {
					b.WriteString(lipgloss.NewStyle().Italic(true).Render(s[i+1 : i+1+j]))
					i += j + 2
					continue
				}
			}
			b.WriteByte(c)
			i++

		case '[':
			if text, url, next, ok := parseLink(s, i); ok {
				b.WriteString(styleLink.Render(text))
				if url != "" && url != text {
					b.WriteString(styleDim.Render(" (" + url + ")"))
				}
				i = next
				continue
			}
			b.WriteByte(c)
			i++

		default:
			b.WriteByte(c)
			i++
		}
	}
	return b.String()
}

// parseLink 解析 markdown 链接 [text](url)，返回文本、URL 与结束下标。
func parseLink(s string, i int) (text, url string, next int, ok bool) {
	end := strings.IndexByte(s[i:], ']')
	if end < 0 {
		return "", "", 0, false
	}
	end += i
	if end+1 >= len(s) || s[end+1] != '(' {
		return "", "", 0, false
	}
	closeParen := strings.IndexByte(s[end+2:], ')')
	if closeParen < 0 {
		return "", "", 0, false
	}
	closeParen += end + 2
	return s[i+1 : end], strings.TrimSpace(s[end+2 : closeParen]), closeParen + 1, true
}

// ---------- block helpers ----------

// heading 识别 ATX 标题，返回层级与去掉 # 号后的文本。
func heading(s string) (level int, text string, ok bool) {
	n := 0
	for n < len(s) && s[n] == '#' {
		n++
	}
	if n == 0 || n > 6 {
		return 0, "", false
	}
	// 宽松一点：# 后面允许直接跟正文（LLM 常写 "##标题" 不带空格），
	// 仅当紧跟着又是 # 时才不算（那是更深的标题层级，交给 n 上限处理）。
	if n < len(s) && s[n] == '#' {
		return 0, "", false
	}
	return n, strings.TrimSpace(s[n:]), true
}

// isHR 识别分割线（--- / *** / ___，至少 3 个）。
func isHR(s string) bool {
	t := strings.TrimSpace(s)
	if len(t) < 3 {
		return false
	}
	for _, c := range []byte{'-', '*', '_'} {
		if strings.Trim(string(t), string(c)) == "" && len(t) >= 3 {
			return true
		}
	}
	return false
}

// splitList 识别列表标记，返回渲染用标记与内容。无序统一成 "• "，有序保留原编号。
func splitList(s string) (marker, rest string, ok bool) {
	if len(s) >= 2 && (s[0] == '-' || s[0] == '*' || s[0] == '+') && (s[1] == ' ' || s[1] == '\t') {
		return "• ", strings.TrimSpace(s[2:]), true
	}
	n := 0
	for n < len(s) && s[n] >= '0' && s[n] <= '9' {
		n++
	}
	if n > 0 && n < len(s) && (s[n] == '.' || s[n] == ')') && n+1 < len(s) && s[n+1] == ' ' {
		return s[:n+1] + " ", strings.TrimSpace(s[n+2:]), true
	}
	return "", "", false
}

// wrapText 按显示宽度折行（ANSI 感知），短行不额外补空格。
func wrapText(s string, w int) string {
	if lipgloss.Width(s) <= w {
		return s
	}
	return softWrap(s, w)
}

// 折行 token。ANSI 转义序列单独成 token，不参与宽度计算也不被切断。
type wrapToken struct {
	raw   string
	width int
	kind  int // 0=word 1=space 2=ansi
}

func tokenizeANSI(s string) []wrapToken {
	var (
		out  []wrapToken
		word strings.Builder
	)
	flushWord := func() {
		if word.Len() == 0 {
			return
		}
		t := word.String()
		out = append(out, wrapToken{raw: t, width: lipgloss.Width(t), kind: 0})
		word.Reset()
	}
	r := []rune(s)
	for i := 0; i < len(r); i++ {
		c := r[i]
		if c == '\x1b' && i+1 < len(r) && r[i+1] == '[' {
			flushWord()
			j := i + 2
			for j < len(r) && !(r[j] >= 'a' && r[j] <= 'z') && !(r[j] >= 'A' && r[j] <= 'Z') {
				j++
			}
			if j < len(r) {
				j++
			}
			out = append(out, wrapToken{raw: string(r[i:j]), kind: 2})
			i = j - 1
			continue
		}
		if c == ' ' {
			flushWord()
			out = append(out, wrapToken{raw: " ", width: 1, kind: 1})
			continue
		}
		word.WriteRune(c)
	}
	flushWord()
	return out
}

func runeWidth(r rune) int { return lipgloss.Width(string(r)) }

// softWrap 按显示宽度折行：优先在空格断，CJK 等无空格长串逐 rune 断开。
// 宽度按终端单元格计算（全角字符占 2 列），不按 rune 个数。
func softWrap(s string, w int) string {
	var (
		b     strings.Builder
		lineW int
	)
	for _, tk := range tokenizeANSI(s) {
		switch tk.kind {
		case 2: // ANSI
			b.WriteString(tk.raw)
		case 1: // 空格
			if lineW == 0 {
				continue // 丢弃行首空格
			}
			if lineW+1 > w {
				b.WriteString("\n")
				lineW = 0
				continue
			}
			b.WriteString(" ")
			lineW++
		default: // 词
			if tk.width <= w {
				if lineW > 0 && lineW+tk.width > w {
					b.WriteString("\n")
					lineW = 0
				}
				b.WriteString(tk.raw)
				lineW += tk.width
				continue
			}
			// 超宽词（典型：整段无空格的中文）：逐字符填到行宽为止。
			for _, c := range tk.raw {
				rw := runeWidth(c)
				if lineW > 0 && lineW+rw > w {
					b.WriteString("\n")
					lineW = 0
				}
				b.WriteRune(c)
				lineW += rw
			}
		}
	}
	return b.String()
}

func leadingSpaces(s string) int {
	n := 0
	for n < len(s) && (s[n] == ' ' || s[n] == '\t') {
		n++
	}
	return n
}

func isWordByte(b byte) bool {
	return b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' || b >= '0' && b <= '9'
}

func nextWordByte(s string, i int) bool {
	return i >= 0 && i < len(s) && isWordByte(s[i])
}

// pad0 是分割线右端的留白，避免顶到 viewport 边。
const pad0 = 2
