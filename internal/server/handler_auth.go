package server

import (
	"net/http"
	"time"

	"github.com/xxl6097/gofs/internal/auth"
	"github.com/xxl6097/gofs/internal/fsutil"
)

// handleAuth 校验 Basic 凭据，供前端登录框使用。
//
// 与普通接口有三点不同：
//   - 不返回 WWW-Authenticate 挑战，否则浏览器会在页面上弹出脱离
//     应用上下文的原生登录框，打断应用自己的登录流程；
//   - 校验只看「账号密码是否有效」（auth.Verify），不要求对某个路径有权限，
//     否则一个只被授予 /docs 权限的账号会因为对根目录无权限而登录失败；
//   - 失败信息以 JSON 返回，方便前端直接展示。
func (s *Server) handleAuth(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodPost {
		w.Header().Set("Allow", "GET, POST")
		http.Error(w, "405 Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}

	// 未启用鉴权时前端不必显示登录入口。
	if !s.auth.Enabled() {
		s.writeJSON(w, map[string]any{
			"auth_on":       false,
			"authenticated": true,
			"anonymous":     true,
			"perms":         s.buildPerms(permResult{Perm: auth.PermReadWrite}),
		})
		return
	}

	user, pass, ok := credentials(r)
	if !ok || user == "" {
		s.writeJSONStatus(w, http.StatusUnauthorized, map[string]any{
			"message": "请输入用户名与密码",
		})
		return
	}
	if !s.auth.Verify(user, pass) {
		s.logger.Errorf("登录失败：账号 %q 凭据不正确", user)
		s.writeJSONStatus(w, http.StatusUnauthorized, map[string]any{
			"message": "用户名或密码不正确",
		})
		return
	}

	// 该账号的权限用于决定页面按钮显隐。
	// 默认按根目录计算，但对根目录无权限时退化为只读，避免整页按钮全灭。
	// 调用方可以带 ?path= 指定自己实际所在的目录：一个只被授予 /docs 权限的
	// 账号站在 /docs 里时，理应拿到完整的按钮，而不是被当成只读。
	perm := s.auth.Lookup("/", user, pass, true)
	if p := r.URL.Query().Get("path"); p != "" && !fsutil.HasParentSegment(p) {
		if scoped := s.auth.Lookup(p, user, pass, true); scoped != auth.PermNone {
			perm = scoped
		}
	}
	if perm == auth.PermNone {
		perm = auth.PermRead
	}
	s.logger.Infof("登录成功：%s", user)

	// 除了权限，还要把「未登录外壳里刻意没下发」的配置一并补齐，
	// 否则前端登录后仍然不知道上传归档、在线编辑、密钥等能力是否可用。
	//
	// allow_settings 要按**服务根**的权限算，而不是调用方所在的路径：
	// 只有能读写整个服务的账号才该看到「服务设置」入口。
	admin := s.auth.Lookup("/", user, pass, true) == auth.PermReadWrite
	reply := map[string]any{
		"auth_on":         true,
		"authenticated":   true,
		"user":            user,
		"anonymous":       false,
		"perms":           s.buildPerms(permResult{Perm: perm, User: user, Authenticated: true}),
		"upload_dated":    s.cfg.UploadDated(),
		"allow_keys":      s.cfg.AllowKeys && perm == auth.PermReadWrite,
		"edit_max_size":   s.cfg.EditMaxSize,
		"upload_max_size": s.settings.UploadMaxSize(),
		"allow_settings":  admin,
		"allow_users":     s.cfg.AllowUserManage && admin,
	}
	if s.cfg.UploadDated() {
		reply["upload_date_dir"] = s.cfg.UploadDateDir(time.Now())
	}
	s.writeJSON(w, reply)
}
