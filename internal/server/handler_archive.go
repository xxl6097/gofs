package server

import (
	"archive/zip"
	"compress/flate"
	"encoding/json"
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

	"github.com/uuxia/gofs/internal/auth"
	"github.com/uuxia/gofs/internal/fsutil"
)

// maxPicks 限制单次打包可选中的条目数，避免超长请求拖垮服务。
const maxPicks = 5000

// zipItem 描述待写入压缩包的一个条目。
type zipItem struct {
	abs  string // 磁盘绝对路径
	rel  string // 包内相对路径，始终以 / 分隔
	info fs.FileInfo
}

// flateLevel 把配置的压缩级别映射为 flate 级别。
func (s *Server) flateLevel() int {
	switch strings.ToLower(s.cfg.Compress) {
	case "low":
		return flate.BestSpeed
	case "high":
		return flate.BestCompression
	case "medium":
		return flate.DefaultCompression
	default:
		return flate.BestSpeed
	}
}

// useStore 表示禁用压缩，直接存储。
func (s *Server) useStore() bool { return strings.EqualFold(s.cfg.Compress, "none") }

// handleZipRequest 处理 POST ?zip：打包请求体里列出的选中条目。
//
// 用 POST 而不是把路径都堆到查询参数上，是为了让「选中一大批文件再打包」
// 不受 URL 长度限制（浏览器与常见代理在 8KiB 左右就会拒绝）。
func (s *Server) handleZipRequest(w http.ResponseWriter, r *http.Request, urlPath, abs string) {
	if !s.cfg.AllowArchive {
		http.Error(w, "403 Forbidden: 未开启目录打包（--allow-archive）", http.StatusForbidden)
		return
	}
	info, err := os.Stat(abs)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			http.Error(w, "404 Not Found", http.StatusNotFound)
			return
		}
		s.writeErr(w, err)
		return
	}
	if !info.IsDir() {
		http.Error(w, "400 Bad Request: 打包目标必须是目录", http.StatusBadRequest)
		return
	}
	picks, err := picksFromBody(r)
	if err != nil {
		http.Error(w, "400 Bad Request: "+err.Error(), http.StatusBadRequest)
		return
	}
	s.serveZip(w, r, abs, urlPath, picks)
}

// picksFromQuery 解析 GET 请求里的 ?pick= 参数。
//
// 只支持重复出现的参数（?pick=a&pick=b），不做逗号切分：
// 文件名里本来就允许有逗号，切分会造成歧义。
func picksFromQuery(q url.Values) []string {
	var out []string
	for _, v := range q["pick"] {
		v = strings.TrimSpace(v)
		if v != "" {
			out = append(out, v)
		}
	}
	if len(out) > maxPicks {
		out = out[:maxPicks]
	}
	return out
}

// picksFromBody 解析 POST 请求体（{"picks":[...]}）里的选中项。
func picksFromBody(r *http.Request) ([]string, error) {
	var req struct {
		Picks []string `json:"picks"`
	}
	dec := json.NewDecoder(io.LimitReader(r.Body, 4<<20))
	if err := dec.Decode(&req); err != nil {
		return nil, fmt.Errorf("请求体不是合法 JSON: %w", err)
	}
	if len(req.Picks) > maxPicks {
		req.Picks = req.Picks[:maxPicks]
	}
	return req.Picks, nil
}

// serveZip 把目录流式打包为 zip 下载。
//
// picks 为空表示打包整个目录（默认行为）；否则只打包选中的条目，
// 若选中的是目录，其内容仍会递归包含。
//
// 采用「先扫描再写出」两阶段：扫描阶段可提前发现错误并给出正确的状态码，
// 写出阶段边压缩边发送，避免大目录占用额外的磁盘与内存。
func (s *Server) serveZip(w http.ResponseWriter, r *http.Request, dir, urlPath string, picks []string) {
	picked := len(picks) > 0

	var (
		items   []zipItem
		skipped []string
		err     error
	)
	if picked {
		items, skipped = s.resolvePicks(r, urlPath, picks)
		if len(items) == 0 {
			http.Error(w, "400 Bad Request: 选中的条目都无法打包（不存在、越界或没有读取权限）",
				http.StatusBadRequest)
			return
		}
	} else {
		items, err = collectTree(dir)
		if err != nil {
			s.writeErr(w, err)
			return
		}
	}

	var totalBytes int64
	for _, it := range items {
		if !it.info.IsDir() {
			totalBytes += it.info.Size()
		}
	}

	name := zipDownloadName(dir, items, picked)

	// ---- 阶段二：流式写出 ----
	h := w.Header()
	h.Set("Content-Type", "application/zip")
	h.Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"; filename*=UTF-8''%s`,
		asciiFallback(name), urlEncode(name)))
	h.Set("X-Gofs-Archive-Entries", fmt.Sprintf("%d", len(items)))
	h.Set("X-Gofs-Archive-Bytes", fmt.Sprintf("%d", totalBytes))
	if picked {
		h.Set("X-Gofs-Archive-Mode", "picked")
	} else {
		h.Set("X-Gofs-Archive-Mode", "all")
	}
	if len(skipped) > 0 {
		// 部分选中项被拒（越界、无权限、已删除）时如实告知，而不是静默少打包。
		h.Set("X-Gofs-Archive-Skipped", fmt.Sprintf("%d", len(skipped)))
	}
	h.Set("Cache-Control", "no-store")

	zw := zip.NewWriter(w)
	defer zw.Close()

	method := uint16(zip.Deflate)
	if s.useStore() {
		method = zip.Store
	} else {
		level := s.flateLevel()
		zw.RegisterCompressor(zip.Deflate, func(out io.Writer) (io.WriteCloser, error) {
			return flate.NewWriter(out, level)
		})
	}

	for _, it := range items {
		if err := r.Context().Err(); err != nil {
			// 客户端断开了，停止打包。
			return
		}
		if it.info.IsDir() {
			// 目录以结尾斜杠的条目表示。
			hdr := &zip.FileHeader{
				Name:     it.rel + "/",
				Method:   zip.Store,
				Modified: it.info.ModTime(),
			}
			hdr.SetMode(it.info.Mode())
			if _, err := zw.CreateHeader(hdr); err != nil {
				s.logger.Errorf("打包目录 %s 失败: %v", it.rel, err)
				return
			}
			continue
		}

		hdr := &zip.FileHeader{
			Name:     it.rel,
			Method:   method,
			Modified: it.info.ModTime(),
		}
		hdr.SetMode(0o644)
		fw, err := zw.CreateHeader(hdr)
		if err != nil {
			s.logger.Errorf("打包条目 %s 失败: %v", it.rel, err)
			return
		}
		src, err := os.Open(it.abs)
		if err != nil {
			// 无法读取时写入说明文本，保持包结构完整。
			_, _ = io.WriteString(fw, fmt.Sprintf("[gofs] 无法读取该文件: %v\n", err))
			continue
		}
		_, cerr := io.Copy(fw, src)
		src.Close()
		if cerr != nil {
			s.logger.Errorf("打包复制 %s 失败: %v", it.rel, cerr)
			return
		}
	}
}

// collectTree 递归收集 dir 下的全部条目，rel 相对于 dir。
// 单个条目不可读时跳过，不阻塞整个打包。
func collectTree(dir string) ([]zipItem, error) {
	var items []zipItem
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if d != nil && d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if p == dir {
			return nil
		}
		rel, rerr := filepath.Rel(dir, p)
		if rerr != nil {
			return nil
		}
		info, ierr := d.Info()
		if ierr != nil {
			return nil
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return nil // 不打包软链，避免导出根目录之外的内容
		}
		items = append(items, zipItem{abs: p, rel: filepath.ToSlash(rel), info: info})
		return nil
	})
	return items, err
}

// collectOne 收集单个选中条目：文件直接返回，目录则连同其内容一起展开。
func collectOne(abs, rel string) ([]zipItem, error) {
	// 用 Lstat：软链本身不是要打包的对象。
	info, err := os.Lstat(abs)
	if err != nil {
		return nil, err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return nil, nil
	}
	if !info.IsDir() {
		return []zipItem{{abs: abs, rel: rel, info: info}}, nil
	}

	children, err := collectTree(abs)
	if err != nil {
		return nil, err
	}
	out := make([]zipItem, 0, len(children)+1)
	out = append(out, zipItem{abs: abs, rel: rel, info: info})
	for _, c := range children {
		c.rel = path.Join(rel, c.rel)
		out = append(out, c)
	}
	return out, nil
}

// resolvePicks 把选中的 URL 路径解析为磁盘条目。
//
// 每个选中项都要依次过三关：不含 ".."、能解析到服务根之内、具备读权限。
// 任何一关不过都会被跳过（而不是让整个请求失败），跳过的项由调用方
// 通过响应头告知前端。
//
// 这里刻意**不**要求选中项位于当前目录之下：搜索结果是跨目录的，
// 强制落在当前目录会把「搜索后打包所选」这个用法直接堵死。安全性并不
// 依赖这条约束——权限由逐项的 checkPerm 复核（路径级 ACL 照样生效），
// 根外访问由 Resolve 拦掉。包内路径优先取「相对当前目录」，
// 不在当前目录下的（搜索命中）则退回「相对服务根」，避免同名互相覆盖。
func (s *Server) resolvePicks(r *http.Request, urlPath string, picks []string) ([]zipItem, []string) {
	base := strings.TrimSuffix(fsutil.CleanURLPath(urlPath), "/")
	if base == "" {
		base = "/"
	}

	var items []zipItem
	var skipped []string
	seen := make(map[string]bool)

	for _, raw := range picks {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		// ".." 必须显式拒绝：CleanURLPath 会把 ../../a 静默改写成 a，
		// 虽然结果仍在根内，但调用方的逃逸意图被掩盖了（与解压侧一致）。
		if fsutil.HasParentSegment(raw) {
			skipped = append(skipped, raw)
			continue
		}

		// 以 / 开头视为相对服务根，否则视为相对当前目录。
		p := raw
		if !strings.HasPrefix(p, "/") {
			p = path.Join(base, p)
		}
		p = fsutil.CleanURLPath(p)

		// 逐项复核读权限，避免用打包绕过路径级 ACL。
		if s.checkPerm(r, p) == auth.PermNone {
			skipped = append(skipped, raw)
			continue
		}
		abs, err := s.res.Resolve(p)
		if err != nil {
			skipped = append(skipped, raw)
			continue
		}

		rel := strings.Trim(strings.TrimPrefix(p, base), "/")
		if rel == "" {
			// 选中的就是当前目录本身：等于打包全部，但没有可用的包内名。
			skipped = append(skipped, raw)
			continue
		}
		if seen[rel] {
			continue
		}
		got, err := collectOne(abs, rel)
		if err != nil || len(got) == 0 {
			skipped = append(skipped, raw)
			continue
		}
		seen[rel] = true
		items = append(items, got...)
	}
	return items, skipped
}

// zipDownloadName 决定下载时呈现的 zip 文件名。
func zipDownloadName(dir string, items []zipItem, picked bool) string {
	dirName := strings.TrimSuffix(filepath.Base(dir), "/")
	if dirName == "." || dirName == string(filepath.Separator) || dirName == "" {
		dirName = "root"
	}
	if !picked {
		return dirName + ".zip"
	}
	// 只选了一个顶层条目时直接用它的名字，下载下来更直观。
	if len(items) == 1 && !strings.Contains(items[0].rel, "/") {
		return items[0].rel + ".zip"
	}
	return dirName + "-selected.zip"
}

// asciiFallback 生成 Content-Disposition 的 ASCII 备份文件名。
func asciiFallback(name string) string {
	var b strings.Builder
	for _, r := range name {
		if r < 128 && r != '"' && r != '\\' && r >= 32 {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
	}
	return b.String()
}

// urlEncode 做 RFC 3986 百分号编码，用于 filename*。
func urlEncode(s string) string {
	const hexDigits = "0123456789ABCDEF"
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') ||
			c == '-' || c == '_' || c == '.' || c == '~' {
			b.WriteByte(c)
			continue
		}
		b.WriteByte('%')
		b.WriteByte(hexDigits[c>>4])
		b.WriteByte(hexDigits[c&0x0f])
	}
	return b.String()
}
