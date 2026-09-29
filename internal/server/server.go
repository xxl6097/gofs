// Package server 实现 gofs 的 HTTP 层：路由分发、权限校验与页面渲染。
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
	"net/url"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"
	"time"

	"github.com/xxl6097/gofs/internal/auth"
	"github.com/xxl6097/gofs/internal/config"
	"github.com/xxl6097/gofs/internal/fsutil"
	"github.com/xxl6097/gofs/internal/uploadkey"
)

// Server 持有全部运行时依赖。
type Server struct {
	cfg   *config.Config
	res   *fsutil.Resolver
	auth  *auth.Authenticator
	keys  *uploadkey.Store
	guard *guard
	// settings 存放可以在运行期修改、且会被并发读取的设置。
	settings *runtimeSettings
	assets   fs.FS
	ui       *uiTemplate
	logger   *Logger
}

// WebDAV 风格的方法，标准库未定义。
const (
	// MethodMkcol 创建集合（目录）。
	MethodMkcol = "MKCOL"
	// MethodMove 移动/重命名资源。
	MethodMove = "MOVE"
	// MethodCopy 复制资源。
	MethodCopy = "COPY"
)

// New 构造 Server。assets 为内嵌前端资源（根目录即 http 路径根）。
func New(cfg *config.Config, embedded fs.FS) (*Server, error) {
	res, err := fsutil.NewResolver(cfg.ServePath, cfg.AllowSymlink)
	if err != nil {
		return nil, err
	}
	// 鉴权：启动参数给的规则 + 用户表（可运行期增删改）。
	authn, err := auth.Open(cfg.AuthRules, cfg.UserFile)
	if err != nil {
		return nil, err
	}

	assetsFS := embedded
	if cfg.AssetsDir != "" {
		info, err := os.Stat(cfg.AssetsDir)
		if err != nil {
			return nil, fmt.Errorf("前端资源目录不可用: %w", err)
		}
		if !info.IsDir() {
			return nil, fmt.Errorf("前端资源路径 %q 不是目录", cfg.AssetsDir)
		}
		assetsFS = os.DirFS(cfg.AssetsDir)
	}

	ui, err := loadUITemplate(assetsFS)
	if err != nil {
		return nil, err
	}

	logger, err := NewLogger(cfg.LogFormat, cfg.LogFile)
	if err != nil {
		return nil, err
	}

	// 上传密钥存储：路径为空时只保存在内存中。
	keys, err := uploadkey.New(cfg.KeyFile)
	if err != nil {
		_ = logger.Close()
		return nil, err
	}

	return &Server{
		cfg:      cfg,
		res:      res,
		auth:     authn,
		keys:     keys,
		guard:    newGuard(cfg),
		settings: newRuntimeSettings(cfg, res.Root()),
		assets:   assetsFS,
		ui:       ui,
		logger:   logger,
	}, nil
}

// Handler 返回配置好中间件的 http.Handler。
// 中间件由外到内的顺序：
// 路径前缀 -> 并发闸门 -> 访问日志 -> CORS -> 跨站校验 -> panic 恢复 -> 路由。
//
// 并发闸门放在最外层，是为了在真正开始干活之前就把多余请求挡掉；
// 它拒绝的请求不写访问日志 —— 服务过载时再拼命写日志只会雪上加霜。
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/__gofs__/health", s.handleHealth)
	mux.HandleFunc("/__gofs__/auth", s.handleAuth)
	mux.HandleFunc("/__gofs__/keys", s.handleKeys)
	mux.HandleFunc("/__gofs__/settings", s.handleSettings)
	mux.HandleFunc("/__gofs__/users", s.handleUsers)
	mux.HandleFunc("/__gofs__/extract", s.handleExtract)
	mux.HandleFunc("/__gofs__/text", s.handleText)
	mux.HandleFunc("/__gofs__/assets/", s.handleAsset)
	mux.HandleFunc("/", s.handleRoot)

	var h http.Handler = mux
	h = s.withRecover(h)
	h = s.withCSRFProtect(h)
	h = s.withCORS(h)
	h = s.withLogging(h)
	h = s.withConcurrency(h)
	h = s.withPathPrefix(h)
	return h
}

// Root 返回被服务的根目录绝对路径。
func (s *Server) Root() string { return s.res.Root() }

// AuthEnabled 表示当前是否存在任何鉴权规则。
//
// 账号可以来自启动参数，也可以来自用户表，所以不能只看 cfg.AuthRules 的条数 ——
// 一个有 users.json、没写 -a 的实例其实是要登录的。
func (s *Server) AuthEnabled() bool { return s.auth.Enabled() }

// Close 关闭日志等资源。
func (s *Server) Close() error {
	if s.keys != nil {
		if err := s.keys.Flush(); err != nil {
			s.logger.Errorf("写出上传密钥失败: %v", err)
		}
	}
	return s.logger.Close()
}

// ---------------------------------------------------------------------------
// 中间件
// ---------------------------------------------------------------------------

// statusRecorder 记录响应状态码与字节数，并透传 Flush 以支持 SSE。
type statusRecorder struct {
	http.ResponseWriter
	status int
	wrote  bool
	bytes  int64
}

// WriteHeader 记录首次写出的状态码。
func (r *statusRecorder) WriteHeader(code int) {
	if !r.wrote {
		r.status = code
		r.wrote = true
	}
	r.ResponseWriter.WriteHeader(code)
}

// Write 记录响应体长度。
func (r *statusRecorder) Write(b []byte) (int, error) {
	if !r.wrote {
		r.status = http.StatusOK
		r.wrote = true
	}
	n, err := r.ResponseWriter.Write(b)
	r.bytes += int64(n)
	return n, err
}

// Flush 透传到底层 ResponseWriter，SSE 依赖它逐条推送。
func (r *statusRecorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Unwrap 让 http.ResponseController 能找到底层实现。
func (r *statusRecorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

// withRecover 兜住 panic，避免单个请求打挂进程。
func (s *Server) withRecover(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			rec := recover()
			if rec == nil {
				return
			}
			s.logger.Errorf("%s %s panic: %v\n%s", r.Method, r.URL.Path, rec, debug.Stack())
			if sr, ok := w.(*statusRecorder); !ok || !sr.wrote {
				http.Error(w, "500 Internal Server Error", http.StatusInternalServerError)
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// withLogging 记录访问日志。
func (s *Server) withLogging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		// 用 defer 保证 panic 场景下也能留下访问记录。
		defer func() {
			user, _, _ := r.BasicAuth()
			if user == "" {
				user = r.URL.Query().Get("user")
			}
			// 会话身份也要记上，否则登录之后的媒体请求在日志里全是空用户 ——
			// 而「谁在读这个大文件」正是访问日志最该回答的问题。
			if user == "" {
				if u, ok := s.auth.ResolveSession(sessionToken(r)); ok {
					user = u
				}
			}
			if !rec.wrote {
				rec.status = http.StatusOK
			}
			s.logger.Log(r, rec.status, user, start)
		}()
		next.ServeHTTP(rec, r)
	})
}

// withCORS 按需注入 CORS 响应头并处理预检请求。
func (s *Server) withCORS(next http.Handler) http.Handler {
	if !s.cfg.EnableCORS {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Access-Control-Allow-Origin", "*")
		h.Set("Access-Control-Allow-Methods", "GET, PUT, POST, DELETE, MKCOL, MOVE, OPTIONS, HEAD")
		h.Set("Access-Control-Allow-Headers", "Authorization, Content-Type, X-Update-Range, Destination, Range, X-Gofs-Ajax")
		h.Set("Access-Control-Expose-Headers", "Content-Length, Content-Range, Location, X-Gofs-Offset, X-Gofs-Path")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// pathPrefixResponseWriter 已由 statusRecorder 取代，此处不再需要额外包装。

// withPathPrefix 处理访问前缀。
func (s *Server) withPathPrefix(next http.Handler) http.Handler {
	prefix := s.cfg.PathPrefix
	if prefix == "" {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		if p == prefix {
			http.Redirect(w, r, prefix+"/", http.StatusMovedPermanently)
			return
		}
		if !strings.HasPrefix(p, prefix+"/") {
			http.NotFound(w, r)
			return
		}
		r2 := r.Clone(r.Context())
		r2.URL.Path = strings.TrimPrefix(p, prefix)
		next.ServeHTTP(w, r2)
	})
}

// ---------------------------------------------------------------------------
// 权限
// ---------------------------------------------------------------------------

// permResult 描述一次鉴权的结果。
type permResult struct {
	// Perm 为授权级别。
	Perm auth.Permission
	// User 为登录用户名（匿名时为空）。
	User string
	// Authenticated 表示是否携带并通过了具名账号校验。
	Authenticated bool
	// UploadKey 非 nil 表示本次请求由上传密钥授权，而非账号密码。
	UploadKey *uploadkey.Key
}

// credentials 从请求中提取凭据，优先 Basic 头，其次查询参数。
func credentials(r *http.Request) (user, pass string, ok bool) {
	if user, pass, ok = r.BasicAuth(); ok {
		return user, pass, true
	}
	if u := r.URL.Query().Get("user"); u != "" {
		return u, r.URL.Query().Get("pass"), true
	}
	return "", "", false
}

// ---------------------------------------------------------------------------
// 会话 cookie
//
// 为什么需要它：页面用 HTTP Basic，凭据握在 JS 手里，由 apiFetch 逐请求加
// Authorization 头。但**浏览器自己**发起的请求不会带这个头 ——
// <video>/<img> 的加载、<a download> 的下载都不经过 JS。
// 结果是：开了鉴权就放不了大视频（拿不到数据，只能整份 fetch 成 blob 进内存），
// 下载也只能绕道 blob。
//
// 登录时下发一个 HttpOnly 的会话 cookie 就能解决：浏览器会自动把它附加到
// 同源的这些请求上，于是 Range 流式播放、拖动进度条、原生下载全部可用，
// 与文件大小无关。
//
// 安全取舍见 session.go 与 handleAuth 的注释。

// sessionCookieName 是会话 cookie 的名字。
const sessionCookieName = "gofs_sid"

// sessionCookiePath 返回 cookie 的作用路径。
//
// 必须带上访问前缀：部署在 /files 下时，cookie 若只写 Path=/，
// 浏览器确实会发，但反过来若配了前缀却不写进去，前缀下的请求就收不到它。
// 带前缀是两种部署都成立的那个写法。
func (s *Server) sessionCookiePath() string {
	p := s.cfg.PathPrefix
	if p == "" {
		return "/"
	}
	return p + "/"
}

// setSessionCookie 下发会话 cookie。
//
// HttpOnly：JS 读不到 token，XSS 偷不走。
// SameSite=Lax：跨站的子资源请求（<img>/<video>/<iframe>）不会带上它 ——
//
//	挡住「诱导已登录用户访问恶意页面 → 借其身份读私有文件」这类环境授权攻击。
//	跨站表单 POST 同理。代价是跨站顶层导航会带（Lax 的语义），可接受。
//	（同站子域不算跨站，所以有子域的部署仍有 CSRF 面 —— 那由 withCSRFProtect
//	对写方法的同源校验兜住，与本次改动无关。）
//
// Secure：仅在 HTTPS 下加，否则纯 HTTP 部署会收不到 cookie。
func (s *Server) setSessionCookie(w http.ResponseWriter, token string) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    token,
		Path:     s.sessionCookiePath(),
		MaxAge:   int(auth.SessionTTL().Seconds()),
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   s.cfg.TLSCert != "",
	})
}

// clearSessionCookie 让浏览器丢掉会话 cookie（值留空 + MaxAge<0）。
func (s *Server) clearSessionCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    "",
		Path:     s.sessionCookiePath(),
		MaxAge:   -1,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   s.cfg.TLSCert != "",
	})
}

// sessionToken 取出请求里的会话 token（没有则空串）。
func sessionToken(r *http.Request) string {
	c, err := r.Cookie(sessionCookieName)
	if err != nil {
		return ""
	}
	return c.Value
}

// credsOf 解析请求身份，返回权限判定需要的三样东西。
//
// 优先级：Basic / 查询串 > 会话 cookie。带显式凭据时以凭据为准 ——
// 那是最新表达出来的意图（比如换个账号登录），cookie 只是「上次登录过」。
// 注意两者可能属于**不同**的用户，所以 cookieUser 单独返回，
// 绝不与 user 混用。
func (s *Server) credsOf(r *http.Request) (user, pass string, hasCred bool, cookieUser string) {
	user, pass, hasCred = credentials(r)
	if hasCred {
		return user, pass, true, ""
	}
	if token := sessionToken(r); token != "" {
		if u, ok := s.auth.ResolveSession(token); ok {
			return "", "", false, u
		}
	}
	return "", "", false, ""
}

// permOf 按请求身份计算对 urlPath 的权限。
func (s *Server) permOf(r *http.Request, urlPath string) (perm auth.Permission, user string, authenticated bool) {
	user, pass, hasCred, cookieUser := s.credsOf(r)
	switch {
	case hasCred:
		// 显式凭据：走原来的路径，密码要现验。
		return s.auth.Lookup(urlPath, user, pass, true), user, true
	case cookieUser != "":
		// 会话身份：密码在登录那一刻已经验过，这里只按身份取权限。
		return s.auth.PermFor(cookieUser, urlPath), cookieUser, true
	}
	// 匿名。
	return s.auth.Lookup(urlPath, "", "", false), "", false
}

// checkPerm 计算对 urlPath 的权限，不写任何响应。
// 需要在不产生副作用的前提下预判权限时使用（例如上传路径被重写之后）。
func (s *Server) checkPerm(r *http.Request, urlPath string) auth.Permission {
	perm, _, _ := s.permOf(r, urlPath)
	return perm
}

// authorize 解析请求凭据并计算对 urlPath 的权限。
// 需要质询时直接写响应并返回 false。
func (s *Server) authorize(w http.ResponseWriter, r *http.Request, urlPath string) (permResult, bool) {
	if !s.auth.Enabled() {
		return permResult{Perm: auth.PermReadWrite}, true
	}

	// 认证限速：Basic 认证可以被无限次尝试，不设闸门就等于把口令交给
	// 字典攻击。这里按来源统计失败次数，超限后临时封禁。
	ip := clientIP(r)
	if blocked, remain := s.authBlocked(ip); blocked {
		w.Header().Set("Retry-After", fmt.Sprintf("%d", int(remain.Seconds())+1))
		http.Error(w, "429 Too Many Requests: 认证失败次数过多，请稍后再试", http.StatusTooManyRequests)
		return permResult{}, false
	}

	// hadCookie 单独记一下：credsOf 解析失败时 cookieUser 是空的，
	// 分不清「没带 cookie」和「带了但无效」—— 而后者要按失败计数。
	hadCookie := sessionToken(r) != ""

	user, pass, hasCred, cookieUser := s.credsOf(r)
	var perm auth.Permission
	switch {
	case hasCred:
		perm = s.auth.Lookup(urlPath, user, pass, true)
	case cookieUser != "":
		// 会话身份：cookie 在登录时已经验过密码，这里只按身份取权限。
		user = cookieUser
		perm = s.auth.PermFor(cookieUser, urlPath)
	default:
		perm = s.auth.Lookup(urlPath, "", "", false)
	}
	if perm != auth.PermNone {
		s.noteAuthSuccess(ip)
		// 会话身份也算「已认证」—— 否则登录后每个请求都会被当成匿名。
		return permResult{Perm: perm, User: user, Authenticated: hasCred || cookieUser != ""}, true
	}

	// 只有「带了凭据但不对」才算一次失败；完全没带凭据只是未登录，
	// 否则一个不带凭据的爬虫就能把正常用户的来源封掉。
	//
	// 无效 cookie（过期/已撤销/伪造）同样算失败：cookie 值完全由客户端提供，
	// 伪造一个来试探是可行的，所以它和错密码一样该计数。
	if hasCred || hadCookie {
		s.noteAuthFailure(ip)
	}

	if s.auth.NeedsAuth(urlPath) {
		// cookie 无效就顺手清掉，免得浏览器每个后续请求都带着一张废票
		// 反复撞限速闸门。
		if hadCookie && cookieUser == "" {
			s.clearSessionCookie(w)
		}
		// 前端的 fetch 会带 X-Gofs-Ajax 标记，此时只回 401 不带挑战，
		// 让应用自己的登录框处理，而不是弹出浏览器原生对话框。
		if shouldChallenge(r) {
			w.Header().Set("WWW-Authenticate", `Basic realm="gofs", charset="UTF-8"`)
		}
		http.Error(w, "401 Unauthorized", http.StatusUnauthorized)
		return permResult{}, false
	}
	http.Error(w, "403 Forbidden", http.StatusForbidden)
	return permResult{}, false
}

// requireWrite 在只读授权时拒绝写操作。
func (s *Server) requireWrite(w http.ResponseWriter, p permResult) bool {
	if p.Perm == auth.PermReadWrite {
		return true
	}
	http.Error(w, "403 Forbidden: 只读权限", http.StatusForbidden)
	return false
}

// ---------------------------------------------------------------------------
// 路由分发
// ---------------------------------------------------------------------------

// shouldChallenge 判断 401 响应是否附带 WWW-Authenticate 挑战头。
//
// 浏览器看到挑战头会弹出原生登录框。对于地址栏导航和直接下载文件，
// 这是期望行为；但对于应用内部的 fetch，弹原生框会打断应用自己的
// 登录流程（且那个框无法承载跳转、无法登出），因此前端请求会带上
// X-Gofs-Ajax 标记并要求不要挑战。
func shouldChallenge(r *http.Request) bool {
	return r.Header.Get("X-Gofs-Ajax") != "1"
}

// isAppShellNavigation 判断请求是否属于「浏览器地址栏打开应用页面」。
//
// 只认目录形态的路径（/ 或 /dir/）：这类请求应当拿到应用外壳，
// 数据由前端登录后自行补齐；而 /a.txt 这类直接指向文件的请求
// 仍走 401 + 挑战，让浏览器弹出原生登录框——那时原生框反而是合适的。
func isAppShellNavigation(r *http.Request) bool {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		return false
	}
	if !strings.Contains(r.Header.Get("Accept"), "text/html") {
		return false
	}
	q := r.URL.Query()
	for _, k := range []string{"json", "simple", "zip", "hash", "q"} {
		if q.Has(k) {
			return false
		}
	}
	p := r.URL.Path
	return p == "/" || strings.HasSuffix(p, "/")
}

// handleRoot 处理除内部 API 之外的所有请求。
func (s *Server) handleRoot(w http.ResponseWriter, r *http.Request) {
	urlPath := r.URL.Path

	// 上传密钥走独立分支：它只代表「往某个目录传文件」这一件事，
	// 不参与读、删、改名、解压等任何其它操作。
	if token := uploadKeyToken(r); token != "" {
		s.handleWithUploadKey(w, r, urlPath, token)
		return
	}

	// 页面导航未认证时返回应用外壳（200），由前端弹出登录框。
	// 直接回 401 + 挑战会触发浏览器原生对话框；而「先跳登录页再回来」
	// 又会因为浏览器不会自动带 Basic 凭据而陷入拿不到数据的死循环。
	// 返回外壳 + 前端登录后自行 fetch 数据，可以同时避开这两个问题。
	if s.auth.Enabled() && isAppShellNavigation(r) && s.checkPerm(r, urlPath) == auth.PermNone {
		s.renderAuthGate(w, r, urlPath)
		return
	}

	p, ok := s.authorize(w, r, urlPath)
	if !ok {
		return
	}

	// 单文件模式下只允许读。
	if single := s.res.SingleFile(); single != "" {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			http.Error(w, "405 Method Not Allowed", http.StatusMethodNotAllowed)
			return
		}
		s.serveFile(w, r, urlPath)
		return
	}

	abs, err := s.res.Resolve(urlPath)
	if err != nil {
		http.Error(w, "403 Forbidden", http.StatusForbidden)
		return
	}

	// 打包下载：GET 从 ?pick= 取选中项，POST 从请求体取。
	// 走 POST 是为了让「选中一大批文件再打包」不受 URL 长度限制，
	// 因此要在下面把 POST 派发给上传处理之前拦下来。
	if r.Method == http.MethodPost && r.URL.Query().Has("zip") {
		s.handleZipRequest(w, r, urlPath, abs)
		return
	}

	switch r.Method {
	case http.MethodGet, http.MethodHead:
		s.handleGet(w, r, urlPath, abs, p)
	case http.MethodPut:
		s.handlePut(w, r, urlPath, p)
	case http.MethodPost:
		s.handlePost(w, r, urlPath, p)
	case http.MethodDelete:
		s.handleDelete(w, r, urlPath, abs, p)
	case MethodMkcol:
		s.handleMkcol(w, r, urlPath, abs, p)
	case MethodMove:
		s.handleMove(w, r, urlPath, p)
	default:
		w.Header().Set("Allow", "GET, HEAD, PUT, POST, DELETE, MKCOL, MOVE, OPTIONS")
		http.Error(w, "405 Method Not Allowed", http.StatusMethodNotAllowed)
	}
}

// handleGet 处理读取类请求：目录列表、搜索、打包、文件下载。
func (s *Server) handleGet(w http.ResponseWriter, r *http.Request, urlPath, abs string, p permResult) {
	info, err := os.Stat(abs)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			if s.cfg.RenderSPA {
				s.serveIndexFallback(w, r)
				return
			}
			http.Error(w, "404 Not Found", http.StatusNotFound)
			return
		}
		s.writeErr(w, err)
		return
	}

	if !info.IsDir() {
		s.serveFile(w, r, urlPath)
		return
	}

	q := r.URL.Query()

	// 压缩包预览与解压入口统一走 /__gofs__/extract，这里只做提示。
	if q.Has("extract") {
		http.Redirect(w, r, s.cfg.PathPrefix+"/__gofs__/extract?path="+url.QueryEscape(urlPath), http.StatusTemporaryRedirect)
		return
	}

	// 目录打包下载。带 ?pick= 时只打包选中的条目，否则打包整个目录。
	if q.Has("zip") {
		if !s.cfg.AllowArchive {
			http.Error(w, "403 Forbidden: 未开启目录打包（--allow-archive）", http.StatusForbidden)
			return
		}
		s.serveZip(w, r, abs, urlPath, picksFromQuery(q))
		return
	}

	// 搜索。
	if pattern := q.Get("q"); pattern != "" {
		if !s.cfg.AllowSearch {
			http.Error(w, "403 Forbidden: 未开启搜索（--allow-search）", http.StatusForbidden)
			return
		}
		s.serveSearch(w, r, abs, urlPath, pattern)
		return
	}

	// 渲染 index.html。
	if s.cfg.RenderIndex || s.cfg.RenderTryIndex {
		if s.tryServeIndex(w, r, abs) {
			return
		}
		if s.cfg.RenderIndex {
			http.Error(w, "404 Not Found", http.StatusNotFound)
			return
		}
	}

	listing, err := fsutil.ReadDir(s.res, abs, s.cfg.Hidden, s.cfg.ListMaxEntries)
	if err != nil {
		s.writeErr(w, err)
		return
	}
	listing.Path = fsutil.CleanURLPath(urlPath)

	// 明确要求 JSON 时直接返回数据。
	if q.Has("json") || wantsJSON(r) {
		s.writeJSON(w, listing)
		return
	}
	// 只要名字列表。
	if q.Has("simple") {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		for _, e := range listing.Entries {
			name := e.Name
			if e.IsDir {
				name += "/"
			}
			fmt.Fprintln(w, name)
		}
		return
	}

	s.renderIndex(w, r, listing, p)
}

// serveFile 输出文件内容，支持 Range 与 Sha256 查询。
func (s *Server) serveFile(w http.ResponseWriter, r *http.Request, urlPath string) {
	abs, err := s.res.Resolve(urlPath)
	if err != nil {
		http.Error(w, "403 Forbidden", http.StatusForbidden)
		return
	}
	f, err := os.Open(abs)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			if s.cfg.RenderSPA {
				s.serveIndexFallback(w, r)
				return
			}
			http.Error(w, "404 Not Found", http.StatusNotFound)
			return
		}
		s.writeErr(w, err)
		return
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		s.writeErr(w, err)
		return
	}
	if info.IsDir() {
		http.Redirect(w, r, urlPath+"/", http.StatusMovedPermanently)
		return
	}

	// ?hash 返回 sha256。
	// 只对中等大小的文件开放：摘要要读完整份内容，让一个请求去算
	// 几十 GB 文件的哈希，等于送出一个免费的 CPU/IO 耗尽入口。
	if r.URL.Query().Has("hash") {
		if s.cfg.HashMaxSize > 0 && info.Size() > s.cfg.HashMaxSize {
			http.Error(w, fmt.Sprintf(
				"413 Payload Too Large: 文件超过计算摘要的大小上限 %s",
				humanSize(s.cfg.HashMaxSize)), http.StatusRequestEntityTooLarge)
			return
		}
		sum, err := fileSHA256(abs)
		if err != nil {
			s.writeErr(w, err)
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		fmt.Fprint(w, sum)
		return
	}

	// 内嵌资源优先于同名磁盘文件？不，磁盘文件优先，符合直觉。
	if etag := fmt.Sprintf(`"%x-%x"`, info.ModTime().UnixNano(), info.Size()); etag != "" {
		w.Header().Set("ETag", etag)
	}
	// 给下载也设一个「无进展超时」：没有它，一个「连上就不读数据」的客户端
	// 会一直占着并发名额（服务端阻塞在写 socket 上），几十个就能拖垮服务。
	// 用进展而不是总时长，是为了不误杀慢速的大文件传输。
	w = withWriteProgress(w, s.cfg.DownloadTimeout)
	s.guardInlineContent(w, r, info.Name())
	http.ServeContent(w, r, info.Name(), info.ModTime(), f)
}

// guardInlineContent 抑制「上传的文件在同源下执行脚本」带来的风险。
//
// 文件服务器天然会把用户上传的 .html / .svg 原样吐回浏览器，而浏览器
// 会把它当作**本站页面**渲染。于是一个能上传文件的人只要放一个
// <script>fetch('/__gofs__/keys',{method:'POST'...})</script> 的页面，
// 再诱导管理员打开，就能借管理员已登录的会话为所欲为。这是典型的存储型 XSS。
//
// 两道处理：
//   - nosniff：禁止浏览器按内容猜测类型（否则一个无扩展名文件也能被当 HTML 执行）；
//   - 对 HTML / SVG / XML 加 CSP sandbox：页面仍可正常预览渲染，
//     但里面的脚本无法读取本站数据、也无法发起同源请求。
//
// 需要连脚本一起预览时用 --no-html-sandbox 关掉第二道。
func (s *Server) guardInlineContent(w http.ResponseWriter, r *http.Request, name string) {
	h := w.Header()
	h.Set("X-Content-Type-Options", "nosniff")

	if s.cfg.DisableHTMLSandbox {
		return
	}
	if !isScriptableContent(name) {
		return
	}
	// sandbox 不带 allow-same-origin：文档被放进一个不透明源，
	// 同源请求与本地存储全部失效，而渲染与样式不受影响。
	h.Set("Content-Security-Policy", "sandbox")
}

// scriptableExts 是浏览器会当作可执行文档渲染的扩展名。
var scriptableExts = map[string]bool{
	".html": true, ".htm": true, ".xhtml": true, ".svg": true,
	".xml": true, ".xsl": true, ".mhtml": true,
}

// isScriptableContent 判断该文件名是否属于「浏览器会当文档执行」的类型。
func isScriptableContent(name string) bool {
	return scriptableExts[strings.ToLower(filepath.Ext(name))]
}

// fileSHA256 计算文件摘要。
func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// ---------------------------------------------------------------------------
// 工具
// ---------------------------------------------------------------------------

// wantsJSON 判断客户端是否期望 JSON。
func wantsJSON(r *http.Request) bool {
	accept := r.Header.Get("Accept")
	if strings.Contains(accept, "application/json") {
		return true
	}
	return false
}

// writeJSON 输出 JSON 响应。
func (s *Server) writeJSON(w http.ResponseWriter, v any) {
	s.writeJSONStatus(w, http.StatusOK, v)
}

// writeJSONStatus 以指定状态码输出 JSON 响应。
func (s *Server) writeJSONStatus(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	if code != http.StatusOK {
		w.WriteHeader(code)
	}
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		s.logger.Errorf("写出 JSON 失败: %v", err)
	}
}

// writeErr 把错误映射为合适的 HTTP 状态码。
func (s *Server) writeErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, fs.ErrNotExist):
		http.Error(w, "404 Not Found", http.StatusNotFound)
	case errors.Is(err, fs.ErrPermission):
		http.Error(w, "403 Forbidden", http.StatusForbidden)
	default:
		s.logger.Errorf("请求处理失败: %v", err)
		http.Error(w, "500 "+err.Error(), http.StatusInternalServerError)
	}
}

// handleHealth 健康检查。
func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	s.writeJSON(w, map[string]any{
		"status":  "ok",
		"version": config.Version,
		"time":    time.Now().Format(time.RFC3339),
	})
}

// redirectTo 302 跳转。
func redirectTo(w http.ResponseWriter, r *http.Request, to string) {
	http.Redirect(w, r, to, http.StatusSeeOther)
}

// quotePath 对路径做 URL 编码，保留分隔符。
func quotePath(p string) string {
	segs := strings.Split(p, "/")
	for i, s := range segs {
		segs[i] = url.PathEscape(s)
	}
	return strings.Join(segs, "/")
}
