package session

import (
	"strings"
	"testing"
	"unicode/utf8"

	"gsh/internal/api"
)

// TestSummarizeStripsAttachments 标题摘要不能把附件原文算进去。
func TestSummarizeStripsAttachments(t *testing.T) {
	got := summarize("帮我看下\n--- 附件 note.txt（36B）---\nline one\nline two\n--- 附件结束 ---")
	if got != "帮我看下" {
		t.Fatalf("got %q", got)
	}
	if got := summarize("\n--- 附件 a.txt（1B）---\nx\n--- 附件结束 ---"); got != "（附件）" {
		t.Fatalf("only-attach got %q", got)
	}
}

// TestSearchFindsAcrossSessions 检索要能跨会话命中正文，并给出上下文片段。
func TestSearchFindsAcrossSessions(t *testing.T) {
	dir := t.TempDir()
	st := &store{dir: dir}

	s1, err := st.New()
	if err != nil {
		t.Fatal(err)
	}
	_ = s1.Append(api.Message{Role: api.RoleUser, Content: "帮我查一下 golang 的 context 用法"})
	_ = s1.Append(api.Message{Role: api.RoleAssistant, Content: "context 用于传递取消信号"})

	s2, err := st.New()
	if err != nil {
		t.Fatal(err)
	}
	_ = s2.Append(api.Message{Role: api.RoleUser, Content: "无关内容"})

	hits, err := st.Search("CONTEXT", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 {
		t.Fatalf("应命中 1 个会话，实际 %d", len(hits))
	}
	h := hits[0]
	if h.ID != s1.ID() {
		t.Fatalf("命中会话错误: %s", h.ID)
	}
	if h.Count != 2 {
		t.Fatalf("命中次数应为 2，实际 %d", h.Count)
	}
	if !strings.Contains(h.Snippet, "context") && !strings.Contains(h.Snippet, "CONTEXT") {
		t.Fatalf("片段应包含关键词: %q", h.Snippet)
	}
}

// TestSearchEmptyQuery 空检索词应报错而不是返回全部会话。
func TestSearchEmptyQuery(t *testing.T) {
	st := &store{dir: t.TempDir()}
	if _, err := st.Search("  ", 10); err == nil {
		t.Fatal("空检索词应返回错误")
	}
}

// TestSnippetOfHandlesCJK 中文里按 rune 切片段，不能切出半个字符。
func TestSnippetOfHandlesCJK(t *testing.T) {
	text := strings.Repeat("前", 100) + "关键词" + strings.Repeat("后", 100)
	sn, n := snippetOf(text, "关键词")
	if n != 1 {
		t.Fatalf("命中次数错误: %d", n)
	}
	if !utf8.ValidString(sn) {
		t.Fatal("片段产生非法 UTF-8")
	}
	if !strings.Contains(sn, "关键词") {
		t.Fatalf("片段应包含关键词: %q", sn)
	}
}
