package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"

	"gsh/internal/api"
)

// WebSearchTool 联网检索。
//
// 三种后端：
//   - duckduckgo（默认）：解析 html 端点，无需密钥；
//   - searxng：自建实例，需 endpoint；
//   - tavily：商业 API，需 api_key。
//
// 是否走代理由配置决定，不读系统代理——本机直连与代理可达性常常相反，
// 让工具自己猜只会得到「有时能搜有时不能搜」。
type WebSearchTool struct {
	provider   string
	endpoint   string
	apiKey     string
	maxResults int
	client     *http.Client
}

// newWebSearch 从配置构造。返回 nil 表示配置不完整（如 tavily 缺 key），
// 此时不注册该工具，而不是注册一个必定失败的工具。
func newWebSearch(cfg map[string]any) *WebSearchTool {
	t := &WebSearchTool{provider: "duckduckgo", maxResults: 5}
	if cfg != nil {
		if v, ok := cfg["provider"].(string); ok && v != "" {
			t.provider = strings.ToLower(v)
		}
		if v, ok := cfg["endpoint"].(string); ok {
			t.endpoint = strings.TrimRight(v, "/")
		}
		if v, ok := cfg["api_key"].(string); ok {
			t.apiKey = resolveSecret(v)
		}
		if f, ok := cfg["max_results"].(float64); ok && f > 0 {
			t.maxResults = int(f)
		}
	}
	timeout := 20 * time.Second
	if cfg != nil {
		if f, ok := cfg["timeout_seconds"].(float64); ok && f > 0 {
			timeout = time.Duration(f) * time.Second
		}
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	if cfg != nil {
		if v, ok := cfg["proxy"].(string); ok && v != "" {
			if pu, err := url.Parse(v); err == nil {
				transport.Proxy = http.ProxyURL(pu)
			}
		}
	}
	t.client = &http.Client{Timeout: timeout, Transport: transport}

	switch t.provider {
	case "duckduckgo", "ddg":
		t.provider = "duckduckgo"
		if t.endpoint == "" {
			t.endpoint = "https://html.duckduckgo.com/html/"
		}
	case "searxng", "searx":
		if t.endpoint == "" {
			return nil
		}
	case "tavily":
		if t.endpoint == "" {
			t.endpoint = "https://api.tavily.com/search"
		}
		if t.apiKey == "" {
			return nil
		}
	default:
		return nil
	}
	return t
}

// resolveSecret 支持 env: 前缀，避免密钥明文落盘。
func resolveSecret(v string) string {
	if strings.HasPrefix(v, "env:") {
		return os.Getenv(strings.TrimPrefix(v, "env:"))
	}
	return v
}

// Spec 实现 api.Tool。
func (t *WebSearchTool) Spec() api.ToolSpec {
	return api.ToolSpec{
		Name:     "web_search",
		ReadOnly: true,
		Description: "搜索互联网并返回标题、链接与摘要。用于查最新信息、文档、报错原因等。" +
			"结果是第三方摘要，关键事实应再用 web_fetch 打开原文核对。",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"query":       map[string]any{"type": "string", "description": "检索词"},
				"max_results": map[string]any{"type": "integer", "description": "返回条数，默认沿用配置"},
			},
			"required": []string{"query"},
		},
	}
}

// searchResult 统一的结果条目，三个后端都归一到这里。
type searchResult struct {
	Title   string `json:"title"`
	URL     string `json:"url"`
	Snippet string `json:"snippet"`
}

// Run 实现 api.Tool。
func (t *WebSearchTool) Run(ctx context.Context, argsJSON string) (string, error) {
	m, err := argObj(argsJSON)
	if err != nil {
		return "", err
	}
	q := strings.TrimSpace(argStr(m, "query"))
	if q == "" {
		return "", fmt.Errorf("query 不能为空")
	}
	limit := t.maxResults
	if f, ok := m["max_results"].(float64); ok && f > 0 {
		limit = int(f)
	}

	var (
		results []searchResult
		serr    error
	)
	switch t.provider {
	case "duckduckgo":
		results, serr = t.searchDDG(ctx, q, limit)
	case "searxng":
		results, serr = t.searchSearx(ctx, q, limit)
	case "tavily":
		results, serr = t.searchTavily(ctx, q, limit)
	default:
		serr = fmt.Errorf("未配置搜索后端")
	}
	if serr != nil {
		return "", serr
	}
	if len(results) == 0 {
		return fmt.Sprintf("没有找到与 %q 相关的结果。", q), nil
	}
	var b strings.Builder
	fmt.Fprintf(&b, "搜索 %q 的前 %d 条结果：\n", q, len(results))
	for i, r := range results {
		fmt.Fprintf(&b, "\n%d. %s\n   %s\n", i+1, r.Title, r.URL)
		if r.Snippet != "" {
			fmt.Fprintf(&b, "   %s\n", r.Snippet)
		}
	}
	return b.String(), nil
}

// searchDDG 解析 html 端点的结果页。
// 该端点结构稳定多年：标题在 a.result__a，摘要在 a.result__snippet。
func (t *WebSearchTool) searchDDG(ctx context.Context, q string, limit int) ([]searchResult, error) {
	u := t.endpoint + "?q=" + url.QueryEscape(q)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept-Language", "zh-CN,zh;q=0.9,en;q=0.8")
	resp, err := t.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("搜索请求失败（该后端常需代理，检查 plugins_cfg.tools.websearch.proxy）: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("搜索返回 HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, err
	}
	return parseDDG(string(body), limit), nil
}

var (
	ddgLinkRe    = regexp.MustCompile(`(?s)<a[^>]*class="result__a"[^>]*href="([^"]+)"[^>]*>(.*?)</a>`)
	ddgSnippetRe = regexp.MustCompile(`(?s)<a[^>]*class="result__snippet"[^>]*>(.*?)</a>`)
	tagRe        = regexp.MustCompile(`(?s)<[^>]+>`)
	spaceRe      = regexp.MustCompile(`\s+`)
)

// parseDDG 从结果页抽出条目。链接是 duckduckgo 的跳转形式，
// 真实地址在 uddg 参数里，必须解出来给模型，否则它拿到的是不可读的中间地址。
func parseDDG(html string, limit int) []searchResult {
	links := ddgLinkRe.FindAllStringSubmatch(html, -1)
	snips := ddgSnippetRe.FindAllStringSubmatch(html, -1)
	out := make([]searchResult, 0, limit)
	for i, m := range links {
		if len(out) >= limit {
			break
		}
		title := cleanText(m[2])
		if title == "" {
			continue
		}
		r := searchResult{Title: title, URL: realURL(m[1])}
		if i < len(snips) && len(snips[i]) > 1 {
			r.Snippet = cleanText(snips[i][1])
		}
		out = append(out, r)
	}
	return out
}

// realURL 解开 duckduckgo 跳转链接；非跳转链接原样返回。
func realURL(raw string) string {
	raw = strings.ReplaceAll(raw, "&amp;", "&")
	if strings.HasPrefix(raw, "//") {
		raw = "https:" + raw
	}
	u, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	if target := u.Query().Get("uddg"); target != "" {
		return target
	}
	return raw
}

func cleanText(s string) string {
	s = tagRe.ReplaceAllString(s, "")
	s = htmlUnescape(s)
	return strings.TrimSpace(spaceRe.ReplaceAllString(s, " "))
}

// searchSearx 走 SearXNG 的 json 输出。
func (t *WebSearchTool) searchSearx(ctx context.Context, q string, limit int) ([]searchResult, error) {
	u := fmt.Sprintf("%s/search?q=%s&format=json", t.endpoint, url.QueryEscape(q))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", userAgent)
	resp, err := t.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("SearXNG 请求失败: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("SearXNG 返回 HTTP %d（实例可能未开启 json 输出）", resp.StatusCode)
	}
	var payload struct {
		Results []struct {
			Title   string `json:"title"`
			URL     string `json:"url"`
			Content string `json:"content"`
		} `json:"results"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return nil, err
	}
	out := make([]searchResult, 0, limit)
	for _, r := range payload.Results {
		if len(out) >= limit {
			break
		}
		out = append(out, searchResult{Title: r.Title, URL: r.URL, Snippet: r.Content})
	}
	return out, nil
}

// searchTavily 走 Tavily 的 /search 接口。
func (t *WebSearchTool) searchTavily(ctx context.Context, q string, limit int) ([]searchResult, error) {
	body, _ := json.Marshal(map[string]any{
		"api_key":     t.apiKey,
		"query":       q,
		"max_results": limit,
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.endpoint, strings.NewReader(string(body)))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := t.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("Tavily 请求失败: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return nil, fmt.Errorf("Tavily 返回 HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(msg)))
	}
	var payload struct {
		Results []struct {
			Title   string `json:"title"`
			URL     string `json:"url"`
			Content string `json:"content"`
		} `json:"results"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return nil, err
	}
	out := make([]searchResult, 0, limit)
	for _, r := range payload.Results {
		out = append(out, searchResult{Title: r.Title, URL: r.URL, Snippet: r.Content})
	}
	return out, nil
}
