// Package logx 进程级日志：同时写 stderr 与按天切分的日志文件。
//
// 之所以单独建包：gsh 各插件（loop / tools / web）都直接用标准库 log，
// 这里统一 SetOutput，任何一处 log.Printf 都会同时进终端与日志文件，
// 不必逐个插件改调用点。
package logx

import (
	"io"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"
)

var (
	mu      sync.Mutex
	curDate string
	logDir  string
	file    *os.File
)

// Setup 在 dir 下建立日志目录，并把标准库 log 输出重定向到多路（stderr + 当日文件）。
// 返回日志文件路径；失败时退回仅 stderr 并返回空路径。
func Setup(dir string) string {
	mu.Lock()
	defer mu.Unlock()
	logDir = dir
	if err := os.MkdirAll(dir, 0o755); err != nil {
		log.SetOutput(os.Stderr)
		return ""
	}
	path := filepath.Join(dir, time.Now().Format("20060102")+".log")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		log.SetOutput(os.Stderr)
		return ""
	}
	file = f
	curDate = time.Now().Format("20060102")
	log.SetOutput(io.MultiWriter(os.Stderr, &dayWriter{}))
	log.SetFlags(log.LstdFlags)
	return path
}

// dayWriter 每天第一次写入时切换到新文件，避免单文件无限增长。
type dayWriter struct{}

func (d *dayWriter) Write(p []byte) (int, error) {
	mu.Lock()
	defer mu.Unlock()
	today := time.Now().Format("20060102")
	if today != curDate && logDir != "" {
		if nf, err := os.OpenFile(filepath.Join(logDir, today+".log"),
			os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600); err == nil {
			if file != nil {
				_ = file.Close()
			}
			file = nf
			curDate = today
		}
	}
	if file == nil {
		return len(p), nil
	}
	return file.Write(p)
}

// Close 关闭日志文件。
func Close() {
	mu.Lock()
	defer mu.Unlock()
	if file != nil {
		_ = file.Close()
		file = nil
	}
}
