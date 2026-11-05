// Package loop agent 主循环：user → LLM ↔ tools → final answer。
// 模型每次请求可发起若干 tool_calls；循环执行并回填结果，直到模型给出无工具调用的最终回复，
// 或触发最大轮数保护。
package loop

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"gsh/internal/api"
	"gsh/internal/kernel"
)

// MaxTurns 单次用户输入允许的最大模型调用轮数。
const MaxTurns = 24

// 工具执行默认超时。命令卡住时不能让整轮对话无限期静默。
const defaultToolTimeout = 60 * time.Second

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

// RunTraced 执行一次完整 agent 循环；onEvent 非 nil 时回调运行事件。
func (l *agentLoop) RunTraced(ctx context.Context, userText string, onDelta api.StreamDelta, onEvent api.EventHandler) (string, error) {
	return l.RunSession(ctx, "", userText, onDelta, onEvent)
}

// RunWithParts 带附件执行一轮（图片等多模态内容）。
func (l *agentLoop) RunWithParts(ctx context.Context, sessionID, userText string, parts []api.ContentPart, onDelta api.StreamDelta, onEvent api.EventHandler) (string, error) {
	k := kernel.Current
	if k == nil {
		return "", fmt.Errorf("loop: kernel not bound")
	}
	sess, reg, model, err := l.deps(k, sessionID)
	if err != nil {
		return "", err
	}
	return l.run(ctx, k, sess, reg, model, userText, parts, onDelta, onEvent)
}

// RunSession 在指定会话里执行一轮；sessionID 为空时沿用进程内默认会话。
//
// 为什么需要显式 sessionID：Web UI 的会话列表可以同时开多个会话，
// 若仍用「全局 activeSession」，用户在 A 会话里问一句会写进 B 的文件。
func (l *agentLoop) RunSession(ctx context.Context, sessionID string, userText string, onDelta api.StreamDelta, onEvent api.EventHandler) (string, error) {
	return l.RunWithParts(ctx, sessionID, userText, nil, onDelta, onEvent)
}

// deps 取出跑一轮所需的服务与会话。sessionID 为空时沿用进程内默认会话。
func (l *agentLoop) deps(k *kernel.Kernel, sessionID string) (api.Session, api.ToolRegistry, api.LLM, error) {
	store, err := kernel.Use[api.SessionStore](k, "sessionStore")
	if err != nil {
		return nil, nil, nil, err
	}
	reg, err := kernel.Use[api.ToolRegistry](k, "toolRegistry")
	if err != nil {
		return nil, nil, nil, err
	}
	model, err := kernel.Use[api.LLM](k, "llm")
	if err != nil {
		return nil, nil, nil, err
	}

	var sess api.Session
	if sessionID != "" {
		sess, err = store.Open(sessionID)
		if err != nil {
			return nil, nil, nil, err
		}
		return sess, reg, model, nil
	}
	sessv := k.Value("activeSession")
	if s, ok := sessv.(api.Session); ok && s != nil {
		return s, reg, model, nil
	}
	sess, err = store.New()
	if err != nil {
		return nil, nil, nil, err
	}
	k.SetValue("activeSession", sess)
	return sess, reg, model, nil
}

// run 循环本体：与「会话从哪来」解耦，便于 CLI/Web 共用。
func (l *agentLoop) run(
	ctx context.Context,
	k *kernel.Kernel,
	sess api.Session,
	reg api.ToolRegistry,
	model api.LLM,
	userText string,
	parts []api.ContentPart,
	onDelta api.StreamDelta,
	onEvent api.EventHandler,
) (string, error) {
	if err := sess.Append(api.Message{Role: api.RoleUser, Content: userText, Parts: parts}); err != nil {
		return "", err
	}

	// 并行执行工具时会有多个 goroutine 同时回调事件；SSE 的 ResponseWriter
	// 不是并发安全的，所以这里统一加锁串行化，顺序由完成先后决定。
	var emitMu sync.Mutex
	emit := func(ev api.Event) {
		if onEvent == nil {
			return
		}
		emitMu.Lock()
		defer emitMu.Unlock()
		onEvent(ev)
	}

	// 提示词服务可选：没装载时退回最小系统提示，绝不让模型裸跑。
	var builder api.PromptBuilder
	if b, err := kernel.Use[api.PromptBuilder](k, "promptBuilder"); err == nil {
		builder = b
	}
	cc := readCompactConfig(k)
	pc := readParallelConfig(k)
	toolTimeout := readToolTimeout(k)
	// 摘要用辅助模型；未注册该服务时退回主模型，单模型配置无感。
	aux, _ := kernel.Use[api.LLM](k, "auxLLM")
	if aux == nil {
		aux = model
	}

	for turn := 0; turn < MaxTurns; turn++ {
		// 压缩放在组包之前：摘要 + 最近轮次才是本轮真正发出去的历史。
		if ok, before, after, summarized := compact(ctx, sess, model, aux, cc); ok {
			notice := compactNotice(before, after, summarized)
			log.Printf("上下文压缩: %s", notice)
			emit(api.Event{Kind: api.EventCompacted, Compacted: after, Data: notice})
		}

		history := sess.Messages()
		system := "你是 gsh，一个在用户本机终端工作的 AI 编程助手。"
		if builder != nil {
			system = builder.Build(reg.Specs())
		}
		// 压缩摘要作为 history 的首条 system 落盘。这里合并进主提示，
		// 避免发出两条相邻 system 消息——部分网关只认第一条，摘要会被静默丢掉。
		if len(history) > 0 && history[0].Role == api.RoleSystem {
			system += "\n\n# 历史摘要\n" + history[0].Content
			history = history[1:]
		}
		msgs := make([]api.Message, 0, len(history)+1)
		msgs = append(msgs, api.Message{Role: api.RoleSystem, Content: system})
		msgs = append(msgs, history...)

		handlers := api.StreamHandlers{
			OnText: onDelta,
			OnReason: func(text string) {
				if strings.TrimSpace(text) != "" {
					emit(api.Event{Kind: api.EventReason, Data: text})
				}
			},
		}
		// 发出去之前先记下估算量，等 usage 回来做校准。
		// 工具表也计入——它在请求里同样占 token，漏掉会让估算系统性偏低。
		sentEstimate := api.EstimateTokensOf(msgs) + estimateSpecsTokens(reg.Specs())
		assistant, err := model.Chat(ctx, msgs, reg.Specs(), handlers)
		if err != nil {
			return "", err
		}
		// usage 取的是「刚结束的这次补全」；provider 没上报时是本地估算值。
		if ur, ok := model.(api.UsageReporter); ok {
			if u := ur.LastUsage(); u.TotalTokens > 0 {
				// 只有真实上报才拿来校准：用本地估算值校准自己等于自我强化，
				// 一旦某轮估偏就会一路偏下去。
				if !u.Estimated && u.PromptTokens > 0 {
					api.CalibrateTokens(sentEstimate, u.PromptTokens)
				}
				emit(api.Event{Kind: api.EventUsage, Usage: &u})
			}
		}
		// 推理内容只走事件通道，不写进会话历史。
		assistant.Reasoning = ""
		if err := sess.Append(assistant); err != nil {
			return "", err
		}
		if len(assistant.ToolCalls) == 0 {
			emit(api.Event{Kind: api.EventFinal, Data: assistant.Content})
			return assistant.Content, nil
		}
		// 工具执行：并行或串行在上限内完成，结果仍按模型给出的调用顺序回填。
		// 顺序不能乱——tool 消息必须与 assistant 的 tool_call_id 一一对应。
		results := execToolCalls(ctx, sess.ID(), reg, pc, assistant.ToolCalls, toolTimeout, emit)
		// 清单变更单独上抛：界面不用解析工具返回文本就能画出进度。
		for _, call := range assistant.ToolCalls {
			if call.Name != "todo_write" {
				continue
			}
			if ts, terr := kernel.Use[api.TodoStore](k, "todo"); terr == nil && ts != nil {
				emit(api.Event{Kind: api.EventTodo, Todo: ts.Items()})
			}
			break
		}
		for _, res := range results {
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

// readToolTimeout 从 tools 配置读单次工具执行超时（秒）。
func readToolTimeout(k *kernel.Kernel) time.Duration {
	if k == nil {
		return defaultToolTimeout
	}
	cfg, ok := k.Options("tools")
	if !ok || cfg == nil {
		return defaultToolTimeout
	}
	if f, ok := number(cfg["tool_timeout_seconds"]); ok && f > 0 {
		return time.Duration(f * float64(time.Second))
	}
	return defaultToolTimeout
}

// parallelConfig 工具并行执行策略。
// 默认开启：模型一次给出多个互不相关的读取/检索调用时，串行执行纯属浪费。
type parallelConfig struct {
	Enabled bool
	Limit   int
}

// estimateSpecsTokens 估算工具定义本身占的 token。
//
// 工具表每次请求都要带上，是一份固定开销：工具多了能到 1~2k token。
// 漏掉它，估算会系统性偏低——工具越多偏得越多，正好在「工具多、上下文紧」
// 这种最容易撞窗口的场景下失准。
func estimateSpecsTokens(specs []api.ToolSpec) int {
	n := 0
	for _, s := range specs {
		b, err := json.Marshal(s)
		if err != nil {
			continue
		}
		n += api.EstimateTokensOf([]api.Message{{Content: string(b)}})
	}
	return n
}

func readParallelConfig(k *kernel.Kernel) parallelConfig {
	c := parallelConfig{Enabled: true, Limit: 4}
	if k == nil {
		return c
	}
	cfg, ok := k.Options("tools")
	if !ok || cfg == nil {
		return c
	}
	if v, ok := cfg["parallel_tools"].(bool); ok {
		c.Enabled = v
	}
	if f, ok := number(cfg["parallel_limit"]); ok && f >= 2 {
		c.Limit = int(f)
	}
	return c
}

// execToolCalls 执行一轮全部工具调用。
//
// 并行有两个前提：没有交互式审批器（弹窗会抢同一路输入），且确有多个调用。
// 事件的 tool_call 先全部发出，让界面立刻把待执行项列出来；
// tool_result 按实际完成顺序发出（带 call ID，前端据此配对）。
func execToolCalls(ctx context.Context, sessionID string, reg api.ToolRegistry, pc parallelConfig, calls []api.ToolCall, timeout time.Duration, emit func(api.Event)) []api.ToolResult {
	if len(calls) == 0 {
		return nil
	}
	for _, call := range calls {
		emit(api.Event{Kind: api.EventToolCall, ID: call.ID, Name: call.Name, Args: call.Args})
	}

	out := make([]api.ToolResult, len(calls))
	// 钩子取一次就够；没装钩子插件时 hr 为 nil，整条路径直接绕过。
	var hr api.HookRunner
	if k := kernel.Current; k != nil {
		if v, err := kernel.Use[api.HookRunner](k, "hooks"); err == nil {
			hr = v
		}
	}
	run := func(i int) {
		call := calls[i]
		// 前钩子：可以在真正执行前拦下这次调用。
		// 拦截也走正常的结果回填，让模型看到「为什么被拒绝」而不是凭空少了条结果。
		if hr != nil {
			if r := hr.Run(ctx, api.HookEvent{Phase: api.HookBeforeTool, Tool: call.Name, Args: call.Args, SessionID: sessionID}); r.Blocked {
				out[i] = api.ToolResult{
					CallID:  call.ID,
					Name:    call.Name,
					Content: "[hook] " + r.Message,
					IsError: true,
				}
				emit(api.Event{Kind: api.EventToolResult, ID: call.ID, Name: call.Name, Data: out[i].Content, Err: true})
				return
			}
		}
		// 会话 id 注入 ctx，让审计能关联到具体会话；
		// 单次执行加超时，卡住的命令不会拖死整轮对话。
		execCtx := context.WithValue(ctx, api.AuditSessionKey, sessionID)
		execCtx, cancel := context.WithTimeout(execCtx, timeout)
		res := reg.Execute(execCtx, call)
		cancel()
		// 后钩子：给结果补一段外部判断（格式检查、敏感信息提示之类）。
		if hr != nil {
			if r := hr.Run(ctx, api.HookEvent{
				Phase: api.HookAfterTool, Tool: call.Name, Args: call.Args,
				SessionID: sessionID, Result: res.Content, IsError: res.IsError,
			}); r.Message != "" {
				res.Content = strings.TrimRight(res.Content, "\n") + "\n[hook] " + r.Message
			}
		}
		out[i] = res
		emit(api.Event{Kind: api.EventToolResult, ID: res.CallID, Name: res.Name, Data: res.Content, Err: res.IsError})
	}

	serial := len(calls) == 1 || !pc.Enabled || reg.HasApprover()
	if serial {
		for i := range calls {
			run(i)
		}
		return out
	}

	limit := pc.Limit
	if limit < 2 {
		limit = 2
	}
	sem := make(chan struct{}, limit)
	var wg sync.WaitGroup
	for i := range calls {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			run(i)
		}(i)
	}
	wg.Wait()
	return out
}
