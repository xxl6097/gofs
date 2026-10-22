package server

import (
	"net/http"

	"github.com/xxl6097/gofs/internal/auth"
	"github.com/xxl6097/gofs/internal/fsutil"
)

// realURLOf 把磁盘路径解析为真实路径，再映射回服务 URL。
//
// ok=false 表示真实路径不在任何服务根下（例如 --allow-symlink 指向根外，
// 或路径已被删除）。root 内的软链会返回它**目标**的 URL，因此调用方可以
// 用目标路径再跑一次 ACL。
func (s *Server) realURLOf(abs string) (string, bool) {
	real, err := fsutil.ResolveReal(abs)
	if err != nil {
		return "", false
	}
	rel, err := s.res.Rel(real)
	if err != nil {
		return "", false
	}
	return rel, true
}

// realPermOf 计算磁盘目标真实路径的 URL 与权限。
func (s *Server) realPermOf(r *http.Request, abs string) (url string, perm auth.Permission, within bool) {
	rel, ok := s.realURLOf(abs)
	if !ok {
		return "", auth.PermNone, false
	}
	return rel, s.checkPerm(r, rel), true
}

// realPermAllowed 与 requireRealPerm 相同，但不写响应，供遍历辅助函数使用。
//
// 根外目标：分享/上传密钥没有可表达的路径规则，一律拒绝；普通账号由
// --allow-symlink 显式决定（该开关的定义就是允许链接指向根外）。
func (s *Server) realPermAllowed(r *http.Request, abs string, need auth.Permission) bool {
	_, perm, within := s.realPermOf(r, abs)
	if !within {
		return s.cfg.AllowSymlink && shareTokenOf(r) == "" && uploadKeyToken(r) == ""
	}
	return perm >= need
}

// requireRealPerm 在请求路径权限之外，再按磁盘真实路径复核一次。
// 这是软链绕过 ACL 的修复点：请求路径可以命中较宽的规则，链接目标却可能
// 落在另一条更严格的规则下。
func (s *Server) requireRealPerm(w http.ResponseWriter, r *http.Request, abs string, need auth.Permission) bool {
	if s.realPermAllowed(r, abs, need) {
		return true
	}
	http.Error(w, "403 Forbidden: 没有足够的权限", http.StatusForbidden)
	return false
}
