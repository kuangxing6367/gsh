package tools

import (
	"testing"

	"gsh/internal/kernel"
)

// probePlugin 只是拿一个合法的 Ctx 来构造 permSwitch，
// 不改内核、不装载工具——测的是档位切换本身的语义。
type probePlugin struct{ fn func(*kernel.Ctx) error }

func (p probePlugin) Name() string              { return "probe" }
func (p probePlugin) Apply(c *kernel.Ctx) error { return p.fn(c) }

// TestPlanModeIsReadOnlyPlusFlag 计划模式必须同时满足三件事：
// 底层权限降到只读、运行时打出 planMode、对外报的档位名是 plan。
// 少了任何一条，界面与模型的行为就会对不上。
func TestPlanModeIsReadOnlyPlusFlag(t *testing.T) {
	k := kernel.New(&kernel.Config{})
	err := k.Load(probePlugin{fn: func(c *kernel.Ctx) error {
		g := NewGuard(PermStandard)
		ps := &permSwitch{guard: g, ctx: c}

		if err := ps.SetMode("plan"); err != nil {
			t.Fatalf("切到 plan 失败: %v", err)
		}
		if g.Mode() != PermReadOnly {
			t.Fatalf("plan 的底层权限应是只读，实际 %v", g.Mode())
		}
		if v, _ := c.Runtime("planMode").(bool); !v {
			t.Fatal("planMode 标记必须为 true，否则提示词不会要求先出方案")
		}
		if got := ps.Mode(); got != "plan" {
			t.Fatalf("对外档位名应为 plan，实际 %q", got)
		}

		// 切回去时标记必须清掉，否则模型会一直以为自己在计划模式。
		if err := ps.SetMode("full"); err != nil {
			t.Fatalf("切到 full 失败: %v", err)
		}
		if v, _ := c.Runtime("planMode").(bool); v {
			t.Fatal("离开 plan 后 planMode 必须为 false")
		}
		if got := ps.Mode(); got != "full" {
			t.Fatalf("应为 full，实际 %q", got)
		}
		return nil
	}})
	if err != nil {
		t.Fatal(err)
	}
}
