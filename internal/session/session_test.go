package session

import "testing"

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
