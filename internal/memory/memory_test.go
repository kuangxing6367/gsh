package memory

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestAppendAndText 追加的记录要能读回，并带日期前缀。
func TestAppendAndText(t *testing.T) {
	p := filepath.Join(t.TempDir(), "memory", "MEMORY.md")
	s, err := newStore(p)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Append("用户偏好 tab 缩进"); err != nil {
		t.Fatal(err)
	}
	body := s.Text()
	if !strings.Contains(body, "用户偏好 tab 缩进") {
		t.Fatalf("记忆内容未读回: %q", body)
	}
	if !strings.Contains(body, "- ") {
		t.Fatalf("应带列表前缀: %q", body)
	}
	// 文件权限必须是 0600：记忆里可能有敏感信息。
	// Windows 不保留 POSIX 权限位，这里只校验类 Unix 平台。
	if runtime.GOOS != "windows" {
		fi, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		if perm := fi.Mode().Perm(); perm != 0o600 {
			t.Fatalf("记忆文件权限应为 0600，实际 %o", perm)
		}
	}
}

// TestTextTruncatesFromTail 超长时保留最近的部分（新记忆更有价值）。
func TestTextTruncatesFromTail(t *testing.T) {
	s := &store{body: strings.Repeat("旧", injectLimit) + "最新的约定"}
	got := s.Text()
	if !strings.Contains(got, "最新的约定") {
		t.Fatal("截断应保留末尾的最新内容")
	}
	if !strings.Contains(got, "较早的记忆已省略") {
		t.Fatal("截断时应标明省略")
	}
}

// TestAppendRejectsEmpty 空记录不应写入文件。
func TestAppendRejectsEmpty(t *testing.T) {
	s, err := newStore(filepath.Join(t.TempDir(), "MEMORY.md"))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Append("   "); err == nil {
		t.Fatal("空记录应报错")
	}
}

// TestRememberTool 工具写入后，服务侧能读到。
func TestRememberTool(t *testing.T) {
	s, err := newStore(filepath.Join(t.TempDir(), "MEMORY.md"))
	if err != nil {
		t.Fatal(err)
	}
	tool := &RememberTool{store: s}
	out, err := tool.Run(context.Background(), `{"note":"项目用 Go 1.21"}`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "已记入") {
		t.Fatalf("返回值异常: %q", out)
	}
	if !strings.Contains(s.Text(), "项目用 Go 1.21") {
		t.Fatal("记忆未写入")
	}
}

// TestRememberToolBadArgs 参数缺失时报错，不静默写入空记录。
func TestRememberToolBadArgs(t *testing.T) {
	s, _ := newStore(filepath.Join(t.TempDir(), "MEMORY.md"))
	tool := &RememberTool{store: s}
	if _, err := tool.Run(context.Background(), `{}`); err == nil {
		t.Fatal("缺 note 应报错")
	}
}
