// elfcheck 校验 Linux 产物是不是「能在老机器上跑」的那种二进制。
//
// 为什么需要它：Go 在交叉编译时默认 CGO_ENABLED=0，产物静态链接、不依赖 glibc。
// 但 CI 的 ubuntu runner 上编译 linux/amd64 属于「同平台编译」，CGO 默认为 1，
// 一旦哪个环节漏了 CGO_ENABLED=0，产物就会动态链接 runner 自带的 glibc。
// 结果是：在新系统上跑得好好的，下载到 CentOS 7 / Debian 10 上直接
// "version `GLIBC_2.34' not found"。
//
// 这种错误只会在用户机器上暴露，所以在打包阶段就必须拦住。
//
// 用法：go run ./scripts/elfcheck <二进制路径>
package main

import (
	"debug/elf"
	"fmt"
	"os"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "用法: elfcheck <二进制路径>")
		os.Exit(2)
	}
	bad := false
	for _, path := range os.Args[1:] {
		if err := check(path); err != nil {
			fmt.Fprintf(os.Stderr, "✗ %s: %v\n", path, err)
			bad = true
			continue
		}
		fmt.Printf("✓ %s\n", path)
	}
	if bad {
		os.Exit(1)
	}
}

// check 检查一个 ELF 是否自包含。
func check(path string) error {
	f, err := elf.Open(path)
	if err != nil {
		return fmt.Errorf("不是可解析的 ELF: %w", err)
	}
	defer f.Close()

	// PT_INTERP 存在 = 需要动态加载器（ld-linux-*.so），即依赖系统 libc。
	for _, p := range f.Progs {
		if p.Type == elf.PT_INTERP {
			return fmt.Errorf("动态链接（含 PT_INTERP，需要系统 ld.so）——检查是否漏了 CGO_ENABLED=0")
		}
	}

	// DT_NEEDED 列出运行期依赖的共享库，静态产物应当为空。
	if libs, err := f.ImportedLibraries(); err == nil {
		if len(libs) > 0 {
			return fmt.Errorf("依赖共享库 %v —— 检查是否漏了 CGO_ENABLED=0", libs)
		}
	}

	// 兜底：符号表里出现 GLIBC_ 版本引用说明带了 glibc 版本约束。
	if syms, err := f.DynamicSymbols(); err == nil {
		for _, s := range syms {
			if s.Version != "" && len(s.Version) > 6 && s.Version[:6] == "GLIBC_" {
				return fmt.Errorf("引用了 %s，在旧发行版上会跑不起来", s.Version)
			}
		}
	}
	return nil
}
