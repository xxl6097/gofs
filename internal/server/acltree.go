package server

import (
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"path"
	"path/filepath"

	"github.com/xxl6097/gofs/internal/auth"
)

// walkTree 遍历 abs 下的所有后代（不含 abs 本身），把每个后代的磁盘路径与
// URL 路径交给 fn。fn 返回非空字符串时中止并作为失败原因。
//
// WalkDir 不跟随软链：软链条目本身会被检查，但它的目标不参与遍历 ——
// 删除/移动软链操作的是链接，不是目标内容。
func (s *Server) walkTree(abs string, fn func(p, rel string) string) (string, bool) {
	var reason string
	err := filepath.WalkDir(abs, func(p string, _ fs.DirEntry, werr error) error {
		if werr != nil {
			reason = fmt.Sprintf("%s: %v", p, werr)
			return werr
		}
		if p == abs {
			return nil
		}
		rel, rerr := s.res.Rel(p)
		if rerr != nil {
			reason = p
			return rerr
		}
		if bad := fn(p, rel); bad != "" {
			reason = bad
			return errors.New(bad)
		}
		return nil
	})
	if err != nil {
		if reason == "" {
			reason = err.Error()
		}
		return reason, false
	}
	return "", true
}

// requireDeleteTree 用于递归删除 / 覆盖目标目录：每个后代都必须有 rw。
//
// 只检查目录本身是不够的：`/docs:rw` + `/docs/secret:r` 这种
// 「目录可写、子项只读」的规则不能被一次 recursive 删除绕过。
func (s *Server) requireDeleteTree(r *http.Request, abs string) (string, bool) {
	return s.walkTree(abs, func(_, rel string) string {
		// WalkDir 返回的是磁盘真实名字，无需再逐段做大小写规范化。
		if s.checkPermExact(r, rel) != auth.PermReadWrite {
			return rel
		}
		return ""
	})
}

// requireMoveTree 用于移动 / 重命名目录：每个后代在新路径上的权限
// **不得高于**旧路径，否则可以把只读子项搬到一个规则更宽的位置再改写。
func (s *Server) requireMoveTree(r *http.Request, srcAbs, srcURL, dstURL string) (string, bool) {
	return s.walkTree(srcAbs, func(p, oldURL string) string {
		relPart, err := filepath.Rel(srcAbs, p)
		if err != nil {
			return oldURL
		}
		newURL := path.Join(dstURL, filepath.ToSlash(relPart))
		oldPerm := s.checkPermExact(r, oldURL)
		if oldPerm == auth.PermNone {
			return oldURL
		}
		if newPerm := s.checkPerm(r, newURL); newPerm > oldPerm {
			return fmt.Sprintf("%s（移动到 %s 后权限会提升）", oldURL, newURL)
		}
		return ""
	})
}
