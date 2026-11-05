package tools

import (
	"context"
	"os"
	"strings"
	"testing"
)

// ddgPage 是结果页的最小结构样本：标题在 a.result__a，摘要在 a.result__snippet。
const ddgPage = `<div class="result">
<a rel="nofollow" class="result__a" href="//duckduckgo.com/l/?uddg=https%3A%2F%2Fpkg.go.dev%2Fcontext&amp;rut=abc">context package - Go Packages</a>
<a class="result__snippet" href="//duckduckgo.com/l/?uddg=https%3A%2F%2Fpkg.go.dev%2Fcontext">Package <b>context</b> defines the Context type.</a>
</div>
<div class="result">
<a rel="nofollow" class="result__a" href="https://go.dev/blog/context">Go Concurrency Patterns</a>
<a class="result__snippet">How to use context.</a>
</div>`

// TestParseDDG 跳转链接要解出真实地址，标签要去掉，摘要要清理空白。
func TestParseDDG(t *testing.T) {
	got := parseDDG(ddgPage, 5)
	if len(got) != 2 {
		t.Fatalf("应解析出 2 条，实际 %d", len(got))
	}
	if got[0].URL != "https://pkg.go.dev/context" {
		t.Fatalf("跳转链接未解开: %s", got[0].URL)
	}
	if got[1].URL != "https://go.dev/blog/context" {
		t.Fatalf("普通链接不应被改动: %s", got[1].URL)
	}
	if strings.Contains(got[0].Snippet, "<b>") {
		t.Fatalf("摘要未去标签: %q", got[0].Snippet)
	}
	if !strings.Contains(got[0].Title, "context package") {
		t.Fatalf("标题错误: %q", got[0].Title)
	}
}

// TestParseDDGRespectsLimit 条数上限必须生效。
func TestParseDDGRespectsLimit(t *testing.T) {
	if got := parseDDG(ddgPage, 1); len(got) != 1 {
		t.Fatalf("limit=1 时应只返回 1 条，实际 %d", len(got))
	}
}

// TestNewWebSearchRequiresConfig 配置不完整的后端不注册工具（返回 nil），
// 避免给模型一个注定失败的工具。
func TestNewWebSearchRequiresConfig(t *testing.T) {
	if t1 := newWebSearch(map[string]any{"provider": "tavily"}); t1 != nil {
		t.Fatal("tavily 缺 api_key 时不应构造成功")
	}
	if t2 := newWebSearch(map[string]any{"provider": "searxng"}); t2 != nil {
		t.Fatal("searxng 缺 endpoint 时不应构造成功")
	}
	if t3 := newWebSearch(map[string]any{"provider": "duckduckgo"}); t3 == nil {
		t.Fatal("duckduckgo 无需额外配置，应构造成功")
	}
}

// TestResolveSecretEnv 密钥支持 env: 引用，不落明文。
func TestResolveSecretEnv(t *testing.T) {
	t.Setenv("GSH_TEST_KEY", "abc123")
	if got := resolveSecret("env:GSH_TEST_KEY"); got != "abc123" {
		t.Fatalf("env 引用失败: %q", got)
	}
	if got := resolveSecret("plain"); got != "plain" {
		t.Fatalf("普通值不应被改写: %q", got)
	}
}

// TestWebSearchLive 真实联网检索。默认跳过：CI 与离线环境不该依赖外网，
// 需要时 GSH_LIVE_SEARCH=1 手动验证解析链路是否还对得上目标站点的结构。
func TestWebSearchLive(t *testing.T) {
	if os.Getenv("GSH_LIVE_SEARCH") == "" {
		t.Skip("设置 GSH_LIVE_SEARCH=1 才跑真实检索")
	}
	tool := newWebSearch(map[string]any{
		"provider": "duckduckgo",
		"proxy":    os.Getenv("GSH_LIVE_PROXY"),
	})
	if tool == nil {
		t.Fatal("构造失败")
	}
	out, err := tool.Run(context.Background(), `{"query":"golang context","max_results":3}`)
	if err != nil {
		t.Fatalf("检索失败: %v", err)
	}
	if !strings.Contains(out, "http") {
		t.Fatalf("结果里没有链接: %s", out)
	}
	t.Logf("检索输出:\n%s", out)
}
