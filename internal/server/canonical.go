package server

import (
	"context"
	"net/http"
	"os"
	"sync"

	"github.com/xxl6097/gofs/internal/fsutil"
)

// canonicalCacheKey 把「本次请求的路径规范化缓存」放进 context。
//
// 缓存只在单个请求内有效：请求外缓存会在改名/改大小写后留下过期的 ACL
// 判定；请求内同一路径会被 authorize / checkPerm / 真实路径复核多次访问，
// 缓存能避免对同一目录反复 ReadDir。
type canonicalCacheKey struct{}

type dirListing struct {
	entries []os.DirEntry
	err     error
}

const canonicalDirCacheMax = 256

type canonicalCache struct {
	mu   sync.Mutex
	m    map[string]string
	dirs map[string]dirListing
	// paths 保存“授权时看到的真实文件/目录快照”，打开文件时用来防止
	// 检查与 open 之间的软链/目录替换（TOCTOU）。
	paths map[string]*authorizedPath
	// authUser 是本次请求中**真的验过密码**的账号名。
	// 访问日志只应记录它，而不是客户端随手塞进 Authorization 头的用户名。
	authUser string
}

func (c *canonicalCache) setAuthUser(user string) {
	c.mu.Lock()
	c.authUser = user
	c.mu.Unlock()
}

func (c *canonicalCache) getAuthUser() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.authUser
}

// noteVerifiedUser 记录本次请求已验证的账号名（供访问日志使用）。
func (s *Server) noteVerifiedUser(r *http.Request, user string) {
	if user == "" {
		return
	}
	if c, ok := r.Context().Value(canonicalCacheKey{}).(*canonicalCache); ok {
		c.setAuthUser(user)
	}
}

// noteAuthorizedPath 记录一次授权时看到的真实路径快照。
func (s *Server) noteAuthorizedPath(r *http.Request, urlPath, real string) {
	if urlPath == "" || real == "" {
		return
	}
	if c, ok := r.Context().Value(canonicalCacheKey{}).(*canonicalCache); ok {
		c.mu.Lock()
		// 已存在键必须允许覆盖：替换/备份上传会先把原目标移走，再写入新文件；
		// 如果此时因为缓存已满而保留旧 inode 快照，secureOpenWrite 会误判成
		// “路径在授权后发生变化”，把一次合法的上传拒成 409。
		if _, exists := c.paths[urlPath]; exists || len(c.paths) < 256 {
			c.paths[urlPath] = snapshotAuthorized(real)
		}
		c.mu.Unlock()
	}
}

// authorizedPathFor 取出某个 URL 对应的授权快照；没有则返回 nil。
func (s *Server) authorizedPathFor(r *http.Request, urlPath string) *authorizedPath {
	if urlPath == "" {
		return nil
	}
	if c, ok := r.Context().Value(canonicalCacheKey{}).(*canonicalCache); ok {
		c.mu.Lock()
		defer c.mu.Unlock()
		return c.paths[urlPath]
	}
	return nil
}

// verifiedUser 返回本次请求已验证的账号名；没有则为空串。
func (s *Server) verifiedUser(r *http.Request) string {
	if c, ok := r.Context().Value(canonicalCacheKey{}).(*canonicalCache); ok {
		return c.getAuthUser()
	}
	return ""
}

// ReadDir 实现 fsutil.DirLister：同一请求内复用目录项。
func (c *canonicalCache) ReadDir(dir string) ([]os.DirEntry, error) {
	c.mu.Lock()
	if v, ok := c.dirs[dir]; ok {
		c.mu.Unlock()
		return v.entries, v.err
	}
	c.mu.Unlock()

	entries, err := os.ReadDir(dir)

	c.mu.Lock()
	if len(c.dirs) < canonicalDirCacheMax {
		c.dirs[dir] = dirListing{entries: entries, err: err}
	}
	c.mu.Unlock()
	return entries, err
}

// withCanonicalCache 为每个请求准备一个规范化缓存。
func (s *Server) withCanonicalCache(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c := &canonicalCache{
			m:     make(map[string]string),
			dirs:  make(map[string]dirListing),
			paths: make(map[string]*authorizedPath),
		}
		next.ServeHTTP(w, r.WithContext(
			context.WithValue(r.Context(), canonicalCacheKey{}, c)))
	})
}

// canonicalPath 返回与磁盘真实名字对齐的 URL 路径。
//
// ok=false 表示规范化失败（典型是目录可执行但不可读、或 fd 耗尽）。
// 调用方必须 fail-closed：在大小写不敏感文件系统上，回退到请求原路径会让
// ACL 按错误大小写匹配到更宽的规则，而底层打开的是另一条更严格规则保护
// 的文件 —— 那是一条可稳定利用的越权路径。
func (s *Server) canonicalPath(r *http.Request, p string) (canon string, ok bool) {
	clean := fsutil.CleanURLPath(p)
	c, _ := r.Context().Value(canonicalCacheKey{}).(*canonicalCache)
	if c != nil {
		c.mu.Lock()
		v, hit := c.m[clean]
		c.mu.Unlock()
		if hit {
			return v, true
		}
	}

	var err error
	if c != nil {
		canon, err = s.res.CanonicalURLPathWith(clean, c)
	} else {
		canon, err = s.res.CanonicalURLPath(clean)
	}
	if err != nil || canon == "" {
		return "", false
	}
	if c != nil {
		c.mu.Lock()
		if len(c.m) < 2048 {
			c.m[clean] = canon
		}
		c.mu.Unlock()
	}
	return canon, true
}
