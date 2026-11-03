//go:build windows

package tools

import (
	"syscall"
	"unicode/utf16"
	"unicode/utf8"
	"unsafe"
)

var (
	kernel32     = syscall.NewLazyDLL("kernel32.dll")
	procMB2WChar = kernel32.NewProc("MultiByteToWideChar")
)

// decodeConsole 把非 UTF-8 的本地输出按系统 ANSI 代码页（CP936/GBK）转成 UTF-8。
//
// 为什么需要：Windows 上 cmd 及其子进程写管道时用的是 ANSI 代码页，
// 中文全是 GBK 字节。直接当 UTF-8 交给模型就是乱码，模型会以为命令执行失败。
// 这里不引三方依赖，直接调 kernel32 转码。
func decodeConsole(b []byte) []byte {
	if len(b) == 0 || utf8.Valid(b) {
		return b
	}
	const cpACP = 0 // CP_ACP：跟随系统 ANSI 代码页
	n, _, _ := procMB2WChar.Call(
		cpACP, 0,
		uintptr(unsafe.Pointer(&b[0])), uintptr(len(b)),
		0, 0)
	if n == 0 {
		return b
	}
	wide := make([]uint16, n)
	procMB2WChar.Call(
		cpACP, 0,
		uintptr(unsafe.Pointer(&b[0])), uintptr(len(b)),
		uintptr(unsafe.Pointer(&wide[0])), n)
	return []byte(string(utf16.Decode(wide)))
}
