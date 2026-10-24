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

// lockStateOf 读取某个账号锁定记录（测试内部用）。
func lockStateOf(t *testing.T, s *Server, user string) (time.Time, int) {
	t.Helper()
	s.guard.mu.Lock()
	defer s.guard.mu.Unlock()
	rec := s.guard.fails[authLockKey(user)]
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
		"--auth-lock-base-delay", "400ms",
		"--auth-lock-max-delay", "2s",
		// 把 IP / 全局维度调高，避免它们干扰本测试。
		"--auth-fail-ip-limit", "1000",
		"--auth-fail-global-limit", "10000"))

	lock := func(user string) {
		for i := 0; i < 3; i++ {
			do(t, s, http.MethodGet, "/?json", nil, badBasic(user))
		}
	}

	lock("alice")
	aUntil, aFails := lockStateOf(t, s, "alice")
	if aUntil.IsZero() {
		t.Fatal("alice 未进入锁定")
	}
	lock("bob")
	bUntil, bFails := lockStateOf(t, s, "bob")
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
		if got, _ := lockStateOf(t, s, "alice"); !got.Equal(aUntil) {
			t.Fatalf("第 %d 次轮询续期了锁定：原 %s，现 %s", i+1, aUntil, got)
		}
	}
	aAfter, aFailsAfter := lockStateOf(t, s, "alice")
	if !aAfter.Equal(aUntil) {
		t.Fatalf("轮询后续期：alice 解锁时间 %s -> %s", aUntil, aAfter)
	}
	if aFailsAfter != aFails {
		t.Fatalf("轮询后失败计数被累加：%d -> %d", aFails, aFailsAfter)
	}
	bAfter, bFailsAfter := lockStateOf(t, s, "bob")
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
		"--auth-lock-base-delay", "3s",
		"--auth-lock-max-delay", "10s",
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

// TestAuthLockAdminExemptRecovery 验证本机管理通道：
// 远程来源被锁后，从 127.0.0.1 用正确口令登录可以立即恢复，
// 并把该账号在远程来源上的锁定状态一并清零。
func TestAuthLockAdminExemptRecovery(t *testing.T) {
	s := newTestServer(t, testConfig(t, t.TempDir(),
		"-a", "admin:secret@/:rw",
		"--auth-fail-limit", "3",
		"--auth-lock-base-delay", "5m",
		"--auth-lock-max-delay", "30m",
		"--auth-fail-ip-limit", "1000",
		"--auth-fail-global-limit", "10000"))

	for i := 0; i < 3; i++ {
		do(t, s, http.MethodGet, "/?json", nil, badBasic("admin"))
	}
	remote := "192.0.2.1:1234"
	if r := doFrom(t, s, remote, http.MethodGet, "/?json", goodBasic("admin", "secret")); r.code != http.StatusTooManyRequests {
		t.Fatalf("远程来源在锁定期间正确密码返回 %d，期望 429", r.code)
	}

	// 管理员从本机回环直连：豁免锁定，正确口令立即登录成功。
	local := doFrom(t, s, "127.0.0.1:4321", http.MethodGet, "/?json", goodBasic("admin", "secret"))
	if local.code != http.StatusOK {
		t.Fatalf("本机恢复通道登录返回 %d，期望 200", local.code)
	}
	if locked, _ := s.authThrottled("192.0.2.1", authClassBasic, "admin"); locked {
		t.Fatal("本机登录成功后远程来源的锁定未被清除")
	}
	// 远程来源随即恢复。
	if r := doFrom(t, s, remote, http.MethodGet, "/?json", goodBasic("admin", "secret")); r.code != http.StatusOK {
		t.Fatalf("恢复后远程来源正确密码返回 %d，期望 200", r.code)
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
	if until, fails := lockStateOf(t, s, "admin"); !until.IsZero() || fails != 0 {
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
