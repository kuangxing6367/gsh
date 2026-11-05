package codeindex

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"gsh/internal/api"
)

// SearchTool 查询索引：给名字，回文件与行号。
type SearchTool struct{ idx *index }

// Spec 实现 api.Tool。
func (t *SearchTool) Spec() api.ToolSpec {
	return api.ToolSpec{
		Name:     "code_search",
		ReadOnly: true,
		Description: "在代码库索引里按符号名或文件路径查找定义位置，返回文件、行号与定义类型。" +
			"适合回答「某个函数/类/组件在哪」。索引只含签名，读到候选文件后仍需 read_file 看实现。",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"query": map[string]any{
					"type":        "string",
					"description": "符号名或路径片段，大小写不敏感",
				},
				"limit": map[string]any{
					"type":        "integer",
					"description": "最多返回多少个文件，默认 12",
				},
			},
			"required": []string{"query"},
		},
	}
}

// Run 实现 api.Tool。
func (t *SearchTool) Run(_ context.Context, argsJSON string) (string, error) {
	var a struct {
		Query string `json:"query"`
		Limit int    `json:"limit"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &a); err != nil {
		return "", fmt.Errorf("参数解析失败: %w", err)
	}
	if strings.TrimSpace(a.Query) == "" {
		return "", fmt.Errorf("query 不能为空")
	}
	if a.Limit <= 0 {
		a.Limit = 12
	}
	// 查询前先做一次节流过期检查：长会话里刚写的文件，
	// 不重建索引就永远查不到，而工具会明确回答「没有」。
	t.idx.maybeRefresh(false)
	files, syms := t.idx.Stats()
	hits := t.idx.Search(a.Query, a.Limit)
	// 查不到时强制复查一次。索引落后 30 秒就回一句「没有这个符号」，
	// 比返回旧结果更容易把模型带偏——它可能因此改用 grep 全盘乱找，
	// 或者直接断言「项目里没有这个功能」。
	if len(hits) == 0 {
		t.idx.maybeRefresh(true)
		files, syms = t.idx.Stats()
		hits = t.idx.Search(a.Query, a.Limit)
	}
	if len(hits) == 0 {
		return fmt.Sprintf("索引里没有匹配 %q 的条目（当前收录 %d 个文件、%d 个定义）。\n"+
			"可以改用 grep 全文搜索，或先确认符号名拼写。", a.Query, files, syms), nil
	}
	var b strings.Builder
	fmt.Fprintf(&b, "命中 %d 个文件（索引共 %d 文件 / %d 定义）：\n", len(hits), files, syms)
	for _, e := range hits {
		fmt.Fprintf(&b, "\n%s", e.Path)
		if e.Lines > 0 {
			fmt.Fprintf(&b, "  [%s, %d 行]", e.Lang, e.Lines)
		}
		b.WriteString("\n")
		for i, s := range e.Symbols {
			if i >= 8 {
				fmt.Fprintf(&b, "  … 另有 %d 个定义\n", len(e.Symbols)-i)
				break
			}
			kind := s.Kind
			if kind == "" {
				kind = "def"
			}
			fmt.Fprintf(&b, "  L%d  %s %s\n", s.Line, kind, s.Name)
		}
	}
	return strings.TrimRight(b.String(), "\n"), nil
}
