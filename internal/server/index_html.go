package server

import (
	"context"
	"fmt"
	"html"
	"net/http"
	"net/url"
	"strings"

	"github.com/xxl6097/gofs/internal/fsutil"
)

// renderLinkIndex 输出一份**服务端渲染**的目录列表，供 wget / curl 这类
// 命令行工具遍历。
//
// 为什么必须有这个：原来的 HTML 视图是把内嵌的 SPA 外壳原样返回，
// 里面的文件名是前端用 JS 画出来的 —— 服务端吐出的 HTML 里**一个 <a> 都没有**。
// 浏览器没问题，但 `wget -r` 拿到的就是那个空壳，遍历不下去，
// 分享出去的目录对命令行工具等于不可用。
//
// 判据是 `Accept` 里有没有 text/html：浏览器一定带，wget / curl 发的是 `*/*`。
// 这和已有的「?json / ?simple」是同一个思路 —— 由客户端声明它想要什么。
//
// shareToken 非空时会把令牌拼进每个链接 —— 但**只在查询串形式**下需要。
// 走路径形式（/__share__/<token>/docs/）时链接就是普通的相对路径，
// 令牌已经在前缀里，wget 拼出来的文件名也才是干净的 `readme.txt`。
// 这一点很重要：带查询串的链接会让 wget 把文件名存成
// `readme.txt?share=gofs_xxx`。
func (s *Server) renderLinkIndex(w http.ResponseWriter, r *http.Request,
	listing *fsutil.Listing, shareToken string) {

	w.Header().Set("Content-Type", "text/html; charset=utf-8")

	// 链接上要不要挂 ?share=：
	//   - 路径形式下不用（前缀已经带着令牌了）；
	//   - 查询串形式下必须挂，否则 wget 跟着相对链接走就会 401。
	suffix := ""
	if shareToken != "" && !isSharePathRequest(r) {
		suffix = "?share=" + url.QueryEscape(shareToken)
	}

	dirPath := listing.Path
	if !strings.HasSuffix(dirPath, "/") {
		dirPath += "/"
	}

	var b strings.Builder
	b.WriteString("<!DOCTYPE html>\n<html><head><meta charset=\"utf-8\">\n")
	fmt.Fprintf(&b, "<title>Index of %s</title>\n", html.EscapeString(dirPath))
	b.WriteString("<style>body{font:14px/1.6 monospace;margin:24px}" +
		"a{text-decoration:none}hr{border:0;border-top:1px solid #ccc}</style>\n")
	b.WriteString("</head><body>\n")
	fmt.Fprintf(&b, "<h1>Index of %s</h1>\n<hr>\n<pre>\n", html.EscapeString(dirPath))

	// 上级目录。根目录就不给了。
	if dirPath != "/" {
		fmt.Fprintf(&b, "<a href=\"../%s\">../</a>\n", suffix)
	}

	for _, e := range listing.Entries {
		// 每一段单独转义，保留 / 作为分隔符 —— 否则中文目录名会被整段编码成
		// %XX，wget 拉下来的文件名就是乱码。
		href := escapePathSegment(e.Name)
		if e.IsDir {
			href += "/"
		}
		label := e.Name
		if e.IsDir {
			label += "/"
		}
		fmt.Fprintf(&b, "<a href=\"%s%s\">%s</a>\n", href, suffix, html.EscapeString(label))
	}

	b.WriteString("</pre>\n<hr>\n</body></html>\n")
	fmt.Fprint(w, b.String())
}

// isToolClient 判断这是不是一个「命令行工具」而不是浏览器。
//
// 判据：Accept **存在**且不含 text/html。wget / curl 默认发 `*/*`，会被认出来。
//
// 「没有 Accept 头」刻意不算工具：那是少数客户端（和测试）的行为，
// 按浏览器处理更保守 —— 保持它们原有的输出不变，改动只落在明确声明
// 不要 HTML 的客户端上。
func isToolClient(r *http.Request) bool {
	accept := r.Header.Get("Accept")
	if accept == "" {
		return false
	}
	return !strings.Contains(accept, "text/html")
}

// escapePathSegment 逐段百分号编码，保留 "/" 作为分隔符。
func escapePathSegment(name string) string {
	parts := strings.Split(name, "/")
	for i, p := range parts {
		parts[i] = url.PathEscape(p)
	}
	return strings.Join(parts, "/")
}

// ---------------------------------------------------------------------------
// 路径形式的分享地址
//
// 除了 `?share=<token>`，还支持 `/__share__/<token>/<原路径>`。
//
// 为什么要有第二种：wget 会把查询串拼进文件名，递归下载下来的是
// `readme.txt?share=gofs_xxx` 这种名字，基本没法用。路径形式下所有链接
// 都是普通相对路径，文件名干净，令牌也天然跟着每一层目录走下去。
//
// 令牌走 context 而不是请求头：头是客户端可控的，能被伪造。
// ---------------------------------------------------------------------------

// sharePathPrefix 是路径形式分享地址的前缀。
const sharePathPrefix = "/__share__/"

type shareCtxKey struct{}

// withSharePath 把 /__share__/<token>/<rest> 改写成 /<rest>，
// 并把令牌放进请求 context 供 shareTokenOf 取用。
//
// 只在路径前缀**刚好匹配**时改写；其余请求原样透传。
func (s *Server) withSharePath(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		if !strings.HasPrefix(p, sharePathPrefix) {
			next.ServeHTTP(w, r)
			return
		}
		rest := p[len(sharePathPrefix):]
		i := strings.IndexByte(rest, '/')
		if i < 0 {
			http.NotFound(w, r)
			return
		}
		token, tail := rest[:i], rest[i:]

		r2 := r.Clone(r.Context())
		r2.URL = cloneURL(r.URL)
		r2.URL.Path = tail
		// 令牌经 context 传给下游，客户端伪造不了。
		r2 = r2.WithContext(context.WithValue(r2.Context(), shareCtxKey{}, token))
		next.ServeHTTP(w, r2)
	})
}

// cloneURL 复制一份 URL，避免改动原始请求。
func cloneURL(u *url.URL) *url.URL {
	c := *u
	return &c
}

// isSharePathRequest 判断当前请求是不是走路径形式的分享地址。
func isSharePathRequest(r *http.Request) bool {
	_, ok := r.Context().Value(shareCtxKey{}).(string)
	return ok
}
