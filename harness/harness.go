// Package harness 把 gsh 从「一个 CLI 应用」降维成「一个可嵌入的系统组件」。
//
// 这是重写 DSH 的真正目的：DSH 是一个跑不起来的 Node 应用（冷启动 5 分钟、
// 1GB 依赖、MIT 只读投放）；gsh 的内核是 Go 包，任何项目——
// zcbot、AI 沙箱、你自己的运维脚本——都能 import 进来，三行代码长出 agent 能力：
//
//	h := harness.New(cfg)
//	h.LoadDefaults()           // 装载默认插件集
//	h.Serve(":8080")           // 或 h.Run(ctx, prompt, nil)
//
// 也可只挑选需要的插件装载，而非全量。
package harness

import (
	"context"

	"gsh/internal/api"
	"gsh/internal/kernel"
	"gsh/internal/llm"
	"gsh/internal/loop"
	"gsh/internal/session"
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

// LoadDefaults 装载默认插件集：session → tools → llm → loop → web → ui。
func (h *Harness) LoadDefaults() error {
	loaders := []func(*kernel.Kernel) error{
		session.RegisterKernel,
		tools.RegisterKernel,
		llm.RegisterKernel,
		loop.RegisterKernel,
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

// RunTUI 启动 opencode 风格的终端 UI（阻塞）。复用内核的 loop / session 服务。
func (h *Harness) RunTUI() error {
	return tui.Run(h.k)
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

// Shutdown 停机：倒序执行各插件清理函数。
func (h *Harness) Shutdown() { h.k.Shutdown() }
