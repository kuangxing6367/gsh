package main

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// TestRejectsDynamicELF 反向用例：系统自带的 ls 是动态链接的，
// 校验器必须拒绝它。没有这条，check() 有可能永远返回 nil 而没人发现。
func TestRejectsDynamicELF(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("需要 Linux 上现成的动态链接二进制")
	}
	for _, p := range []string{"/bin/ls", "/usr/bin/ls"} {
		if _, err := os.Stat(p); err != nil {
			continue
		}
		if err := check(p); err == nil {
			t.Fatalf("%s 是动态链接的，check 却放行了", p)
		}
		return
	}
	t.Skip("找不到可用的动态链接样本")
}

// TestRejectsNonELF 非 ELF 文件（脚本、文本）不能当成产物放行。
func TestRejectsNonELF(t *testing.T) {
	p := filepath.Join(t.TempDir(), "not-a-binary")
	if err := os.WriteFile(p, []byte("#!/bin/sh\necho hi\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := check(p); err == nil {
		t.Fatal("脚本文件不应通过 ELF 校验")
	}
}

// TestRejectsMissing 路径不存在时报错而不是静默通过。
func TestRejectsMissing(t *testing.T) {
	if err := check(filepath.Join(t.TempDir(), "nope")); err == nil {
		t.Fatal("不存在的文件应报错")
	}
}
