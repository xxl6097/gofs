package server

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/uuxia/gofs/internal/config"
)

// guard 汇总服务端的自我防护能力。
//
// 这些检查的共同目标是：**让服务在被恶意使用时退化，而不是崩溃**。
// 文件服务器最容易被击穿的三处是磁盘、内存与文件描述符，
// 对应下面三道闸门。
type guard struct {
	cfg *config.Config

	// slots 是「同时在处理的请求数」闸门。
	// 它兜的是文件描述符与内存，不是业务限流，所以默认值给得很宽。
	slots chan struct{}
	// jobs 是「同时进行的重任务」闸门（上传 / 解压）。
	// 这两类操作会长时间占住连接并吃磁盘 IO，单独再收一道。
	jobs chan struct{}

	mu    sync.Mutex
	fails map[string]*failRecord
}

// failRecord 记录某个来源的认证失败情况。
type failRecord struct {
	count   int
	last    time.Time
	blocked time.Time
}

// newGuard 依据配置构造防护器。
func newGuard(cfg *config.Config) *guard {
	g := &guard{cfg: cfg, fails: make(map[string]*failRecord)}
	if cfg.MaxConcurrent > 0 {
		g.slots = make(chan struct{}, cfg.MaxConcurrent)
	}
	if cfg.MaxConcurrentJobs > 0 {
		g.jobs = make(chan struct{}, cfg.MaxConcurrentJobs)
	}
	return g
}

// ---------------------------------------------------------------------------
// 并发闸门
// ---------------------------------------------------------------------------

// queueWait 是取不到名额时的短暂等待窗口。
//
// 直接回 503 会让正常的突发流量（比如页面一次加载十几个资源）失败；
// 但也不能无限排队 —— 排队本身要占内存，排太久反而放大风险。
const queueWait = 3 * time.Second

// withConcurrency 限制同时处理的请求数。
func (s *Server) withConcurrency(next http.Handler) http.Handler {
	g := s.guard
	if g == nil || g.slots == nil {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		release, ok := g.acquire(r.Context(), g.slots, queueWait)
		if !ok {
			w.Header().Set("Retry-After", "1")
			http.Error(w, "503 Service Unavailable: 服务繁忙，请稍后重试", http.StatusServiceUnavailable)
			return
		}
		defer release()
		next.ServeHTTP(w, r)
	})
}

// acquireJob 为重任务（上传 / 解压）取一个名额。
//
// 与全局闸门不同，这里取不到就直接放弃：重任务排队只会让所有任务都更慢，
// 而且它们本来就可以被客户端重试。
func (s *Server) acquireJob(ctx context.Context) (func(), bool) {
	g := s.guard
	if g == nil || g.jobs == nil {
		return func() {}, true
	}
	return g.acquire(ctx, g.jobs, 0)
}

// acquire 取一个名额，返回释放函数。
// wait 为 0 表示不做等待，取不到立刻返回 false。
func (g *guard) acquire(ctx context.Context, sem chan struct{}, wait time.Duration) (func(), bool) {
	// 先试一次非阻塞获取，命中时完全不引入定时器开销。
	select {
	case sem <- struct{}{}:
		return func() { <-sem }, true
	default:
	}

	if wait <= 0 {
		return nil, false
	}

	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case sem <- struct{}{}:
		return func() { <-sem }, true
	case <-timer.C:
		return nil, false
	case <-ctx.Done():
		return nil, false
	}
}

// ---------------------------------------------------------------------------
// 跨站请求校验（CSRF）
// ---------------------------------------------------------------------------

// writeMethods 是需要做来源校验的方法。
//
// 只有在需要携带凭据才能生效的方法上校验才有意义：
// Basic 凭据由浏览器自动附带，恶意页面只要能让浏览器发一个请求，
// 就能借用户已登录的会话删文件、覆盖文件。
var writeMethods = map[string]bool{
	http.MethodPost:   true,
	http.MethodPut:    true,
	http.MethodDelete: true,
	MethodMkcol:       true,
	MethodMove:        true,
	MethodCopy:        true,
}

// withCSRFProtect 拒绝跨站发起的写请求。
//
// 判据是 Origin/Referer 与请求 Host 是否同源：
//   - 浏览器发起的跨站请求**一定**会带这两个头之一，且指向攻击者站点；
//   - curl / 脚本这类非浏览器客户端通常都不带，因此不受影响；
//   - 同源的页面请求带的是本站地址，放行。
//
// 代价是反向代理改写 Host 而保留 Origin 时可能误判，
// 这种情况可以用 --no-csrf-protect 关掉。
func (s *Server) withCSRFProtect(next http.Handler) http.Handler {
	if s.cfg.DisableCSRFProtect {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if writeMethods[r.Method] && !sameOrigin(r) {
			s.logger.Errorf("拒绝跨站写请求：%s %s from origin=%q host=%q",
				r.Method, r.URL.Path, originOf(r), r.Host)
			http.Error(w, "403 Forbidden: 跨站请求被拒绝（如需关闭请使用 --no-csrf-protect）",
				http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// originOf 返回请求声明的来源（Origin 优先，其次 Referer）。
func originOf(r *http.Request) string {
	if v := r.Header.Get("Origin"); v != "" {
		return v
	}
	return r.Header.Get("Referer")
}

// sameOrigin 判断请求是否来自本站。
// 没有来源信息时返回 true —— 那是非浏览器客户端，不是 CSRF 关心的情况。
func sameOrigin(r *http.Request) bool {
	raw := originOf(r)
	if raw == "" {
		return true
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		// 解析不出主机名的一律当作可疑来源（包括 Origin: null）。
		return false
	}
	return strings.EqualFold(u.Host, r.Host)
}

// ---------------------------------------------------------------------------
// 认证失败限速
// ---------------------------------------------------------------------------

// authBlocked 判断该来源是否正处在封禁期。
func (s *Server) authBlocked(ip string) (bool, time.Duration) {
	g := s.guard
	if g == nil || s.cfg.AuthFailLimit <= 0 {
		return false, 0
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	rec := g.fails[ip]
	if rec == nil {
		return false, 0
	}
	now := time.Now()
	if now.Before(rec.blocked) {
		return true, rec.blocked.Sub(now)
	}
	return false, 0
}

// noteAuthFailure 记录一次认证失败，达到阈值时开始封禁。
func (s *Server) noteAuthFailure(ip string) {
	g := s.guard
	if g == nil || s.cfg.AuthFailLimit <= 0 {
		return
	}
	limit := s.cfg.AuthFailLimit
	window := s.cfg.AuthFailWindow

	g.mu.Lock()
	defer g.mu.Unlock()

	// 顺手做一次惰性清理，避免被大量伪造来源撑爆内存。
	// 只在字典明显变大时才扫，正常情况下这份开销可以忽略。
	if len(g.fails) > 4096 {
		now := time.Now()
		for k, v := range g.fails {
			if now.Sub(v.last) > 2*window {
				delete(g.fails, k)
			}
		}
	}

	rec := g.fails[ip]
	now := time.Now()
	if rec == nil || now.Sub(rec.last) > window {
		rec = &failRecord{}
		g.fails[ip] = rec
	}
	rec.count++
	rec.last = now
	if rec.count >= limit {
		rec.blocked = now.Add(window)
		rec.count = 0
		s.logger.Errorf("来源 %s 认证失败达 %d 次，封禁至 %s", ip, limit, rec.blocked.Format(time.RFC3339))
	}
}

// noteAuthSuccess 在认证通过后清掉该来源的失败计数。
func (s *Server) noteAuthSuccess(ip string) {
	g := s.guard
	if g == nil || s.cfg.AuthFailLimit <= 0 {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	delete(g.fails, ip)
}

// ---------------------------------------------------------------------------
// 超时：按「有没有进展」而不是「总共花了多久」
// ---------------------------------------------------------------------------

// progressWriter 在每次成功写出后顺延写期限。
//
// 固定总时长（比如「30 分钟必须传完」）对慢速的大文件传输是灾难：
// 10 GB 在 1 MB/s 的链路上要三个小时，会被硬生生掐断。
// 而真正需要防的只是「卡住不动」—— 客户端连上却不读数据，
// 服务端阻塞在写 socket 上，却一直占着并发名额。
//
// 所以这里改成：只要还在推进（每次 Write 有字节写出）就续期，
// 连续 silence 超过阈值才判定为死连接。
type progressWriter struct {
	http.ResponseWriter
	rc      *http.ResponseController
	timeout time.Duration
}

func (p *progressWriter) Write(b []byte) (int, error) {
	n, err := p.ResponseWriter.Write(b)
	if n > 0 {
		_ = p.rc.SetWriteDeadline(time.Now().Add(p.timeout))
	}
	return n, err
}

// Flush 透传，保持 SSE 等场景可用。
func (p *progressWriter) Flush() {
	if f, ok := p.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Unwrap 让外层的 ResponseController 仍能找到底层实现。
func (p *progressWriter) Unwrap() http.ResponseWriter { return p.ResponseWriter }

// withWriteProgress 给响应加上「无进展超时」。
func withWriteProgress(w http.ResponseWriter, d time.Duration) http.ResponseWriter {
	if d <= 0 {
		return w
	}
	rc := http.NewResponseController(w)
	_ = rc.SetWriteDeadline(time.Now().Add(d))
	return &progressWriter{ResponseWriter: w, rc: rc, timeout: d}
}

// progressReader 与 progressWriter 对称：每次读到数据就顺延读期限。
type progressReader struct {
	r       io.ReadCloser
	rc      *http.ResponseController
	timeout time.Duration
}

func (p *progressReader) Read(b []byte) (int, error) {
	n, err := p.r.Read(b)
	if n > 0 {
		_ = p.rc.SetReadDeadline(time.Now().Add(p.timeout))
	}
	return n, err
}

func (p *progressReader) Close() error { return p.r.Close() }

// withReadProgress 给请求体加上「无进展超时」。
// 上传大文件时只要还在传就不会被掐断，只有真正停滞才断开。
func withReadProgress(w http.ResponseWriter, r *http.Request, d time.Duration) {
	if d <= 0 || r.Body == nil {
		return
	}
	rc := http.NewResponseController(w)
	_ = rc.SetReadDeadline(time.Now().Add(d))
	r.Body = &progressReader{r: r.Body, rc: rc, timeout: d}
}

// requestBody 给请求体套上大小上限。
//
// 返回 false 表示请求过大、已经写出响应，调用方应立即返回。
//
// 带 Content-Length 时提前拒绝，而不是等读到上限才发现：后者意味着
// 服务端要先把上限那么多字节读进来，而客户端此时还在往外发数据，
// 于是它收到的往往是连接重置（BrokenPipe / ERR_CONNECTION_RESET），
// 而不是我们精心准备的那条 413 说明。提前判断能让错误信息真正送达。
// 没有 Content-Length（chunked）的请求只能靠 MaxBytesReader 兜底。
func requestBody(w http.ResponseWriter, r *http.Request, limit int64, what string) bool {
	if limit <= 0 {
		return true
	}
	if r.ContentLength > limit {
		http.Error(w, fmt.Sprintf("413 Payload Too Large: %s 超过上限 %s",
			what, humanSize(limit)), http.StatusRequestEntityTooLarge)
		return false
	}
	r.Body = http.MaxBytesReader(w, r.Body, limit)
	return true
}

// isBodyTooLarge 判断错误是否来自 MaxBytesReader 的截断。
func isBodyTooLarge(err error) bool {
	var maxErr *http.MaxBytesError
	return errors.As(err, &maxErr)
}

// humanSize 把字节数格式化成便于阅读的形式（用于错误提示）。
func humanSize(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	units := []string{"KiB", "MiB", "GiB", "TiB", "PiB"}
	v := float64(n)
	i := -1
	for v >= unit && i < len(units)-1 {
		v /= unit
		i++
	}
	return fmt.Sprintf("%.1f %s", v, units[i])
}

// checkNameLength 校验路径中每一段的长度。
//
// 超长文件名如果直接交给文件系统，得到的是一句 ENAMETOOLONG，
// 透出去就是 500 —— 那看起来像服务端出故障，实际是请求本身不合法。
func (s *Server) checkNameLength(w http.ResponseWriter, urlPath string) bool {
	limit := s.cfg.MaxNameBytes
	if limit <= 0 {
		return true
	}
	for _, seg := range strings.Split(strings.Trim(urlPath, "/"), "/") {
		if len(seg) > limit {
			http.Error(w, fmt.Sprintf("400 Bad Request: 文件名过长（%d 字节，上限 %d 字节）",
				len(seg), limit), http.StatusBadRequest)
			return false
		}
	}
	return true
}
