// gsh — "Go shell harness"：DSH（DeepSeek Harness）思想的 Go 重写。
// 一切皆插件：kernel 装载 session / tools / llm / loop / ui，
// config.json 声明启用哪些插件与各自配置（bundle / profile 形式）。
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"gsh/harness"
	"gsh/internal/kernel"
)

const version = "0.1.0"

func main() {
	var (
		cfgPath  = flag.String("c", "", "config path (default ./config.json)")
		headless = flag.String("e", "", "execute one prompt then exit (headless)")
		webMode  = flag.Bool("web", false, "start web UI server")
		webAddr  = flag.String("addr", ":8934", "web listen address (with -web)")
		tuiMode  = flag.Bool("tui", false, "start opencode-style terminal UI")
		showVer  = flag.Bool("v", false, "print version and loaded plugins")
		verbose  = flag.Bool("verbose", false, "print loaded plugins")
		listTool = flag.Bool("tools", false, "print resolved shell / enabled tools / skills then exit")
		listSessions = flag.Bool("ls", false, "list sessions and exit")
		sessionID    = flag.String("s", "", "resume session id (enter TUI to continue)")
		forkID       = flag.String("fork", "", "fork session id then resume")
		replayID     = flag.String("replay", "", "replay session trajectory and exit")
		viewID       = flag.String("view", "", "open session id in read-only browse mode (TUI)")
		webShare     = flag.Bool("w", false, "with -replay: print web share link instead of plain text")
		webHost      = flag.String("web-host", "gsh.zgric.top", "base host for -replay -w share link (override if gsh web UI is hosted elsewhere)")
	)
	flag.Parse()
	if *showVer && *headless == "" {
		fmt.Println("gsh", version)
		if *verbose {
			return
		}
	}

	cfg, err := loadConfig(*cfgPath)
	if err != nil {
		fatal(err)
	}

	h := harness.New(cfg)
	defer h.Shutdown()
	if err := h.LoadDefaults(); err != nil {
		fatal(err)
	}
	if *verbose {
		fmt.Println("plugins:", stringsJoin(h.LoadedPlugins(), ", "))
	}
	if *listTool {
		info, err := harness.Use[func() string](h, "toolInfo")
		if err != nil {
			fatal(err)
		}
		fmt.Print(info())
		return
	}

	ctx := context.Background()
	wantSession := *sessionID != "" || *forkID != ""
	switch {
	case *listSessions:
		listSessionsOut(h)
	case *replayID != "":
		if *webShare {
			// 分享链接指向 gsh Web UI 的只读轨迹视图（#/share/<id>）。
			// 域名默认 gsh.zgric.top；若 Web UI 部署在其他域名，用 -web-host 覆盖。
			fmt.Printf("分享链接：https://%s/#/share/%s\n", *webHost, *replayID)
			return
		}
		out, err := h.ReplaySession(*replayID)
		if err != nil {
			fatal(err)
		}
		fmt.Println(out)
	case *webMode:
		fmt.Printf("gsh web UI → http://localhost%s\n", *webAddr)
		if err := h.Serve(*webAddr); err != nil {
			fatal(err)
		}
	case *viewID != "":
		if err := h.RunTUIReadOnly(*viewID); err != nil {
			fatal(err)
		}
	case *tuiMode:
		runTUI(h, wantSession, *sessionID, *forkID)
	case *headless != "":
		id := resolveSessionID(h, *sessionID, *forkID)
		if err := h.RunHeadlessWithSession(ctx, *headless, id); err != nil {
			fatal(err)
		}
	default:
		if wantSession {
			// 只给了 -s / --fork（没显式 -tui）：进 TUI 回放历史并继续。
			id := resolveSessionID(h, *sessionID, *forkID)
			if err := h.RunTUIWithSession(id); err != nil {
				fatal(err)
			}
		} else if err := h.RunREPL(ctx); err != nil {
			fatal(err)
		}
	}
}

func loadConfig(explicit string) (*kernel.Config, error) {
	path := explicit
	if path == "" {
		path = filepath.Join(".", "config.json")
	}
	cfg := &kernel.Config{DataDir: filepath.Join(".", ".gsh")}
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) && explicit == "" {
			return cfg, nil // 无配置文件用默认值
		}
		return nil, fmt.Errorf("read config: %w", err)
	}
	if err := json.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("parse config %s: %w", path, err)
	}
	if abs, err := filepath.Abs(path); err == nil {
		cfg.Path = abs
	} else {
		cfg.Path = path
	}
	return cfg, nil
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "gsh:", err)
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

// listSessionsOut 打印会话列表（id / 时间 / 条数 / 标题）。
func listSessionsOut(h *harness.Harness) {
	list, err := h.ListSessions()
	if err != nil {
		fatal(err)
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
func runTUI(h *harness.Harness, wantSession bool, sessionID, forkID string) {
	if wantSession {
		id := resolveSessionID(h, sessionID, forkID)
		if err := h.RunTUIWithSession(id); err != nil {
			fatal(err)
		}
	} else {
		if err := h.RunTUI(); err != nil {
			fatal(err)
		}
	}
}

// resolveSessionID 处理 -s / --fork：fork 优先，派生新会话后返回其 id。
func resolveSessionID(h *harness.Harness, sessionID, forkID string) string {
	if forkID != "" {
		id, err := h.ForkSession(forkID)
		if err != nil {
			fatal(err)
		}
		fmt.Printf("forked → %s\n", id)
		return id
	}
	return sessionID
}
