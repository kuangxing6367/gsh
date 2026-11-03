package tools

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestWorkspaceResolve 相对路径必须落在工作区内，绝对路径原样保留。
func TestWorkspaceResolve(t *testing.T) {
	dir := t.TempDir()
	ws := &Workspace{}
	if err := ws.Set(dir); err != nil {
		t.Fatal(err)
	}
	if got, want := ws.Resolve("a/b.txt"), filepath.Join(dir, "a", "b.txt"); got != want {
		t.Fatalf("Resolve relative → %q, want %q", got, want)
	}
	abs := filepath.Join(dir, "x.txt")
	if got := ws.Resolve(abs); got != abs {
		t.Fatalf("Resolve absolute → %q, want %q", got, abs)
	}
	if got, want := ws.Resolve(""), filepath.Clean(dir); got != want {
		t.Fatalf("Resolve empty → %q, want %q", got, want)
	}
}

// TestWorkspaceSetRejectsBadPath 切换工作区必须校验目录存在，否则打错字后
// 所有写操作会静默失败到不存在的路径。
func TestWorkspaceSetRejectsBadPath(t *testing.T) {
	ws := &Workspace{}
	if err := ws.Set(filepath.Join(t.TempDir(), "nope")); err == nil {
		t.Fatal("missing dir must error")
	}
	f := filepath.Join(t.TempDir(), "f.txt")
	if err := os.WriteFile(f, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := ws.Set(f); err == nil {
		t.Fatal("file as workspace must error")
	}
}

// TestToolsFollowWorkspace 关键回归：切换工作区后，文件工具必须写进新工作区，
// 而不是继续按进程 cwd 落盘。这是 shell 与文件工具坐标系不一致的根源。
func TestToolsFollowWorkspace(t *testing.T) {
	a, b := t.TempDir(), t.TempDir()
	ws := &Workspace{}
	if err := ws.Set(a); err != nil {
		t.Fatal(err)
	}

	write := &WriteFileTool{ws: ws}
	if _, err := write.Run(context.Background(), argsJSON(t, map[string]any{
		"path": "note.txt", "content": "in-a",
	})); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(a, "note.txt")); err != nil {
		t.Fatalf("file should land in workspace A: %v", err)
	}

	if err := ws.Set(b); err != nil {
		t.Fatal(err)
	}
	write = &WriteFileTool{ws: ws}
	if _, err := write.Run(context.Background(), argsJSON(t, map[string]any{
		"path": "note.txt", "content": "in-b",
	})); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(b, "note.txt"))
	if err != nil {
		t.Fatalf("file should land in workspace B: %v", err)
	}
	if string(data) != "in-b" {
		t.Fatalf("B content = %q", data)
	}

	// 旧工作区里的文件不能被新工作区覆盖
	old, err := os.ReadFile(filepath.Join(a, "note.txt"))
	if err != nil || string(old) != "in-a" {
		t.Fatalf("A must stay untouched, got %q err %v", old, err)
	}
}

// TestListDirFollowsWorkspace list_dir 默认路径同样要跟随工作区。
func TestListDirFollowsWorkspace(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	ws := &Workspace{}
	if err := ws.Set(dir); err != nil {
		t.Fatal(err)
	}
	out, err := (&ListDirTool{ws: ws}).Run(context.Background(), `{}`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "sub") {
		t.Fatalf("list_dir should show workspace children, got %q", out)
	}
}
