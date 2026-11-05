// Package tools 安全策略：命令风险扫描。
// 设计动机：一条无约束的 shell 命令就足以清空整块磁盘，破坏性调用必须显式放行。
// gsh 把 bash 工具默认置于"安全优先"姿态：灾难性命令无论配置如何一律拦截；
// 破坏性强但日常有用的命令（递归删除、git 破坏性操作、下载管道执行等）默认拦截，
// 需显式 allow_destructive:true 才放行。
package tools

import (
	"regexp"
)

// alwaysRules 灾难性 / 不可逆命令：无论 allow_destructive 如何都拒绝执行。
// 这类命令一旦跑错目标就是数据永久丢失，不该交给 agent 自动执行。
var alwaysRules = []struct {
	re     *regexp.Regexp
	reason string
}{
	{regexp.MustCompile(`(?i)\brm\b[^\n]*--no-preserve-root`), "rm --no-preserve-root（绕过根目录保护）"},
	{regexp.MustCompile(`(?i)\brm\b[^\n;&|]*?\s(?:/\*?|~/\*?|~|\$HOME/\*?|\$HOME|\*)(?:\s|$)`), "rm 作用于根目录 / 通配 / 家目录（不可逆）"},
	// 顶层系统目录：/etc、/usr、/home 本身（或其下通配）才算致命；
	// /home/user/project 这类具体子目录交给 gated 规则，避免误判成灾难级。
	{regexp.MustCompile(`(?i)\brm\b[^\n;&|]*?\s/(?:etc|usr|bin|sbin|lib|lib64|boot|dev|sys|proc|root|home|var|opt|srv|tmp)(?:/\*)?(?:\s|$)`), "rm 作用于系统目录（不可逆）"},
	{regexp.MustCompile(`(?i)\brm\b[^\n;&|]*?\s(?:-[a-z]*[rf][a-z]*\s+){1,3}\.\.`), "rm 作用于上级目录 ..（越界删除）"},
	{regexp.MustCompile(`(?i)\b(?:del|erase)\b[^\n]*(?:\s/[sq]+\b|\s/q\s/s|\s/s\s/q)`), "Windows 递归删除 del /s（不可逆）"},
	{regexp.MustCompile(`(?i)\b(?:rd|rmdir)\b[^\n]*\s/s\b`), "Windows 递归删除 rd/rmdir /s"},
	{regexp.MustCompile(`(?i)\b(?:format|mkfs|wipefs|fdisk|parted)\b`), "磁盘格式化 / 分区操作"},
	{regexp.MustCompile(`(?i)\bdd\b[^\n]*\bof=(?:/dev/|\\\.\\PHYSICALDRIVE)`), "dd 写入块设备（覆盖整盘）"},
	{regexp.MustCompile(`(?i):\(\)\s*\{`), "fork bomb（:(){ :|:& };:）"},
	{regexp.MustCompile(`(?i)\b(?:shutdown|reboot|halt|poweroff|init\s+0|telinit\s+0)\b`), "系统关机 / 重启"},
	{regexp.MustCompile(`(?i)>\s*/dev/(?:sd|hd|nvm|vd|sda)`), "重定向覆写块设备"},
	{regexp.MustCompile(`(?i)\bch(?:mod|own)\b[^\n]*\s-R\s+/(?:\s|$)`), "递归 chmod/chown 根目录"},
	{regexp.MustCompile(`(?i)\btruncate\b[^\n]*\s-s\s*0\b[^\n]*\s/`), "truncate 清空根目录文件"},
}

// gatedRules 破坏性但日常有用的命令：allow_destructive=false 时拦截。
var gatedRules = []struct {
	re     *regexp.Regexp
	reason string
}{
	{regexp.MustCompile(`(?i)\brm\b[^\n]*\s-[a-z]*r`), "递归删除 rm -r（破坏性；放行需 allow_destructive:true）"},
	{regexp.MustCompile(`(?i)\b(?:rd|rmdir)\b[^\n]*\s-[a-z]*s`), "递归删除 rmdir -s（破坏性）"},
	{regexp.MustCompile(`(?i)\b(?:del|erase)\b[^\n]*\s-[a-z]*[sq]`), "Windows 删除 del /s|/q（破坏性）"},
	{regexp.MustCompile(`(?i)\bgit\b[^\n]*\bclean\b[^\n]*\s-[a-z]*[fdx]`), "git clean -fdx（未跟踪文件全删）"},
	{regexp.MustCompile(`(?i)\bgit\b[^\n]*\breset\b[^\n]*--hard`), "git reset --hard（丢弃工作区修改）"},
	{regexp.MustCompile(`(?i)\bgit\b[^\n]*\bcheckout\b[^\n]*(?:\s\.\s*$|\s--\s*\.)`), "git checkout .（覆盖全部文件）"},
	{regexp.MustCompile(`(?i)(?:curl|wget|fetch)\b[^\n]*\|\s*(?:ba)?sh\b`), "下载脚本直接管道执行（供应链高风险）"},
	{regexp.MustCompile(`(?i)\bch(?:mod|own)\b[^\n]*\s-R\b`), "递归改权限 chmod/chown -R（破坏性；放行需 allow_destructive）"},
	{regexp.MustCompile(`(?i)\bfind\b[^\n]*\s-delete\b`), "find -delete（批量删文件）"},
	{regexp.MustCompile(`(?i)\btruncate\b[^\n]*\s-s\s*0\b`), "truncate -s 0（清空文件）"},
}

// scanCommand 返回命令命中的规则。always 非空表示致命（永远拦截）；
// gated 非空表示受限（仅 allow_destructive=false 时拦截）。
func scanCommand(cmd string) (always string, gated string) {
	for _, r := range alwaysRules {
		if r.re.MatchString(cmd) {
			return r.reason, ""
		}
	}
	for _, r := range gatedRules {
		if r.re.MatchString(cmd) {
			return "", r.reason
		}
	}
	return "", ""
}
