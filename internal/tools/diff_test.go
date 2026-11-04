package tools

import (
	"strings"
	"testing"
)

func TestFileDiffAddRemoveChange(t *testing.T) {
	old := "line1\nline2\nline3\n"
	new := "line1\nline2 changed\nline4\n"
	d := fileDiff(old, new, "f.txt")
	if !strings.Contains(d, "+line2 changed") {
		t.Errorf("expected added changed line in diff, got:\n%s", d)
	}
	if !strings.Contains(d, "-line2") {
		t.Errorf("expected removed line in diff, got:\n%s", d)
	}
	if !strings.Contains(d, "+line4") {
		t.Errorf("expected added line4 in diff, got:\n%s", d)
	}
	if !strings.Contains(d, "@@") {
		t.Errorf("expected unified @@ header in diff, got:\n%s", d)
	}
}

func TestFileDiffNewFileAllAdditions(t *testing.T) {
	d := fileDiff("", "a\nb\n", "new.txt")
	for _, ln := range strings.Split(d, "\n") {
		// 删除操作行是 "- " 开头；文件头是 "--- " 开头，不算。
		if strings.HasPrefix(ln, "- ") || ln == "-" {
			t.Errorf("new file should be all additions, found deletion op: %q\ndiff:\n%s", ln, d)
		}
	}
	if !strings.Contains(d, "+a") || !strings.Contains(d, "+b") {
		t.Errorf("new file diff missing additions, got:\n%s", d)
	}
}

func TestExtractDiffRoundtrip(t *testing.T) {
	summary := "wrote x (5 bytes)"
	diff := "--- a\n+++ b\n@@ -1 +1 @@\n-lineA\n+lineB\n"
	embedded := embedDiff(summary, diff)
	rest, got := ExtractDiff(embedded)
	if got != strings.Trim(diff, "\n") {
		t.Errorf("extracted diff mismatch:\n got=%q\nwant=%q", got, strings.Trim(diff, "\n"))
	}
	if rest != summary {
		t.Errorf("extracted rest mismatch: got=%q want=%q", rest, summary)
	}
}

func TestExtractDiffNone(t *testing.T) {
	rest, diff := ExtractDiff("plain result text")
	if diff != "" {
		t.Errorf("expected no diff, got %q", diff)
	}
	if rest != "plain result text" {
		t.Errorf("rest mismatch: %q", rest)
	}
}
