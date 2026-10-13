package server

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/xxl6097/gofs/internal/auth"
	"github.com/xxl6097/gofs/internal/fsutil"
)

// uiTemplate 保存内嵌前端资源的原始内容。
type uiTemplate struct {
	index    []byte
	notFound []byte
}

// loadUITemplate 从资源文件系统载入 index.html（必需）与 404.html（可选）。
func loadUITemplate(assets fs.FS) (*uiTemplate, error) {
	index, err := fs.ReadFile(assets, "index.html")
	if err != nil {
		return nil, fmt.Errorf("前端资源缺少 index.html: %w", err)
	}
	t := &uiTemplate{index: index}
	if nf, err := fs.ReadFile(assets, "404.html"); err == nil {
		t.notFound = nf
	}
	return t, nil
}

// permsData 描述当前用户在本路径上的可用操作，供前端决定按钮显隐。
type permsData struct {
	Read    bool `json:"read"`
	Write   bool `json:"write"`
	Delete  bool `json:"delete"`
	Search  bool `json:"search"`
	Archive bool `json:"archive"`
	Extract bool `json:"extract"`
	Edit    bool `json:"edit"`
}

// pageData 是注入到页面里的初始状态。
type pageData struct {
	Version string `json:"version"`
	// Title 为站点标题，前端用它设置浏览器标签页与顶栏品牌名。
	Title      string          `json:"title"`
	PathPrefix string          `json:"path_prefix"`
	Path       string          `json:"path"`
	Name       string          `json:"name"`
	Query      string          `json:"query,omitempty"`
	Listing    *fsutil.Listing `json:"listing"`
	Perms      permsData       `json:"perms"`
	User       string          `json:"user"`
	Anonymous  bool            `json:"anonymous"`
	AuthOn     bool            `json:"auth_on"`
	Extract    extractLimits   `json:"extract_limits"`
	// UploadDated 表示上传是否会自动归档到日期目录。
	UploadDated bool `json:"upload_dated"`
	// UploadDateDir 为当前时刻对应的归档目录段，例如 "2026/09/28"。
	UploadDateDir string `json:"upload_date_dir"`
	// UploadDateLayout 为归档布局的原始写法，供前端展示说明。
	UploadDateLayout string `json:"upload_date_layout"`
	// EditMaxSize 为在线编辑的大小上限，前端据此判断是否给出「编辑」入口。
	EditMaxSize int64 `json:"edit_max_size"`
	// AuthRequired 表示这是一张尚未登录的应用外壳（无任何真实数据）。
	AuthRequired bool `json:"auth_required"`
	// AllowKeys 表示当前用户可以在页面上管理上传密钥。
	AllowKeys bool `json:"allow_keys"`
	// UploadMaxSize 为当前生效的单文件上传上限（字节），0 表示不限制。
	UploadMaxSize int64 `json:"upload_max_size"`
	// AllowSettings 表示当前用户可以在页面上修改服务级设置
	// （切换根目录、调整上传上限）。
	AllowSettings bool `json:"allow_settings"`
	// AllowUpgrade 表示服务端注入了 Upgrader，页面上才显示「检查更新」。
	// 未注入时整块都不显示 —— 免得给一个点了必然 404 的按钮。
	AllowUpgrade bool `json:"allow_upgrade"`
	// AllowRestart 表示服务端注入了 Restarter，页面上才显示「重启」。
	// 与 AllowUpgrade 相互独立：只重启不升级的部署很常见。
	AllowRestart bool `json:"allow_restart"`
	// AllowUninstall 表示注入的 Restarter **还实现了 Uninstaller**，
	// 页面上才显示「卸载」。只实现 Restarter 的部署拿不到这个按钮 ——
	// 能力由值的方法集决定（见 config.Uninstaller）。
	AllowUninstall bool `json:"allow_uninstall"`
	// AllowUsers 表示当前用户可以在页面上管理用户（增删改）。
	AllowUsers bool `json:"allow_users"`
}

// extractLimits 把解压安全上限透给前端展示。
type extractLimits struct {
	MaxTotalBytes int64 `json:"max_total_bytes"`
	MaxFiles      int   `json:"max_files"`
	MaxRatio      int   `json:"max_ratio"`
}

// buildPerms 依据授权结果与全局开关计算页面可用能力。
func (s *Server) buildPerms(p permResult) permsData {
	canRead := p.Perm != auth.PermNone
	rw := p.Perm == auth.PermReadWrite
	return permsData{
		Read:    canRead,
		Write:   rw && s.cfg.AllowUpload,
		Delete:  rw && s.cfg.AllowDelete,
		Search:  canRead && s.cfg.AllowSearch,
		Archive: canRead && s.cfg.AllowArchive,
		Extract: rw && s.cfg.AllowExtract,
		// Edit 必须跟 Write/Delete/Extract 一样用 rw，不能只要求 canRead。
		// 漏了这一条，只读账号会看到「编辑」按钮，点下去必然被
		// textSave 的 403（它要求读写权限）挡回来 —— 按钮等于骗人。
		// 分享链接是只读的，这个 bug 在它上面最容易暴露。
		Edit: rw && s.cfg.AllowEdit,
	}
}

// newPageData 组装注入页面的初始状态。
// admin 决定是否显示「服务设置」入口（切换根目录、调整上传上限）。
func (s *Server) newPageData(p permResult, listing *fsutil.Listing, query string, admin bool) pageData {
	return pageData{
		Version:          s.cfg.Version,
		Title:            s.settings.Title(),
		PathPrefix:       s.PathPrefix(),
		Path:             listing.Path,
		Name:             listing.Name,
		Query:            query,
		Listing:          listing,
		Perms:            s.buildPerms(p),
		AllowUpgrade:     s.cfg.Upgrader != nil,
		AllowRestart:     s.cfg.Restarter != nil,
		AllowUninstall:   s.canUninstall(),
		User:             p.User,
		Anonymous:        !p.Authenticated,
		AuthOn:           s.auth.Enabled(),
		UploadDated:      s.cfg.UploadDated(),
		UploadDateDir:    s.cfg.UploadDateDir(time.Now()),
		UploadDateLayout: s.cfg.UploadDateLayout,
		EditMaxSize:      s.settings.EditMaxSize(),
		AllowKeys:        s.cfg.AllowKeys && p.Perm == auth.PermReadWrite,
		UploadMaxSize:    s.settings.UploadMaxSize(),
		AllowSettings:    admin,
		AllowUsers:       s.cfg.AllowUserManage && s.auth.Enabled() && admin,
		Extract: extractLimits{
			MaxTotalBytes: s.cfg.ExtractMaxTotal,
			MaxFiles:      s.cfg.ExtractMaxFiles,
			MaxRatio:      s.cfg.ExtractMaxRatio,
		},
	}
}

// renderIndex 渲染目录页面。
func (s *Server) renderIndex(w http.ResponseWriter, r *http.Request, listing *fsutil.Listing, p permResult) {
	s.writeHTML(w, s.newPageData(p, listing, "", s.isAdminRequest(r)))
}

// serveSearch 处理 ?q= 搜索。?mode= 选择匹配方式（fuzzy/exact/wildcard/regex），
// 缺省或无法识别时按 fuzzy 处理。
func (s *Server) serveSearch(w http.ResponseWriter, r *http.Request, abs, urlPath, pattern string) {
	entries, err := fsutil.Search(s.res, abs, fsutil.SearchOptions{
		Pattern: pattern,
		Mode:    fsutil.ParseSearchMode(r.URL.Query().Get("mode")),
		Hidden:  s.cfg.Hidden,
		Limit:   3000,
	})
	if err != nil {
		// 表达式非法（坏正则 / 坏通配符）是调用方的输入错误，回 400。
		// 不能交给 writeErr —— 它把未知错误一律当 500。
		if errors.Is(err, fsutil.ErrBadSearchPattern) {
			http.Error(w, "400 Bad Request: "+err.Error(), http.StatusBadRequest)
			return
		}
		s.writeErr(w, err)
		return
	}
	p, _ := s.authorize(w, r, urlPath)
	listing := &fsutil.Listing{
		Path:    fsutil.CleanURLPath(urlPath),
		Name:    "搜索: " + pattern,
		IsDir:   true,
		Entries: entries,
		Total:   len(entries),
	}
	for _, e := range entries {
		if e.IsDir {
			listing.DirCount++
		} else {
			listing.FileSize += e.Size
		}
	}

	if r.URL.Query().Has("json") || wantsJSON(r) {
		s.writeJSON(w, listing)
		return
	}
	s.writeHTML(w, s.newPageData(p, listing, pattern, s.isAdminRequest(r)))
}

// writeHTML 把初始数据注入 index.html 后输出。
func (s *Server) writeHTML(w http.ResponseWriter, data pageData) {
	// 默认 JSON 编码会转义 < > &，可安全地放进 <script> 内。
	raw, err := json.Marshal(data)
	if err != nil {
		s.writeErr(w, err)
		return
	}
	out := bytes.Replace(s.ui.index, []byte("__GOFS_DATA__"), raw, 1)

	base := s.PathPrefix()
	out = bytes.ReplaceAll(out, []byte("__GOFS_ASSETS__"), []byte(base+"/fs/assets/"))
	out = bytes.ReplaceAll(out, []byte("__GOFS_BASE__"), []byte(base))

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(out)
}

// tryServeIndex 在目录下尝试返回 index.html。
func (s *Server) tryServeIndex(w http.ResponseWriter, r *http.Request, dir string) bool {
	idx := filepath.Join(dir, "index.html")
	st, err := os.Stat(idx)
	if err != nil || st.IsDir() {
		return false
	}
	f, err := os.Open(idx)
	if err != nil {
		return false
	}
	defer f.Close()
	http.ServeContent(w, r, "index.html", st.ModTime(), f)
	return true
}

// serveIndexFallback 在 SPA 模式下把未命中的路径回退到根目录 index.html。
func (s *Server) serveIndexFallback(w http.ResponseWriter, r *http.Request) {
	idx := filepath.Join(s.res.Root(), "index.html")
	f, err := os.Open(idx)
	if err != nil {
		if s.ui.notFound != nil {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write(s.ui.notFound)
			return
		}
		http.Error(w, "404 Not Found", http.StatusNotFound)
		return
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		http.Error(w, "500 Internal Server Error", http.StatusInternalServerError)
		return
	}
	http.ServeContent(w, r, "index.html", st.ModTime(), f)
}

// renderAuthGate 渲染一张「尚未登录」的应用外壳。
//
// 页面里不含任何真实数据（listing 为空数组），只带 auth_required 标记，
// 由前端弹出登录框；登录成功后前端自行 fetch 数据再渲染。
// 这样未登录者拿不到任何文件名、目录结构或配置信息。
func (s *Server) renderAuthGate(w http.ResponseWriter, r *http.Request, urlPath string) {
	clean := fsutil.CleanURLPath(urlPath)
	s.writeHTML(w, pageData{
		Version:    s.cfg.Version,
		Title:      s.settings.Title(),
		PathPrefix: s.PathPrefix(),
		Path:       clean,
		Name:       "/",
		Listing:    &fsutil.Listing{Path: clean, Name: "/", IsDir: true, Entries: []fsutil.Entry{}},
		Perms:      permsData{},
		AuthOn:     true,
		// 标题也要带上：登录页就在品牌名旁边，漏了它会让品牌名先显示
		// 默认的 gofs、登录之后才跳成配置值。
		AuthRequired: true,
		EditMaxSize:  s.settings.EditMaxSize(),
	})
}

// handleAsset 返回内嵌或自定义的前端静态资源。
func (s *Server) handleAsset(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimPrefix(r.URL.Path, "/fs/assets/")
	if name == "" || strings.Contains(name, "..") {
		http.NotFound(w, r)
		return
	}
	f, err := s.assets.Open(name)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			http.NotFound(w, r)
			return
		}
		s.writeErr(w, err)
		return
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		s.writeErr(w, err)
		return
	}
	if st.IsDir() {
		http.NotFound(w, r)
		return
	}
	// 内嵌资源在开发期变化频繁，统一短缓存 + 强校验。
	w.Header().Set("Cache-Control", "no-cache")
	rs, ok := f.(io.ReadSeeker)
	if !ok {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("ETag", s.assetETag(name, st, rs))
	http.ServeContent(w, r, name, st.ModTime(), rs)
}

// assetETag 给出某个前端资源的 ETag。
//
// ⚠️ 内嵌资源（embed.FS）**不能**用 modTime+size 拼：embed 的文件 ModTime
// 恒为零值，时间那一项永远不变，ETag 就退化成「只认文件大小」。于是内容改了
// 而字节数没变时 ETag 不变，浏览器一直收到 304、继续跑旧 JS ——
// 表现就是「前端改了、部署了，页面还是老样子」，而且没有 Last-Modified
// 可以退（零 modtime 会被 ServeContent 跳过）。所以这里按**内容**算。
//
// 走 WithAssetsDir 的磁盘目录仍用 modTime+size：那边的 mtime 是真的，
// 而且作者靠它做「改前端不用重启」的热更新，按内容算反而要多读一遍文件。
// 内嵌内容的散列只算一次，之后走缓存（embed 的内容在运行期不会变）。
func (s *Server) assetETag(name string, st fs.FileInfo, rs io.ReadSeeker) string {
	fallback := fmt.Sprintf(`"a-%x-%x"`, st.ModTime().UnixNano(), st.Size())
	if !st.ModTime().IsZero() {
		return fallback
	}
	if v, ok := s.assetTags.Load(name); ok {
		return v.(string)
	}
	h := sha256.New()
	if _, err := io.Copy(h, rs); err != nil {
		return fallback
	}
	// 读到了 EOF，要退回开头，否则下面的 ServeContent 什么都发不出去。
	if _, err := rs.Seek(0, io.SeekStart); err != nil {
		return fallback
	}
	tag := fmt.Sprintf(`"a-%x"`, h.Sum(nil)[:12])
	s.assetTags.Store(name, tag)
	return tag
}
