package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
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

	// ipFails / globalFails 是「单 IP」与「全局」两个维度的失败窗口。
	// 它们与上面的 fails（来源 IP + 账号维度）相互独立：后者只影响
	// 「密码输错之后回 429 还是 401」，前者专门堵跨用户名的口令喷洒。
	ipFails     *authSprayWindow
	globalFails *authSprayWindow
}

// failRecord 记录一个「来源 IP + 账号」组合的锁定状态。
//
// 键是「IP + 用户名」的摘要，不区分凭据类别。按 IP 隔离是刻意的：
// 只知道用户名的人无法从别处把这个账号锁死；换个来源 IP 就是另一把锁。
// 用户名原文不落 map，避免超长用户名（请求头可达 64 KiB）撑大内存。
//
// 状态机有三条硬规则（见 noteAuthFailure / authThrottled）：
//   - 锁定期间只读，任何新请求都不得累加计数或延长 lockedUntil；
//   - 锁定时长按指数退避增长，封顶 maxDelay；
//   - 认证成功后立即清零。
//
// ⚠️ 锁定状态**只在密码校验之后**参与决策：正确密码永远可以登录，
// 锁定只决定一次失败的响应是 401 还是 429（附带 Retry-After）。
type failRecord struct {
	// fails 是连续失败次数。成功登录后归零；锁定期间不增长。
	fails int
	// lockedUntil 是退避截止时间。锁定只由「密码验证失败」设置，
	// 退避期内的请求不会改写它。
	lockedUntil time.Time
	// last 是最后一次**计入**失败的时间，仅用于清理过期记录。
	last time.Time
}

// authThrottleStep 是 IP/全局维度在超限时的减速步长。
//
// 它发生在**验证密码之前**，只拖慢猜测，不拒绝请求：正确密码仍会
// 在同一次请求里走到校验。IP+账号维度的锁定不使用这个等待，
// 它的语义是「密码错误后才返回 429」。
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
	g.ipFails = newAuthSprayWindow(cfg.AuthFailIPLimit, cfg.AuthFailIPWindow)
	g.globalFails = newAuthSprayWindow(cfg.AuthFailGlobalLimit, cfg.AuthFailGlobalWindow)
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

// authLockKey 是锁定记录的键：来源 IP + 用户名，不区分凭据类别。
//
// 用 SHA-256 而不是直接拼接后当 map 键：用户名长度由请求头决定
// （可达 64 KiB），摘要键能避免少量请求就占掉大量内存，也不会因为
// 用户名里的控制字符污染诊断信息。NUL 分隔 IP 与用户名，避免
// "1.2.3.4a"+"bc" 与 "1.2.3.4"+"abc" 撞到同一个键。
//
// 按 IP + 用户名隔离，而不是只按用户名：知道账号名的人无法从自己的
// 来源把别人锁死；同一账号从另一个 IP 看到的是另一条独立记录。
// IP / 全局维度有各自的滑动窗口，不受这里影响。
func authLockKey(ip, user string) string {
	raw := strings.TrimSpace(ip) + "\x00" + strings.TrimSpace(user)
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}

// authThrottled 是锁定状态的 Check：只读，锁定期内绝不写状态。
//
// ⚠️ 它不负责在验密码前拒绝请求。调用方必须先验证口令，只在密码错误
// 之后才用这里的返回值决定回 401 还是 429。正确密码不受任何影响。
// 返回值 true 表示响应应带 429 + 真实剩余时间。
func (s *Server) authThrottled(ip, class, user string) (bool, time.Duration) {
	g := s.guard
	if g == nil || s.cfg.AuthFailLimit <= 0 {
		return false, 0
	}
	// 管理员恢复通道：豁免来源的失败不计数，因此这里也不会看到锁。
	if s.isAuthLockExempt(ip) {
		return false, 0
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	rec := g.fails[authLockKey(ip, user)]
	if rec == nil {
		return false, 0
	}
	now := time.Now()
	if now.Before(rec.lockedUntil) {
		return true, rec.lockedUntil.Sub(now)
	}
	return false, 0
}

// waitAuthThrottle 在 IP/全局节流窗口内等一小段时间再继续验证密码。
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

// writeAuthLocked 写认证失败的 429 响应（普通文本格式）。
func writeAuthLocked(w http.ResponseWriter, remain time.Duration) {
	w.Header().Set("Retry-After", retryAfterSeconds(remain))
	http.Error(w, "429 Too Many Requests: 认证失败次数过多，请稍后再试",
		http.StatusTooManyRequests)
}

// retryAfterSeconds 把剩余等待时长换算成 Retry-After 的秒数（向上取整）。
//
// 之前这里写死 "1"：实际锁定可能是几分钟，客户端按 1 秒重试只会
// 在锁定期内反复撞 429。按真实剩余时间返回，客户端才能正确退避。
func retryAfterSeconds(remain time.Duration) string {
	if remain <= 0 {
		return "1"
	}
	secs := int64(remain / time.Second)
	if remain%time.Second != 0 {
		secs++
	}
	if secs < 1 {
		secs = 1
	}
	return strconv.FormatInt(secs, 10)
}

// noteAuthFailureState 记录一次**已经验证失败**的凭据尝试，并判断响应
// 是否应回 429。调用方必须已经确认密码错误；正确密码永远不会走到这里，
// 所以锁定状态不会挡住正确凭据。
//
// already 必须在记录前读取：达到阈值的那一次仍回 401，下一次失败才 429。
// spray 为 true 时同时计入单 IP / 全局维度（仅认证入口使用）。
func (s *Server) noteAuthFailureState(ip, class, user string, spray bool) (bool, time.Duration) {
	already, _ := s.authThrottled(ip, class, user)
	var sprayOver bool
	var retry time.Duration
	if spray {
		sprayOver, retry = s.noteSprayFailure(ip, user)
	}
	_, lockRetry, _ := s.noteAuthFailure(ip, class, user)
	if !already && !sprayOver {
		return false, 0
	}
	if already && lockRetry > retry {
		retry = lockRetry
	}
	return true, retry
}

// noteAuthFailureAndReply 记录一次账号失败，必要时补写 429。
func (s *Server) noteAuthFailureAndReply(w http.ResponseWriter, ip, class, user string) bool {
	limited, retry := s.noteAuthFailureState(ip, class, user, false)
	if !limited {
		return false
	}
	writeAuthLocked(w, retry)
	return true
}

// isAuthLockExempt 判断来源 IP 是否在管理员恢复通道的豁免网段内。
func (s *Server) isAuthLockExempt(ip string) bool {
	if ip == "" || len(s.authLockExempt) == 0 {
		return false
	}
	parsed := net.ParseIP(ip)
	if parsed == nil {
		return false
	}
	return ipInAny(parsed, s.authLockExempt)
}

// authLockAlert 输出一条可告警的结构化账号锁定日志。
func (s *Server) authLockAlert(ip, class, user string, failures int,
	lockedUntil time.Time, delay time.Duration) {
	payload := struct {
		Event       string `json:"event"`
		User        string `json:"user"`
		SrcIP       string `json:"src_ip"`
		Class       string `json:"class"`
		Failures    int    `json:"failures"`
		LockedUntil string `json:"locked_until"`
		Delay       string `json:"delay"`
	}{
		Event:       "auth_account_locked",
		User:        truncateAuthUser(user),
		SrcIP:       ip,
		Class:       class,
		Failures:    failures,
		LockedUntil: lockedUntil.UTC().Format(time.RFC3339),
		Delay:       delay.String(),
	}
	data, err := json.Marshal(payload)
	if err != nil {
		s.logger.Errorf("账号锁定告警序列化失败: %v", err)
		return
	}
	s.logger.Errorf("账号锁定 %s", data)
}

// authLockDelay 按连续失败次数计算本次锁定时长。
//
// 首次达到阈值锁 baseDelay；之后每多触发一次翻倍，封顶 maxDelay。
// baseDelay 为 `--auth-lock-base-delay`（旧名 --auth-fail-window），
// maxDelay 为 `--auth-lock-max-delay`。
func (s *Server) authLockDelay(fails int) time.Duration {
	base := s.cfg.AuthFailWindow
	if base <= 0 {
		base = time.Minute
	}
	max := s.cfg.AuthLockMaxDelay
	if max <= 0 {
		max = 30 * time.Minute
	}
	shift := fails - s.cfg.AuthFailLimit
	if shift < 0 {
		shift = 0
	}
	// 32 倍已经足够越过默认的 30m 上限；再大也只是溢出风险。
	if shift > 5 {
		shift = 5
	}
	d := base
	for i := 0; i < shift; i++ {
		if d >= max || d > max/2 {
			return max
		}
		d *= 2
	}
	if d > max {
		return max
	}
	return d
}

// noteAuthFailure 是锁定的 OnFailure：**只在密码验证失败后调用**。
//
// 锁定期间不再有新请求被提前拒绝，所有口令都会照常验证；即便并发的
// 所有失败都走到这里，已锁定的记录也不会被累加计数、也不会延长
// lockedUntil。这正是「攻击者每分钟探活一次就能永久锁死账号」的修复。
//
// 返回值 locked 表示本次失败是否让该「来源 IP + 账号」处于锁定状态；
// retry 是剩余时间。
func (s *Server) noteAuthFailure(ip, class, user string) (
	locked bool, retry time.Duration, fails int,
) {
	g := s.guard
	if g == nil || s.cfg.AuthFailLimit <= 0 {
		return false, 0, 0
	}
	// 管理员恢复通道：豁免来源的失败不计数，也就不会形成锁定。
	if s.isAuthLockExempt(ip) {
		return false, 0, 0
	}
	limit := s.cfg.AuthFailLimit
	key := authLockKey(ip, user)
	now := time.Now()

	g.mu.Lock()
	defer g.mu.Unlock()

	rec := g.fails[key]
	// 已锁定：不写任何状态，直接返回剩余时间。
	if rec != nil && now.Before(rec.lockedUntil) {
		return true, rec.lockedUntil.Sub(now), rec.fails
	}

	// 惰性清理：只在表明显变大时扫，避免被大量伪造来源/用户名撑爆内存。
	// 锁定期内的记录永远保留；其余按「最后一次计入失败 + 2×封顶时长」过期。
	ttl := 2 * s.cfg.AuthLockMaxDelay
	if ttl <= 0 {
		ttl = 2 * time.Hour
	}
	if len(g.fails) > 4096 {
		for k, v := range g.fails {
			if v.lockedUntil.After(now) {
				continue
			}
			if now.Sub(v.last) > ttl {
				delete(g.fails, k)
			}
		}
	}
	// 硬上限：持续注入新键时，仅靠「清过期」挡不住内存增长。
	// 淘汰时跳过仍在锁定期的记录，否则攻击者可以故意灌满表，
	// 把针对自己的锁定记录挤掉后继续猜口令。
	if len(g.fails) >= maxAuthFailEntries {
		n := 0
		for k, v := range g.fails {
			if n >= maxAuthFailEntries/2 {
				break
			}
			if v.lockedUntil.After(now) {
				continue
			}
			delete(g.fails, k)
			n++
		}
		if len(g.fails) >= maxAuthFailEntries {
			return false, 0, 0
		}
	}

	if rec == nil {
		rec = &failRecord{}
		g.fails[key] = rec
	}
	rec.fails++
	rec.last = now
	fails = rec.fails
	if fails < limit {
		return false, 0, fails
	}

	d := s.authLockDelay(fails)
	rec.lockedUntil = now.Add(d)
	s.authLockAlert(ip, class, user, fails, rec.lockedUntil, d)
	return true, d, fails
}

// noteAuthSuccess 是锁定的 OnSuccess：立刻清零该「来源 IP + 账号」的
// 计数与锁定状态。
//
// 键包含 IP，所以一次成功登录只清掉当前来源的锁；别的来源即使还在
// 退避期，也影响不到这里。成功登录本身不受锁定影响。
func (s *Server) noteAuthSuccess(ip, class, user string) {
	g := s.guard
	if g == nil || s.cfg.AuthFailLimit <= 0 {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	delete(g.fails, authLockKey(ip, user))
}

// ---------------------------------------------------------------------------
// 按来源 IP / 全局的失败限速
// ---------------------------------------------------------------------------

// authGlobalKey 是全局失败窗口里唯一的一个键。
const authGlobalKey = "*"

// maxAuthSprayEntries 是单 IP / 全局两张失败表的硬上限。
//
// 与 maxAuthFailEntries 同理：攻击者可以伪造大量来源地址（只有在部署了
// 可信代理且转发头可被伪造时）持续注入新键。超过上限时优先淘汰没有处于
// 封禁期的记录；如果整张表都在封禁期，宁可不记这一次，也不让 map 无界增长。
const maxAuthSprayEntries = 65536

// authFailureEvent 是窗口内的一次失败。
//
// user 在**入表前**就已经脱敏（见 maskAuthUser）：窗口只需要给告警提供
// 「像什么账号」，没必要为了日志把完整用户名留在内存里 —— 请求头里的
// 用户名最长可到 64 KiB，原样保存会让内存占用被攻击者牵着走。
type authFailureEvent struct {
	at   time.Time
	user string
}

// authSprayEntry 是某个维度（全局或单个 IP）的窗口状态。
type authSprayEntry struct {
	// events 按时间升序，最多保留 limit+1 条 —— 判定「是否超限」和
	// 计算「多久后回到阈值以内」都只需要最老的这一条。
	events []authFailureEvent
	// blockedUntil 是本次超限的截止时间。
	//
	// ⚠️ 封禁期内再失败**不会**延长它：否则攻击者只要持续尝试，
	// Retry-After 就永远是满窗口，既不符合「真实剩余时间」，客户端也
	// 无法退避。到点后窗口内的事件自然滑出，重新从空窗口开始计数。
	blockedUntil time.Time
}

// authSprayWindow 是一个带过期清理的滑动窗口。
//
// 它只记录**认证失败**：正确密码不会走到这里，所以即便某个 IP 因攻击
// 被暂时拖慢，合法用户仍能在一次短等待后登录（见 waitAuthThrottle）。
type authSprayWindow struct {
	limit  int
	window time.Duration

	mu        sync.Mutex
	entries   map[string]*authSprayEntry
	lastSweep time.Time
}

func newAuthSprayWindow(limit int, window time.Duration) *authSprayWindow {
	if limit <= 0 || window <= 0 {
		return nil
	}
	return &authSprayWindow{
		limit:   limit,
		window:  window,
		entries: make(map[string]*authSprayEntry),
	}
}

// state 报告该键当前是否处于超限封禁期，以及还要等多久。
func (w *authSprayWindow) state(key string, now time.Time) (bool, time.Duration) {
	if w == nil {
		return false, 0
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	e := w.entries[key]
	if e == nil {
		return false, 0
	}
	w.pruneLocked(e, now)
	if now.Before(e.blockedUntil) {
		return true, e.blockedUntil.Sub(now)
	}
	if !e.blockedUntil.IsZero() {
		e.blockedUntil = time.Time{}
	}
	if len(e.events) == 0 {
		delete(w.entries, key)
	}
	return false, 0
}

// record 记一次失败，并返回记录后的状态。
//
// over 表示该维度已超过阈值（本次请求也应返回 429）；retry 是真实的
// 剩余解封时间；count 是窗口内的失败次数（受 limit+1 上限约束）；
// users 是最近尝试过的用户名（已脱敏）；crossed 表示**本次**刚好跨过阈值，
// 调用方应据此只发一条告警，而不是每次失败都刷屏。
func (w *authSprayWindow) record(key, user string, now time.Time) (
	over bool, retry time.Duration, count int, users []string, crossed bool,
) {
	if w == nil {
		return false, 0, 0, nil, false
	}
	w.mu.Lock()
	defer w.mu.Unlock()

	w.sweepLocked(now)

	e := w.entries[key]
	if e == nil {
		if len(w.entries) >= maxAuthSprayEntries && !w.evictLocked(now) {
			// 整张表都在封禁期：这次不记，避免内存无界增长。
			return false, 0, 0, nil, false
		}
		e = &authSprayEntry{}
		w.entries[key] = e
	}
	w.pruneLocked(e, now)

	// 封禁期内不堆事件、也不延长截止时间：Retry-After 才能真正递减。
	if now.Before(e.blockedUntil) {
		return true, e.blockedUntil.Sub(now), len(e.events),
			recentAuthUsers(e.events, 10), false
	}
	if !e.blockedUntil.IsZero() {
		e.blockedUntil = time.Time{}
	}

	wasOver := len(e.events) > w.limit
	e.events = append(e.events, authFailureEvent{at: now, user: maskAuthUser(user)})
	// 只保留 limit+1 条：超过阈值之后，判定只需要「最老的那一条何时过期」。
	if max := w.limit + 1; len(e.events) > max {
		e.events = e.events[len(e.events)-max:]
	}
	count = len(e.events)
	over = count > w.limit
	if over {
		// 需要让 count-limit 条事件滑出窗口；剩下的第一条（下标
		// count-limit-1）过期时，窗口内就正好剩下 limit 条。
		idx := count - w.limit - 1
		e.blockedUntil = e.events[idx].at.Add(w.window)
		retry = e.blockedUntil.Sub(now)
		crossed = !wasOver
	}
	return over, retry, count, recentAuthUsers(e.events, 10), crossed
}

// pruneLocked 丢弃已经滑出窗口的事件。调用方需持有 w.mu。
func (w *authSprayWindow) pruneLocked(e *authSprayEntry, now time.Time) {
	cutoff := now.Add(-w.window)
	i := 0
	for i < len(e.events) && !e.events[i].at.After(cutoff) {
		i++
	}
	if i > 0 {
		e.events = e.events[i:]
	}
}

// sweepLocked 定期清理整张表，避免大量只出现一次的来源长期占着内存。
// 调用方需持有 w.mu。
func (w *authSprayWindow) sweepLocked(now time.Time) {
	if now.Sub(w.lastSweep) < w.window {
		return
	}
	w.lastSweep = now
	cutoff := now.Add(-w.window)
	for k, e := range w.entries {
		if now.Before(e.blockedUntil) {
			continue
		}
		if len(e.events) == 0 || !e.events[len(e.events)-1].at.After(cutoff) {
			delete(w.entries, k)
		}
	}
}

// evictLocked 在表满时淘汰一条不在封禁期的记录。
// 返回 false 表示没有可淘汰的记录。调用方需持有 w.mu。
func (w *authSprayWindow) evictLocked(now time.Time) bool {
	for k, e := range w.entries {
		if now.Before(e.blockedUntil) {
			continue
		}
		delete(w.entries, k)
		return true
	}
	return false
}

// recentAuthUsers 取窗口内最近尝试的用户名（最多 max 个、去重）。
// 事件里的 user 已经是脱敏值（见 authFailureEvent），这里不再处理原文。
func recentAuthUsers(events []authFailureEvent, max int) []string {
	seen := make(map[string]bool, len(events))
	out := make([]string, 0, max)
	for i := len(events) - 1; i >= 0 && len(out) < max; i-- {
		name := events[i].user
		if seen[name] {
			continue
		}
		seen[name] = true
		out = append(out, name)
	}
	return out
}

// authUserLogPrefixRunes 是告警日志里保留的用户名前缀字符数。
//
// 只留前几个字符：告警需要的是「像什么账号」，而不是完整账号名；
// 用户名本身可能等于手机号/邮箱，原样写进日志会成为另一条泄露渠道。
const authUserLogPrefixRunes = 3

// maskAuthUser 脱敏一个用户名，供告警日志使用。
func maskAuthUser(user string) string {
	user = strings.TrimSpace(user)
	if user == "" {
		return "<empty>"
	}
	r := []rune(user)
	if len(r) <= authUserLogPrefixRunes {
		return string(r) + "…"
	}
	return string(r[:authUserLogPrefixRunes]) + "…"
}

// authSprayAlert 是「单 IP 失败超限」的告警钩子。
//
// 目前只输出一条结构化日志（JSON）。接入告警系统时替换这里即可，
// 调用点不需要再改。注意 users 已经过脱敏，不要在这里改回原文。
func (s *Server) authSprayAlert(srcIP string, failures int, users []string) {
	payload := struct {
		Event    string   `json:"event"`
		SrcIP    string   `json:"src_ip"`
		Failures int      `json:"failures"`
		Limit    int      `json:"limit"`
		Window   string   `json:"window"`
		Users    []string `json:"users"`
	}{
		Event:    "auth_fail_ip_threshold",
		SrcIP:    srcIP,
		Failures: failures,
		Limit:    s.cfg.AuthFailIPLimit,
		Window:   s.cfg.AuthFailIPWindow.String(),
		Users:    users,
	}
	data, err := json.Marshal(payload)
	if err != nil {
		s.logger.Errorf("认证失败限速告警序列化失败: %v", err)
		return
	}
	s.logger.Errorf("认证失败限速告警 %s", data)
}

// authSprayThrottled 按「全局 -> 单 IP」的顺序判断是否超限。
//
// 返回的剩余时间是两个维度里最长的那个：客户端必须等到所有维度都解封
// 才有意义。没有凭据的请求不会走到这里，会话/匿名访问也不受影响。
func (s *Server) authSprayThrottled(ip string) (bool, time.Duration) {
	g := s.guard
	if g == nil {
		return false, 0
	}
	now := time.Now()
	over := false
	var retry time.Duration
	if g.globalFails != nil {
		if o, d := g.globalFails.state(authGlobalKey, now); o {
			over = true
			retry = d
		}
	}
	if g.ipFails != nil && ip != "" {
		if o, d := g.ipFails.state(ip, now); o {
			over = true
			if d > retry {
				retry = d
			}
		}
	}
	return over, retry
}

// noteSprayFailure 在「全局」与「单 IP」两个维度各记一次失败。
//
// 调用方必须已经确认密码不正确。返回值与 authSprayThrottled 相同：
// over=true 表示本次请求应返回 429。成功登录**不清零**这两个维度 ——
// 否则攻击者只要穿插一个自己的合法账号，就能把针对别人的喷洒计数抹掉。
func (s *Server) noteSprayFailure(ip, user string) (over bool, retry time.Duration) {
	g := s.guard
	if g == nil {
		return false, 0
	}
	now := time.Now()

	if g.globalFails != nil {
		o, d, _, _, _ := g.globalFails.record(authGlobalKey, user, now)
		if o {
			over = true
			retry = d
		}
	}
	if g.ipFails != nil && ip != "" {
		o, d, count, users, crossed := g.ipFails.record(ip, user, now)
		if o {
			over = true
			if d > retry {
				retry = d
			}
		}
		if crossed {
			s.authSprayAlert(ip, count, users)
		}
	}
	return over, retry
}

// throttleAuthSpray 在认证入口按「单 IP / 全局」维度做「先减速、再验密码」。
//
// 它只拖慢猜测，不拒绝请求：超限时先等一个节流步长，正确密码随后仍会
// 走到校验。账号锁定（IP + 账号维度）不在这里判断 —— 那条状态只决定
// 密码错误之后回 401 还是 429（见 noteAuthFailureState）。
//
// 只有认证入口（GET / 的 Basic 校验、/fs/auth）才应该调用它；文件传输
// 路径不参与这两个维度，避免一次喷洒把正常的传输请求拖慢。
func (s *Server) throttleAuthSpray(r *http.Request, class string) (string, bool) {
	ip := s.clientIP(r)
	if class != authClassBasic && class != authClassQuery && class != authClassPassword {
		return ip, true
	}
	if over, remain := s.authSprayThrottled(ip); over {
		if !s.waitAuthThrottle(r, remain) {
			return ip, false
		}
	}
	return ip, true
}

// noteAuthFailureAndReplyScoped 在三个维度上记一次失败；任一维度超限
// 就写 429 并返回 true。
//
// 与 noteAuthFailureAndReply 的区别只是多了「全局 / 单 IP」两个计数维度；
// 「IP + 账号」维度达到阈值的那一次仍回 401、下一次才 429。
func (s *Server) noteAuthFailureAndReplyScoped(w http.ResponseWriter, ip, class, user string) bool {
	limited, retry := s.noteAuthFailureState(ip, class, user, true)
	if !limited {
		return false
	}
	writeAuthLocked(w, retry)
	return true
}

// authSprayScope 判断请求是否属于「会参与 IP/全局限速的认证入口」。
//
// 目前只有 GET / 的 Basic 校验（应用外壳预检与 authorize 两条分支）：
// /fs/extract、/fs/text、/fs/office 以及上传/下载都带着具体的文件路径，
// 不会命中这里。静态资源 /fs/assets/* 走独立处理器，根本不经过鉴权，
// 自然也不计数。
func authSprayScope(r *http.Request, urlPath string) bool {
	return r.Method == http.MethodGet && urlPath == "/"
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
