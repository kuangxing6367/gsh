package hooks

import (
	"context"
	"os"
	"regexp"
	"runtime"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"gsh/internal/api"
)

func mustRe(t *testing.T, pat string) *regexp.Regexp {
	t.Helper()
	re, err := regexp.Compile(pat)
	if err != nil {
		t.Fatalf("bad pattern %q: %v", pat, err)
	}
	return re
}

func TestParseList(t *testing.T) {
	got := parseList([]any{
		"echo plain",
		map[string]any{"command": "echo obj", "match": "^bash$", "timeout_seconds": float64(3)},
		map[string]any{"command": "echo bad", "match": "([unclosed"},
		map[string]any{"match": "no-command"},
	})
	if len(got) != 2 {
		t.Fatalf("want 2 rules, got %d: %+v", len(got), got)
	}
	if got[0].match != nil {
		t.Fatal("字符串写法应匹配全部工具")
	}
	if got[1].match == nil || !got[1].match.MatchString("bash") || got[1].match.MatchString("bashx") {
		t.Fatalf("match 正则未生效：%v", got[1].match)
	}
	if got[1].timeout <= 0 {
		t.Fatal("timeout 未解析")
	}
}

func TestBeforeToolBlocks(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("cmd 下退出码语义不同，行为在 Linux 上验证")
	}
	r := &runner{before: []rule{{command: "echo 禁止执行; exit 1"}}}
	res := r.Run(context.Background(), api.HookEvent{Phase: api.HookBeforeTool, Tool: "bash"})
	if !res.Blocked {
		t.Fatal("非零退出应拦截")
	}
	if !strings.Contains(res.Message, "禁止执行") {
		t.Fatalf("拦截原因应带上钩子输出，实际 %q", res.Message)
	}
}

func TestBeforeToolPass(t *testing.T) {
	r := &runner{before: []rule{{command: "true"}}}
	if res := r.Run(context.Background(), api.HookEvent{Phase: api.HookBeforeTool, Tool: "bash"}); res.Blocked {
		t.Fatalf("成功退出不应拦截：%+v", res)
	}
}

func TestAfterToolNotes(t *testing.T) {
	r := &runner{after: []rule{
		{command: "echo 检查通过"},
		{command: "true"}, // 无输出，不该产生空行补注
	}}
	res := r.Run(context.Background(), api.HookEvent{Phase: api.HookAfterTool, Tool: "bash"})
	if res.Blocked {
		t.Fatal("after_tool 没有拦截语义")
	}
	if res.Message != "检查通过" {
		t.Fatalf("want 检查通过, got %q", res.Message)
	}
}

func TestMatchFiltersTool(t *testing.T) {
	r := &runner{before: []rule{{match: mustRe(t, "^bash$"), command: "exit 1"}}}
	if res := r.Run(context.Background(), api.HookEvent{Phase: api.HookBeforeTool, Tool: "read_file"}); res.Blocked {
		t.Fatal("不匹配的工具不该被拦")
	}
	if res := r.Run(context.Background(), api.HookEvent{Phase: api.HookBeforeTool, Tool: "bash"}); !res.Blocked {
		t.Fatal("匹配的工具应被拦")
	}
}

func TestTimeout(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("依赖 sh 的 sleep")
	}
	r := &runner{before: []rule{{command: "sleep 2", timeout: 50 * time.Millisecond}}}
	res := r.Run(context.Background(), api.HookEvent{Phase: api.HookBeforeTool, Tool: "bash"})
	if !res.Blocked || !strings.Contains(res.Message, "超时") {
		t.Fatalf("超时应拦截并说明，实际 %+v", res)
	}
}

// TestEnvFieldSmallStaysInline 小字段直接进环境变量，不产生临时文件。
func TestEnvFieldSmallStaysInline(t *testing.T) {
	env, cleanup := envField("GSH_ARGS", "short")
	defer cleanup()
	if env != "GSH_ARGS=short" {
		t.Fatalf("小字段应内联，实际 %q", env)
	}
}

// TestEnvFieldLargeGoesToFile 大字段改走临时文件，路径能读到完整内容。
func TestEnvFieldLargeGoesToFile(t *testing.T) {
	big := strings.Repeat("中", envMax) // 3 字节/字，远超内联阈值
	env, cleanup := envField("GSH_ARGS", big)
	defer cleanup()

	const prefix = "GSH_ARGS_FILE="
	if !strings.HasPrefix(env, prefix) {
		t.Fatalf("大字段应改为文件路径，实际前缀 %q", env[:min(len(env), 32)])
	}
	p := strings.TrimPrefix(env, prefix)
	got, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("读临时文件失败: %v", err)
	}
	if string(got) != big {
		t.Fatalf("临时文件内容被截断：长度 %d，期望 %d", len(got), len(big))
	}
}

// TestLargePayloadDoesNotFailStart 大参数不能让钩子起不来。
//
// Windows 的环境块上限是 32KB：把整段工具结果塞进 GSH_RESULT 时，
// 子进程会直接创建失败，报的还是看不出原因的「环境过大」。
func TestLargePayloadDoesNotFailStart(t *testing.T) {
	big := strings.Repeat("中", 20000) // 60KB
	h := rule{command: "exit 0"}
	if _, ok := runOne(context.Background(), h, api.HookEvent{
		Phase: api.HookAfterTool, Tool: "bash", Args: big, Result: big,
	}); !ok {
		t.Fatal("大参数不该导致钩子启动失败")
	}
}

// TestNoteIsClipped 钩子补注必须截断在字符边界上。
// 它直接进模型上下文，一条输出大文件的规则能把每轮请求都撑大几十 KB。
func TestNoteIsClipped(t *testing.T) {
	s := strings.Repeat("中", noteMax) // 3 字节/字，远超上限
	got := clipNote(s)
	if len(got) > noteMax+64 {
		t.Fatalf("截断后仍超长: %d", len(got))
	}
	if !utf8.ValidString(got) {
		t.Fatal("截断破坏了 UTF-8")
	}
	if !strings.Contains(got, "已截断") {
		t.Fatal("应带截断标记")
	}
	if short := clipNote("ok"); short != "ok" {
		t.Fatalf("短内容不该被改动: %q", short)
	}
}
