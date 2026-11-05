// Package uiplugin 交互层：REPL（交互会话）与 headless（-e 一次性执行）。
package ui

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/signal"
	"strings"

	"github.com/charmbracelet/lipgloss"
	"gsh/internal/api"
	"gsh/internal/kernel"
	"gsh/internal/tools"
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
		_ = printAnswer(exec, ctx, line)
	}
	return nil
}

// RunHeadless 一次性执行模式：单条输入 → 回复 → 退出。
// 执行失败时返回错误，由 main 写日志并以非 0 退出码结束。
func RunHeadless(ctx context.Context, input string, exec func(ctx context.Context, input string, onDelta api.StreamDelta) (string, error)) error {
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt)
	defer stop()
	return printAnswer(exec, ctx, input)
}

// printAnswer 执行并输出：流式 delta 已打过的内容不重复打印。返回执行错误。
func printAnswer(exec func(ctx context.Context, string string, onDelta api.StreamDelta) (string, error), ctx context.Context, line string) error {
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
		return err
	}
	if out != "" && !printed {
		fmt.Println(out)
	}
	return nil
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

// Trajectory 视图样式（与终端 UI 共用同一套配色语义）。
var (
	trUser = lipgloss.NewStyle().Foreground(lipgloss.Color("#5898f0"))
	trDim  = lipgloss.NewStyle().Foreground(lipgloss.Color("#909090"))
	trTool = lipgloss.NewStyle().Foreground(lipgloss.Color("#5fd7af"))
	trAdd  = lipgloss.NewStyle().Foreground(lipgloss.Color("#3fb950"))
	trDel  = lipgloss.NewStyle().Foreground(lipgloss.Color("#f85149"))
)

// RenderTrajectory 把会话消息重建成可读的轨迹文本（Trajectory 视图）。
// 与终端 UI 的回放共用同一套语义：用户块 / 助手文本 / 工具调用行 + diff。
// 用作 `gsh -replay <id>` 的非交互回放，便于在无图形终端里复盘一次 agent 运行。
func RenderTrajectory(sess api.Session) string {
	msgs := sess.Messages()
	toolRes := map[string]api.Message{}
	for _, m := range msgs {
		if m.Role == api.RoleTool && m.ToolCallID != "" {
			toolRes[m.ToolCallID] = m
		}
	}
	var b strings.Builder
	b.WriteString(trUser.Render("# "+sess.ID()) + "\n")
	for _, m := range msgs {
		switch m.Role {
		case api.RoleUser:
			if strings.TrimSpace(m.Content) == "" {
				continue
			}
			b.WriteString("\n" + trUser.Render("◆ 你") + "\n")
			b.WriteString(indentBlock(m.Content, "  ") + "\n")
		case api.RoleAssistant:
			for _, tc := range m.ToolCalls {
				b.WriteString("\n" + trTool.Render("* "+tc.Name) + " " + trDim.Render(compactArgs(tc.Args)) + "\n")
				if res, ok := toolRes[tc.ID]; ok {
					rest, diff := tools.ExtractDiff(res.Content)
					if diff != "" {
						b.WriteString(renderDiffBlock(diff))
					}
					if rest != "" {
						b.WriteString(indentBlock(compactText(rest), "  ") + "\n")
					}
				}
			}
			if strings.TrimSpace(m.Content) != "" {
				b.WriteString("\n" + trDim.Render("◇ 助手") + "\n")
				b.WriteString(indentBlock(m.Content, "  ") + "\n")
			}
		}
	}
	return b.String()
}

// indentBlock 给每一行加前缀。
func indentBlock(s, prefix string) string {
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		lines[i] = prefix + l
	}
	return strings.Join(lines, "\n")
}

// renderDiffBlock 给统一 diff 的增/删/头行上色。
func renderDiffBlock(diff string) string {
	var b strings.Builder
	for _, line := range strings.Split(diff, "\n") {
		switch {
		case strings.HasPrefix(line, "+++"), strings.HasPrefix(line, "---"):
			b.WriteString(trDim.Render(line) + "\n")
		case strings.HasPrefix(line, "@@"):
			b.WriteString(trDim.Render(line) + "\n")
		case strings.HasPrefix(line, "+"):
			b.WriteString(trAdd.Render(line) + "\n")
		case strings.HasPrefix(line, "-"):
			b.WriteString(trDel.Render(line) + "\n")
		default:
			b.WriteString(line + "\n")
		}
	}
	return b.String()
}
