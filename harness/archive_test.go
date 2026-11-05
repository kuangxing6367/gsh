package harness

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// TestArchiveIndexRoundTrip 已归档索引要能读回写入的内容。
//
// 这个索引是「不重复归档」的唯一依据：丢了它，同一个会话每跑一次
// -archive 就会被再追写一遍，月度归档文件变成重复内容的堆。
func TestArchiveIndexRoundTrip(t *testing.T) {
	dir := t.TempDir()

	if got := readArchivedIndex(dir); len(got) != 0 {
		t.Fatalf("目录里没有索引文件时应返回空表，实际 %v", got)
	}
	if err := appendArchivedIndex(dir, []string{"s1", "s2"}); err != nil {
		t.Fatal(err)
	}
	if err := appendArchivedIndex(dir, []string{"s3"}); err != nil {
		t.Fatal(err)
	}

	got := readArchivedIndex(dir)
	for _, id := range []string{"s1", "s2", "s3"} {
		if !got[id] {
			t.Fatalf("%s 应已被登记，实际 %v", id, got)
		}
	}
	if len(got) != 3 {
		t.Fatalf("应恰好 3 条，实际 %d", len(got))
	}

	// 索引文件必须是 0600：里面是会话 id，不该让同机其他用户读到。
	// Windows 的 os.Stat 不反映真实 ACL，这一条只在类 Unix 上有意义。
	if runtime.GOOS != "windows" {
		st, err := os.Stat(filepath.Join(dir, archivedIndexFile))
		if err != nil {
			t.Fatal(err)
		}
		if st.Mode().Perm()&0o077 != 0 {
			t.Fatalf("权限过宽: %v", st.Mode().Perm())
		}
	}
}

// TestArchiveIndexToleratesGarbage 索引被写坏时不该让归档整个失败，
// 最坏只是重复归档一次。
func TestArchiveIndexToleratesGarbage(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, archivedIndexFile), []byte("\n\n  \nbroken"), 0o600); err != nil {
		t.Fatal(err)
	}
	got := readArchivedIndex(dir)
	if !got["broken"] {
		t.Fatalf("有效行应被读入，实际 %v", got)
	}
}
