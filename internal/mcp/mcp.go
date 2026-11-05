// Package mcp 接入 Model Context Protocol 服务器，把外部工具接进同一个工具表。
//
// 只实现 stdio + tools 能力：这两样覆盖了绝大多数现成 MCP server
// （文件系统、数据库、浏览器、公司内部系统）。resources / prompts
// 暂不接入——它们的语义是「给模型看资料」，而 gsh 里资料走读文件这条路。
package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"sync"
	"time"

	"gsh/internal/api"
	"gsh/internal/kernel"
)

// 握手与单次调用的默认时限。MCP server 常是 npx/uvx 起的进程，
// 冷启动要装包，给太短会误判成坏配置。
const (
	handshakeTimeout = 20 * time.Second
	callTimeout      = 60 * time.Second
	protocolVersion  = "2024-11-05"
)

// RegisterKernel 挂载 MCP 插件。必须排在 tools 之后：它要往工具表里加条目。
func RegisterKernel(k *kernel.Kernel) error { return k.Load(&Plugin{}) }

// Plugin MCP 客户端插件。
type Plugin struct{}

// Name 实现 kernel.Plugin。
func (p *Plugin) Name() string { return "mcp" }

// Apply 逐个拉起配置里的 server，把它们的工具注册进来。
// 任何一个 server 起不来都只跳过它自己：一个坏配置不该让整个进程不可用。
func (p *Plugin) Apply(ctx *kernel.Ctx) error {
	cfg := ctx.Config()
	if cfg == nil {
		return nil
	}
	servers, ok := cfg["servers"].([]any)
	if !ok || len(servers) == 0 {
		return nil
	}
	reg, err := kernel.Use[api.ToolRegistry](kernel.Current, "toolRegistry")
	if err != nil {
		return nil
	}
	var clients []*Client
	for _, raw := range servers {
		sc, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		c, cerr := start(sc)
		if cerr != nil {
			// 配置错误写进运行期状态，Web/TUI 的 /tools 面板能看到原因。
			ctx.Emit("mcp.error", fmt.Sprintf("%v: %v", sc["name"], cerr))
			continue
		}
		clients = append(clients, c)
		tools, lerr := c.ListTools()
		if lerr != nil {
			ctx.Emit("mcp.error", fmt.Sprintf("%s: 列工具失败: %v", c.name, lerr))
			_ = c.Close()
			continue
		}
		for _, t := range tools {
			if rerr := reg.Register(&Tool{client: c, remote: t}); rerr != nil {
				ctx.Emit("mcp.error", fmt.Sprintf("%s/%s: 注册失败: %v", c.name, t.Name, rerr))
			}
		}
	}
	ctx.Provide("mcpClients", clients)
	// 内核关闭时收掉子进程，避免 npx 起的 node 残留。
	ctx.OnShutdown(func() {
		for _, c := range clients {
			_ = c.Close()
		}
	})
	return nil
}

// Client 是一个已握手的 MCP server 连接。
type Client struct {
	name string
	// readOnly 是 server 级别的声明：由配置的 read_only 决定。
	// MCP 无法从工具描述可靠推断会不会写盘，所以默认按「可能写」处理，
	// 只读模式下一律拒绝；确实只读的 server 在配置里显式声明即可。
	readOnly bool
	cmd      *exec.Cmd
	in       io.WriteCloser
	out      *bufio.Reader
	lines    chan []byte
	readErr  chan error

	mu     sync.Mutex // 一次只跑一个请求-响应周期
	nextID int
}

// remoteTool 是 server 声明的工具。
type remoteTool struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
}

// rpcResponse 是 JSON-RPC 响应。
type rpcResponse struct {
	ID     *json.RawMessage `json:"id"`
	Method string           `json:"method"`
	Result json.RawMessage  `json:"result"`
	Error  *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// start 拉起一个 server 进程并完成 initialize 握手。
func start(cfg map[string]any) (*Client, error) {
	name, _ := cfg["name"].(string)
	command, _ := cfg["command"].(string)
	if name == "" || command == "" {
		return nil, fmt.Errorf("缺少 name 或 command")
	}
	var args []string
	if raw, ok := cfg["args"].([]any); ok {
		for _, a := range raw {
			if s, ok := a.(string); ok {
				args = append(args, s)
			}
		}
	}
	cmd := exec.Command(command, args...)
	var env []string
	if raw, ok := cfg["env"].(map[string]any); ok {
		env = cmd.Environ()
		for k, v := range raw {
			if s, ok := v.(string); ok {
				env = append(env, k+"="+s)
			}
		}
	}
	cmd.Env = env
	in, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	// server 的日志走 stderr，不读会把它自己憋死（管道写满）。
	stderr, _ := cmd.StderrPipe()
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("启动失败: %w", err)
	}
	go func() { io.Copy(io.Discard, stderr) }()

	readOnly, _ := cfg["read_only"].(bool)
	c := &Client{
		name: name, cmd: cmd, in: in, readOnly: readOnly,
		out:     bufio.NewReaderSize(stdout, 64*1024),
		lines:   make(chan []byte, 64),
		readErr: make(chan error, 1),
	}
	go c.pump()
	if err := c.initialize(); err != nil {
		_ = c.Close()
		return nil, err
	}
	return c, nil
}

// pump 把 stdout 的每一行推进 channel。
// 读放在独立 goroutine 里，请求侧才能用 select 做超时；
// 若在请求侧直接阻塞读，超时后那半行数据会被下一个请求读到。
func (c *Client) pump() {
	for {
		line, err := c.out.ReadBytes('\n')
		if len(line) > 0 {
			c.lines <- line
		}
		if err != nil {
			c.readErr <- err
			close(c.lines)
			return
		}
	}
}

// initialize 完成协议握手。
func (c *Client) initialize() error {
	params := map[string]any{
		"protocolVersion": protocolVersion,
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": "gsh", "version": "1"},
	}
	if err := c.request(context.Background(), "initialize", params, nil); err != nil {
		return err
	}
	// initialized 是通知，没有响应。
	b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "method": "notifications/initialized"})
	_, err := c.in.Write(append(b, '\n'))
	return err
}

// ListTools 拉取 server 的工具清单。
func (c *Client) ListTools() ([]remoteTool, error) {
	var out struct {
		Tools []remoteTool `json:"tools"`
	}
	if err := c.request(context.Background(), "tools/list", map[string]any{}, &out); err != nil {
		return nil, err
	}
	return out.Tools, nil
}

// CallTool 调用一个远端工具，把 content 里的文本拼成结果。
func (c *Client) CallTool(ctx context.Context, name, argsJSON string) (string, bool, error) {
	var args any = map[string]any{}
	if strings.TrimSpace(argsJSON) != "" {
		if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
			return "", true, fmt.Errorf("参数不是合法 JSON: %w", err)
		}
	}
	var out struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		IsError bool `json:"isError"`
	}
	if err := c.request(ctx, "tools/call", map[string]any{"name": name, "arguments": args}, &out); err != nil {
		return "", true, err
	}
	var parts []string
	for _, p := range out.Content {
		if p.Text != "" {
			parts = append(parts, p.Text)
		}
	}
	text := strings.Join(parts, "\n")
	if strings.TrimSpace(text) == "" {
		text = "（工具没有返回文本内容）"
	}
	return text, out.IsError, nil
}

// request 发一条 JSON-RPC 请求并等对应的响应。
func (c *Client) request(ctx context.Context, method string, params any, result any) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	id := c.nextID
	c.nextID++
	msg := map[string]any{"jsonrpc": "2.0", "id": id, "method": method}
	if params != nil {
		msg["params"] = params
	}
	b, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	if _, err := c.in.Write(append(b, '\n')); err != nil {
		return fmt.Errorf("写入失败: %w", err)
	}

	timeout := callTimeout
	if method == "initialize" {
		timeout = handshakeTimeout
	}
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			return fmt.Errorf("%s 超时（%s）", method, timeout)
		case err := <-c.readErr:
			return fmt.Errorf("连接已断开: %v", err)
		case line, ok := <-c.lines:
			if !ok {
				return fmt.Errorf("server 已退出")
			}
			var resp rpcResponse
			if err := json.Unmarshal(line, &resp); err != nil {
				continue // 有些 server 会往 stdout 打非 JSON 日志
			}
			// server 主动发来的请求（如 sampling）：明确回不支持，别让它挂着等。
			if resp.Method != "" && resp.ID != nil {
				c.replyUnsupported(*resp.ID, resp.Method)
				continue
			}
			if resp.ID == nil || !idEquals(*resp.ID, id) {
				continue // 通知，或上一个超时请求的迟到响应
			}
			if resp.Error != nil {
				return fmt.Errorf("%s 返回错误 %d: %s", method, resp.Error.Code, resp.Error.Message)
			}
			if result != nil && len(resp.Result) > 0 {
				if err := json.Unmarshal(resp.Result, result); err != nil {
					return fmt.Errorf("解析 %s 响应失败: %w", method, err)
				}
			}
			return nil
		}
	}
}

// replyUnsupported 告知 server 该能力不可用。
func (c *Client) replyUnsupported(id json.RawMessage, method string) {
	b, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"error":   map[string]any{"code": -32601, "message": "gsh 不支持 " + method},
	})
	_, _ = c.in.Write(append(b, '\n'))
}

// Close 关闭连接并回收进程。
func (c *Client) Close() error {
	_ = c.in.Close()
	if c.cmd != nil && c.cmd.Process != nil {
		done := make(chan struct{})
		go func() {
			_ = c.cmd.Wait()
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			_ = c.cmd.Process.Kill()
		}
	}
	return nil
}

// idEquals 比较响应 id 与请求 id。id 可能是数字也可能是字符串。
func idEquals(raw json.RawMessage, want int) bool {
	var n int
	if err := json.Unmarshal(raw, &n); err == nil {
		return n == want
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s == fmt.Sprint(want)
	}
	return false
}
