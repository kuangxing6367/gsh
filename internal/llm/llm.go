// Package llmplugin 模型服务插件。
// provider=echo：离线 mock，回应一段演示文本 + 一次工具调用（自测链路用）。
// provider=openai：任意 OpenAI 兼容 API（DeepSeek / new-api / vLLM ...），支持流式。
package llm

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"gsh/internal/api"
	"gsh/internal/kernel"
)

// RegisterKernel 挂载 LLM 插件。
func RegisterKernel(k *kernel.Kernel) error { return k.Load(&Plugin{}) }

// Plugin LLM 插件。
type Plugin struct{}

// Name 实现 kernel.Plugin。
func (p *Plugin) Name() string { return "llm" }

// Apply 按配置注册模型表：llm 是主模型代理，auxLLM 是摘要等辅助任务的代理。
// 两者都把「取哪个模型」推迟到每次调用，切换模型不必重启进程。
func (p *Plugin) Apply(ctx *kernel.Ctx) error {
	mgr, err := NewManager(ctx.Config())
	if err != nil {
		return err
	}
	ctx.Provide("llm", mgr.CurrentProxy())
	ctx.Provide("auxLLM", mgr.AuxProxy())
	ctx.Provide("modelManager", mgr)
	return nil
}

// ---------- 配置解析 ----------

// Params 采样与输出参数。指针语义区分「没配」与「显式配 0」：
// 没配就不往请求体里塞字段，让 provider 用自己的默认值。
type Params struct {
	Temperature      *float64 `json:"temperature,omitempty"`
	TopP             *float64 `json:"top_p,omitempty"`
	MaxTokens        *int     `json:"max_tokens,omitempty"`
	PresencePenalty  *float64 `json:"presence_penalty,omitempty"`
	FrequencyPenalty *float64 `json:"frequency_penalty,omitempty"`
	Stop             []string `json:"stop,omitempty"`
}

// Thinking 深度思考请求注入配置。
// style 取值：none / auto / openai / anthropic / flag。
// 不同网关认的字段完全不同，猜错要么被忽略要么直接 400，所以必须由配置显式选择。
type Thinking struct {
	Enabled      bool   `json:"enabled"`
	Style        string `json:"style"`
	Effort       string `json:"effort"`
	BudgetTokens int    `json:"budget_tokens"`
}

// Retry 重试策略，仅覆盖 429 / 5xx / 网络错误；4xx 参数错误不重试。
type Retry struct {
	Max    int `json:"max"`
	BaseMS int `json:"base_ms"`
}

// Config LLM 插件配置展平后的形态。
type Config struct {
	Provider       string
	BaseURL        string
	APIKey         string
	Model          string
	ThinkingFormat string
	TimeoutSeconds int
	Params         Params
	Thinking       Thinking
	Retry          Retry
	// ContextWindow 模型上下文窗口（token）。0 表示未知——此时压缩判定
	// 退回字符阈值，绝不按模型名猜窗口大小。
	ContextWindow int
}

// defaultConfig 给出保守默认值：不擅自开启思考、不改采样、输出上限 4096。
func defaultConfig() Config {
	return Config{
		Provider:       "openai",
		BaseURL:        "https://api.deepseek.com",
		Model:          "deepseek-chat",
		ThinkingFormat: "none",
		TimeoutSeconds: 120,
		Retry:          Retry{Max: 2, BaseMS: 800},
	}
}

func num(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case float32:
		return float64(n), true
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	case json.Number:
		f, err := n.Float64()
		return f, err == nil
	case string:
		f, err := strconv.ParseFloat(n, 64)
		return f, err == nil
	}
	return 0, false
}

func floatPtr(v any) *float64 {
	if f, ok := num(v); ok {
		return &f
	}
	return nil
}

func intPtr(v any) *int {
	if f, ok := num(v); ok {
		n := int(f)
		return &n
	}
	return nil
}

// parseConfig 把 map 配置展平成强类型结构。所有字段都可缺省。
func parseConfig(cfg map[string]any) Config {
	c := defaultConfig()
	if cfg == nil {
		return c
	}
	if v, ok := cfg["provider"].(string); ok && v != "" {
		c.Provider = strings.ToLower(v)
	}
	if v, ok := cfg["base_url"].(string); ok && v != "" {
		c.BaseURL = strings.TrimRight(v, "/")
	}
	if v, ok := cfg["model"].(string); ok && v != "" {
		c.Model = v
	}
	// Key 支持 env: 前缀引用环境变量，避免明文落盘。
	if v, ok := cfg["api_key"].(string); ok && v != "" {
		if strings.HasPrefix(v, "env:") {
			c.APIKey = os.Getenv(strings.TrimPrefix(v, "env:"))
		} else {
			c.APIKey = v
		}
	}
	if v, ok := cfg["thinking_format"].(string); ok {
		switch strings.ToLower(v) {
		case "deepseek", "reasoner":
			c.ThinkingFormat = "deepseek"
		default:
			c.ThinkingFormat = "none"
		}
	}
	if v, ok := num(cfg["timeout_seconds"]); ok && v > 0 {
		c.TimeoutSeconds = int(v)
	}
	if v, ok := num(cfg["context_window"]); ok && v > 0 {
		c.ContextWindow = int(v)
	}
	if p, ok := cfg["params"].(map[string]any); ok {
		c.Params.Temperature = floatPtr(p["temperature"])
		c.Params.TopP = floatPtr(p["top_p"])
		c.Params.MaxTokens = intPtr(p["max_tokens"])
		c.Params.PresencePenalty = floatPtr(p["presence_penalty"])
		c.Params.FrequencyPenalty = floatPtr(p["frequency_penalty"])
		if s, ok := p["stop"].([]any); ok {
			for _, it := range s {
				if str, ok := it.(string); ok {
					c.Params.Stop = append(c.Params.Stop, str)
				}
			}
		} else if s, ok := p["stop"].([]string); ok {
			c.Params.Stop = s
		}
	}
	if t, ok := cfg["thinking"].(map[string]any); ok {
		c.Thinking.Enabled, _ = t["enabled"].(bool)
		c.Thinking.Style, _ = t["style"].(string)
		c.Thinking.Effort, _ = t["effort"].(string)
		if f, ok := num(t["budget_tokens"]); ok {
			c.Thinking.BudgetTokens = int(f)
		}
	}
	if r, ok := cfg["retry"].(map[string]any); ok {
		if f, ok := num(r["max"]); ok {
			c.Retry.Max = int(f)
		}
		if f, ok := num(r["base_ms"]); ok {
			c.Retry.BaseMS = int(f)
		}
	}
	if c.Retry.Max < 0 {
		c.Retry.Max = 0
	}
	if c.Retry.BaseMS <= 0 {
		c.Retry.BaseMS = 800
	}
	return c
}

// buildLLM 根据配置构造 provider。
func buildLLM(cfg map[string]any) (api.LLM, error) {
	c := parseConfig(cfg)
	switch c.Provider {
	case "echo":
		return &echoLLM{}, nil
	case "openai":
		return newOpenAI(c), nil
	default:
		return nil, fmt.Errorf("llm: unknown provider %q (echo|openai)", c.Provider)
	}
}

// ---------- echo provider ----------

func parseNodeDirective(text string) string { return parseDirective(text, "node=") }
func parseCmdDirective(text string) string  { return parseDirective(text, "cmd=") }

// parseDirective 从文本中取出 "<key><value>" 形式的指令值（离线自检用）。
func parseDirective(text, key string) string {
	fields := strings.Fields(text)
	for i, f := range fields {
		if !strings.HasPrefix(f, key) {
			continue
		}
		parts := []string{strings.TrimPrefix(f, key)}
		for _, g := range fields[i+1:] {
			if strings.HasPrefix(g, "node=") || strings.HasPrefix(g, "cmd=") {
				break
			}
			parts = append(parts, g)
		}
		return strings.Join(parts, " ")
	}
	return ""
}

type echoLLM struct {
	turn int
	mu   sync.Mutex
}

func (e *echoLLM) Chat(_ context.Context, msgs []api.Message, tools []api.ToolSpec, h api.StreamHandlers) (api.Message, error) {
	e.mu.Lock()
	e.turn++
	turn := e.turn
	e.mu.Unlock()

	var lastUser string
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == api.RoleUser {
			lastUser = msgs[i].Content
			break
		}
	}
	// 思考通道单独走一路，验证「推理不进正文、不落会话」。
	if h.OnReason != nil {
		h.OnReason("[echo] 检查工具集与工作区…")
	}
	usedTool := false
	for _, m := range msgs {
		if m.Role == api.RoleTool {
			usedTool = true
			break
		}
	}
	if !usedTool && len(tools) > 0 {
		node := parseNodeDirective(lastUser)
		custom := parseCmdDirective(lastUser)
		cmd := "echo gsh-works " + fmt.Sprint(turn)
		switch {
		case custom != "":
			cmd = custom
		case node != "":
			cmd += "; hostname; uname -a"
		}
		args := map[string]any{"command": cmd}
		if node != "" {
			args["node"] = node
		}
		raw, _ := json.Marshal(args)
		return api.Message{
			Role:      api.RoleAssistant,
			Content:   fmt.Sprintf("[echo] 收到: %q；先演示一次工具调用。", lastUser),
			Reasoning: "[echo] 检查工具集与工作区…",
			ToolCalls: []api.ToolCall{{
				ID:   fmt.Sprintf("call_echo_%d", turn),
				Name: "bash",
				Args: string(raw),
			}},
		}, nil
	}
	text := fmt.Sprintf("[echo] 完成。用户输入: %q；本次会话共 %d 轮模型调用；工具结果已并入。", lastUser, turn)
	if h.OnText != nil {
		h.OnText(text)
	}
	return api.Message{Role: api.RoleAssistant, Content: text}, nil
}

// ContextWindow echo 无真实模型，窗口未知。
func (e *echoLLM) ContextWindow() int { return 0 }

// EstimateTokens 实现 api.TokenEstimator。
func (e *echoLLM) EstimateTokens(msgs []api.Message) int { return EstimateMessages(msgs) }

func (e *echoLLM) LastUsage() api.Usage {
	in, out := 0, utf8.RuneCountInString("[echo] 完成")
	return api.Usage{PromptTokens: in, CompletionTokens: out, TotalTokens: in + out, Estimated: true}
}

// ---------- openai-compatible provider ----------

type openaiLLM struct {
	cfg    Config
	httpc  *http.Client
	mu     sync.Mutex
	last   api.Usage
	reason bytes.Buffer // 本轮累计思考内容，仅用于诊断输出
}

func newOpenAI(c Config) *openaiLLM {
	return &openaiLLM{
		cfg:   c,
		httpc: &http.Client{Timeout: time.Duration(c.TimeoutSeconds) * time.Second},
	}
}

func (o *openaiLLM) LastUsage() api.Usage {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.last
}

// ContextWindow 实现 api.ContextWindowReporter。
func (o *openaiLLM) ContextWindow() int { return o.cfg.ContextWindow }

// EstimateTokens 实现 api.TokenEstimator。
func (o *openaiLLM) EstimateTokens(msgs []api.Message) int { return EstimateMessages(msgs) }

func (o *openaiLLM) setUsage(u api.Usage) {
	o.mu.Lock()
	o.last = u
	o.mu.Unlock()
}

// oaRequest 出站请求体。参数全用指针 + omitempty：没配就不发，
// 避免用自以为是的默认值覆盖 provider 侧的模型配置。
type oaRequest struct {
	Model            string      `json:"model"`
	Messages         []oaMsg     `json:"messages"`
	Tools            []oaTool    `json:"tools,omitempty"`
	Stream           bool        `json:"stream"`
	Temperature      *float64    `json:"temperature,omitempty"`
	TopP             *float64    `json:"top_p,omitempty"`
	MaxTokens        *int        `json:"max_tokens,omitempty"`
	PresencePenalty  *float64    `json:"presence_penalty,omitempty"`
	FrequencyPenalty *float64    `json:"frequency_penalty,omitempty"`
	Stop             []string    `json:"stop,omitempty"`
	ReasoningEffort  string      `json:"reasoning_effort,omitempty"`
	Thinking         *oaThinking `json:"thinking,omitempty"`
}

type oaThinking struct {
	Type         string `json:"type"`
	BudgetTokens int    `json:"budget_tokens,omitempty"`
}

// applyParams 按配置把采样参数与思考注入写进请求体。
func (o *openaiLLM) applyParams(r *oaRequest) {
	r.Temperature = o.cfg.Params.Temperature
	r.TopP = o.cfg.Params.TopP
	r.MaxTokens = o.cfg.Params.MaxTokens
	r.PresencePenalty = o.cfg.Params.PresencePenalty
	r.FrequencyPenalty = o.cfg.Params.FrequencyPenalty
	r.Stop = o.cfg.Params.Stop
	if !o.cfg.Thinking.Enabled {
		return
	}
	style := strings.ToLower(o.cfg.Thinking.Style)
	if style == "" || style == "auto" {
		style = inferThinkingStyle(o.cfg.Model)
	}
	switch style {
	case "openai":
		r.ReasoningEffort = o.cfg.Thinking.Effort
		if r.ReasoningEffort == "" {
			r.ReasoningEffort = "medium"
		}
	case "anthropic":
		r.Thinking = &oaThinking{Type: "enabled", BudgetTokens: o.cfg.Thinking.BudgetTokens}
	case "flag":
		r.Thinking = &oaThinking{Type: "enabled"}
	}
}

// inferThinkingStyle 在 auto 下按模型名选注入方言。只做保守推断，
// 判断不了就退回 openai 的 reasoning_effort（多数网关会忽略未知字段）。
func inferThinkingStyle(model string) string {
	m := strings.ToLower(model)
	switch {
	case strings.Contains(m, "claude"):
		return "anthropic"
	case strings.Contains(m, "reasoner") || strings.Contains(m, "r1") || strings.Contains(m, "qwq"):
		return "flag"
	default:
		return "openai"
	}
}

// oaMsg OpenAI 出站消息格式。
// 内部 api.Message 不能直接当 wire format：工具调用必须带
// {"type":"function","function":{"name","arguments"}} 这层包装，否则 422。
type oaMsg struct {
	Role string `json:"role"`
	// Content 是 string 或 []oaPart：纯文本走字符串（兼容性最好），
	// 带图片时才是数组——部分网关不接受数组形式的纯文本。
	Content    any          `json:"content,omitempty"`
	ToolCalls  []oaToolCall `json:"tool_calls,omitempty"`
	ToolCallID string       `json:"tool_call_id,omitempty"`
	Name       string       `json:"name,omitempty"`
}

// oaPart 是数组形式 content 里的一段。
type oaPart struct {
	Type     string `json:"type"`
	Text     string `json:"text,omitempty"`
	ImageURL *struct {
		URL string `json:"url"`
	} `json:"image_url,omitempty"`
}

type oaToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

func toWire(msgs []api.Message) []oaMsg {
	out := make([]oaMsg, 0, len(msgs))
	for _, m := range msgs {
		w := oaMsg{
			Role:       string(m.Role),
			ToolCallID: m.ToolCallID,
			Name:       m.Name,
		}
		// 有附件时才升级成数组 content；纯文本保持字符串。
		parts, text := toWireParts(m)
		if len(parts) > 0 {
			w.Content = parts
		} else if text != "" {
			w.Content = text
		}
		for _, tc := range m.ToolCalls {
			var c oaToolCall
			c.ID = tc.ID
			c.Type = "function"
			c.Function.Name = tc.Name
			c.Function.Arguments = tc.Args
			w.ToolCalls = append(w.ToolCalls, c)
		}
		out = append(out, w)
	}
	return out
}

// toWireParts 把文本 + 附件拼成 content 数组，同时返回可能被补注过的文本。
// 单个附件读取失败不阻断整轮请求——读不到就当没这张图，并在文本里留一句提示，
// 否则用户会因为一张图路径写错而完全发不出消息。
// 所有附件都失败时返回空 parts，让调用方退回字符串 content：
// 只含一段文本的数组虽然合法，但兼容性不如字符串。
func toWireParts(m api.Message) ([]oaPart, string) {
	if len(m.Parts) == 0 {
		return nil, m.Content
	}
	parts := make([]oaPart, 0, len(m.Parts)+1)
	text := m.Content
	for _, p := range m.Parts {
		url, err := dataURL(p)
		if err != nil {
			text += fmt.Sprintf("\n（附件 %s 未能读取：%v）", p.Path, err)
			continue
		}
		part := oaPart{Type: "image_url"}
		part.ImageURL = &struct {
			URL string `json:"url"`
		}{URL: url}
		parts = append(parts, part)
	}
	if len(parts) == 0 {
		return nil, text
	}
	if text != "" {
		parts = append([]oaPart{{Type: "text", Text: text}}, parts...)
	}
	return parts, text
}

// dataURL 把本地图片读成 data: URL。超过上限直接拒绝：
// 大多数模型对单图有硬限制，硬塞进去只会换来一个 400。
//
// 结果走缓存：历史消息里存的是路径，同一张图在后续每一轮出站请求中
// 都会被重新展开一次。没有缓存时，一张 8MB 的图会变成 11MB 的字符串，
// 每轮重新分配一遍——十轮对话就是一百多 MB 的纯浪费。
func dataURL(p api.ContentPart) (string, error) {
	const maxImageBytes = 8 << 20
	info, err := os.Stat(p.Path)
	if err != nil {
		return "", err
	}
	if info.Size() > maxImageBytes {
		return "", fmt.Errorf("图片过大（%d KB，上限 8192 KB）", info.Size()/1024)
	}
	mt := p.MediaType
	if mt == "" {
		mt = imageMime(p.Path)
	}
	// 用尺寸与 mtime 作指纹：文件被同名覆盖时缓存自然失效，不需要监听变更。
	key := fmt.Sprintf("%s|%d|%d|%s", p.Path, info.Size(), info.ModTime().UnixNano(), mt)
	if s, ok := imgCache.get(key); ok {
		return s, nil
	}
	data, err := os.ReadFile(p.Path)
	if err != nil {
		return "", err
	}
	url := "data:" + mt + ";base64," + base64.StdEncoding.EncodeToString(data)
	imgCache.put(key, url)
	return url, nil
}

// imgCacheBytes 是图片缓存的上限。超出后按写入顺序淘汰最旧的：
// 一轮对话通常只挂一两张图，64MB 足够覆盖「同一张图反复用」这个场景。
const imgCacheBytes = 64 << 20

// imgCache 是给 data URL 用的极小缓存。不做 LRU——按插入顺序淘汰已经够用，
// 而且省掉一次「每次读都挪位置」的写锁。
var imgCache = &imageCache{items: map[string]*imageEntry{}}

type imageEntry struct {
	url string
	n   int64
}

type imageCache struct {
	mu    sync.Mutex
	items map[string]*imageEntry
	order []string
	bytes int64
}

func (c *imageCache) get(key string) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.items[key]
	if !ok {
		return "", false
	}
	return e.url, true
}

func (c *imageCache) put(key, url string) {
	// 单张就超过总预算时不入缓存：塞进去只会立刻把别的全挤掉。
	if int64(len(url)) > imgCacheBytes/2 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, exists := c.items[key]; exists {
		return
	}
	c.items[key] = &imageEntry{url: url, n: int64(len(url))}
	c.order = append(c.order, key)
	c.bytes += int64(len(url))
	for c.bytes > imgCacheBytes && len(c.order) > 0 {
		old := c.order[0]
		c.order = c.order[1:]
		if e, ok := c.items[old]; ok {
			c.bytes -= e.n
			delete(c.items, old)
		}
	}
}

// imageMime 按扩展名推断 MIME。
func imageMime(path string) string {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".gif":
		return "image/gif"
	case ".webp":
		return "image/webp"
	case ".bmp":
		return "image/bmp"
	default:
		return "image/png"
	}
}

type oaTool struct {
	Type     string       `json:"type"`
	Function api.ToolSpec `json:"function"`
}

type oaUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

type oaResponse struct {
	Choices []struct {
		Message   oaMsg  `json:"message"`
		FinishRsn string `json:"finish_reason"`
	} `json:"choices"`
	Usage *oaUsage `json:"usage,omitempty"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

func fromWire(w oaMsg) api.Message {
	m := api.Message{
		Role:       api.Role(w.Role),
		Content:    contentText(w.Content),
		ToolCallID: w.ToolCallID,
		Name:       w.Name,
	}
	for _, tc := range w.ToolCalls {
		m.ToolCalls = append(m.ToolCalls, api.ToolCall{
			ID: tc.ID, Name: tc.Function.Name, Args: tc.Function.Arguments,
		})
	}
	return m
}

// contentText 把 content 字段还原成字符串。
// 响应方向极少出现数组（部分网关会回 content:[{type:text}]），两种都要认。
func contentText(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case []any:
		var b strings.Builder
		for _, item := range t {
			if m, ok := item.(map[string]any); ok {
				if s, ok := m["text"].(string); ok {
					b.WriteString(s)
				}
			}
		}
		return b.String()
	}
	return ""
}

// oaStreamChunk 推理字段各家不统一，四种常见命名全收；
// 同一 chunk 只取第一个非空，避免重复累计。
type oaStreamChunk struct {
	Choices []struct {
		Delta struct {
			Content          string `json:"content"`
			ReasoningContent string `json:"reasoning_content"`
			Reasoning        string `json:"reasoning"`
			Thinking         string `json:"thinking"`
			ReasoningDelta   string `json:"reasoning_delta"`
			ToolCalls        []struct {
				Index    int    `json:"index"`
				ID       string `json:"id"`
				Type     string `json:"type"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"delta"`
		FinishRsn string `json:"finish_reason"`
	} `json:"choices"`
	Usage *oaUsage `json:"usage,omitempty"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

func firstNonEmpty(ss ...string) string {
	for _, s := range ss {
		if s != "" {
			return s
		}
	}
	return ""
}

// Chat 发起一轮流式补全。思考增量走 OnReason，正文增量走 OnText，两者不混。
func (o *openaiLLM) Chat(ctx context.Context, msgs []api.Message, tools []api.ToolSpec, h api.StreamHandlers) (api.Message, error) {
	reqBody := oaRequest{Model: o.cfg.Model, Messages: toWire(msgs), Stream: true}
	for _, t := range tools {
		reqBody.Tools = append(reqBody.Tools, oaTool{Type: "function", Function: t})
	}
	o.applyParams(&reqBody)
	payload, err := json.Marshal(reqBody)
	if err != nil {
		return api.Message{}, err
	}

	url := o.cfg.BaseURL + "/chat/completions"
	resp, err := o.doWithRetry(ctx, url, payload)
	if err != nil {
		return api.Message{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return api.Message{}, fmt.Errorf("llm: http %d from %s: %s", resp.StatusCode, url, strings.TrimSpace(string(b)))
	}

	var msg api.Message
	var reported *oaUsage
	if strings.Contains(resp.Header.Get("Content-Type"), "text/event-stream") {
		msg, reported = o.readStream(resp.Body, h)
	} else {
		var or oaResponse
		if err := json.NewDecoder(resp.Body).Decode(&or); err != nil {
			return api.Message{}, fmt.Errorf("llm: decode response: %w", err)
		}
		if or.Error != nil {
			return api.Message{}, fmt.Errorf("llm: api error: %s", or.Error.Message)
		}
		if len(or.Choices) == 0 {
			return api.Message{}, errors.New("llm: empty choices")
		}
		msg = fromWire(or.Choices[0].Message)
		msg.Role = api.RoleAssistant
		if h.OnText != nil && msg.Content != "" {
			h.OnText(msg.Content)
		}
		reported = or.Usage
	}
	// 模型只吐思考不给正文时，不能静默返回空回复让上层卡住。
	// 思考原文留在 Reasoning 里（不落盘），正文给一句明确说明。
	if msg.Content == "" && msg.Reasoning != "" && len(msg.ToolCalls) == 0 {
		msg.Content = "（模型本轮只产生了思考内容，未返回正式回复）"
		if h.OnText != nil {
			h.OnText(msg.Content)
		}
	}
	o.setUsage(resolveUsage(msgs, msg, reported))
	return msg, nil
}

// doWithRetry 只对 429 / 5xx / 网络错误退避重试；每次用全新 Reader 重建请求。
func (o *openaiLLM) doWithRetry(ctx context.Context, url string, payload []byte) (*http.Response, error) {
	var lastErr error
	for attempt := 0; attempt <= o.cfg.Retry.Max; attempt++ {
		if attempt > 0 {
			wait := time.Duration(float64(o.cfg.Retry.BaseMS)*math.Pow(2, float64(attempt-1))) * time.Millisecond
			if wait > 8*time.Second {
				wait = 8 * time.Second
			}
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(wait):
			}
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", "application/json")
		if o.cfg.APIKey != "" {
			req.Header.Set("Authorization", "Bearer "+o.cfg.APIKey)
		}
		resp, err := o.httpc.Do(req)
		if err == nil {
			retryable := resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500
			if !retryable {
				return resp, nil
			}
			lastErr = fmt.Errorf("llm: http %d from %s", resp.StatusCode, url)
			// 尊重 Retry-After（秒），但不覆盖 ctx 取消。
			if ra := resp.Header.Get("Retry-After"); ra != "" {
				if sec, e := strconv.Atoi(ra); e == nil && sec > 0 && sec <= 30 {
					select {
					case <-ctx.Done():
						resp.Body.Close()
						return nil, ctx.Err()
					case <-time.After(time.Duration(sec) * time.Second):
					}
				}
			}
			resp.Body.Close()
			continue
		}
		lastErr = fmt.Errorf("llm: request %s: %w", url, err)
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
	}
	return nil, fmt.Errorf("%v（已重试 %d 次）", lastErr, o.cfg.Retry.Max)
}

// estimateTokens 中英混排粗估（与 api 包同一口径）。
func estimateTokens(s string) int {
	if s == "" {
		return 0
	}
	return api.EstimateTokensOf([]api.Message{{Content: s}})
}

// EstimateMessages 估算一组消息的 token 数，供压缩判定与缺失 usage 兜底。
func EstimateMessages(msgs []api.Message) int { return api.EstimateTokensOf(msgs) }

// resolveUsage 优先用 provider 上报值；缺失时本地估算并标记 Estimated。
func resolveUsage(msgs []api.Message, msg api.Message, reported *oaUsage) api.Usage {
	if reported != nil && (reported.TotalTokens > 0 || reported.PromptTokens > 0) {
		total := reported.TotalTokens
		if total == 0 {
			total = reported.PromptTokens + reported.CompletionTokens
		}
		return api.Usage{
			PromptTokens:     reported.PromptTokens,
			CompletionTokens: reported.CompletionTokens,
			TotalTokens:      total,
		}
	}
	in := EstimateMessages(msgs)
	out := estimateTokens(msg.Content) + estimateTokens(msg.Reasoning)
	for _, tc := range msg.ToolCalls {
		out += estimateTokens(tc.Args)
	}
	return api.Usage{PromptTokens: in, CompletionTokens: out, TotalTokens: in + out, Estimated: true}
}

func (o *openaiLLM) readStream(r io.Reader, h api.StreamHandlers) (api.Message, *oaUsage) {
	sc := newSSCanner(r)
	out := &bytes.Buffer{}
	reason := &bytes.Buffer{}
	toolCalls := map[int]*api.ToolCall{}
	var order []int
	var usage *oaUsage
	for sc.Next() {
		line := sc.Line()
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "[DONE]" {
			break
		}
		var chunk oaStreamChunk
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			continue // 容忍单行坏块
		}
		if chunk.Error != nil {
			// 流中报错无法返回 error（签名已定），写进正文让上层可见。
			out.WriteString("\n[llm stream error] " + chunk.Error.Message)
			continue
		}
		if chunk.Usage != nil {
			usage = chunk.Usage
		}
		for _, c := range chunk.Choices {
			if c.Delta.Content != "" {
				out.WriteString(c.Delta.Content)
				if h.OnText != nil {
					h.OnText(c.Delta.Content)
				}
			}
			if rr := firstNonEmpty(c.Delta.ReasoningContent, c.Delta.Reasoning, c.Delta.Thinking, c.Delta.ReasoningDelta); rr != "" {
				reason.WriteString(rr)
				if h.OnReason != nil {
					h.OnReason(rr)
				}
			}
			for _, tc := range c.Delta.ToolCalls {
				cur, ok := toolCalls[tc.Index]
				if !ok {
					cur = &api.ToolCall{}
					toolCalls[tc.Index] = cur
					order = append(order, tc.Index)
				}
				if tc.ID != "" {
					cur.ID = tc.ID
				}
				if tc.Function.Name != "" {
					cur.Name = tc.Function.Name
				}
				cur.Args += tc.Function.Arguments
			}
		}
	}
	if err := sc.Err(); err != nil {
		out.WriteString("\n[llm stream read error] " + err.Error())
	}
	msg := api.Message{Role: api.RoleAssistant, Content: out.String(), Reasoning: reason.String()}
	for _, idx := range order {
		tc := toolCalls[idx]
		if tc.ID == "" {
			tc.ID = fmt.Sprintf("call_idx_%d", idx)
		}
		msg.ToolCalls = append(msg.ToolCalls, *tc)
	}
	return msg, usage
}

// newSSCanner 按 SSE 行扫描。
func newSSCanner(r io.Reader) *sseScanner { return &sseScanner{r: r} }

type sseScanner struct {
	r    io.Reader
	buf  []byte
	line []byte
	done bool
	err  error
}

func (s *sseScanner) Next() bool {
	if s.done {
		return false
	}
	s.line = s.line[:0]
	for {
		i := bytes.IndexByte(s.buf, '\n')
		if i >= 0 {
			s.line = append(s.line, s.buf[:i]...)
			s.buf = s.buf[i+1:]
			return true
		}
		tmp := make([]byte, 4096)
		n, err := s.r.Read(tmp)
		if n > 0 {
			s.buf = append(s.buf, tmp[:n]...)
			continue
		}
		if err != nil {
			s.done = true
			if err != io.EOF {
				s.err = err
			}
			return len(s.line) > 0
		}
	}
}

func (s *sseScanner) Line() string { return strings.TrimRight(string(s.line), "\r") }

func (s *sseScanner) Err() error { return s.err }
