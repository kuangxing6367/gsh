// Package toolsplugin 工具注册表 + 内建工具。
// 工具也是插件注册进来的能力（对齐 DSH 的 tool catalog / executor）。
package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"

	"gsh/internal/api"
	"gsh/internal/kernel"
)

// RegisterKernel 挂载工具插件（含全部内建工具）。
func RegisterKernel(k *kernel.Kernel) error { return k.Load(&Plugin{}) }

// Plugin 工具注册表插件。
type Plugin struct {
	reg    *registry
	skills *SkillRegistry
}

// Name 实现 kernel.Plugin。
func (p *Plugin) Name() string { return "tools" }

// Apply 注册 ToolRegistry 服务，并把内建工具挂进去。
func (p *Plugin) Apply(ctx *kernel.Ctx) error {
	p.reg = &registry{}
	ctx.Provide("toolRegistry", p.reg)

	cfg := ctx.Config()
	bt := &BashTool{}
	if cfg != nil {
		if v, ok := cfg["allow_destructive"].(bool); ok {
			bt.allowDestructive = v
		}
		// shell 候选链：配置优先，缺省用平台默认链（见 shell.go）。
		// 显式指定（"bash" / ["bash","sh"]）意味着「我就用这个」，
		// 探测不到时应当硬报错，而不是悄悄降级成直连执行。
		if raw, ok := cfg["shell"]; ok {
			chain, explicit, err := parseShellConfig(raw)
			if err != nil {
				return err
			}
			bt.shellChain = chain
			bt.requireShell = explicit
		}
		if v, ok := cfg["workspace"].(string); ok && v != "" {
			bt.workspace = v
		}
		// 远程节点配置：plugins_cfg.tools.remote.{nodes, ssh_bin}
		if rc, ok := cfg["remote"].(map[string]any); ok {
			if nb, ok := rc["nodes"].(map[string]any); ok {
				nodes := map[string]NodeSpec{}
				for name, raw := range nb {
					rm, ok := raw.(map[string]any)
					if !ok {
						continue
					}
					ns := NodeSpec{}
					if v, ok := rm["host"].(string); ok {
						ns.Host = v
					}
					if v, ok := rm["user"].(string); ok {
						ns.User = v
					}
					if v, ok := rm["identity"].(string); ok {
						ns.Identity = v
					}
					if v, ok := rm["port"].(float64); ok {
						ns.Port = int(v)
					}
					if v, ok := rm["exec"].(string); ok {
						ns.Exec = v
					}
					if v, ok := rm["args"].([]any); ok {
						for _, a := range v {
							if s, ok := a.(string); ok {
								ns.Args = append(ns.Args, s)
							}
						}
					}
					// 两种后端任一成立即有效：ssh 直连（host+user）或外部命令（exec）。
					validSSH := ns.Host != "" && ns.User != ""
					validExec := ns.Exec != ""
					if validSSH || validExec {
						nodes[name] = ns
					}
				}
				bt.nodes = nodes
			}
			if v, ok := rc["ssh_bin"].(string); ok && v != "" {
				bt.sshBin = v
			}
		}
	}

	// 技能：目录里放 SKILL.md 就有能力，不用改代码。
	skillDir := "skills"
	if cfg != nil {
		if v, ok := cfg["skills"].(map[string]any); ok {
			if d, ok := v["dir"].(string); ok && d != "" {
				skillDir = d
			}
		}
	}
	skills, err := LoadSkills(skillDir)
	if err != nil {
		return fmt.Errorf("tools: 加载技能失败: %w", err)
	}
	p.skills = skills

	all := []api.Tool{
		bt,
		&ReadFileTool{},
		&WriteFileTool{},
		&EditFileTool{},
		&GlobTool{},
		&GrepTool{},
		&ListDirTool{},
		&MakeDirTool{},
		&MovePathTool{},
		&CopyPathTool{},
		&DeletePathTool{allowRecursive: bt.allowDestructive},
		&WebFetchTool{},
		&HTTPRequestTool{},
	}
	if skills.Len() > 0 {
		all = append(all, &ListSkillsTool{reg: skills}, &UseSkillTool{reg: skills})
	}

	// 工具集可裁剪：enabled 非空时作白名单，disabled 永远排除。
	known := make(map[string]bool, len(all))
	for _, t := range all {
		known[t.Spec().Name] = true
	}
	enabled := stringSet(cfg["enabled"])
	disabled := stringSet(cfg["disabled"])
	for _, t := range all {
		name := t.Spec().Name
		if len(enabled) > 0 && !enabled[name] {
			continue
		}
		if disabled[name] {
			continue
		}
		if err := p.reg.Register(t); err != nil {
			return err
		}
	}

	// 诊断信息：让「当前用哪个 shell、有哪些工具、装了几个技能」可见。
	// 这类决策如果只能靠读源码猜，就等于没做配置化。
	ctx.Provide("toolInfo", func() string {
		var b strings.Builder
		chain := bt.shellChain
		if len(chain) == 0 {
			chain = defaultShellChain()
		}
		names := make([]string, 0, len(chain))
		for _, sp := range chain {
			names = append(names, sp.String())
		}
		fmt.Fprintf(&b, "shell 候选链: %s\n", strings.Join(names, " → "))
		if sp, cands, err := bt.pickShell(context.Background()); err != nil {
			fmt.Fprintf(&b, "shell 解析: 失败 — %v\n", err)
			for _, c := range cands {
				fmt.Fprintf(&b, "  - %-12s %s\n", c.Spec.Bin, c.Reason)
			}
		} else {
			fmt.Fprintf(&b, "shell 解析: %s（方言 %s）\n", sp.String(), sp.Family)
		}
		fmt.Fprintf(&b, "工具 (%d): %s\n", len(p.reg.Names()), strings.Join(p.reg.Names(), ", "))
		fmt.Fprintf(&b, "技能 (%d): %s\n", skills.Len(), strings.Join(skillNames(skills), ", "))
		return b.String()
	})

	// 配了不存在的工具名，说明是拼错了——提示但不阻断启动。
	for _, set := range []struct {
		label string
		m     map[string]bool
	}{{"enabled", enabled}, {"disabled", disabled}} {
		for n := range set.m {
			if !known[n] {
				fmt.Fprintf(os.Stderr, "[tools] 警告：%s 里的 %q 不是已知工具（现有：%s）\n",
					set.label, n, strings.Join(sortedKeys(known), ", "))
			}
		}
	}
	return nil
}

// stringSet 把配置里的字符串数组转成集合。
func stringSet(v any) map[string]bool {
	out := map[string]bool{}
	arr, ok := v.([]any)
	if !ok {
		return out
	}
	for _, item := range arr {
		if s, ok := item.(string); ok && s != "" {
			out[s] = true
		}
	}
	return out
}

func skillNames(r *SkillRegistry) []string {
	if r == nil || r.Len() == 0 {
		return []string{"（无）"}
	}
	out := make([]string, 0, r.Len())
	for _, s := range r.List() {
		out = append(out, s.Name)
	}
	return out
}

func sortedKeys(m map[string]bool) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// registry ToolRegistry 实现。
type registry struct {
	mu    sync.RWMutex
	tools map[string]api.Tool
}

func (r *registry) Register(t api.Tool) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.tools == nil {
		r.tools = map[string]api.Tool{}
	}
	name := t.Spec().Name
	if _, dup := r.tools[name]; dup {
		return fmt.Errorf("tools: duplicate tool %q", name)
	}
	r.tools[name] = t
	return nil
}

func (r *registry) Execute(ctx context.Context, call api.ToolCall) api.ToolResult {
	r.mu.RLock()
	t, ok := r.tools[call.Name]
	r.mu.RUnlock()
	if !ok {
		return api.ToolResult{CallID: call.ID, Name: call.Name, Content: "unknown tool: " + call.Name, IsError: true}
	}
	out, err := t.Run(ctx, call.Args)
	res := api.ToolResult{CallID: call.ID, Name: call.Name, Content: out}
	if err != nil {
		res.IsError = true
		if out == "" {
			res.Content = "error: " + err.Error()
		} else {
			res.Content = out + "\nerror: " + err.Error()
		}
	}
	return res
}

func (r *registry) Specs() []api.ToolSpec {
	r.mu.RLock()
	defer r.mu.RUnlock()
	specs := make([]api.ToolSpec, 0, len(r.tools))
	for _, t := range r.tools {
		specs = append(specs, t.Spec())
	}
	sort.Slice(specs, func(i, j int) bool { return specs[i].Name < specs[j].Name })
	return specs
}

func (r *registry) Names() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	names := make([]string, 0, len(r.tools))
	for n := range r.tools {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// argObj 把工具参数 JSON 解析为 map。
func argObj(argsJSON string) (map[string]any, error) {
	if argsJSON == "" {
		return map[string]any{}, nil
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(argsJSON), &m); err != nil {
		return nil, fmt.Errorf("bad arguments json: %w", err)
	}
	return m, nil
}

func argStr(m map[string]any, key string) string {
	if v, ok := m[key].(string); ok {
		return v
	}
	return ""
}

// argStrs 读字符串数组参数（如 argv）。元素非字符串时跳过，避免整体失败。
func argStrs(m map[string]any, key string) []string {
	arr, ok := m[key].([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(arr))
	for _, item := range arr {
		if s, ok := item.(string); ok && s != "" {
			out = append(out, s)
		}
	}
	return out
}
