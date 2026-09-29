package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path"
	"strings"
	"time"

	"github.com/xxl6097/gofs/internal/archive"
	"github.com/xxl6097/gofs/internal/auth"
	"github.com/xxl6097/gofs/internal/fsutil"
)

// extractRequest 为解压请求体。
type extractRequest struct {
	// Path 为压缩包相对服务根目录的路径。
	Path string `json:"path"`
	// Dest 为解压目标目录，留空则使用压缩包同级的同名目录。
	Dest string `json:"dest"`
	// Overwrite 为 true 时覆盖已存在的文件。
	Overwrite bool `json:"overwrite"`
}

// sseWriter 负责按 Server-Sent Events 协议推送消息。
type sseWriter struct {
	w       http.ResponseWriter
	flusher http.Flusher
}

// newSSEWriter 初始化 SSE 响应头。
func newSSEWriter(w http.ResponseWriter) (*sseWriter, error) {
	f, ok := w.(http.Flusher)
	if !ok {
		return nil, errors.New("当前服务器不支持流式响应")
	}
	h := w.Header()
	h.Set("Content-Type", "text/event-stream; charset=utf-8")
	h.Set("Cache-Control", "no-cache, no-transform")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	f.Flush()
	return &sseWriter{w: w, flusher: f}, nil
}

// event 推送一条 SSE 消息。
func (s *sseWriter) event(name string, payload any) {
	raw, err := json.Marshal(payload)
	if err != nil {
		return
	}
	fmt.Fprintf(s.w, "event: %s\ndata: %s\n\n", name, raw)
	s.flusher.Flush()
}

// handleExtract 统一处理压缩包的内容预览、单文件下载与在线解压。
func (s *Server) handleExtract(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet, http.MethodHead:
		s.extractList(w, r)
	case http.MethodPut, http.MethodPost:
		s.extractRun(w, r)
	default:
		w.Header().Set("Allow", "GET, HEAD, PUT, POST")
		http.Error(w, "405 Method Not Allowed", http.StatusMethodNotAllowed)
	}
}

// resolveArchive 解析并校验压缩包路径，返回 Reader 供调用方使用。
func (s *Server) resolveArchive(w http.ResponseWriter, urlPath string) (archive.Reader, string, bool) {
	if !s.cfg.AllowExtract {
		http.Error(w, "403 Forbidden: 未开启在线解压（--allow-extract）", http.StatusForbidden)
		return nil, "", false
	}
	if fsutil.HasParentSegment(urlPath) {
		http.Error(w, "400 Bad Request: 路径中不允许出现 ..", http.StatusBadRequest)
		return nil, "", false
	}
	clean := fsutil.CleanURLPath(urlPath)
	if clean == "/" {
		http.Error(w, "400 Bad Request: 缺少 path 参数", http.StatusBadRequest)
		return nil, "", false
	}
	abs, err := s.res.Resolve(clean)
	if err != nil {
		http.Error(w, "403 Forbidden", http.StatusForbidden)
		return nil, "", false
	}
	st, err := os.Stat(abs)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			http.Error(w, "404 Not Found: 压缩包不存在", http.StatusNotFound)
			return nil, "", false
		}
		s.writeErr(w, err)
		return nil, "", false
	}
	if st.IsDir() {
		http.Error(w, "400 Bad Request: 目标不是文件", http.StatusBadRequest)
		return nil, "", false
	}

	rd, err := archive.Open(abs)
	if err != nil {
		if errors.Is(err, archive.ErrUnsupported) {
			http.Error(w, "415 Unsupported Media Type: 仅支持 zip / tar / tar.gz / tgz / gz",
				http.StatusUnsupportedMediaType)
			return nil, "", false
		}
		s.writeErr(w, err)
		return nil, "", false
	}
	return rd, clean, true
}

// extractList 列出压缩包内容，或流式输出包内单个文件。
func (s *Server) extractList(w http.ResponseWriter, r *http.Request) {
	urlPath := r.URL.Query().Get("path")
	rd, clean, ok := s.resolveArchive(w, urlPath)
	if !ok {
		return
	}
	defer rd.Close()

	// 指定了 file 参数则直接下载包内文件，不解压落盘。
	if inner := r.URL.Query().Get("file"); inner != "" {
		rc, entry, err := rd.OpenEntry(r.Context(), inner)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				http.Error(w, "404 Not Found: 压缩包内无此条目", http.StatusNotFound)
				return
			}
			if errors.Is(err, archive.ErrEncrypted) {
				http.Error(w, "403 Forbidden: "+err.Error(), http.StatusForbidden)
				return
			}
			s.writeErr(w, err)
			return
		}
		defer rc.Close()

		name := path.Base(entry.Name)
		h := w.Header()
		h.Set("Content-Type", "application/octet-stream")
		h.Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"; filename*=UTF-8''%s`,
			asciiFallback(name), urlEncode(name)))
		h.Set("X-Gofs-Archive-Entry", entry.Name)
		if entry.Size >= 0 {
			h.Set("Content-Length", fmt.Sprintf("%d", entry.Size))
		}
		if _, err := io.Copy(w, rc); err != nil {
			s.logger.Errorf("输出包内文件 %s 失败: %v", entry.Name, err)
		}
		return
	}

	entries, err := rd.List(r.Context())
	if err != nil {
		s.writeErr(w, err)
		return
	}
	s.writeJSON(w, map[string]any{
		"path":         clean,
		"format":       rd.Format().String(),
		"default_dest": s.defaultDest(clean),
		"entries":      entries,
		"total":        len(entries),
		"limits": map[string]any{
			"max_total_bytes": s.cfg.ExtractMaxTotal,
			"max_files":       s.cfg.ExtractMaxFiles,
			"max_ratio":       s.cfg.ExtractMaxRatio,
		},
	})
}

// extractRun 执行在线解压，并通过 SSE 持续推送进度。
func (s *Server) extractRun(w http.ResponseWriter, r *http.Request) {
	// 先做一次权限与配置校验，避免建立起 SSE 之后才报错。
	urlPath := r.URL.Query().Get("path")

	// 支持两种入参：JSON 请求体（推荐），或查询参数（便于 curl 一行搞定）。
	req := extractRequest{}
	if ct := r.Header.Get("Content-Type"); strings.HasPrefix(ct, "application/json") {
		dec := json.NewDecoder(io.LimitReader(r.Body, 1<<20))
		if err := dec.Decode(&req); err != nil && !errors.Is(err, io.EOF) {
			http.Error(w, "400 Bad Request: 请求体不是合法 JSON", http.StatusBadRequest)
			return
		}
	}
	if req.Path == "" {
		req.Path = urlPath
	}
	if req.Dest == "" {
		req.Dest = r.URL.Query().Get("dest")
	}
	if !req.Overwrite {
		req.Overwrite = r.URL.Query().Has("overwrite")
	}

	if req.Path == "" {
		http.Error(w, "400 Bad Request: 缺少 path 参数", http.StatusBadRequest)
		return
	}
	// CleanURLPath 会把 .. 静默消解，写盘前必须显式拒绝，避免歧义。
	if fsutil.HasParentSegment(req.Path) || fsutil.HasParentSegment(req.Dest) {
		http.Error(w, "400 Bad Request: 路径中不允许出现 ..", http.StatusBadRequest)
		return
	}

	if !s.cfg.AllowExtract {
		http.Error(w, "403 Forbidden: 未开启在线解压（--allow-extract）", http.StatusForbidden)
		return
	}

	srcClean := fsutil.CleanURLPath(req.Path)
	// 解压是写操作（会在磁盘上创建文件），要求读写权限。
	p, ok := s.authorize(w, r, srcClean)
	if !ok {
		return
	}
	if p.Perm != auth.PermReadWrite {
		http.Error(w, "403 Forbidden: 解压需要读写权限", http.StatusForbidden)
		return
	}

	srcAbs, err := s.res.Resolve(srcClean)
	if err != nil {
		http.Error(w, "403 Forbidden", http.StatusForbidden)
		return
	}
	st, err := os.Stat(srcAbs)
	if err != nil || st.IsDir() {
		http.Error(w, "404 Not Found: 压缩包不存在", http.StatusNotFound)
		return
	}

	// 确定目标目录。
	destClean := req.Dest
	if destClean == "" {
		destClean = s.defaultDest(srcClean)
	}
	// 解压目标同样要落在有写权限的路径上。
	if dp, ok := s.authorize(w, r, destClean); !ok || dp.Perm != auth.PermReadWrite {
		http.Error(w, "403 Forbidden: 对目标目录没有写权限", http.StatusForbidden)
		return
	}
	destClean = fsutil.CleanURLPath(destClean)
	destAbs, err := s.res.Resolve(destClean)
	if err != nil {
		http.Error(w, "403 Forbidden: 目标目录非法", http.StatusForbidden)
		return
	}
	if destAbs == srcAbs {
		http.Error(w, "400 Bad Request: 目标目录不能是压缩包自身", http.StatusBadRequest)
		return
	}

	// 目标目录已存在且非空时，要求显式覆盖，避免把文件混进已有目录。
	if info, err := os.Stat(destAbs); err == nil && info.IsDir() && !req.Overwrite {
		if empty, eerr := isDirEmpty(destAbs); eerr == nil && !empty {
			http.Error(w, "409 Conflict: 目标目录已存在且非空，如需覆盖请设置 overwrite=true",
				http.StatusConflict)
			return
		}
	}

	rd, err := archive.Open(srcAbs)
	if err != nil {
		if errors.Is(err, archive.ErrUnsupported) {
			http.Error(w, "415 Unsupported Media Type: 仅支持 zip / tar / tar.gz / tgz / gz",
				http.StatusUnsupportedMediaType)
			return
		}
		s.writeErr(w, err)
		return
	}
	defer rd.Close()

	// 解压是最重的操作：长时间占 CPU、磁盘 IO 和一条连接。
	// 取不到名额时直接拒绝，让客户端稍后重试，而不是排队把服务拖垮。
	release, ok := s.acquireJob(r.Context())
	if !ok {
		w.Header().Set("Retry-After", "3")
		http.Error(w, "503 Service Unavailable: 解压任务过多，请稍后重试", http.StatusServiceUnavailable)
		return
	}
	defer release()

	entries, err := rd.List(r.Context())
	if err != nil {
		s.writeErr(w, err)
		return
	}

	sse, err := newSSEWriter(w)
	if err != nil {
		s.writeErr(w, err)
		return
	}

	relSrc, _ := s.res.Rel(srcAbs)
	relDest, _ := s.res.Rel(destAbs)
	sse.event("start", map[string]any{
		"src":     relSrc,
		"dest":    relDest,
		"format":  rd.Format().String(),
		"total":   len(entries),
		"started": time.Now().Format(time.RFC3339),
	})

	// 节流：进度最多每 80ms 推一次，减少无意义的前端刷新。
	var lastPush time.Time
	var last, lastSent archive.Progress
	pushed := false
	onProgress := func(pr archive.Progress) {
		last = pr
		now := time.Now()
		if pushed && now.Sub(lastPush) < 80*time.Millisecond {
			return
		}
		lastPush = now
		lastSent = pr
		pushed = true
		sse.event("progress", pr)
	}

	opts := archive.Options{
		Dest:          destAbs,
		Overwrite:     req.Overwrite,
		MaxTotalBytes: s.cfg.ExtractMaxTotal,
		MaxFiles:      s.cfg.ExtractMaxFiles,
		MaxRatio:      s.cfg.ExtractMaxRatio,
	}

	rep, exErr := rd.Extract(r.Context(), opts, onProgress)

	// 补发一次最终进度（若与已推送的不同），保证前端进度条落到准确值。
	if pushed && (lastSent.Files != last.Files || lastSent.Bytes != last.Bytes ||
		lastSent.Current != last.Current || lastSent.Skipped != last.Skipped) {
		sse.event("progress", last)
	}

	if exErr != nil {
		// 客户端主动断开时不算失败。
		if errors.Is(exErr, r.Context().Err()) && r.Context().Err() != nil {
			return
		}
		s.logger.Errorf("解压 %s 失败: %v", relSrc, exErr)
		sse.event("error", map[string]any{
			"message": exErr.Error(),
			"partial": rep,
		})
		return
	}

	s.logger.Infof("解压完成 %s -> %s：%d 个文件 / %d 个目录 / %d 字节（跳过 %d，耗时 %dms）",
		relSrc, relDest, rep.Files, rep.Dirs, rep.Bytes, rep.Skipped, rep.ElapsedMs)
	sse.event("done", map[string]any{
		"report": rep,
		"url":    s.cfg.PathPrefix + quotePath(relDest) + "/",
	})
}

// defaultDest 依据压缩包路径推导默认解压目录。
func (s *Server) defaultDest(urlPath string) string {
	base := path.Base(urlPath)
	lower := strings.ToLower(base)
	for _, suf := range []string{".tar.gz", ".tgz", ".tar", ".zip", ".gz"} {
		if strings.HasSuffix(lower, suf) {
			base = base[:len(base)-len(suf)]
			break
		}
	}
	if base == "" {
		base = "extracted"
	}
	return path.Join(path.Dir(urlPath), base)
}
