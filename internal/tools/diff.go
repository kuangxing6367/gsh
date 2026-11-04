package tools

import (
	"fmt"
	"strings"
)

// diffMarker 工具结果里嵌入 diff 的起止标记。
// 用成对的 XML 风格标记而非 JSON，是为了在 Web 转录、审计原文、终端里都可读；
// TUI 解析出块后做彩色渲染，Web 侧不解析也能原样展示。
const (
	diffStart = "<gsh:diff>"
	diffEnd   = "</gsh:diff>"
)

// fileDiff 生成 a→b 的 unified diff 文本（不含文件头以外的噪音）。
// 用于 write/edit 工具把"改了什么"直接带进工具结果，让展示层（TUI）能彩色渲染，
// 而不是只回一句 "wrote x (n bytes)"。
//
// 实现的是经典 LCS 行级 diff + unified 格式（@@ 块头、上下文 3 行），
// 不引入第三方库——diff 只是给人类看的辅助信息，正确性阈值低，LCS 足够。
func fileDiff(oldText, newText string, label string) string {
	a := splitLines(oldText)
	b := splitLines(newText)
	lcs := lcsTable(a, b)
	ops := backtrack(a, b, lcs)

	var bld strings.Builder
	fmt.Fprintf(&bld, "--- %s\n+++ %s\n", label, label)
	ctx := 3
	var blocks []diffBlock
	cur := diffBlock{}

	flush := func() {
		if len(cur.lines) > 0 {
			blocks = append(blocks, cur)
			cur = diffBlock{}
		}
	}

	for _, op := range ops {
		switch op.kind {
		case "eq":
			cur.lines = append(cur.lines, diffLine{op: " ", text: op.text})
			if len(cur.lines) > 2*ctx {
				// 超长相等段：只保留尾部 ctx 行，前面用省略。
				overflow := len(cur.lines) - ctx
				cur.lines = cur.lines[overflow:]
				cur.skipped += overflow
			}
		case "del":
			flush()
			cur.lines = append(cur.lines, diffLine{op: "-", text: op.text})
		case "add":
			flush()
			cur.lines = append(cur.lines, diffLine{op: "+", text: op.text})
		}
	}
	flush()

	oldNo, newNo := 1, 1
	for _, blk := range blocks {
		if blk.skipped > 0 {
			fmt.Fprintf(&bld, "@@ -%d,%d +%d,%d @@\n", oldNo, 0, newNo, 0)
			bld.WriteString("... " + fmt.Sprint(blk.skipped) + " identical lines omitted ...\n")
			// skipped 段被省略，行号推进由后续相等行负责；这里仅标注。
		}
		blkOldStart, blkNewStart := oldNo, newNo
		var oldCnt, newCnt int
		for _, ln := range blk.lines {
			switch ln.op {
			case " ":
				oldNo++; newNo++; oldCnt++; newCnt++
			case "-":
				oldNo++; oldCnt++
			case "+":
				newNo++; newCnt++
			}
		}
		fmt.Fprintf(&bld, "@@ -%d,%d +%d,%d @@\n", blkOldStart, oldCnt, blkNewStart, newCnt)
		for _, ln := range blk.lines {
			bld.WriteString(ln.op + ln.text + "\n")
		}
	}
	return bld.String()
}

type diffLine struct {
	op   string
	text string
}

type diffBlock struct {
	lines   []diffLine
	skipped int
}

func splitLines(s string) []string {
	if s == "" {
		return nil
	}
	s = strings.ReplaceAll(s, "\r\n", "\n")
	if strings.HasSuffix(s, "\n") {
		s = s[:len(s)-1]
	}
	if s == "" {
		return nil
	}
	return strings.Split(s, "\n")
}

// lcsTable 返回 a/b 的最长公共子序列长度表（行级）。
func lcsTable(a, b []string) [][]int {
	n, m := len(a), len(b)
	t := make([][]int, n+1)
	for i := range t {
		t[i] = make([]int, m+1)
	}
	for i := n - 1; i >= 0; i-- {
		for j := m - 1; j >= 0; j-- {
			if a[i] == b[j] {
				t[i][j] = t[i+1][j+1] + 1
			} else if t[i+1][j] >= t[i][j+1] {
				t[i][j] = t[i+1][j]
			} else {
				t[i][j] = t[i][j+1]
			}
		}
	}
	return t
}

type diffOp struct {
	kind string // eq | del | add
	text string
}

// backtrack 从 LCS 表回溯出编辑序列。
func backtrack(a, b []string, t [][]int) []diffOp {
	var ops []diffOp
	i, j := 0, 0
	for i < len(a) && j < len(b) {
		if a[i] == b[j] {
			ops = append(ops, diffOp{kind: "eq", text: a[i]})
			i++; j++
		} else if t[i+1][j] >= t[i][j+1] {
			ops = append(ops, diffOp{kind: "del", text: a[i]})
			i++
		} else {
			ops = append(ops, diffOp{kind: "add", text: b[j]})
			j++
		}
	}
	for ; i < len(a); i++ {
		ops = append(ops, diffOp{kind: "del", text: a[i]})
	}
	for ; j < len(b); j++ {
		ops = append(ops, diffOp{kind: "add", text: b[j]})
	}
	return ops
}

// embedDiff 把 diff 文本夹进工具结果（标记对之间），便于展示层解析。
func embedDiff(summary, diff string) string {
	if strings.TrimSpace(diff) == "" {
		return summary
	}
	return summary + "\n\n" + diffStart + "\n" + diff + diffEnd + "\n"
}

// ExtractDiff 从工具结果里取出 <gsh:diff> 块；返回 (剩余文本, diff 文本)。
// 没有 diff 块时第二个返回值为空。导出给展示层（TUI）用。
func ExtractDiff(result string) (rest, diff string) {
	si := strings.Index(result, diffStart)
	if si < 0 {
		return result, ""
	}
	ei := strings.Index(result[si:], diffEnd)
	if ei < 0 {
		return result, ""
	}
	ei += si
	before := result[:si]
	diffBody := result[si+len(diffStart) : ei]
	after := result[ei+len(diffEnd):]
	rest = strings.TrimRight(before, "\n") + strings.TrimLeft(after, "\n")
	return rest, strings.Trim(diffBody, "\n")
}
