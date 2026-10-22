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
			m:    make(map[string]string),
			dirs: make(map[string]dirListing),
		}
		next.ServeHTTP(w, r.WithContext(
			context.WithValue(r.Context(), canonicalCacheKey{}, c)))
	})
}

// canonicalPath 返回与磁盘真实名字对齐的 URL 路径。
//
// 失败时回退到清理后的原路径：后续 Resolve/真实路径复核仍会拒绝真正的
// 越界，不会因为规范化失败而放行。
func (s *Server) canonicalPath(r *http.Request, p string) string {
	clean := fsutil.CleanURLPath(p)
	c, _ := r.Context().Value(canonicalCacheKey{}).(*canonicalCache)
	if c != nil {
		c.mu.Lock()
		v, ok := c.m[clean]
		c.mu.Unlock()
		if ok {
			return v
		}
	}

	var (
		canon string
		err   error
	)
	if c != nil {
		canon, err = s.res.CanonicalURLPathWith(clean, c)
	} else {
		canon, err = s.res.CanonicalURLPath(clean)
	}
	if err != nil || canon == "" {
		canon = clean
	}
	if c != nil {
		c.mu.Lock()
		if len(c.m) < 2048 {
			c.m[clean] = canon
		}
		c.mu.Unlock()
	}
	return canon
}
