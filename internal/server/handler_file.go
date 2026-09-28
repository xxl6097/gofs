package server

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/uuxia/gofs/internal/auth"
	"github.com/uuxia/gofs/internal/fsutil"
)

// maxFormMemory 表单上传时内存中保留的上限，超出部分落临时文件。
const maxFormMemory = 32 << 20

// applyUploadLayout 依据配置把上传目标重写到日期归档目录下。
//
// **归档只在「直接传到服务根目录」时生效**：urlPath="/pic.jpg" 会变成
// "/2026/09/28/pic.jpg"。传到子目录（"/sub/a.txt"、"docs/b.txt"）时原样落盘 ——
// 子目录本身通常已经是有意义的分层（项目名、月份、业务线…），再往里套一层
// 年/月/日 只会让目录越陷越深，反而不好找。
//
// 查询参数可以覆盖这个默认行为：
//
//	?dated=0（false/no/off）  跳过归档，脚本要精确指定落点时用
//	?dated=1（true/yes/on）   强制归档，即使在子目录里
func (s *Server) applyUploadLayout(r *http.Request, urlPath string) string {
	clean := fsutil.CleanURLPath(urlPath)
	dateDir := s.cfg.UploadDateDir(time.Now())
	if dateDir == "" {
		return clean
	}
	dated := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("dated")))
	switch dated {
	case "0", "false", "no", "off":
		return clean
	case "1", "true", "yes", "on":
		// 显式要求归档，位置校验交给下面的调用方。
	default:
		// 只在根目录的直属文件上归档。path.Dir("/a.txt") == "/"，
		// 而 path.Dir("/sub/a.txt") == "/sub"。
		if path.Dir(clean) != "/" {
			return clean
		}
	}
	dir, name := path.Split(clean)
	return path.Join(dir, dateDir, name)
}

// handlePut 处理上传：普通覆盖写、以及 X-Update-Range: append 追加写（用于断点续传）。
func (s *Server) handlePut(w http.ResponseWriter, r *http.Request, urlPath string, p permResult) {
	if !s.cfg.AllowUpload {
		http.Error(w, "403 Forbidden: 未开启上传（--allow-upload）", http.StatusForbidden)
		return
	}
	if !s.requireWrite(w, p) {
		return
	}
	if urlPath == "/" || strings.HasSuffix(urlPath, "/") {
		http.Error(w, "400 Bad Request: 上传目标必须是文件路径", http.StatusBadRequest)
		return
	}
	// 文件名长度检查放在最前面：底层文件系统的报错是 ENAMETOOLONG，
	// 直接透出去会变成 500，看起来像服务端故障，实际是请求不合法。
	if !s.checkNameLength(w, urlPath) {
		return
	}

	// 上传会长时间占住连接并吃磁盘 IO，先取一个重任务名额。
	release, ok := s.acquireJob(r.Context())
	if !ok {
		w.Header().Set("Retry-After", "2")
		http.Error(w, "503 Service Unavailable: 上传任务过多，请稍后重试", http.StatusServiceUnavailable)
		return
	}
	defer release()

	// 放宽本次请求的读取时限：大文件上传需要时间。
	// 用的是「无进展超时」——只要还在往里传就不会被打断，
	// 只有连接真正停滞（比如每秒发几个字节占着名额）才会被断开。
	withReadProgress(w, r, s.cfg.UploadReadTimeout)
	// 单文件大小上限：没有它，一个请求就能把磁盘写满。
	// 取值走 settings 而不是 cfg —— 它可以在页面上被管理员随时调整。
	maxSize := s.settings.UploadMaxSize()
	if !requestBody(w, r, maxSize, "上传内容") {
		return
	}

	// 按配置归档到 年/月/日 三级目录，再基于最终路径复核一次。
	destPath := s.applyUploadLayout(r, urlPath)
	if destPath != fsutil.CleanURLPath(urlPath) {
		if p.UploadKey != nil {
			// 密钥场景没有 Basic 凭据，改用密钥自身的范围来判定。
			if !p.UploadKey.AllowsPath(destPath) {
				http.Error(w, "403 Forbidden: 归档目录超出该密钥允许的范围", http.StatusForbidden)
				return
			}
		} else if s.checkPerm(r, destPath) != auth.PermReadWrite {
			http.Error(w, "403 Forbidden: 对归档目录没有写权限", http.StatusForbidden)
			return
		}
	}
	abs, err := s.res.Resolve(destPath)
	if err != nil {
		http.Error(w, "403 Forbidden", http.StatusForbidden)
		return
	}

	appendMode := strings.EqualFold(strings.TrimSpace(r.Header.Get("X-Update-Range")), "append")
	flags := os.O_CREATE | os.O_WRONLY
	if appendMode {
		flags |= os.O_APPEND
	} else {
		flags |= os.O_TRUNC
	}

	// 目标已存在且是目录时拒绝。
	if st, err := os.Stat(abs); err == nil && st.IsDir() {
		http.Error(w, "409 Conflict: 同名目录已存在", http.StatusConflict)
		return
	}
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		s.writeErr(w, err)
		return
	}

	f, err := os.OpenFile(abs, flags, 0o644)
	if err != nil {
		s.writeErr(w, err)
		return
	}
	defer f.Close()

	n, err := io.Copy(f, r.Body)
	if err != nil {
		// 超限时 MaxBytesReader 会以这个错误中断读取。
		if isBodyTooLarge(err) {
			// 非追加模式下磁盘上留着半截文件，必须删掉 —— 否则攻击者可以
			// 反复发超限请求，每次都留下一个文件，磁盘照样会被撑爆。
			// 追加模式则要保留原内容：那是断点续传的续接点。
			if !appendMode {
				_ = f.Close()
				if rmErr := os.Remove(abs); rmErr != nil && !errors.Is(rmErr, fs.ErrNotExist) {
					s.logger.Errorf("清理超限的半截文件 %s 失败: %v", abs, rmErr)
				}
			}
			s.logger.Errorf("上传 %s 超过单文件上限 %d 字节，已中止", destPath, maxSize)
			http.Error(w, fmt.Sprintf("413 Payload Too Large: 超过单文件上限 %s（%d 字节）"+
				"；管理员可在「服务设置」里调整该上限",
				humanSize(maxSize), maxSize), http.StatusRequestEntityTooLarge)
			return
		}
		// 其它中断（网络断开等）：已写入部分保留，便于客户端续传。
		s.logger.Errorf("写入 %s 中断: %v", abs, err)
		http.Error(w, "500 写入失败: "+err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("X-Gofs-Offset", fmt.Sprintf("%d", n))
	// 告知客户端文件的真实落盘路径（归档开启时与请求路径不同）。
	// HTTP 头只能承载 ASCII，因此这里做百分号编码，中文名不会变成乱码。
	w.Header().Set("X-Gofs-Path", quotePath(destPath))
	if appendMode {
		w.Header().Set("X-Gofs-Appended", "true")
	}
	w.WriteHeader(http.StatusCreated)
	fmt.Fprintf(w, "created %s (%d bytes)\n", destPath, n)
}

// handlePost 处理表单上传（浏览器 <input type="file" multiple> 与拖拽兜底）。
// 表单字段：file（可多个）、path（相对当前目录的目标子目录）。
func (s *Server) handlePost(w http.ResponseWriter, r *http.Request, urlPath string, p permResult) {
	if !s.cfg.AllowUpload {
		http.Error(w, "403 Forbidden: 未开启上传（--allow-upload）", http.StatusForbidden)
		return
	}
	if !s.requireWrite(w, p) {
		return
	}

	ct := r.Header.Get("Content-Type")
	if !strings.HasPrefix(ct, "multipart/form-data") {
		http.Error(w, "400 Bad Request: 仅支持 multipart/form-data", http.StatusBadRequest)
		return
	}

	// 与 PUT 同样的三道闸门：重任务名额、读取时限、请求体上限。
	// 表单尤其需要限总量 —— 一个请求可以带任意多个文件。
	release, ok := s.acquireJob(r.Context())
	if !ok {
		w.Header().Set("Retry-After", "2")
		http.Error(w, "503 Service Unavailable: 上传任务过多，请稍后重试", http.StatusServiceUnavailable)
		return
	}
	defer release()
	withReadProgress(w, r, s.cfg.UploadReadTimeout)
	formMax := s.settings.UploadMaxSize()
	if !requestBody(w, r, formMax, "表单内容") {
		return
	}

	if err := r.ParseMultipartForm(maxFormMemory); err != nil {
		if isBodyTooLarge(err) {
			http.Error(w, fmt.Sprintf("413 Payload Too Large: 表单总量超过上限 %s",
				humanSize(formMax)), http.StatusRequestEntityTooLarge)
			return
		}
		s.writeErr(w, err)
		return
	}
	defer func() {
		if r.MultipartForm != nil {
			_ = r.MultipartForm.RemoveAll()
		}
	}()

	// 目标目录 = 当前目录 [+ 表单指定的子路径] [+ 年/月/日 归档目录]。
	destDir := fsutil.CleanURLPath(urlPath)
	if sub := strings.TrimSpace(r.FormValue("path")); sub != "" {
		if fsutil.HasParentSegment(sub) {
			http.Error(w, "400 Bad Request: 非法的目标路径", http.StatusBadRequest)
			return
		}
		destDir = path.Join(destDir, sub)
	}
	// 归档的判断与 handlePut 保持一致：只在「直接传到根目录」时按年月日建三级目录，
	// 传到子目录（含表单显式指定的 path）就原地放。
	// dated 字段可以覆盖：0 跳过归档，1 强制归档。
	var skipDated, forceDated bool
	switch strings.ToLower(strings.TrimSpace(r.FormValue("dated"))) {
	case "0", "false", "no", "off":
		skipDated = true
	case "1", "true", "yes", "on":
		forceDated = true
	}
	if !skipDated && (forceDated || destDir == "/") {
		if dateDir := s.cfg.UploadDateDir(time.Now()); dateDir != "" {
			destDir = path.Join(destDir, dateDir)
		}
	}
	if p.UploadKey != nil {
		// 密钥场景没有 Basic 凭据，改用密钥自身的范围来判定。
		if !p.UploadKey.AllowsPath(destDir) {
			http.Error(w, "403 Forbidden: 目标目录超出该密钥允许的范围", http.StatusForbidden)
			return
		}
	} else if s.checkPerm(r, destDir) != auth.PermReadWrite {
		http.Error(w, "403 Forbidden: 对目标目录没有写权限", http.StatusForbidden)
		return
	}
	base, err := s.res.Resolve(destDir)
	if err != nil {
		http.Error(w, "403 Forbidden: 目标目录非法", http.StatusForbidden)
		return
	}
	if err := os.MkdirAll(base, 0o755); err != nil {
		s.writeErr(w, err)
		return
	}

	files := r.MultipartForm.File["file"]
	if len(files) == 0 {
		http.Error(w, "400 Bad Request: 未收到任何文件", http.StatusBadRequest)
		return
	}

	saved := make([]string, 0, len(files))
	skipped := make([]string, 0)
	for _, fh := range files {
		name := filepath.Base(strings.ReplaceAll(fh.Filename, "\\", "/"))
		if name == "" || name == "." || name == ".." {
			continue
		}
		// 单个名字超长只跳过这一个，不让整批上传失败 —— 多文件表单里
		// 一个坏名字不该拖垮其它合法文件。
		if s.cfg.MaxNameBytes > 0 && len(name) > s.cfg.MaxNameBytes {
			skipped = append(skipped, name)
			continue
		}
		dst := filepath.Join(base, name)
		src, err := fh.Open()
		if err != nil {
			s.writeErr(w, err)
			return
		}
		err = writeAll(dst, src)
		src.Close()
		if err != nil {
			s.writeErr(w, err)
			return
		}
		rel, _ := s.res.Rel(dst)
		saved = append(saved, rel)
	}

	out := map[string]any{"saved": saved, "count": len(saved), "dest": destDir}
	if len(skipped) > 0 {
		out["skipped"] = skipped
		out["message"] = fmt.Sprintf("有 %d 个文件名超过 %d 字节，已跳过",
			len(skipped), s.cfg.MaxNameBytes)
	}
	s.writeJSON(w, out)
}

// writeAll 把 src 全量写入 dst。
func writeAll(dst string, src io.Reader) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = io.Copy(f, src)
	return err
}

// handleDelete 删除文件或目录。目录以 path 参数 style=recursive 递归删除。
func (s *Server) handleDelete(w http.ResponseWriter, r *http.Request, urlPath, abs string, p permResult) {
	if !s.cfg.AllowDelete {
		http.Error(w, "403 Forbidden: 未开启删除（--allow-delete）", http.StatusForbidden)
		return
	}
	if !s.requireWrite(w, p) {
		return
	}
	if isRootPath(urlPath) {
		http.Error(w, "403 Forbidden: 不允许删除服务根目录", http.StatusForbidden)
		return
	}

	info, err := os.Lstat(abs)
	if err != nil {
		s.writeErr(w, err)
		return
	}

	if info.IsDir() {
		// 二次确认：非空目录必须显式声明 recursive，避免误删。
		empty, err := isDirEmpty(abs)
		if err != nil {
			s.writeErr(w, err)
			return
		}
		recursive := r.URL.Query().Has("recursive") ||
			strings.EqualFold(r.Header.Get("X-Gofs-Recursive"), "true")
		if !empty && !recursive {
			http.Error(w, "409 Conflict: 目录非空，删除需追加 ?recursive", http.StatusConflict)
			return
		}
		if err := os.RemoveAll(abs); err != nil {
			s.writeErr(w, err)
			return
		}
	} else if err := os.Remove(abs); err != nil {
		s.writeErr(w, err)
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// handleMkcol 创建目录。
func (s *Server) handleMkcol(w http.ResponseWriter, r *http.Request, urlPath, abs string, p permResult) {
	if !s.cfg.AllowUpload {
		http.Error(w, "403 Forbidden: 未开启创建（--allow-upload）", http.StatusForbidden)
		return
	}
	if !s.requireWrite(w, p) {
		return
	}
	if isRootPath(urlPath) {
		http.Error(w, "409 Conflict: 根目录已存在", http.StatusConflict)
		return
	}
	if _, err := os.Stat(abs); err == nil {
		http.Error(w, "409 Conflict: 目标已存在", http.StatusConflict)
		return
	}
	if err := os.MkdirAll(abs, 0o755); err != nil {
		s.writeErr(w, err)
		return
	}
	w.WriteHeader(http.StatusCreated)
}

// handleMove 移动或重命名。目标取自标准的 Destination 头。
func (s *Server) handleMove(w http.ResponseWriter, r *http.Request, urlPath string, p permResult) {
	if !s.cfg.AllowUpload {
		http.Error(w, "403 Forbidden: 未开启移动（--allow-upload）", http.StatusForbidden)
		return
	}
	if !s.requireWrite(w, p) {
		return
	}
	if isRootPath(urlPath) {
		http.Error(w, "403 Forbidden: 不允许移动服务根目录", http.StatusForbidden)
		return
	}

	destURL := r.Header.Get("Destination")
	if destURL == "" {
		http.Error(w, "400 Bad Request: 缺少 Destination 头", http.StatusBadRequest)
		return
	}
	destPath := destURL
	if u, err := url.Parse(destURL); err == nil && u.Path != "" {
		destPath = u.Path
	}
	// 去掉访问前缀。
	if pre := s.cfg.PathPrefix; pre != "" {
		destPath = strings.TrimPrefix(destPath, pre)
	}
	destPath = fsutil.CleanURLPath(destPath)

	src, err := s.res.Resolve(urlPath)
	if err != nil {
		http.Error(w, "403 Forbidden", http.StatusForbidden)
		return
	}
	dst, err := s.res.Resolve(destPath)
	if err != nil {
		http.Error(w, "403 Forbidden: 目标路径非法", http.StatusForbidden)
		return
	}
	if _, err := os.Stat(src); err != nil {
		s.writeErr(w, err)
		return
	}

	overwrite := strings.EqualFold(r.Header.Get("Overwrite"), "T")
	if _, err := os.Stat(dst); err == nil {
		if !overwrite {
			http.Error(w, "412 Precondition Failed: 目标已存在", http.StatusPreconditionFailed)
			return
		}
		if err := os.RemoveAll(dst); err != nil {
			s.writeErr(w, err)
			return
		}
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		s.writeErr(w, err)
		return
	}
	if err := os.Rename(src, dst); err != nil {
		// 跨设备时退化为复制 + 删除。
		if !errors.Is(err, fs.ErrInvalid) && !strings.Contains(err.Error(), "cross-device") {
			s.writeErr(w, err)
			return
		}
		if err := copyTree(src, dst); err != nil {
			s.writeErr(w, err)
			return
		}
		if err := os.RemoveAll(src); err != nil {
			s.writeErr(w, err)
			return
		}
	}
	w.WriteHeader(http.StatusCreated)
}

// copyTree 递归复制文件或目录。
func copyTree(src, dst string) error {
	info, err := os.Stat(src)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		in, err := os.Open(src)
		if err != nil {
			return err
		}
		defer in.Close()
		return writeAll(dst, in)
	}
	if err := os.MkdirAll(dst, info.Mode().Perm()); err != nil {
		return err
	}
	entries, err := os.ReadDir(src)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if err := copyTree(filepath.Join(src, e.Name()), filepath.Join(dst, e.Name())); err != nil {
			return err
		}
	}
	return nil
}

// isDirEmpty 判断目录是否为空。
func isDirEmpty(dir string) (bool, error) {
	f, err := os.Open(dir)
	if err != nil {
		return false, err
	}
	defer f.Close()
	names, err := f.Readdirnames(1)
	if errors.Is(err, io.EOF) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	return len(names) == 0, nil
}

// isRootPath 判断是否指向服务根。
func isRootPath(urlPath string) bool {
	c := fsutil.CleanURLPath(urlPath)
	return c == "/" || c == ""
}
