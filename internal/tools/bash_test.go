package tools

import (
	"context"
	"strings"
	"testing"
)

func TestBashToolSafetyBlocks(t *testing.T) {
	ctx := context.Background()

	// 默认（allowDestructive=false）：危险命令被拦截，且不真正执行
	bt := &BashTool{}
	_, err := bt.Run(ctx, `{"command":"rm -rf /"}`)
	if err == nil || !strings.Contains(err.Error(), "安全策略") {
		t.Fatalf("expected safety block, got err=%v", err)
	}

	// 普通命令仍可执行
	out, err := bt.Run(ctx, `{"command":"echo ok-gsh"}`)
	if err != nil {
		t.Fatalf("safe command should run, got err=%v", err)
	}
	if !strings.Contains(out, "ok-gsh") {
		t.Fatalf("unexpected output: %q", out)
	}

	// allowDestructive=true：放行（但仍不真跑 rm，这里用 echo 验证开关不破坏执行）
	btOn := &BashTool{allowDestructive: true}
	out, err = btOn.Run(ctx, `{"command":"echo allowed"}`)
	if err != nil || !strings.Contains(out, "allowed") {
		t.Fatalf("allow_destructive should not break execution, got out=%q err=%v", out, err)
	}
}
