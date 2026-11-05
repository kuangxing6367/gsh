package codeindex

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// fixture 造一个小仓库：Go / Python / 前端各一个文件，外加一个应被忽略的目录。
func fixture(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	write := func(rel, body string) {
		p := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
	write("auth.go", `package auth

type Guard struct{}

func (g *Guard) Check(token string) bool {
	return token != ""
}

func ParseToken(s string) string {
	return s
}
`)
	write("web/app.py", `import os

class Session:
    def login(self, user):
        return user

def verify(token):
    return bool(token)
`)
	write("web/ui.tsx", `export function LoginButton() {
  return null;
}

export interface AuthState {
  token: string
}
`)
	write("node_modules/dep/index.js", "function hiddenDep() {}\n")
	write("static/logo.png", "\x89PNG")
	return root
}

func newIndex(t *testing.T, root string) *index {
	t.Helper()
	x := &index{root: root, path: filepath.Join(t.TempDir(), indexFile)}
	if err := x.Refresh(); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	return x
}

func TestScanAndSymbols(t *testing.T) {
	x := newIndex(t, fixture(t))
	files, syms := x.Stats()
	if files != 3 {
		t.Fatalf("应只收录 3 个源码文件，实际 %d", files)
	}
	if syms < 7 {
		t.Fatalf("符号数偏少：%d", syms)
	}
	for _, e := range x.entries {
		for _, s := range e.Symbols {
			if s.Line <= 0 {
				t.Fatalf("%s 的 %s 行号无效", e.Path, s.Name)
			}
		}
	}
}

func TestSearchRanksSymbolFirst(t *testing.T) {
	x := newIndex(t, fixture(t))
	hits := x.Search("ParseToken", 5)
	if len(hits) == 0 {
		t.Fatal("应命中 ParseToken")
	}
	if hits[0].Path != "auth.go" {
		t.Fatalf("首个命中应是 auth.go，实际 %s", hits[0].Path)
	}
	if len(hits[0].Symbols) != 1 || hits[0].Symbols[0].Name != "ParseToken" {
		t.Fatalf("应只回命中的符号：%+v", hits[0].Symbols)
	}
	if got := hits[0].Symbols[0].Line; got != 9 {
		t.Fatalf("ParseToken 应在第 9 行，实际 %d", got)
	}
}

func TestSearchByPath(t *testing.T) {
	x := newIndex(t, fixture(t))
	hits := x.Search("app.py", 5)
	if len(hits) != 1 || hits[0].Path != "web/app.py" {
		t.Fatalf("按路径查不到：%+v", hits)
	}
}

func TestSearchCaseInsensitiveAndMiss(t *testing.T) {
	x := newIndex(t, fixture(t))
	if len(x.Search("guard", 5)) == 0 {
		t.Fatal("应大小写不敏感")
	}
	if len(x.Search("完全没有这个符号", 5)) != 0 {
		t.Fatal("不存在的符号不该有命中")
	}
	if len(x.Search("   ", 5)) != 0 {
		t.Fatal("空查询应返回空")
	}
}

func TestReloadFromCache(t *testing.T) {
	root := fixture(t)
	dir := t.TempDir()
	path := filepath.Join(dir, indexFile)
	first := &index{root: root, path: path}
	if err := first.Refresh(); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	second := &index{root: root, path: path}
	if err := second.load(); err != nil {
		t.Fatalf("缓存加载失败: %v", err)
	}
	if f, _ := second.Stats(); f != 3 {
		t.Fatalf("缓存条目数不对：%d", f)
	}
	if runtime.GOOS != "windows" {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatalf("stat: %v", err)
		}
		if perm := info.Mode().Perm(); perm != 0o600 {
			t.Fatalf("索引文件权限应为 0600，实际 %o", perm)
		}
	}
	// 换根目录后旧缓存必须失效，否则会拿别的仓库的索引回答。
	other := &index{root: t.TempDir(), path: path}
	if err := other.load(); err == nil {
		t.Fatal("根目录变更后不该复用缓存")
	}
}

func TestToolOutput(t *testing.T) {
	x := newIndex(t, fixture(t))
	tool := &SearchTool{idx: x}
	out, err := tool.Run(context.Background(), `{"query":"login"}`)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.Contains(out, "web/app.py") {
		t.Fatalf("应命中 python 的 login：%s", out)
	}
	if !strings.Contains(out, "L4") {
		t.Fatalf("应带行号：%s", out)
	}
	miss, err := tool.Run(context.Background(), `{"query":"zzz_nothing"}`)
	if err != nil {
		t.Fatalf("空结果不应报错: %v", err)
	}
	if !strings.Contains(miss, "没有匹配") {
		t.Fatalf("空结果应给出提示：%s", miss)
	}
	if _, err := tool.Run(context.Background(), `{"query":""}`); err == nil {
		t.Fatal("空 query 应报错")
	}
}

// TestStaleIndexRebuilds 索引必须能自己发现「磁盘比索引新」。
// 少了这一步，索引会永久停在最后一次建表的状态：之后新增的文件查不到，
// 而 code_search 会明确回答「没有这个符号」——比没有索引更误导。
func TestStaleIndexRebuilds(t *testing.T) {
	dir := fixture(t)
	x := newIndex(t, dir)

	// 建索引之后再落一个新文件，模拟「会话进行中用户又写了代码」。
	later := time.Now().Add(2 * time.Second)
	newFile := filepath.Join(dir, "fresh.go")
	if err := os.WriteFile(newFile, []byte("package p\n\nfunc BrandNewThing() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// mtime 显式推后：部分文件系统时间戳精度只到秒，靠写入时刻不可靠。
	if err := os.Chtimes(newFile, later, later); err != nil {
		t.Fatal(err)
	}

	if hits := x.Search("BrandNewThing", 5); len(hits) != 0 {
		t.Fatalf("重建前不该查到新符号: %v", hits)
	}

	// 清掉节流时间戳，模拟「距上次检查已经过了 30 秒」。
	x.mu.Lock()
	x.checkedAt = time.Time{}
	x.mu.Unlock()
	x.maybeRefresh(false)

	hits := x.Search("BrandNewThing", 5)
	if len(hits) != 1 {
		t.Fatalf("过期索引应被重建并收录新符号，实际 %v", hits)
	}
	if hits[0].Path != "fresh.go" {
		t.Fatalf("命中路径不对: %s", hits[0].Path)
	}
}

// TestFreshIndexSkipsRebuild 磁盘没变化时不该白白重扫。
func TestFreshIndexSkipsRebuild(t *testing.T) {
	x := newIndex(t, fixture(t))
	before := x.builtAt
	x.mu.Lock()
	x.checkedAt = time.Time{}
	x.mu.Unlock()
	x.maybeRefresh(false)
	if !x.builtAt.Equal(before) {
		t.Fatal("磁盘无变化时不应重建索引")
	}
}

// TestToolForcesRefreshOnMiss 查不到时必须强制复查一次。
// 否则启动后的 30 秒节流窗口里新增的文件永远查不到，
// 工具会明确回答「索引里没有」——这种确定性错误的杀伤力比慢一点大得多。
func TestToolForcesRefreshOnMiss(t *testing.T) {
	dir := fixture(t)
	x := newIndex(t, dir)
	tool := &SearchTool{idx: x}

	// 时间戳设在「刚检查过一会儿」：仍在 30 秒常规节流窗口内，
	// 但已越过查不到时的短冷却——这正是用户写完代码再提问的真实间隔。
	x.mu.Lock()
	x.checkedAt = time.Now().Add(-3 * time.Second)
	x.mu.Unlock()

	later := time.Now().Add(2 * time.Second)
	p := filepath.Join(dir, "brandnew.go")
	if err := os.WriteFile(p, []byte("package p\n\nfunc JustWritten() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(p, later, later); err != nil {
		t.Fatal(err)
	}

	out, err := tool.Run(context.Background(), `{"query":"JustWritten"}`)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if strings.Contains(out, "没有匹配") {
		t.Fatalf("节流窗口内新增的符号也必须能查到：%s", out)
	}
	if !strings.Contains(out, "brandnew.go") {
		t.Fatalf("应命中新文件：%s", out)
	}
}

// TestForceRefreshCooldown 连续查不到不该每次都全仓重扫。
//
// 模型经常先猜一个名字再查，猜错是常态。没有冷却的话，一轮对话里
// 几次误猜就是几遍目录遍历，仓库一大就卡在工具调用上。
func TestForceRefreshCooldown(t *testing.T) {
	x := newIndex(t, fixture(t))
	before := x.builtAt

	x.mu.Lock()
	x.checkedAt = time.Now() // 刚检查过
	x.mu.Unlock()

	x.maybeRefresh(true)
	if !x.builtAt.Equal(before) {
		t.Fatal("冷却期内的强制复查应被跳过")
	}
}
