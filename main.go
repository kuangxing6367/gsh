// gsh — "Go shell harness"：一切皆插件。
// kernel 装载 session / tools / llm / prompt / loop / web / ui，
// config.json 声明启用哪些插件与各自配置。
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gsh/harness"
	"gsh/internal/kernel"
	"gsh/internal/logx"
)

// version 由发布脚本用 -ldflags "-X main.version=..." 注入，源码里的值只作开发默认。
var version = "0.2.0"

func main() {
	var (
		cfgPath      = flag.String("c", "", "config path (default: <exe dir>/config.json)")
		headless     = flag.String("e", "", "execute one prompt then exit (headless)")
		webMode      = flag.Bool("web", false, "start web UI server")
		webAddr      = flag.String("addr", ":8934", "web listen address (with -web)")
		tuiMode      = flag.Bool("tui", false, "start terminal UI")
		showVer      = flag.Bool("v", false, "print version, paths and loaded plugins")
		verbose      = flag.Bool("verbose", false, "print loaded plugins")
		listTool     = flag.Bool("tools", false, "print resolved shell / enabled tools / skills then exit")
		listSessions = flag.Bool("ls", false, "list sessions and exit")
		searchQuery  = flag.String("search", "", "search sessions by keyword and exit")
		archiveDays  = flag.Int("archive", 0, "export sessions older than N days to archive markdown and exit (0 = off)")
		archivePrune = flag.Bool("archive-prune", false, "with -archive: delete archived session files (destructive)")
		sessionID    = flag.String("s", "", "resume session id (enter TUI to continue)")
		forkID       = flag.String("fork", "", "fork session id then resume")
		replayID     = flag.String("replay", "", "replay session trajectory and exit")
		viewID       = flag.String("view", "", "open session id in read-only browse mode (TUI)")
		webShare     = flag.Bool("w", false, "with -replay: print web share link instead of plain text")
		webHost      = flag.String("web-host", "", "host for -replay -w share link (default: localhost<addr>)")
		modelName    = flag.String("model", "", "select named model from plugins_cfg.llm.models for this run")
		auxName      = flag.String("aux", "", "select named model used for summarization")
		imagePaths   stringListFlag
	)
	flag.Var(&imagePaths, "image", "attach a local image to -e prompt (repeatable)")
	flag.Parse()

	// 以可执行文件所在目录为锚点：配置、数据、技能、日志都相对它解析，
	// 这样「下载解压 → 双击/命令行运行」在任何当前目录下行为一致。
	baseDir, baseErr := exeDir()
	if baseErr != nil {
		fmt.Fprintf(os.Stderr, "gsh: 警告：无法定位程序目录（%v），改用当前目录\n", baseErr)
		baseDir = "."
	}
	if abs, err := filepath.Abs(baseDir); err == nil {
		baseDir = abs
	}

	cfgFile := *cfgPath
	if cfgFile == "" {
		cfgFile = filepath.Join(baseDir, "config.json")
	}

	cfg, cfgCreated, err := loadConfig(cfgFile, *cfgPath == "")
	if err != nil {
		fatal("", err)
	}
	// data_dir 的相对路径一律相对程序目录，不随当前工作目录漂移。
	if cfg.DataDir == "" {
		cfg.DataDir = filepath.Join(".", ".gsh")
	}
	cfg.DataDir = resolveUnder(baseDir, cfg.DataDir)
	cfg.BaseDir = baseDir
	// 命令行选模型只改本次进程的配置快照，不写回 config.json。
	if *modelName != "" || *auxName != "" {
		if cfg.Options == nil {
			cfg.Options = map[string]map[string]any{}
		}
		if cfg.Options["llm"] == nil {
			cfg.Options["llm"] = map[string]any{}
		}
		if *modelName != "" {
			cfg.Options["llm"]["active"] = *modelName
		}
		if *auxName != "" {
			cfg.Options["llm"]["aux"] = *auxName
		}
	}

	logPath := logx.Setup(filepath.Join(cfg.DataDir, "logs"))
	defer logx.Close()

	// 启动横幅：关键路径同时打到终端与日志，避免「不知道配置/日志在哪」。
	// -v / -verbose 时顺便列出已装载插件后退出（诊断用）。
	if *showVer || *verbose {
		fmt.Printf("gsh %s\n", version)
		fmt.Printf("  程序目录  %s\n", baseDir)
		fmt.Printf("  配置文件  %s%s\n", cfg.Path, createdMark(cfgCreated))
		fmt.Printf("  数据目录  %s\n", cfg.DataDir)
		fmt.Printf("  日志文件  %s\n", orNoneLog(logPath))
		h := harness.New(cfg)
		if err := h.LoadDefaults(); err != nil {
			fatal(logPath, err)
		}
		defer h.Shutdown()
		fmt.Println("  plugins: ", stringsJoin(h.LoadedPlugins(), ", "))
		return
	}

	h := harness.New(cfg)
	defer h.Shutdown()
	if err := h.LoadDefaults(); err != nil {
		fatal(logPath, err)
	}
	log.Printf("gsh %s 启动 | 配置=%s 数据=%s 日志=%s", version, cfg.Path, cfg.DataDir, logPath)

	// key 为空是最常见的「一跑就 401」原因：启动时就说清去哪填，别等报错才猜。
	if keyEmpty(cfg) {
		fmt.Fprintf(os.Stderr, "gsh: 提示：尚未配置 api_key，模型调用会失败。\n"+
			"     请编辑 %s 的 plugins_cfg.llm.api_key 后重试。\n", cfg.Path)
		log.Printf("警告：api_key 为空（配置文件 %s）", cfg.Path)
	}

	if *listTool {
		info, err := harness.Use[func() string](h, "toolInfo")
		if err != nil {
			fatal(logPath, err)
		}
		fmt.Print(info())
		return
	}

	ctx := context.Background()
	wantSession := *sessionID != "" || *forkID != ""
	switch {
	case *listSessions:
		listSessionsOut(h, logPath)
	case *searchQuery != "":
		searchSessionsOut(h, *searchQuery, logPath)
	case *archiveDays > 0:
		archiveOut(h, *archiveDays, *archivePrune, logPath)
	case *replayID != "":
		if *webShare {
			host := *webHost
			if host == "" {
				// 未指定时指向本机 Web 端口，而不是某个固定域名。
				host = withScheme("localhost" + *webAddr)
			}
			fmt.Printf("%s/#/share/%s\n", host, *replayID)
			return
		}
		out, err := h.ReplaySession(*replayID)
		if err != nil {
			fatal(logPath, err)
		}
		fmt.Println(out)
	case *webMode:
		log.Printf("web UI 监听 %s", *webAddr)
		fmt.Printf("gsh web UI → http://localhost%s\n", *webAddr)
		if err := h.Serve(*webAddr); err != nil {
			fatal(logPath, err)
		}
	case *viewID != "":
		if err := h.RunTUIReadOnly(*viewID); err != nil {
			fatal(logPath, err)
		}
	case *tuiMode:
		runTUI(h, wantSession, *sessionID, *forkID, logPath)
	case *headless != "":
		id := resolveSessionID(h, *sessionID, *forkID, logPath)
		if len(imagePaths) > 0 {
			out, err := h.RunWithImages(ctx, *headless, id, imagePaths, nil)
			if err != nil {
				fatal(logPath, err)
			}
			if out != "" {
				fmt.Println(out)
			}
			return
		}
		if err := h.RunHeadlessWithSession(ctx, *headless, id); err != nil {
			fatal(logPath, err)
		}
	default:
		if wantSession {
			id := resolveSessionID(h, *sessionID, *forkID, logPath)
			if err := h.RunTUIWithSession(id); err != nil {
				fatal(logPath, err)
			}
		} else if err := h.RunREPL(ctx); err != nil {
			fatal(logPath, err)
		}
	}
}

// exeDir 返回可执行文件所在目录（解析符号链接后的真实路径）。
func exeDir() (string, error) {
	p, err := os.Executable()
	if err != nil {
		return "", err
	}
	if real, err := filepath.EvalSymlinks(p); err == nil {
		p = real
	}
	return filepath.Dir(p), nil
}

// resolveUnder 把可能是相对路径的 p 解析到 base 之下；绝对路径原样返回。
func resolveUnder(base, p string) string {
	if p == "" {
		return ""
	}
	if filepath.IsAbs(p) {
		return p
	}
	return filepath.Join(base, p)
}

func createdMark(created bool) string {
	if created {
		return "  （首次运行已生成，请填入 api_key）"
	}
	return ""
}

func orNoneLog(p string) string {
	if p == "" {
		return "（无法创建日志文件，仅输出到终端）"
	}
	return p
}

func withScheme(host string) string {
	if len(host) >= 4 && host[:4] == "http" {
		return host
	}
	return "https://" + host
}

// keyEmpty 判断是否没配任何 key。多模型写法下 key 可能放在每个模型节里，
// 也可能作为公共缺省放顶层；env: 前缀也算已配置（实际值在模型层展开）。
func keyEmpty(cfg *kernel.Config) bool {
	if cfg == nil || cfg.Options == nil {
		return true
	}
	llm, ok := cfg.Options["llm"]
	if !ok {
		return true
	}
	// echo 是离线自检用的 mock，本来就不需要 key，不该误报。
	models, hasModels := llm["models"].(map[string]any)
	active, _ := llm["active"].(string)
	if !hasModels || len(models) == 0 {
		if strings.EqualFold(strValue(llm["provider"]), "echo") {
			return false
		}
	} else if m, ok := models[active].(map[string]any); ok {
		p := strValue(m["provider"])
		if p == "" {
			p = strValue(llm["provider"])
		}
		if strings.EqualFold(p, "echo") {
			return false
		}
	}
	if v, _ := llm["api_key"].(string); v != "" {
		return false
	}
	if hasModels {
		for _, raw := range models {
			m, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			if v, _ := m["api_key"].(string); v != "" {
				return false
			}
		}
	}
	return true
}

func strValue(v any) string {
	s, _ := v.(string)
	return s
}

// defaultConfig 首次运行、找不到配置文件时生成的最小可用模板。
// 只写必需项，避免把示例里的占位节点塞给用户造成困惑。
func defaultConfig() *kernel.Config {
	return &kernel.Config{
		Plugins: []string{"session", "tools", "memory", "hooks", "codeindex", "llm", "prompt", "loop", "subagent", "mcp", "web", "ui"},
		Options: map[string]map[string]any{
			"llm": {
				"provider":        "openai",
				"base_url":        "https://api.deepseek.com",
				"model":           "deepseek-chat",
				"api_key":         "",
				"thinking_format": "none",
				"timeout_seconds": 120,
				"params": map[string]any{
					"temperature": 0.7,
					"max_tokens":  4096,
				},
				"thinking": map[string]any{
					"enabled": false,
					"style":   "auto",
				},
				"retry": map[string]any{"max": 2, "base_ms": 800},
			},
			"tools": {
				"permission":           "standard",
				"workspace":            ".",
				"workspace_restrict":   true,
				"shell":                "auto",
				"tool_timeout_seconds": 60,
				"parallel_tools":       true,
				"parallel_limit":       4,
			},
			"memory": {
				"enabled": true,
				"path":    ".gsh/memory/MEMORY.md",
			},
			// 钩子与 MCP 默认留空：没有明确需求的进程不该在启动时去拉子进程。
			"hooks": map[string]any{
				"before_tool": []any{},
				"after_tool":  []any{},
			},
			"subagent":  map[string]any{"max_turns": 12},
			"codeindex": map[string]any{"enabled": true},
			"mcp":       map[string]any{"servers": []any{}},
			"session": {
				"compact_enabled": true,
				"max_chars":       24000,
				"token_ratio":     0.75,
				"keep_turns":      20,
				"hard_cap":        400,
			},
			"prompt": {
				"active":   "",
				"personas": map[string]any{},
				"system":   "",
			},
			"web": {"addr": ":8934"},
		},
	}
}

// loadConfig 读取配置文件。defaultPath=true 表示用户没显式 -c：
// 文件不存在时自动生成一份模板（created=true），而不是悄悄用空配置跑。
func loadConfig(path string, defaultPath bool) (*kernel.Config, bool, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) && defaultPath {
			cfg := defaultConfig()
			cfg.DataDir = filepath.Join(".", ".gsh")
			cfg.Path = path
			if werr := writeConfig(path, cfg); werr != nil {
				// 目录不可写（如装在只读位置）：退回内存默认值，并明确告知。
				fmt.Fprintf(os.Stderr, "gsh: 无法生成配置文件 %s：%v（本次使用内置默认配置）\n", path, werr)
				return cfg, false, nil
			}
			return cfg, true, nil
		}
		return nil, false, fmt.Errorf("读取配置 %s: %w", path, err)
	}
	cfg := &kernel.Config{}
	if err := json.Unmarshal(data, cfg); err != nil {
		return nil, false, fmt.Errorf("解析配置 %s: %w", path, err)
	}
	if abs, err := filepath.Abs(path); err == nil {
		cfg.Path = abs
	} else {
		cfg.Path = path
	}
	return cfg, false, nil
}

// writeConfig 把配置写回磁盘（缩进 JSON，便于手改）。
func writeConfig(path string, cfg *kernel.Config) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	// config.json 含 api_key：仅当前用户可读写。
	return os.WriteFile(path, append(b, '\n'), 0o600)
}

func fatal(logPath string, err error) {
	if err != nil {
		log.Printf("fatal: %v", err)
		fmt.Fprintln(os.Stderr, "gsh:", err)
	}
	os.Exit(1)
}

func stringsJoin(ss []string, sep string) string {
	out := ""
	for i, s := range ss {
		if i > 0 {
			out += sep
		}
		out += s
	}
	return out
}

// searchSessionsOut 打印会话检索结果。
func searchSessionsOut(h *harness.Harness, query, logPath string) {
	hits, err := h.SearchSessions(query, 30)
	if err != nil {
		fatal(logPath, err)
	}
	if len(hits) == 0 {
		fmt.Printf("没有会话命中 %q\n", query)
		return
	}
	for _, x := range hits {
		fmt.Printf("%s  %s  %d 处  %s\n    %s\n", x.ID, x.Updated.Format("2006-01-02 15:04"), x.Count, x.Title, x.Snippet)
	}
}

// listSessionsOut 打印会话列表（id / 时间 / 条数 / 标题）。
func listSessionsOut(h *harness.Harness, logPath string) {
	list, err := h.ListSessions()
	if err != nil {
		fatal(logPath, err)
	}
	if len(list) == 0 {
		fmt.Println("（暂无会话）")
		return
	}
	for _, s := range list {
		fmt.Printf("%s  %s  %d 条  %s\n", s.ID, s.Updated.Format("2006-01-02 15:04"), s.Messages, s.Title)
	}
}

// runTUI 启动终端 UI；有会话修饰时定位到指定会话。
func runTUI(h *harness.Harness, wantSession bool, sessionID, forkID, logPath string) {
	if wantSession {
		id := resolveSessionID(h, sessionID, forkID, logPath)
		if err := h.RunTUIWithSession(id); err != nil {
			fatal(logPath, err)
		}
	} else {
		if err := h.RunTUI(); err != nil {
			fatal(logPath, err)
		}
	}
}

// resolveSessionID 处理 -s / --fork：fork 优先，派生新会话后返回其 id。
func resolveSessionID(h *harness.Harness, sessionID, forkID, logPath string) string {
	if forkID != "" {
		id, err := h.ForkSession(forkID)
		if err != nil {
			fatal(logPath, err)
		}
		fmt.Printf("forked → %s\n", id)
		return id
	}
	return sessionID
}

// nl 是换行常量，避免长字符串被工具链的行处理搞坏。
const nl = "\n"

// archiveOut 打印归档结果。
func archiveOut(h *harness.Harness, days int, prune bool, logPath string) {
	rep, err := h.ArchiveBefore(days, prune)
	if err != nil {
		fatal(logPath, err)
	}
	// 两个计数要分开看：这一轮可能没有新会话要导出，但仍有
	// 「此前已归档、这次补跑 prune」的文件要删。只看 Sessions 会
	// 把后一种情况误报成「什么都不用做」。
	if rep.Sessions == 0 && rep.Pruned == 0 {
		fmt.Printf("没有早于 %d 天的会话需要归档。"+nl, days)
		return
	}
	if rep.Sessions > 0 {
		fmt.Printf("已归档 %d 个会话 → %s"+nl, rep.Sessions, rep.Dir)
		months := make([]string, 0, len(rep.Monthly))
		for m := range rep.Monthly {
			months = append(months, m)
		}
		sort.Strings(months)
		for _, m := range months {
			fmt.Printf("  %s.md  %d 个"+nl, m, rep.Monthly[m])
		}
	}
	if prune {
		fmt.Printf("已删除 %d 个会话文件。"+nl, rep.Pruned)
	}
}

// stringListFlag 是可重复的字符串参数（-image a.png -image b.png）。
type stringListFlag []string

// String 实现 flag.Value。
func (s *stringListFlag) String() string { return strings.Join(*s, ",") }

// Set 实现 flag.Value。
func (s *stringListFlag) Set(v string) error {
	*s = append(*s, v)
	return nil
}
