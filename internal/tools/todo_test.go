package tools

import (
	"context"
	"strings"
	"testing"

	"gsh/internal/api"
)

// TestTodoStatusNormalized 模型偶尔会写 doing / running，必须归一成三档之一，
// 否则界面按状态着色时会漏判。
func TestTodoStatusNormalized(t *testing.T) {
	s := &todoStore{}
	err := s.Set([]api.TodoItem{
		{Text: "读代码", Status: "doing"},
		{Text: "改实现", Status: "completed"},
		{Text: "跑测试", Status: "whatever"},
		{Text: "  ", Status: "pending"}, // 空文本应被丢弃
	})
	if err != nil {
		t.Fatal(err)
	}
	items := s.Items()
	if len(items) != 3 {
		t.Fatalf("应保留 3 项，实际 %d", len(items))
	}
	want := []string{"in_progress", "done", "pending"}
	for i, w := range want {
		if items[i].Status != w {
			t.Fatalf("第 %d 项状态应为 %s，实际 %s", i, w, items[i].Status)
		}
	}
}

// TestTodoItemsAreCopied 外部拿到的是副本，改它不能影响内部状态。
func TestTodoItemsAreCopied(t *testing.T) {
	s := &todoStore{}
	_ = s.Set([]api.TodoItem{{Text: "a", Status: "pending"}})
	got := s.Items()
	got[0].Text = "被外部改了"
	if s.Items()[0].Text != "a" {
		t.Fatal("Items 应返回副本")
	}
}

// TestTodoToolRun 工具返回摘要，并写回清单。
func TestTodoToolRun(t *testing.T) {
	s := &todoStore{}
	tool := &TodoTool{store: s}
	out, err := tool.Run(context.Background(), `{"items":[{"text":"第一步","status":"done"},{"text":"第二步","status":"in_progress"}]}`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "共 2 项") {
		t.Fatalf("返回摘要不含总数: %q", out)
	}
	if !strings.Contains(out, "[x] 第一步") || !strings.Contains(out, "[~] 第二步") {
		t.Fatalf("返回摘要渲染错误: %q", out)
	}
	if len(s.Items()) != 2 {
		t.Fatalf("清单未写回: %d", len(s.Items()))
	}
}

// TestTodoToolRejectsNonArray items 不是数组时应报错，而不是静默清空清单。
func TestTodoToolRejectsNonArray(t *testing.T) {
	tool := &TodoTool{store: &todoStore{}}
	if _, err := tool.Run(context.Background(), `{"items":"oops"}`); err == nil {
		t.Fatal("非数组 items 应报错")
	}
}
