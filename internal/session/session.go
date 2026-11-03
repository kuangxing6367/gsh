// Package sessionplugin 提供会话存储插件：
// JSONL append-only 落盘，支持 resume / fork（对齐 DSH 的 append-only session log）。
package session

import (
	"bufio"
	cryptorand "crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"gsh/internal/api"
	"gsh/internal/kernel"
)

// RegisterKernel 把会话插件挂到内核。
func RegisterKernel(k *kernel.Kernel) error { return k.Load(&Plugin{}) }

// Plugin 会话插件本体。
type Plugin struct{}

// Name 实现kernel.Plugin。
func (p *Plugin) Name() string { return "session" }

// Apply 注册 SessionStore 服务。
func (p *Plugin) Apply(ctx *kernel.Ctx) error {
	cfg := ctx.Config()
	dir := ""
	if cfg != nil {
		if v, ok := cfg["dir"].(string); ok && v != "" {
			dir = v
		}
	}
	if dir == "" {
		dir = filepath.Join(ctx.DataDir(), "sessions")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("session: mkdir %s: %w", dir, err)
	}
	ctx.Provide("sessionStore", &store{dir: dir})
	return nil
}

// store SessionStore 实现。
type store struct {
	dir string
	mu  sync.Mutex
}

func (s *store) path(id string) string { return filepath.Join(s.dir, id+".jsonl") }

// titlePath 自定义标题放在同名 .title 文件里：
// 不改 JSONL 格式，删除会话时顺手一起删。
func (s *store) titlePath(id string) string { return filepath.Join(s.dir, id+".title") }

func (s *store) New() (api.Session, error) {
	id := time.Now().UTC().Format("20060102-150405") + "-" + randSuffix()
	f, err := os.OpenFile(s.path(id), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, fmt.Errorf("session: create: %w", err)
	}
	f.Close()
	return &session{id: id, path: s.path(id)}, nil
}

func (s *store) List() ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ents, err := os.ReadDir(s.dir)
	if err != nil {
		return nil, err
	}
	var ids []string
	for _, e := range ents {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".jsonl") {
			continue
		}
		ids = append(ids, strings.TrimSuffix(name, ".jsonl"))
	}
	// 文件名含时间戳，倒序即新→旧
	sort.Sort(sort.Reverse(sort.StringSlice(ids)))
	return ids, nil
}

func (s *store) Open(id string) (api.Session, error) {
	if !validID(id) {
		return nil, fmt.Errorf("session: bad id %q", id)
	}
	if _, err := os.Stat(s.path(id)); err != nil {
		return nil, fmt.Errorf("session: open %s: %w", id, err)
	}
	return &session{id: id, path: s.path(id)}, nil
}

func (s *store) Fork(id string) (api.Session, error) {
	old, err := s.Open(id)
	if err != nil {
		return nil, err
	}
	msgs := old.Messages()
	neu, err := s.New()
	if err != nil {
		return nil, err
	}
	for _, m := range msgs {
		if err := neu.Append(m); err != nil {
			return nil, err
		}
	}
	return neu, nil
}

// ListMeta 返回带标题/时间/消息数的会话列表（新→旧）。
//
// 标题不单独存元数据文件：会话本身就是 append-only JSONL，
// 首条 user 消息就是「用户想干什么」最诚实的概括，算出来比另存一份更难失真。
func (s *store) ListMeta() ([]api.SessionMeta, error) {
	ids, err := s.List()
	if err != nil {
		return nil, err
	}
	out := make([]api.SessionMeta, 0, len(ids))
	for _, id := range ids {
		meta := api.SessionMeta{ID: id, Title: "新会话"}
		if fi, err := os.Stat(s.path(id)); err == nil {
			meta.Updated = fi.ModTime()
		}
		f, err := os.Open(s.path(id))
		if err != nil {
			out = append(out, meta)
			continue
		}
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
		for sc.Scan() {
			line := strings.TrimSpace(sc.Text())
			if line == "" {
				continue
			}
			meta.Messages++
			if meta.Title != "新会话" {
				continue
			}
			var m api.Message
			if json.Unmarshal([]byte(line), &m) == nil && m.Role == api.RoleUser && strings.TrimSpace(m.Content) != "" {
				meta.Title = summarize(m.Content)
			}
		}
		f.Close()
		// 自定义标题优先：侧栏显示的是用户认的名字，不是自动摘要。
		if raw, err := os.ReadFile(s.titlePath(id)); err == nil {
			if t := strings.TrimSpace(string(raw)); t != "" {
				meta.Title = t
			}
		}
		out = append(out, meta)
	}
	return out, nil
}

// summarize 把首条用户消息压成一行标题。
// 附件内容块（"--- 附件 … ---"）先剥掉：标题要认出「用户问了什么」，
// 不是把整段附件原文也算进摘要。
func summarize(s string) string {
	s = stripAttachBlocks(s)
	s = strings.Join(strings.Fields(s), " ")
	if s == "" {
		return "（附件）"
	}
	const max = 42
	if len([]rune(s)) <= max {
		return s
	}
	return string([]rune(s)[:max]) + "…"
}

// Delete 删除会话文件。不做归档——删就是删。
func (s *store) Delete(id string) error {
	if !validID(id) {
		return fmt.Errorf("session: bad id %q", id)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	_ = os.Remove(s.titlePath(id))
	if err := os.Remove(s.path(id)); err != nil {
		return fmt.Errorf("session: delete %s: %w", id, err)
	}
	return nil
}

// session 单个会话。
type session struct {
	id      string
	path    string
	mu      sync.Mutex
	msgs    []api.Message
	loadded bool
}

func (s *session) ID() string { return s.id }

func (s *session) load() error {
	if s.loadded {
		return nil
	}
	f, err := os.Open(s.path)
	if err != nil {
		return err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 1024*1024), 16*1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var m api.Message
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			return fmt.Errorf("session: corrupt line in %s: %w", s.id, err)
		}
		s.msgs = append(s.msgs, m)
	}
	s.loadded = true
	return nil
}

func (s *session) Append(m api.Message) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.load(); err != nil {
		return err
	}
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(s.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := f.Write(append(b, '\n')); err != nil {
		return err
	}
	s.msgs = append(s.msgs, m)
	return nil
}

func (s *session) Messages() []api.Message {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.load(); err != nil {
		return nil
	}
	out := make([]api.Message, len(s.msgs))
	copy(out, s.msgs)
	return out
}

func validID(id string) bool {
	if id == "" || len(id) > 100 {
		return false
	}
	for _, r := range id {
		ok := r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_'
		if !ok {
			return false
		}
	}
	return true
}

func randSuffix() string {
	b := make([]byte, 4)
	if _, err := cryptorand.Read(b); err != nil {
		return fmt.Sprintf("%x", time.Now().UnixNano()%0xffff)[0:4]
	}
	return hex.EncodeToString(b)
}

// Rename 写自定义标题。空串表示清除，回到自动摘要。
func (s *store) Rename(id, title string) error {
	if !validID(id) {
		return fmt.Errorf("session: bad id %q", id)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := os.Stat(s.path(id)); err != nil {
		return fmt.Errorf("session: %q not found", id)
	}
	title = strings.TrimSpace(title)
	if title == "" {
		if err := os.Remove(s.titlePath(id)); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	}
	const max = 80
	if r := []rune(title); len(r) > max {
		title = string(r[:max])
	}
	return os.WriteFile(s.titlePath(id), []byte(title), 0o644)
}

// stripAttachBlocks 去掉发送时拼进消息的附件内容块。
func stripAttachBlocks(s string) string {
	var b strings.Builder
	lines := strings.Split(s, "\n")
	skip := false
	for _, ln := range lines {
		trim := strings.TrimSpace(ln)
		if strings.HasPrefix(trim, "--- 附件 ") && strings.HasSuffix(trim, "---") {
			skip = !strings.Contains(trim, "附件结束")
			continue
		}
		if skip {
			continue
		}
		b.WriteString(ln)
		b.WriteString("\n")
	}
	return b.String()
}
