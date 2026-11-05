package web

import (
	"crypto/rand"
	"encoding/hex"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// 上传上限与允许的类型。图片是唯一支持的多模态输入；
// 拒绝其余类型比事后清理误传的文件省事得多。
const maxUploadBytes = 10 << 20

var allowedImageExt = map[string]bool{
	".png": true, ".jpg": true, ".jpeg": true, ".gif": true, ".webp": true, ".bmp": true,
}

// handleUpload POST multipart(file) → 落地到 uploads 目录，返回可引用的路径与 URL。
// 文件落在工作区之外：不能让模型通过附件路径把自己的产物混进仓库。
func (w *webAPI) handleUpload(rw http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(rw, r.Body, maxUploadBytes)
	if err := r.ParseMultipartForm(maxUploadBytes); err != nil {
		httpError(rw, http.StatusBadRequest, "解析上传失败: "+err.Error())
		return
	}
	file, hdr, err := r.FormFile("file")
	if err != nil {
		httpError(rw, http.StatusBadRequest, "缺少 file 字段")
		return
	}
	defer file.Close()

	ext := strings.ToLower(filepath.Ext(hdr.Filename))
	if !allowedImageExt[ext] {
		httpError(rw, http.StatusBadRequest, "只支持 png/jpg/gif/webp/bmp 图片")
		return
	}
	if err := os.MkdirAll(w.uploadDir, 0o700); err != nil {
		httpError(rw, http.StatusInternalServerError, err.Error())
		return
	}
	name := randName() + ext
	dst := filepath.Join(w.uploadDir, name)
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		httpError(rw, http.StatusInternalServerError, err.Error())
		return
	}
	defer out.Close()
	if _, err := io.Copy(out, file); err != nil {
		httpError(rw, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(rw, map[string]any{
		"path": dst,
		"name": hdr.Filename,
		"url":  "/api/asset?name=" + name,
	})
	// 顺手清理：放在响应之后，不拖慢这次上传。
	go pruneUploads(w.uploadDir)
}

// handleAsset GET ?name= → 回吐已上传的图片（前端缩略图用）。
// 只认 uploads 目录里的裸文件名：带路径分隔符的一律拒绝，避免变成任意文件读取。
func (w *webAPI) handleAsset(rw http.ResponseWriter, r *http.Request) {
	name := r.URL.Query().Get("name")
	if name == "" || name != filepath.Base(name) || strings.Contains(name, "..") {
		httpError(rw, http.StatusBadRequest, "非法文件名")
		return
	}
	if !allowedImageExt[strings.ToLower(filepath.Ext(name))] {
		httpError(rw, http.StatusNotFound, "文件不存在")
		return
	}
	http.ServeFile(rw, r, filepath.Join(w.uploadDir, name))
}

// randName 生成随机文件名，避免同名覆盖。
func randName() string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return "upload"
	}
	return hex.EncodeToString(b)
}

// insideUploads 判断路径是否落在上传目录内。
func (w *webAPI) insideUploads(p string) bool {
	abs, err := filepath.Abs(p)
	if err != nil {
		return false
	}
	root, err := filepath.Abs(w.uploadDir)
	if err != nil {
		return false
	}
	rel, err := filepath.Rel(root, abs)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// 上传目录的保留策略。这个目录只增不减，长期使用会攒到几个 G——
// 而它装的全是可以重新上传一次的东西，没有留档价值。
const (
	uploadKeepAge = 7 * 24 * time.Hour
	uploadMaxSize = 512 << 20
)

// pruneUploads 清掉过期与超额的上传文件，返回删除的数量。
//
// 两条规则各自独立：过期的一律删；总量超标时按 mtime 从旧到新删到阈值以下。
// 调用的地方都在上传之后或启动时，删的是旧文件，正在用的这张不受影响。
func pruneUploads(dir string) int {
	des, err := os.ReadDir(dir)
	if err != nil {
		return 0
	}
	type item struct {
		path string
		size int64
		mod  time.Time
	}
	items := make([]item, 0, len(des))
	var total int64
	deadline := time.Now().Add(-uploadKeepAge)
	removed := 0
	for _, de := range des {
		if de.IsDir() {
			continue
		}
		info, ierr := de.Info()
		if ierr != nil {
			continue
		}
		p := filepath.Join(dir, de.Name())
		if info.ModTime().Before(deadline) && os.Remove(p) == nil {
			removed++
			continue
		}
		items = append(items, item{path: p, size: info.Size(), mod: info.ModTime()})
		total += info.Size()
	}
	if total <= uploadMaxSize {
		return removed
	}
	// 旧的先走：新上传的图更可能正被当前对话引用。
	sort.Slice(items, func(i, j int) bool { return items[i].mod.Before(items[j].mod) })
	for _, it := range items {
		if total <= uploadMaxSize {
			break
		}
		if os.Remove(it.path) == nil {
			total -= it.size
			removed++
		}
	}
	return removed
}
