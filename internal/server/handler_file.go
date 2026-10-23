package server

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/xxl6097/gofs/internal/auth"
	"github.com/xxl6097/gofs/internal/fsutil"
)

// maxFormMemory 表单上传时内存中保留的上限，超出部分落临时文件。
const maxFormMemory = 32 << 20

// maxFormBody 是 multipart 表单上传的硬上限。
//
// ParseMultipartForm 会把超过 maxFormMemory 的部分写进系统临时目录。
// 表单接口不能像流式 PUT 一样允许 UploadMaxSize（默认 10 GiB）——
// 一个请求就能把 /tmp（常见为 tmpfs）写满，进而影响整台机器。
// 更大的文件请用流式 PUT，它不产生临时文件。
const maxFormBody int64 = 1 << 30

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
	resetRead := withReadProgress(w, r, s.cfg.UploadReadTimeout)
	defer resetRead()
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
			// 密钥场景没有 Basic 凭据，改用密钥自身的范围 + 创建者 ACL 判定。
			if !s.uploadKeyAllowed(r, p.UploadKey, destPath) {
				http.Error(w, "403 Forbidden: 归档目录超出该密钥允许的范围，或创建者已无对应权限",
					http.StatusForbidden)
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

	// 最终落点可能已经被日期归档改写（默认布局或 ?dated=1），也可能经软链/
	// 大小写别名落到另一条规则下。handleRoot 复核的是**改写前**的 URL，
	// 改写之后必须按真实目标再复核一次 —— 否则根内一条 `2026 -> private`
	// 的软链就能把写入重定向到没有写权限的目录；上传密钥同理，会越过 scope。
	if realURL, realPerm, within := s.realPermOf(r, abs); within {
		if realURL != destPath {
			if p.UploadKey != nil {
				if !s.uploadKeyAllowed(r, p.UploadKey, realURL) {
					http.Error(w, fmt.Sprintf("403 Forbidden: 该密钥只允许上传到 %s，或创建者已无对应权限",
						p.UploadKey.Scope), http.StatusForbidden)
					return
				}
			} else if realPerm < auth.PermReadWrite {
				http.Error(w, "403 Forbidden: 对目标的真实路径没有写权限", http.StatusForbidden)
				return
			}
		}
	} else if p.UploadKey != nil || shareTokenOf(r) != "" || !s.cfg.AllowSymlink {
		http.Error(w, "403 Forbidden: 目标路径超出服务范围", http.StatusForbidden)
		return
	}

	// 不接受写入到已存在的软链：悬空软链会写到根外，根内软链也可能把内容
	// 导到另一条 ACL 路径上（与 handlePost 的逐文件检查同一口径）。
	if !s.cfg.AllowSymlink {
		if fi, lerr := os.Lstat(abs); lerr == nil && fi.Mode()&os.ModeSymlink != 0 {
			http.Error(w, "403 Forbidden: 目标文件是符号链接", http.StatusForbidden)
			return
		}
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

	n, err := copyUpload(f, r.Body)
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
		http.Error(w, "500 写入失败，请查看服务端日志", http.StatusInternalServerError)
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
	resetRead := withReadProgress(w, r, s.cfg.UploadReadTimeout)
	defer resetRead()
	formMax := s.settings.UploadMaxSize()
	if formMax <= 0 || formMax > maxFormBody {
		// 未限制或限制过大时，表单路径仍需一个独立上限。
		formMax = maxFormBody
	}
	if !requestBody(w, r, formMax, "表单内容（大文件请使用 PUT）") {
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

	files := r.MultipartForm.File["file"]
	if len(files) == 0 {
		// 必须在任何 MkdirAll 之前拒绝：否则一个不带文件的请求
		// 也能借 path= 参数在服务端创建目录（包括穿过软链建到别处）。
		http.Error(w, "400 Bad Request: 未收到任何文件", http.StatusBadRequest)
		return
	}

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
		// 密钥场景没有 Basic 凭据，改用密钥自身的范围 + 创建者 ACL 判定。
		if !s.uploadKeyAllowed(r, p.UploadKey, destDir) {
			http.Error(w, "403 Forbidden: 目标目录超出该密钥允许的范围，或创建者已无对应权限",
				http.StatusForbidden)
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
	// 目标目录可能是软链/大小写别名：先按真实路径复核，再允许创建。
	// 这一步必须发生在 MkdirAll 之前 —— 否则 `docs/link -> ../private`
	// 会让一次表单上传在 alice 只有读权限的 /private 下建目录。
	if realURL, within := s.realURLOf(base); within {
		if realURL != destDir {
			if p.UploadKey != nil {
				if !s.uploadKeyAllowed(r, p.UploadKey, realURL) {
					http.Error(w, "403 Forbidden: 目标目录超出该密钥允许的范围，或创建者已无对应权限",
						http.StatusForbidden)
					return
				}
			} else if s.checkPerm(r, realURL) != auth.PermReadWrite {
				http.Error(w, "403 Forbidden: 对目标目录的真实路径没有写权限", http.StatusForbidden)
				return
			}
		}
	} else if p.UploadKey != nil || shareTokenOf(r) != "" || !s.cfg.AllowSymlink {
		http.Error(w, "403 Forbidden: 目标目录超出服务范围", http.StatusForbidden)
		return
	}

	// 先做完整校验，再开始写盘：权限/软链检查要覆盖每一个目标文件，
	// 且不能出现「前几个已覆盖、后面才发现越权」的半完成状态。
	type uploadTarget struct {
		fh  *multipart.FileHeader
		dst string
	}
	targets := make([]uploadTarget, 0, len(files))
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
		// 目标路径按**文件本身**复核权限，而不是只看目标目录：
		// ACL 支持最长前缀，`/docs:rw` + `/docs/secret.txt:r` 这种
		// 「目录可写、单文件只读」的规则不能被一次表单上传绕过。
		fileURL := path.Join(destDir, name)
		if p.UploadKey != nil {
			if !s.uploadKeyAllowed(r, p.UploadKey, fileURL) {
				http.Error(w, "403 Forbidden: 上传目标超出该密钥允许的范围，或创建者已无对应权限",
					http.StatusForbidden)
				return
			}
		} else if s.checkPerm(r, fileURL) != auth.PermReadWrite {
			http.Error(w, "403 Forbidden: 对目标文件没有写权限", http.StatusForbidden)
			return
		}
		// 目标目录/叶子可能是软链或大小写别名：按真实路径再复核一次。
		if realURL, within := s.realURLOf(dst); within {
			if p.UploadKey != nil {
				if !s.uploadKeyAllowed(r, p.UploadKey, realURL) {
					http.Error(w, "403 Forbidden: 上传目标超出该密钥允许的范围，或创建者已无对应权限",
						http.StatusForbidden)
					return
				}
			} else if s.checkPerm(r, realURL) != auth.PermReadWrite {
				http.Error(w, "403 Forbidden: 对目标文件的真实路径没有写权限", http.StatusForbidden)
				return
			}
		} else if p.UploadKey != nil || shareTokenOf(r) != "" || !s.cfg.AllowSymlink {
			http.Error(w, "403 Forbidden: 目标路径超出服务范围", http.StatusForbidden)
			return
		}
		if !s.cfg.AllowSymlink {
			// 不接受写入到已存在的软链：悬空软链会写到根外，根内软链
			// 也可能把内容导到另一条 ACL 路径上。
			if fi, err := os.Lstat(dst); err == nil && fi.Mode()&os.ModeSymlink != 0 {
				http.Error(w, "403 Forbidden: 目标文件是符号链接", http.StatusForbidden)
				return
			}
			if err := fsutil.EnsureWithin(base, dst); err != nil {
				http.Error(w, "403 Forbidden: 目标路径超出服务范围", http.StatusForbidden)
				return
			}
		}
		targets = append(targets, uploadTarget{fh: fh, dst: dst})
	}

	// 整批校验全部通过后才创建目标目录，避免“前几个文件越权、目录却已建好”
	// 的半完成状态。目录本身已在上方按真实路径复核过权限。
	if len(targets) > 0 {
		if err := os.MkdirAll(base, 0o755); err != nil {
			s.writeErr(w, err)
			return
		}
	}

	saved := make([]string, 0, len(targets))
	for _, t := range targets {
		src, err := t.fh.Open()
		if err != nil {
			s.writeErr(w, err)
			return
		}
		err = savePart(t.dst, src)
		src.Close()
		if err != nil {
			s.writeErr(w, err)
			return
		}
		rel, _ := s.res.Rel(t.dst)
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

// savePart 把一个表单文件字段落到 dst。
//
// ParseMultipartForm 对于超过 maxFormMemory 的字段**已经**把内容写进临时文件了；
// 这里再整份 copy 一遍等于把大文件读写两遍 —— 实测 1GB 走这条路只有 ~300MB/s，
// 而同一台机器上 PUT 能到 ~1.3GB/s，差的正是这一次多余的搬运。
//
// 所以：如果 src 就是那个临时文件（*os.File），且目标与临时文件在同一个文件系统，
// 直接 rename 过去即可 —— 那是纯元数据操作，不搬数据。跨文件系统（EXDEV，比如
// /tmp 是 tmpfs 而数据盘在别处）时 rename 会失败，退回按字节拷贝。
func savePart(dst string, src io.Reader) error {
	if f, ok := src.(*os.File); ok && f.Name() != "" {
		if err := os.Rename(f.Name(), dst); err == nil {
			// 临时文件是 0600，rename 不改权限；补齐成普通上传文件的 0644。
			// 改不动也不影响内容，忽略即可。
			_ = os.Chmod(dst, 0o644)
			return nil
		}
	}
	return writeAll(dst, src)
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
	_, err = copyUpload(f, src)
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
	if isRootPath(urlPath) || s.res.IsRoot(abs) {
		// ⚠️ 必须判 abs，不能只看 URL：附加站点挂在 /<标签> 下，
		// `DELETE /<标签>?recursive` 解析出来正是那个站点的根目录 ——
		// 只认 urlPath 是不是 "/" 会把它整个删掉。
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
		// 递归删除会对每个后代生效：逐个复核，不能只看目录本身。
		if bad, ok := s.requireDeleteTree(r, abs); !ok {
			http.Error(w, "403 Forbidden: 目录内的 "+bad+" 没有写权限", http.StatusForbidden)
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
	// 去掉访问前缀。要求**整段**匹配：`/filesX/y` 不该被前缀 `/files`
	// 剥成 `X/y` —— 那会把用户没写的路径当成目标。
	if pre := s.PathPrefix(); pre != "" {
		switch {
		case destPath == pre:
			destPath = "/"
		case strings.HasPrefix(destPath, pre+"/"):
			destPath = strings.TrimPrefix(destPath, pre)
		}
	}
	// 与源路径一样，按磁盘真实名字对齐（大小写/规范化/短名）。
	canon, ok := s.canonicalPath(r, destPath)
	if !ok {
		http.Error(w, "403 Forbidden: 目标路径超出服务范围或无法安全规范化", http.StatusForbidden)
		return
	}
	destPath = canon

	// 目标名的长度也要挡：不挡的话，一个超长名会被文件系统拒成 ENAMETOOLONG，
	// 透出去就是 500 —— 看起来像服务端故障，实际是请求本身不合法，而上传同一个
	// 名字（handlePut）一直是拒绝的。两边口径必须一致。
	if !s.checkNameLength(w, destPath) {
		return
	}

	src, err := s.res.Resolve(urlPath)
	if err != nil {
		http.Error(w, "403 Forbidden", http.StatusForbidden)
		return
	}
	// 同 handleDelete：移动/重命名也不能拿附加站点的根开刀。
	// 在这里判是因为 src 刚解析出来 —— 上面的 isRootPath 只看得到 URL，
	// 而 `MOVE /<标签>` 的 src 正是那个站点的根目录。
	if s.res.IsRoot(src) {
		http.Error(w, "403 Forbidden: 不允许移动服务根目录", http.StatusForbidden)
		return
	}
	dst, err := s.res.Resolve(destPath)
	if err != nil {
		// 越界（含 .. 穿越、软链逃逸）单独说清楚 ——
		// 和「路径写错了」不是一回事，用户需要知道是被范围挡住了。
		if errors.Is(err, fsutil.ErrEscaped) {
			http.Error(w, "403 Forbidden: 目标路径超出服务范围", http.StatusForbidden)
			return
		}
		http.Error(w, "403 Forbidden: 目标路径非法", http.StatusForbidden)
		return
	}
	if _, err := os.Stat(src); err != nil {
		s.writeErr(w, err)
		return
	}

	// 目标也要过两道关 —— 只挡源是远远不够的：
	//
	//   - **目标不能是服务根**（任何站点的根）。下面 `Overwrite: T` 那一支会
	//     `os.RemoveAll(dst)`，dst 是根就等于把整个被服务的目录删光；
	//   - **目标要有写权限**。不判的话，一个只在 /docs 有写权限的账号可以把
	//     文件 MOVE 到任何别的地方去，等于绕开了按路径的权限模型。
	if s.res.IsRoot(dst) {
		http.Error(w, "403 Forbidden: 不允许把目标设为服务根目录", http.StatusForbidden)
		return
	}
	if s.checkPerm(r, destPath) != auth.PermReadWrite {
		http.Error(w, "403 Forbidden: 对目标路径没有写权限", http.StatusForbidden)
		return
	}
	// 目标路径本身也可能是软链/大小写别名，真实目标必须同样可写。
	if realURL, within := s.realURLOf(dst); within {
		if s.checkPerm(r, realURL) != auth.PermReadWrite {
			http.Error(w, "403 Forbidden: 对目标真实路径没有写权限", http.StatusForbidden)
			return
		}
	} else if shareTokenOf(r) != "" || !s.cfg.AllowSymlink {
		http.Error(w, "403 Forbidden: 目标路径超出服务范围", http.StatusForbidden)
		return
	}

	// 目标落在源里面（含「就是它自己」）必须挡掉。
	//
	// 不挡的话，os.Rename 会返回 EINVAL，用户看到的是一句裸内核错误
	// （"rename ...: invalid argument"）；而那个跨设备兜底分支一旦被触发，
	// 就是「复制到自己的子目录里、再把源删掉」—— 数据直接没了。
	// 这里用路径关系判断，比事后分辨 errno 可靠得多。
	if insideOrSame(src, dst) {
		http.Error(w, "400 Bad Request: 不能把目录移动到它自己里面", http.StatusBadRequest)
		return
	}

	// 目录移动会改变所有后代的 URL：逐个比较新旧权限，禁止「搬到更宽
	// 的规则下」把只读子项变成可写。
	if srcInfo, lerr := os.Lstat(src); lerr == nil && srcInfo.IsDir() {
		if bad, ok := s.requireMoveTree(r, src, urlPath, destPath); !ok {
			http.Error(w, "403 Forbidden: "+bad, http.StatusForbidden)
			return
		}
	}

	overwrite := strings.EqualFold(r.Header.Get("Overwrite"), "T")
	if st, lerr := os.Lstat(dst); lerr == nil {
		if !overwrite {
			http.Error(w, "412 Precondition Failed: 目标已存在", http.StatusPreconditionFailed)
			return
		}
		// 覆盖目录 = 递归删除目标里的全部内容，同样要逐项复核。
		if st.IsDir() {
			if bad, ok := s.requireDeleteTree(r, dst); !ok {
				http.Error(w, "403 Forbidden: 目标目录内的 "+bad+" 没有写权限", http.StatusForbidden)
				return
			}
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
		if err := copyTree(src, dst, s.cfg.AllowSymlink); err != nil {
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
//
// followSymlinks 为 false（默认的 !--allow-symlink）时跳过软链，不跟随。
// 否则跨设备 MOVE 的兜底分支会把根外文件内容复制进来，或者沿着
// 「软链 → 上级目录」无限递归。
func copyTree(src, dst string, followSymlinks bool) error {
	info, err := os.Lstat(src)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		if !followSymlinks {
			return nil
		}
		// 显式允许软链的部署保持旧语义：复制软链指向的内容。
		info, err = os.Stat(src)
		if err != nil {
			return err
		}
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
		if err := copyTree(filepath.Join(src, e.Name()), filepath.Join(dst, e.Name()), followSymlinks); err != nil {
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

// insideOrSame 判断 dst 是否就是 src，或落在 src 的子树里。
//
// 两个参数都已经是**绝对的真实路径**（由 Resolver.Resolve 给出，
// 已经做过 Clean、越界检查与软链解析），所以这里可以直接比路径关系。
//
// 为什么不用 os.Rename 的返回值判断：EINVAL 在不同平台/文件系统上
// 语义并不一致，靠错误码反推「是不是移进了自己」太脆。
func insideOrSame(src, dst string) bool {
	rel, err := filepath.Rel(src, dst)
	if err != nil {
		// 无法比较（例如跨卷、不同前缀）时保守放行：
		// 真正的越界已由 Resolve 挡在更前面。
		return false
	}
	// rel == "." 表示同一个路径；
	// 不以 ".." 开头表示 dst 在 src 之下。
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}
