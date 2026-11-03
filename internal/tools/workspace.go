package tools

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// Workspace 当前工作区。
//
// 存在的理由：DSH 的语义是「会话绑定一个工作区，所有相对路径以它为基准」。
// 如果只有 shell 的 cwd 跟着变、read_file/write_file 仍按进程 cwd 解析，
// 模型看到的就是两套坐标系——它会读到 A 目录的文件、写进 B 目录的文件，
// 而且看起来都「成功」了。
//
// 并发安全：Web UI 可以在会话运行中切换工作区，读写都要加锁。
type Workspace struct {
	mu   sync.RWMutex
	root string
}

// NewWorkspace 创建工作区。root 为空时用当前目录。
func NewWorkspace(root string) *Workspace {
	w := &Workspace{}
	if err := w.Set(root); err != nil {
		// 配置里的路径不存在不应让整个进程起不来：
		// 退回当前目录，问题留给 env_info 与启动诊断暴露。
		if cwd, e := os.Getwd(); e == nil {
			w.root = cwd
		}
	}
	return w
}

// Root 返回工作区绝对路径。
func (w *Workspace) Root() string {
	if w == nil {
		return ""
	}
	w.mu.RLock()
	defer w.mu.RUnlock()
	return w.root
}

// Set 切换工作区。必须是已存在的目录，避免打错字后所有操作落到空目录。
func (w *Workspace) Set(root string) error {
	if strings.TrimSpace(root) == "" {
		root = "."
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return err
	}
	info, err := os.Stat(abs)
	if err != nil {
		return fmt.Errorf("工作区不存在：%w", err)
	}
	if !info.IsDir() {
		return fmt.Errorf("工作区不是目录：%s", abs)
	}
	w.mu.Lock()
	w.root = abs
	w.mu.Unlock()
	return nil
}

// Resolve 把工具参数里的路径解析成绝对路径。
// 绝对路径原样返回（工具允许访问工作区之外，这是显式行为，不是默认）。
func (w *Workspace) Resolve(p string) string {
	if p == "" {
		p = "."
	}
	if filepath.IsAbs(p) {
		return filepath.Clean(p)
	}
	root := w.Root()
	if root == "" {
		return filepath.Clean(p)
	}
	return filepath.Clean(filepath.Join(root, p))
}
