// Package hooks 让外部命令插手工具执行流程。
//
// 两处可插：工具执行之前（可以拦下）和执行之后（可以补一句注）。
// 判定权交给一段外部命令而不是内置规则——团队缺的规则千奇百怪
// （禁 rm -rf、禁改生产配置、必须带 issue 号），只有用户自己写得最准。
package hooks

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"strings"
	"time"
	"unicode/utf8"

	"golang.org/x/text/encoding/simplifiedchinese"

	"gsh/internal/api"
	"gsh/internal/kernel"
)

// 单条钩子命令的默认超时。钩子是同步阻塞执行路径上的，
// 卡住就等于整个会话卡住，所以必须给个兜底时限。
const defaultTimeout = 10 * time.Second

// RegisterKernel 挂载钩子插件。必须排在 tools 之后：它要读工具名做匹配。
func RegisterKernel(k *kernel.Kernel) error { return k.Load(&Plugin{}) }

// Plugin 钩子插件。
type Plugin struct{}

// Name 实现 kernel.Plugin。
func (p *Plugin) Name() string { return "hooks" }

// Apply 解析配置里的钩子表并发布服务。
func (p *Plugin) Apply(ctx *kernel.Ctx) error {
	r := &runner{}
	cfg := ctx.Config()
	if cfg != nil {
		r.before = parseList(cfg["before_tool"])
		r.after = parseList(cfg["after_tool"])
	}
	ctx.Provide("hooks", api.HookRunner(r))
	return nil
}

// rule 是一条编译好的钩子。
type rule struct {
	match   *regexp.Regexp // 工具名正则；为空表示全部匹配
	command string
	timeout time.Duration
}

// hit 判断这条钩子是否作用于该工具。
func (r rule) hit(tool string) bool {
	return r.match == nil || r.match.MatchString(tool)
}

// runner 实现 api.HookRunner。
type runner struct {
	before []rule
	after  []rule
}

// noteMax 单条钩子补注进入上下文的上限。
//
// 钩子输出会直接接在工具结果后面进模型上下文。不加限制的话，
// 一条 `cat big.log` 之类的规则就能让每次工具调用都多带几十 KB，
// 几轮下来直接撞窗口——而且用户完全看不出是钩子干的。
const noteMax = 4096

// Run 依次执行该阶段命中的全部钩子。
// before_tool 任一钩子返回非零即拦截；after_tool 的输出按顺序累积成补注。
func (r *runner) Run(ctx context.Context, ev api.HookEvent) api.HookResult {
	rules := r.before
	if ev.Phase == api.HookAfterTool {
		rules = r.after
	}
	var notes []string
	for _, h := range rules {
		if !h.hit(ev.Tool) {
			continue
		}
		out, ok := runOne(ctx, h, ev)
		if ev.Phase == api.HookBeforeTool {
			if !ok {
				msg := strings.TrimSpace(out)
				if msg == "" {
					msg = "被钩子拦下（命令以非零码退出）"
				}
				return api.HookResult{Blocked: true, Message: clipNote(msg)}
			}
			continue
		}
		if s := strings.TrimSpace(out); s != "" {
			notes = append(notes, clipNote(s))
		}
	}
	if len(notes) == 0 {
		return api.HookResult{}
	}
	return api.HookResult{Message: clipNote(strings.Join(notes, "\n"))}
}

// clipNote 把补注文本截到上限内，并按字符边界回退，
// 免得把中文切成半个字（非法字节会一路带进 JSON 和模型上下文）。
func clipNote(s string) string {
	if len(s) <= noteMax {
		return s
	}
	cut := noteMax
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "\n…[钩子输出已截断]"
}

// envMax 单个环境变量的安全长度。
//
// Windows 的整个环境块上限是 32KB，而工具参数与结果都可能到几十上百 KB
// （write_file 的 content、bash 的一段构建日志）。整段塞进环境变量会让
// 子进程直接起不来，报的还是「环境过大」这种看不出原因的错。超过这条线
// 就改走临时文件，环境里只留路径。
const envMax = 4096

// runOne 执行一条钩子，返回合并的 stdout+stderr 与是否成功。
func runOne(ctx context.Context, h rule, ev api.HookEvent) (string, bool) {
	timeout := h.timeout
	if timeout <= 0 {
		timeout = defaultTimeout
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	var cmd *exec.Cmd
	if runtime.GOOS == "windows" {
		// 先切到 UTF-8 代码页：默认 GBK 下钩子输出的中文到模型那里就是乱码。
		cmd = exec.CommandContext(cctx, "cmd", "/C", "chcp 65001>nul && "+h.command)
	} else {
		cmd = exec.CommandContext(cctx, "sh", "-c", h.command)
	}
	// 超时后光杀 sh/cmd 是不够的：真正干活的是它派生的子进程，
	// 那一条会活下来继续占资源。交给平台实现去杀整棵进程树。
	killTree(cmd)
	// 进程被杀的瞬间它的子进程可能还攥着 stdout 管道。没有这条，
	// cmd.Run 会卡在等 IO 上，超时形同虚设。
	cmd.WaitDelay = 2 * time.Second

	// 事件字段走环境变量而不是参数拼接：参数里可能带引号、换行、中文，
	// 拼进命令行必然被转义问题咬。
	argsEnv, argsClean := envField("GSH_ARGS", ev.Args)
	defer argsClean()
	resultEnv, resultClean := envField("GSH_RESULT", ev.Result)
	defer resultClean()
	cmd.Env = append(cmd.Environ(),
		"GSH_PHASE="+ev.Phase,
		"GSH_TOOL="+ev.Tool,
		argsEnv,
		"GSH_SESSION="+ev.SessionID,
		resultEnv,
		"GSH_ERROR="+boolText(ev.IsError),
	)
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	if err := cmd.Run(); err != nil {
		if cctx.Err() == context.DeadlineExceeded {
			return "钩子命令超时（" + timeout.String() + "）", false
		}
		// 父上下文被取消（用户按了中断）不是钩子的判决，
		// 不能把它当成「钩子拒绝执行」回给模型。
		if ctx.Err() != nil {
			return "钩子命令被中断", false
		}
		return decode(buf.Bytes()), false
	}
	return decode(buf.Bytes()), true
}

// envField 返回一条 `KEY=VALUE` 环境项和清理函数。
//
// 小字段直接进环境变量；大字段写到 0600 的临时文件，环境项变成
// `GSH_ARGS_FILE=<路径>` / `GSH_RESULT_FILE=<路径>`，脚本按需读文件。
// 返回的必须是完整的一条 KEY=VALUE——裸名字和裸值作为独立元素塞进
// cmd.Env 会让环境块格式非法，子进程直接起不来。
func envField(name, val string) (string, func()) {
	if len(val) <= envMax {
		return name + "=" + val, func() {}
	}
	f, err := os.CreateTemp("", "gsh-hook-*")
	if err != nil {
		// 建不了临时文件就退回截断，至少别让钩子完全跑不起来。
		return name + "=" + val[:envMax], func() {}
	}
	defer f.Close()
	if _, err := f.WriteString(val); err != nil {
		os.Remove(f.Name())
		return name + "=" + val[:envMax], func() {}
	}
	p := f.Name()
	return name + "_FILE=" + p, func() { os.Remove(p) }
}

// parseList 把配置里的一段钩子数组编译成规则表。
// 单条写字符串（命令本身）或对象（command/match/timeout_seconds）都可以。
func parseList(v any) []rule {
	items, ok := v.([]any)
	if !ok {
		return nil
	}
	var out []rule
	for _, it := range items {
		switch t := it.(type) {
		case string:
			if cmd := strings.TrimSpace(t); cmd != "" {
				out = append(out, rule{command: cmd})
			}
		case map[string]any:
			cmd, _ := t["command"].(string)
			if strings.TrimSpace(cmd) == "" {
				continue
			}
			r := rule{command: cmd}
			if pat, _ := t["match"].(string); strings.TrimSpace(pat) != "" {
				re, err := regexp.Compile(pat)
				if err != nil {
					// 正则写错就跳过这条，而不是让整个进程起不来。
					continue
				}
				r.match = re
			}
			if f, ok := t["timeout_seconds"].(float64); ok && f > 0 {
				r.timeout = time.Duration(f * float64(time.Second))
			}
			out = append(out, r)
		}
	}
	return out
}

// decode 把钩子输出尽量还原成 UTF-8。
// Windows 的 cmd 可能按本地代码页（中文机器是 GBK）输出，
// 直接当 UTF-8 透传会让模型读到一串乱码，比没有输出更糟。
func decode(b []byte) string {
	if utf8.Valid(b) {
		return string(b)
	}
	if out, err := simplifiedchinese.GBK.NewDecoder().Bytes(b); err == nil {
		return string(out)
	}
	return string(b)
}

func boolText(b bool) string {
	if b {
		return "1"
	}
	return "0"
}
