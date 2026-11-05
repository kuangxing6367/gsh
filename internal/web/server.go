// Package web WebUI 插件：HTTP 服务 + SSE 流式对话 + 会话管理。
// 前端 embed 进二进制，保持单文件分发。
package web

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"gsh/internal/api"
	"gsh/internal/kernel"
	"gsh/internal/tools"
)

//go:embed static
var staticFS embed.FS

// RegisterKernel 挂载 web 插件。
func RegisterKernel(k *kernel.Kernel) error { return k.Load(&Plugin{}) }

// Plugin web 插件。
type Plugin struct {
	mu  sync.Mutex
	srv *http.Server
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
	ctx.Provide("webServer", &Server{addr: addr, dataDir: ctx.DataDir()})
	// 启动时清一次历史上传：上次进程退出时可能已攒下过期文件。
	// 异步执行，不影响首屏。
	go pruneUploads(filepath.Join(ctx.DataDir(), "uploads"))
	return nil
}

// Server HTTP 服务句柄（main 调 Run 阻塞启动）。
type Server struct {
	addr    string
	dataDir string // 上传附件落地目录的父级（.gsh/web）
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
	reg, _ := kernel.Use[api.ToolRegistry](k, "toolRegistry")

	w := &webAPI{
		k:         k,
		loop:      loopSvc,
		store:     store,
		reg:       reg,
		runs:      map[string]context.CancelFunc{},
		uploadDir: filepath.Join(s.dataDir, "uploads"),
	}
	if g, err := kernel.Use[*tools.Guard](k, "guard"); err == nil {
		w.guard = g
	}
	if ps, err := kernel.Use[api.PermissionSwitch](k, "permission"); err == nil {
		w.perm = ps
	}
	if a, err := kernel.Use[*tools.AuditLog](k, "audit"); err == nil {
		w.audit = a
	}
	mux.HandleFunc("GET /api/state", w.handleState)
	mux.HandleFunc("GET /api/sessions", w.handleSessions)
	mux.HandleFunc("GET /api/session", w.handleSessionDetail)
	mux.HandleFunc("POST /api/session/new", w.handleSessionNew)
	mux.HandleFunc("POST /api/session/delete", w.handleSessionDelete)
	mux.HandleFunc("POST /api/session/rename", w.handleSessionRename)
	mux.HandleFunc("POST /api/chat", w.handleChat)
	mux.HandleFunc("POST /api/upload", w.handleUpload)
	mux.HandleFunc("GET /api/asset", w.handleAsset)
	mux.HandleFunc("POST /api/stop", w.handleStop)
	mux.HandleFunc("POST /api/settings", w.handleSettings)
	mux.HandleFunc("GET /api/permission", w.handlePermissionGet)
	mux.HandleFunc("GET /api/tools", w.handleTools)
	mux.HandleFunc("POST /api/permission", w.handlePermissionSet)
	mux.HandleFunc("GET /api/workspace/browse", w.handleBrowse)
	mux.HandleFunc("POST /api/workspace", w.handleSetWorkspace)
	mux.HandleFunc("POST /api/workspace/restrict", w.handleRestrictWorkspace)
	mux.HandleFunc("GET /api/audit", w.handleAudit)
	mux.HandleFunc("GET /api/models", w.handleModels)
	mux.HandleFunc("POST /api/models/use", w.handleModelUse)

	srv := &http.Server{Addr: s.addr, Handler: mux}
	return srv.ListenAndServe()
}

type webAPI struct {
	k     *kernel.Kernel
	loop  api.Loop
	store api.SessionStore
	reg   api.ToolRegistry
	guard *tools.Guard
	audit *tools.AuditLog
	// perm 是带 plan 档的完整切换器；guard 只认四个 Permission 枚举，
	// 少了它 Web 就永远切不到计划模式。
	perm api.PermissionSwitch

	uploadDir string

	mu   sync.Mutex
	runs map[string]context.CancelFunc
}

// ---------- state ----------

// handleState GET → 首屏所需的全部信息。
func (w *webAPI) handleState(rw http.ResponseWriter, r *http.Request) {
	metas, err := w.store.ListMeta()
	if err != nil {
		httpError(rw, http.StatusInternalServerError, err.Error())
		return
	}
	models, active, aux := w.modelState()
	// 多模型写法下顶层没有 model 字段，顶栏应显示当前选中的模型名。
	shownModel := w.llmString("model")
	if active != "" {
		shownModel = active
	}
	writeJSON(rw, map[string]any{
		"models":             models,
		"model_active":       active,
		"model_aux":          aux,
		"model":              shownModel,
		"provider":           w.llmString("provider"),
		"base_url":           w.llmString("base_url"),
		"thinking_format":    w.llmString("thinking_format"),
		"api_key_set":        w.llmString("api_key") != "",
		"workspace":          w.wsRoot(),
		"workspace_restrict": w.wsRestricted(),
		"permission":         w.permName(),
		"permission_label":   w.permLabel(),
		"tools":              w.toolNames(),
		"sessions":           metas,
		"audit_enabled":      w.audit != nil && w.audit.Enabled(),
		"now":                time.Now().Format(time.RFC3339),
	})
}

// modelState 读取当前模型表状态；服务未装载时返回空列表，首屏不因此报错。
func (w *webAPI) modelState() ([]string, string, string) {
	if w.k == nil {
		return nil, "", ""
	}
	mm, err := kernel.Use[api.ModelManager](w.k, "modelManager")
	if err != nil {
		return nil, "", ""
	}
	return mm.Names(), mm.ActiveName(), mm.AuxName()
}

// handleModels GET → 具名模型列表与当前主/辅模型。
func (w *webAPI) handleModels(rw http.ResponseWriter, _ *http.Request) {
	names, active, aux := w.modelState()
	writeJSON(rw, map[string]any{"models": names, "active": active, "aux": aux})
}

// handleModelUse POST → 切换主模型或辅助模型，下一次请求立即生效。
func (w *webAPI) handleModelUse(rw http.ResponseWriter, r *http.Request) {
	var req struct {
		Name string `json:"name"`
		Aux  bool   `json:"aux"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpError(rw, http.StatusBadRequest, "请求体不是合法 JSON")
		return
	}
	if req.Name == "" {
		httpError(rw, http.StatusBadRequest, "缺少 name")
		return
	}
	mm, err := kernel.Use[api.ModelManager](w.k, "modelManager")
	if err != nil {
		httpError(rw, http.StatusServiceUnavailable, err.Error())
		return
	}
	if req.Aux {
		err = mm.SetAux(req.Name)
	} else {
		err = mm.Use(req.Name)
	}
	if err != nil {
		httpError(rw, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(rw, map[string]any{"ok": true, "active": mm.ActiveName(), "aux": mm.AuxName()})
}

func (w *webAPI) llmString(key string) string {
	if w.k == nil {
		return ""
	}
	if cfg, ok := w.k.Options("llm"); ok {
		if v, ok := cfg[key].(string); ok {
			return v
		}
	}
	return ""
}

func (w *webAPI) toolString(key string) string {
	if w.k == nil {
		return ""
	}
	if cfg, ok := w.k.Options("tools"); ok {
		if v, ok := cfg[key].(string); ok {
			return v
		}
	}
	return ""
}

func (w *webAPI) toolNames() []string {
	if w.reg == nil {
		return []string{}
	}
	return w.reg.Names()
}

// ---------- sessions ----------

// handleSessions GET → 会话摘要列表（新→旧）。
func (w *webAPI) handleSessions(rw http.ResponseWriter, r *http.Request) {
	// 带 q 时走全文检索：侧栏搜索框与「历史回看」共用这一个入口。
	if q := strings.TrimSpace(r.URL.Query().Get("q")); q != "" {
		hits, err := w.store.Search(q, 50)
		if err != nil {
			httpError(rw, http.StatusInternalServerError, err.Error())
			return
		}
		if hits == nil {
			hits = []api.SessionHit{}
		}
		writeJSON(rw, map[string]any{"query": q, "hits": hits})
		return
	}
	metas, err := w.store.ListMeta()
	if err != nil {
		httpError(rw, http.StatusInternalServerError, err.Error())
		return
	}
	if metas == nil {
		metas = []api.SessionMeta{}
	}
	writeJSON(rw, map[string]any{"sessions": metas})
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

// handleSessionNew POST → 新建空会话。
func (w *webAPI) handleSessionNew(rw http.ResponseWriter, r *http.Request) {
	sess, err := w.store.New()
	if err != nil {
		httpError(rw, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(rw, map[string]any{"id": sess.ID()})
}

// handleSessionDelete POST {id} → 删除会话（真删）。
func (w *webAPI) handleSessionDelete(rw http.ResponseWriter, r *http.Request) {
	var req struct {
		ID string `json:"id"`
	}
	body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	_ = json.Unmarshal(body, &req)
	if req.ID == "" {
		httpError(rw, http.StatusBadRequest, "id is required")
		return
	}
	if err := w.store.Delete(req.ID); err != nil {
		httpError(rw, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(rw, map[string]any{"ok": true})
}

// ---------- chat ----------

// handleChat POST {prompt, session_id, run_id} → SSE。
func (w *webAPI) handleChat(rw http.ResponseWriter, r *http.Request) {
	var req struct {
		Prompt    string   `json:"prompt"`
		SessionID string   `json:"session_id"`
		RunID     string   `json:"run_id"`
		Images    []string `json:"images"` // 上传接口返回的本地路径
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 4<<20))
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

	var writeMu sync.Mutex
	send := func(event string, data any) {
		b, _ := json.Marshal(data)
		writeMu.Lock()
		fmt.Fprintf(rw, "event: %s\ndata: %s\n\n", event, b)
		flusher.Flush()
		writeMu.Unlock()
	}

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	if req.RunID != "" {
		w.mu.Lock()
		w.runs[req.RunID] = cancel
		w.mu.Unlock()
		defer func() {
			w.mu.Lock()
			delete(w.runs, req.RunID)
			w.mu.Unlock()
		}()
	}

	send("start", map[string]any{"session_id": req.SessionID, "run_id": req.RunID})

	// 附件只认上传目录里的文件：不接受前端直接指定任意本地路径，
	// 否则「让模型看一张图」就变成了「让模型读你磁盘上的任何文件」。
	parts := make([]api.ContentPart, 0, len(req.Images))
	ignored := 0
	dup := map[string]bool{}
	for _, p := range req.Images {
		if !w.insideUploads(p) {
			log.Printf("忽略非上传目录的附件: %s", p)
			ignored++
			continue
		}
		// 同一张图重复给出时只留一份：每多一份就是几百上千 token 的重复账单，
		// 而模型看到的其实是同一个像素。
		if dup[p] {
			continue
		}
		dup[p] = true
		parts = append(parts, api.ContentPart{Type: "image", Path: p})
	}
	// 被丢掉的部分必须说出来：用户以为图发出去了，模型其实没见过，
	// 接下来它会照着「你没有附图」的样子回答，用户只会觉得模型瞎了。
	if ignored > 0 {
		send("notice", map[string]any{
			"text": fmt.Sprintf("%d 个附件不在上传目录，已忽略；请重新用附件按钮上传", ignored),
		})
	}

	_, runErr := w.loop.RunWithParts(ctx, req.SessionID, req.Prompt, parts,
		func(delta string) { send("delta", map[string]string{"text": delta}) },
		func(ev api.Event) { send(string(ev.Kind), ev) },
	)
	if runErr != nil {
		// 服务端必须留痕：前端只显示一行错误，排查时得能从日志看到完整原因。
		log.Printf("chat 失败 session=%s: %v", req.SessionID, runErr)
		if ctx.Err() != nil {
			send("stopped", map[string]string{"message": "已被用户停止"})
		} else {
			send("error", map[string]string{"message": runErr.Error()})
		}
	}
	send("done", map[string]any{})
}

// handleStop POST {run_id} → 取消正在跑的一轮。
func (w *webAPI) handleStop(rw http.ResponseWriter, r *http.Request) {
	var req struct {
		RunID string `json:"run_id"`
	}
	body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	_ = json.Unmarshal(body, &req)
	w.mu.Lock()
	cancel, ok := w.runs[req.RunID]
	w.mu.Unlock()
	if !ok {
		httpError(rw, http.StatusNotFound, "run not found")
		return
	}
	cancel()
	writeJSON(rw, map[string]any{"ok": true})
}

// ---------- workspace ----------

// handleBrowse GET ?path= → 列出该目录下的子目录（目录选择器用）。
func (w *webAPI) handleBrowse(rw http.ResponseWriter, r *http.Request) {
	p := r.URL.Query().Get("path")
	if p == "" {
		p = w.wsRoot()
	}
	abs, err := filepath.Abs(p)
	if err != nil {
		httpError(rw, http.StatusBadRequest, err.Error())
		return
	}
	ents, err := os.ReadDir(abs)
	if err != nil {
		httpError(rw, http.StatusBadRequest, err.Error())
		return
	}
	dirs := []map[string]string{}
	for _, e := range ents {
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		dirs = append(dirs, map[string]string{"name": e.Name(), "path": filepath.Join(abs, e.Name())})
	}
	writeJSON(rw, map[string]any{"path": abs, "parent": filepath.Dir(abs), "dirs": dirs})
}

// handleSetWorkspace POST {path} → 切换所有工具的工作区基准。
func (w *webAPI) handleSetWorkspace(rw http.ResponseWriter, r *http.Request) {
	var req struct {
		Path string `json:"path"`
	}
	body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	_ = json.Unmarshal(body, &req)
	if strings.TrimSpace(req.Path) == "" {
		httpError(rw, http.StatusBadRequest, "path is required")
		return
	}
	ws, err := kernel.Use[*tools.Workspace](w.k, "workspace")
	if err != nil {
		httpError(rw, http.StatusInternalServerError, err.Error())
		return
	}
	if err := ws.Set(req.Path); err != nil {
		httpError(rw, http.StatusBadRequest, err.Error())
		return
	}
	// 顺手写回配置，下次启动仍是这个工作区。
	toolsCfg, _ := w.k.Options("tools")
	next := map[string]any{}
	for k, v := range toolsCfg {
		next[k] = v
	}
	next["workspace"] = ws.Root()
	w.k.SetOptions("tools", next)
	_ = persistPluginOptions(w.k.ConfigPath(), "tools", next)

	writeJSON(rw, map[string]any{"ok": true, "workspace": ws.Root()})
}

func (w *webAPI) wsRoot() string {
	if ws, err := kernel.Use[*tools.Workspace](w.k, "workspace"); err == nil {
		return ws.Root()
	}
	return "."
}

func (w *webAPI) wsRestricted() bool {
	if ws, err := kernel.Use[*tools.Workspace](w.k, "workspace"); err == nil {
		return ws.Restricted()
	}
	return true
}

// handleRestrictWorkspace POST {restrict: bool} → 运行期切换路径白名单并写回配置。
// 只影响文件类工具；bash 命令自身不受此边界约束。
func (w *webAPI) handleRestrictWorkspace(rw http.ResponseWriter, r *http.Request) {
	var req struct {
		Restrict *bool `json:"restrict"`
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<16))
	if err != nil {
		httpError(rw, http.StatusBadRequest, "read body: "+err.Error())
		return
	}
	if err := json.Unmarshal(body, &req); err != nil || req.Restrict == nil {
		httpError(rw, http.StatusBadRequest, "restrict (bool) is required")
		return
	}
	ws, err := kernel.Use[*tools.Workspace](w.k, "workspace")
	if err != nil {
		httpError(rw, http.StatusInternalServerError, err.Error())
		return
	}
	ws.SetRestricted(*req.Restrict)
	toolsCfg, _ := w.k.Options("tools")
	next := map[string]any{}
	for k, v := range toolsCfg {
		next[k] = v
	}
	next["workspace_restrict"] = *req.Restrict
	w.k.SetOptions("tools", next)
	_ = persistPluginOptions(w.k.ConfigPath(), "tools", next)
	writeJSON(rw, map[string]any{"ok": true, "workspace_restrict": ws.Restricted()})
}

// handleSessionRename POST {id, title} → 改会话标题（空 title 清除自定义名）。
func (w *webAPI) handleSessionRename(rw http.ResponseWriter, r *http.Request) {
	var req struct {
		ID    string `json:"id"`
		Title string `json:"title"`
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<16))
	if err != nil {
		httpError(rw, http.StatusBadRequest, "read body: "+err.Error())
		return
	}
	if err := json.Unmarshal(body, &req); err != nil {
		httpError(rw, http.StatusBadRequest, "bad json: "+err.Error())
		return
	}
	if req.ID == "" {
		httpError(rw, http.StatusBadRequest, "id is required")
		return
	}
	if err := w.store.Rename(req.ID, req.Title); err != nil {
		httpError(rw, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(rw, map[string]any{"ok": true, "id": req.ID, "title": req.Title})
}

// ---------- tools / skills ----------

// handleTools GET → 工具规格 + 技能列表 + 运行诊断。
// 存在的理由：模型有哪些能力、当前环境是什么样，不该只能靠 -tools flag
// 在终端里看——Web 侧同样要能回答「它到底能干什么」。
func (w *webAPI) handleTools(rw http.ResponseWriter, _ *http.Request) {
	specs := []api.ToolSpec{}
	if w.reg != nil {
		specs = w.reg.Specs()
	}
	toolsOut := make([]map[string]any, 0, len(specs))
	for _, sp := range specs {
		toolsOut = append(toolsOut, map[string]any{
			"name":        sp.Name,
			"description": sp.Description,
			"parameters":  sp.Parameters,
		})
	}

	skillsOut := []map[string]string{}
	if s, err := kernel.Use[*tools.SkillRegistry](w.k, "skills"); err == nil && s != nil {
		for _, sk := range s.List() {
			skillsOut = append(skillsOut, map[string]string{
				"name":        sk.Name,
				"description": sk.Description,
			})
		}
	}

	diag := ""
	if f, err := kernel.Use[func() string](w.k, "toolInfo"); err == nil && f != nil {
		diag = f()
	}

	writeJSON(rw, map[string]any{
		"tools":      toolsOut,
		"skills":     skillsOut,
		"diag":       diag,
		"permission": w.permName(),
		"workspace":  w.wsRoot(),
	})
}

// ---------- audit ----------

// handleAudit GET ?n= → 最近 n 条操作审计记录（新→旧，最多 1000）。
// enabled=false 时返回空列表（审计被 config 关闭）。
func (w *webAPI) handleAudit(rw http.ResponseWriter, r *http.Request) {
	n := 100
	if v := r.URL.Query().Get("n"); v != "" {
		if x, err := strconv.Atoi(v); err == nil && x > 0 {
			n = x
		}
	}
	if n > 1000 {
		n = 1000
	}
	entries := []tools.AuditEntry{}
	enabled := false
	if w.audit != nil {
		enabled = true
		if es, err := w.audit.Recent(n); err == nil {
			entries = es
		}
	}
	writeJSON(rw, map[string]any{"enabled": enabled, "entries": entries})
}

// ---------- permission ----------

// permName / permLabel 当前权限档位，供首屏与状态栏显示。
func (w *webAPI) permName() string {
	if w.perm != nil {
		return w.perm.Mode()
	}
	if w.guard == nil {
		return "standard"
	}
	return w.guard.Mode().String()
}

func (w *webAPI) permLabel() string { return permLabelOf(w.permName()) }

// permLabelOf 档位名 → 中文标签。计划档的底层权限是只读，
// 标签必须单独给，否则界面会把「计划」显示成「只读」。
func permLabelOf(mode string) string {
	switch mode {
	case "read_only":
		return "只读"
	case "full":
		return "完全权限"
	case "plan":
		return "计划"
	}
	return "标准"
}

// handlePermissionGet GET → 当前档位与可选项。
func (w *webAPI) handlePermissionGet(rw http.ResponseWriter, _ *http.Request) {
	writeJSON(rw, map[string]any{
		"mode":  w.permName(),
		"label": w.permLabel(),
		"options": []map[string]string{
			{"mode": "read_only", "label": "只读", "desc": "仅读取与网络访问，不执行命令、不改文件"},
			{"mode": "standard", "label": "标准", "desc": "可读写文件与执行命令，破坏性命令仍被拦截"},
			{"mode": "full", "label": "完全权限", "desc": "破坏性命令放行（灾难性命令仍然拦截）"},
			{"mode": "plan", "label": "计划", "desc": "只读 + 先出方案，等确认后再动手"},
		},
	})
}

// handlePermissionSet POST → 切换档位，下一次工具调用即生效。
func (w *webAPI) handlePermissionSet(rw http.ResponseWriter, r *http.Request) {
	if w.guard == nil {
		httpError(rw, http.StatusServiceUnavailable, "permission guard not available")
		return
	}
	var req map[string]any
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<16))
	if err != nil {
		httpError(rw, http.StatusBadRequest, "read body: "+err.Error())
		return
	}
	if err := json.Unmarshal(body, &req); err != nil {
		httpError(rw, http.StatusBadRequest, "bad json: "+err.Error())
		return
	}
	mode, _ := req["mode"].(string)
	if w.perm != nil {
		if err := w.perm.SetMode(mode); err != nil {
			httpError(rw, http.StatusBadRequest, err.Error())
			return
		}
		cur := w.perm.Mode()
		writeJSON(rw, map[string]any{"mode": cur, "label": permLabelOf(cur)})
		return
	}
	p, err := tools.ParsePerm(mode)
	if err != nil {
		httpError(rw, http.StatusBadRequest, err.Error())
		return
	}
	w.guard.Set(p)
	writeJSON(rw, map[string]any{"mode": p.String(), "label": p.Label()})
}

// ---------- settings ----------

// handleSettings POST → 改模型配置：热更新服务 + 写回 config.json。
//
// 只接受白名单字段：让接口能改什么，必须是代码里明写的，
// 否则前端一个拼写错误就能把整个配置节替换掉。
func (w *webAPI) handleSettings(rw http.ResponseWriter, r *http.Request) {
	var req map[string]any
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		httpError(rw, http.StatusBadRequest, "read body: "+err.Error())
		return
	}
	if err := json.Unmarshal(body, &req); err != nil {
		httpError(rw, http.StatusBadRequest, "bad json: "+err.Error())
		return
	}

	cur, _ := w.k.Options("llm")
	next := map[string]any{}
	for k, v := range cur {
		next[k] = v
	}
	for _, key := range []string{"provider", "base_url", "model", "thinking_format", "api_key"} {
		v, ok := req[key]
		if !ok {
			continue
		}
		s, _ := v.(string)
		s = strings.TrimSpace(s)
		// 空字符串表示「不改」：前端不回显 API Key，留空不能把已有 key 抹掉。
		if s == "" {
			continue
		}
		next[key] = s
	}

	llmSvc, err := kernel.Use[api.LLM](w.k, "llm")
	if err != nil {
		httpError(rw, http.StatusInternalServerError, err.Error())
		return
	}
	if c, ok := llmSvc.(api.Configurable); ok {
		if err := c.Configure(next); err != nil {
			httpError(rw, http.StatusBadRequest, err.Error())
			return
		}
	}
	w.k.SetOptions("llm", next)

	if err := persistPluginOptions(w.k.ConfigPath(), "llm", next); err != nil {
		// 热更新已生效，写盘失败只警告不否定这次修改。
		writeJSON(rw, map[string]any{"ok": true, "warning": "配置已在内存生效，但写回文件失败：" + err.Error()})
		return
	}
	writeJSON(rw, map[string]any{"ok": true})
}

// persistPluginOptions 把某个插件的配置节写回 config.json（保留其它节）。
func persistPluginOptions(path, plugin string, opts map[string]any) error {
	if path == "" {
		return nil
	}
	root := map[string]any{}
	if raw, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(raw, &root)
	} else if !os.IsNotExist(err) {
		return err
	}
	pc, ok := root["plugins_cfg"].(map[string]any)
	if !ok {
		pc = map[string]any{}
		root["plugins_cfg"] = pc
	}
	pc[plugin] = opts
	b, err := json.MarshalIndent(root, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(b, '\n'), 0o600)
}

// ---------- helpers ----------

func writeJSON(rw http.ResponseWriter, v any) {
	rw.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(rw).Encode(v)
}

func httpError(rw http.ResponseWriter, code int, msg string) {
	rw.Header().Set("Content-Type", "application/json; charset=utf-8")
	rw.WriteHeader(code)
	_ = json.NewEncoder(rw).Encode(map[string]string{"error": msg})
}
