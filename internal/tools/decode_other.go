//go:build !windows

package tools

// decodeConsole 非 Windows 平台输出基本已是 UTF-8，原样返回。
func decodeConsole(b []byte) []byte { return b }
