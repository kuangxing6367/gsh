package loop

import (
	"context"
	"errors"
	"strings"
	"testing"

	"gsh/internal/api"
)

type memSession struct {
	msgs     []api.Message
	replaced int
}

func (s *memSession) ID() string { return "test" }
func (s *memSession) Append(m api.Message) error {
	s.msgs = append(s.msgs, m)
	return nil
}
func (s *memSession) Messages() []api.Message {
	return append([]api.Message(nil), s.msgs...)
}
func (s *memSession) Replace(m []api.Message) error {
	s.replaced++
	s.msgs = append([]api.Message(nil), m...)
	return nil
}

type stubLLM struct {
	reply string
	err   error
	// 捕获摘要请求，确认摘要调用不带工具、不触发工具调用
	lastMsgs  []api.Message
	lastTools []api.ToolSpec
}

func (s *stubLLM) Chat(_ context.Context, msgs []api.Message, tools []api.ToolSpec, _ api.StreamHandlers) (api.Message, error) {
	s.lastMsgs = msgs
	s.lastTools = tools
	if s.err != nil {
		return api.Message{}, s.err
	}
	return api.Message{Role: api.RoleAssistant, Content: s.reply}, nil
}

// longHistory 造一段带完整工具调用配对的多轮历史。
func longHistory() []api.Message {
	return []api.Message{
		{Role: api.RoleUser, Content: "q1 " + strings.Repeat("旧内容", 300)},
		{Role: api.RoleAssistant, Content: "", ToolCalls: []api.ToolCall{{ID: "c1", Name: "read_file", Args: `{"path":"a"}`}}},
		{Role: api.RoleTool, ToolCallID: "c1", Name: "read_file", Content: "文件内容"},
		{Role: api.RoleAssistant, Content: "a1"},
		{Role: api.RoleUser, Content: "q2 " + strings.Repeat("旧内容", 300)},
		{Role: api.RoleAssistant, Content: "a2"},
		{Role: api.RoleUser, Content: "q3"},
		{Role: api.RoleAssistant, Content: "", ToolCalls: []api.ToolCall{{ID: "c3", Name: "bash", Args: `{"command":"ls"}`}}},
		{Role: api.RoleTool, ToolCallID: "c3", Name: "bash", Content: "ok"},
		{Role: api.RoleAssistant, Content: "a3"},
		{Role: api.RoleUser, Content: "q4"},
		{Role: api.RoleAssistant, Content: "a4"},
	}
}

// assertToolPairs 检查保留区没有悬空的 tool_call。
func assertToolPairs(t *testing.T, msgs []api.Message) {
	t.Helper()
	for i, m := range msgs {
		if len(m.ToolCalls) == 0 {
			continue
		}
		for _, tc := range m.ToolCalls {
			found := false
			for j := i + 1; j < len(msgs) && msgs[j].Role == api.RoleTool; j++ {
				if msgs[j].ToolCallID == tc.ID {
					found = true
					break
				}
			}
			if !found {
				t.Fatalf("第 %d 条 assistant 的 tool_call %s 被截断，没有配对结果", i, tc.ID)
			}
		}
	}
}

func TestCutPointLandsOnUserBoundary(t *testing.T) {
	msgs := longHistory()
	cut := cutPoint(msgs, 2)
	if cut <= 0 || msgs[cut].Role != api.RoleUser {
		t.Fatalf("切点必须落在 user 消息上，实际 index=%d role=%s", cut, msgs[cut].Role)
	}
	if msgs[cut].Content != "q3" {
		t.Fatalf("保留最近 2 轮应从 q3 开始，实际 %q", msgs[cut].Content)
	}
	assertToolPairs(t, msgs[cut:])
}

func TestCompactKeepsToolPairsAndSummarizes(t *testing.T) {
	sess := &memSession{msgs: longHistory()}
	model := &stubLLM{reply: "用户要改 a 文件；已确认用 tab 缩进。"}
	cfg := compactConfig{Enabled: true, MaxChars: 100, KeepTurns: 2, HardCap: 400}

	ok, before, after, summarized := compact(context.Background(), sess, model, model, cfg)
	if !ok || !summarized {
		t.Fatalf("应执行带摘要的压缩: ok=%v summarized=%v", ok, summarized)
	}
	if before != 12 || after >= before {
		t.Fatalf("压缩前后数字必须真实且确实减少: %d → %d", before, after)
	}
	got := sess.Messages()
	if got[0].Role != api.RoleSystem || !strings.Contains(got[0].Content, "tab 缩进") {
		t.Fatalf("首条应为摘要: %+v", got[0])
	}
	assertToolPairs(t, got)
	if model.lastTools != nil {
		t.Fatal("生成摘要时不能把工具集传给模型")
	}
	if !strings.Contains(model.lastMsgs[0].Content, "压缩成一份紧凑的上下文摘要") {
		t.Fatal("摘要请求缺少指令")
	}
}

func TestCompactFallsBackToTruncationWhenSummaryFails(t *testing.T) {
	sess := &memSession{msgs: longHistory()}
	model := &stubLLM{err: errors.New("上游 503")}
	cfg := compactConfig{Enabled: true, MaxChars: 100, KeepTurns: 2, HardCap: 400}

	ok, _, _, summarized := compact(context.Background(), sess, model, model, cfg)
	if !ok {
		t.Fatal("摘要失败也必须完成纯裁剪，不能阻断会话")
	}
	if summarized {
		t.Fatal("摘要失败时 summarized 应为 false")
	}
	got := sess.Messages()
	if len(got) == 0 || got[0].Role != api.RoleUser {
		t.Fatalf("降级后首条应是最近的 user 消息，实际 %+v", got[0])
	}
	assertToolPairs(t, got)
}

func TestNeedsCompactRespectsSwitchAndThreshold(t *testing.T) {
	msgs := longHistory()
	cfg := compactConfig{Enabled: false, MaxChars: 1, KeepTurns: 2, HardCap: 3}
	if needsCompact(msgs, cfg, 0) {
		t.Fatal("关闭后不应触发")
	}
	cfg.Enabled = true
	cfg.MaxChars = 1 << 20
	cfg.HardCap = 1 << 20
	if needsCompact(msgs, cfg, 0) {
		t.Fatal("未超阈值不应触发")
	}
	cfg.MaxChars = 10
	if !needsCompact(msgs, cfg, 0) {
		t.Fatal("超字符阈值应触发")
	}
}

func TestRenderForSummarySkipsRawToolOutput(t *testing.T) {
	out := renderForSummary(longHistory(), 1<<20)
	if strings.Contains(out, "文件内容") || strings.Contains(out, `"command":"ls"`) {
		t.Fatal("摘要输入不应包含工具原始输出与参数")
	}
	if !strings.Contains(out, "调用工具: bash") {
		t.Fatal("摘要输入应保留「调用过什么工具」这一事实")
	}
}
