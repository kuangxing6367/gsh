// Package llmplugin 模型服务插件。
// provider=echo：离线 mock，回应一段演示文本 + 一次工具调用（自测链路用）。
// provider=openai：任意 OpenAI 兼容 API（DeepSeek / new-api / vLLM ...），支持流式。
package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"gsh/internal/api"
	"gsh/internal/kernel"
)

// RegisterKernel 挂载 LLM 插件。
func RegisterKernel(k *kernel.Kernel) error { return k.Load(&Plugin{}) }

// Plugin LLM 插件。
type Plugin struct{}

// Name 实现 kernel.Plugin。
func (p *Plugin) Name() string { return "llm" }

// Apply 按 provider 配置注册 llm 服务。
func (p *Plugin) Apply(ctx *kernel.Ctx) error {
	inner, err := buildLLM(ctx.Config())
	if err != nil {
		return err
	}
	// 包一层可替换实现：Web 设置面板改模型后不必重启进程。
	ctx.Provide("llm", &switchable{inner: inner})
	return nil
}

// buildLLM 根据配置构造 provider。
func buildLLM(cfg map[string]any) (api.LLM, error) {
	provider := "echo"
	if cfg != nil {
		if v, ok := cfg["provider"].(string); ok && v != "" {
			provider = strings.ToLower(v)
		}
	}
	switch provider {
	case "echo":
		return &echoLLM{}, nil
	case "openai":
		c := &openaiLLM{httpc: &http.Client{Timeout: 10 * time.Minute}}
		if cfg != nil {
			if v, ok := cfg["base_url"].(string); ok {
				c.baseURL = strings.TrimRight(v, "/")
			}
			if v, ok := cfg["api_key"].(string); ok {
				c.apiKey = v
			}
			if v, ok := cfg["model"].(string); ok {
				c.model = v
			}
			// 显式方言：默认 none，绝不猜测。
			if v, ok := cfg["thinking_format"].(string); ok {
				switch strings.ToLower(v) {
				case "deepseek", "reasoner":
					c.thinkingFormat = "deepseek"
				default:
					c.thinkingFormat = "none"
				}
			}
		}
		if c.baseURL == "" {
			c.baseURL = "https://api.deepseek.com"
		}
		if c.model == "" {
			c.model = "deepseek-chat"
		}
		return c, nil
	default:
		return nil, fmt.Errorf("llm: unknown provider %q (echo|openai)", provider)
	}
}

// switchable 可热替换的 LLM 代理。
// 读路径持读锁，Configure 持写锁整体换掉实现，正在跑的请求不受影响。
type switchable struct {
	mu    sync.RWMutex
	inner api.LLM
}

func (s *switchable) Chat(ctx context.Context, msgs []api.Message, tools []api.ToolSpec, onDelta api.StreamDelta) (api.Message, error) {
	s.mu.RLock()
	inner := s.inner
	s.mu.RUnlock()
	if inner == nil {
		return api.Message{}, fmt.Errorf("llm: not configured")
	}
	return inner.Chat(ctx, msgs, tools, onDelta)
}

// Configure 实现 api.Configurable。
func (s *switchable) Configure(cfg map[string]any) error {
	inner, err := buildLLM(cfg)
	if err != nil {
		return err
	}
	s.mu.Lock()
	s.inner = inner
	s.mu.Unlock()
	return nil
}

// ---------- echo provider ----------

// parseNodeDirective 从用户输入里解析 "node=<名字>" 指令（离线自检用）。
// 只认第一个空格前的 token，避免误伤正常文本。
func parseNodeDirective(text string) string {
	return parseDirective(text, "node=")
}

// parseCmdDirective 解析 "cmd=<命令>" 指令：指定 echo mock 实际要执行的命令。
// 这样离线自检也能触发真实工具行为（含安全策略），而不只是无害的 echo。
func parseCmdDirective(text string) string {
	return parseDirective(text, "cmd=")
}

// parseDirective 从文本中取出 "<key><value>" 形式的指令值。
// 规则：扫描所有 token，找到以 key 开头的那个；值 = 该 token 余下部分 +
// 后续 token（直到遇到另一个 "xxx=" 形式的指令）。
func parseDirective(text, key string) string {
	fields := strings.Fields(text)
	for i, f := range fields {
		if !strings.HasPrefix(f, key) {
			continue
		}
		parts := []string{strings.TrimPrefix(f, key)}
		// key= 后面的 token 属于值：拼接到下一个已知指令之前
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

type echoLLM struct{ turn int }

func (e *echoLLM) Chat(_ context.Context, msgs []api.Message, tools []api.ToolSpec, onDelta api.StreamDelta) (api.Message, error) {
	e.turn++
	var lastUser string
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == api.RoleUser {
			lastUser = msgs[i].Content
			break
		}
	}
	// 有用户消息且还没调用过工具 → 演示一次 bash 工具调用
	usedTool := false
	for _, m := range msgs {
		if m.Role == api.RoleTool {
			usedTool = true
			break
		}
	}
	if !usedTool && len(tools) > 0 {
		// 离线自检指令：node=<节点> 指定远程节点；cmd=<命令> 指定实际要跑的命令。
		// 两者都没给时，退化为无害的本地 echo 演示。
		node := parseNodeDirective(lastUser)
		custom := parseCmdDirective(lastUser)
		cmd := "echo gsh-works " + fmt.Sprint(e.turn)
		switch {
		case custom != "":
			cmd = custom
		case node != "":
			// 远程演示时附带 hostname/uname，便于确认命令确实跑在目标机器上
			cmd += "; hostname; uname -a"
		}
		args := map[string]any{"command": cmd}
		if node != "" {
			args["node"] = node
		}
		raw, _ := json.Marshal(args)
		return api.Message{
			Role:    api.RoleAssistant,
			Content: fmt.Sprintf("[echo] 收到: %q；先演示一次工具调用。", lastUser),
			ToolCalls: []api.ToolCall{{
				ID:   fmt.Sprintf("call_echo_%d", e.turn),
				Name: "bash",
				Args: string(raw),
			}},
		}, nil
	}
	text := fmt.Sprintf("[echo] 完成。用户输入: %q；本次会话共 %d 轮模型调用；工具结果已并入。", lastUser, e.turn)
	if onDelta != nil {
		onDelta(text)
	}
	return api.Message{Role: api.RoleAssistant, Content: text}, nil
}

// ---------- openai-compatible provider ----------

type openaiLLM struct {
	baseURL string
	apiKey  string
	model   string
	// thinkingFormat 显式声明推理字段格式，绝不靠 hostname 猜测（DSH 的坑：
	// 自建网关被误判方言导致 token 花销翻三倍）。
	// 取值："" / "none"（默认，标准 OpenAI 协议）；"deepseek"（reasoner 模型吐 reasoning_content）。
	thinkingFormat string
	httpc          *http.Client
}

type oaRequest struct {
	Model    string   `json:"model"`
	Messages []oaMsg  `json:"messages"`
	Tools    []oaTool `json:"tools,omitempty"`
	Stream   bool     `json:"stream"`
}

// oaMsg OpenAI 出站消息格式。
// 内部 api.Message 不能直接当 wire format：工具调用必须带
// {"type":"function","function":{"name","arguments"}} 这层包装，否则 422。
type oaMsg struct {
	Role       string       `json:"role"`
	Content    string       `json:"content,omitempty"`
	ToolCalls  []oaToolCall `json:"tool_calls,omitempty"`
	ToolCallID string       `json:"tool_call_id,omitempty"`
	Name       string       `json:"name,omitempty"`
}

type oaToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

// toWire 把内部消息转换为 OpenAI 出站格式。
func toWire(msgs []api.Message) []oaMsg {
	out := make([]oaMsg, 0, len(msgs))
	for _, m := range msgs {
		w := oaMsg{
			Role:       string(m.Role),
			Content:    m.Content,
			ToolCallID: m.ToolCallID,
			Name:       m.Name,
		}
		for _, tc := range m.ToolCalls {
			var c oaToolCall
			c.ID = tc.ID
			c.Type = "function"
			c.Function.Name = tc.Name
			c.Function.Arguments = tc.Args
			w.ToolCalls = append(w.ToolCalls, c)
		}
		// 助手消息带工具调用时，OpenAI 要求 content 可为空但字段存在性正常；
		// 这里保持省略（omitempty）即可。
		out = append(out, w)
	}
	return out
}

type oaTool struct {
	Type     string       `json:"type"`
	Function api.ToolSpec `json:"function"`
}

type oaResponse struct {
	Choices []struct {
		Message   oaMsg  `json:"message"`
		FinishRsn string `json:"finish_reason"`
	} `json:"choices"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

// fromWire 把 OpenAI 响应消息转回内部结构。
func fromWire(w oaMsg) api.Message {
	m := api.Message{
		Role:       api.Role(w.Role),
		Content:    w.Content,
		ToolCallID: w.ToolCallID,
		Name:       w.Name,
	}
	for _, tc := range w.ToolCalls {
		m.ToolCalls = append(m.ToolCalls, api.ToolCall{
			ID:   tc.ID,
			Name: tc.Function.Name,
			Args: tc.Function.Arguments,
		})
	}
	return m
}

type oaStreamChunk struct {
	Choices []struct {
		Delta struct {
			Content          string `json:"content"`
			ReasoningContent string `json:"reasoning_content"`
			// OpenAI 流式工具调用：function 是嵌套对象（不是扁平字段）。
			ToolCalls []struct {
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
	Error *struct {
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

func (o *openaiLLM) Chat(ctx context.Context, msgs []api.Message, tools []api.ToolSpec, onDelta api.StreamDelta) (api.Message, error) {
	reqBody := oaRequest{Model: o.model, Messages: toWire(msgs), Stream: true}
	for _, t := range tools {
		reqBody.Tools = append(reqBody.Tools, oaTool{Type: "function", Function: t})
	}
	payload, err := json.Marshal(reqBody)
	if err != nil {
		return api.Message{}, err
	}
	url := o.baseURL + "/chat/completions"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return api.Message{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	if o.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+o.apiKey)
	}
	resp, err := o.httpc.Do(req)
	if err != nil {
		return api.Message{}, fmt.Errorf("llm: request %s: %w", url, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return api.Message{}, fmt.Errorf("llm: http %d from %s: %s", resp.StatusCode, url, strings.TrimSpace(string(b)))
	}

	ct := resp.Header.Get("Content-Type")
	if strings.Contains(ct, "text/event-stream") {
		return o.readStream(resp.Body, onDelta)
	}
	// 非 SSE 回退
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
	msg := fromWire(or.Choices[0].Message)
	msg.Role = api.RoleAssistant
	if onDelta != nil && msg.Content != "" {
		onDelta(msg.Content)
	}
	return msg, nil
}

func (o *openaiLLM) readStream(r io.Reader, onDelta api.StreamDelta) (api.Message, error) {
	sc := newSSCanner(r)
	out := &bytes.Buffer{}
	reason := &bytes.Buffer{} // 推理内容（deepseek-reasoner 等）
	toolCalls := map[int]*api.ToolCall{}
	var order []int
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
			return api.Message{}, fmt.Errorf("llm: stream error: %s", chunk.Error.Message)
		}
		for _, c := range chunk.Choices {
			if c.Delta.Content != "" {
				out.WriteString(c.Delta.Content)
				if onDelta != nil {
					onDelta(c.Delta.Content)
				}
			}
			if c.Delta.ReasoningContent != "" {
				reason.WriteString(c.Delta.ReasoningContent)
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
		return api.Message{}, fmt.Errorf("llm: stream read: %w", err)
	}
	msg := api.Message{Role: api.RoleAssistant, Content: out.String()}
	// deepseek 显式方言：若最终 content 为空（模型只吐了推理），用推理内容兜底，
	// 避免 agent 拿到空回复卡死。
	if o.thinkingFormat == "deepseek" && msg.Content == "" && reason.Len() > 0 {
		if reason.Len() > 2000 {
			msg.Content = "[thinking]\n" + reason.String()[:2000]
		} else {
			msg.Content = "[thinking]\n" + reason.String()
		}
	}
	for _, idx := range order {
		tc := toolCalls[idx]
		if tc.ID == "" {
			tc.ID = fmt.Sprintf("call_idx_%d", idx)
		}
		msg.ToolCalls = append(msg.ToolCalls, *tc)
	}
	return msg, nil
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
