package mcp

import (
	"context"
	"fmt"
	"strings"

	"gsh/internal/api"
)

// Tool 把远端 MCP 工具包装成本地工具。
//
// 名字带 server 前缀：不同 server 的工具重名很常见（get、search），
// 直接注册会互相覆盖，模型也无从判断该用哪个。
type Tool struct {
	client *Client
	remote remoteTool
}

// Name 是暴露给模型的工具名。
func (t *Tool) Name() string {
	return "mcp_" + sanitize(t.client.name) + "_" + sanitize(t.remote.Name)
}

// Spec 实现 api.Tool。
func (t *Tool) Spec() api.ToolSpec {
	desc := strings.TrimSpace(t.remote.Description)
	if desc == "" {
		desc = "由 MCP server " + t.client.name + " 提供的工具"
	}
	params := t.remote.InputSchema
	if params == nil {
		params = map[string]any{"type": "object", "properties": map[string]any{}}
	}
	return api.ToolSpec{Name: t.Name(), Description: desc, Parameters: params, ReadOnly: t.client.readOnly}
}

// Run 实现 api.Tool。
func (t *Tool) Run(ctx context.Context, argsJSON string) (string, error) {
	text, isErr, err := t.client.CallTool(ctx, t.remote.Name, argsJSON)
	if err != nil {
		return "", err
	}
	if isErr {
		return text, fmt.Errorf("MCP 工具返回错误: %s", firstLine(text))
	}
	return text, nil
}

// sanitize 把名字里的非法字符换成下划线，保证工具名只含 [a-z0-9_]。
func sanitize(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	return strings.Trim(b.String(), "_")
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
