package web

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// touch 造一个指定大小的文件并把 mtime 设成给定时间。
func touch(t *testing.T, dir, name string, size int, mod time.Time) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, make([]byte, size), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(p, mod, mod); err != nil {
		t.Fatal(err)
	}
	return p
}

// TestPruneUploadsDropsExpired 过期文件必须被清掉，新的必须留着。
// 这个目录只增不减，长期使用会攒到几个 G，装的全是可以重新上传一次的东西。
func TestPruneUploadsDropsExpired(t *testing.T) {
	dir := t.TempDir()
	old := touch(t, dir, "old.png", 16, time.Now().Add(-8*24*time.Hour))
	fresh := touch(t, dir, "fresh.png", 16, time.Now())

	removed := pruneUploads(dir)
	if removed != 1 {
		t.Fatalf("应删除 1 个过期文件，实际 %d", removed)
	}
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Fatal("过期文件应被删除")
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Fatalf("新文件不该被删: %v", err)
	}
}

// TestPruneUploadsTrimsBySize 总量超标时按 mtime 从旧到新删到阈值以下。
func TestPruneUploadsTrimsBySize(t *testing.T) {
	dir := t.TempDir()
	// 每份 100MB，共 6 份 = 600MB，超过 512MB 上限。
	const chunk = 100 << 20
	now := time.Now()
	var paths []string
	for i := 0; i < 6; i++ {
		// 逐个更旧：p0 最旧。
		paths = append(paths, touch(t, dir, "p"+string(rune('0'+i))+".png", chunk, now.Add(-time.Duration(6-i)*time.Minute)))
	}

	removed := pruneUploads(dir)
	if removed == 0 {
		t.Fatal("超过配额却一个都没删")
	}
	var total int64
	for _, p := range paths {
		if info, err := os.Stat(p); err == nil {
			total += info.Size()
		}
	}
	if total > uploadMaxSize {
		t.Fatalf("清理后仍超过配额: %d MB", total>>20)
	}
	// 最旧的必须先走：新上传的图更可能正被当前对话引用。
	if _, err := os.Stat(paths[0]); !os.IsNotExist(err) {
		t.Fatal("最旧的文件应先被删除")
	}
	// 最新的必须留下。
	if _, err := os.Stat(paths[5]); err != nil {
		t.Fatalf("最新的文件不该被删: %v", err)
	}
}

// TestPruneUploadsNoopWhenHealthy 目录不存在或没超额时不该出错。
func TestPruneUploadsNoopWhenHealthy(t *testing.T) {
	if n := pruneUploads(filepath.Join(t.TempDir(), "nope")); n != 0 {
		t.Fatalf("目录不存在应返回 0，实际 %d", n)
	}
	dir := t.TempDir()
	touch(t, dir, "a.png", 32, time.Now())
	if n := pruneUploads(dir); n != 0 {
		t.Fatalf("未超额不该删除，实际 %d", n)
	}
}
