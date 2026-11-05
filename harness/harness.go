// Package harness 把 gsh 从「一个 CLI 应用」降维成「一个可嵌入的系统组件」。
//
// gsh 的内核是 Go 包，任何项目都能 import 进来，三行代码长出 agent 能力：
//
//	h := harness.New(cfg)
//	h.LoadDefaults()           // 装载默认插件集
//	h.Serve(":8080")           // 或 h.Run(ctx, prompt, nil)
//
// 也可只挑选需要的插件装载，而非全量。
package harness

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"gsh/internal/api"
	"gsh/internal/codeindex"
	"gsh/internal/hooks"
	"gsh/internal/kernel"
	"gsh/internal/llm"
	"gsh/internal/loop"
	"gsh/internal/mcp"
	"gsh/internal/memory"
	"gsh/internal/prompt"
	"gsh/internal/session"
	"gsh/internal/subagent"
	"gsh/internal/tools"
	"gsh/internal/tui"
	"gsh/internal/ui"
	"gsh/internal/web"
)

// Harness 是 gsh 作为组件的高层入口：组合内核与默认插件集。
type Harness struct {
	k *kernel.Kernel
}

// New 用给定配置创建 harness，并绑定全局内核（loop 运行时取用）。
func New(cfg *kernel.Config) *Harness {
	k := kernel.New(cfg)
	kernel.Current = k // 单内核进程模型；loop 等运行时服务据此取用
	return &Harness{k: k}
}

// LoadDefaults 装载默认插件集。
// prompt 必须排在 tools 之后：它要读 tools 发布的工作区与权限档位。
func (h *Harness) LoadDefaults() error {
	loaders := []func(*kernel.Kernel) error{
		session.RegisterKernel,
		tools.RegisterKernel,
		memory.RegisterKernel,    // 依赖 tools 的注册表（remember 工具）
		hooks.RegisterKernel,     // 工具执行前后的外部钩子
		codeindex.RegisterKernel, // 依赖 tools 的工作区与注册表
		llm.RegisterKernel,
		prompt.RegisterKernel,
		loop.RegisterKernel,
		subagent.RegisterKernel, // 向注册表加 task 工具，需 prompt 已就绪
		mcp.RegisterKernel,      // 把外部 MCP server 的工具并进同一张表
		web.RegisterKernel,
		ui.RegisterKernel,
	}
	for _, load := range loaders {
		if err := load(h.k); err != nil {
			return err
		}
	}
	return nil
}

// Use 按类型取一个已注册服务（包级泛型函数；Go 1.24 不支持泛型方法）。
// 例：loop, err := harness.Use[api.Loop](h, "loop")
func Use[T any](h *Harness, name string) (T, error) { return kernel.Use[T](h.k, name) }

// LoadedPlugins 返回已装载插件名（按装载序）。
func (h *Harness) LoadedPlugins() []string { return h.k.LoadedPlugins() }

// Run 执行一次 prompt（REPL / headless 共用），返回最终回复文本。
func (h *Harness) Run(ctx context.Context, prompt string, onDelta api.StreamDelta) (string, error) {
	lp, err := Use[api.Loop](h, "loop")
	if err != nil {
		return "", err
	}
	return lp.Run(ctx, prompt, onDelta)
}

// RunWithImages 带图片执行一轮（headless）。图片按本地路径读取，
// 路径不存在时直接报错——静默忽略图片会让用户以为模型看过了。
func (h *Harness) RunWithImages(ctx context.Context, prompt, sessionID string, images []string, onDelta api.StreamDelta) (string, error) {
	lp, err := Use[api.Loop](h, "loop")
	if err != nil {
		return "", err
	}
	parts := make([]api.ContentPart, 0, len(images))
	for _, p := range images {
		if _, serr := os.Stat(p); serr != nil {
			return "", fmt.Errorf("图片不存在: %s: %w", p, serr)
		}
		abs, aerr := filepath.Abs(p)
		if aerr != nil {
			abs = p
		}
		parts = append(parts, api.ContentPart{Type: "image", Path: abs})
	}
	return lp.RunWithParts(ctx, sessionID, prompt, parts, onDelta, nil)
}

// Serve 启动 Web UI 服务（阻塞），addr 为空则用配置中的地址。
func (h *Harness) Serve(addr string) error {
	ws, err := Use[*web.Server](h, "webServer")
	if err != nil {
		return err
	}
	if addr != "" {
		ws.SetAddr(addr)
	}
	return ws.Run(h.k)
}

// RunREPL 交互模式（终端）。工具调用/结果会打印到终端。
func (h *Harness) RunREPL(ctx context.Context) error {
	lp, err := Use[api.Loop](h, "loop")
	if err != nil {
		return err
	}
	return ui.RunREPL(ctx, func(ctx context.Context, in string, d api.StreamDelta) (string, error) {
		return lp.RunTraced(ctx, in, d, ui.PrintTrace)
	})
}

// RunTUI 启动终端 UI（阻塞）。复用内核的 loop / session 服务。
func (h *Harness) RunTUI() error {
	return tui.Run(h.k)
}

// RunTUIReadOnly 以只读浏览模式打开指定会话（TUI）：可滚动查看历史，禁止发送。
func (h *Harness) RunTUIReadOnly(id string) error {
	return tui.RunReadOnly(h.k, id)
}

// RunHeadless 一次性执行模式：单条输入 → 回复 → 退出。工具调用/结果会打印到终端。
func (h *Harness) RunHeadless(ctx context.Context, prompt string) error {
	lp, err := Use[api.Loop](h, "loop")
	if err != nil {
		return err
	}
	return ui.RunHeadless(ctx, prompt, func(ctx context.Context, in string, d api.StreamDelta) (string, error) {
		return lp.RunTraced(ctx, in, d, ui.PrintTrace)
	})
}

// ListSessions 列出所有会话（id / 标题 / 时间 / 消息数），供 CLI 概览。
func (h *Harness) ListSessions() ([]api.SessionMeta, error) {
	store, err := Use[api.SessionStore](h, "sessionStore")
	if err != nil {
		return nil, err
	}
	return store.ListMeta()
}

// SearchSessions 在会话正文里检索关键词，供 CLI / TUI / Web 共用。
func (h *Harness) SearchSessions(query string, limit int) ([]api.SessionHit, error) {
	store, err := Use[api.SessionStore](h, "sessionStore")
	if err != nil {
		return nil, err
	}
	return store.Search(query, limit)
}

// ForkSession 以指定会话为蓝本派生新会话，返回新会话 id。
func (h *Harness) ForkSession(id string) (string, error) {
	store, err := Use[api.SessionStore](h, "sessionStore")
	if err != nil {
		return "", err
	}
	neu, err := store.Fork(id)
	if err != nil {
		return "", err
	}
	return neu.ID(), nil
}

// ReplaySession 把指定会话的事件流渲染为可读轨迹文本（Trajectory 视图）。
func (h *Harness) ReplaySession(id string) (string, error) {
	store, err := Use[api.SessionStore](h, "sessionStore")
	if err != nil {
		return "", err
	}
	sess, err := store.Open(id)
	if err != nil {
		return "", err
	}
	return ui.RenderTrajectory(sess), nil
}

// RunTUIWithSession 启动终端 UI 并定位到指定会话（resume / fork 后继续对话）。
func (h *Harness) RunTUIWithSession(sessionID string) error {
	return tui.RunWithSession(h.k, sessionID)
}

// RunREPLWithSession 在指定会话上启动交互 REPL。
func (h *Harness) RunREPLWithSession(ctx context.Context, sessionID string) error {
	lp, err := Use[api.Loop](h, "loop")
	if err != nil {
		return err
	}
	return ui.RunREPL(ctx, func(ctx context.Context, in string, d api.StreamDelta) (string, error) {
		return lp.RunSession(ctx, sessionID, in, d, ui.PrintTrace)
	})
}

// RunHeadlessWithSession 在指定会话上一次性执行。
func (h *Harness) RunHeadlessWithSession(ctx context.Context, prompt, sessionID string) error {
	lp, err := Use[api.Loop](h, "loop")
	if err != nil {
		return err
	}
	return ui.RunHeadless(ctx, prompt, func(ctx context.Context, in string, d api.StreamDelta) (string, error) {
		return lp.RunSession(ctx, sessionID, in, d, ui.PrintTrace)
	})
}

// Shutdown 停机：倒序执行各插件清理函数。
func (h *Harness) Shutdown() { h.k.Shutdown() }
