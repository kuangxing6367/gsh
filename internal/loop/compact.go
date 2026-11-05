// Package loop 的上下文压缩实现。
//
// 为什么按「轮」而不是按「条」切：一条 assistant 消息若带 tool_calls，
// 后面必须紧跟每个 call 的 tool 结果；从中间截断会留下悬空的 tool_call_id，
// 下一轮请求直接被 provider 判为格式非法。所以切点只能落在 user 消息边界。
package loop

import (
	"context"
	"fmt"
	"log"
	"strings"

	"gsh/internal/api"
	"gsh/internal/kernel"
)

// compactConfig 压缩阈值。默认值与长会话实测可用的区间对齐，
// 全部可由 plugins_cfg.session 覆盖；compact_enabled=false 可整体关闭。
type compactConfig struct {
	Enabled   bool
	MaxChars  int
	KeepTurns int
	HardCap   int
	// MaxTokens 显式的 token 阈值（0 = 由模型窗口 × TokenRatio 推导）。
	MaxTokens int
	// TokenRatio 占窗口的比例上限，超过即压缩。默认 0.75，留出输出额度。
	TokenRatio float64
}

func readCompactConfig(k *kernel.Kernel) compactConfig {
	c := compactConfig{Enabled: true, MaxChars: 24000, KeepTurns: 20, HardCap: 400, TokenRatio: 0.75}
	if k == nil {
		return c
	}
	cfg, ok := k.Options("session")
	if !ok || cfg == nil {
		return c
	}
	if v, ok := cfg["compact_enabled"].(bool); ok {
		c.Enabled = v
	}
	if f, ok := number(cfg["max_chars"]); ok {
		c.MaxChars = int(f)
	}
	if f, ok := number(cfg["keep_turns"]); ok {
		c.KeepTurns = int(f)
	}
	if f, ok := number(cfg["hard_cap"]); ok {
		c.HardCap = int(f)
	}
	if f, ok := number(cfg["max_tokens"]); ok && f > 0 {
		c.MaxTokens = int(f)
	}
	if f, ok := number(cfg["token_ratio"]); ok && f > 0 && f < 1 {
		c.TokenRatio = f
	}
	if c.MaxChars <= 0 {
		c.MaxChars = 24000
	}
	if c.KeepTurns < 2 {
		c.KeepTurns = 2
	}
	if c.HardCap <= c.KeepTurns {
		c.HardCap = c.KeepTurns * 20
	}
	return c
}

func number(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	}
	return 0, false
}

func messagesChars(msgs []api.Message) int {
	n := 0
	for _, m := range msgs {
		n += len(m.Content)
		for _, tc := range m.ToolCalls {
			n += len(tc.Args)
		}
	}
	return n
}

// needsCompact 三个条件任一成立即触发压缩：
//  1. token 占用超过窗口比例（或显式 max_tokens）——最贴近 provider 真实限制；
//  2. 字符数超过 max_chars——窗口未知时的兜底；
//  3. 消息条数超过 hard_cap——防止大量短消息把请求撑爆。
func needsCompact(msgs []api.Message, c compactConfig, window int) bool {
	if !c.Enabled || len(msgs) <= c.KeepTurns {
		return false
	}
	if limit := c.tokenLimit(window); limit > 0 && messagesTokens(msgs) > limit {
		return true
	}
	return messagesChars(msgs) > c.MaxChars || len(msgs) > c.HardCap
}

// tokenLimit 计算本次应触发的 token 上限。返回 0 表示无法按 token 判定。
func (c compactConfig) tokenLimit(window int) int {
	if c.MaxTokens > 0 {
		return c.MaxTokens
	}
	if window <= 0 {
		return 0
	}
	return int(float64(window) * c.TokenRatio)
}

// messagesTokens 与 llm 包同一口径的粗估。压缩判定发生在组包前，
// 用估算值即可，不必等 provider 回 usage。
func messagesTokens(msgs []api.Message) int { return api.EstimateTokensOf(msgs) }

// cutPoint 返回「保留区间」的起始下标：倒数第 KeepTurns 个 user 消息的位置。
// 找不到足够轮次时返回 0（不压缩），保证不切坏工具调用配对。
func cutPoint(msgs []api.Message, keepTurns int) int {
	users := 0
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role != api.RoleUser {
			continue
		}
		users++
		if users == keepTurns {
			return i
		}
	}
	return 0
}

// renderForSummary 把早期对话压成纯文本供摘要模型阅读。
// 只保留 user / assistant 正文与工具名，工具原始输出不进摘要请求
// （那正是要压掉的体积，且常含大段无关日志）。
func renderForSummary(msgs []api.Message, limit int) string {
	var b strings.Builder
	for _, m := range msgs {
		var line string
		switch m.Role {
		case api.RoleUser:
			line = "用户: " + m.Content
		case api.RoleAssistant:
			if len(m.ToolCalls) > 0 {
				names := make([]string, 0, len(m.ToolCalls))
				for _, tc := range m.ToolCalls {
					names = append(names, tc.Name)
				}
				line = "助手: " + m.Content + " [调用工具: " + strings.Join(names, ", ") + "]"
			} else {
				line = "助手: " + m.Content
			}
		default:
			continue
		}
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if r := []rune(line); len(r) > 800 {
			line = string(r[:800]) + "…"
		}
		b.WriteString(line)
		b.WriteByte('\n')
		if b.Len() >= limit {
			break
		}
	}
	s := b.String()
	if len(s) > limit {
		s = s[:limit]
	}
	return s
}

const summaryInstruction = `把下面这段较早的对话压缩成一份紧凑的上下文摘要，供后续对话继续使用时不丢失关键信息。

要求：
- 保留：用户目标与约束、已做的决定、改过的文件与路径、命令执行结论、遇到的错误与解决办法、尚未完成的待办。
- 丢弃：寒暄、重复表述、大段原始输出、工具参数细节。
- 用中文，条目化，只输出摘要本身，不要加开场白或解释。
- 控制在 800 字以内。

对话记录：
`

// summarize 调用模型生成早期对话摘要。失败时返回空串由上层降级为纯裁剪，
// 绝不让「压缩这一步」把正在进行的会话打断。
func summarize(ctx context.Context, model api.LLM, msgs []api.Message) string {
	if model == nil {
		return ""
	}
	text := renderForSummary(msgs, 12000)
	if strings.TrimSpace(text) == "" {
		return ""
	}
	out, err := model.Chat(ctx, []api.Message{
		{Role: api.RoleUser, Content: summaryInstruction + text},
	}, nil, api.StreamHandlers{})
	if err != nil || strings.TrimSpace(out.Content) == "" {
		return ""
	}
	return strings.TrimSpace(out.Content)
}

// compact 执行一次压缩，返回是否执行、压缩前/后消息数、以及摘要是否成功。
// 摘要失败不返回 error：降级为「保留最近 N 轮、不带摘要」继续跑。
func compact(ctx context.Context, sess api.Session, model, aux api.LLM, c compactConfig) (bool, int, int, bool) {
	msgs := sess.Messages()
	before := len(msgs)
	// 窗口来自当前主模型；未知（0）时判定自动退回字符阈值。
	window := 0
	if r, ok := model.(api.ContextWindowReporter); ok {
		window = r.ContextWindow()
	}
	if !needsCompact(msgs, c, window) {
		return false, before, before, false
	}
	cut := cutPoint(msgs, c.KeepTurns)
	if cut <= 0 {
		return false, before, before, false
	}
	// 摘要走辅助模型（没配时也就是主模型），不占用主模型的推理预算。
	summaryModel := aux
	if summaryModel == nil {
		summaryModel = model
	}
	summary := summarize(ctx, summaryModel, msgs[:cut])
	recent := append([]api.Message(nil), msgs[cut:]...)

	var out []api.Message
	if summary != "" {
		out = append(out, api.Message{
			Role:    api.RoleSystem,
			Content: "（较早对话的摘要，供延续上下文使用）\n" + summary,
		})
	}
	out = append(out, recent...)
	if len(out) >= before {
		return false, before, before, false // 压了等于没压，别做无意义的重写
	}
	if err := sess.Replace(out); err != nil {
		// 落盘失败只记日志，不阻断本轮对话。
		log.Printf("压缩落盘失败，本轮跳过压缩: %v", err)
		return false, before, before, false
	}
	if summary == "" {
		log.Printf("摘要生成失败，降级为纯裁剪: %d → %d 条", before, len(out))
	}
	return true, before, len(out), summary != ""
}

// compactNotice 生成给 UI 的提示文本。
func compactNotice(before, after int, summarized bool) string {
	mode := "纯裁剪"
	if summarized {
		mode = "摘要 + 保留最近轮次"
	}
	return fmt.Sprintf("上下文已压缩（%d → %d 条，%s）", before, after, mode)
}
