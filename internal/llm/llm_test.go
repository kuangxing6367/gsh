package llm

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"gsh/internal/api"
)

func TestParseConfig(t *testing.T) {
	t.Setenv("GSH_TEST_KEY", "sk-from-env")
	c := parseConfig(map[string]any{
		"api_key":         "env:GSH_TEST_KEY",
		"model":           "some-model",
		"timeout_seconds": 30,
		"params": map[string]any{
			"temperature": 0.2,
			"max_tokens":  1024,
			"stop":        []any{"END"},
		},
		"thinking": map[string]any{"enabled": true, "style": "openai", "effort": "high"},
		"retry":    map[string]any{"max": 4, "base_ms": 100},
	})
	if c.APIKey != "sk-from-env" {
		t.Fatalf("env: 前缀未生效: %q", c.APIKey)
	}
	if c.TimeoutSeconds != 30 || c.Retry.Max != 4 || c.Retry.BaseMS != 100 {
		t.Fatalf("超时/重试解析错误: %+v", c)
	}
	if c.Params.Temperature == nil || *c.Params.Temperature != 0.2 {
		t.Fatal("temperature 未解析")
	}
	if c.Params.MaxTokens == nil || *c.Params.MaxTokens != 1024 {
		t.Fatal("max_tokens 未解析")
	}
	if len(c.Params.Stop) != 1 || c.Params.Stop[0] != "END" {
		t.Fatalf("stop 未解析: %v", c.Params.Stop)
	}
	if !c.Thinking.Enabled || c.Thinking.Effort != "high" {
		t.Fatal("thinking 未解析")
	}
}

func TestParamsOmittedWhenNotConfigured(t *testing.T) {
	c := defaultConfig()
	o := newOpenAI(c)
	r := oaRequest{Model: "m"}
	o.applyParams(&r)
	b, _ := json.Marshal(r)
	for _, forbidden := range []string{"temperature", "max_tokens", "thinking", "reasoning_effort"} {
		if strings.Contains(string(b), forbidden) {
			t.Fatalf("未配置的参数不该出现在请求体: %s", forbidden)
		}
	}
}

func TestThinkingStyles(t *testing.T) {
	cases := []struct {
		style string
		want  string
	}{
		{"openai", `"reasoning_effort":"medium"`},
		{"anthropic", `"thinking":{"type":"enabled"`},
		{"flag", `"thinking":{"type":"enabled"}`},
	}
	for _, tc := range cases {
		c := defaultConfig()
		c.Thinking = Thinking{Enabled: true, Style: tc.style}
		o := newOpenAI(c)
		r := oaRequest{Model: "m"}
		o.applyParams(&r)
		b, _ := json.Marshal(r)
		if !strings.Contains(string(b), tc.want) {
			t.Fatalf("style=%s 期望含 %s，实际 %s", tc.style, tc.want, b)
		}
	}
	c := defaultConfig()
	c.Thinking = Thinking{Enabled: true, Style: "auto"}
	c.Model = "deepseek-reasoner"
	o := newOpenAI(c)
	r := oaRequest{Model: c.Model}
	o.applyParams(&r)
	b, _ := json.Marshal(r)
	if !strings.Contains(string(b), `"thinking":{"type":"enabled"}`) {
		t.Fatalf("auto 应按模型名推断方言: %s", b)
	}
}

func sseBody(lines ...string) string {
	var b strings.Builder
	for _, l := range lines {
		b.WriteString("data: " + l + "\n\n")
	}
	b.WriteString("data: [DONE]\n\n")
	return b.String()
}

func TestReadStreamSeparatesReasoningFromContent(t *testing.T) {
	o := newOpenAI(defaultConfig())
	var reasons, texts strings.Builder
	h := api.StreamHandlers{
		OnText:   func(s string) { texts.WriteString(s) },
		OnReason: func(s string) { reasons.WriteString(s) },
	}
	body := sseBody(
		`{"choices":[{"delta":{"content":"正文A"}}]}`,
		`{"choices":[{"delta":{"reasoning_content":"想A"}}]}`,
		`{"choices":[{"delta":{"reasoning":"想B"}}]}`,
		`{"choices":[{"delta":{"thinking":"想C"}}]}`,
		`{"choices":[{"delta":{"reasoning_delta":"想D"}}]}`,
		`{"choices":[{"delta":{"content":"正文B"}}]}`,
	)
	msg, _ := o.readStream(strings.NewReader(body), h)
	if msg.Content != "正文A正文B" {
		t.Fatalf("正文被思考内容污染: %q", msg.Content)
	}
	if msg.Reasoning != "想A想B想C想D" || reasons.String() != msg.Reasoning {
		t.Fatalf("四种推理字段未完整上抛: msg=%q callback=%q", msg.Reasoning, reasons.String())
	}
	if texts.String() != msg.Content {
		t.Fatal("正文回调与消息内容不一致")
	}
}

func TestChatSendsSystemPromptAndParams(t *testing.T) {
	var captured map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &captured)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, sseBody(
			`{"choices":[{"delta":{"content":"回答"}}]}`,
			`{"choices":[{"delta":{}}],"usage":{"prompt_tokens":42,"completion_tokens":7,"total_tokens":49}}`,
		))
	}))
	defer srv.Close()

	c := defaultConfig()
	c.BaseURL = srv.URL
	c.Params.Temperature = func() *float64 { v := 0.3; return &v }()
	c.Params.MaxTokens = func() *int { v := 2048; return &v }()
	o := newOpenAI(c)
	msg, err := o.Chat(context.Background(), []api.Message{
		{Role: api.RoleSystem, Content: "你是 gsh"},
		{Role: api.RoleUser, Content: "你好"},
	}, nil, api.StreamHandlers{})
	if err != nil {
		t.Fatal(err)
	}
	if msg.Content != "回答" {
		t.Fatalf("回复不对: %q", msg.Content)
	}
	if captured == nil {
		t.Fatal("没收到请求")
	}
	if got, _ := captured["temperature"].(float64); got != 0.3 {
		t.Fatalf("temperature 未发出: %v", captured["temperature"])
	}
	if got, _ := captured["max_tokens"].(float64); got != 2048 {
		t.Fatalf("max_tokens 未发出: %v", captured["max_tokens"])
	}
	msgs, _ := captured["messages"].([]any)
	if len(msgs) != 2 {
		t.Fatalf("system + user 应都发出: %v", msgs)
	}
	first, _ := msgs[0].(map[string]any)
	if first["role"] != "system" || first["content"] != "你是 gsh" {
		t.Fatalf("system 消息丢失或位置不对: %v", first)
	}
	u := o.LastUsage()
	if u.TotalTokens != 49 || u.Estimated {
		t.Fatalf("应优先用 provider 上报的真实用量: %+v", u)
	}
}

func TestUsageFallsBackToEstimate(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, sseBody(`{"choices":[{"delta":{"content":"一段中文回答"}}]}`))
	}))
	defer srv.Close()
	c := defaultConfig()
	c.BaseURL = srv.URL
	o := newOpenAI(c)
	_, err := o.Chat(context.Background(), []api.Message{{Role: api.RoleUser, Content: "问题"}}, nil, api.StreamHandlers{})
	if err != nil {
		t.Fatal(err)
	}
	u := o.LastUsage()
	if !u.Estimated || u.TotalTokens <= 0 {
		t.Fatalf("provider 不给 usage 时应本地估算: %+v", u)
	}
}

func TestRetryOn429ThenSucceed(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) == 1 {
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = io.WriteString(w, `{"error":{"message":"rate limited"}}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, sseBody(`{"choices":[{"delta":{"content":"ok"}}]}`))
	}))
	defer srv.Close()
	c := defaultConfig()
	c.BaseURL = srv.URL
	c.Retry = Retry{Max: 2, BaseMS: 1}
	o := newOpenAI(c)
	msg, err := o.Chat(context.Background(), []api.Message{{Role: api.RoleUser, Content: "hi"}}, nil, api.StreamHandlers{})
	if err != nil {
		t.Fatal(err)
	}
	if msg.Content != "ok" || calls.Load() != 2 {
		t.Fatalf("应在 429 后重试成功: content=%q calls=%d", msg.Content, calls.Load())
	}
}

func TestNoRetryOn400(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"error":{"message":"bad request"}}`)
	}))
	defer srv.Close()
	c := defaultConfig()
	c.BaseURL = srv.URL
	c.Retry = Retry{Max: 3, BaseMS: 1}
	o := newOpenAI(c)
	_, err := o.Chat(context.Background(), []api.Message{{Role: api.RoleUser, Content: "hi"}}, nil, api.StreamHandlers{})
	if err == nil {
		t.Fatal("400 应该报错")
	}
	if calls.Load() != 1 {
		t.Fatalf("4xx 参数错误不该重试，实际请求 %d 次", calls.Load())
	}
}

func TestRetryRespectsContextCancel(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()
	c := defaultConfig()
	c.BaseURL = srv.URL
	c.Retry = Retry{Max: 5, BaseMS: 5000}
	o := newOpenAI(c)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Millisecond)
	defer cancel()
	_, err := o.Chat(ctx, []api.Message{{Role: api.RoleUser, Content: "hi"}}, nil, api.StreamHandlers{})
	if err == nil {
		t.Fatal("应返回取消错误")
	}
	if calls.Load() > 2 {
		t.Fatalf("取消后不应继续重试: %d 次", calls.Load())
	}
}

func TestModelOnlyReturnsReasoningDoesNotSilence(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, sseBody(`{"choices":[{"delta":{"reasoning_content":"只有思考"}}]}`))
	}))
	defer srv.Close()
	c := defaultConfig()
	c.BaseURL = srv.URL
	o := newOpenAI(c)
	msg, err := o.Chat(context.Background(), []api.Message{{Role: api.RoleUser, Content: "hi"}}, nil, api.StreamHandlers{})
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(msg.Content) == "" {
		t.Fatal("只吐思考时不能返回空正文把上层卡死")
	}
	if strings.Contains(msg.Content, "只有思考") {
		t.Fatal("思考原文不该被塞进正文")
	}
	if msg.Reasoning != "只有思考" {
		t.Fatalf("思考内容应保留在独立字段: %q", msg.Reasoning)
	}
}

// TestImagePartBecomesDataURL 验证图片附件被编码成 OpenAI 的 image_url 段。
func TestImagePartBecomesDataURL(t *testing.T) {
	dir := t.TempDir()
	imgPath := filepath.Join(dir, "shot.png")
	// 1x1 透明 PNG，够验证 base64 与 MIME 推断。
	raw, _ := base64.StdEncoding.DecodeString("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNkYPhfDwAChwGA60e6kgAAAABJRU5ErkJggg==")
	if err := os.WriteFile(imgPath, raw, 0o644); err != nil {
		t.Fatal(err)
	}

	var captured map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &captured)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, sseBody(`{"choices":[{"delta":{"content":"看到了"}}]}`))
	}))
	defer srv.Close()

	c := defaultConfig()
	c.BaseURL = srv.URL
	o := newOpenAI(c)
	if _, err := o.Chat(context.Background(), []api.Message{{
		Role:    api.RoleUser,
		Content: "这张图里是什么",
		Parts:   []api.ContentPart{{Type: "image", Path: imgPath}},
	}}, nil, api.StreamHandlers{}); err != nil {
		t.Fatal(err)
	}
	msgs, _ := captured["messages"].([]any)
	if len(msgs) != 1 {
		t.Fatalf("应只有一条 user 消息: %v", msgs)
	}
	m, _ := msgs[0].(map[string]any)
	parts, ok := m["content"].([]any)
	if !ok {
		t.Fatalf("带图片时 content 必须是数组: %v", m["content"])
	}
	if len(parts) != 2 {
		t.Fatalf("应为 文本 + 图片 两段: %v", parts)
	}
	first, _ := parts[0].(map[string]any)
	if first["type"] != "text" || first["text"] != "这张图里是什么" {
		t.Fatalf("文本段不对: %v", first)
	}
	second, _ := parts[1].(map[string]any)
	if second["type"] != "image_url" {
		t.Fatalf("图片段类型不对: %v", second)
	}
	iu, _ := second["image_url"].(map[string]any)
	url, _ := iu["url"].(string)
	if !strings.HasPrefix(url, "data:image/png;base64,") {
		t.Fatalf("应是 png data URL: %.40s", url)
	}
}

// TestMissingImageDegradesToText 验证图片读不到时不阻断整轮请求。
func TestMissingImageDegradesToText(t *testing.T) {
	var captured map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &captured)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, sseBody(`{"choices":[{"delta":{"content":"好"}}]}`))
	}))
	defer srv.Close()

	c := defaultConfig()
	c.BaseURL = srv.URL
	o := newOpenAI(c)
	if _, err := o.Chat(context.Background(), []api.Message{{
		Role:    api.RoleUser,
		Content: "看图",
		Parts:   []api.ContentPart{{Type: "image", Path: filepath.Join(t.TempDir(), "nope.png")}},
	}}, nil, api.StreamHandlers{}); err != nil {
		t.Fatalf("图片缺失不该让整轮失败: %v", err)
	}
	msgs, _ := captured["messages"].([]any)
	m, _ := msgs[0].(map[string]any)
	text, ok := m["content"].(string)
	if !ok {
		t.Fatalf("没有可用图片时应退回字符串 content: %v", m["content"])
	}
	if !strings.Contains(text, "未能读取") {
		t.Fatalf("应把失败原因写进文本: %q", text)
	}
}

// TestDataURLCachedAcrossTurns 同一张图在后续轮次必须走缓存。
// 历史消息只存路径，每轮出站都要展开一次；8MB 的图变成 11MB 字符串，
// 不缓存就是每轮重新读盘 + 重新编码一遍。
func TestDataURLCachedAcrossTurns(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "shot.png")
	raw, _ := base64.StdEncoding.DecodeString("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNkYPhfDwAChwGA60e6kgAAAABJRU5ErkJggg==")
	if err := os.WriteFile(p, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	part := api.ContentPart{Type: "image", Path: p}
	first, err := dataURL(part)
	if err != nil {
		t.Fatal(err)
	}

	info, statErr := os.Stat(p)
	if statErr != nil {
		t.Fatal(statErr)
	}
	// 用同样长度、且把 mtime 还原成原值的垃圾内容覆盖：
	// 缓存 key（路径+大小+mtime）完全不变，只要还去读盘结果必然变；
	// 结果不变就说明走的是缓存，没有重新读文件。
	garbage := make([]byte, info.Size())
	copy(garbage, "not a png at all")
	if err := os.WriteFile(p, garbage, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(p, info.ModTime(), info.ModTime()); err != nil {
		t.Fatal(err)
	}

	second, err := dataURL(part)
	if err != nil {
		t.Fatalf("缓存命中时不该读盘失败: %v", err)
	}
	if first != second {
		t.Fatal("key 未变时不该重新读盘编码")
	}
}

// TestDataURLInvalidatedByMtime 同名文件被覆盖后缓存必须失效，
// 否则用户换了一张图，模型看到的还是上一张。
func TestDataURLInvalidatedByMtime(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "shot.png")
	raw, _ := base64.StdEncoding.DecodeString("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNkYPhfDwAChwGA60e6kgAAAABJRU5ErkJggg==")
	if err := os.WriteFile(p, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	first, err := dataURL(api.ContentPart{Type: "image", Path: p})
	if err != nil {
		t.Fatal(err)
	}

	// 换成本地不同内容、并把 mtime 推后，模拟同名覆盖。
	raw[0] = 0x00
	if err := os.WriteFile(p, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	later := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(p, later, later); err != nil {
		t.Fatal(err)
	}
	second, err := dataURL(api.ContentPart{Type: "image", Path: p})
	if err != nil {
		t.Fatal(err)
	}
	if first == second {
		t.Fatal("文件内容变了，缓存必须失效")
	}
}
