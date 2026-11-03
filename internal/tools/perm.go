package tools

import (
	"fmt"
	"strings"
	"sync"
)

// Perm 权限档位。
//
// 存在的理由：DSH 输入区的「完全权限」不是装饰，它决定 agent 能做什么。
// 只有两档（拦截/放行破坏性命令）不够用：读代码和改代码应该是两种授权，
// 而不是逼用户在做只读分析时也把写权限交出去。
type Perm int

const (
	// PermReadOnly 只允许读取与网络访问：不执行 shell，不写文件。
	PermReadOnly Perm = iota
	// PermStandard 允许写文件与常规命令，但破坏性命令仍被拦截。
	PermStandard
	// PermFull 完全权限：破坏性命令放行（仍然拦截灾难性命令）。
	PermFull
)

// ParsePerm 解析配置/接口传入的档位名。空串按标准模式处理。
func ParsePerm(s string) (Perm, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "standard", "default":
		return PermStandard, nil
	case "read_only", "readonly", "read", "plan":
		return PermReadOnly, nil
	case "full", "danger", "yolo", "all":
		return PermFull, nil
	}
	return PermStandard, fmt.Errorf("未知权限档位 %q（read_only|standard|full）", s)
}

// String 档位名，用于接口与日志。
func (p Perm) String() string {
	switch p {
	case PermReadOnly:
		return "read_only"
	case PermFull:
		return "full"
	}
	return "standard"
}

// Label 中文标签，供 UI 直接显示。
func (p Perm) Label() string {
	switch p {
	case PermReadOnly:
		return "只读"
	case PermFull:
		return "完全权限"
	}
	return "标准"
}

// Guard 运行期权限闸门。
//
// 并发安全：Web UI 可以在会话运行中切档，工具线程随时会读。
type Guard struct {
	mu   sync.RWMutex
	mode Perm
}

// NewGuard 创建闸门。
func NewGuard(p Perm) *Guard { return &Guard{mode: p} }

// Mode 当前档位。
func (g *Guard) Mode() Perm {
	if g == nil {
		return PermStandard
	}
	g.mu.RLock()
	defer g.mu.RUnlock()
	return g.mode
}

// Set 切换档位。
func (g *Guard) Set(p Perm) {
	if g == nil {
		return
	}
	g.mu.Lock()
	g.mode = p
	g.mu.Unlock()
}

// AllowDestructive 是否放行受限（破坏性）命令。
func (g *Guard) AllowDestructive() bool { return g.Mode() == PermFull }

// mutationTools 只读模式下被拒绝的工具。
// 用白名单反推容易漏：新增写工具时忘了加白名单就等于默认放行，
// 所以这里显式列黑名单，并让 IsReadOnlyTool 兜住未知工具。
var mutationTools = map[string]bool{
	"bash":        true,
	"write_file":  true,
	"edit_file":   true,
	"make_dir":    true,
	"move_path":   true,
	"copy_path":   true,
	"delete_path": true,
}

// Check 判断某工具在当前档位下是否可用。
// 返回空串表示放行，否则是要回给模型的原因。
func (g *Guard) Check(tool string) string {
	if g.Mode() != PermReadOnly {
		return ""
	}
	if mutationTools[tool] {
		return fmt.Sprintf("当前为只读模式，%s 不可用。需要修改文件或执行命令时，请让用户在输入区把权限切换到「标准」。", tool)
	}
	return ""
}
