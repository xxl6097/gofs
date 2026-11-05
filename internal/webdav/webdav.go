// Package webdav 实现 WebDAV 服务器，支持账号管理和目录访问控制。
package webdav

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"golang.org/x/net/webdav"
)

// Account 表示一个 WebDAV 账号。
type Account struct {
	Username    string   `json:"username"`
	Password    string   `json:"password,omitempty"` // 创建时需要，存储时不返回
	PasswordHash string  `json:"password_hash,omitempty"` // PBKDF2 哈希
	AllowedDirs []string `json:"allowed_dirs"` // 允许访问的目录列表
	CreatedAt   int64    `json:"created_at"`
	UpdatedAt   int64    `json:"updated_at"`
}

// Server 是 WebDAV 服务器实例。
type Server struct {
	mu       sync.RWMutex
	accounts map[string]*Account
	rootDir  string
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

	// 检查路径权限
	if !s.isPathAllowed(username, r.URL.Path) {
		http.Error(w, "无权访问此路径", http.StatusForbidden)
		return
	}

	// 调用 WebDAV 处理器
	s.handler.ServeHTTP(w, r)
}

// Handler 返回可以挂载到 HTTP 路由的处理器。
func (s *Server) Handler() http.Handler {
	return s
}
