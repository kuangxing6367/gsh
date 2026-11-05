package tools

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// TestClipAtKeepsUTF8Valid 截断必须落在字符边界上。
// 按字节直接切会把汉字劈成两半，输出尾部出现替换符，
// 这串非法字节还会被 JSON 编码器原样带出去。
func TestClipAtKeepsUTF8Valid(t *testing.T) {
	// 1 字节 ASCII 前缀 + 3 字节汉字：字符边界在 1,4,7…，
	// 只要切口不在边界上，旧实现必然切坏。
	s := "a" + strings.Repeat("中", 20000)
	got := clipAt(s, maxToolOutput)
	if !utf8.ValidString(got) {
		t.Fatalf("截断结果不是合法 UTF-8，尾部字节 % x", got[len(got)-12:])
	}
	if !strings.HasSuffix(got, "...[truncated]") {
		t.Fatal("应带截断标记")
	}
	// 回退最多 3 字节（一个汉字），上限不该被明显突破。
	if n := len(got); n < maxToolOutput-4 || n > maxToolOutput+32 {
		t.Fatalf("截断后长度异常: %d", n)
	}
}

// TestClipAtShortPassThrough 没超限的内容必须原样返回，不加标记。
func TestClipAtShortPassThrough(t *testing.T) {
	s := "正常输出"
	if got := clipAt(s, maxToolOutput); got != s {
		t.Fatalf("短内容被改动: %q", got)
	}
}
