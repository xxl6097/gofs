package server

import (
	"net/http"
	"path/filepath"
	"strings"

	"github.com/xxl6097/gofs/internal/config"
	"github.com/xxl6097/gofs/internal/webdav"
)

// handleWebDAVAccounts 处理 /api/webdav/accounts 请求（列出和创建账号）
func (s *Server) handleWebDAVAccounts(w http.ResponseWriter, r *http.Request) {
	// 检查管理员权限
	if !s.requireAdminOr(w, r, "管理 WebDAV 账号") {
		return
	}

	api := s.getWebDAVAPI()
	if api == nil {
		http.Error(w, "WebDAV 功能未启用", http.StatusServiceUnavailable)
		return
	}

	switch r.Method {
	case http.MethodGet:
		api.HandleListAccounts(w, r)
	case http.MethodPost:
		api.HandleCreateAccount(w, r)
	default:
		http.Error(w, "方法不允许", http.StatusMethodNotAllowed)
	}
}

// handleWebDAVAccountsDetail 处理 /api/webdav/accounts/{username} 请求（更新、删除、修改密码）
func (s *Server) handleWebDAVAccountsDetail(w http.ResponseWriter, r *http.Request) {
	// 检查管理员权限
	if !s.requireAdminOr(w, r, "管理 WebDAV 账号") {
		return
	}

	api := s.getWebDAVAPI()
	if api == nil {
		http.Error(w, "WebDAV 功能未启用", http.StatusServiceUnavailable)
		return
	}

	// 提取路径
	path := strings.TrimPrefix(r.URL.Path, "/api/webdav/accounts/")
	if path == "" {
		http.Error(w, "用户名不能为空", http.StatusBadRequest)
		return
	}

	// 检查是否是修改密码请求
	if strings.HasSuffix(path, "/password") && r.Method == http.MethodPost {
		api.HandleChangePassword(w, r)
		return
	}

	switch r.Method {
	case http.MethodPut:
		api.HandleUpdateAccount(w, r)
	case http.MethodDelete:
		api.HandleDeleteAccount(w, r)
	default:
		http.Error(w, "方法不允许", http.StatusMethodNotAllowed)
	}
}

// getWebDAVAPI 获取或创建 WebDAV API 实例（懒加载）
func (s *Server) getWebDAVAPI() *webdav.API {
	s.webdavMu.Lock()
	defer s.webdavMu.Unlock()

	if s.webdavAPI != nil {
		return s.webdavAPI
	}

	// 初始化 WebDAV 服务
	root := s.res.Root()
	webdavServer, err := webdav.New(root)
	if err != nil {
		s.logger.Errorf("WebDAV 初始化失败: %v", err)
		return nil
	}

	// 确定账号文件路径：使用与 keys.json 相同的目录
	accountsFile := filepath.Join(config.DefaultKeyDir(), "webdav_accounts.json")

	// 创建存储
	storage := webdav.NewStorage(accountsFile)

	// 加载已有账号
	accounts, err := storage.Load()
	if err != nil {
		accounts = make(map[string]*webdav.Account)
	}
	webdavServer.LoadAccounts(accounts)

	// 创建 API
	s.webdavAPI = webdav.NewAPI(webdavServer, storage)
	s.webdavServer = webdavServer

	s.logger.Infof("WebDAV 服务已初始化，账号文件: %s", accountsFile)

	return s.webdavAPI
}
