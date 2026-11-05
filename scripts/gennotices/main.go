// 生成 THIRD-PARTY-NOTICES.md。
//
// 发布产物是静态链接的（CGO_ENABLED=0），依赖的版权声明必须随二进制一起分发，
// 否则那些 MIT / BSD 许可里的 "shall be included in all copies" 就没有落实。
// 这里把实际链接进程序的模块（go list -deps，而不是 go list -m all——
// 后者含 test-only 依赖）连同许可原文一起抓出来。
//
// 用法：go run ./scripts/gennotices > THIRD-PARTY-NOTICES.md
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

type module struct {
	Path    string
	Version string
	Dir     string
}

type entry struct {
	module
	LicenseName string
	LicenseText string
	Note        string
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "gennotices:", err)
		os.Exit(1)
	}
}

func run() error {
	if err := os.Chdir(repoRoot()); err != nil {
		return err
	}

	paths, err := linkedModules()
	if err != nil {
		return err
	}

	var entries []entry
	for _, p := range paths {
		m, err := moduleInfo(p)
		if err != nil {
			return fmt.Errorf("%s: %w", p, err)
		}
		e := entry{module: m}
		name, text, err := findLicense(m.Dir)
		if err != nil {
			return fmt.Errorf("%s: %w", p, err)
		}
		e.LicenseName, e.LicenseText = name, text
		if text == "" {
			// 少数模块（如 go-localereader）只在 README 里写 "MIT"，不带许可文件。
			// 不能替它编一份原文，如实标注来源。
			e.LicenseName = "MIT"
			e.Note = "模块未附带 LICENSE 文件，许可声明见其 README"
		}
		entries = append(entries, e)
	}

	sort.Slice(entries, func(i, j int) bool { return entries[i].Path < entries[j].Path })

	w := bufio.NewWriter(os.Stdout)
	defer w.Flush()
	fmt.Fprint(w, header)
	fmt.Fprintln(w, "| 模块 | 版本 | 许可 |")
	fmt.Fprintln(w, "| --- | --- | --- |")
	for _, e := range entries {
		fmt.Fprintf(w, "| `%s` | `%s` | %s |\n", e.Path, e.Version, e.LicenseName)
	}
	fmt.Fprintln(w)

	for _, e := range entries {
		fmt.Fprintf(w, "## %s\n\n", e.Path)
		fmt.Fprintf(w, "- 版本：`%s`\n- 许可：%s\n", e.Version, e.LicenseName)
		if e.Note != "" {
			fmt.Fprintf(w, "- 说明：%s\n", e.Note)
		}
		fmt.Fprintln(w)
		if e.LicenseText == "" {
			continue
		}
		fmt.Fprintln(w, "```")
		fmt.Fprint(w, strings.TrimRight(e.LicenseText, "\n"))
		fmt.Fprintln(w)
		fmt.Fprintln(w, "```")
		fmt.Fprintln(w)
	}
	return nil
}

const header = `# Third-Party Notices

gsh 的发布二进制以 ` + "`CGO_ENABLED=0`" + ` 静态编译，下列 Go 模块被链接进二进制。
按各模块许可要求，其版权声明与许可原文随本文件一并保留。

本文件由 ` + "`scripts/gennotices`" + ` 生成，请勿手工编辑。

`

// linkedModules 返回实际参与构建的第三方模块路径（不含主模块）。
func linkedModules() ([]string, error) {
	out, err := output("go", "list", "-deps",
		"-f", `{{if .Module}}{{if not .Module.Main}}{{.Module.Path}}{{end}}{{end}}`, ".")
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var paths []string
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || seen[line] {
			continue
		}
		seen[line] = true
		paths = append(paths, line)
	}
	sort.Strings(paths)
	return paths, nil
}

func moduleInfo(path string) (module, error) {
	out, err := output("go", "list", "-m", "-json", path)
	if err != nil {
		return module{}, err
	}
	var m module
	if err := json.Unmarshal(out, &m); err != nil {
		return module{}, err
	}
	if m.Dir == "" {
		return module{}, fmt.Errorf("模块未下载到本地（Dir 为空），先执行 go mod download")
	}
	return m, nil
}

// findLicense 按优先级找许可文件，返回许可类型名与原文。
func findLicense(dir string) (string, string, error) {
	cands := []string{"LICENSE", "LICENSE.md", "LICENSE.txt", "COPYING", "COPYING.md", "COPYING.txt"}
	// 目录里可能有 LICENSE-MIT / LICENSE-BSD 这类双许可拆分，一并兜住。
	found := map[string]bool{}
	ents, err := os.ReadDir(dir)
	if err != nil {
		return "", "", err
	}
	for _, e := range ents {
		up := strings.ToUpper(e.Name())
		if strings.HasPrefix(up, "LICENSE") || strings.HasPrefix(up, "COPYING") {
			found[e.Name()] = true
		}
	}

	var file string
	for _, c := range cands {
		if found[c] {
			file = c
			break
		}
	}
	if file == "" {
		names := make([]string, 0, len(found))
		for n := range found {
			names = append(names, n)
		}
		sort.Strings(names)
		if len(names) > 0 {
			file = names[0]
		}
	}
	if file == "" {
		return "", "", nil
	}

	b, err := os.ReadFile(filepath.Join(dir, file))
	if err != nil {
		return "", "", err
	}
	text := string(b)
	return classify(text), text, nil
}

// classify 粗略识别许可类型。只用于表格里给人看的标签，不参与任何判断。
func classify(text string) string {
	t := strings.ToLower(text)
	first := strings.ToLower(strings.SplitN(strings.TrimSpace(text), "\n", 2)[0])
	switch {
	case strings.Contains(first, "apache license"):
		return "Apache-2.0"
	case strings.Contains(first, "mit license") || strings.Contains(t, "permission is hereby granted, free of charge"):
		return "MIT"
	case strings.Contains(t, "redistribution and use in source and binary forms"):
		if strings.Contains(t, "neither the name") {
			return "BSD-3-Clause"
		}
		return "BSD-2-Clause"
	case strings.Contains(t, "isc license") || strings.Contains(t, "permission to use, copy, modify, and/or distribute"):
		return "ISC"
	case strings.Contains(t, "mozilla public license"):
		return "MPL-2.0"
	case strings.Contains(t, "gnu general public license"):
		return "GPL"
	}
	return "见原文"
}

func output(name string, args ...string) ([]byte, error) {
	cmd := exec.Command(name, args...)
	cmd.Stderr = os.Stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("%s %s: %w", name, strings.Join(args, " "), err)
	}
	return out, nil
}

func repoRoot() string {
	out, err := output("go", "env", "GOMOD")
	if err != nil {
		return "."
	}
	mod := strings.TrimSpace(string(out))
	if mod == "" || mod == os.DevNull {
		return "."
	}
	return filepath.Dir(mod)
}
