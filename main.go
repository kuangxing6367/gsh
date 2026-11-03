// gsh — "Go shell harness"：DSH（DeepSeek Harness）思想的 Go 重写。
// 一切皆插件：kernel 装载 session / tools / llm / loop / ui，
// config.json 声明启用哪些插件与各自配置（对齐 DSH 的 bundle/profile）。
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
	switch {
	case *tuiMode:
		if err := h.RunTUI(); err != nil {
			fatal(err)
		}
	case *webMode:
		fmt.Printf("gsh web UI → http://localhost%s\n", *webAddr)
		if err := h.Serve(*webAddr); err != nil {
			fatal(err)
		}
	case *headless != "":
		if err := h.RunHeadless(ctx, *headless); err != nil {
			fatal(err)
		}
	default:
		if err := h.RunREPL(ctx); err != nil {
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
