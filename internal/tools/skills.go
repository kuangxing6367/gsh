package tools

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gsh/internal/api"
)

// userAgent 出网请求的默认 UA（不暴露运行时细节）。
const userAgent = "gsh-agent/0.1"

// userAgentPlaceholder 占位，避免常量被误删。
var _ = userAgent

// ---------- skills ----------

// Skill 一项能力包：一个含 SKILL.md 的目录。
//
// 这里刻意采用「渐进式披露」：list_skills 只暴露索引（name + description），
// 正文要点名 use_skill 才加载。原因是把全部技能正文塞进上下文，会挤掉真正的
// 任务信息，工具越全反而越笨——能力范围靠目录扩展，上下文按需消耗。
type Skill struct {
	Name        string
	Description string
	Dir         string // 技能目录（资源、脚本都在里面）
	Path        string // SKILL.md 路径
}

// SkillRegistry 从目录加载的技能表。
type SkillRegistry struct {
	dir    string
	skills []Skill
}

// LoadSkills 扫描目录加载技能。
// 识别两种布局：<dir>/<name>/SKILL.md 与 <dir>/<name>.md。
func LoadSkills(dir string) (*SkillRegistry, error) {
	reg := &SkillRegistry{dir: dir}
	if dir == "" {
		return reg, nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return reg, nil // 目录不存在 = 没有技能，不是错误
		}
		return nil, err
	}
	for _, e := range entries {
		if e.IsDir() {
			p := filepath.Join(dir, e.Name(), "SKILL.md")
			if _, err := os.Stat(p); err == nil {
				if sk, err := readSkill(p, filepath.Join(dir, e.Name())); err == nil {
					reg.skills = append(reg.skills, sk)
				}
			}
			continue
		}
		if strings.EqualFold(filepath.Ext(e.Name()), ".md") {
			p := filepath.Join(dir, e.Name())
			if sk, err := readSkill(p, dir); err == nil {
				reg.skills = append(reg.skills, sk)
			}
		}
	}
	sort.Slice(reg.skills, func(i, j int) bool { return reg.skills[i].Name < reg.skills[j].Name })
	return reg, nil
}

// Len 技能数量。
func (r *SkillRegistry) Len() int {
	if r == nil {
		return 0
	}
	return len(r.skills)
}

// List 全部技能。
func (r *SkillRegistry) List() []Skill {
	if r == nil {
		return nil
	}
	return r.skills
}

// Find 按名字查找（大小写不敏感）。
func (r *SkillRegistry) Find(name string) (Skill, bool) {
	if r == nil {
		return Skill{}, false
	}
	for _, s := range r.skills {
		if strings.EqualFold(s.Name, name) {
			return s, true
		}
	}
	return Skill{}, false
}

// readSkill 解析 SKILL.md 的 frontmatter 与正文。
func readSkill(path, dir string) (Skill, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return Skill{}, err
	}
	sk := Skill{Path: path, Dir: dir}
	body := string(raw)
	if name, desc, rest, ok := splitFrontmatter(body); ok {
		sk.Name, sk.Description = name, desc
		body = rest
	}
	if sk.Name == "" {
		sk.Name = strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
		if filepath.Base(path) == "SKILL.md" {
			sk.Name = filepath.Base(dir)
		}
	}
	if sk.Description == "" {
		// 无 frontmatter 时取正文首行非空行作描述
		for _, ln := range strings.Split(body, "\n") {
			ln = strings.TrimSpace(strings.TrimPrefix(ln, "#"))
			if ln != "" {
				sk.Description = ln
				break
			}
		}
	}
	return sk, nil
}

// splitFrontmatter 解析 --- 包裹的简易 YAML（只取 name/description）。
func splitFrontmatter(s string) (name, desc, rest string, ok bool) {
	t := strings.TrimPrefix(strings.ReplaceAll(s, "\r\n", "\n"), "\ufeff")
	if !strings.HasPrefix(t, "---\n") {
		return "", "", s, false
	}
	end := strings.Index(t[4:], "\n---")
	if end < 0 {
		return "", "", s, false
	}
	head := t[4 : 4+end]
	rest = strings.TrimPrefix(t[4+end+4:], "\n")
	for _, ln := range strings.Split(head, "\n") {
		kv := strings.SplitN(ln, ":", 2)
		if len(kv) != 2 {
			continue
		}
		key := strings.TrimSpace(kv[0])
		val := strings.TrimSpace(kv[1])
		val = strings.Trim(val, `"'`)
		switch strings.ToLower(key) {
		case "name":
			name = val
		case "description", "summary":
			desc = val
		}
	}
	return name, desc, rest, true
}

// ---------- list_skills ----------

// ListSkillsTool 列出可用技能索引。
type ListSkillsTool struct{ reg *SkillRegistry }

// Spec 工具描述。
func (t *ListSkillsTool) Spec() api.ToolSpec {
	return api.ToolSpec{
		Name:        "list_skills",
		ReadOnly:    true,
		Description: "List available skills (name + one-line description). Skills are on-demand playbooks; call use_skill to read the full instructions before doing a task that matches one.",
		Parameters:  map[string]any{"type": "object", "properties": map[string]any{}},
	}
}

// Run 返回索引。
func (t *ListSkillsTool) Run(_ context.Context, _ string) (string, error) {
	if t.reg.Len() == 0 {
		return "（没有已安装的技能：把含 SKILL.md 的目录放到 plugins_cfg.tools.skills.dir 下即可）", nil
	}
	var b strings.Builder
	dir := t.reg.dir
	if abs, err := filepath.Abs(dir); err == nil {
		dir = abs
	}
	fmt.Fprintf(&b, "技能目录：%s\n\n", dir)
	for _, s := range t.reg.skills {
		fmt.Fprintf(&b, "- %s: %s\n", s.Name, s.Description)
	}
	b.WriteString("\n用 use_skill(name) 读取正文。")
	return b.String(), nil
}

// ---------- use_skill ----------

// UseSkillTool 加载某个技能的完整说明。
type UseSkillTool struct{ reg *SkillRegistry }

// Spec 工具描述。
func (t *UseSkillTool) Spec() api.ToolSpec {
	return api.ToolSpec{
		Name:        "use_skill",
		ReadOnly:    true,
		Description: "Load the full instructions of a skill by name (see list_skills). Returns the SKILL.md body plus its directory, so you can read bundled files or run its scripts.",
		Parameters: map[string]any{
			"type":       "object",
			"properties": map[string]any{"name": map[string]any{"type": "string"}},
			"required":   []string{"name"},
		},
	}
}

// Run 返回技能正文。
func (t *UseSkillTool) Run(_ context.Context, argsJSON string) (string, error) {
	m, err := argObj(argsJSON)
	if err != nil {
		return "", err
	}
	name := argStr(m, "name")
	if name == "" {
		return "", fmt.Errorf("name is required")
	}
	sk, ok := t.reg.Find(name)
	if !ok {
		var names []string
		for _, s := range t.reg.List() {
			names = append(names, s.Name)
		}
		return "", fmt.Errorf("未知技能 %q（可用：%s）", name, strings.Join(names, ", "))
	}
	raw, err := os.ReadFile(sk.Path)
	if err != nil {
		return "", err
	}
	body := string(raw)
	if _, _, rest, ok := splitFrontmatter(body); ok {
		body = rest
	}
	body = clipAt(body, maxToolOutput)
	return fmt.Sprintf("[skill: %s]\n目录: %s\n\n%s", sk.Name, sk.Dir, strings.TrimSpace(body)), nil
}
