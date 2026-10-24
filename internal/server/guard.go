package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/xxl6097/gofs/internal/config"
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
	count int
	last  time.Time
	// throttledUntil 是「需要减速」的截止时间。注意它**不是**「拒绝服务」
	// 的截止时间：进入节流窗口后，每次尝试仍会验证密码，只是先等一小段
	// 时间。这样攻击者没法用错误口令把合法用户锁在门外。
	throttledUntil time.Time
}

// authThrottleStep 是节流窗口内两次口令校验之间的最小间隔。
//
// 不把整个统计窗口当成封禁：那样只要知道用户名，随便编几次错误口令就能
// 让 admin 在 5 分钟内连正确密码都登不上（账号锁定 DoS）。改成「每次先等
// 半秒再验」之后，暴力破解被压到每秒约 2 次，而合法用户带着正确密码
// 最多等半秒就能登录 —— 限速还在，锁死没有了。
const authThrottleStep = 500 * time.Millisecond

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

// apiBodyReadTimeout 是 JSON 等小型 API 请求体的绝对读取时限。
//
// 上传/解压的文件体不走它（那类请求用 --upload-read-timeout 的
// 「无进展」语义）；这里只约束「几 KB 的 JSON 却永远发不完」这种
// 占着并发名额的慢速请求。
const apiBodyReadTimeout = 30 * time.Second

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
// 传输安全：可选的「强制 HTTPS」
// ---------------------------------------------------------------------------

// withSecureAuth 在关闭明文认证时，拒绝明文连接上的账号密码并忽略会话。
//
// Basic 只是 Base64，同网段抓包就能还原密码；会话 Cookie 是 bearer 凭据，
// 抓包后可以原样复用。默认为了 HTTP 开箱即用是允许的；需要收紧时用
// --allow-insecure-auth=false 关闭，此时只在以下情况放行（见 secureTransport）：
// 直接 HTTPS、可信代理声明 HTTPS、本机回环/Unix socket。
//
// 关闭明文认证后，其余情况：
//   - 带 Basic / 查询串口令 → 403，并提示改用 HTTPS；
//   - 只带会话 cookie → 不认它（sessionToken 返回空），顺手清掉，继续按匿名处理。
//     不直接 403 是为了让浏览器还能打开登录页；否则带着一张旧 cookie
//     的用户会看到一张错误页，连改配置的入口都没有。
func (s *Server) withSecureAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.auth.Enabled() || s.secureTransport(r) {
			next.ServeHTTP(w, r)
			return
		}
		if hasPasswordCredentials(r) {
			w.Header().Set("Cache-Control", "no-store")
			msg := "账号密码不能通过明文 HTTP 传输：请改用 HTTPS，" +
				"或用 --allow-insecure-auth（默认开启）允许明文认证"
			if r.Header.Get("X-Gofs-Ajax") == "1" {
				s.writeJSONStatus(w, http.StatusForbidden, map[string]any{"message": msg})
				return
			}
			http.Error(w, "403 Forbidden: "+msg, http.StatusForbidden)
			return
		}
		if s.sessionTokenRaw(r) != "" {
			s.clearSessionCookie(w, r)
		}
		next.ServeHTTP(w, r)
	})
}

// hasPasswordCredentials 判断请求是否携带了可被直接还原/重放的口令。
func hasPasswordCredentials(r *http.Request) bool {
	if _, _, ok := r.BasicAuth(); ok {
		return true
	}
	q := r.URL.Query()
	return q.Has("user") || q.Has("pass")
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
		if writeMethods[r.Method] && !s.sameOrigin(r) {
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
//
// 除主机外还比较协议：直接跑 HTTPS 时，来自同主机 HTTP 页面的写请求
// 也应拒绝。代理终止 TLS 的部署只有在配置了可信代理、且代理给了
// X-Forwarded-Proto 时才能确定外部协议；确定不了就不做协议比较，
// 避免把正常的 HTTPS 写入误杀。
func (s *Server) sameOrigin(r *http.Request) bool {
	raw := originOf(r)
	if raw == "" {
		return true
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		// 解析不出主机名的一律当作可疑来源（包括 Origin: null）。
		return false
	}
	if !strings.EqualFold(u.Host, r.Host) {
		return false
	}
	if scheme := s.externalScheme(r); scheme != "" && !strings.EqualFold(u.Scheme, scheme) {
		return false
	}
	return true
}

// externalScheme 返回客户端实际使用的协议；无法确定时返回空串。
func (s *Server) externalScheme(r *http.Request) string {
	if r.TLS != nil {
		return "https"
	}
	if !trustedPeer(r, s.trustedProxies) {
		return ""
	}
	proto := strings.ToLower(strings.TrimSpace(r.Header.Get("X-Forwarded-Proto")))
	// 多层代理时常见 "https,http"：最左侧才是客户端看到的那一段。
	if i := strings.IndexByte(proto, ','); i >= 0 {
		proto = strings.TrimSpace(proto[:i])
	}
	switch proto {
	case "https":
		return "https"
	case "http":
		return "http"
	}
	return ""
}

// ---------------------------------------------------------------------------
// 认证失败限速
// ---------------------------------------------------------------------------

// maxAuthFailEntries 是失败记录表的硬上限。
//
// 键里带用户名之后，攻击者可以用大量随机用户名把表撑大；惰性清理只清
// 「已经过期」的项，挡不住持续注入的新键。超过上限时先清过期项，仍超限
// 则丢弃一半 —— 这是可用性与内存之间的取舍，限速本来就是尽力而为。
const maxAuthFailEntries = 65536

// 凭据类别。失败计数必须按类别隔离：
//
//   - Basic / 查询串认证可以被无限次尝试，需要按「来源+账号」限速；
//   - 会话 Cookie 是 256 位随机的服务端 token，客户端伪造不出有效值，
//     无效 Cookie 不构成口令猜测，不能让它污染会话/匿名访问的限速桶；
//   - 匿名请求更不该被别人的失败计数连坐。
//
// 早先三者共用空用户名键，结果 10 个坏 Cookie 就能把同一 IP 上所有
// 有效会话和匿名访问锁死（反向代理未配置可信代理时是整站）。
const (
	authClassBasic   = "basic"
	authClassQuery   = "query"
	authClassSession = "session"
	authClassAnon    = "anon"
	// authClassPassword 是「已登录账号在改密时提交当前密码」这条路径。
	//
	// 单列一档而不是并进 basic：会话本身可能是被窃取的，攻击者拿到会话后
	// 可以在这里尝试猜当前密码。它需要限速，但不该把同一 IP 的正常登录
	// 一起封掉 —— 两边的失败计数互不连坐。
	authClassPassword = "password"
)

// maxAuthUserLogBytes 限制写入日志的用户名长度。
//
// 请求头默认允许 64 KiB，用户名可以是几乎任意长；原样写日志会让
// 匿名请求用一条 60 KiB 的“用户名”撑大日志文件/日志收集链路。
const maxAuthUserLogBytes = 128

// truncateAuthUser 把用户名按 UTF-8 边界截断到可安全写日志的长度。
func truncateAuthUser(user string) string {
	if len(user) <= maxAuthUserLogBytes {
		return user
	}
	cut := maxAuthUserLogBytes
	for cut > 0 && !utf8.RuneStart(user[cut]) {
		cut--
	}
	return user[:cut] + "…"
}

// authFailKey 把「来源 + 凭据类别 + 尝试的账号」摘要成固定长度的键。
//
// 用 SHA-256 而不是直接拼接，原因有二：
//   - 用户名长度由请求头决定，直接做 map 键会让少量请求就占掉数 GB 内存；
//   - 固定长度的键也不会因为用户名里的控制字符污染日志/诊断。
//
// ⚠️ 不能只用 IP：否则任何持有合法账号的人，只要在两次错误猜测之间
// 穿插一次自己的成功请求，就能把整个 IP 的失败计数清零，对别人的账号
// 做无限次口令猜测（BCP 的经典绕过）。成功只能清掉**本账号**的记录。
func authFailKey(ip, class, user string) string {
	sum := sha256.Sum256([]byte(ip + "\x00" + class + "\x00" + strings.TrimSpace(user)))
	return hex.EncodeToString(sum[:])
}

// authThrottled 判断该来源+类别+账号是否正处在节流窗口。
func (s *Server) authThrottled(ip, class, user string) (bool, time.Duration) {
	g := s.guard
	if g == nil || s.cfg.AuthFailLimit <= 0 {
		return false, 0
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	rec := g.fails[authFailKey(ip, class, user)]
	if rec == nil {
		return false, 0
	}
	now := time.Now()
	if now.Before(rec.throttledUntil) {
		return true, rec.throttledUntil.Sub(now)
	}
	return false, 0
}

// waitAuthThrottle 在节流窗口内等一小段时间再继续验证密码。
//
// ⚠️ 这里是「等待」而不是「拒绝」：拒绝会让攻击者仅凭用户名就能把
// 合法账号锁死。等待把每次猜测压慢，但正确密码永远能走到校验。
// 返回 false 表示客户端在等待期间断开了。
func (s *Server) waitAuthThrottle(r *http.Request, remain time.Duration) bool {
	wait := authThrottleStep
	if remain < wait {
		wait = remain
	}
	if wait <= 0 {
		return true
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-timer.C:
		return true
	case <-r.Context().Done():
		return false
	}
}

// throttledReply 在失败计数已进入节流窗口时写 429，返回 true 表示已写响应。
//
// 调用它之前密码**已经验过且失败**，所以正确密码不会走到这里 ——
// 这是「限速但不锁死」的关键：429 只影响猜错的人，不影响拿对密码的人。
func (s *Server) throttledReply(w http.ResponseWriter, ip, class, user string) bool {
	if throttled, _ := s.authThrottled(ip, class, user); !throttled {
		return false
	}
	// 实际节流步长是 500ms，按秒向上取整就是 1。
	w.Header().Set("Retry-After", "1")
	http.Error(w, "429 Too Many Requests: 认证失败次数过多，请稍后再试",
		http.StatusTooManyRequests)
	return true
}

// noteAuthFailureAndReply 记录一次失败；如果**这次之前**就已经在节流窗口里，
// 写 429 并返回 true。
//
// 达到阈值的那一次本身仍按普通认证失败处理（401），从下一次开始才回 429：
// 这样「第 N 次失败」的语义与以前一致，也不会让边界上的那一次看起来像
// 被额外惩罚。密码正确时不要调用。
func (s *Server) noteAuthFailureAndReply(w http.ResponseWriter, ip, class, user string) bool {
	already, _ := s.authThrottled(ip, class, user)
	s.noteAuthFailure(ip, class, user)
	if !already {
		return false
	}
	return s.throttledReply(w, ip, class, user)
}

// noteAuthFailure 记录一次认证失败，达到阈值时进入节流窗口。
//
// class/user 是本次**实际尝试的凭据类别与账号**（没有凭据时不应调用）。
// 计数按「来源 + 类别 + 账号」隔离，成功登录不会清掉别人账号的失败记录，
// 也不会因为无效 Cookie/匿名请求连坐有效会话。
func (s *Server) noteAuthFailure(ip, class, user string) {
	g := s.guard
	if g == nil || s.cfg.AuthFailLimit <= 0 {
		return
	}
	limit := s.cfg.AuthFailLimit
	window := s.cfg.AuthFailWindow
	key := authFailKey(ip, class, user)

	g.mu.Lock()
	defer g.mu.Unlock()

	// 顺手做一次惰性清理，避免被大量伪造来源/用户名撑爆内存。
	// 只在字典明显变大时才扫，正常情况下这份开销可以忽略。
	if len(g.fails) > 4096 {
		now := time.Now()
		for k, v := range g.fails {
			if now.Sub(v.last) > 2*window {
				delete(g.fails, k)
			}
		}
	}
	// 硬上限：持续注入新键时，仅靠「清过期」挡不住内存增长。
	// 淘汰时跳过仍在节流期的记录，否则攻击者可以故意灌满表，
	// 把针对自己的节流记录挤掉后继续猜口令。
	if len(g.fails) >= maxAuthFailEntries {
		n := 0
		now := time.Now()
		for k, v := range g.fails {
			if n >= maxAuthFailEntries/2 {
				break
			}
			if v.throttledUntil.After(now) {
				continue
			}
			delete(g.fails, k)
			n++
		}
		if len(g.fails) >= maxAuthFailEntries {
			// 表里全是仍在生效的节流记录：宁可本次不记新键，
			// 也不要让 map 无界增长。已有的记录仍会按时过期。
			return
		}
	}

	rec := g.fails[key]
	now := time.Now()
	if rec == nil || now.Sub(rec.last) > window {
		rec = &failRecord{}
		g.fails[key] = rec
	}
	rec.count++
	rec.last = now
	if rec.count >= limit {
		rec.throttledUntil = now.Add(window)
		rec.count = 0
		s.logger.Errorf("来源 %s（%s 账号 %q）认证失败达 %d 次，进入 %s 节流窗口（正确密码仍可在等待后登录）",
			ip, class, truncateAuthUser(user), limit, window)
	}
}

// noteAuthSuccess 在认证通过后清掉**该来源+类别+账号**的失败计数。
//
// 绝不能只按 IP 清：那会让任何持有合法账号的人通过穿插成功请求
// 把针对别的账号的失败记录一并抹掉，限速形同虚设。
func (s *Server) noteAuthSuccess(ip, class, user string) {
	g := s.guard
	if g == nil || s.cfg.AuthFailLimit <= 0 {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	delete(g.fails, authFailKey(ip, class, user))
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
//
// 返回的 cleanup 会把写期限清掉：连接可能被 keep-alive 复用，
// 留着一条未来的 deadline 会影响下一个请求。
func withWriteProgress(w http.ResponseWriter, d time.Duration) (http.ResponseWriter, func()) {
	if d <= 0 {
		return w, func() {}
	}
	rc := http.NewResponseController(w)
	_ = rc.SetWriteDeadline(time.Now().Add(d))
	return &progressWriter{ResponseWriter: w, rc: rc, timeout: d}, func() {
		_ = rc.SetWriteDeadline(time.Time{})
	}
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
//
// 返回的 cleanup 负责清除读期限（keep-alive 连接会被复用）。
func withReadProgress(w http.ResponseWriter, r *http.Request, d time.Duration) func() {
	if d <= 0 || r.Body == nil {
		return func() {}
	}
	rc := http.NewResponseController(w)
	if err := rc.SetReadDeadline(time.Now().Add(d)); err != nil {
		return func() {}
	}
	r.Body = &progressReader{r: r.Body, rc: rc, timeout: d}
	return func() {
		_ = rc.SetReadDeadline(time.Time{})
	}
}

// withBodyReadDeadline 给一次 API 请求体读取加上**绝对**时限。
//
// 与 withReadProgress 的区别：不因收到少量字节而续期。它用于 JSON 这类
// 体积很小、正常客户端瞬间就能发完的请求 —— 一个字节一字节地慢慢发
// 不该能把并发名额永远占住。
func withBodyReadDeadline(w http.ResponseWriter, d time.Duration) func() {
	if d <= 0 {
		return func() {}
	}
	rc := http.NewResponseController(w)
	if err := rc.SetReadDeadline(time.Now().Add(d)); err != nil {
		return func() {}
	}
	return func() {
		_ = rc.SetReadDeadline(time.Time{})
	}
}

// uploadCopyBuf 是搬运上传请求体时单次读写的字节数。
//
// io.Copy 默认一次只搬 32KB —— 一个 1GiB 的文件就是三万余次 write(2)。
// 在本机（页缓存把所有写都吸收掉）这看不出问题，但落到「每次写都有固定开销」
// 的存储上（限速的云盘、网络卷、日志落在慢盘的日志式文件系统）差别就大了：
// 实测过一台这样的服务器，**读**能跑到 30–55 MiB/s，**上传**却始终卡在
// ~3.3 MiB/s，而服务端文件大小的增长曲线与客户端发送速率严丝合缝
// （说明阻塞点不在网络，而在 write(2)）。把单次写入放大到 1MiB，
// 写入系统调用少 32 倍，文件系统也才有机会把相邻的写合并成顺序落盘。
const uploadCopyBuf = 1 << 20

// uploadBufPool 复用缓冲区：上传是长连接，每次请求现分配 1MiB 没必要，
// 而并发闸门允许同时有几百个上传，真按峰值分配就是几百 MiB 的垃圾。
var uploadBufPool = sync.Pool{New: func() any { return make([]byte, uploadCopyBuf) }}

// onlyWriter 只暴露 Write，把底层类型实现的其它接口统统藏起来。
//
// ⚠️ 这层包装不是多余的：*os.File 实现了 io.ReaderFrom，io.Copy 会改走
// f.ReadFrom(r)，我们准备的缓冲区会被**无声忽略**（ReadFrom 内部自己开
// 32KB），看起来改了其实没改。包一层之后 io.Copy 才会用我们的缓冲区。
type onlyWriter struct{ w io.Writer }

func (w onlyWriter) Write(p []byte) (int, error) { return w.w.Write(p) }

// copyUpload 把上传的请求体搬进目标文件，用池化的大缓冲区（见 uploadCopyBuf）。
func copyUpload(dst io.Writer, src io.Reader) (int64, error) {
	buf := uploadBufPool.Get().([]byte)
	defer uploadBufPool.Put(buf)
	return io.CopyBuffer(onlyWriter{dst}, src, buf)
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

// nameTooLong 报告路径里第一段超长的名称及其字节数（limit <= 0 表示不限制）。
//
// 抽成不写响应的纯函数，是为了让批量重命名接口复用**同一套上限与同一句文案**：
// 两处各写一遍，迟早会出现「上传拒绝、改名放行」这种自相矛盾的判定。
func (s *Server) nameTooLong(urlPath string) (string, int, bool) {
	limit := s.cfg.MaxNameBytes
	if limit <= 0 {
		return "", 0, false
	}
	for _, seg := range strings.Split(strings.Trim(urlPath, "/"), "/") {
		if len(seg) > limit {
			return seg, len(seg), true
		}
	}
	return "", 0, false
}

// nameTooLongMsg 是超长名称的统一文案（上传与重命名共用）。
func (s *Server) nameTooLongMsg(n int) string {
	return fmt.Sprintf("文件名过长（%d 字节，上限 %d 字节）", n, s.cfg.MaxNameBytes)
}

// checkNameLength 校验路径中每一段的长度。
//
// 超长文件名如果直接交给文件系统，得到的是一句 ENAMETOOLONG，
// 透出去就是 500 —— 那看起来像服务端出故障，实际是请求本身不合法。
func (s *Server) checkNameLength(w http.ResponseWriter, urlPath string) bool {
	if _, n, ok := s.nameTooLong(urlPath); ok {
		http.Error(w, "400 Bad Request: "+s.nameTooLongMsg(n), http.StatusBadRequest)
		return false
	}
	return true
}
