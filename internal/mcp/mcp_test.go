package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"
)

// TestMain 兼作假 MCP server 的入口：同一个二进制，凭环境变量切换角色。
// 这样测试不依赖外部 npm 包，也不会因为网络问题飘。
func TestMain(m *testing.M) {
	if os.Getenv("GSH_FAKE_MCP") == "1" {
		fakeServer()
		return
	}
	os.Exit(m.Run())
}

// fakeServer 是一个最小 MCP server：两个工具，其中一个固定报错。
func fakeServer() {
	in := bufio.NewReader(os.Stdin)
	out := os.Stdout
	for {
		line, err := in.ReadBytes('\n')
		if err != nil {
			return
		}
		var req struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params struct {
				Name      string         `json:"name"`
				Arguments map[string]any `json:"arguments"`
			} `json:"params"`
		}
		if json.Unmarshal(line, &req) != nil {
			continue
		}
		reply := func(result any) {
			b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": result})
			out.Write(append(b, '\n'))
		}
		switch req.Method {
		case "initialize":
			reply(map[string]any{"protocolVersion": protocolVersion, "capabilities": map[string]any{}})
		case "notifications/initialized":
			// 通知没有 id，不回
		case "tools/list":
			reply(map[string]any{"tools": []map[string]any{
				{
					"name":        "echo",
					"description": "回显 text",
					"inputSchema": map[string]any{"type": "object", "properties": map[string]any{"text": map[string]any{"type": "string"}}},
				},
				{
					"name":        "boom",
					"description": "总是失败",
					"inputSchema": map[string]any{"type": "object"},
				},
			}})
		case "tools/call":
			switch req.Params.Name {
			case "echo":
				text, _ := req.Params.Arguments["text"].(string)
				reply(map[string]any{"content": []map[string]any{{"type": "text", "text": "echo:" + text}}})
			case "boom":
				reply(map[string]any{
					"content": []map[string]any{{"type": "text", "text": "炸了\n第二行"}},
					"isError": true,
				})
			default:
				b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": req.ID, "error": map[string]any{"code": -32602, "message": "unknown tool"}})
				out.Write(append(b, '\n'))
			}
		default:
			b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": req.ID, "error": map[string]any{"code": -32601, "message": "no such method"}})
			out.Write(append(b, '\n'))
		}
	}
}

// startFake 拉起假 server。
func startFake(t *testing.T, name string) *Client {
	t.Helper()
	if os.Getenv("GSH_FAKE_MCP") == "1" {
		t.Skip("server 进程里不该再起 server")
	}
	c, err := start(map[string]any{
		"name":    name,
		"command": os.Args[0],
		"args":    []any{"-test.run=TestMain"},
		"env":     map[string]any{"GSH_FAKE_MCP": "1"},
	})
	if err != nil {
		t.Fatalf("启动假 server 失败: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func TestListAndCall(t *testing.T) {
	c := startFake(t, "demo")
	tools, err := c.ListTools()
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	if len(tools) != 2 {
		t.Fatalf("工具数应为 2，实际 %d", len(tools))
	}
	text, isErr, err := c.CallTool(context.Background(), "echo", `{"text":"你好"}`)
	if err != nil || isErr {
		t.Fatalf("调用失败: %v / isErr=%v", err, isErr)
	}
	if text != "echo:你好" {
		t.Fatalf("返回值不对: %q", text)
	}
}

func TestCallErrorResult(t *testing.T) {
	c := startFake(t, "demo")
	text, isErr, err := c.CallTool(context.Background(), "boom", `{}`)
	if err != nil {
		t.Fatalf("协议层不该报错: %v", err)
	}
	if !isErr {
		t.Fatal("应标记为工具级错误")
	}
	if !strings.Contains(text, "炸了") {
		t.Fatalf("应带回原始文本: %q", text)
	}
}

func TestUnknownMethodAndBadArgs(t *testing.T) {
	c := startFake(t, "demo")
	if err := c.request(context.Background(), "no/such", map[string]any{}, nil); err == nil {
		t.Fatal("未知方法应报错")
	}
	if _, _, err := c.CallTool(context.Background(), "echo", `{bad json`); err == nil {
		t.Fatal("坏参数应在本地就报错")
	}
}

func TestToolNaming(t *testing.T) {
	c := startFake(t, "My Server")
	tools, err := c.ListTools()
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	tool := &Tool{client: c, remote: tools[0]}
	if got := tool.Name(); got != "mcp_my_server_echo" {
		t.Fatalf("工具名规范化不对: %q", got)
	}
	if spec := tool.Spec(); spec.Name != tool.Name() || spec.Parameters == nil {
		t.Fatalf("Spec 不完整: %+v", spec)
	}
	out, err := tool.Run(context.Background(), `{"text":"x"}`)
	if err != nil || out != "echo:x" {
		t.Fatalf("包装后调用失败: %q %v", out, err)
	}
	// 工具级错误应折成 error 返回，主循环据此标红。
	if _, err := (&Tool{client: c, remote: tools[1]}).Run(context.Background(), `{}`); err == nil {
		t.Fatal("isError 应转成 Go error")
	}
}

func TestDeadServerTimesOut(t *testing.T) {
	c := startFake(t, "demo")
	_ = c.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, _, err := c.CallTool(ctx, "echo", `{"text":"x"}`); err == nil {
		t.Fatal("server 关掉后调用应失败")
	}
}
