package tools

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestWorkspaceResolve 相对路径以 root 为基准；root 内的绝对路径放行。
func TestWorkspaceResolve(t *testing.T) {
	dir := t.TempDir()
	ws := &Workspace{restrict: true}
	if err := ws.Set(dir); err != nil {
		t.Fatal(err)
	}
	if got, err := ws.Resolve("a/b.txt"); err != nil || got != filepath.Join(dir, "a", "b.txt") {
		t.Fatalf("Resolve relative → %q, %v", got, err)
	}
	abs := filepath.Join(dir, "x.txt")
	if got, err := ws.Resolve(abs); err != nil || got != abs {
		t.Fatalf("Resolve absolute in root → %q, %v", got, err)
	}
	if got, err := ws.Resolve(""); err != nil || got != filepath.Clean(dir) {
		t.Fatalf("Resolve empty → %q, %v", got, err)
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

// TestWorkspaceRejectsEscape 白名单必须拦住绝对路径出界、`..` 逃逸，
// 且不能把兄弟目录（共同前缀）误判为包含。
func TestWorkspaceRejectsEscape(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "app")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	sibling := filepath.Join(base, "application")
	if err := os.MkdirAll(sibling, 0o755); err != nil {
		t.Fatal(err)
	}
	ws := &Workspace{restrict: true}
	if err := ws.Set(root); err != nil {
		t.Fatal(err)
	}

	cases := []string{
		filepath.Join(sibling, "secret.txt"),
		"../application/secret.txt",
		"../outside.txt",
		filepath.Join(base, "other.txt"),
	}
	for _, p := range cases {
		if got, err := ws.Resolve(p); err == nil {
			t.Errorf("应拒绝越界路径 %q，却返回 %q", p, got)
		}
	}
	for _, p := range []string{".", "sub", "sub/new/deep.txt"} {
		if _, err := ws.Resolve(p); err != nil {
			t.Errorf("工作区内路径 %q 被误拒：%v", p, err)
		}
	}
}

// TestWorkspaceSymlinkEscape 符号链接指向工作区外时，即使链接文件本身在
// 工作区内也要拒绝——这是路径白名单最容易漏的一层。
func TestWorkspaceSymlinkEscape(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "app")
	outside := filepath.Join(base, "outside")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "link")
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("本机无法创建符号链接（Windows 常需开发者模式/管理员）：%v", err)
	}
	ws := &Workspace{restrict: true}
	if err := ws.Set(root); err != nil {
		t.Fatal(err)
	}
	if got, err := ws.Resolve("link/secret.txt"); err == nil {
		t.Fatalf("符号链接逃逸未被拦住：%q", got)
	}
	if _, err := ws.Resolve("normal.txt"); err != nil {
		t.Fatalf("普通路径被误拒：%v", err)
	}
}

// TestWorkspaceRestrictOff 显式关闭白名单后退回旧行为（绝对路径出界放行）。
func TestWorkspaceRestrictOff(t *testing.T) {
	dir := t.TempDir()
	ws := NewWorkspace(dir)
	if !ws.Restricted() {
		t.Fatal("默认应开启白名单")
	}
	outside := filepath.Join(t.TempDir(), "x.txt")
	if _, err := ws.Resolve(outside); err == nil {
		t.Fatal("开启时应拒绝出界路径")
	}
	ws.SetRestricted(false)
	if got, err := ws.Resolve(outside); err != nil || got != outside {
		t.Fatalf("关闭后应放行：%q, %v", got, err)
	}
	ws.SetRestricted(true)
	if _, err := ws.Resolve(outside); err == nil {
		t.Fatal("重新开启后应再次拒绝")
	}
}

// TestToolsRejectEscape 端到端：各文件工具都必须把出界路径挡在 Run 里，
// 而不是只靠 Workspace.Resolve 单测「看起来安全」。
func TestToolsRejectEscape(t *testing.T) {
	dir := t.TempDir()
	ws := &Workspace{restrict: true}
	if err := ws.Set(dir); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "secret.txt")
	if err := os.WriteFile(outside, []byte("SECRET"), 0o644); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	type call struct {
		name string
		run  func() (string, error)
	}
	calls := []call{
		{"read_file", func() (string, error) {
			return (&ReadFileTool{ws: ws}).Run(ctx, argsJSON(t, map[string]any{"path": outside}))
		}},
		{"write_file", func() (string, error) {
			return (&WriteFileTool{ws: ws}).Run(ctx, argsJSON(t, map[string]any{"path": outside, "content": "overwrite"}))
		}},
		{"list_dir", func() (string, error) {
			return (&ListDirTool{ws: ws}).Run(ctx, argsJSON(t, map[string]any{"path": filepath.Dir(outside)}))
		}},
		{"glob", func() (string, error) {
			return (&GlobTool{ws: ws}).Run(ctx, argsJSON(t, map[string]any{"pattern": filepath.Join(filepath.Dir(outside), "*.txt")}))
		}},
		{"delete_path", func() (string, error) {
			return (&DeletePathTool{ws: ws}).Run(ctx, argsJSON(t, map[string]any{"path": outside}))
		}},
	}
	for _, c := range calls {
		if out, err := c.run(); err == nil {
			t.Errorf("%s 未拦住出界路径，返回 %q", c.name, out)
		} else if !strings.Contains(err.Error(), "拒绝访问") {
			t.Errorf("%s 报错不是白名单拒绝：%v", c.name, err)
		}
	}
	if data, err := os.ReadFile(outside); err != nil || string(data) != "SECRET" {
		t.Fatalf("工作区外文件被改动：%q, %v", data, err)
	}
}

// TestToolsFollowWorkspace 关键回归：切换工作区后，文件工具必须写进新工作区，
// 而不是继续按进程 cwd 落盘。这是 shell 与文件工具坐标系不一致的根源。
func TestToolsFollowWorkspace(t *testing.T) {
	a, b := t.TempDir(), t.TempDir()
	ws := &Workspace{restrict: true}
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
	ws := &Workspace{restrict: true}
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
