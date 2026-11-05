package harness

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"gsh/internal/api"
	"gsh/internal/ui"
)

// ArchiveReport 归档结果。
type ArchiveReport struct {
	Monthly  map[string]int // 月份 → 归档会话数
	Sessions int
	Dir      string
	Pruned   int
}

// ArchiveBefore 把早于 days 天的会话导出成按月分组的 Markdown 归档。
//
// 归档只「导出」，不「搬迁」：会话文件仍在原地，搜索和回看照样能用。
// 想把磁盘腾出来是另一件事（prune=true 才删），两件事分开做，
// 避免「归档」这个动作顺手把资料删了。
func (h *Harness) ArchiveBefore(days int, prune bool) (*ArchiveReport, error) {
	if days <= 0 {
		days = 7
	}
	store, err := Use[api.SessionStore](h, "sessionStore")
	if err != nil {
		return nil, err
	}
	metas, err := store.ListMeta()
	if err != nil {
		return nil, err
	}
	dir := filepath.Join(h.k.DataDirFor("session"), "archive")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	cutoff := time.Now().AddDate(0, 0, -days)
	rep := &ArchiveReport{Monthly: map[string]int{}, Dir: dir}

	// 已归档过的会话 id。不带这个索引的话，同一个会话每跑一次 -archive
	// 就会被再写一遍（归档不删原文件），月度文件迅速变成重复内容的堆。
	seen := readArchivedIndex(dir)

	// 同一批会话按月份归到同一个文件，归档是给「翻旧账」用的，
	// 一个会话一个文件只会变成另一种形态的垃圾堆。
	byMonth := map[string][]string{}
	var done []string
	for _, m := range metas {
		if m.Updated.After(cutoff) {
			continue
		}
		if seen[m.ID] {
			continue
		}
		sess, oerr := store.Open(m.ID)
		if oerr != nil {
			continue
		}
		month := m.Updated.Format("2006-01")
		body := ui.SessionToMarkdown(sess.Messages(), m.Title, m.ID)
		byMonth[month] = append(byMonth[month], body)
		rep.Monthly[month]++
		rep.Sessions++
		done = append(done, m.ID)
	}
	months := make([]string, 0, len(byMonth))
	for m := range byMonth {
		months = append(months, m)
	}
	sort.Strings(months)
	for _, month := range months {
		path := filepath.Join(dir, month+".md")
		f, ferr := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
		if ferr != nil {
			return nil, ferr
		}
		if st, serr := f.Stat(); serr == nil && st.Size() == 0 {
			fmt.Fprintf(f, "# gsh 会话归档 %s\n\n", month)
		}
		for _, body := range byMonth[month] {
			fmt.Fprintf(f, "\n---\n\n%s\n", strings.TrimSpace(body))
		}
		if cerr := f.Close(); cerr != nil {
			// 内容没落稳就别往下走，尤其不能在此时删源文件。
			return nil, cerr
		}
	}

	// 删除放在写完之后：反过来的话，一旦写盘失败就是
	// 「原文件没了、归档也没写成」，且不可恢复。
	//
	// 除了本次刚归档的，此前归档过但没删的也要一并清掉——
	// 否则「先 -archive、后补 -archive-prune」这种用法永远删不动，
	// 因为那些会话已经被索引跳过、不会出现在本次的 done 里。
	if prune {
		thisRun := make(map[string]bool, len(done))
		for _, id := range done {
			thisRun[id] = true
		}
		for _, m := range metas {
			if m.Updated.After(cutoff) {
				continue
			}
			if !thisRun[m.ID] && !seen[m.ID] {
				continue
			}
			if derr := store.Delete(m.ID); derr == nil {
				rep.Pruned++
			}
		}
	}
	// 只在确认写成功后登记，失败时下次还能重试。
	if err := appendArchivedIndex(dir, done); err != nil {
		return rep, err
	}
	return rep, nil
}

// archivedIndexFile 记录已归档的会话 id，一行一个。
const archivedIndexFile = ".archived"

// readArchivedIndex 读已归档索引；文件不存在或损坏时按空处理，
// 最坏结果只是重复归档一次，不该让整个命令失败。
func readArchivedIndex(dir string) map[string]bool {
	out := map[string]bool{}
	data, err := os.ReadFile(filepath.Join(dir, archivedIndexFile))
	if err != nil {
		return out
	}
	for _, line := range strings.Split(string(data), "\n") {
		if id := strings.TrimSpace(line); id != "" {
			out[id] = true
		}
	}
	return out
}

func appendArchivedIndex(dir string, ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	f, err := os.OpenFile(filepath.Join(dir, archivedIndexFile), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	for _, id := range ids {
		if _, werr := fmt.Fprintln(f, id); werr != nil {
			return werr
		}
	}
	return nil
}
