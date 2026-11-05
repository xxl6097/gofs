// Package webdav 实现 WebDAV 服务器，支持账号管理和目录访问控制。
package webdav

import (
	"context"
	"fmt"
	"html"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/webdav"
)

// Account 表示一个 WebDAV 账号。
type Account struct {
	Username     string   `json:"username"`
	Password     string   `json:"password,omitempty"`      // 创建时需要，存储时不返回
	PasswordHash string   `json:"password_hash,omitempty"` // PBKDF2 哈希
	AllowedDirs  []string `json:"allowed_dirs"`            // 允许访问的目录列表
	CreatedAt    int64    `json:"created_at"`
	UpdatedAt    int64    `json:"updated_at"`
}

// Resolver 把 WebDAV 的 URL 路径（已去掉挂载前缀）解析为磁盘绝对路径。
//
// 单根部署用 New 就够了；gofs 的多站点根（"/work" 指向另一个磁盘目录）
// 实现这个接口后通过 SetResolver 注入，文件操作、目录浏览和账号目录校验
// 就都会走同一套解析，而不是傻拼在第一个根目录下面。
type Resolver interface {
	Resolve(urlPath string) (string, error)
}

// Server 是 WebDAV 服务器实例。
type Server struct {
	mu       sync.RWMutex
	accounts map[string]*Account
	rootDir  string
	resolver Resolver
	prefix   string // 挂载前缀，例如 "/dav/"
	handler  *webdav.Handler
}

// New 创建一个新的 WebDAV 服务器。
func New(rootDir string) (*Server, error) {
	absRoot, err := filepath.Abs(rootDir)
	if err != nil {
		return nil, fmt.Errorf("解析根目录失败: %w", err)
	}
	absRoot = filepath.Clean(absRoot)

	s := &Server{
		accounts: make(map[string]*Account),
		rootDir:  absRoot,
		resolver: singleRoot(absRoot),
	}

	// 创建 WebDAV 处理器
	s.handler = &webdav.Handler{
		FileSystem: &resolvedFS{s: s},
		LockSystem: webdav.NewMemLS(),
		Logger: func(r *http.Request, err error) {
			if err != nil {
				fmt.Printf("WebDAV error: %v\n", err)
			}
		},
	}

	return s, nil
}

// SetResolver 注入 URL 路径解析器，让附加站点（"/work" 之类）也能被访问。
//
// 解析器在每次请求时调用，所以运行期增删站点无需重建 WebDAV 服务。
func (s *Server) SetResolver(res Resolver) {
	s.mu.Lock()
	s.resolver = res
	s.mu.Unlock()
}

// resolve 把 URL 路径交给当前解析器；没注入时退回单根目录。
func (s *Server) resolve(urlPath string) (string, error) {
	s.mu.RLock()
	res := s.resolver
	s.mu.RUnlock()
	if res == nil {
		return singleRoot(s.rootDir).Resolve(urlPath)
	}
	return res.Resolve(urlPath)
}

// isRoot 判断磁盘路径是否正好是某个挂载根（主目录或附加站点）。
// 不支持这个查询的解析器（单根）返回 false。
func (s *Server) isRoot(abs string) bool {
	s.mu.RLock()
	res := s.resolver
	s.mu.RUnlock()
	checker, ok := res.(interface{ IsRoot(string) bool })
	return ok && checker.IsRoot(abs)
}

// singleRoot 是单根目录的解析器：URL 路径直接相对 root。
type singleRoot string

func (r singleRoot) Resolve(urlPath string) (string, error) {
	root := string(r)
	clean := filepath.Clean("/" + strings.TrimPrefix(urlPath, "/"))
	full := filepath.Join(root, filepath.FromSlash(clean))
	// filepath.Join 已经消掉了 ".."，这里再确认一次没有跑出根目录。
	if full != root && !strings.HasPrefix(full, root+string(filepath.Separator)) {
		return "", fmt.Errorf("路径越界: %s", urlPath)
	}
	return full, nil
}

// resolvedFS 让 webdav.Handler 的文件操作先经过 Resolver。
//
// 直接用 webdav.Dir 只能绑定一个根目录，"/work/..." 会被当成主目录下的
// work 子目录；换成这层适配后，每个请求都能落到正确的挂载站点。
type resolvedFS struct{ s *Server }

func (fs *resolvedFS) Mkdir(_ context.Context, name string, perm os.FileMode) error {
	p, err := fs.s.resolve(name)
	if err != nil {
		return err
	}
	return os.Mkdir(p, perm)
}

func (fs *resolvedFS) OpenFile(_ context.Context, name string, flag int, perm os.FileMode) (webdav.File, error) {
	p, err := fs.s.resolve(name)
	if err != nil {
		return nil, err
	}
	return os.OpenFile(p, flag, perm)
}

func (fs *resolvedFS) RemoveAll(_ context.Context, name string) error {
	p, err := fs.s.resolve(name)
	if err != nil {
		return err
	}
	// 授权目录可能就是站点根本身；WebDAV 的 DELETE 不该把整个挂载目录端掉
	// （主目录同理）。目录里的内容仍然可以正常删除。
	if fs.s.isRoot(p) {
		return os.ErrPermission
	}
	return os.RemoveAll(p)
}

func (fs *resolvedFS) Rename(_ context.Context, oldName, newName string) error {
	oldPath, err := fs.s.resolve(oldName)
	if err != nil {
		return err
	}
	newPath, err := fs.s.resolve(newName)
	if err != nil {
		return err
	}
	if fs.s.isRoot(oldPath) || fs.s.isRoot(newPath) {
		return os.ErrPermission
	}
	return os.Rename(oldPath, newPath)
}

func (fs *resolvedFS) Stat(_ context.Context, name string) (os.FileInfo, error) {
	p, err := fs.s.resolve(name)
	if err != nil {
		return nil, err
	}
	return os.Stat(p)
}

// SetPrefix 设置挂载前缀（例如 "/dav/"）。
//
// webdav.Handler 需要知道前缀，才能把 "/dav/docs/a.txt" 映射到根目录下的
// "docs/a.txt"；不设的话它会去找根目录里的 "dav/docs/a.txt"，所有请求都是 404。
// 目录权限按去掉前缀后的路径判断，所以 ACL 里的 "/docs" 才和请求对得上。
func (s *Server) SetPrefix(prefix string) {
	if prefix == "" {
		return
	}
	if !strings.HasPrefix(prefix, "/") {
		prefix = "/" + prefix
	}
	if !strings.HasSuffix(prefix, "/") {
		prefix += "/"
	}

	s.mu.Lock()
	s.prefix = prefix
	s.mu.Unlock()
	s.handler.Prefix = prefix
}

// AddAccount 添加一个 WebDAV 账号。
func (s *Server) AddAccount(username, password string, allowedDirs []string) error {
	if username == "" {
		return fmt.Errorf("用户名不能为空")
	}
	if password == "" {
		return fmt.Errorf("密码不能为空")
	}

	dirs, err := s.normalizeAllowedDirs(allowedDirs)
	if err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if _, exists := s.accounts[username]; exists {
		return fmt.Errorf("用户 %s 已存在", username)
	}

	now := timeNow().Unix()
	account := &Account{
		Username:    username,
		AllowedDirs: dirs,
		CreatedAt:   now,
		UpdatedAt:   now,
	}

	// 使用 PBKDF2 哈希密码
	account.PasswordHash = hashPassword(password)
	s.accounts[username] = account

	return nil
}

// RemoveAccount 删除一个 WebDAV 账号。
func (s *Server) RemoveAccount(username string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, exists := s.accounts[username]; !exists {
		return fmt.Errorf("用户 %s 不存在", username)
	}

	delete(s.accounts, username)
	return nil
}

// normalizeAllowedDirs 规范化并校验账号的允许目录。
//
// 目录是服务侧的 URL 路径（"/docs"、挂载站点 "/work"），必须交给
// Resolver 解析后再 stat —— 直接拼在 rootDir 下面会把挂载站点误判为
// 「目录不存在」。顺带去掉重复项和尾部斜杠，避免 ACL 里出现两条等价规则。
func (s *Server) normalizeAllowedDirs(allowedDirs []string) ([]string, error) {
	if len(allowedDirs) == 0 {
		return nil, fmt.Errorf("至少需要指定一个允许访问的目录")
	}

	out := make([]string, 0, len(allowedDirs))
	seen := make(map[string]bool, len(allowedDirs))
	for _, dir := range allowedDirs {
		dir = strings.TrimSpace(dir)
		if dir == "" {
			continue
		}
		clean := filepath.ToSlash(filepath.Clean("/" + strings.TrimPrefix(dir, "/")))
		if seen[clean] {
			continue
		}

		abs, err := s.resolve(clean)
		if err != nil {
			return nil, fmt.Errorf("目录 %s 不存在: %w", dir, err)
		}
		info, err := os.Stat(abs)
		if err != nil {
			return nil, fmt.Errorf("目录 %s 不存在: %w", dir, err)
		}
		if !info.IsDir() {
			return nil, fmt.Errorf("%s 不是目录", dir)
		}

		seen[clean] = true
		out = append(out, clean)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("至少需要指定一个允许访问的目录")
	}
	return out, nil
}

// UpdateAccount 更新账号的允许目录列表。
func (s *Server) UpdateAccount(username string, allowedDirs []string) error {
	dirs, err := s.normalizeAllowedDirs(allowedDirs)
	if err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	account, exists := s.accounts[username]
	if !exists {
		return fmt.Errorf("用户 %s 不存在", username)
	}

	account.AllowedDirs = dirs
	account.UpdatedAt = timeNow().Unix()

	return nil
}

// ChangePassword 修改账号密码。
func (s *Server) ChangePassword(username, newPassword string) error {
	if newPassword == "" {
		return fmt.Errorf("密码不能为空")
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	account, exists := s.accounts[username]
	if !exists {
		return fmt.Errorf("用户 %s 不存在", username)
	}

	account.PasswordHash = hashPassword(newPassword)
	account.UpdatedAt = timeNow().Unix()

	return nil
}

// ListAccounts 列出所有账号（不含密码）。
func (s *Server) ListAccounts() []*Account {
	s.mu.RLock()
	defer s.mu.RUnlock()

	accounts := make([]*Account, 0, len(s.accounts))
	for _, acc := range s.accounts {
		// 返回副本，不含密码哈希
		accounts = append(accounts, &Account{
			Username:    acc.Username,
			AllowedDirs: append([]string{}, acc.AllowedDirs...),
			CreatedAt:   acc.CreatedAt,
			UpdatedAt:   acc.UpdatedAt,
		})
	}

	return accounts
}

// LoadAccounts 批量加载账号（用于从存储恢复）。
func (s *Server) LoadAccounts(accounts map[string]*Account) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.accounts = accounts
}

// authenticate 验证用户名和密码。
func (s *Server) authenticate(username, password string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()

	account, exists := s.accounts[username]
	if !exists {
		return false
	}

	return verifyPassword(password, account.PasswordHash)
}

// isPathAllowed 检查用户是否有权限访问指定路径。
func (s *Server) isPathAllowed(username, requestPath string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()

	account, exists := s.accounts[username]
	if !exists {
		return false
	}

	// 如果没有配置允许目录，拒绝访问
	if len(account.AllowedDirs) == 0 {
		return false
	}

	// 规范化请求路径
	cleanPath := filepath.Clean("/" + strings.TrimPrefix(requestPath, "/"))

	// 检查是否在允许的目录中
	for _, allowedDir := range account.AllowedDirs {
		allowedClean := filepath.Clean("/" + strings.TrimPrefix(allowedDir, "/"))

		// 检查路径是否以允许的目录开头
		if cleanPath == allowedClean || strings.HasPrefix(cleanPath, allowedClean+"/") {
			return true
		}
	}

	return false
}

// stripPrefix 去掉 WebDAV 的挂载前缀，得到用于 ACL 与磁盘解析的路径。
func stripPrefix(p, prefix string) string {
	if prefix == "" {
		return p
	}
	if strings.HasPrefix(p, prefix) {
		return "/" + strings.TrimPrefix(p, prefix)
	}
	if p+"/" == prefix {
		return "/"
	}
	return p
}

// forbidden 返回 403，并把账号的授权目录附在消息里。
//
// 客户端连到 /dav/ 根目录时只拿到一个干巴巴的 403，很难看出「要连子目录」；
// 带上授权目录能直接指路。
func (s *Server) forbidden(w http.ResponseWriter, username string) {
	msg := "无权访问此路径"
	if dirs := s.allowedDirs(username); len(dirs) > 0 {
		msg += "；该账号只被授权访问: " + strings.Join(dirs, ", ")
	}
	http.Error(w, msg, http.StatusForbidden)
}

// allowedDirs 返回账号被授权的目录（副本）。
func (s *Server) allowedDirs(username string) []string {
	s.mu.RLock()
	defer s.mu.RUnlock()

	account, exists := s.accounts[username]
	if !exists {
		return nil
	}
	return append([]string{}, account.AllowedDirs...)
}

// ServeHTTP 处理 WebDAV 请求。
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// 获取 Basic Auth 凭证
	username, password, ok := r.BasicAuth()
	if !ok {
		w.Header().Set("WWW-Authenticate", `Basic realm="WebDAV"`)
		http.Error(w, "需要认证", http.StatusUnauthorized)
		return
	}

	// 验证凭证
	if !s.authenticate(username, password) {
		w.Header().Set("WWW-Authenticate", `Basic realm="WebDAV"`)
		http.Error(w, "认证失败", http.StatusUnauthorized)
		return
	}

	// 检查路径权限。ACL 里的目录是相对服务根目录的（"/docs"、"/work"），
	// 而请求路径带着挂载前缀（"/dav/docs"），比较前先剥掉前缀。
	s.mu.RLock()
	prefix := s.prefix
	s.mu.RUnlock()

	aclPath := stripPrefix(r.URL.Path, prefix)
	if !s.isPathAllowed(username, aclPath) {
		s.forbidden(w, username)
		return
	}

	// COPY / MOVE 的目标路径也要过 ACL。只检查请求路径（源）的话，
	// 一个只被授权 "/docs" 的账号可以把它挪到 "/work" 或其它目录里。
	if r.Method == "COPY" || r.Method == "MOVE" {
		if dst := r.Header.Get("Destination"); dst != "" {
			if u, err := url.Parse(dst); err == nil {
				dstPath := u.Path
				if dstPath == "" {
					dstPath = u.Opaque
				}
				if !s.isPathAllowed(username, stripPrefix(dstPath, prefix)) {
					s.forbidden(w, username)
					return
				}
			}
		}
	}

	// 浏览器直接打开目录时 GET 会得到 405（列目录应该是 PROPFIND），
	// 用户容易以为服务坏了。这里直接在浏览器里列出目录内容。
	if s.serveDirectoryLanding(w, r, username, aclPath, prefix) {
		return
	}

	// 调用 WebDAV 处理器
	s.handler.ServeHTTP(w, r)
}

// serveDirectoryLanding 处理「浏览器 GET 一个 WebDAV 目录」的请求。
//
// 真正的 WebDAV 客户端用 PROPFIND 列目录，不会走这里；只有人在浏览器里
// 直接打开链接才会命中，所以这里给一份只读的文件列表，点进去就能下载/预览。
// 上传、重命名等操作仍然要用 WebDAV 客户端或网页版。
// 返回 false 表示不是这种情况，交回 WebDAV 处理器。
func (s *Server) serveDirectoryLanding(w http.ResponseWriter, r *http.Request, username, aclPath, prefix string) bool {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		return false
	}

	clean := filepath.Clean("/" + strings.TrimPrefix(aclPath, "/"))
	// 目录浏览和 WebDAV 文件操作走同一个解析器，挂载站点里的目录也能列出来。
	fsPath, err := s.resolve(clean)
	if err != nil {
		return false
	}

	info, err := os.Stat(fsPath)
	if err != nil || !info.IsDir() {
		return false
	}

	entries, err := os.ReadDir(fsPath)
	if err != nil {
		return false
	}

	type entry struct {
		name string
		dir  bool
		size int64
		mod  time.Time
	}
	list := make([]entry, 0, len(entries))
	for _, e := range entries {
		fi, err := e.Info()
		if err != nil {
			continue // 列目录期间被删掉的文件直接跳过
		}
		list = append(list, entry{e.Name(), e.IsDir(), fi.Size(), fi.ModTime()})
	}
	sort.Slice(list, func(i, j int) bool {
		if list[i].dir != list[j].dir {
			return list[i].dir // 目录在前
		}
		return strings.ToLower(list[i].name) < strings.ToLower(list[j].name)
	})

	// 相对链接指回网页版：/dav/cmd/ → ../../cmd/。
	// 用相对路径而不是绝对路径，带 --path-prefix 部署时也能指对。
	segments := func(p string) int {
		t := strings.Trim(p, "/")
		if t == "" {
			return 0
		}
		return strings.Count(t, "/") + 1
	}
	climb := strings.Repeat("../", segments(clean)+segments(prefix))

	var uiHref string
	if clean == "/" {
		uiHref = climb
	} else {
		uiHref = climb + strings.TrimPrefix(clean, "/") + "/"
	}

	var b strings.Builder
	b.WriteString("<!doctype html><html lang=\"zh-CN\"><head><meta charset=\"utf-8\">")
	b.WriteString("<meta name=\"viewport\" content=\"width=device-width,initial-scale=1\">")
	b.WriteString("<title>WebDAV · " + html.EscapeString(clean) + "</title>")
	b.WriteString("<style>" +
		"body{margin:0;padding:32px;font:15px/1.7 -apple-system,BlinkMacSystemFont,\"Segoe UI\",sans-serif;" +
		"color:#222;background:#fafafa}" +
		"main{max-width:760px;margin:0 auto;background:#fff;border:1px solid #e5e5e5;border-radius:10px;" +
		"padding:20px 24px 24px}" +
		"h1{font-size:16px;margin:0 0 4px;font-weight:600;word-break:break-all}" +
		".meta{font-size:12px;color:#888;margin:0 0 16px}" +
		"code{background:#f2f2f2;border-radius:4px;padding:2px 6px}" +
		"ul{list-style:none;margin:0;padding:0}" +
		"li{display:flex;align-items:center;gap:10px;padding:8px 6px;border-top:1px solid #f0f0f0}" +
		"li:hover{background:#f7f9ff}" +
		"li .n{flex:1;min-width:0;overflow:hidden;text-overflow:ellipsis;white-space:nowrap}" +
		"li .s{flex:none;font:12px/1 ui-monospace,SFMono-Regular,Menlo,monospace;color:#999;width:78px;" +
		"text-align:right}" +
		"li .t{flex:none;font-size:12px;color:#999;width:112px;text-align:right}" +
		"a{color:#2563eb;text-decoration:none}a:hover{text-decoration:underline}" +
		"p.note{color:#888;font-size:13px;margin:18px 0 0}" +
		"</style></head><body><main>")

	b.WriteString("<h1><code>" + html.EscapeString(clean) + "</code></h1>")
	b.WriteString("<p class=\"meta\">只读浏览 · 共 " + fmt.Sprintf("%d", len(list)) +
		" 项 · 上传/重命名请用 WebDAV 客户端或 <a href=\"" +
		html.EscapeString(uiHref) + "\">网页版</a></p>")

	b.WriteString("<ul>")
	// 上级目录只在 ACL 也允许访问时才给，避免点出一个 403。
	if clean != "/" && s.isPathAllowed(username, filepath.Dir(clean)) {
		b.WriteString("<li><div class=\"n\"><a href=\"../\">⬆ 上级目录</a></div>" +
			"<div class=\"s\"></div><div class=\"t\"></div></li>")
	}
	for _, e := range list {
		href := "./" + url.PathEscape(e.name)
		name := e.name
		if e.dir {
			href += "/"
			name += "/"
		}
		b.WriteString("<li><div class=\"n\"><a href=\"" + html.EscapeString(href) + "\">" +
			html.EscapeString(name) + "</a></div>")
		if e.dir {
			b.WriteString("<div class=\"s\">目录</div>")
		} else {
			b.WriteString("<div class=\"s\">" + humanSize(e.size) + "</div>")
		}
		b.WriteString("<div class=\"t\">" + e.mod.Format("2006-01-02 15:04") + "</div></li>")
	}
	if len(list) == 0 {
		b.WriteString("<li><div class=\"n\" style=\"color:#999\">这个目录是空的</div></li>")
	}
	b.WriteString("</ul>")

	b.WriteString("<p class=\"note\">这是 WebDAV 端点 <code>" +
		html.EscapeString(strings.TrimSuffix(r.URL.Path, "/")) +
		"</code> 的只读视图；浏览器里的链接会沿用当前登录的 WebDAV 账号。</p>")
	b.WriteString("</main></body></html>")

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodHead {
		return true
	}
	_, _ = w.Write([]byte(b.String()))
	return true
}

// humanSize 把字节数格式化成便于阅读的形式。
func humanSize(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGTPE"[exp])
}

// Handler 返回可以挂载到 HTTP 路由的处理器。
func (s *Server) Handler() http.Handler {
	return s
}
