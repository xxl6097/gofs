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
	"mime"
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"github.com/xxl6097/gofs/internal/auth"
	"github.com/xxl6097/gofs/internal/config"
	"github.com/xxl6097/gofs/internal/fsutil"
	"github.com/xxl6097/gofs/internal/uploadkey"
)

// Server 持有全部运行时依赖。
type Server struct {
	cfg *config.Config
	// res 是**唯一**的路径解析器，运行期可增删站点 —— 所以它必须是同一个
	// 指针、永不替换（替换一个被无锁读的接口值就是竞态）。主目录服务在 /，
	// 附加站点挂在各自标签下；单根就是「没有附加站点」。
	res *fsutil.Roots
	// sitesMu 串行化增删站点（读路径完全不碰它）。
	sitesMu sync.Mutex

	// upgradeGate / uninstallGate 是「同一时刻只允许一次」的闸门
	// （实现在 runlock.go）。挂在实例上而不是包级 map：不会随实例数增长，
	// 也不必拿 *Server 当键。
	upgradeGate   runGate
	uninstallGate runGate
	auth          *auth.Authenticator
	keys          *uploadkey.Store
	guard         *guard
	// settings 存放可以在运行期修改、且会被并发读取的设置。
	settings *runtimeSettings
	assets   fs.FS
	ui       *uiTemplate
	logger   *Logger
	// trustedProxies 是允许提供转发头的反向代理网段；空表示不信任。
	trustedProxies []*net.IPNet

	// assetTags 缓存内嵌资源的 ETag（按内容算，只算一次）。
	// 为什么不能只用 modTime+size：见 handler_list.go 的 assetETag。
	assetTags sync.Map
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
	roots, err := fsutil.BuildRoots(cfg.ServePaths, cfg.AllowSymlink)
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

	trustedProxies := parseTrustedProxies(cfg.TrustedProxies)

	logger, err := NewLogger(cfg.LogFormat, cfg.LogFile, trustedProxies)
	if err != nil {
		return nil, err
	}
	writable := cfg.AllowUpload || cfg.AllowEdit || cfg.AllowExtract
	if writable && (cfg.RenderIndex || cfg.RenderTryIndex || cfg.RenderSPA) {
		if cfg.UnsafeIndexHTML {
			logger.Errorf("安全提示：--unsafe-index-html 已开启。" +
				"可写部署中的 index.html 会作为同源页面执行，" +
				"拥有写权限的账号可借此读取登录凭据并调用全部接口")
		} else {
			logger.Infof("安全提示：可写部署中的 index.html 已启用 CSP sandbox" +
				"（脚本可运行但不透明源）；需要同源应用请显式 --unsafe-index-html")
		}
	}

	// 上传密钥存储：路径为空时只保存在内存中。
	keys, err := uploadkey.New(cfg.KeyFile)
	if err != nil {
		_ = logger.Close()
		return nil, err
	}

	srv := &Server{
		cfg:            cfg,
		res:            roots,
		auth:           authn,
		keys:           keys,
		guard:          newGuard(cfg),
		settings:       newRuntimeSettings(cfg, roots.Root()),
		assets:         assetsFS,
		ui:             ui,
		logger:         logger,
		trustedProxies: trustedProxies,
	}
	// 恢复上次落盘的附加站点。**尽力而为**：单条坏数据只记日志、不阻止启动
	// （与读取标题同一口径）—— 一个坏掉的设置文件不该让整个服务起不来。
	srv.restoreSites()
	return srv, nil
}

// restoreSites 把设置文件里记着的附加站点重新挂上。
func (s *Server) restoreSites() {
	for _, rec := range s.settings.Sites() {
		abs, err := fsutil.NormalizePath(rec.Path)
		if err != nil {
			s.logger.Errorf("恢复站点 %q 失败: %v", rec.Path, err)
			continue
		}
		// 边界启动时也要再算一遍：设置文件可能被手工改过，也可能是靠
		// 上次更宽的白名单加进来的。用的是**站点**的那套判定，别跟
		// 「换根」的默认范围混了。
		if !s.settings.AllowsSite(abs) {
			s.logger.Errorf("站点 %q 不在允许范围内，已跳过（可用 --root-allow 放开）", rec.Path)
			continue
		}
		m, err := s.res.AddSite(abs, rec.Label)
		if err != nil {
			s.logger.Errorf("恢复站点 %q 失败: %v", rec.Path, err)
			continue
		}
		s.warnSiteACLOverlap(m.Label)
		s.logger.Infof("已恢复站点：/%s → %s", m.Label, m.Res.Root())
	}
}

// Handler 返回配置好中间件的 http.Handler。
// 中间件由外到内的顺序：
// 路径前缀 -> 分享路径改写 -> 并发闸门 -> 访问日志 -> CORS -> 跨站校验
// -> panic 恢复 -> 路由。
//
// 并发闸门放在最外层，是为了在真正开始干活之前就把多余请求挡掉；
// 它拒绝的请求不写访问日志 —— 服务过载时再拼命写日志只会雪上加霜。
//
// ⚠️ 分享路径改写必须排在**路径前缀之内**。它匹配的是 `/s/<token>/…` 这个
// 字面前缀，而带前缀部署时请求上来的是 `/files/s/<token>/…` —— 排在前面就
// 匹配不上（早先正是这么排的，于是前缀部署下路径形式的分享链接一律 404）。
// 放在里面还有个好处：改写之后，日志、CORS、跨站校验看到的仍是干净路径。
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/fs/health", s.handleHealth)
	mux.HandleFunc("/fs/auth", s.handleAuth)
	mux.HandleFunc("/fs/keys", s.handleKeys)
	// 一键上传脚本：用上传密钥换一段可执行的脚本（见 up_script.go）。
	// 走密钥而非账号密码，所以放在 keys 之外单独注册。
	mux.HandleFunc(upScriptName, s.handleUpScript)
	mux.HandleFunc("/fs/settings", s.handleSettings)
	mux.HandleFunc("/fs/upgrade", s.handleUpgrade)
	mux.HandleFunc("/fs/restart", s.handleRestart)
	// 卸载是**独立**的扩展点（WithUninstaller），与 Restarter 无关。
	mux.HandleFunc("/fs/uninstall", s.handleUninstall)
	mux.HandleFunc("/fs/users", s.handleUsers)
	// 批量重命名：整批校验通过才动手（见 handler_rename.go）。
	mux.HandleFunc("/fs/rename-batch", s.handleRenameBatch)
	mux.HandleFunc("/fs/extract", s.handleExtract)
	mux.HandleFunc("/fs/text", s.handleText)
	mux.HandleFunc("/fs/office", s.handleOffice)
	mux.HandleFunc("/fs/assets/", s.handleAsset)
	mux.HandleFunc("/", s.handleRoot)

	var h http.Handler = mux
	h = s.withRecover(h)
	h = s.withCSRFProtect(h)
	h = s.withCORS(h)
	h = s.withLogging(h)
	h = s.withConcurrency(h)
	// 先按前缀剥离（/files/s/<token>/… -> /s/<token>/…），再改写分享路径。
	h = s.withSharePath(h)
	h = s.withPathPrefix(h)
	// 路径规范化缓存放在路由之前：有些路径会被同一个请求检查多次。
	h = s.withCanonicalCache(h)
	// 安全响应头放在最外层，连前缀不匹配的 404 也带上。
	h = s.withSecurityHeaders(h)
	return h
}

// withSecurityHeaders 注入一组与具体业务无关的安全响应头。
//
// 不引入会破坏前端脚本的严格 CSP；HTML/SVG/XML 的 sandbox 由
// guardInlineContent 单独追加。X-Frame-Options 用 SAMEORIGIN 而不是 DENY，
// 因为前端的 PDF 预览依赖同源 iframe。
func (s *Server) withSecurityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		// SAMEORIGIN 而不是 DENY：前端的 PDF 预览依赖同源 <iframe>。
		// 跨站嵌入仍然被挡住，足以防点击劫持。
		h.Set("X-Frame-Options", "SAMEORIGIN")
		h.Set("Referrer-Policy", "no-referrer")
		next.ServeHTTP(w, r)
	})
}

// Root 返回被服务的根目录绝对路径。
//
// 多根时返回**第一个**根：多根没有「唯一根」这回事，这里的用途是展示与
// 拼接（banner、SPA 回退）。按请求选根的是 Resolve，不是这个。
func (s *Server) Root() string { return s.res.Root() }

// Mounts 返回全部挂载点（[0] 是主目录），供 banner 与设置界面展示。
func (s *Server) Mounts() []fsutil.Mount { return s.res.Mounts() }

// MountLabels 返回**附加站点**的标签（主目录服务在 /，没有前缀）。
func (s *Server) MountLabels() []string { return s.res.Labels() }

// MultiRoot 表示除主目录外还有附加站点。
func (s *Server) MultiRoot() bool { return s.res.HasExtras() }

// PathPrefix 返回当前生效的访问路径前缀（"" 表示服务在根）。
//
// 它可能被页面上的设置改掉，所以任何要拼链接、写 cookie Path、或做路由
// 判断的地方都必须**现取**，不能启动时读一次 cfg.PathPrefix 存起来。
func (s *Server) PathPrefix() string { return s.settings.PathPrefix() }

// ListenBind / ListenPort 返回当前生效的监听地址与端口。
//
// 权威值在 runtimeSettings 里（可落盘、可在页面上改）。根包建监听时来问这里，
// 不自己另存一份 —— 两个来源迟早会对不上。
func (s *Server) ListenBind() string { return s.settings.ListenBind() }

// ListenPort 见 ListenBind。
func (s *Server) ListenPort() int { return s.settings.ListenPort() }

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
			// ⚠️ 查询串里的 user 不能直接写进日志：它由客户端提供、没有经过
			// 任何校验，任何匿名请求都能用 `?user=admin` 伪造审计身份。
			// 需要查询串凭据的客户端应改用 Authorization 头或会话。
			//
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
//
// 前缀是**运行期可改**的（设置 → 常规），所以不能像早先那样在构造中间件时
// 把它捕获进闭包 —— 那样改过之后路由仍按旧前缀走。也不再有「空前缀直接
// 透传」的短路：前缀可以随时从空变成非空，透传的中间件就再也装不上剥离逻辑了。
// 代价是每请求多一次 RLock，可忽略。
func (s *Server) withPathPrefix(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		prefix := s.PathPrefix()
		if prefix == "" {
			next.ServeHTTP(w, r)
			return
		}
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
	// Share 表示本次请求由分享令牌授权。
	//
	// 单独记一笔是因为**权限级别不足以描述它**：可读写分享在范围内的
	// Perm 也是 PermReadWrite，与「对根有读写权限的账号」一模一样。
	// 不看这个标志，那些「按 rw 判断是不是管理员」的地方就会把
	// 一个拿到链接的陌生人当成管理员放行。
	Share bool
}

// sharePerm 把分享链接映射成它在范围内授予的权限级别。
//
// 独立的函数而不是内联在 permOf/authorize 里：这两处必须永远一致，
// 一处改了另一处漏掉就会「页面按只读渲染、写请求却放行」。
func sharePerm(k *uploadkey.Key) auth.Permission {
	if k.ReadWrite() {
		return auth.PermReadWrite
	}
	return auth.PermRead
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

// shareCookieName 是分享链接的 cookie 名字。
//
// 为什么分享要下发一个 cookie：目录分享打开的是**页面**，而页面里的目录列表、
// 预览这些都是前端自己再发 XHR 拉的 —— 那些请求不会带最初 URL 上的 `?share=`，
// 于是全部 401，用户看到的是登录框而不是目录。
// 下发一个 Path 限定在分享范围内的 cookie，后续请求就会自动带上。
//
// 它和会话 cookie 是两回事：这条只代表「某个分享链接被打开过」，
// 权限级别照抄令牌（只读或可读写），且严格限制在链接自身的范围内。
const shareCookieName = "gofs_share"

// shareTokenOf 取出请求里的分享令牌：查询参数优先，其次是 cookie。
//
// 查询参数优先是有意的：同一个浏览器可能开过多个分享链接，
// 显式带上来的那一个才是当前意图。
func shareTokenOf(r *http.Request) string {
	// 路径形式（/s/<token>/…）：令牌在 context 里，伪造不了。
	if v, ok := r.Context().Value(shareCtxKey{}).(string); ok && v != "" {
		return v
	}
	if v := r.URL.Query().Get("share"); v != "" {
		return v
	}
	if c, err := r.Cookie(shareCookieName); err == nil {
		return c.Value
	}
	return ""
}

// setShareCookie 把分享令牌记到 cookie 上，Path 限定在分享范围内。
//
// Path 限定是关键：这条 cookie 只该在分享到的那棵子树里被带上，
// 不该跟着用户跑到站点的别处去。
//
// ⚠️ scope 是**文件路径**，而 cookie 的 Path 比的是**请求 URL**。带前缀
// 部署时 URL 是 /files/docs/…，只写 /docs/ 的话浏览器根本不会带上这条
// cookie，分享页面里的 XHR 就全 401（页面能打开、列表却刷不出来）。
// 所以要先把访问前缀拼进去。
func (s *Server) setShareCookie(w http.ResponseWriter, r *http.Request, token, scope string, expires time.Time) {
	if token == "" {
		return
	}
	// 默认 12 小时：目录分享要能在页面里正常翻页，太短会在浏览中途失效。
	maxAge := int((12 * time.Hour).Seconds())
	if !expires.IsZero() {
		left := int(time.Until(expires).Seconds())
		if left <= 0 {
			return // 已经过期，不必下发
		}
		if left < maxAge {
			maxAge = left
		}
	}
	p := s.PathPrefix() + strings.TrimSuffix(scope, "/") + "/"
	if strings.HasPrefix(p, "//") {
		p = "/"
	}
	http.SetCookie(w, &http.Cookie{
		Name:     shareCookieName,
		Value:    token,
		Path:     p,
		MaxAge:   maxAge,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   s.cookieSecure(r),
	})
}

// cookieSecure 判断 cookie 是否应带 Secure。
//
// 直接跑 HTTPS，或请求来自可信代理且代理声明外部协议是 HTTPS 时都要加；
// 只判断本地是否配置证书会在 TLS 终止于反向代理的常见部署里漏掉 Secure。
func (s *Server) cookieSecure(r *http.Request) bool {
	return s.cfg.TLSCert != "" || s.externalScheme(r) == "https"
}

// sessionCookiePath 返回 cookie 的作用路径。
//
// 必须带上访问前缀：部署在 /files 下时，cookie 若只写 Path=/，
// 浏览器确实会发，但反过来若配了前缀却不写进去，前缀下的请求就收不到它。
// 带前缀是两种部署都成立的那个写法。
func (s *Server) sessionCookiePath() string {
	return cookiePathForPrefix(s.PathPrefix())
}

// cookiePathForPrefix 把访问前缀换算成 cookie 的 Path。
//
// 单独抽出来是为了能在前缀变更时算**旧值的** Path —— 那条 cookie 得显式
// 清掉，否则它会在浏览器的 cookie 罐里一直躺到过期为止。
func cookiePathForPrefix(prefix string) string {
	if prefix == "" {
		return "/"
	}
	return prefix + "/"
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
func (s *Server) setSessionCookie(w http.ResponseWriter, r *http.Request, token string) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    token,
		Path:     s.sessionCookiePath(),
		MaxAge:   int(auth.SessionTTL().Seconds()),
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   s.cookieSecure(r),
	})
}

// clearSessionCookie 让浏览器丢掉会话 cookie（值留空 + MaxAge<0）。
func (s *Server) clearSessionCookie(w http.ResponseWriter, r *http.Request) {
	s.clearSessionCookieAt(w, r, s.sessionCookiePath())
}

// clearSessionCookieAt 按指定 Path 清掉会话 cookie。
//
// 前缀变更时要清的是**旧 Path** 的那条：浏览器的 cookie 是按 (name, domain,
// path) 三元组存的，新 Path 上那条清不掉旧 Path 上那条。
func (s *Server) clearSessionCookieAt(w http.ResponseWriter, r *http.Request, path string) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    "",
		Path:     path,
		MaxAge:   -1,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   s.cookieSecure(r),
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
	// 与 authorize 使用同一套规范化：否则页面数据里的 perms 会与实际
	// 文件操作看到的路径不一致。
	return s.permOfExact(r, s.canonicalPath(r, urlPath))
}

// permOfExact 与 permOf 相同，但假定调用方给出的已经是磁盘上的真实名字。
//
// 供遍历目录树的场景使用：WalkDir 返回的条目名本来就是磁盘真实名字，
// 再对每个后代做一次「逐段 ReadDir 找真实大小写」会把递归删除/移动
// 放大成 O(n²)。
func (s *Server) permOfExact(r *http.Request, urlPath string) (perm auth.Permission, user string, authenticated bool) {
	// 分享令牌：换来的只是「不用登录」，权限级别由令牌自己定（只读 / 可读写），
	// 且严格限制在令牌范围内。
	//
	// ⚠️ 这一支必须放在这里（而不是只放在 authorize 里）：
	// checkPerm 走的就是 permOf，而它决定了**页面数据**里的 perms 与
	// auth_required。只在 authorize 里认令牌的话，分享访客拿到的页面是
	// 「需要登录、权限全无」的空壳 —— 前端会弹登录框，正好是分享要避免的事。
	if tok := shareTokenOf(r); tok != "" {
		if k, err := s.keys.Verify(tok); err == nil &&
			k.IsShare() && k.AllowsPath(urlPath) {
			return sharePerm(k), "share:" + k.Prefix, true
		}
	}

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

// checkPermExact 用于路径已由 WalkDir 等来源保证是磁盘真实名字的场景。
func (s *Server) checkPermExact(r *http.Request, urlPath string) auth.Permission {
	perm, _, _ := s.permOfExact(r, urlPath)
	return perm
}

// clientIP 返回经可信代理校正后的客户端地址。
func (s *Server) clientIP(r *http.Request) string { return clientIP(r, s.trustedProxies) }

// checkAuthLimit 在验证凭据前检查来源是否已被封禁。
//
// 所有会验证密码的入口都必须经过它：只在 authorize / handleAuth 里检查
// 是不够的 —— /fs/settings、/fs/users、/fs/keys、/fs/upgrade 等接口同样
// 能验证口令，漏掉任何一个都等于给字典攻击留了无限次入口。
func (s *Server) checkAuthLimit(w http.ResponseWriter, r *http.Request, user string) (string, bool) {
	ip := s.clientIP(r)
	if blocked, remain := s.authBlocked(ip, user); blocked {
		w.Header().Set("Retry-After", fmt.Sprintf("%d", int(remain.Seconds())+1))
		http.Error(w, "429 Too Many Requests: 认证失败次数过多，请稍后再试", http.StatusTooManyRequests)
		return ip, false
	}
	return ip, true
}

// authorize 解析请求凭据并计算对 urlPath 的权限。
// 需要质询时直接写响应并返回 false。
func (s *Server) authorize(w http.ResponseWriter, r *http.Request, urlPath string) (permResult, bool) {
	if !s.auth.Enabled() {
		return permResult{Perm: auth.PermReadWrite}, true
	}
	// ACL 必须按磁盘上的真实名字匹配（大小写/规范化/短名）。
	urlPath = s.canonicalPath(r, urlPath)

	// 认证限速：Basic 认证可以被无限次尝试，不设闸门就等于把口令交给
	// 字典攻击。计数按「来源 + 尝试的账号」隔离：只按来源统计的话，
	// 持有任意合法账号的人可以穿插自己的成功请求把别人的失败计数清零。
	limitUser, _, _ := credentials(r)
	ip, ok := s.checkAuthLimit(w, r, limitUser)
	if !ok {
		return permResult{}, false
	}

	// 分享链接：带 `?share=<token>`（或那条 cookie）的请求，按令牌授予权限。
	//
	// 它换来的只是「不用登录」，不是「能读任何东西」—— 令牌自身限定了范围，
	// 超出范围的请求会退回去走正常鉴权（于是拿到 401，而不是悄悄放行）。
	//
	// 权限级别由令牌决定：只读令牌写操作会被 requireWrite 挡下；可读写令牌
	// 则在范围内与一个读写账号等价 —— 但**不等于管理员**，见 isAdminRequest。
	//
	// 写到权限判定之前，是因为这就是一次正常的鉴权判定，
	// 只是凭据从「账号密码」换成了「分享令牌」。
	if tok := shareTokenOf(r); tok != "" {
		if k, err := s.keys.Verify(tok); err == nil &&
			k.IsShare() && k.AllowsPath(urlPath) {
			// 首次拿到令牌的那一次就下发 cookie，之后靠它认人。
			//
			// ⚠️ 判据不能只看 `?share=`。分享地址有**两种形式**，而「分享」
			// 按钮生成出来的偏偏是没有查询串的那种（/s/<token>/…，为了 wget
			// 递归下来文件名干净）。只认查询串的话，路径形式打开的页面拿不到
			// cookie —— 页面本身照样能渲染（初始数据是服务端注入的），但页面里
			// 随后的每一个 XHR（列子目录、预览、上传、删除）都会 401，
			// 前端收到 401 就弹登录框：表现为「点任何一个操作都跳登录页」。
			//
			// 所以判据改成「这次用的令牌不是从 cookie 来的」：两种形式都覆盖，
			// 而已经在用 cookie 的请求不再重设，避免每个 XHR 都带一次 Set-Cookie。
			if c, err := r.Cookie(shareCookieName); err != nil || c.Value != tok {
				s.setShareCookie(w, r, tok, k.Scope, k.ExpiresAt)
			}
			return permResult{
				Perm:          sharePerm(k),
				User:          "share:" + k.Prefix,
				Authenticated: true,
				Share:         true,
			}, true
		}
	}

	// hadCookie 单独记一下：credsOf 解析失败时 cookieUser 是空的，
	// 分不清「没带 cookie」和「带了但无效」—— 而后者要按失败计数。
	hadCookie := sessionToken(r) != ""

	user, pass, hasCred, cookieUser := s.credsOf(r)
	var perm auth.Permission
	switch {
	case hasCred:
		perm = s.auth.Lookup(urlPath, user, pass, true)
		// 失败计数**只在这里**清零：真的用密码验过一次才算。
		//
		// ⚠️ 不能放在下面统一的 `perm != PermNone` 分支里。那样任何一次
		// 「匿名就能读」的请求都会把计数抹掉 —— 部署里有 `-a @/:r` 这类
		// 公开只读规则（很常见）时，攻击者只要在两次猜密码之间插一个匿名
		// GET，计数永远归零、封禁永远不触发，字典攻击又回到无限次。
		// 会话 cookie 同理不在这里清：它是「过去某次登录成功」的凭据，
		// 不代表当下验过密码，而且可能被第三方持有一份。
		if perm != auth.PermNone {
			s.noteAuthSuccess(ip, user)
		}
	case cookieUser != "":
		// 会话身份：cookie 在登录时已经验过密码，这里只按身份取权限。
		user = cookieUser
		perm = s.auth.PermFor(cookieUser, urlPath)
	default:
		perm = s.auth.Lookup(urlPath, "", "", false)
	}
	if perm != auth.PermNone {
		// 会话身份也算「已认证」—— 否则登录后每个请求都会被当成匿名。
		return permResult{Perm: perm, User: user, Authenticated: hasCred || cookieUser != ""}, true
	}

	// 只有「带了凭据但不对」才算一次失败；完全没带凭据只是未登录，
	// 否则一个不带凭据的爬虫就能把正常用户的来源封掉。
	//
	// 无效 cookie（过期/已撤销/伪造）同样算失败：cookie 值完全由客户端提供，
	// 伪造一个来试探是可行的，所以它和错密码一样该计数。
	if hasCred || hadCookie {
		s.noteAuthFailure(ip, limitUser)
	}

	if s.auth.NeedsAuth(urlPath) {
		// cookie 无效就顺手清掉，免得浏览器每个后续请求都带着一张废票
		// 反复撞限速闸门。
		if hadCookie && cookieUser == "" {
			s.clearSessionCookie(w, r)
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
	// ⚠️ 鉴权与落盘解析必须使用**同一个规范路径**。
	//
	// ServeMux 只对**转义后**的路径做规范化重定向：`/docs/%2e%2e/secret.txt`
	// 的转义路径里没有字面量 `..`，不会触发重定向，而 URL.Path 已经解码成
	// `/docs/../secret.txt`。若鉴权拿这个原始值去匹配 ACL（命中 /docs），
	// 落盘时 Resolve 又把它折叠成 `/secret.txt`，受限账号就能越出自己的目录。
	//
	// CleanURLPath 保留结尾斜杠语义，所以对正常路径没有任何行为变化。
	// 之后再按磁盘真实名字（大小写/Unicode 规范化/Windows 短名）对齐一次，
	// 否则 ACL 会按请求里的写法匹配，而文件系统按另一套语义打开同一个文件。
	urlPath := s.canonicalPath(r, r.URL.Path)

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
	//
	// ⚠️ 这个预检也会验证 Basic 口令（checkPerm），因此必须与 authorize
	// 共用同一套失败计数/封禁：否则攻击者对 "/" 发随机密码就能绕过限速，
	// 用 PBKDF2 把 CPU 打满。
	if s.auth.Enabled() && isAppShellNavigation(r) {
		ip := s.clientIP(r)
		user, _, hasCred := credentials(r)
		if blocked, remain := s.authBlocked(ip, user); blocked {
			w.Header().Set("Retry-After", fmt.Sprintf("%d", int(remain.Seconds())+1))
			http.Error(w, "429 Too Many Requests: 认证失败次数过多，请稍后再试",
				http.StatusTooManyRequests)
			return
		}
		if s.checkPerm(r, urlPath) == auth.PermNone {
			if hasCred {
				s.noteAuthFailure(ip, user)
			} else if sessionToken(r) != "" {
				// 无效/过期的 cookie 与猜密码同性质。
				s.noteAuthFailure(ip, "")
			}
			s.renderAuthGate(w, r, urlPath)
			return
		}
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
		// ErrNoRoot 在混合形状下已不会出现（首段不匹配就是主目录下的普通
		// 路径），保留这条分支纯粹是兜底；真正的「想越界」仍是 403。
		if errors.Is(err, fsutil.ErrNoRoot) {
			http.Error(w, "404 Not Found", http.StatusNotFound)
			return
		}
		http.Error(w, "403 Forbidden", http.StatusForbidden)
		return
	}

	// 软链（以及大小写/规范化别名）可能让实际目标落在另一条 ACL 规则下。
	// 请求路径命中的是较宽的规则，真实目标却可能是只读甚至无权限：
	// 读写之前必须按真实路径再复核一次。
	//
	// DELETE / MOVE 的叶子如果是软链，操作对象是链接本身（os.Remove /
	// os.Rename 不会跟随），不按目标权限校验，否则用户无法清理自己的链接。
	leafLink := false
	if fi, lerr := os.Lstat(abs); lerr == nil && fi.Mode()&os.ModeSymlink != 0 {
		leafLink = true
	}
	if !(leafLink && (r.Method == http.MethodDelete || r.Method == MethodMove)) {
		realURL, realPerm, within := s.realPermOf(r, abs)
		if within {
			if realURL != urlPath {
				if realPerm == auth.PermNone {
					http.Error(w, "403 Forbidden: 目标路径没有访问权限", http.StatusForbidden)
					return
				}
				if realPerm < p.Perm {
					p.Perm = realPerm
				}
			}
		} else if p.Share || !s.cfg.AllowSymlink {
			// 分享链接无法表达根外目标；普通账号只有显式 --allow-symlink 才放行。
			http.Error(w, "403 Forbidden: 目标路径超出服务范围", http.StatusForbidden)
			return
		}
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
			// 浏览器深链（不是 JSON 接口）也返回完整外壳，只是状态码 404。
			// 否则从书签/历史进来只会看到一张裸文本页，看起来像服务挂了。
			if r.Method == http.MethodGet && !wantsJSON(r) &&
				!r.URL.Query().Has("json") {
				listing := &fsutil.Listing{
					Path:  fsutil.CleanURLPath(urlPath),
					Name:  path.Base(urlPath),
					IsDir: true,
				}
				data := s.newPageData(p, listing, "", s.isAdminRequest(r))
				data.NotFound = true
				s.writeHTMLStatus(w, data, http.StatusNotFound)
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

	// 压缩包预览与解压入口统一走 /fs/extract，这里只做提示。
	if q.Has("extract") {
		http.Redirect(w, r, s.PathPrefix()+"/fs/extract?path="+url.QueryEscape(urlPath), http.StatusTemporaryRedirect)
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
	s.mergeSiteEntries(listing, r, urlPath)
	s.writeListing(w, r, listing, urlPath, p)
}

// mergeSiteEntries 把附加站点作为条目并进**主目录**的列表。
//
// 站点挂在根这一层，所以只在 `/` 上并 —— 子目录里再列一遍没有意义
// （那些路径本来也不是站点）。
//
// 为什么可以并进来：条目 `/backup` 会让 `DELETE /backup?recursive` 解析到
// 站点根目录，但删除/移动的根保护已经按**解析后的绝对路径**判定
// （见 Roots.IsRoot），所以那条路是 403，不会真把站点删掉。前端还会对
// Site 条目隐藏那几个按钮。
//
// 这里过滤掉「看不到的站点」，是**防御性**的：按当前的权限模型（规则是最长
// 前缀匹配，没有 deny），能列出主目录的账号必然对 `/` 有读权限，而每个站点
// 路径都在 `/` 之下 —— 所以此刻它其实拦不到什么。留着是因为这一层一旦成立
// 就直接生效：将来若允许站点挂在别的前缀下，或权限模型支持「只授某个站点」，
// 不改成「看不到的站点连名字都不露」就等于把机器上挂了什么泄出去。
func (s *Server) mergeSiteEntries(listing *fsutil.Listing, r *http.Request, urlPath string) {
	if !s.MultiRoot() || fsutil.CleanURLPath(urlPath) != "/" {
		return
	}
	for _, m := range s.Mounts()[1:] {
		sitePath := "/" + m.Label
		if s.checkPerm(r, sitePath) == auth.PermNone {
			continue
		}
		e := fsutil.Entry{Path: sitePath, Name: m.Label, IsDir: true, Site: true}
		if fi, err := os.Stat(m.Res.Root()); err == nil {
			e.Mtime = fi.ModTime().Format(time.RFC3339)
		}
		listing.Entries = append(listing.Entries, e)
		listing.DirCount++
	}
	// 并完重新排一次序：否则站点会一律坠在真实条目后面，
	// 而页面（客户端也会排序）与 ?json / wget 拿到的顺序就对不上了。
	fsutil.SortEntries(listing.Entries)
	listing.Total = len(listing.Entries)
}

// writeListing 输出一次目录列举的结果。
//
// 从 handleGet 里抽出来是为了让多根模式下的虚拟根（挂载点列表）复用同一套
// 分支：?json / ?simple / 命令行工具 / SPA 外壳，两处必须完全一致 ——
// 各写一份迟早会出现「/ 上能 wget、/data 上不能」这种怪事。
func (s *Server) writeListing(w http.ResponseWriter, r *http.Request,
	listing *fsutil.Listing, urlPath string, p permResult) {

	q := r.URL.Query()
	listing.Path = fsutil.CleanURLPath(urlPath)

	// 明确要求 JSON 时直接返回数据。
	if q.Has("json") || wantsJSON(r) {
		s.writeJSON(w, listing)
		return
	}
	// 只要名字列表。
	if q.Has("simple") {
		w, reset := withWriteProgress(w, s.cfg.DownloadTimeout)
		defer reset()
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

	// 命令行工具（wget / curl 这类）要的是**带链接的列表**，不是 SPA 外壳。
	// 走这一支的判据与 ?json / ?simple 一致：由客户端用 Accept 声明要什么。
	if isToolClient(r) {
		s.renderLinkIndex(w, r, listing, shareTokenOf(r))
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
		// 摘要要读完整个文件：与上传/解压共用重任务闸门。
		release, ok := s.acquireJob(r.Context())
		if !ok {
			w.Header().Set("Retry-After", "2")
			http.Error(w, "503 Service Unavailable: 摘要任务过多，请稍后重试",
				http.StatusServiceUnavailable)
			return
		}
		sum, err := fileSHA256(abs)
		release()
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
	// 先按与 http.ServeContent 相同的规则确定 Content-Type，再据此决定沙箱。
	//
	// ⚠️ 不能只看扩展名：ServeContent 对不认识的扩展名会嗅探内容，
	// 一份 .md / 无扩展名文件只要以 <!DOCTYPE html> 开头就会被标成 text/html，
	// 浏览器会当页面执行；而 .shtml / .xht 本身就是 HTML 类文档，却不在
	// 早先的白名单里。按最终 Content-Type 判断才能覆盖这些路径。
	ct := detectContentType(f, info.Name())
	w.Header().Set("Content-Type", ct)
	// 给下载也设一个「无进展超时」：没有它，一个「连上就不读数据」的客户端
	// 会一直占着并发名额（服务端阻塞在写 socket 上），几十个就能拖垮服务。
	// 用进展而不是总时长，是为了不误杀慢速的大文件传输。
	w, resetWrite := withWriteProgress(w, s.cfg.DownloadTimeout)
	defer resetWrite()
	s.guardInlineContent(w, r, info.Name(), ct)
	http.ServeContent(w, r, info.Name(), info.ModTime(), f)
}

// detectContentType 按 http.ServeContent 的规则确定响应 Content-Type：
// 先查扩展名，查不到时读取前 512 字节做内容嗅探。
//
// 单独抽出来是为了让「决定要不要加 CSP sandbox」和「真正写出的
// Content-Type」使用同一个值 —— 两边各算一次迟早会漂移。
func detectContentType(f *os.File, name string) string {
	if ct := mime.TypeByExtension(strings.ToLower(filepath.Ext(name))); ct != "" {
		return ct
	}
	buf := make([]byte, 512)
	n, err := f.ReadAt(buf, 0)
	if err != nil && !errors.Is(err, io.EOF) {
		return "application/octet-stream"
	}
	return http.DetectContentType(buf[:n])
}

// guardInlineContent 抑制「上传的文件在同源下执行脚本」带来的风险。
//
// 文件服务器天然会把用户上传的内容原样吐回浏览器，而浏览器会把它当作
// **本站页面**渲染。于是一个能上传文件的人只要放一个带 <script> 的页面，
// 再诱导管理员打开，就能借管理员已登录的会话为所欲为。这是典型的存储型 XSS。
//
// 两道处理：
//   - nosniff：禁止浏览器按内容猜测类型；
//   - 对 HTML / XHTML / SVG / XML 加 CSP sandbox：文档进入不透明源，
//     读不到本站 cookie / 存储，也发不出带凭据的同源请求。
//
// ⚠️ 判据必须是**最终 Content-Type**，而不是扩展名：ServeContent 会把
// 未知扩展名的 HTML 内容嗅探成 text/html，.shtml / .xht 也天然是 HTML
// 文档。早先只认扩展名白名单，这三类都能绕过沙箱。
//
// HTML 的 sandbox 额外带 allow-scripts：静态页面里的脚本需要能运行，
// 但没有 allow-same-origin，脚本仍在不透明源中，拿不到本站数据。
// SVG / XML 更严格，连脚本也不开放。需要完全关闭沙箱（脚本 + 同源）
// 时才用 --no-html-sandbox，那是部署方明确承担风险的选择。
func (s *Server) guardInlineContent(w http.ResponseWriter, r *http.Request, name, contentType string) {
	h := w.Header()
	h.Set("X-Content-Type-Options", "nosniff")

	if s.cfg.DisableHTMLSandbox {
		return
	}
	// --unsafe-index-html 是显式承担风险：让 index.html 在 render-index
	// 与直接访问两条路径上行为一致。其它 HTML 仍保持沙箱。
	if s.cfg.UnsafeIndexHTML && strings.EqualFold(filepath.Base(name), "index.html") {
		return
	}
	mediaType := contentType
	if mt, _, err := mime.ParseMediaType(contentType); err == nil {
		mediaType = mt
	}
	mediaType = strings.ToLower(strings.TrimSpace(mediaType))

	// 扩展名作为兜底：有些类型（如 .mhtml）在部分平台的 MIME 表里
	// 没有映射，Content-Type 会被嗅探成 text/plain，但浏览器仍可能按
	// 文档处理。两条判据取并集。
	scriptable := scriptableMediaTypes[mediaType] || isScriptableContent(name)
	if !scriptable {
		return
	}
	// 注意：绝不加 allow-same-origin —— 那会让上传的页面直接变回本站页面，
	// 存储型 XSS 的第二道防线就没了。
	if htmlMediaTypes[mediaType] || isHTMLContent(name) {
		h.Set("Content-Security-Policy", "sandbox allow-scripts; frame-ancestors 'none'")
		return
	}
	h.Set("Content-Security-Policy", "sandbox; frame-ancestors 'none'")
}

// scriptableMediaTypes 是浏览器会当作可执行文档渲染的 MIME 类型。
var scriptableMediaTypes = map[string]bool{
	"text/html":             true,
	"application/xhtml+xml": true,
	"image/svg+xml":         true,
	"application/xml":       true,
	"text/xml":              true,
	"application/xsl+xml":   true,
	"text/xsl":              true,
}

// htmlMediaTypes 是允许在沙箱内运行脚本的 HTML 类文档。
var htmlMediaTypes = map[string]bool{
	"text/html":             true,
	"application/xhtml+xml": true,
}

// scriptableExts 是浏览器会当作可执行文档渲染的扩展名。
var scriptableExts = map[string]bool{
	".html": true, ".htm": true, ".shtml": true, ".shtm": true,
	".xhtml": true, ".xht": true, ".svg": true,
	".xml": true, ".xsl": true, ".mhtml": true, ".mht": true,
}

// htmlExts 是允许在沙箱内运行脚本的 HTML 类文档。
var htmlExts = map[string]bool{
	".html": true, ".htm": true, ".shtml": true, ".shtm": true,
	".xhtml": true, ".xht": true,
}

func isHTMLContent(name string) bool {
	return htmlExts[strings.ToLower(filepath.Ext(name))]
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
	// 大 JSON（目录列举 / 搜索结果）遇到「连上不读」的客户端时，
	// 不能让 Write 永久阻塞并占着并发名额。
	w, reset := withWriteProgress(w, s.cfg.DownloadTimeout)
	defer reset()

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	// 禁止浏览器按内容嗅探类型。JSON 里可能带着文档原文（包括 <script> 字样），
	// 万一被某条路径当成 text/html 渲染，那就是一个 XSS。
	// 声明成 JSON + nosniff，它就只是数据。
	w.Header().Set("X-Content-Type-Options", "nosniff")
	// 认证/设置/密钥等响应可能包含账号名、根目录、一次性明文密钥；
	// 不允许浏览器或中间缓存留下副本。
	w.Header().Set("Cache-Control", "no-store")
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
		// 内部错误只进日志，不回给客户端：err.Error() 里常带绝对路径、
		// 系统调用细节，对攻击者是免费的信息泄露。
		s.logger.Errorf("请求处理失败: %v", err)
		http.Error(w, "500 Internal Server Error", http.StatusInternalServerError)
	}
}

// handleHealth 健康检查。
func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	s.writeJSON(w, map[string]any{
		"status":  "ok",
		"version": s.cfg.Version,
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
