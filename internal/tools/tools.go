// Package toolsplugin 工具注册表 + 内建工具。
// 工具也是插件注册进来的能力（统一 catalog / executor）。
package tools

import (
	"context"
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
	cfg := ctx.Config()
	p.reg = &registry{}
	ctx.Provide("toolRegistry", p.reg)

	// 操作审计日志：默认开启，plugins_cfg.tools.audit=false 关闭。
	// 关闭后 Record 是 no-op，不影响任何执行路径。
	auditEnabled := true
	if cfg != nil {
		if v, ok := cfg["audit"].(bool); ok {
			auditEnabled = v
		}
	}
	auditLog, err := NewAuditLog(ctx.DataDir(), auditEnabled)
	if err != nil {
		return err
	}
	ctx.Provide("audit", auditLog)
	ctx.OnShutdown(func() { _ = auditLog.Close() })
	p.reg.audit = auditLog

	// 权限档位：permission 优先；只有旧字段 allow_destructive:true 时视为完全权限。
	perm := PermStandard
	if cfg != nil {
		if v, ok := cfg["permission"].(string); ok && v != "" {
			pv, err := ParsePerm(v)
			if err != nil {
				return err
			}
			perm = pv
		} else if v, ok := cfg["allow_destructive"].(bool); ok && v {
			perm = PermFull
		}
	}
	guard := NewGuard(perm)
	p.reg.guard = guard
	// 权限是运行期状态：Web UI 切档后下一次工具调用立刻按新档位执行。
	ctx.Provide("guard", guard)
	ctx.SetRuntime("permission", perm.String())
	// 权限档位可在运行期切换：界面切档后提示词里的档位说明也要跟着变。
	ctx.Provide("permission", api.PermissionSwitch(&permSwitch{guard: guard, ctx: ctx}))

	// 工作区默认锚定程序目录（而非当前工作目录），配置里的相对路径也相对它解析。
	ws := NewWorkspace(ctx.BaseDir())
	if cfg != nil {
		if v, ok := cfg["workspace"].(string); ok && v != "" {
			if !filepath.IsAbs(v) {
				v = filepath.Join(ctx.BaseDir(), v)
			}
			if err := ws.Set(v); err != nil {
				fmt.Fprintf(os.Stderr, "[tools] 警告：workspace %q 不可用（%v），改用 %s\n", v, err, ws.Root())
			}
		}
		// 白名单默认开启（NewWorkspace）；只有显式配 false 才放行出界路径。
		if v, ok := cfg["workspace_restrict"].(bool); ok {
			ws.SetRestricted(v)
		}
	}
	// 工作区是跨插件共享的运行时状态：Web UI 切目录后，
	// shell 的 cwd 与所有文件工具的路径解析必须同时切换。
	ctx.Provide("workspace", ws)

	bt := &BashTool{ws: ws, guard: guard}
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

	// 技能：目录里放 SKILL.md 就有能力，不用改代码。相对路径锚定程序目录。
	skillDir := filepath.Join(ctx.BaseDir(), "skills")
	if cfg != nil {
		if v, ok := cfg["skills"].(map[string]any); ok {
			if d, ok := v["dir"].(string); ok && d != "" {
				if !filepath.IsAbs(d) {
					d = filepath.Join(ctx.BaseDir(), d)
				}
				skillDir = d
			}
		}
	}
	skills, err := LoadSkills(skillDir)
	if err != nil {
		return fmt.Errorf("tools: 加载技能失败: %w", err)
	}
	p.skills = skills
	// 技能目录也是运行期能力：Web 侧的「工具/技能」面板要能列出它们。
	ctx.Provide("skills", skills)

	// 计划清单：工具与界面共享同一份内存状态。
	todo := &todoStore{}
	ctx.Provide("todo", api.TodoStore(todo))

	all := []api.Tool{
		bt,
		&TodoTool{store: todo},
		&ReadFileTool{ws: ws},
		&WriteFileTool{ws: ws},
		&EditFileTool{ws: ws},
		&GlobTool{ws: ws},
		&GrepTool{ws: ws},
		&ListDirTool{ws: ws},
		&MakeDirTool{ws: ws},
		&MovePathTool{ws: ws},
		&CopyPathTool{ws: ws},
		&DeletePathTool{allowRecursive: bt.allowDestructive, guard: guard, ws: ws},
		&WebFetchTool{},
		&HTTPRequestTool{},
	}
	if skills.Len() > 0 {
		all = append(all, &ListSkillsTool{reg: skills}, &UseSkillTool{reg: skills})
	}
	// 联网检索需显式配置后端（默认 duckduckgo 也要配 proxy 才在多数网络环境可用）。
	// 没配就不注册：给模型一个注定失败的工具比不给更糟。
	if wc, ok := cfg["websearch"].(map[string]any); ok {
		if ws := newWebSearch(wc); ws != nil {
			all = append(all, ws)
		}
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
		fmt.Fprintf(&b, "工作区: %s\n", ws.Root())
		fmt.Fprintf(&b, "权限: %s（%s）\n", guard.Mode(), guard.Mode().Label())
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
	// guard 权限闸门：在唯一的执行入口上拦截，
	// 避免「每个工具各写一遍检查、新增工具漏检」。
	guard *Guard
	// audit 操作审计日志：同样挂在唯一入口上，保证每一条调用（含被拦/不存在）
	// 都被记录，新增工具无需各自埋点。
	audit *AuditLog
	// approver 交互式审批器（TUI 注入，可为 nil）。
	// 标准档位下，高风险工具执行前先问用户 allow/deny/always。
	approver api.ToolApprover
	// alwaysAllowed 本进程内「总是放行」的工具名集合（用户选过 always 后写入）。
	alwaysAllowed map[string]bool
}

// SetApprover 注入/清除交互式审批器。
func (r *registry) SetApprover(a api.ToolApprover) {
	r.mu.Lock()
	r.approver = a
	r.mu.Unlock()
}

// HasApprover 实现 api.ToolRegistry。审批弹窗共用一路输入，必须串行执行。
func (r *registry) HasApprover() bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.approver != nil
}

// riskyTools 标准档位下执行前需确认的工具（破坏性 / 写类）。
// 只读工具（read/glob/grep/list_dir）与网络读取（web_fetch/http_request）不在此列。
var riskyTools = map[string]bool{
	"bash":        true,
	"write_file":  true,
	"edit_file":   true,
	"make_dir":    true,
	"move_path":   true,
	"copy_path":   true,
	"delete_path": true,
}

// needsApproval 判断某工具在当前档位下是否需交互确认。
// 完全权限档位跳过；只读档位由 Guard.Check 直接拦截，不会到这里。
func (r *registry) needsApproval(name string) bool {
	if r.guard != nil && (r.guard.Mode() == PermFull || r.guard.Mode() == PermReadOnly) {
		return false
	}
	return riskyTools[name]
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
	start := time.Now()
	perm := "standard"
	if r.guard != nil {
		perm = r.guard.Mode().String()
	}
	node := ""
	if m, e := argObj(call.Args); e == nil {
		node = argStr(m, "node")
	}

	r.mu.RLock()
	t, ok := r.tools[call.Name]
	r.mu.RUnlock()
	if !ok {
		r.audit.recordFromCall(ctx, call, node, "denied", "unknown tool: "+call.Name, perm, false, nil, 0, time.Since(start).Milliseconds())
		return api.ToolResult{CallID: call.ID, Name: call.Name, Content: "unknown tool: " + call.Name, IsError: true}
	}
	// 只读判定看工具自己的声明：没声明的按「可能写」处理，
	// 这样新加的工具和 MCP 接进来的工具都不会成为只读模式下的写入通道。
	if reason := r.guard.CheckTool(call.Name, t.Spec().ReadOnly); reason != "" {
		r.audit.recordFromCall(ctx, call, node, "blocked", reason, perm, false, nil, 0, time.Since(start).Milliseconds())
		return api.ToolResult{CallID: call.ID, Name: call.Name, Content: "error: " + reason, IsError: true}
	}
	// 交互式确认：标准档位下、高风险工具、且用户没选过「总是放行」时才问。
	r.mu.RLock()
	approver := r.approver
	alwaysNow := r.alwaysAllowed[call.Name]
	r.mu.RUnlock()
	if approver != nil && r.needsApproval(call.Name) {
		always := alwaysNow
		if !always {
			dec, aerr := approver.Approve(ctx, call)
			if aerr != nil {
				r.audit.recordFromCall(ctx, call, node, "denied", "user cancelled: "+aerr.Error(), perm, false, nil, 0, time.Since(start).Milliseconds())
				return api.ToolResult{CallID: call.ID, Name: call.Name, Content: "error: 用户取消了该工具调用", IsError: true}
			}
			if !dec.Allow {
				r.audit.recordFromCall(ctx, call, node, "denied", "user denied", perm, false, nil, 0, time.Since(start).Milliseconds())
				return api.ToolResult{CallID: call.ID, Name: call.Name, Content: "error: 用户拒绝了该工具调用", IsError: true}
			}
			if dec.Always {
				r.mu.Lock()
				if r.alwaysAllowed == nil {
					r.alwaysAllowed = map[string]bool{}
				}
				r.alwaysAllowed[call.Name] = true
				r.mu.Unlock()
			}
		}
	}
	out, err := t.Run(ctx, call.Args)
	res := api.ToolResult{CallID: call.ID, Name: call.Name, Content: out}
	ok2 := true
	if err != nil {
		ok2 = false
		res.IsError = true
		if out == "" {
			res.Content = "error: " + err.Error()
		} else {
			res.Content = out + "\nerror: " + err.Error()
		}
	}
	r.audit.recordFromCall(ctx, call, node, "execute", "", perm, ok2, err, len(out), time.Since(start).Milliseconds())
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

// permSwitch 把 Guard 暴露成 api.PermissionSwitch，并在切档时同步运行时状态。
type permSwitch struct {
	guard *Guard
	ctx   *kernel.Ctx
}

// Mode 返回对外展示的档位名。plan 是把只读档加了「先出方案」的语义，
// 底层权限仍是只读，但对外必须报 plan——否则界面显示「只读」，
// 用户以为自己只切了只读，实际模型被要求先给方案，两边认知对不上。
func (p *permSwitch) Mode() string {
	if v := p.ctx.Runtime("planMode"); v != nil {
		if b, ok := v.(bool); ok && b {
			return "plan"
		}
	}
	return p.guard.Mode().String()
}

func (p *permSwitch) SetMode(name string) error {
	// plan 是「只读 + 先出计划」的组合档：权限按只读执行，
	// 同时把 planMode 打开，让系统提示要求模型先给方案再等确认。
	if strings.EqualFold(strings.TrimSpace(name), "plan") {
		p.guard.Set(PermReadOnly)
		p.ctx.SetRuntime("permission", PermReadOnly.String())
		p.ctx.SetRuntime("planMode", true)
		return nil
	}
	v, err := ParsePerm(name)
	if err != nil {
		return err
	}
	p.guard.Set(v)
	p.ctx.SetRuntime("permission", v.String())
	p.ctx.SetRuntime("planMode", false)
	return nil
}
