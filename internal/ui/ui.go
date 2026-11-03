// Package uiplugin 交互层：REPL（交互会话）与 headless（-e 一次性执行）。
package ui

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/signal"
	"strings"

	"gsh/internal/api"
	"gsh/internal/kernel"
)

// RegisterKernel 挂载 UI 插件。
func RegisterKernel(k *kernel.Kernel) error { return k.Load(&Plugin{}) }

// Plugin UI 插件。
type Plugin struct{}

// Name 实现 kernel.Plugin。
func (p *Plugin) Name() string { return "ui" }

// Apply 无服务注册，仅提供入口函数给 main。
func (p *Plugin) Apply(_ *kernel.Ctx) error { return nil }

// RunREPL 交互模式。exec 为注入的执行函数（避免 UI 直接依赖 loop 包）。
func RunREPL(ctx context.Context, exec func(ctx context.Context, input string, onDelta api.StreamDelta) (string, error)) error {
	fmt.Println("gsh — 一切皆插件的 agent（Go 版）· Ctrl+C / /quit 退出")
	sc := bufio.NewScanner(os.Stdin)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for {
		fmt.Print("\n> ")
		if !sc.Scan() {
			break
		}
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		switch line {
		case "/quit", "/exit", "/q":
			return nil
		}
		printAnswer(exec, ctx, line)
	}
	return nil
}

// RunHeadless 一次性执行模式：单条输入 → 回复 → 退出。
func RunHeadless(ctx context.Context, input string, exec func(ctx context.Context, input string, onDelta api.StreamDelta) (string, error)) error {
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt)
	defer stop()
	printAnswer(exec, ctx, input)
	return nil
}

// printAnswer 执行并输出：流式 delta 已打过的内容不重复打印。
func printAnswer(exec func(ctx context.Context, string string, onDelta api.StreamDelta) (string, error), ctx context.Context, line string) {
	printed := false
	out, err := exec(ctx, line, func(t string) {
		if !printed {
			printed = true
		}
		fmt.Print(t)
	})
	fmt.Println()
	if err != nil {
		fmt.Printf("[error] %v\n", err)
		return
	}
	if out != "" && !printed {
		fmt.Println(out)
	}
}

// PrintTrace 把 loop 的事件流打到终端（工具调用/结果）。
// 让 headless/REPL 能看见命令到底执行了什么、跑在哪台机器上。
func PrintTrace(ev api.Event) {
	switch ev.Kind {
	case api.EventToolCall:
		fmt.Printf("[tool] %s %s\n", ev.Name, compactArgs(ev.Args))
	case api.EventToolResult:
		fmt.Printf("[result] %s\n", compactText(ev.Data))
	}
}

// compactArgs 压缩工具参数为单行摘要。
func compactArgs(argsJSON string) string {
	s := strings.Join(strings.Fields(argsJSON), " ")
	if len(s) > 160 {
		s = s[:160] + "..."
	}
	return s
}

// compactText 压缩工具结果为单行摘要。
func compactText(text string) string {
	s := strings.Join(strings.Fields(text), " ")
	if len(s) > 160 {
		s = s[:160] + "..."
	}
	return s
}
