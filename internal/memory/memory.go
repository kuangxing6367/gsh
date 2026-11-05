// Package memory 跨会话长期记忆。
//
// 与 todo 的区别：清单记的是「这一轮做到哪了」，记忆记的是「以后都该知道的事」——
// 用户的偏好、项目的硬约定、曾经踩过的坑。前者用完即弃，后者要活过会话边界。
package memory

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"gsh/internal/api"
	"gsh/internal/kernel"
)

// 注入系统提示的上限。记忆文件本身不裁剪（用户可能要长期积累），
// 但每次只把最近的这段送进上下文，避免把窗口吃光。
const injectLimit = 4000

// RegisterKernel 挂载记忆插件。必须排在 tools 之后：它要向工具注册表加 remember。
func RegisterKernel(k *kernel.Kernel) error { return k.Load(&Plugin{}) }

// Plugin 记忆插件。
type Plugin struct{}

// Name 实现 kernel.Plugin。
func (p *Plugin) Name() string { return "memory" }

// Apply 打开记忆文件、注册服务与 remember 工具。
func (p *Plugin) Apply(ctx *kernel.Ctx) error {
	// 数据目录已经是 .gsh/memory（按插件名分配），不要再套一层。
	path := filepath.Join(ctx.DataDir(), "MEMORY.md")
	if cfg := ctx.Config(); cfg != nil {
		if v, ok := cfg["path"].(string); ok && v != "" {
			if !filepath.IsAbs(v) {
				v = filepath.Join(ctx.BaseDir(), v)
			}
			path = v
		}
		if v, ok := cfg["enabled"].(bool); ok && !v {
			return nil
		}
	}
	store, err := newStore(path)
	if err != nil {
		return err
	}
	ctx.Provide("memory", api.MemoryStore(store))

	// 工具注册表由 tools 插件提供；缺失时（精简插件集）只保留提示词注入。
	if reg, err := kernel.Use[api.ToolRegistry](kernel.Current, "toolRegistry"); err == nil && reg != nil {
		if err := reg.Register(&RememberTool{store: store}); err != nil {
			return err
		}
	}
	return nil
}

// store 文件型记忆：追加写，读取时取最近一段。
type store struct {
	mu   sync.RWMutex
	path string
	body string
}

func newStore(path string) (*store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("memory: 创建目录失败: %w", err)
	}
	s := &store{path: path}
	if err := s.load(); err != nil {
		return nil, err
	}
	// 文件不存在视为空记忆，首次 Append 时创建。
	return s, nil
}

func (s *store) load() error {
	b, err := os.ReadFile(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("memory: 读取失败: %w", err)
	}
	s.mu.Lock()
	s.body = string(b)
	s.mu.Unlock()
	return nil
}

// Path 记忆文件位置，启动诊断与 remember 的返回值用。
func (s *store) Path() string { return s.path }

// Text 返回注入系统提示的内容。
func (s *store) Text() string {
	s.mu.RLock()
	body := s.body
	s.mu.RUnlock()
	body = strings.TrimSpace(body)
	if body == "" {
		return ""
	}
	if len(body) > injectLimit {
		// 按 rune 截断，避免把多字节字符切坏。
		r := []rune(body)
		if len(r) > injectLimit {
			r = r[len(r)-injectLimit:]
		}
		return "（较早的记忆已省略）\n" + string(r)
	}
	return body
}

// Append 追加一条带时间戳的记录。
func (s *store) Append(note string) error {
	note = strings.TrimSpace(note)
	if note == "" {
		return fmt.Errorf("记录内容不能为空")
	}
	line := fmt.Sprintf("- %s %s\n", time.Now().Format("2006-01-02"), strings.ReplaceAll(note, "\n", " "))
	f, err := os.OpenFile(s.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("memory: 写入失败: %w", err)
	}
	defer f.Close()
	if _, err := f.WriteString(line); err != nil {
		return err
	}
	s.mu.Lock()
	if s.body != "" && !strings.HasSuffix(s.body, "\n") {
		s.body += "\n"
	}
	s.body += line
	s.mu.Unlock()
	return nil
}

// RememberTool 把一条长期有效的信息写进记忆文件。
type RememberTool struct {
	store *store
}

// Spec 实现 api.Tool。
func (t *RememberTool) Spec() api.ToolSpec {
	return api.ToolSpec{
		Name:     "remember",
		ReadOnly: true,
		Description: "把一条跨会话长期有效的信息写进记忆文件（用户偏好、项目硬约定、踩过的坑）。" +
			"只记以后若干次对话都用得上的内容，不要记临时的文件内容或本次任务的中间结果。",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"note": map[string]any{"type": "string", "description": "一句话，说清「记住什么」，不要写理由铺垫"},
			},
			"required": []string{"note"},
		},
	}
}

// Run 实现 api.Tool。
func (t *RememberTool) Run(_ context.Context, argsJSON string) (string, error) {
	var args struct {
		Note string `json:"note"`
	}
	if err := unmarshalArgs(argsJSON, &args); err != nil {
		return "", err
	}
	if err := t.store.Append(args.Note); err != nil {
		return "", err
	}
	return fmt.Sprintf("已记入 %s", t.store.Path()), nil
}

func unmarshalArgs(s string, v any) error {
	if strings.TrimSpace(s) == "" {
		return fmt.Errorf("缺少参数")
	}
	return json.Unmarshal([]byte(s), v)
}
