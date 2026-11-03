package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"time"

	"gsh/internal/api"
)

// httpClient 共享客户端：工具调用应快速失败，不拖住整个 loop。
var httpClient = &http.Client{Timeout: 60 * time.Second}

// ---------- web_fetch ----------

// WebFetchTool 抓网页并转成纯文本（便于模型阅读）。
type WebFetchTool struct{}

// Spec 工具描述。
func (t *WebFetchTool) Spec() api.ToolSpec {
	return api.ToolSpec{
		Name:        "web_fetch",
		Description: "Fetch a URL and return readable text (HTML is stripped of tags/scripts). For raw bodies, status codes or non-GET methods use http_request.",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"url":       map[string]any{"type": "string"},
				"max_chars": map[string]any{"type": "integer", "description": "Max characters returned, default 20000"},
			},
			"required": []string{"url"},
		},
	}
}

// Run 抓取并转文本。
func (t *WebFetchTool) Run(ctx context.Context, argsJSON string) (string, error) {
	m, err := argObj(argsJSON)
	if err != nil {
		return "", err
	}
	url := argStr(m, "url")
	if url == "" {
		return "", fmt.Errorf("url is required")
	}
	maxChars := 20000
	if v, ok := m["max_chars"].(float64); ok && v > 0 {
		maxChars = int(v)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", userAgent)
	resp, err := httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return "", err
	}
	ctype := resp.Header.Get("Content-Type")
	body := string(raw)
	if strings.Contains(ctype, "html") || strings.HasPrefix(strings.TrimSpace(body), "<") {
		body = htmlToText(body)
	}
	if len(body) > maxChars {
		body = body[:maxChars] + "\n...[truncated]"
	}
	if resp.StatusCode >= 400 {
		return body, fmt.Errorf("HTTP %d %s", resp.StatusCode, resp.Status)
	}
	return body, nil
}

// ---------- http_request ----------

// HTTPRequestTool 通用 HTTP 调用（支持任意方法/头/体）。
type HTTPRequestTool struct{}

// Spec 工具描述。
func (t *HTTPRequestTool) Spec() api.ToolSpec {
	return api.ToolSpec{
		Name:        "http_request",
		Description: "Send an HTTP request (any method) with custom headers/body; returns status, headers and raw body. Use for APIs, not for reading articles (use web_fetch).",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"url":     map[string]any{"type": "string"},
				"method":  map[string]any{"type": "string", "description": "GET/POST/PUT/PATCH/DELETE/HEAD, default GET"},
				"headers": map[string]any{"type": "object", "description": "Request headers"},
				"body":    map[string]any{"type": "string", "description": "Request body"},
			},
			"required": []string{"url"},
		},
	}
}

// Run 发请求。
func (t *HTTPRequestTool) Run(ctx context.Context, argsJSON string) (string, error) {
	m, err := argObj(argsJSON)
	if err != nil {
		return "", err
	}
	url := argStr(m, "url")
	if url == "" {
		return "", fmt.Errorf("url is required")
	}
	method := strings.ToUpper(argStr(m, "method"))
	if method == "" {
		method = http.MethodGet
	}
	body := argStr(m, "body")
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, rd)
	if err != nil {
		return "", err
	}
	if hdrs, ok := m["headers"].(map[string]any); ok {
		keys := make([]string, 0, len(hdrs))
		for k := range hdrs {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			if v, ok := hdrs[k].(string); ok {
				req.Header.Set(k, v)
			}
		}
	}
	if req.Header.Get("User-Agent") == "" {
		req.Header.Set("User-Agent", userAgent)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return "", err
	}

	var b strings.Builder
	fmt.Fprintf(&b, "HTTP %d %s\n", resp.StatusCode, resp.Status)
	hkeys := make([]string, 0, len(resp.Header))
	for k := range resp.Header {
		hkeys = append(hkeys, k)
	}
	sort.Strings(hkeys)
	for _, k := range hkeys {
		fmt.Fprintf(&b, "%s: %s\n", k, strings.Join(resp.Header[k], ", "))
	}
	b.WriteString("\n")
	text := string(raw)
	// JSON 美化，便于模型读结构；解析失败就原样。
	if strings.Contains(resp.Header.Get("Content-Type"), "json") || json.Valid(raw) {
		var buf bytes.Buffer
		if err := json.Indent(&buf, raw, "", "  "); err == nil {
			text = buf.String()
		}
	}
	if len(text) > maxToolOutput {
		text = text[:maxToolOutput] + "\n...[truncated]"
	}
	b.WriteString(text)
	return b.String(), nil
}

// ---------- HTML -> text ----------

var (
	reScriptStyle = regexp.MustCompile(`(?is)<(script|style|noscript)[^>]*>.*?</(script|style|noscript)>`)
	reComment     = regexp.MustCompile(`(?s)<!--.*?-->`)
	reBlockTag    = regexp.MustCompile(`(?i)</?(p|div|section|article|br|li|tr|h[1-6]|pre|blockquote|table|ul|ol)[^>]*>`)
	reTag         = regexp.MustCompile(`(?s)<[^>]*>`)
	reBlankLines  = regexp.MustCompile(`\n{3,}`)
	reSpaces      = regexp.MustCompile(`[ \t]{2,}`)
)

// htmlToText 粗糙但够用的 HTML 转文本：去脚本样式、块级换行、解实体、压空白。
func htmlToText(h string) string {
	h = reComment.ReplaceAllString(h, "")
	h = reScriptStyle.ReplaceAllString(h, "")
	h = reBlockTag.ReplaceAllString(h, "\n")
	h = reTag.ReplaceAllString(h, "")
	h = htmlUnescape(h)
	h = reSpaces.ReplaceAllString(h, " ")
	lines := strings.Split(h, "\n")
	for i, ln := range lines {
		lines[i] = strings.TrimSpace(ln)
	}
	h = strings.Join(lines, "\n")
	h = reBlankLines.ReplaceAllString(h, "\n\n")
	return strings.TrimSpace(h)
}

// htmlUnescape 只处理常见实体，不引入 html 包的完整开销。
var entReplacer = strings.NewReplacer(
	"&nbsp;", " ", "&amp;", "&", "&lt;", "<", "&gt;", ">",
	"&quot;", `"`, "&#39;", "'", "&apos;", "'",
)

func htmlUnescape(s string) string { return entReplacer.Replace(s) }
