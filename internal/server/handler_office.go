package server

import (
	"archive/zip"
	"errors"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/xxl6097/gofs/internal/auth"
	"github.com/xxl6097/gofs/internal/fsutil"
	"github.com/xxl6097/gofs/internal/office"
)

// officeEndpoints 把扩展名映射到 office 包认识的格式名。
//
// 只有 OOXML（2007+）。老的 .doc/.xls/.ppt 是 OLE2 复合文档，
// 二进制格式，标准库解不了 —— 那些文件仍然走下载。
var officeEndpoints = map[string]string{
	".docx": "docx",
	".xlsx": "xlsx",
	".pptx": "pptx",
	".docm": "docx", // 带宏的变体，正文结构相同（宏不会被执行）
	".xlsm": "xlsx",
	".pptm": "pptx",
}

// officeFormatFor 判断这个文件名是不是可预览的 Office 文档。
func officeFormatFor(name string) string {
	return officeEndpoints[strings.ToLower(filepath.Ext(name))]
}

// officePreviewable 判断该路径是否属于可预览的 Office 类型。
// 前端用它来决定点文件名是预览还是下载。
func officePreviewable(path string) bool { return officeFormatFor(path) != "" }

// handleOffice 把 Office 文档提取成结构化内容供网页预览。
//
//	GET /__gofs__/office?path=<文件路径>
//
// 返回的是**中性模型**（块、表格、文本片段）而不是 HTML：
// 服务端不生成任何标记，前端用 DOM API 渲染。文档里的内容再刁钻，
// 也注入不进页面 —— 返回 HTML 的写法要处处记得转义，漏一处就是 XSS。
func (s *Server) handleOffice(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "405 Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}

	urlPath := r.URL.Query().Get("path")
	if fsutil.HasParentSegment(urlPath) {
		http.Error(w, "400 Bad Request: 路径中不允许出现 ..", http.StatusBadRequest)
		return
	}
	clean := fsutil.CleanURLPath(urlPath)
	if clean == "/" || strings.HasSuffix(clean, "/") {
		http.Error(w, "400 Bad Request: 需要指定具体文件路径", http.StatusBadRequest)
		return
	}

	// ⚠️ 预览等同于读取文件内容，必须做读权限校验。
	// 这是一条独立路由，不经过 handleRoot 的授权 —— 少这道校验就是
	// 任意文件内容泄漏（隔壁 /__gofs__/extract 就吃过这个亏）。
	p, ok := s.authorize(w, r, clean)
	if !ok {
		return
	}
	if p.Perm < auth.PermRead {
		http.Error(w, "403 Forbidden: 没有读取该文件的权限", http.StatusForbidden)
		return
	}

	abs, err := s.res.Resolve(clean)
	if err != nil {
		http.Error(w, "403 Forbidden", http.StatusForbidden)
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
		http.Error(w, "400 Bad Request: 目标不是文件", http.StatusBadRequest)
		return
	}

	format := officeFormatFor(abs)
	if format == "" {
		http.Error(w, "415 Unsupported Media Type: 仅支持 docx / xlsx / pptx 预览",
			http.StatusUnsupportedMediaType)
		return
	}

	// 单个文件的大小上限。OOXML 是压缩包，一个几十 KB 的文件解出来
	// 可能有几百 MB —— 所以这里挡的是**压缩包本身**的尺寸，
	// 解压后的量由 office 包自己的上限兜住。
	if s.cfg.UploadMaxSize > 0 && st.Size() > s.cfg.UploadMaxSize {
		http.Error(w, "413 Payload Too Large: 文件过大，请下载后打开", http.StatusRequestEntityTooLarge)
		return
	}

	f, err := os.Open(abs)
	if err != nil {
		s.writeErr(w, err)
		return
	}
	defer f.Close()

	zr, err := zip.NewReader(f, st.Size())
	if err != nil {
		// 不是合法的 zip：多半是内容与扩展名不符。
		http.Error(w, "415 Unsupported Media Type: 文件不是有效的 Office 文档（内容与扩展名不符）",
			http.StatusUnsupportedMediaType)
		return
	}

	doc, err := office.Extract(zr, format, office.Options{})
	if err != nil {
		if errors.Is(err, office.ErrNotOffice) {
			http.Error(w, "415 Unsupported Media Type: 无法解析该文档（可能已加密或结构异常）",
				http.StatusUnsupportedMediaType)
			return
		}
		s.writeErr(w, err)
		return
	}
	doc.Title = clean
	s.writeJSON(w, doc)
}
