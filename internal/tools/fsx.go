package tools

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"gsh/internal/api"
)

// ---------- list_dir ----------

// ListDirTool 列目录内容。
type ListDirTool struct{}

// Spec 工具描述。
func (t *ListDirTool) Spec() api.ToolSpec {
	return api.ToolSpec{
		Name:        "list_dir",
		Description: "List directory entries with type/size/mtime. Prefer this over `ls` for structured output. Use glob for pattern matching across subdirs.",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"path":      map[string]any{"type": "string", "description": "Directory path, default ."},
				"recursive": map[string]any{"type": "boolean", "description": "Walk subdirectories"},
				"depth":     map[string]any{"type": "integer", "description": "Max path levels below path when recursive (1 = direct children), default 2"},
			},
		},
	}
}

// Run 执行列目录。
func (t *ListDirTool) Run(_ context.Context, argsJSON string) (string, error) {
	m, err := argObj(argsJSON)
	if err != nil {
		return "", err
	}
	root := argStr(m, "path")
	if root == "" {
		root = "."
	}
	recursive, _ := m["recursive"].(bool)
	depth := 2
	if v, ok := m["depth"].(float64); ok && v > 0 {
		depth = int(v)
	}
	info, err := os.Stat(root)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return fmt.Sprintf("%s\t%s\t%d bytes\n", root, "file", info.Size()), nil
	}

	var b strings.Builder
	base := filepath.Clean(root)
	count := 0
	const maxEntries = 2000

	var walk func(dir string, level int) error
	walk = func(dir string, level int) error {
		entries, err := os.ReadDir(dir)
		if err != nil {
			return err
		}
		sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
		for _, e := range entries {
			if count >= maxEntries {
				return nil
			}
			full := filepath.Join(dir, e.Name())
			rel, _ := filepath.Rel(base, full)
			kind := "file"
			var size int64
			var mtime time.Time
			if fi, err := e.Info(); err == nil {
				mtime = fi.ModTime()
				size = fi.Size()
				if fi.IsDir() {
					kind = "dir"
					size = 0
				} else if fi.Mode()&os.ModeSymlink != 0 {
					kind = "link"
				}
			}
			fmt.Fprintf(&b, "%s\t%s\t%d\t%s\n", filepath.ToSlash(rel), kind, size, mtime.Format("2006-01-02 15:04"))
			count++
			if e.IsDir() && recursive && level < depth {
				if err := walk(full, level+1); err != nil {
					return err
				}
			}
		}
		return nil
	}
	if err := walk(root, 1); err != nil {
		return b.String(), err
	}
	if count >= maxEntries {
		b.WriteString(fmt.Sprintf("...[truncated at %d entries]\n", maxEntries))
	}
	return b.String(), nil
}

// ---------- make_dir ----------

// MakeDirTool 创建目录（含父目录）。
type MakeDirTool struct{}

// Spec 工具描述。
func (t *MakeDirTool) Spec() api.ToolSpec {
	return api.ToolSpec{
		Name:        "make_dir",
		Description: "Create a directory and any missing parents (like mkdir -p).",
		Parameters: map[string]any{
			"type":       "object",
			"properties": map[string]any{"path": map[string]any{"type": "string"}},
			"required":   []string{"path"},
		},
	}
}

// Run 创建目录。
func (t *MakeDirTool) Run(_ context.Context, argsJSON string) (string, error) {
	m, err := argObj(argsJSON)
	if err != nil {
		return "", err
	}
	path := argStr(m, "path")
	if path == "" {
		return "", fmt.Errorf("path is required")
	}
	if err := os.MkdirAll(path, 0o755); err != nil {
		return "", err
	}
	return "created: " + path, nil
}

// ---------- move_path ----------

// MovePathTool 移动/重命名（同盘 rename）。
type MovePathTool struct{}

// Spec 工具描述。
func (t *MovePathTool) Spec() api.ToolSpec {
	return api.ToolSpec{
		Name:        "move_path",
		Description: "Move or rename a file/directory. Creates destination parent dirs. Cross-device moves may fail; then use copy_path + delete_path.",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"from": map[string]any{"type": "string"},
				"to":   map[string]any{"type": "string"},
			},
			"required": []string{"from", "to"},
		},
	}
}

// Run 执行移动。
func (t *MovePathTool) Run(_ context.Context, argsJSON string) (string, error) {
	m, err := argObj(argsJSON)
	if err != nil {
		return "", err
	}
	from, to := argStr(m, "from"), argStr(m, "to")
	if from == "" || to == "" {
		return "", fmt.Errorf("from and to are required")
	}
	if dir := filepath.Dir(to); dir != "." && dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return "", err
		}
	}
	if err := os.Rename(from, to); err != nil {
		return "", fmt.Errorf("%w（跨盘或目标已存在时请改用 copy_path + delete_path）", err)
	}
	return fmt.Sprintf("moved: %s -> %s", from, to), nil
}

// ---------- copy_path ----------

// CopyPathTool 复制文件或目录。
type CopyPathTool struct{}

// Spec 工具描述。
func (t *CopyPathTool) Spec() api.ToolSpec {
	return api.ToolSpec{
		Name:        "copy_path",
		Description: "Copy a file or directory tree. Creates destination parent dirs.",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"from":      map[string]any{"type": "string"},
				"to":        map[string]any{"type": "string"},
				"overwrite": map[string]any{"type": "boolean", "description": "Overwrite existing files, default true"},
			},
			"required": []string{"from", "to"},
		},
	}
}

// Run 执行复制。
func (t *CopyPathTool) Run(_ context.Context, argsJSON string) (string, error) {
	m, err := argObj(argsJSON)
	if err != nil {
		return "", err
	}
	from, to := argStr(m, "from"), argStr(m, "to")
	if from == "" || to == "" {
		return "", fmt.Errorf("from and to are required")
	}
	overwrite := true
	if v, ok := m["overwrite"].(bool); ok {
		overwrite = v
	}
	info, err := os.Stat(from)
	if err != nil {
		return "", err
	}
	if err := copyAny(from, to, info, overwrite); err != nil {
		return "", err
	}
	return fmt.Sprintf("copied: %s -> %s", from, to), nil
}

func copyAny(from, to string, info os.FileInfo, overwrite bool) error {
	if info.IsDir() {
		if err := os.MkdirAll(to, info.Mode().Perm()|0o100); err != nil {
			return err
		}
		entries, err := os.ReadDir(from)
		if err != nil {
			return err
		}
		for _, e := range entries {
			childInfo, err := e.Info()
			if err != nil {
				return err
			}
			if err := copyAny(filepath.Join(from, e.Name()), filepath.Join(to, e.Name()), childInfo, overwrite); err != nil {
				return err
			}
		}
		return nil
	}
	if !overwrite {
		if _, err := os.Stat(to); err == nil {
			return fmt.Errorf("目标已存在：%s（overwrite=false）", to)
		}
	}
	if dir := filepath.Dir(to); dir != "." && dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	src, err := os.Open(from)
	if err != nil {
		return err
	}
	defer src.Close()
	dst, err := os.OpenFile(to, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, info.Mode().Perm())
	if err != nil {
		return err
	}
	if _, err := io.Copy(dst, src); err != nil {
		dst.Close()
		return err
	}
	return dst.Close()
}

// ---------- delete_path ----------

// DeletePathTool 删除文件/目录（受安全策略约束）。
type DeletePathTool struct {
	// allowRecursive 允许删除非空目录（由 allow_destructive 决定）。
	allowRecursive bool
}

// Spec 工具描述。
func (t *DeletePathTool) Spec() api.ToolSpec {
	return api.ToolSpec{
		Name:        "delete_path",
		Description: "Delete a file, or an empty directory. Deleting a non-empty directory requires recursive=true AND allow_destructive=true in config. Root/home/top-level paths are always refused.",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"path":      map[string]any{"type": "string"},
				"recursive": map[string]any{"type": "boolean", "description": "Required to remove a non-empty directory"},
			},
			"required": []string{"path"},
		},
	}
}

// Run 执行删除。
func (t *DeletePathTool) Run(_ context.Context, argsJSON string) (string, error) {
	m, err := argObj(argsJSON)
	if err != nil {
		return "", err
	}
	path := argStr(m, "path")
	if path == "" {
		return "", fmt.Errorf("path is required")
	}
	if reason := unsafeDeleteTarget(path); reason != "" {
		return "", fmt.Errorf("拒绝删除：%s", reason)
	}
	info, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		if err := os.Remove(path); err != nil {
			return "", err
		}
		return "deleted file: " + path, nil
	}
	recursive, _ := m["recursive"].(bool)
	entries, err := os.ReadDir(path)
	if err != nil {
		return "", err
	}
	if len(entries) > 0 {
		if !recursive {
			return "", fmt.Errorf("目录非空（%d 项）：如需递归删除，传 recursive=true 且配置 allow_destructive=true", len(entries))
		}
		if !t.allowRecursive {
			return "", fmt.Errorf("递归删除目录被拦截（受限）：需在 config.json 的 plugins_cfg.tools 设 allow_destructive: true")
		}
		if err := os.RemoveAll(path); err != nil {
			return "", err
		}
		return "deleted dir (recursive): " + path, nil
	}
	if err := os.Remove(path); err != nil {
		return "", err
	}
	return "deleted empty dir: " + path, nil
}

// unsafeDeleteTarget 拒绝对根/家目录/盘符根这类目标动手。
// 与 safety.go 的命令扫描互补：那条管 shell 字符串，这条管工具参数。
func unsafeDeleteTarget(path string) string {
	clean := filepath.Clean(path)
	if clean == "." || clean == ".." {
		return "目标是当前/上级目录"
	}
	if filepath.IsAbs(clean) {
		// 盘符根（C:\）或文件系统根（/）
		if clean == string(filepath.Separator) || filepath.Dir(clean) == clean {
			return "目标是文件系统根目录"
		}
		if vol := filepath.VolumeName(clean); vol != "" && strings.TrimSuffix(clean, string(filepath.Separator)) == vol {
			return "目标是磁盘根目录"
		}
		// 顶层目录本体（/tmp、/etc、C:\Windows）不允许整个删
		parts := strings.FieldsFunc(clean, func(r rune) bool { return r == '/' || r == '\\' })
		if len(parts) <= 2 && !strings.Contains(clean, "/.") {
			rest := clean
			if v := filepath.VolumeName(clean); v != "" {
				rest = strings.TrimPrefix(clean, v)
			}
			rest = strings.Trim(rest, `/\`)
			if !strings.ContainsAny(rest, `/\`) {
				return fmt.Sprintf("目标是顶层路径 %q（疑似系统目录）", clean)
			}
		}
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		if clean == filepath.Clean(home) {
			return "目标是用户家目录"
		}
	}
	return ""
}
