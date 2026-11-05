package tools

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
)

// Workspace 当前工作区。
//
// 会话绑定一个工作区，所有相对路径以它为基准，越界访问一律拒绝。
// 如果只有 shell 的 cwd 跟着变、read_file/write_file 仍按进程 cwd 解析，
// 模型看到的就是两套坐标系——它会读到 A 目录的文件、写进 B 目录的文件，
// 而且看起来都「成功」了。
//
// 白名单语义（restrict）：所有文件类工具的路径都必须在 root 内。不仅拦
// `..`，也拦绝对路径越界和符号链接越界。关闭时退回旧行为（允许绝对路径出界）。
// 注意：这只覆盖文件工具；bash 命令本身能 cd 出工作区，不属于这层边界。
//
// 并发安全：Web UI 可以在会话运行中切换工作区，读写都要加锁。
type Workspace struct {
	mu       sync.RWMutex
	root     string
	restrict bool
}

// NewWorkspace 创建工作区。root 为空时用当前目录。默认开启白名单。
func NewWorkspace(root string) *Workspace {
	w := &Workspace{restrict: true}
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

// Restricted 返回白名单是否开启。
func (w *Workspace) Restricted() bool {
	if w == nil {
		return false
	}
	w.mu.RLock()
	defer w.mu.RUnlock()
	return w.restrict
}

// SetRestricted 运行期切换白名单（Web 设置页热更新用）。
func (w *Workspace) SetRestricted(on bool) {
	if w == nil {
		return
	}
	w.mu.Lock()
	w.restrict = on
	w.mu.Unlock()
}

// Set 切换工作区。必须是已存在的目录，避免打错字后所有操作落到空目录。
// 切换 root 是管理员/Web UI 的显式动作，不受当前白名单约束。
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

// Resolve 把工具参数里的路径解析成绝对路径，并校验它落在白名单内。
// 相对路径以 root 为基准；绝对路径不再无条件放行。
// 白名单关闭时只做解析、不校验（兼容旧的显式出界行为）。
func (w *Workspace) Resolve(p string) (string, error) {
	target := w.resolvePath(p)
	if !w.Restricted() {
		return target, nil
	}
	root := w.Root()
	if root == "" {
		return target, nil // 进程 cwd 都没拿到时不假装安全，交给系统权限兜底
	}
	if err := checkWithin(root, target); err != nil {
		return "", err
	}
	return target, nil
}

// resolvePath 纯路径计算，不含策略判断。
func (w *Workspace) resolvePath(p string) string {
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

// checkWithin 校验 target 是否落在 root 内，且符号链接解析后仍在内。
//
// 目标文件可能还不存在（write_file 新建），这时逐级向上找最近的已存在祖先，
// 对祖先做 EvalSymlinks；剩余不存在的后缀不可能已是指向外面的链接。
func checkWithin(root, target string) error {
	// 第一层：纯路径判定。不依赖文件系统，通配符、目标不存在照样生效，
	// 直接挡住 `..`、绝对路径出界、兄弟目录前缀。
	if !pathWithin(root, target) {
		return fmt.Errorf("拒绝访问：%s 不在工作区 %s 内", target, root)
	}

	// 第二层：符号链接判定。root 自身可能是链接，先解析成真实路径。
	realRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		realRoot = filepath.Clean(root)
	}
	cur := target
	for {
		real, err := filepath.EvalSymlinks(cur)
		if err == nil {
			if !pathWithin(realRoot, real) {
				return fmt.Errorf("拒绝访问：%s 不在工作区 %s 内（链接解析后为 %s）", target, root, real)
			}
			return nil
		}
		// 目标可能不存在（write_file 新建），也可能含通配符（glob），
		// 两种情况下逐级上溯到最近的已存在祖先再判断。
		parent := filepath.Dir(cur)
		if parent == cur {
			return fmt.Errorf("路径校验失败：%w", err)
		}
		cur = parent
	}
}

// pathWithin 判断 p 是否等于 root 或位于其下。用 Rel 而非字符串前缀，
// 避免 /data/app 与 /data/application 这类兄弟目录被误判为包含。
func pathWithin(root, p string) bool {
	r, q := filepath.Clean(root), filepath.Clean(p)
	if runtime.GOOS == "windows" {
		r, q = strings.ToLower(r), strings.ToLower(q)
	}
	rel, err := filepath.Rel(r, q)
	if err != nil {
		return false
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return false
	}
	return !filepath.IsAbs(rel)
}
