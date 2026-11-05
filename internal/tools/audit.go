package tools

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"gsh/internal/api"
)

// AuditLog 操作审计日志：append-only JSONL，记录每一次工具调用的元数据。
//
// 存在的理由：gsh 未来要嵌进沙箱 / 运维体系，这类场景下「谁在什么时候让 agent
// 跑了什么、有没有被安全策略拦下来」是刚需，不是可选项。审计落盘与「会话消息」
// 是两套流：会话流面向回放，审计流面向合规与事后追溯，二者目的不同，拆开才不互相拖累。
//
// 默认开启；config 的 plugins_cfg.tools.audit=false 可关闭（关闭后 Record 是 no-op）。
// 审计只记元数据与脱敏后的参数，绝不记工具输出原文或文件内容——避免把命令回显、
// 读到的文件、泄露的密钥再写一份到磁盘。
type AuditLog struct {
	mu      sync.Mutex
	f       *os.File
	enabled bool
}

// NewAuditLog 在 <dataDir>/audit/audit.log 打开（或创建）审计文件。
// enabled=false 时仍返回可用实例，只是 Record 不落盘。
func NewAuditLog(dataDir string, enabled bool) (*AuditLog, error) {
	if !enabled {
		return &AuditLog{enabled: false}, nil
	}
	dir := filepath.Join(dataDir, "audit")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("audit: mkdir %s: %w", dir, err)
	}
	f, err := os.OpenFile(filepath.Join(dir, "audit.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, fmt.Errorf("audit: open: %w", err)
	}
	return &AuditLog{f: f, enabled: true}, nil
}

// Close 关闭审计文件（内核停机时调用）。
func (a *AuditLog) Close() error {
	if a == nil || a.f == nil {
		return nil
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.f.Close()
}

// Enabled 是否实际落盘（config 关闭时为 false）。
func (a *AuditLog) Enabled() bool { return a != nil && a.enabled }

// AuditEntry 一条审计记录。
type AuditEntry struct {
	TS        string `json:"ts"`                // RFC3339 时间戳
	Session   string `json:"session"`           // 会话 id（无会话为 "-"）
	Tool      string `json:"tool"`              // 工具名
	Args      string `json:"args"`              // 脱敏后的参数 JSON（保留结构）
	Node      string `json:"node,omitempty"`    // 远程节点名（本地为空）
	Action    string `json:"action"`            // denied | blocked | execute
	Blocked   string `json:"blocked,omitempty"` // 拦截原因（guard 拦 / 工具不存在）
	Perm      string `json:"perm"`              // 执行时权限档位
	Ok        bool   `json:"ok"`                // 执行是否成功
	Err       string `json:"err,omitempty"`     // 错误信息（截断）
	ResultLen int    `json:"result_len"`        // 结果内容长度（不记内容）
	DurMs     int64  `json:"dur_ms"`            // 执行耗时（毫秒）
}

// Record 写一条审计记录。
func (a *AuditLog) Record(e AuditEntry) {
	if a == nil || !a.enabled || a.f == nil {
		return
	}
	if e.TS == "" {
		e.TS = time.Now().UTC().Format(time.RFC3339)
	}
	if e.Session == "" {
		e.Session = "-"
	}
	b, err := json.Marshal(e)
	if err != nil {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	_, _ = a.f.Write(append(b, '\n'))
}

// Recent 读取最近 n 条审计记录（新→旧）。
func (a *AuditLog) Recent(n int) ([]AuditEntry, error) {
	if a == nil || !a.enabled {
		return []AuditEntry{}, nil
	}
	path := ""
	a.mu.Lock()
	if a.f != nil {
		path = a.f.Name()
	}
	a.mu.Unlock()
	if path == "" {
		return []AuditEntry{}, nil
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	// 文件可能很大：只保留末尾 n 行，用定长环形缓冲，避免整文件读入。
	ring := make([]string, 0, n)
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		if len(ring) >= n {
			ring = ring[1:]
		}
		ring = append(ring, line)
	}
	out := make([]AuditEntry, 0, len(ring))
	for _, line := range ring {
		var e AuditEntry
		if err := json.Unmarshal([]byte(line), &e); err == nil {
			out = append(out, e)
		}
	}
	// ring 已是旧→新；审计面板要新→旧，反转。
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out, nil
}

// RecordFromCall 从一次工具调用上下文构造并写入审计记录，是 registry.Execute 的收口点。
func (a *AuditLog) recordFromCall(ctx context.Context, call api.ToolCall, node, action, blocked, perm string, ok bool, err error, resultLen int, durMs int64) {
	if a == nil || !a.enabled {
		return
	}
	e := AuditEntry{
		Session:   "-",
		Tool:      call.Name,
		Args:      redactArgs(call.Args),
		Node:      node,
		Action:    action,
		Blocked:   blocked,
		Perm:      perm,
		Ok:        ok,
		ResultLen: resultLen,
		DurMs:     durMs,
	}
	if s, ok2 := ctx.Value(api.AuditSessionKey).(string); ok2 && s != "" {
		e.Session = s
	}
	if err != nil {
		msg := err.Error()
		if len(msg) > 300 {
			msg = msg[:300] + "…"
		}
		e.Err = msg
	}
	a.Record(e)
}

// ---------- 脱敏 ----------

var (
	reDeepseekKey = regexp.MustCompile(`sk-[A-Za-z0-9]{20,}`)
	reBearer      = regexp.MustCompile(`(?i)Bearer\s+[A-Za-z0-9._\-]{8,}`)
	reCredPair    = regexp.MustCompile(`(?i)(api[_-]?key|token|secret|password|passwd|access[_-]?token|authorization)=[^\s"'<>]+`)
)

// redactArgs 把工具参数 JSON 里的敏感值脱敏，保留结构用于审计。
// 做法：解析为 map，对敏感键名整体掩码；对字符串值再扫一遍明确的密钥模式
// （DeepSeek key、Bearer token、key=value 形式的凭据）。命令本身（command/argv）
// 是审计核心，不会被整体抹掉，只会把其中偶然粘贴的密钥片段掩码。
func redactArgs(argsJSON string) string {
	if strings.TrimSpace(argsJSON) == "" {
		return ""
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(argsJSON), &m); err != nil {
		// 不是合法 JSON（理论上不会有）：直接对原串做模式掩码兜底。
		return redactSecrets(argsJSON)
	}
	redactMap(m)
	b, err := json.Marshal(m)
	if err != nil {
		return redactSecrets(argsJSON)
	}
	return string(b)
}

func redactMap(m map[string]any) {
	for k, v := range m {
		if isSensitiveKey(k) {
			m[k] = "***REDACTED***"
			continue
		}
		switch val := v.(type) {
		case string:
			m[k] = redactSecrets(val)
		case map[string]any:
			redactMap(val)
		case []any:
			for i := range val {
				if s, ok := val[i].(string); ok {
					val[i] = redactSecrets(s)
				}
			}
		}
	}
}

func isSensitiveKey(k string) bool {
	lk := strings.ToLower(k)
	for _, s := range []string{"token", "secret", "password", "passwd", "api_key", "apikey", "authorization", "bearer", "credential", "private_key", "access_token"} {
		if strings.Contains(lk, s) {
			return true
		}
	}
	return false
}

func redactSecrets(s string) string {
	s = reDeepseekKey.ReplaceAllString(s, "sk-***REDACTED***")
	s = reBearer.ReplaceAllString(s, "Bearer ***REDACTED***")
	s = reCredPair.ReplaceAllString(s, "$1=***REDACTED***")
	return s
}
