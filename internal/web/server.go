// Package web WebUI 插件：HTTP 服务 + SSE 流式对话 + 会话管理。
// 前端 embed 进二进制，保持单文件分发。
package web

import (
	"embed"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"sync"

	"gsh/internal/api"
	"gsh/internal/kernel"
)

//go:embed static
var staticFS embed.FS

// RegisterKernel 挂载 web 插件。
func RegisterKernel(k *kernel.Kernel) error { return k.Load(&Plugin{}) }

// Plugin web 插件。
type Plugin struct {
	mu     sync.Mutex
	srv    *http.Server
	logSeq int
}

// Name 实现 kernel.Plugin。
func (p *Plugin) Name() string { return "web" }

// Apply 注册 HTTP 入口服务。
func (p *Plugin) Apply(ctx *kernel.Ctx) error {
	cfg := ctx.Config()
	addr := ":8934"
	if cfg != nil {
		if v, ok := cfg["addr"].(string); ok && v != "" {
			addr = v
		}
	}
	ctx.Provide("webServer", &Server{addr: addr})
	return nil
}

// Server HTTP 服务句柄（main 调 Run 阻塞启动）。
type Server struct {
	addr string
}

// SetAddr 运行时覆盖监听地址（main 的 -addr flag）。
func (s *Server) SetAddr(a string) { s.addr = a }

// Run 启动 HTTP 服务（阻塞）。
func (s *Server) Run(k *kernel.Kernel) error {
	mux := http.NewServeMux()

	sub, err := fs.Sub(staticFS, "static")
	if err != nil {
		return fmt.Errorf("web: static fs: %w", err)
	}
	mux.Handle("GET /", http.FileServer(http.FS(sub)))

	loopSvc, err := kernel.Use[api.Loop](k, "loop")
	if err != nil {
		return err
	}
	store, err := kernel.Use[api.SessionStore](k, "sessionStore")
	if err != nil {
		return err
	}

	w := &webAPI{loop: loopSvc, store: store}
	mux.HandleFunc("POST /api/chat", w.handleChat)
	mux.HandleFunc("GET /api/sessions", w.handleSessions)
	mux.HandleFunc("GET /api/session", w.handleSessionDetail)

	srv := &http.Server{Addr: s.addr, Handler: mux}
	return srv.ListenAndServe()
}

type webAPI struct {
	loop  api.Loop
	store api.SessionStore
}

// handleChat POST {prompt} → SSE 流式返回 delta/tool_call/tool_result/final/error 事件。
func (w *webAPI) handleChat(rw http.ResponseWriter, r *http.Request) {
	var req struct {
		Prompt string `json:"prompt"`
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err == nil {
		_ = json.Unmarshal(body, &req)
	}
	if req.Prompt == "" {
		httpError(rw, http.StatusBadRequest, "prompt is required")
		return
	}
	flusher, ok := rw.(http.Flusher)
	if !ok {
		httpError(rw, http.StatusInternalServerError, "streaming unsupported")
		return
	}
	rw.Header().Set("Content-Type", "text/event-stream")
	rw.Header().Set("Cache-Control", "no-cache")
	rw.Header().Set("X-Accel-Buffering", "no")

	send := func(event string, data any) {
		b, _ := json.Marshal(data)
		fmt.Fprintf(rw, "event: %s\ndata: %s\n\n", event, b)
		flusher.Flush()
	}

	ctx := r.Context()
	_, runErr := w.loop.RunTraced(ctx, req.Prompt,
		func(delta string) { send("delta", map[string]string{"text": delta}) },
		func(ev api.Event) { send(string(ev.Kind), ev) },
	)
	if runErr != nil {
		send("error", map[string]string{"message": runErr.Error()})
	}
	send("done", map[string]any{})
}

// handleSessions GET → 会话 ID 列表（新→旧）。
func (w *webAPI) handleSessions(rw http.ResponseWriter, r *http.Request) {
	ids, err := w.store.List()
	if err != nil {
		httpError(rw, http.StatusInternalServerError, err.Error())
		return
	}
	if ids == nil {
		ids = []string{}
	}
	writeJSON(rw, map[string]any{"sessions": ids})
}

// handleSessionDetail GET ?id=xxx → 会话全部消息。
func (w *webAPI) handleSessionDetail(rw http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("id")
	if id == "" {
		httpError(rw, http.StatusBadRequest, "id is required")
		return
	}
	sess, err := w.store.Open(id)
	if err != nil {
		httpError(rw, http.StatusNotFound, err.Error())
		return
	}
	msgs := sess.Messages()
	if msgs == nil {
		msgs = []api.Message{}
	}
	writeJSON(rw, map[string]any{"id": sess.ID(), "messages": msgs})
}

func writeJSON(rw http.ResponseWriter, v any) {
	rw.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(rw).Encode(v)
}

func httpError(rw http.ResponseWriter, code int, msg string) {
	rw.Header().Set("Content-Type", "application/json; charset=utf-8")
	rw.WriteHeader(code)
	_ = json.NewEncoder(rw).Encode(map[string]string{"error": msg})
}
