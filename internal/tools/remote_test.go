package tools

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 远程执行分支的行为验证：配置解析、未知节点报错、远程模式致命/受限命令仍本地拦截。
func TestBashToolRemote(t *testing.T) {
	ctx := context.Background()
	bt := &BashTool{
		nodes: map[string]NodeSpec{"n1": {Host: "h", Port: 22, User: "u"}},
	}

	// 未知节点：报错且不发起 SSH（无连接尝试）
	_, err := bt.Run(ctx, `{"command":"echo x","node":"nope"}`)
	if err == nil || !strings.Contains(err.Error(), "未知节点") {
		t.Fatalf("expected unknown-node error, got %v", err)
	}

	// 远程模式致命命令：本地 scanCommand 先拦，绝不发起 SSH
	_, err = bt.Run(ctx, `{"command":"rm -rf /","node":"n1"}`)
	if err == nil || !strings.Contains(err.Error(), "安全策略") {
		t.Fatalf("remote fatal command must be blocked locally, got %v", err)
	}

	// 远程模式受限命令同样拦截（allow_destructive=false）
	_, err = bt.Run(ctx, `{"command":"rm -r /tmp/x","node":"n1"}`)
	if err == nil || !strings.Contains(err.Error(), "安全策略") {
		t.Fatalf("remote gated command must be blocked, got %v", err)
	}
}

// TestBashToolExecBackend 外部命令后端：占位符替换、stdin 兜底、安全策略前置。
func TestBashToolExecBackend(t *testing.T) {
	ctx := context.Background()
	// 替身程序：把收到的参数与 stdin 都打印出来。
	// 用 go 自身当替身，避免依赖 sh/bash（Windows 上无 sh）。
	marker := filepath.Join(t.TempDir(), "out.txt")
	bt := &BashTool{
		nodes: map[string]NodeSpec{
			"viaArg": {
				Exec: os.Args[0],
				Args: []string{"-test.run=TestHelperProcess", "--", "echo", "{cmd}"},
			},
			"viaStdin": {
				Exec: os.Args[0],
				Args: []string{"-test.run=TestHelperProcess", "--", "stdin"},
			},
		},
	}

	// 占位符 {cmd} 被替换进 argv
	out, err := bt.Run(ctx, `{"command":"hello world","node":"viaArg"}`)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !strings.Contains(out, "hello world") {
		t.Errorf("placeholder {cmd} not substituted: %q", out)
	}

	// 无占位符 → 命令走 stdin
	out, err = bt.Run(ctx, `{"command":"from-stdin","node":"viaStdin"}`)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !strings.Contains(out, "from-stdin") {
		t.Errorf("command should go through stdin: %q", out)
	}

	// 安全策略在启动外部程序之前生效
	_, err = bt.Run(ctx, `{"command":"rm -rf /","node":"viaArg"}`)
	if err == nil || !strings.Contains(err.Error(), "安全策略") {
		t.Fatalf("fatal command must be blocked before exec, got %v", err)
	}
	_ = marker
}

// TestHelperProcess 替身程序：被 exec 后端当作外部命令调用。
// 通过 -test.run=TestHelperProcess -- <mode> [args...] 传参。
func TestHelperProcess(t *testing.T) {
	idx := -1
	for i, a := range os.Args {
		if a == "--" {
			idx = i
			break
		}
	}
	if idx < 0 {
		return // 不是被当作替身调用，正常跳过
	}
	rest := os.Args[idx+1:]
	mode := ""
	if len(rest) > 0 {
		mode = rest[0]
		rest = rest[1:]
	}
	switch mode {
	case "stdin":
		data, _ := io.ReadAll(os.Stdin)
		fmt.Printf("STDIN:%s", strings.TrimSpace(string(data)))
	case "echo":
		fmt.Printf("ARGS:%s", strings.Join(rest, " "))
	default:
		fmt.Printf("MODE:%s", mode)
	}
	os.Exit(0)
}
