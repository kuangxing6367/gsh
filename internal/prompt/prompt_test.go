package prompt

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gsh/internal/api"
	"gsh/internal/kernel"
)

func TestBuildInjectsToolsAndEnvironment(t *testing.T) {
	ws := t.TempDir()
	b := New(Options{Workspace: ws, BaseDir: ws, Mode: "read_only"})
	got := b.Build([]api.ToolSpec{
		{Name: "read_file", Description: "读文件内容"},
		{Name: "bash", Description: "执行 shell 命令\n第二行应被截掉"},
	})
	for _, want := range []string{
		"# 角色与工作方式", "# 沟通规范", "# 可用工具",
		"read_file", "bash", "执行 shell 命令", ws, "read_only",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("系统提示缺少 %q\n---\n%s", want, got)
		}
	}
	if strings.Contains(got, "第二行应被截掉") {
		t.Fatal("工具描述应只取首行")
	}
}

func TestBuildWithoutToolsIsExplicit(t *testing.T) {
	got := New(Options{Workspace: t.TempDir()}).Build(nil)
	if !strings.Contains(got, "没有启用任何工具") {
		t.Fatal("无工具时应明确告知模型，不能让它凭空猜能力")
	}
}

func TestProjectContextAndPersona(t *testing.T) {
	ws := t.TempDir()
	if err := os.WriteFile(filepath.Join(ws, "AGENTS.md"), []byte("本项目一律用 tab 缩进。"), 0o644); err != nil {
		t.Fatal(err)
	}
	b := New(Options{
		Workspace: ws,
		Config: map[string]any{
			"active":   "review",
			"personas": map[string]any{"review": "只做代码审查，不改文件。"},
			"system":   "额外指令：先给结论。",
		},
	})
	got := b.Build([]api.ToolSpec{{Name: "read_file"}})
	for _, want := range []string{"本项目一律用 tab 缩进", "只做代码审查，不改文件", "额外指令：先给结论"} {
		if !strings.Contains(got, want) {
			t.Fatalf("缺少 %q", want)
		}
	}
	if filepath.Base(b.ProjectContextPath()) != "AGENTS.md" {
		t.Fatalf("项目说明路径不对: %s", b.ProjectContextPath())
	}
}

func TestProjectContextTruncated(t *testing.T) {
	ws := t.TempDir()
	if err := os.WriteFile(filepath.Join(ws, "GSH.md"), []byte(strings.Repeat("说", maxContextBytes+1000)), 0o644); err != nil {
		t.Fatal(err)
	}
	got := New(Options{Workspace: ws}).Build(nil)
	if !strings.Contains(got, "已截断") {
		t.Fatal("超大项目说明必须截断")
	}
	if len([]rune(strings.Split(got, "# 项目约定\n")[1])) > maxContextBytes/2+200 {
		t.Fatal("项目说明没有被有效限量")
	}
}

// TestPlanModeSectionOnlyWhenFlagged 计划模式段落靠运行时标记驱动：
// 标记在时必须出现，标记清掉后必须消失——否则切回标准档模型还以为自己在读方案。
func TestPlanModeSectionOnlyWhenFlagged(t *testing.T) {
	k := kernel.New(&kernel.Config{})
	k.SetValue("planMode", true)

	on := New(Options{Workspace: t.TempDir(), Kernel: k}).Build(nil)
	if !strings.Contains(on, "# 计划模式") {
		t.Fatalf("planMode 为真时应注入计划模式段落\n---\n%s", on)
	}
	for _, want := range []string{"先给出可执行方案", "等用户确认"} {
		if !strings.Contains(on, want) {
			t.Fatalf("计划模式段落缺少 %q", want)
		}
	}

	k.SetValue("planMode", false)
	off := New(Options{Workspace: t.TempDir(), Kernel: k}).Build(nil)
	if strings.Contains(off, "# 计划模式") {
		t.Fatal("planMode 为假时不应出现计划模式段落")
	}
}
