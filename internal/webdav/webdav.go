// Package webdav 实现 WebDAV 服务器，支持账号管理和目录访问控制。
package webdav

import (
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

// Server 是 WebDAV 服务器实例。
type Server struct {
	mu       sync.RWMutex
	accounts map[string]*Account
	rootDir  string
	prefix   string // 挂载前缀，例如 "/dav/"
	handler  *webdav.Handler
}

// New 创建一个新的 WebDAV 服务器。
func New(rootDir string) (*Server, error) {
	absRoot, err := filepath.Abs(rootDir)
	if err != nil {
		return nil, fmt.Errorf("解析根目录失败: %w", err)
	}

	s := &Server{
		accounts: make(map[string]*Account),
		rootDir:  absRoot,
	}

	// 创建 WebDAV 处理器
	s.handler = &webdav.Handler{
		FileSystem: webdav.Dir(absRoot),
		LockSystem: webdav.NewMemLS(),
		Logger: func(r *http.Request, err error) {
			if err != nil {
				fmt.Printf("WebDAV error: %v\n", err)
			}
		},
	}

	return s, nil
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

	s.mu.Lock()
	defer s.mu.Unlock()

	if _, exists := s.accounts[username]; exists {
		return fmt.Errorf("用户 %s 已存在", username)
	}

	// 验证目录是否存在
	for _, dir := range allowedDirs {
		fullPath := filepath.Join(s.rootDir, dir)
		if info, err := os.Stat(fullPath); err != nil {
			return fmt.Errorf("目录 %s 不存在: %w", dir, err)
		} else if !info.IsDir() {
			return fmt.Errorf("%s 不是目录", dir)
		}
	}

	now := timeNow().Unix()
	account := &Account{
		Username:    username,
		AllowedDirs: allowedDirs,
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

// UpdateAccount 更新账号的允许目录列表。
func (s *Server) UpdateAccount(username string, allowedDirs []string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	account, exists := s.accounts[username]
	if !exists {
		return fmt.Errorf("用户 %s 不存在", username)
	}

	// 验证目录是否存在
	for _, dir := range allowedDirs {
		fullPath := filepath.Join(s.rootDir, dir)
		if info, err := os.Stat(fullPath); err != nil {
			return fmt.Errorf("目录 %s 不存在: %w", dir, err)
		} else if !info.IsDir() {
			return fmt.Errorf("%s 不是目录", dir)
		}
	}

	account.AllowedDirs = allowedDirs
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

	// 检查路径权限。ACL 里的目录是相对服务根目录的（"/docs"），
	// 而请求路径带着挂载前缀（"/dav/docs"），比较前先剥掉前缀。
	s.mu.RLock()
	prefix := s.prefix
	s.mu.RUnlock()

	aclPath := r.URL.Path
	if prefix != "" {
		if strings.HasPrefix(aclPath, prefix) {
			aclPath = "/" + strings.TrimPrefix(aclPath, prefix)
		} else if aclPath+"/" == prefix {
			aclPath = "/"
		}
	}

	if !s.isPathAllowed(username, aclPath) {
		// 把授权目录一并回给已认证的用户：客户端连到 /dav/ 根目录时只拿到
		// 一个干巴巴的 403，很难看出「要连子目录」这件事。
		msg := "无权访问此路径"
		if dirs := s.allowedDirs(username); len(dirs) > 0 {
			msg += "；该账号只被授权访问: " + strings.Join(dirs, ", ")
		}
		http.Error(w, msg, http.StatusForbidden)
		return
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
	fsPath := filepath.Join(s.rootDir, filepath.FromSlash(clean))
	// filepath.Clean 已经消掉了 ".."，这里再确认一次没有跑出服务根目录。
	if fsPath != s.rootDir && !strings.HasPrefix(fsPath, s.rootDir+string(filepath.Separator)) {
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
