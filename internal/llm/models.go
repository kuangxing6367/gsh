package llm

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"

	"gsh/internal/api"
)

// Manager 管理一组具名模型配置，并区分主模型与辅助模型。
//
// 主模型跑对话，辅助模型跑摘要、标题这类机械活——长会话里摘要调用次数不少，
// 全用主模型既慢又贵。没配辅助模型时退回主模型，行为与单模型写法完全一致。
type Manager struct {
	mu     sync.RWMutex
	specs  map[string]map[string]any
	order  []string
	active string
	aux    string
	built  map[string]api.LLM
}

// NewManager 解析配置。两种写法都接受：
//  1. 顶层直接写 provider/base_url/model/api_key —— 单模型（旧配置原样可用）
//  2. 顶层写 active/aux + models:{名字: {模型配置}} —— 多模型
//
// 多模型写法下，顶层公共字段（provider / base_url / api_key / timeout_seconds /
// retry / thinking_format）作为缺省值，每个模型只写自己要覆盖的部分。
func NewManager(cfg map[string]any) (*Manager, error) {
	m := &Manager{
		specs: map[string]map[string]any{},
		built: map[string]api.LLM{},
	}
	if cfg == nil {
		cfg = map[string]any{}
	}
	common := map[string]any{}
	for _, k := range []string{"provider", "base_url", "api_key", "timeout_seconds", "retry", "thinking_format", "params", "thinking"} {
		if v, ok := cfg[k]; ok {
			common[k] = v
		}
	}
	raw, ok := cfg["models"].(map[string]any)
	if !ok || len(raw) == 0 {
		spec := map[string]any{}
		for k, v := range cfg {
			if k == "models" || k == "active" || k == "aux" {
				continue
			}
			spec[k] = v
		}
		m.specs["default"] = spec
		m.order = []string{"default"}
		m.active = "default"
		m.aux = "default"
	} else {
		for name, item := range raw {
			spec := map[string]any{}
			for k, v := range common {
				spec[k] = v
			}
			if sub, ok := item.(map[string]any); ok {
				for k, v := range sub {
					spec[k] = v
				}
			}
			m.specs[name] = spec
			m.order = append(m.order, name)
		}
		sort.Strings(m.order)
		m.active, _ = cfg["active"].(string)
		if m.active == "" || m.specs[m.active] == nil {
			m.active = m.order[0]
		}
		m.aux, _ = cfg["aux"].(string)
		if m.specs[m.aux] == nil {
			m.aux = m.active
		}
	}
	// 预构建并尽早暴露配置错误（provider 拼错、模型表为空等），
	// 而不是等到用户发第一条消息才炸。
	for name, spec := range m.specs {
		built, err := buildLLM(spec)
		if err != nil {
			return nil, fmt.Errorf("llm: 模型 %q: %w", name, err)
		}
		m.built[name] = built
	}
	return m, nil
}

// Names 返回按名字排序的模型列表。
func (m *Manager) Names() []string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return append([]string(nil), m.order...)
}

func (m *Manager) ActiveName() string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.active
}

func (m *Manager) AuxName() string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.aux
}

// Use 切换主模型。运行中的请求不受影响，下一轮起生效。
func (m *Manager) Use(name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.specs[name] == nil {
		return fmt.Errorf("llm: 未知模型 %q（可用: %s）", name, strings.Join(m.order, ", "))
	}
	m.active = name
	return nil
}

// SetAux 指定辅助模型。
func (m *Manager) SetAux(name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.specs[name] == nil {
		return fmt.Errorf("llm: 未知模型 %q", name)
	}
	m.aux = name
	return nil
}

func (m *Manager) pick(aux bool) api.LLM {
	m.mu.RLock()
	defer m.mu.RUnlock()
	name := m.active
	if aux && m.specs[m.aux] != nil {
		name = m.aux
	}
	if l := m.built[name]; l != nil {
		return l
	}
	return m.built[m.active]
}

// CurrentProxy 暴露给内核作 "llm" 服务：每次调用都取当前主模型，
// 于是热切换不需要重启进程，也不需要替换已注入的依赖。
func (m *Manager) CurrentProxy() api.LLM { return &proxy{m: m} }

// AuxProxy 暴露给内核作 "auxLLM" 服务，专供摘要等辅助任务。
func (m *Manager) AuxProxy() api.LLM { return &proxy{m: m, aux: true} }

// Reconfigure 整体重建模型表（Web 设置面板保存时用）。
func (m *Manager) Reconfigure(cfg map[string]any) error {
	next, err := NewManager(cfg)
	if err != nil {
		return err
	}
	m.mu.Lock()
	m.specs, m.order, m.active, m.aux, m.built = next.specs, next.order, next.active, next.aux, next.built
	m.mu.Unlock()
	return nil
}

// proxy 把「取哪个模型」推迟到每次调用，实现无重启切换。
type proxy struct {
	m   *Manager
	aux bool
}

func (p *proxy) Chat(ctx context.Context, msgs []api.Message, tools []api.ToolSpec, h api.StreamHandlers) (api.Message, error) {
	return p.m.pick(p.aux).Chat(ctx, msgs, tools, h)
}

func (p *proxy) LastUsage() api.Usage {
	if r, ok := p.m.pick(p.aux).(api.UsageReporter); ok {
		return r.LastUsage()
	}
	return api.Usage{}
}

// ContextWindow 透传当前模型的窗口大小；未知时为 0。
func (p *proxy) ContextWindow() int {
	if r, ok := p.m.pick(p.aux).(api.ContextWindowReporter); ok {
		return r.ContextWindow()
	}
	return 0
}

// EstimateTokens 透传当前模型的估算口径。
func (p *proxy) EstimateTokens(msgs []api.Message) int {
	if e, ok := p.m.pick(p.aux).(api.TokenEstimator); ok {
		return e.EstimateTokens(msgs)
	}
	return 0
}

func (p *proxy) Configure(cfg map[string]any) error { return p.m.Reconfigure(cfg) }
