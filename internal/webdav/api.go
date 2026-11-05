package webdav

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// API 处理 WebDAV 账号管理的 HTTP 接口。
type API struct {
	server  *Server
	storage *Storage
}

// NewAPI 创建一个新的 API 实例。
func NewAPI(server *Server, storage *Storage) *API {
	return &API{
		server:  server,
		storage: storage,
	}
}

// Server 返回底层 WebDAV 服务器，供 HTTP 路由把 /dav/ 挂上去。
func (a *API) Server() *Server { return a.server }

// createAccountRequest 创建账号的请求体。
type createAccountRequest struct {
	Username    string   `json:"username"`
	Password    string   `json:"password"`
	AllowedDirs []string `json:"allowed_dirs"`
}

// updateAccountRequest 更新账号的请求体。
type updateAccountRequest struct {
	AllowedDirs []string `json:"allowed_dirs"`
}

// changePasswordRequest 修改密码的请求体。
type changePasswordRequest struct {
	NewPassword string `json:"new_password"`
}

// HandleListAccounts 处理列出所有账号的请求。
// GET /api/webdav/accounts
func (a *API) HandleListAccounts(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "方法不允许", http.StatusMethodNotAllowed)
		return
	}

	accounts := a.server.ListAccounts()

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"accounts": accounts,
	})
}

// HandleCreateAccount 处理创建账号的请求。
// POST /api/webdav/accounts
func (a *API) HandleCreateAccount(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "方法不允许", http.StatusMethodNotAllowed)
		return
	}

	var req createAccountRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, fmt.Sprintf("解析请求失败: %v", err), http.StatusBadRequest)
		return
	}

	// 验证输入
	if req.Username == "" {
		http.Error(w, "用户名不能为空", http.StatusBadRequest)
		return
	}
	if req.Password == "" {
		http.Error(w, "密码不能为空", http.StatusBadRequest)
		return
	}
	if len(req.AllowedDirs) == 0 {
		http.Error(w, "至少需要指定一个允许访问的目录", http.StatusBadRequest)
		return
	}

	// 创建账号
	if err := a.server.AddAccount(req.Username, req.Password, req.AllowedDirs); err != nil {
		http.Error(w, fmt.Sprintf("创建账号失败: %v", err), http.StatusBadRequest)
		return
	}

	// 保存到文件
	if err := a.storage.Save(a.server.accounts); err != nil {
		http.Error(w, fmt.Sprintf("保存失败: %v", err), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(map[string]interface{}{
		"message":  "账号创建成功",
		"username": req.Username,
	})
}

// HandleUpdateAccount 处理更新账号的请求。
// PUT /api/webdav/accounts/{username}
func (a *API) HandleUpdateAccount(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPut {
		http.Error(w, "方法不允许", http.StatusMethodNotAllowed)
		return
	}

	// 从 URL 中提取用户名
	username := strings.TrimPrefix(r.URL.Path, "/api/webdav/accounts/")
	if username == "" {
		http.Error(w, "用户名不能为空", http.StatusBadRequest)
		return
	}

	var req updateAccountRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, fmt.Sprintf("解析请求失败: %v", err), http.StatusBadRequest)
		return
	}

	if len(req.AllowedDirs) == 0 {
		http.Error(w, "至少需要指定一个允许访问的目录", http.StatusBadRequest)
		return
	}

	// 更新账号
	if err := a.server.UpdateAccount(username, req.AllowedDirs); err != nil {
		http.Error(w, fmt.Sprintf("更新账号失败: %v", err), http.StatusBadRequest)
		return
	}

	// 保存到文件
	if err := a.storage.Save(a.server.accounts); err != nil {
		http.Error(w, fmt.Sprintf("保存失败: %v", err), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"message": "账号更新成功",
	})
}

// HandleDeleteAccount 处理删除账号的请求。
// DELETE /api/webdav/accounts/{username}
func (a *API) HandleDeleteAccount(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodDelete {
		http.Error(w, "方法不允许", http.StatusMethodNotAllowed)
		return
	}

	// 从 URL 中提取用户名
	username := strings.TrimPrefix(r.URL.Path, "/api/webdav/accounts/")
	if username == "" {
		http.Error(w, "用户名不能为空", http.StatusBadRequest)
		return
	}

	// 删除账号
	if err := a.server.RemoveAccount(username); err != nil {
		http.Error(w, fmt.Sprintf("删除账号失败: %v", err), http.StatusBadRequest)
		return
	}

	// 保存到文件
	if err := a.storage.Save(a.server.accounts); err != nil {
		http.Error(w, fmt.Sprintf("保存失败: %v", err), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"message": "账号删除成功",
	})
}

// HandleChangePassword 处理修改密码的请求。
// POST /api/webdav/accounts/{username}/password
func (a *API) HandleChangePassword(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "方法不允许", http.StatusMethodNotAllowed)
		return
	}

	// 从 URL 中提取用户名
	pathParts := strings.Split(strings.TrimPrefix(r.URL.Path, "/api/webdav/accounts/"), "/")
	if len(pathParts) < 2 {
		http.Error(w, "无效的请求路径", http.StatusBadRequest)
		return
	}
	username := pathParts[0]

	var req changePasswordRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, fmt.Sprintf("解析请求失败: %v", err), http.StatusBadRequest)
		return
	}

	if req.NewPassword == "" {
		http.Error(w, "新密码不能为空", http.StatusBadRequest)
		return
	}

	// 修改密码
	if err := a.server.ChangePassword(username, req.NewPassword); err != nil {
		http.Error(w, fmt.Sprintf("修改密码失败: %v", err), http.StatusBadRequest)
		return
	}

	// 保存到文件
	if err := a.storage.Save(a.server.accounts); err != nil {
		http.Error(w, fmt.Sprintf("保存失败: %v", err), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"message": "密码修改成功",
	})
}
