// Package loop agent 主循环：user → LLM ↔ tools → final answer。
// 模型每次请求可发起若干 tool_calls；循环执行并回填结果，直到模型给出无工具调用的最终回复，
// 或触发最大轮数保护。
package loop

import (
	"context"
	"fmt"

	"gsh/internal/api"
	"gsh/internal/kernel"
)

// MaxTurns 单次用户输入允许的最大模型调用轮数。
const MaxTurns = 24

// RegisterKernel 挂载 loop 插件。
func RegisterKernel(k *kernel.Kernel) error { return k.Load(&Plugin{}) }

// Plugin loop 插件。
type Plugin struct{}

// Name 实现 kernel.Plugin。
func (p *Plugin) Name() string { return "loop" }

// Apply 组装 loop 服务（依赖 sessionStore / toolRegistry / llm）。
func (p *Plugin) Apply(ctx *kernel.Ctx) error {
	ctx.Provide("loop", &agentLoop{})
	return nil
}

type agentLoop struct{}

// Run 无事件回调的便捷入口。
func (l *agentLoop) Run(ctx context.Context, userText string, onDelta api.StreamDelta) (string, error) {
	return l.RunTraced(ctx, userText, onDelta, nil)
}

// RunTraced 执行一次完整 agent 循环；onEvent 非 nil 时回调 tool_call/tool_result/final 事件。
func (l *agentLoop) RunTraced(ctx context.Context, userText string, onDelta api.StreamDelta, onEvent api.EventHandler) (string, error) {
	return l.RunSession(ctx, "", userText, onDelta, onEvent)
}

// RunSession 在指定会话里执行一轮；sessionID 为空时沿用进程内默认会话。
//
// 为什么需要显式 sessionID：Web UI 的会话列表可以同时开多个会话，
// 若仍用「全局 activeSession」，用户在 A 会话里问一句会写进 B 的文件。
func (l *agentLoop) RunSession(ctx context.Context, sessionID string, userText string, onDelta api.StreamDelta, onEvent api.EventHandler) (string, error) {
	k := kernel.Current
	if k == nil {
		return "", fmt.Errorf("loop: kernel not bound")
	}
	store, err := kernel.Use[api.SessionStore](k, "sessionStore")
	if err != nil {
		return "", err
	}
	reg, err := kernel.Use[api.ToolRegistry](k, "toolRegistry")
	if err != nil {
		return "", err
	}
	model, err := kernel.Use[api.LLM](k, "llm")
	if err != nil {
		return "", err
	}

	var sess api.Session
	if sessionID != "" {
		sess, err = store.Open(sessionID)
		if err != nil {
			return "", err
		}
	} else {
		sessv := k.Value("activeSession")
		if s, ok := sessv.(api.Session); ok && s != nil {
			sess = s
		} else {
			sess, err = store.New()
			if err != nil {
				return "", err
			}
			k.SetValue("activeSession", sess)
		}
	}

	return l.run(ctx, sess, reg, model, userText, onDelta, onEvent)
}

// run 循环本体：与「会话从哪来」解耦，便于 CLI/Web 共用。
func (l *agentLoop) run(
	ctx context.Context,
	sess api.Session,
	reg api.ToolRegistry,
	model api.LLM,
	userText string,
	onDelta api.StreamDelta,
	onEvent api.EventHandler,
) (string, error) {
	if err := sess.Append(api.Message{Role: api.RoleUser, Content: userText}); err != nil {
		return "", err
	}

	emit := func(ev api.Event) {
		if onEvent != nil {
			onEvent(ev)
		}
	}

	for turn := 0; turn < MaxTurns; turn++ {
		assistant, err := model.Chat(ctx, sess.Messages(), reg.Specs(), onDelta)
		if err != nil {
			return "", err
		}
		if err := sess.Append(assistant); err != nil {
			return "", err
		}
		if len(assistant.ToolCalls) == 0 {
			emit(api.Event{Kind: api.EventFinal, Data: assistant.Content})
			return assistant.Content, nil // 最终回复
		}
		for _, call := range assistant.ToolCalls {
			emit(api.Event{Kind: api.EventToolCall, ID: call.ID, Name: call.Name, Args: call.Args})
			// 把会话 id 注入 ctx，让工具执行入口的审计能关联到「哪个会话」。
			execCtx := context.WithValue(ctx, api.AuditSessionKey, sess.ID())
			res := reg.Execute(execCtx, call)
			emit(api.Event{Kind: api.EventToolResult, ID: res.CallID, Name: res.Name, Data: res.Content, Err: res.IsError})
			if err := sess.Append(api.Message{
				Role:       api.RoleTool,
				Content:    res.Content,
				ToolCallID: res.CallID,
				Name:       res.Name,
			}); err != nil {
				return "", err
			}
		}
	}
	return "", fmt.Errorf("loop: exceeded %d model turns without final answer", MaxTurns)
}
