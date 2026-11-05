// Package codeindex 维护一份代码库符号索引。
//
// 目的是把「找东西」从逐文件 grep 变成一次查表：模型问「鉴权在哪实现」，
// 先查索引拿到候选文件与定义行号，再 read_file 精读几处。
// 索引只做定位，不替代读文件——索引里永远只有签名，没有实现。
package codeindex

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"gsh/internal/api"
	"gsh/internal/kernel"
)

// 规模上限。索引是给「一个人能读完的仓库」用的；
// 超过这个量级说明该上真正的语言服务器，硬扫只会拖慢启动。
const (
	maxFiles     = 5000
	maxFileBytes = 512 * 1024
	maxPerFile   = 200
	indexFile    = "index.json"
)

// skipDirs 是明确不该进索引的目录。
var skipDirs = map[string]bool{
	".git": true, ".hg": true, ".svn": true, "node_modules": true,
	"vendor": true, "dist": true, "build": true, "target": true,
	".venv": true, "venv": true, "__pycache__": true, ".idea": true,
	".vscode": true, ".next": true, ".cache": true,
}

// langs 是可解析符号的扩展名到语言名。
var langs = map[string]string{
	".go": "go", ".py": "python", ".js": "javascript", ".mjs": "javascript",
	".cjs": "javascript", ".ts": "typescript", ".tsx": "typescript",
	".jsx": "javascript", ".rs": "rust", ".java": "java", ".kt": "kotlin",
	".rb": "ruby", ".php": "php", ".c": "c", ".h": "c", ".cc": "cpp",
	".cpp": "cpp", ".hpp": "cpp", ".cs": "csharp", ".sh": "shell",
	".lua": "lua", ".swift": "swift", ".sql": "sql",
}

// 各语言的顶层定义。只认行首（允许缩进，兼容类内方法），
// 不做语法分析——正则漏掉的边角，比引入一棵解析树划算得多。
var patterns = map[string][]struct {
	re   *regexp.Regexp
	kind string
}{
	"go": {
		{regexp.MustCompile(`^func\s+(?:\([^)]*\)\s*)?([A-Za-z_]\w*)`), "func"},
		{regexp.MustCompile(`^type\s+([A-Za-z_]\w*)\s+(?:struct|interface)?`), "type"},
		{regexp.MustCompile(`^(?:var|const)\s+([A-Za-z_]\w*)`), "var"},
	},
	"python": {
		{regexp.MustCompile(`^\s*class\s+([A-Za-z_]\w*)`), "class"},
		{regexp.MustCompile(`^\s*(?:async\s+)?def\s+([A-Za-z_]\w*)`), "func"},
	},
	"javascript": {
		{regexp.MustCompile(`^\s*(?:export\s+)?(?:default\s+)?(?:async\s+)?function\s+([A-Za-z_$][\w$]*)`), "func"},
		{regexp.MustCompile(`^\s*(?:export\s+)?class\s+([A-Za-z_$][\w$]*)`), "class"},
		{regexp.MustCompile(`^\s*(?:export\s+)?(?:const|let|var)\s+([A-Za-z_$][\w$]*)\s*=`), "var"},
	},
	"typescript": {
		{regexp.MustCompile(`^\s*(?:export\s+)?(?:default\s+)?(?:async\s+)?function\s+([A-Za-z_$][\w$]*)`), "func"},
		{regexp.MustCompile(`^\s*(?:export\s+)?(?:abstract\s+)?class\s+([A-Za-z_$][\w$]*)`), "class"},
		{regexp.MustCompile(`^\s*(?:export\s+)?(?:interface|type|enum)\s+([A-Za-z_$][\w$]*)`), "type"},
		{regexp.MustCompile(`^\s*(?:export\s+)?(?:const|let|var)\s+([A-Za-z_$][\w$]*)\s*[=:]`), "var"},
	},
	"rust": {
		{regexp.MustCompile(`^\s*(?:pub\s+)?(?:async\s+)?fn\s+(\w+)`), "func"},
		{regexp.MustCompile(`^\s*(?:pub\s+)?(?:struct|enum|trait)\s+(\w+)`), "type"},
	},
	"java": {
		{regexp.MustCompile(`^\s*(?:public|private|protected)?\s*(?:static\s+)?(?:final\s+)?class\s+(\w+)`), "class"},
		{regexp.MustCompile(`^\s*(?:public|private|protected)\s+[\w<>\[\]]+\s+(\w+)\s*\(`), "func"},
	},
}

// RegisterKernel 挂载索引插件。必须排在 tools 之后：它要读工作区根目录。
func RegisterKernel(k *kernel.Kernel) error { return k.Load(&Plugin{}) }

// Plugin 索引插件。
type Plugin struct{}

// Name 实现 kernel.Plugin。
func (p *Plugin) Name() string { return "codeindex" }

// Apply 加载或构建索引，并注册查询工具。
func (p *Plugin) Apply(ctx *kernel.Ctx) error {
	root := ""
	if ws, err := kernel.Use[api.WorkspaceInfo](kernel.Current, "workspace"); err == nil && ws != nil {
		root = ws.Root()
	}
	if root == "" {
		root = ctx.BaseDir()
	}
	idx := &index{root: root, path: filepath.Join(ctx.DataDir(), indexFile)}
	// 启动时只尝试读缓存，不主动扫盘：冷启动多花 3 秒扫仓库，
	// 用户第一句话的等待时间会变成不可解释的卡顿。
	// 缓存命中后仍需在后台确认它是否过期——否则新增的文件永远进不来。
	if err := idx.load(); err != nil {
		_ = idx.Refresh()
	} else {
		go idx.maybeRefresh(false)
	}
	ctx.Provide("codeIndex", api.CodeIndex(idx))
	if reg, err := kernel.Use[api.ToolRegistry](kernel.Current, "toolRegistry"); err == nil && reg != nil {
		return reg.Register(&SearchTool{idx: idx})
	}
	return nil
}

// index 是索引本体，读写都加锁：查询来自工具调用，重建可能来自命令，
// 两者不在同一个 goroutine。
type index struct {
	mu      sync.RWMutex
	root    string
	path    string
	entries []api.IndexEntry
	// builtAt 是建索引时的时间戳。用来判断磁盘上的文件是否比索引新——
	// 少了这个判断，索引会永久停留在最后一次建表的状态：
	// 之后新增的文件永远查不到，而 code_search 会明确回答「没有这个符号」，
	// 比没有索引更误导。
	builtAt time.Time
	// checkedAt 是上次做过期检查的时间，避免每次查询都 walk 一遍目录。
	checkedAt time.Time
}

// Search 实现 api.CodeIndex：先比符号，再比路径。
func (x *index) Search(query string, limit int) []api.IndexEntry {
	q := strings.ToLower(strings.TrimSpace(query))
	if q == "" {
		return nil
	}
	if limit <= 0 {
		limit = 20
	}
	type scored struct {
		e     api.IndexEntry
		score int
	}
	var hits []scored
	x.mu.RLock()
	defer x.mu.RUnlock()
	for _, e := range x.entries {
		best := 0
		lowerPath := strings.ToLower(e.Path)
		switch {
		case strings.Contains(lowerPath, q):
			best = 10
		}
		var matched []api.Symbol
		for _, s := range e.Symbols {
			ls := strings.ToLower(s.Name)
			switch {
			case ls == q:
				best = max(best, 100)
			case strings.HasPrefix(ls, q):
				best = max(best, 60)
			case strings.Contains(ls, q):
				best = max(best, 30)
			default:
				continue
			}
			matched = append(matched, s)
		}
		if best == 0 {
			continue
		}
		// 只回命中的符号，避免一个文件带出上百条无关定义。
		out := e
		if len(matched) > 0 {
			out.Symbols = matched
		}
		hits = append(hits, scored{e: out, score: best})
	}
	sort.SliceStable(hits, func(i, j int) bool {
		if hits[i].score != hits[j].score {
			return hits[i].score > hits[j].score
		}
		return hits[i].e.Path < hits[j].e.Path
	})
	if len(hits) > limit {
		hits = hits[:limit]
	}
	out := make([]api.IndexEntry, 0, len(hits))
	for _, h := range hits {
		out = append(out, h.e)
	}
	return out
}

// Stats 实现 api.CodeIndex。
func (x *index) Stats() (int, int) {
	x.mu.RLock()
	defer x.mu.RUnlock()
	n := 0
	for _, e := range x.entries {
		n += len(e.Symbols)
	}
	return len(x.entries), n
}

// Root 实现 api.CodeIndex。
func (x *index) Root() string { return x.root }

// Refresh 实现 api.CodeIndex：重新扫盘并落盘。
//
// builtAt 取扫描**开始前**的时间：扫描期间被改动的文件下轮仍会被判为更新，
// 宁可多扫一次，也不要让一次并发写入的改动被当成「已经收录」。
func (x *index) Refresh() error {
	start := time.Now()
	entries := scan(x.root)
	x.mu.Lock()
	x.entries = entries
	x.builtAt = start
	x.checkedAt = time.Now()
	x.mu.Unlock()
	return x.save()
}

// load 读缓存。根目录变了就视为失效，避免拿着旧仓库的索引答新问题。
func (x *index) load() error {
	data, err := os.ReadFile(x.path)
	if err != nil {
		return err
	}
	var f struct {
		Root    string           `json:"root"`
		BuiltAt time.Time        `json:"built_at"`
		Entries []api.IndexEntry `json:"entries"`
	}
	if err := json.Unmarshal(data, &f); err != nil {
		return err
	}
	if f.Root != x.root || len(f.Entries) == 0 {
		return os.ErrNotExist
	}
	x.mu.Lock()
	x.entries = f.Entries
	x.builtAt = f.BuiltAt
	x.mu.Unlock()
	return nil
}

// save 原子落盘：临时文件 + rename，半截索引比没有索引更误导。
func (x *index) save() error {
	x.mu.RLock()
	f := struct {
		Root    string           `json:"root"`
		BuiltAt time.Time        `json:"built_at"`
		Entries []api.IndexEntry `json:"entries"`
	}{Root: x.root, BuiltAt: x.builtAt, Entries: x.entries}
	x.mu.RUnlock()
	data, err := json.Marshal(f)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(x.path), 0o700); err != nil {
		return err
	}
	tmp := x.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, x.path)
}

// checkEvery 过期检查的最小间隔。检查本身也要 walk 目录（只 stat 不读内容），
// 每次查询都做一遍没必要；30 秒足够让「刚写的文件」在下一轮对话里可见。
const checkEvery = 30 * time.Second

// forceCheckEvery 强制复查的最小间隔。
//
// 查不到时重扫是有必要的（刚写的文件可能没进索引），但「符号真的不存在」
// 同样常见——模型经常先猜个名字再查。没有冷却的话，一次对话里连猜几个
// 错名字就是几遍全仓扫描。半秒足够让「写完再问」及时看到，也挡得住
// 同一轮里的连续误猜。
const forceCheckEvery = 500 * time.Millisecond

// maybeRefresh 在索引落后于磁盘时重建。
//
// force 为真时跳过常规节流，立刻检查一次——用在「查不到」的路径上：
// 返回「没有这个符号」比返回旧结果更容易误导人。但保留一个更短的冷却
// 窗口，避免连续误猜把 CPU 打满。
func (x *index) maybeRefresh(force bool) {
	x.mu.RLock()
	builtAt, checkedAt := x.builtAt, x.checkedAt
	x.mu.RUnlock()
	if force {
		if time.Since(checkedAt) < forceCheckEvery {
			return
		}
	} else if time.Since(checkedAt) < checkEvery {
		return
	}
	x.mu.Lock()
	// 双检：另一路已经在这期间检查过了。
	gap := checkEvery
	if force {
		gap = forceCheckEvery
	}
	if time.Since(x.checkedAt) < gap {
		x.mu.Unlock()
		return
	}
	x.checkedAt = time.Now()
	x.mu.Unlock()

	// 没有 builtAt（老版本写出的索引文件）直接重建一次。
	if builtAt.IsZero() || latestMTime(x.root, builtAt) {
		_ = x.Refresh()
	}
}

// latestMTime 判断仓库里是否有文件比 ts 更新。只 stat、不读内容，
// 成本远低于重建；一旦发现更新的文件就立刻返回，不遍历完整棵树。
func latestMTime(root string, ts time.Time) bool {
	found := false
	filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if skipDirs[d.Name()] || (strings.HasPrefix(d.Name(), ".") && d.Name() != ".") {
				return filepath.SkipDir
			}
			return nil
		}
		if _, ok := langs[strings.ToLower(filepath.Ext(path))]; !ok {
			return nil
		}
		info, ierr := d.Info()
		if ierr != nil {
			return nil
		}
		if info.ModTime().After(ts) {
			found = true
			return filepath.SkipAll
		}
		return nil
	})
	return found
}

// scan 遍历仓库，产出索引条目。
func scan(root string) []api.IndexEntry {
	var out []api.IndexEntry
	filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil // 权限不足的目录直接跳过，不中断整次扫描
		}
		if d.IsDir() {
			if skipDirs[d.Name()] || strings.HasPrefix(d.Name(), ".") && d.Name() != "." {
				return filepath.SkipDir
			}
			return nil
		}
		if len(out) >= maxFiles {
			return filepath.SkipAll
		}
		lang, ok := langs[strings.ToLower(filepath.Ext(path))]
		if !ok {
			return nil
		}
		info, ierr := d.Info()
		if ierr != nil || info.Size() > maxFileBytes {
			return nil
		}
		rel, rerr := filepath.Rel(root, path)
		if rerr != nil {
			rel = path
		}
		e := api.IndexEntry{Path: filepath.ToSlash(rel), Lang: lang}
		e.Lines, e.Symbols = parseFile(path, lang)
		out = append(out, e)
		return nil
	})
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}

// parseFile 逐行抽取定义。只读一遍文件，行号自然就是行号。
func parseFile(path, lang string) (int, []api.Symbol) {
	f, err := os.Open(path)
	if err != nil {
		return 0, nil
	}
	defer f.Close()
	pats := patterns[lang]
	var syms []api.Symbol
	line := 0
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line++
		if len(pats) == 0 || len(syms) >= maxPerFile {
			continue
		}
		text := sc.Text()
		for _, p := range pats {
			m := p.re.FindStringSubmatch(text)
			if m == nil || len(m) < 2 {
				continue
			}
			syms = append(syms, api.Symbol{Name: m[1], Line: line, Kind: p.kind})
			break // 一行只记一个定义，避免 var 与 func 重复命中
		}
	}
	return line, syms
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
