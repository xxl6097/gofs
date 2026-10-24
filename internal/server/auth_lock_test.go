package server

import (
	"bytes"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/xxl6097/gofs/internal/config"
)

// testClientIP 是 httptest.NewRequest 的默认 RemoteAddr 主机地址。
const testClientIP = "192.0.2.1"

// lockStateOf 读取某个「来源 IP + 账号」的锁定记录（测试内部用）。
func lockStateOf(t *testing.T, s *Server, ip, user string) (time.Time, int) {
	t.Helper()
	s.guard.mu.Lock()
	defer s.guard.mu.Unlock()
	rec := s.guard.fails[authLockKey(ip, user)]
	if rec == nil {
		return time.Time{}, 0
	}
	return rec.lockedUntil, rec.fails
}

func badBasic(user string) map[string]string {
	return map[string]string{"Authorization": basicAuth(user, "wrong-password")}
}

func goodBasic(user, pass string) map[string]string {
	return map[string]string{"Authorization": basicAuth(user, pass)}
}

// TestAuthLockNoRenewalOnPolling 是本次修复的关键差分测试：
// A 在锁定期间被轮询 20 次，B 完全不请求；两者的解锁时间必须一致，
// A 的 lockedUntil 不能被任何一次轮询往后推。
func TestAuthLockNoRenewalOnPolling(t *testing.T) {
	s := newTestServer(t, testConfig(t, t.TempDir(),
		"-a", "alice:apw@/:rw",
		"-a", "bob:bpw@/:rw",
		"--auth-fail-limit", "3",
		// 退避期必须明显长于 20 次轮询的耗时：短到几百毫秒时，
		// 机器一慢（-race / CI）轮询就会落在解封之后，测试变成 flaky。
		"--auth-lock-base-delay", "5s",
		"--auth-lock-max-delay", "10s",
		// 把 IP / 全局维度调高，避免它们干扰本测试。
		"--auth-fail-ip-limit", "1000",
		"--auth-fail-global-limit", "10000"))

	lock := func(user string) {
		for i := 0; i < 3; i++ {
			do(t, s, http.MethodGet, "/?json", nil, badBasic(user))
		}
	}

	lock("alice")
	aUntil, aFails := lockStateOf(t, s, testClientIP, "alice")
	if aUntil.IsZero() {
		t.Fatal("alice 未进入锁定")
	}
	lock("bob")
	bUntil, bFails := lockStateOf(t, s, testClientIP, "bob")
	if bUntil.IsZero() {
		t.Fatal("bob 未进入锁定")
	}
	if aUntil.After(bUntil) {
		t.Fatalf("alice 先触发锁定，解锁时间却晚于 bob：alice=%s bob=%s", aUntil, bUntil)
	}

	// A 被轮询 20 次。每次都应是 429，且不得改变 lockedUntil / fails。
	for i := 0; i < 20; i++ {
		r := do(t, s, http.MethodGet, "/?json", nil, badBasic("alice"))
		if r.code != http.StatusTooManyRequests {
			t.Fatalf("锁定期间第 %d 次轮询返回 %d，期望 429", i+1, r.code)
		}
		if got, _ := lockStateOf(t, s, testClientIP, "alice"); !got.Equal(aUntil) {
			t.Fatalf("第 %d 次轮询续期了锁定：原 %s，现 %s", i+1, aUntil, got)
		}
	}
	aAfter, aFailsAfter := lockStateOf(t, s, testClientIP, "alice")
	if !aAfter.Equal(aUntil) {
		t.Fatalf("轮询后续期：alice 解锁时间 %s -> %s", aUntil, aAfter)
	}
	if aFailsAfter != aFails {
		t.Fatalf("轮询后失败计数被累加：%d -> %d", aFails, aFailsAfter)
	}
	bAfter, bFailsAfter := lockStateOf(t, s, testClientIP, "bob")
	if !bAfter.Equal(bUntil) || bFailsAfter != bFails {
		t.Fatalf("未请求的 bob 状态发生了变化：until %s -> %s, fails %d -> %d",
			bUntil, bAfter, bFails, bFailsAfter)
	}
	t.Logf("A(轮询 20 次) 解锁=%s；B(零请求) 解锁=%s", aUntil, bUntil)

	// 等过两者的锁定时间：两个账号都必须恢复。
	time.Sleep(time.Until(bUntil) + 80*time.Millisecond)
	for _, c := range []struct{ user, pass string }{{"alice", "apw"}, {"bob", "bpw"}} {
		if r := do(t, s, http.MethodGet, "/?json", nil, goodBasic(c.user, c.pass)); r.code != http.StatusOK {
			t.Fatalf("%s 在解锁后正确密码返回 %d，期望 200", c.user, r.code)
		}
	}
}

// TestAuthLockDelayExponential 钉住指数退避：首次 1m，之后翻倍，封顶 30m。
func TestAuthLockDelayExponential(t *testing.T) {
	s := &Server{cfg: &config.Config{
		AuthFailLimit:    10,
		AuthFailWindow:   time.Minute,
		AuthLockMaxDelay: 30 * time.Minute,
	}}
	cases := []struct {
		fails int
		want  time.Duration
	}{
		{10, time.Minute},     // 首次
		{11, 2 * time.Minute}, // 翻倍
		{12, 4 * time.Minute},
		{13, 8 * time.Minute},
		{14, 16 * time.Minute},
		{15, 30 * time.Minute}, // 32m -> 封顶
		{100, 30 * time.Minute},
	}
	for _, c := range cases {
		if got := s.authLockDelay(c.fails); got != c.want {
			t.Errorf("authLockDelay(%d) = %s，期望 %s", c.fails, got, c.want)
		}
	}
}

// TestAuthLockRetryAfterCountsDown 验证锁定期间 Retry-After 是真实剩余秒数，
// 且轮询不会把它重置回满值。
func TestAuthLockRetryAfterCountsDown(t *testing.T) {
	s := newTestServer(t, testConfig(t, t.TempDir(),
		"-a", "admin:secret@/:rw",
		"--auth-fail-limit", "2",
		// 留足余量，避免 -race / CI 下校验耗时把退避期吃掉。
		"--auth-lock-base-delay", "10s",
		"--auth-lock-max-delay", "30s",
		"--auth-fail-ip-limit", "1000",
		"--auth-fail-global-limit", "10000"))

	do(t, s, http.MethodGet, "/?json", nil, badBasic("admin"))
	do(t, s, http.MethodGet, "/?json", nil, badBasic("admin"))

	first := do(t, s, http.MethodGet, "/?json", nil, badBasic("admin"))
	if first.code != http.StatusTooManyRequests {
		t.Fatalf("首次锁定响应 = %d，期望 429", first.code)
	}
	firstRetry, err := strconv.Atoi(first.head.Get("Retry-After"))
	if err != nil || firstRetry < 2 {
		t.Fatalf("首次 Retry-After = %q，期望约 3 秒", first.head.Get("Retry-After"))
	}

	time.Sleep(1200 * time.Millisecond)
	second := do(t, s, http.MethodGet, "/?json", nil, badBasic("admin"))
	if second.code != http.StatusTooManyRequests {
		t.Fatalf("锁定期间再次请求 = %d，期望 429", second.code)
	}
	secondRetry, err := strconv.Atoi(second.head.Get("Retry-After"))
	if err != nil {
		t.Fatalf("第二次 Retry-After = %q 不是整数", second.head.Get("Retry-After"))
	}
	if secondRetry >= firstRetry {
		t.Fatalf("Retry-After 未随时间递减：%d -> %d", firstRetry, secondRetry)
	}
}

// TestAuthLockCorrectPasswordBypassesLock 验证新策略的核心语义：
// 锁定只影响错误口令的响应；正确口令照常登录，并在成功后清除该来源的锁。
func TestAuthLockCorrectPasswordBypassesLock(t *testing.T) {
	s := newTestServer(t, testConfig(t, t.TempDir(),
		"-a", "admin:secret@/:rw",
		"--auth-fail-limit", "3",
		"--auth-lock-base-delay", "5m",
		"--auth-lock-max-delay", "30m",
		"--auth-fail-ip-limit", "1000",
		"--auth-fail-global-limit", "10000"))

	remote := testClientIP + ":1234"
	for i := 0; i < 3; i++ {
		doFrom(t, s, remote, http.MethodGet, "/?json", badBasic("admin"))
	}
	if locked, _ := s.authThrottled(testClientIP, authClassBasic, "admin"); !locked {
		t.Fatal("3 次失败后该来源+账号未进入锁定")
	}
	// 锁定期内继续输错：口令仍会被验证，失败后回 429。
	if r := doFrom(t, s, remote, http.MethodGet, "/?json", badBasic("admin")); r.code != http.StatusTooManyRequests {
		t.Fatalf("锁定期内错误口令返回 %d，期望 429", r.code)
	}
	// 锁定期内正确口令：不受 429 影响。
	if r := doFrom(t, s, remote, http.MethodGet, "/?json", goodBasic("admin", "secret")); r.code != http.StatusOK {
		t.Fatalf("锁定期内正确密码返回 %d，期望 200", r.code)
	}
	// 成功登录清除了当前来源的锁定记录。
	if locked, _ := s.authThrottled(testClientIP, authClassBasic, "admin"); locked {
		t.Fatal("正确登录后当前来源的锁定未清除")
	}
}

// TestAuthLockIsolatedBySourceIP 验证失败计数按「IP + 用户名」隔离：
// 另一个来源 IP 不会因为别人猜错而看到 429，也不需要等退避期。
func TestAuthLockIsolatedBySourceIP(t *testing.T) {
	s := newTestServer(t, testConfig(t, t.TempDir(),
		"-a", "admin:secret@/:rw",
		"--auth-fail-limit", "3",
		"--auth-lock-base-delay", "5m",
		"--auth-lock-max-delay", "30m",
		"--auth-fail-ip-limit", "1000",
		"--auth-fail-global-limit", "10000"))

	attacker := "198.51.100.7:1234"
	for i := 0; i < 3; i++ {
		doFrom(t, s, attacker, http.MethodGet, "/?json", badBasic("admin"))
	}
	if locked, _ := s.authThrottled("198.51.100.7", authClassBasic, "admin"); !locked {
		t.Fatal("攻击来源未进入锁定")
	}

	// 同一账号、另一个来源：错误口令仍回 401，而不是 429。
	other := "203.0.113.9:1234"
	if r := doFrom(t, s, other, http.MethodGet, "/?json", badBasic("admin")); r.code != http.StatusUnauthorized {
		t.Fatalf("其它来源首次错误口令返回 %d，期望 401", r.code)
	}
	// 另一个来源的正确口令当然也能登录。
	if r := doFrom(t, s, other, http.MethodGet, "/?json", goodBasic("admin", "secret")); r.code != http.StatusOK {
		t.Fatalf("其它来源正确密码返回 %d，期望 200", r.code)
	}
	// 攻击来源自己用正确密码也能立即登录（锁定不阻止正确凭据）。
	if r := doFrom(t, s, attacker, http.MethodGet, "/?json", goodBasic("admin", "secret")); r.code != http.StatusOK {
		t.Fatalf("锁定来源的正确密码返回 %d，期望 200", r.code)
	}
}

// TestAuthLockExemptSourceNotCounted 验证管理员恢复通道：豁免来源的失败
// 不进入锁定记录，因此本机不会因为反复输错而被账号锁拖慢。
func TestAuthLockExemptSourceNotCounted(t *testing.T) {
	s := newTestServer(t, testConfig(t, t.TempDir(),
		"-a", "admin:secret@/:rw",
		"--auth-fail-limit", "2",
		"--auth-lock-base-delay", "5m",
		"--auth-lock-max-delay", "30m",
		"--auth-fail-ip-limit", "1000",
		"--auth-fail-global-limit", "10000"))

	local := "127.0.0.1:4321"
	for i := 0; i < 5; i++ {
		doFrom(t, s, local, http.MethodGet, "/?json", badBasic("admin"))
	}
	if locked, _ := s.authThrottled("127.0.0.1", authClassBasic, "admin"); locked {
		t.Fatal("豁免来源不应进入锁定")
	}
	if r := doFrom(t, s, local, http.MethodGet, "/?json", goodBasic("admin", "secret")); r.code != http.StatusOK {
		t.Fatalf("豁免来源正确密码返回 %d，期望 200", r.code)
	}
}

// TestAuthLockStructuredLog 验证锁定触发时输出可告警的结构化日志。
func TestAuthLockStructuredLog(t *testing.T) {
	s := newTestServer(t, testConfig(t, t.TempDir(),
		"-a", "admin:secret@/:rw",
		"--auth-fail-limit", "2",
		"--auth-lock-base-delay", "1m",
		"--auth-lock-max-delay", "30m",
		"--auth-fail-ip-limit", "1000",
		"--auth-fail-global-limit", "10000"))

	var buf bytes.Buffer
	s.logger = &Logger{out: &buf}
	do(t, s, http.MethodGet, "/?json", nil, badBasic("admin"))
	do(t, s, http.MethodGet, "/?json", nil, badBasic("admin"))

	logs := buf.String()
	for _, want := range []string{
		`"event":"auth_account_locked"`,
		`"user":"admin"`,
		`"failures":2`,
		`"locked_until":"`,
		`"delay":"1m0s"`,
	} {
		if !strings.Contains(logs, want) {
			t.Errorf("锁定日志缺少 %s，实际日志：\n%s", want, logs)
		}
	}
}

// TestAuthLockSuccessClearsState 验证成功登录立即清零计数与锁定状态。
func TestAuthLockSuccessClearsState(t *testing.T) {
	s := newTestServer(t, testConfig(t, t.TempDir(),
		"-a", "admin:secret@/:rw",
		"--auth-fail-limit", "3",
		"--auth-lock-base-delay", "5m",
		"--auth-lock-max-delay", "30m",
		"--auth-fail-ip-limit", "1000",
		"--auth-fail-global-limit", "10000"))

	do(t, s, http.MethodGet, "/?json", nil, badBasic("admin"))
	do(t, s, http.MethodGet, "/?json", nil, badBasic("admin"))
	if r := do(t, s, http.MethodGet, "/?json", nil, goodBasic("admin", "secret")); r.code != http.StatusOK {
		t.Fatalf("成功登录返回 %d，期望 200", r.code)
	}
	if until, fails := lockStateOf(t, s, testClientIP, "admin"); !until.IsZero() || fails != 0 {
		t.Fatalf("成功登录后状态未清零：until=%s fails=%d", until, fails)
	}
	// 重新累计两次仍不应锁定（计数从 0 开始）。
	for i := 0; i < 2; i++ {
		if r := do(t, s, http.MethodGet, "/?json", nil, badBasic("admin")); r.code != http.StatusUnauthorized {
			t.Fatalf("成功清零后第 %d 次失败返回 %d，期望 401", i+1, r.code)
		}
	}
}

// TestAuthLockExemptConfig 验证管理员恢复网段可配置（默认仅回环）。
func TestAuthLockExemptConfig(t *testing.T) {
	cfg := testConfig(t, t.TempDir(),
		"-a", "admin:secret@/:rw",
		"--auth-lock-base-delay", "1m",
		"--auth-lock-max-delay", "30m")
	if !containsString(cfg.AuthLockExempt, "127.0.0.1/32") {
		t.Fatalf("默认恢复网段缺少回环：%v", cfg.AuthLockExempt)
	}
	s := newTestServer(t, cfg)
	if !s.isAuthLockExempt("127.0.0.1") {
		t.Error("127.0.0.1 应被豁免")
	}
	if s.isAuthLockExempt("192.0.2.1") {
		t.Error("192.0.2.1 不应被默认豁免")
	}
}

func containsString(xs []string, want string) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}
