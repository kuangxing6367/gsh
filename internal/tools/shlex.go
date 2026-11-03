package tools

import (
	"fmt"
	"strings"
)

// splitCommand 按 POSIX shell 的引用规则把命令行拆成 argv。
//
// 存在的理由：环境里可能根本没有 shell（精简容器、scratch 镜像、被裁掉的
// busybox、Windows 上只有 cmd 而模型写的是 POSIX）。此时「不执行」是最差的
// 选择——模型想跑 `git status` 这种单条命令，根本不需要 shell。
//
// 边界要说清楚：这里只处理引用与转义，**不做**变量展开、通配符、管道、
// 重定向、命令替换。那些是 shell 的语义，没有 shell 就不该假装有——
// 假装的结果是模型以为 `cd x && ls` 生效了，实际只跑了 `cd`。
func splitCommand(s string) ([]string, error) {
	var (
		out  []string
		cur  strings.Builder
		have bool // cur 里是否已开始累积一个 token（区分空字符串参数 ""）
	)
	rs := []rune(s)
	for i := 0; i < len(rs); i++ {
		c := rs[i]
		switch c {
		case ' ', '\t', '\n', '\r':
			if have {
				out = append(out, cur.String())
				cur.Reset()
				have = false
			}
		case '\'':
			// 单引号内一切原样，直到下一个单引号。
			have = true
			j := i + 1
			for ; j < len(rs) && rs[j] != '\''; j++ {
				cur.WriteRune(rs[j])
			}
			if j >= len(rs) {
				return nil, fmt.Errorf("未闭合的单引号")
			}
			i = j
		case '"':
			have = true
			j := i + 1
			for ; j < len(rs) && rs[j] != '"'; j++ {
				if rs[j] == '\\' && j+1 < len(rs) {
					nx := rs[j+1]
					// 双引号内只有这几个字符可转义，其余反斜杠保留原义。
					if nx == '"' || nx == '\\' || nx == '$' || nx == '`' || nx == '\n' {
						cur.WriteRune(nx)
						j++
						continue
					}
				}
				cur.WriteRune(rs[j])
			}
			if j >= len(rs) {
				return nil, fmt.Errorf("未闭合的双引号")
			}
			i = j
		case '\\':
			have = true
			if i+1 < len(rs) {
				cur.WriteRune(rs[i+1])
				i++
			}
		default:
			have = true
			cur.WriteRune(c)
		}
	}
	if have {
		out = append(out, cur.String())
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("空命令")
	}
	return out, nil
}

// shellQuote 把 argv 还原成一条 POSIX 命令行字符串。
// 用途：argv 形式要发到远程节点时，后端只接受一条命令字符串，
// 必须把每个参数单独转义，否则参数里的空格/引号会在远端被重新切词。
func shellQuote(argv []string) string {
	parts := make([]string, 0, len(argv))
	for _, a := range argv {
		if a == "" {
			parts = append(parts, "''")
			continue
		}
		if !strings.ContainsAny(a, " \t\n'\"\\$`&|;<>()*?[]#~!") {
			parts = append(parts, a)
			continue
		}
		parts = append(parts, "'"+strings.ReplaceAll(a, "'", `'\''`)+"'")
	}
	return strings.Join(parts, " ")
}

// needsShell 判断一条命令是否用到了 shell 语义（这些在直连模式下无法还原）。
// 用途不是拒绝执行，而是把「你这条命令我只能部分执行」提前告诉调用方。
func needsShell(s string) (string, bool) {
	inSingle, inDouble := false, false
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '\\' && !inSingle:
			i++
		case c == '\'' && !inDouble:
			inSingle = !inSingle
		case c == '"' && !inSingle:
			inDouble = !inDouble
		case inSingle || inDouble:
			// 引号内不算 shell 语义
		case c == '|':
			return "管道 |", true
		case c == '>':
			return "重定向 >", true
		case c == '<':
			return "重定向 <", true
		case c == ';':
			return "命令分隔 ;", true
		case c == '&':
			return "后台/逻辑与 &", true
		case c == '`':
			return "命令替换 `", true
		case c == '$':
			return "变量展开 $", true
		}
	}
	return "", false
}
