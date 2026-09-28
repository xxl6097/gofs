package server

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/uuxia/gofs/internal/auth"
	"github.com/uuxia/gofs/internal/fsutil"
	"github.com/uuxia/gofs/internal/textfile"
)

// textDoc 是读取文本文件接口的响应。
type textDoc struct {
	Path     string `json:"path"`
	Name     string `json:"name"`
	Size     int64  `json:"size"`
	Mtime    string `json:"mtime"`
	MtimeMs  int64  `json:"mtime_ms"`
	Language string `json:"language"`
	Newline  string `json:"newline"`
	BOM      string `json:"bom,omitempty"`
	Content  string `json:"content"`
	// Hash 是内容的短摘要，用作保存时的乐观锁凭据。
	// 比 mtime 可靠：同一毫秒内的两次写入时间戳可能相同，内容摘要则不会。
	Hash     string `json:"hash"`
	Editable bool   `json:"editable"`
	ReadOnly bool   `json:"read_only"`
	MaxSize  int64  `json:"max_size"`
	MaxSave  int64  `json:"max_save"`
}

// textSaveRequest 是保存文本文件接口的请求体。
type textSaveRequest struct {
	Path string `json:"path"`
	// Content 为编辑器里的完整内容。
	Content string `json:"content"`
	// BaseHash 是打开文件时拿到的内容摘要，用于乐观锁。
	// 为空表示不做冲突检测。
	BaseHash string `json:"base_hash"`
	// BaseMtimeMs 是退化方案：BaseHash 为空时用修改时间比对。
	BaseMtimeMs int64 `json:"base_mtime_ms"`
	// Force 为 true 时忽略冲突直接覆盖。
	Force bool `json:"force"`
	// Newline 指定写回的换行风格；为空则沿用原文件的风格。
	Newline string `json:"newline"`
}

// contentHash 计算内容的短摘要，用于编辑器的乐观锁。
func contentHash(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])[:16]
}

// handleText 处理文本文件的读取与保存。
func (s *Server) handleText(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet, http.MethodHead:
		s.textRead(w, r)
	case http.MethodPut, http.MethodPost:
		s.textSave(w, r)
	default:
		w.Header().Set("Allow", "GET, HEAD, PUT, POST")
		http.Error(w, "405 Method Not Allowed", http.StatusMethodNotAllowed)
	}
}

// resolveTextTarget 解析并校验文本文件路径。
func (s *Server) resolveTextTarget(w http.ResponseWriter, urlPath string) (abs, clean string, ok bool) {
	if fsutil.HasParentSegment(urlPath) {
		http.Error(w, "400 Bad Request: 路径中不允许出现 ..", http.StatusBadRequest)
		return "", "", false
	}
	clean = fsutil.CleanURLPath(urlPath)
	if clean == "/" || strings.HasSuffix(clean, "/") {
		http.Error(w, "400 Bad Request: 需要指定具体文件路径", http.StatusBadRequest)
		return "", "", false
	}
	if s.res.SingleFile() != "" {
		http.Error(w, "403 Forbidden: 单文件模式下不支持在线编辑", http.StatusForbidden)
		return "", "", false
	}
	abs, err := s.res.Resolve(clean)
	if err != nil {
		http.Error(w, "403 Forbidden", http.StatusForbidden)
		return "", "", false
	}
	return abs, clean, true
}

// textRead 读取文本内容供编辑器显示。
func (s *Server) textRead(w http.ResponseWriter, r *http.Request) {
	if !s.cfg.AllowEdit {
		http.Error(w, "403 Forbidden: 未开启在线编辑（--allow-edit）", http.StatusForbidden)
		return
	}
	abs, clean, ok := s.resolveTextTarget(w, r.URL.Query().Get("path"))
	if !ok {
		return
	}

	p, ok := s.authorize(w, r, clean)
	if !ok {
		return
	}

	st, err := os.Stat(abs)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			http.Error(w, "404 Not Found: 文件不存在", http.StatusNotFound)
			return
		}
		s.writeErr(w, err)
		return
	}
	if st.IsDir() {
		http.Error(w, "400 Bad Request: 目标是目录，请指定文件", http.StatusBadRequest)
		return
	}
	if st.Size() > s.cfg.EditMaxSize {
		http.Error(w, fmt.Sprintf("413 Payload Too Large: 文件 %s 超过在线编辑上限 %s，请下载后编辑",
			formatBytes(st.Size()), formatBytes(s.cfg.EditMaxSize)), http.StatusRequestEntityTooLarge)
		return
	}

	raw, err := os.ReadFile(abs)
	if err != nil {
		s.writeErr(w, err)
		return
	}
	if !textfile.Sniff(raw) {
		http.Error(w, "415 Unsupported Media Type: 该文件不是纯文本，无法在线编辑",
			http.StatusUnsupportedMediaType)
		return
	}

	bom, body := textfile.TrimBOM(raw)
	if kind := textfile.BOMKind(bom); strings.HasPrefix(kind, "utf-16") {
		http.Error(w, "415 Unsupported Media Type: 暂不支持编辑 "+kind+" 编码的文件",
			http.StatusUnsupportedMediaType)
		return
	}

	s.writeJSON(w, textDoc{
		Path:     clean,
		Name:     path.Base(clean),
		Size:     st.Size(),
		Mtime:    st.ModTime().Format(time.RFC3339),
		MtimeMs:  st.ModTime().UnixMilli(),
		Language: textfile.Language(clean),
		Newline:  textfile.Newline(body),
		BOM:      textfile.BOMKind(bom),
		Content:  string(body),
		Hash:     contentHash(raw),
		Editable: true,
		ReadOnly: p.Perm != auth.PermReadWrite,
		MaxSize:  s.cfg.EditMaxSize,
		MaxSave:  textfile.MaxSaveSize,
	})
}

// textSave 保存编辑器提交的内容。
func (s *Server) textSave(w http.ResponseWriter, r *http.Request) {
	if !s.cfg.AllowEdit {
		http.Error(w, "403 Forbidden: 未开启在线编辑（--allow-edit）", http.StatusForbidden)
		return
	}

	var req textSaveRequest
	dec := json.NewDecoder(io.LimitReader(r.Body, textfile.MaxSaveSize+(1<<20)))
	if err := dec.Decode(&req); err != nil {
		if errors.Is(err, io.EOF) {
			http.Error(w, "400 Bad Request: 请求体为空", http.StatusBadRequest)
			return
		}
		http.Error(w, "400 Bad Request: 请求体不是合法 JSON（内容过大时也会如此）", http.StatusBadRequest)
		return
	}
	if req.Path == "" {
		http.Error(w, "400 Bad Request: 缺少 path", http.StatusBadRequest)
		return
	}
	if int64(len(req.Content)) > textfile.MaxSaveSize {
		http.Error(w, fmt.Sprintf("413 Payload Too Large: 内容超过 %s 上限",
			formatBytes(textfile.MaxSaveSize)), http.StatusRequestEntityTooLarge)
		return
	}
	// NUL 字节说明内容已不是纯文本，写进去只会损坏文件。
	if strings.IndexByte(req.Content, 0) >= 0 {
		http.Error(w, "400 Bad Request: 内容包含 NUL 字节，无法作为文本保存", http.StatusBadRequest)
		return
	}

	abs, clean, ok := s.resolveTextTarget(w, req.Path)
	if !ok {
		return
	}
	p, ok := s.authorize(w, r, clean)
	if !ok {
		return
	}
	if p.Perm != auth.PermReadWrite {
		http.Error(w, "403 Forbidden: 保存需要读写权限", http.StatusForbidden)
		return
	}

	// 读取原文件状态：用于冲突检测、权限位继承、换行与 BOM 保留。
	var (
		baseInfo fs.FileInfo
		existing []byte
	)
	if st, err := os.Stat(abs); err == nil {
		if st.IsDir() {
			http.Error(w, "409 Conflict: 目标是目录", http.StatusConflict)
			return
		}
		baseInfo = st
		// 只在体积可控时读原内容，避免为了判定换行把大文件整个读进内存。
		if st.Size() <= s.cfg.EditMaxSize {
			existing, _ = os.ReadFile(abs)
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		s.writeErr(w, err)
		return
	}

	// 乐观锁：文件在打开之后被别人改过就拒绝，避免静默覆盖。
	// 优先比对内容摘要（同一毫秒内的两次写入时间戳可能相同，摘要不会），
	// 摘要不可用时退回到修改时间。
	if baseInfo != nil && !req.Force {
		conflict := false
		switch {
		case req.BaseHash != "" && existing != nil:
			conflict = contentHash(existing) != req.BaseHash
		case req.BaseMtimeMs > 0:
			conflict = baseInfo.ModTime().UnixMilli() != req.BaseMtimeMs
		}
		if conflict {
			resp := map[string]any{
				"message":       "文件在编辑期间已被修改",
				"current_mtime": baseInfo.ModTime().Format(time.RFC3339),
				"current_size":  baseInfo.Size(),
			}
			if existing != nil {
				resp["current_hash"] = contentHash(existing)
			}
			s.writeJSONStatus(w, http.StatusConflict, resp)
			return
		}
	}

	// 换行风格：优先用请求指定的，其次沿用原文件，最后默认 LF。
	newline := req.Newline
	if newline != textfile.NewlineLF && newline != textfile.NewlineCRLF && newline != textfile.NewlineCR {
		newline = ""
	}
	if newline == "" {
		if existing != nil {
			_, body := textfile.TrimBOM(existing)
			newline = textfile.Newline(body)
		} else {
			newline = textfile.NewlineLF
		}
	}

	out := []byte(textfile.ApplyNewline(req.Content, newline))

	// 保留 UTF-8 BOM，避免保存后文件的编码标记被悄悄改掉。
	if bom, _ := textfile.TrimBOM(existing); len(bom) > 0 && textfile.BOMKind(bom) == "utf-8" {
		out = append(append(make([]byte, 0, len(bom)+len(out)), bom...), out...)
	}

	mode := fs.FileMode(0o644)
	created := true
	if baseInfo != nil {
		mode = baseInfo.Mode().Perm()
		created = false
	}

	if err := atomicWriteFile(abs, out, mode); err != nil {
		s.writeErr(w, err)
		return
	}

	st, err := os.Stat(abs)
	if err != nil {
		s.writeErr(w, err)
		return
	}
	action := "覆盖"
	if created {
		action = "新建"
	}
	s.logger.Infof("保存文本 %s：%d 字节（%s、%s 换行）", clean, len(out), action, newlineName(newline))

	s.writeJSON(w, map[string]any{
		"path":     clean,
		"size":     int64(len(out)),
		"mtime":    st.ModTime().Format(time.RFC3339),
		"mtime_ms": st.ModTime().UnixMilli(),
		"hash":     contentHash(out),
		"created":  created,
		"newline":  newline,
		"saved_at": time.Now().Format(time.RFC3339),
	})
}

// newlineName 把换行风格转成可读名称。
func newlineName(nl string) string {
	switch nl {
	case textfile.NewlineCRLF:
		return "CRLF"
	case textfile.NewlineCR:
		return "CR"
	default:
		return "LF"
	}
}

// atomicWriteFile 先写同目录临时文件再 rename，保证不会出现写到一半的残缺文件。
//
// 注意：rename 会替换 inode，因此目标文件若存在硬链接，链接会被断开；
// 这是为保证原子性所付出的代价，属于可接受取舍。
func atomicWriteFile(target string, data []byte, mode fs.FileMode) error {
	dir := filepath.Dir(target)
	// 临时文件以 . 开头，短暂出现在目录列表中不显眼。
	tmp, err := os.CreateTemp(dir, ".gofs-write-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	abort := func() {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
	}

	if _, err := tmp.Write(data); err != nil {
		abort()
		return err
	}
	if err := tmp.Chmod(mode); err != nil {
		abort()
		return err
	}
	if err := tmp.Sync(); err != nil {
		abort()
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	if err := os.Rename(tmpName, target); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	return nil
}

// formatBytes 把字节数格式化为可读字符串。
func formatBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for n/div >= unit && exp < 4 {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTP"[exp])
}
