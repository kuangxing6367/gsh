// Package prompt 组装系统提示词。
//
// 提示词分三层拼装，顺序固定（越靠后越具体，越能覆盖前面的泛化指令）：
//  1. 内置三节：角色与工作方式 / 沟通规范 / 工作区与工具
//  2. 项目级上下文：工作区根目录的 AGENTS.md 或 GSH.md
//  3. 人格附加提示：配置里选中的 persona
//
// 动态信息（工作区路径、系统、权限档位、工具清单、时间）在每次 Build 时重算，
// 因为工具集可能随权限切换而变化，缓存整段提示会拿到过期的工具名单。
package prompt

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"gsh/internal/api"
	"gsh/internal/kernel"
)

// 项目上下文文件的体积上限。超出即截断，避免一份巨大的说明把上下文吃满。
const maxContextBytes = 32 << 10

// 候选项目上下文文件名，按优先级排列，命中第一个即停。
var contextFiles = []string{"AGENTS.md", "GSH.md", "agents.md"}

// RegisterKernel 挂载提示词插件。
func RegisterKernel(k *kernel.Kernel) error { return k.Load(&Plugin{}) }

// Plugin 提示词插件。
type Plugin struct{}

// Name 实现 kernel.Plugin。
func (p *Plugin) Name() string { return "prompt" }

// Apply 读取配置并注册 PromptBuilder 服务。
func (p *Plugin) Apply(ctx *kernel.Ctx) error {
	o := Options{
		BaseDir: ctx.BaseDir(),
		Config:  ctx.Config(),
		Mode:    ctx.RuntimeString("permission"),
		Kernel:  kernel.Current,
	}
	ctx.Provide("promptBuilder", New(o))
	return nil
}

// Options 构造参数。
type Options struct {
	BaseDir   string
	Workspace string
	Mode      string
	Config    map[string]any
	Kernel    *kernel.Kernel
}

// Builder 系统提示词组装器，并发安全（Build 不写自身状态）。
type Builder struct {
	workspace   string
	mode        string
	custom      string
	persona     string
	project     string
	projectFrom string
	k           *kernel.Kernel
	now         func() time.Time
}

// currentWorkspace 优先取 tools 插件的实时工作区；服务不可用时退回构造时的路径。
func (b *Builder) currentWorkspace() string {
	if b.k != nil {
		if ws, err := kernel.Use[api.WorkspaceInfo](b.k, "workspace"); err == nil && ws.Root() != "" {
			return ws.Root()
		}
	}
	return b.workspace
}

// New 按配置构造组装器。缺失项一律留空降级，绝不让提示词环节阻断启动。
func New(o Options) *Builder {
	b := &Builder{
		workspace: o.Workspace,
		mode:      o.Mode,
		k:         o.Kernel,
		now:       time.Now,
	}
	if b.workspace == "" {
		b.workspace = o.BaseDir
	}
	if b.workspace == "" {
		if wd, err := os.Getwd(); err == nil {
			b.workspace = wd
		}
	}
	if b.mode == "" {
		b.mode = "standard"
	}
	if o.Config != nil {
		b.custom, _ = o.Config["system"].(string)
		b.persona = selectPersona(o.Config)
	}
	b.project, b.projectFrom = readProjectContext(b.workspace)
	return b
}

// selectPersona 取 active 指定的人格提示；personas 支持字符串或 {name: prompt} 映射。
func selectPersona(cfg map[string]any) string {
	raw, ok := cfg["personas"]
	if !ok {
		return ""
	}
	active, _ := cfg["active"].(string)
	switch ps := raw.(type) {
	case map[string]any:
		if active == "" {
			return ""
		}
		s, _ := ps[active].(string)
		return strings.TrimSpace(s)
	case []any:
		// 也接受 [{name, prompt}] 形式，顺序无关。
		for _, item := range ps {
			m, ok := item.(map[string]any)
			if !ok || active == "" {
				continue
			}
			name, _ := m["name"].(string)
			if name == active {
				s, _ := m["prompt"].(string)
				return strings.TrimSpace(s)
			}
		}
	}
	return ""
}

// readProjectContext 从工作区根读项目说明；找不到返回空串。
func readProjectContext(root string) (string, string) {
	if root == "" {
		return "", ""
	}
	for _, name := range contextFiles {
		p := filepath.Join(root, name)
		b, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		s := strings.TrimSpace(string(b))
		if s == "" {
			continue
		}
		if len(b) > maxContextBytes {
			s = string(b[:maxContextBytes]) + "\n\n（项目说明过长，已截断）"
		}
		return s, p
	}
	return "", ""
}

// Build 拼出完整的系统提示。tools 为空时也给出明确说明，避免模型凭空猜能力。
func (b *Builder) Build(tools []api.ToolSpec) string {
	var s strings.Builder
	ws := b.currentWorkspace()
	// 项目说明随工作区实时重读：切换目录后原先那份约定不该继续生效。
	project, projectFrom := readProjectContext(ws)

	s.WriteString("# 角色与工作方式\n")
	s.WriteString("你是在用户本机终端里工作的 AI 编程助手。你通过工具读写文件、执行命令、检索信息，")
	s.WriteString("并在一轮对话里反复「推理 → 调用工具 → 看结果 → 再推理」直到任务真正完成。\n\n")
	s.WriteString("- 先看清再动手：改动前先读相关文件，不臆测内容。\n")
	s.WriteString("- 能并行的独立动作尽量一次性发起，不要挤牙膏。\n")
	s.WriteString("- 工具报错时先读懂错误、调整参数再试；连续失败要向用户说明卡在哪，而不是反复盲试。\n")
	s.WriteString("- 只做被要求的事，不顺手重构无关代码；改动范围越小越好。\n")
	s.WriteString("- 不编造事实：没查到的路径、版本、运行结果就如实说没查到。\n\n")

	s.WriteString("# 沟通规范\n")
	s.WriteString("- 结论先行，中文作答；技术名词可保留英文原文。\n")
	s.WriteString("- 不复述用户的话，不写空洞的客套与总结段。\n")
	s.WriteString("- 改了什么就列改了什么，路径用相对工作区的写法。\n")
	s.WriteString("- 有不确定处明确标出来，不要把猜测写成结论。\n\n")

	s.WriteString("# 运行环境\n")
	s.WriteString(fmt.Sprintf("- 工作区：%s\n", ws))
	s.WriteString(fmt.Sprintf("- 系统：%s/%s\n", runtime.GOOS, runtime.GOARCH))
	s.WriteString(fmt.Sprintf("- 权限档位：%s\n", b.mode))
	s.WriteString(fmt.Sprintf("- 本地时间：%s\n", b.now().Format("2006-01-02 15:04")))
	s.WriteString("- 路径规则：所有相对路径都相对上面的工作区解析；越界访问会被拒绝，不要尝试绕过。\n\n")

	s.WriteString("# 可用工具\n")
	if len(tools) == 0 {
		s.WriteString("当前没有启用任何工具。只能基于已知信息回答，不要声称自己读过或改过文件。\n")
	} else {
		names := make([]string, 0, len(tools))
		for _, t := range tools {
			desc := strings.TrimSpace(t.Description)
			if i := strings.IndexByte(desc, '\n'); i >= 0 {
				desc = desc[:i]
			}
			if r := []rune(desc); len(r) > 90 {
				desc = string(r[:90]) + "…"
			}
			if desc == "" {
				names = append(names, t.Name)
			} else {
				names = append(names, fmt.Sprintf("%s — %s", t.Name, desc))
			}
		}
		sort.Strings(names)
		for _, n := range names {
			s.WriteString("- " + n + "\n")
		}
		s.WriteString("\n调用工具时参数必须是合法 JSON；写操作会受权限档位约束，必要时等待用户批准。\n")
	}

	// 有索引时点明它的存在：模型不知道有这个工具，就只会一路 grep。
	if b.k != nil {
		if ci, err := kernel.Use[api.CodeIndex](b.k, "codeIndex"); err == nil && ci != nil {
			if files, syms := ci.Stats(); files > 0 {
				s.WriteString("\n# 代码索引\n")
				s.WriteString(fmt.Sprintf("当前工作区已索引 %d 个文件、%d 个定义。要定位某个函数/类/组件在哪，先用 code_search，", files, syms))
				s.WriteString("拿到文件与行号后再 read_file 精读；只有索引查不到时才退回 grep。\n")
			}
		}
	}

	// 长期记忆来自过去的会话；放在项目约定之前——项目约定更贴近眼前这份代码，
	// 两者冲突时以当前仓库为准。
	if b.k != nil {
		if mem, err := kernel.Use[api.MemoryStore](b.k, "memory"); err == nil && mem != nil {
			if t := strings.TrimSpace(mem.Text()); t != "" {
				s.WriteString("\n# 长期记忆\n")
				s.WriteString("以下是此前会话里记下、默认仍然有效的事实。与实际观察冲突时以实际为准，并告知用户。\n\n")
				s.WriteString(t + "\n")
			}
		}
	}

	if project != "" {
		s.WriteString("\n# 项目约定\n")
		s.WriteString(fmt.Sprintf("以下来自 %s，优先级高于上面的通用规范：\n\n", filepath.Base(projectFrom)))
		s.WriteString(project + "\n")
	}
	// 计划模式：只读权限 + 先出方案。提示里必须把「不要动手」说清楚，
	// 否则模型碰到写工具被拒会反复重试，而不是停下来解释打算怎么做。
	if b.k != nil {
		if v, _ := b.k.Value("planMode").(bool); v {
			s.WriteString("\n# 计划模式\n")
			s.WriteString("当前处于计划模式：写文件、改文件和执行命令都已禁用，只有只读操作可用。\n")
			s.WriteString("你的任务是先给出可执行方案，而不是动手。方案里要写清：涉及哪些文件、")
			s.WriteString("每处改什么、为什么这么改、有哪些风险和不确定的地方。\n")
			s.WriteString("可以读文件、可以搜索来把方案做实；方案给出后停下等用户确认，不要试探性地去写。\n")
		}
	}
	if b.persona != "" {
		s.WriteString("\n# 当前人格补充\n")
		s.WriteString(b.persona + "\n")
	}
	if b.custom != "" {
		s.WriteString("\n# 自定义指令\n")
		s.WriteString(strings.TrimSpace(b.custom) + "\n")
	}
	return s.String()
}

// ProjectContextPath 返回实际命中的项目说明路径，供诊断输出（未命中为空）。
func (b *Builder) ProjectContextPath() string { return b.projectFrom }
