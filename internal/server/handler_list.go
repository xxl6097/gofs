package server

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path"
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
	// UserFromStartup 表示当前账号由启动参数定义，密码修改会保存为覆盖值。
	UserFromStartup bool `json:"user_from_startup"`
	// PasswordPersistent 表示用户表有落盘路径；为 false 时改密重启后失效。
	PasswordPersistent bool          `json:"password_persistent"`
	Extract            extractLimits `json:"extract_limits"`
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
	// NotFound 表示这是一张 404 外壳：目录不存在，但前端仍能显示
	// 完整的顶栏/主题与「回到根目录」入口，而不是一张裸文本错误页。
	NotFound bool `json:"not_found,omitempty"`
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
	// AllowUninstall 表示注入了 Uninstaller，页面上才显示「卸载」。
	// 与 Restarter 相互独立：只卸载不重启、只重启不卸载都很常见
	//（见 config.Uninstaller）。
	AllowUninstall bool `json:"allow_uninstall"`
	// AllowUsers 表示当前用户可以在页面上管理用户（增删改）。
	AllowUsers bool `json:"allow_users"`
}

// extractLimits 把解压安全上限透给前端展示。
//
// ⚠️ 三个值必须都为正。任一为 0 视为「未配置」：服务端会拒绝解压，
// 页面上的 perms.extract 也会是 false —— 绝不能把 0 当成无限制。
type extractLimits struct {
	MaxTotalBytes int64 `json:"max_total_bytes"`
	MaxFiles      int   `json:"max_files"`
	MaxRatio      int   `json:"max_ratio"`
}

// authGateData 是「未登录且没有有效分享令牌」时下发的最小外壳。
//
// 只包含渲染登录页真正需要的字段：站点标题、路径前缀（拼 API 地址）、
// 当前路径、鉴权标记。**不要**往这里加任何服务端配置（版本、上传上限、
// 归档布局、解压上限、密钥/设置开关…）—— 未认证访问者能读到源码里
// 所有 JSON 字段。
type authGateData struct {
	Title        string `json:"title"`
	PathPrefix   string `json:"path_prefix"`
	Path         string `json:"path"`
	AuthOn       bool   `json:"auth_on"`
	AuthRequired bool   `json:"auth_required"`
}

// restrictedPageData 是匿名访问（分享链接或匿名规则）时下发的页面数据。
//
// 只含当前范围内的列表、权限与导航字段；不含任何全局配置。
// 它既用于匿名分享，也用于 `-a @/:r` 这类匿名规则 —— 两者都没有
// 登录账号，页面只需要能列出/操作范围内的东西。
type restrictedPageData struct {
	PathPrefix string          `json:"path_prefix"`
	Path       string          `json:"path"`
	Name       string          `json:"name"`
	Query      string          `json:"query,omitempty"`
	Listing    *fsutil.Listing `json:"listing"`
	Perms      permsData       `json:"perms"`
	NotFound   bool            `json:"not_found,omitempty"`
}

// extractLimitsConfigured 表示三个解压上限都已配置。
//
// 任一为 0 都视为「未配置」，而不是「无限制」：解压是破坏性写操作，
// 缺少限额时宁可拒绝，也不能静默退化成无上限。
func (s *Server) extractLimitsConfigured() bool {
	return s.cfg.ExtractMaxFiles > 0 &&
		s.cfg.ExtractMaxTotal > 0 &&
		s.cfg.ExtractMaxRatio > 0
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
		// 上限未配置时不能把解压入口画出来：服务端会直接拒绝。
		Extract: rw && s.cfg.AllowExtract && s.extractLimitsConfigured(),
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
		Version:            s.cfg.Version,
		Title:              s.settings.Title(),
		PathPrefix:         s.PathPrefix(),
		Path:               listing.Path,
		Name:               listing.Name,
		Query:              query,
		Listing:            listing,
		Perms:              s.buildPerms(p),
		AllowUpgrade:       s.cfg.Upgrader != nil,
		AllowRestart:       s.cfg.Restarter != nil,
		AllowUninstall:     s.canUninstall(),
		User:               p.User,
		Anonymous:          !p.Authenticated,
		AuthOn:             s.auth.Enabled(),
		UserFromStartup:    s.auth.FromStartup(p.User),
		PasswordPersistent: s.auth.UserFile() != "",
		UploadDated:        s.cfg.UploadDated(),
		UploadDateDir:      s.cfg.UploadDateDir(time.Now()),
		UploadDateLayout:   s.cfg.UploadDateLayout,
		EditMaxSize:        s.settings.EditMaxSize(),
		// 密钥面板要排除分享令牌：管理密钥的接口只认账号凭据
		// （见 handleKeys），所以对分享访客来说那个入口点进去必然 401。
		// 一个「亮着但用不了」的入口比直接不给更糟。
		AllowKeys:     s.cfg.AllowKeys && p.Perm == auth.PermReadWrite && !p.Share,
		UploadMaxSize: s.settings.UploadMaxSize(),
		AllowSettings: admin,
		AllowUsers:    s.cfg.AllowUserManage && s.auth.Enabled() && admin,
		Extract: extractLimits{
			MaxTotalBytes: s.cfg.ExtractMaxTotal,
			MaxFiles:      s.cfg.ExtractMaxFiles,
			MaxRatio:      s.cfg.ExtractMaxRatio,
		},
	}
}

// pageFor 依据请求身份选择下发哪一档页面数据。
//
//   - 未启用鉴权：没有账号可登录，页面就是全部功能；
//   - 具名账号（Basic / 会话）：完整配置（功能零回归）；
//   - 匿名分享 / 匿名规则：只有当前范围的列表与权限，不含全局配置。
//
// 登录页外壳不走这里，见 renderAuthGate。
func (s *Server) pageFor(p permResult, listing *fsutil.Listing, query string, admin, notFound bool) any {
	if !s.auth.Enabled() || p.Account {
		data := s.newPageData(p, listing, query, admin)
		data.NotFound = notFound
		return data
	}
	return restrictedPageData{
		PathPrefix: s.PathPrefix(),
		Path:       listing.Path,
		Name:       listing.Name,
		Query:      query,
		Listing:    listing,
		Perms:      s.buildPerms(p),
		NotFound:   notFound,
	}
}

// renderIndex 渲染目录页面。
func (s *Server) renderIndex(w http.ResponseWriter, r *http.Request, listing *fsutil.Listing, p permResult) {
	s.writeHTML(w, s.pageFor(p, listing, "", s.isAdminRequest(r), false))
}

// searchMaxEntries 是单次搜索最多回多少条。
//
// 与目录列举的上限分开（那个是 --list-max-entries）：搜索要跨整棵子树，
// 一次命中上万条很常见，而前端的表格也渲染不了那么多。
// 超出的部分由 Listing.Truncated / TotalAll 如实告知用户。
const searchMaxEntries = 3000

// serveSearch 处理 ?q= 搜索。?mode= 选择匹配方式（fuzzy/exact/wildcard/regex），
// 缺省或无法识别时按 fuzzy 处理。
func (s *Server) serveSearch(w http.ResponseWriter, r *http.Request, abs, urlPath, pattern string) {
	// 搜索会遍历整棵子树：与上传/解压共用重任务闸门，避免被反复触发。
	release, ok := s.acquireJob(r.Context())
	if !ok {
		w.Header().Set("Retry-After", "2")
		http.Error(w, "503 Service Unavailable: 搜索任务过多，请稍后重试", http.StatusServiceUnavailable)
		return
	}
	defer release()

	entries, total, err := s.searchSecure(r, urlPath, abs, fsutil.SearchOptions{
		Pattern: pattern,
		Mode:    fsutil.ParseSearchMode(r.URL.Query().Get("mode")),
		Hidden:  s.cfg.Hidden,
		Limit:   searchMaxEntries,
		Ctx:     r.Context(),
	})
	if err != nil {
		// 客户端已经断开：遍历是被 context 停下来的，不必再写响应/记错误日志。
		if r.Context().Err() != nil {
			return
		}
		if errors.Is(err, errPathChanged) {
			http.Error(w, "409 Conflict: 搜索目录在请求期间发生变化，请重试", http.StatusConflict)
			return
		}
		// 表达式非法（坏正则 / 坏通配符）是调用方的输入错误，回 400。
		// 不能交给 writeErr —— 它把未知错误一律当 500。
		if errors.Is(err, fsutil.ErrBadSearchPattern) {
			http.Error(w, "400 Bad Request: "+err.Error(), http.StatusBadRequest)
			return
		}
		s.writeErr(w, err)
		return
	}
	p, ok := s.authorize(w, r, urlPath)
	if !ok {
		// 第二次 authorize 可能因认证限速失败；它已经写好响应，
		// 不能再继续写页面，否则 429/401 会和 HTML 拼在一起。
		return
	}
	// 截断要如实上报：TotalAll 是命中总数、Truncated 表示只回了一部分。
	// 少这两个字段的话，命中一万条只显示前 3000，界面上却看不出被截断 ——
	// 用户会以为「就这么多」。目录列举早就是这么做的，搜索照同一口径。
	listing := &fsutil.Listing{
		Path:      fsutil.CleanURLPath(urlPath),
		Name:      "搜索: " + pattern,
		IsDir:     true,
		Entries:   entries,
		Total:     len(entries),
		TotalAll:  total,
		Truncated: total > len(entries),
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
	s.writeHTML(w, s.pageFor(p, listing, pattern, s.isAdminRequest(r), false))
}

// writeHTML 把初始数据注入 index.html 后输出。
func (s *Server) writeHTML(w http.ResponseWriter, data any) {
	s.writeHTMLStatus(w, data, http.StatusOK)
}

// writeHTMLStatus 与 writeHTML 相同，但允许指定状态码：
// 目录不存在时仍然返回可用的页面外壳（按身份决定完整/受限），
// 只是 HTTP 状态为 404。
func (s *Server) writeHTMLStatus(w http.ResponseWriter, data any, status int) {
	// 目录页/搜索页可能带着上万条条目，同样需要「无进展超时」。
	w, reset := withWriteProgress(w, s.cfg.DownloadTimeout)
	defer reset()

	// 默认 JSON 编码会转义 < > &，可安全地放进 <script> 内。
	raw, err := json.Marshal(data)
	if err != nil {
		s.writeErr(w, err)
		return
	}
	out := bytes.Replace(s.ui.index, []byte("__GOFS_DATA__"), raw, 1)

	// 前缀会落进 href/src 属性，必须做 HTML 转义。输入侧已经拒绝
	// 引号/尖括号等字符，但这里是最后一道防线：旧版本写入的 settings.json、
	// 或将来放宽输入校验，都不该让前缀直接变成标记。
	base := html.EscapeString(s.PathPrefix())
	out = bytes.ReplaceAll(out, []byte("__GOFS_ASSETS__"), []byte(base+"/fs/assets/"))
	out = bytes.ReplaceAll(out, []byte("__GOFS_BASE__"), []byte(base))

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_, _ = w.Write(out)
}

// indexSandboxNeeded 表示服务目录里的 index.html 是否必须进入沙箱。
//
// 只要有任一远程写入口（上传 / 在线编辑 / 解压），index.html 就可能被
// 非管理员写入；把它当同源页面执行等于把本站凭据与接口交给写入者。
// 纯只读部署（内容只由本机运维更新）不需要沙箱。
func (s *Server) indexSandboxNeeded() bool {
	if s.cfg.UnsafeIndexHTML {
		return false
	}
	return s.cfg.AllowUpload || s.cfg.AllowEdit || s.cfg.AllowExtract
}

// hardenIndex 给可写部署中的 index.html 加上与普通 HTML 文件相同的沙箱。
func (s *Server) hardenIndex(w http.ResponseWriter) {
	if s.indexSandboxNeeded() {
		// allow-scripts 保留静态页面的脚本；没有 allow-same-origin，
		// 文档处于不透明源，读不到本站 cookie / storage / 同源接口。
		w.Header().Set("Content-Security-Policy", "sandbox allow-scripts; frame-ancestors 'none'")
	}
}

// tryServeIndex 在目录下尝试返回 index.html。
func (s *Server) tryServeIndex(w http.ResponseWriter, r *http.Request, urlPath, dir string) bool {
	idx := filepath.Join(dir, "index.html")
	if !s.cfg.AllowSymlink {
		// 不服务指向根外的 index.html：它会被浏览器当成同源页面执行。
		if fi, err := os.Lstat(idx); err == nil && fi.Mode()&os.ModeSymlink != 0 {
			return false
		}
	}
	st, err := os.Stat(idx)
	if err != nil || st.IsDir() {
		return false
	}
	// index.html 本身也可能是软链/大小写别名：按真实目标复核读权限。
	realURL, realAbs, perm, within := s.realPermOf(r, idx)
	idxURL := path.Join(urlPath, "index.html")
	if within {
		if perm < auth.PermRead {
			return false
		}
		s.noteAuthorizedPath(r, idxURL, realAbs)
		if realURL != idxURL {
			s.noteAuthorizedPath(r, realURL, realAbs)
		}
	} else if !s.cfg.AllowSymlink || s.shareTokenOf(r) != "" || uploadKeyToken(r) != "" {
		return false
	}
	f, err := s.secureOpenRead(r, idxURL, idx)
	if err != nil {
		return false
	}
	defer f.Close()
	w.Header().Set("X-Content-Type-Options", "nosniff")
	s.hardenIndex(w)
	s.setUserContentCache(w)
	w, reset := withWriteProgress(w, s.cfg.DownloadTimeout)
	defer reset()
	http.ServeContent(w, r, "index.html", st.ModTime(), f)
	return true
}

// serveIndexFallback 在 SPA 模式下把未命中的路径回退到根目录 index.html。
func (s *Server) serveIndexFallback(w http.ResponseWriter, r *http.Request) {
	idx := filepath.Join(s.res.Root(), "index.html")
	if !s.cfg.AllowSymlink {
		if fi, err := os.Lstat(idx); err == nil && fi.Mode()&os.ModeSymlink != 0 {
			http.Error(w, "404 Not Found", http.StatusNotFound)
			return
		}
	}
	realURL, realAbs, perm, within := s.realPermOf(r, idx)
	if within {
		if perm < auth.PermRead {
			http.Error(w, "404 Not Found", http.StatusNotFound)
			return
		}
		s.noteAuthorizedPath(r, "/index.html", realAbs)
		if realURL != "/index.html" {
			s.noteAuthorizedPath(r, realURL, realAbs)
		}
	} else if !s.cfg.AllowSymlink || s.shareTokenOf(r) != "" || uploadKeyToken(r) != "" {
		http.Error(w, "404 Not Found", http.StatusNotFound)
		return
	}
	f, err := s.secureOpenRead(r, "/index.html", idx)
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
	w.Header().Set("X-Content-Type-Options", "nosniff")
	s.hardenIndex(w)
	s.setUserContentCache(w)
	w, reset := withWriteProgress(w, s.cfg.DownloadTimeout)
	defer reset()
	http.ServeContent(w, r, "index.html", st.ModTime(), f)
}

// renderAuthGate 渲染一张「尚未登录」的最小外壳。
//
// 只下发 authGateData 里的五个字段。绝不能退回 pageData：那个结构体会
// 把版本、上传/编辑/解压上限、归档布局、密钥/设置/升级开关等配置
// 以零值或默认值的形式整套写进页面 JSON，未认证访问者直接可读。
//
// 前端拿到 auth_required 后弹登录框；登录成功再通过 /fs/auth 拿到完整
// 权限与配置。listing 刻意不下发（前端把 null 当作“还没读过目录”）。
func (s *Server) renderAuthGate(w http.ResponseWriter, r *http.Request, urlPath string) {
	clean := fsutil.CleanURLPath(urlPath)
	s.writeHTML(w, authGateData{
		Title:      s.settings.Title(),
		PathPrefix: s.PathPrefix(),
		Path:       clean,
		AuthOn:     true,
		// auth_required 是登录页的触发开关，必须保留。
		AuthRequired: true,
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
	w, reset := withWriteProgress(w, s.cfg.DownloadTimeout)
	defer reset()
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
