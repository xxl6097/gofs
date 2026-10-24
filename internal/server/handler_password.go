package server

import (
	"errors"
	"net/http"

	"github.com/xxl6097/gofs/internal/auth"
)

// passwordChangeRequest 是账号自助改密的请求体。
//
// 字段名带 current_ 前缀而不是 password：这里要同时收「旧」和「新」两个
// 密码，叫 password / new_password 很容易让人少看一个词就把当前密码填错位置。
type passwordChangeRequest struct {
	CurrentPassword string `json:"current_password"`
	NewPassword     string `json:"new_password"`
}

// handlePasswordChange 让**已登录的账号修改自己的密码**。
//
// 与 PUT /fs/users 的区别：那条路是管理员重置任意账号（不需要旧密码，
// 但要求服务根写权限）；这里是普通账号改自己（需要旧密码，不要求任何
// 路径权限）。把两者分开，才能让只被授予 /docs 的账号也具备最基本的
// 账号自我保护能力。
//
// 改密成功后所有会话都会被 auth 层作废（包括发起这次请求的会话），
// 这里顺手清掉浏览器里的 cookie；该账号此前签发的上传密钥与分享链接
// 也一并撤销 —— 它们是不依赖密码的 bearer 凭据。
func (s *Server) handlePasswordChange(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		http.Error(w, "405 Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}
	if !s.auth.Enabled() {
		http.Error(w, "400 Bad Request: 服务未启用鉴权，没有可修改的账户密码",
			http.StatusBadRequest)
		return
	}

	// 身份只接受 Basic 头或 HttpOnly 会话：
	//   - 查询串里的口令会进访问日志、浏览器历史与 Referer，不适合改密；
	//   - 分享令牌不是账号身份，改不了任何人的密码（ResolveSession 也不会认它）。
	user, pass, hasCred, class := credentialsWithSource(r)
	if hasCred && class != authClassBasic {
		http.Error(w, "400 Bad Request: 修改密码请使用 Authorization 头或已登录会话",
			http.StatusBadRequest)
		return
	}
	if hasCred {
		// Basic 凭据本身也要先验过：否则只要知道一个账号名，就能从
		// 「来自启动参数」这类错误里区分出它存不存在，甚至绕过失败计数。
		// 先验口令，失败后才由 noteAuthFailureAndReply 决定 401/429。
		ip := s.clientIP(r)
		if user == "" || !s.auth.Verify(user, pass) {
			if s.noteAuthFailureAndReply(w, ip, class, user) {
				return
			}
			if shouldChallenge(r) {
				w.Header().Set("WWW-Authenticate", `Basic realm="gofs", charset="UTF-8"`)
			}
			http.Error(w, "401 Unauthorized", http.StatusUnauthorized)
			return
		}
		s.noteVerifiedUser(r, user)
		s.noteAuthSuccess(ip, class, user)
	} else {
		u, ok := s.auth.ResolveSession(s.sessionToken(r))
		if !ok || u == "" {
			if shouldChallenge(r) {
				w.Header().Set("WWW-Authenticate", `Basic realm="gofs", charset="UTF-8"`)
			}
			http.Error(w, "401 Unauthorized", http.StatusUnauthorized)
			return
		}
		user = u
	}

	// 这条路径还会验证一次当前密码，必须和登录共用一套失败计数；
	// 单独一档，避免改密失败把登录/匿名访问连坐节流。
	// 锁定状态不在这里提前拒绝：先让 ChangePassword 验当前密码，
	// 只有验错才回 401/429。
	ip := s.clientIP(r)

	var req passwordChangeRequest
	if !s.decodeJSON(w, r, &req) {
		return
	}

	err := s.auth.ChangePassword(user, req.CurrentPassword, req.NewPassword)
	if err != nil {
		if errors.Is(err, auth.ErrPasswordMismatch) {
			if s.noteAuthFailureAndReply(w, ip, authClassPassword, user) {
				return
			}
			http.Error(w, "403 Forbidden: "+err.Error(), http.StatusForbidden)
			return
		}
		s.userError(w, err)
		return
	}

	s.noteVerifiedUser(r, user)
	s.noteAuthSuccess(ip, authClassPassword, user)
	// 改密后旧密码、旧会话、旧密钥都必须失效。会话由 auth 层断掉，
	// 密钥/分享是 bearer 凭据，需要在这一层按创建者撤销。
	s.revokeUserKeys(user, "修改密码")
	s.clearSessionCookie(w, r)
	s.logger.Infof("账号 %q 修改了自己的密码，相关会话与密钥已作废", user)
	w.WriteHeader(http.StatusNoContent)
}
