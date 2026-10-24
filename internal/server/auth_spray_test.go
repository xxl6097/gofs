package server

import (
	"bytes"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestAuthSprayBlockedByIP 复现任务里的攻击：大量不同用户名 + 同一个口令。
//
// 修复前计数只按「用户名」隔离，每个用户名只失败一次，永远到不了阈值，
// 80 次尝试一次 429 都不会出现。现在单 IP 维度会先超限。
func TestAuthSprayBlockedByIP(t *testing.T) {
	root := makeFixture(t)
	s := newTestServer(t, testConfig(t, root,
		"-a", "admin:secret@/:rw",
		// 把用户名维度调高，确保 429 只能来自新增的 IP 维度。
		"--auth-fail-limit", "100",
		"--auth-fail-ip-limit", "3",
		"--auth-fail-ip-window", "1m"))

	var got429 int
	for i := 0; i < 10; i++ {
		r := do(t, s, http.MethodGet, "/?json", nil, map[string]string{
			"Authorization": basicAuth(fmt.Sprintf("spray-%02d", i), "same-wrong"),
		})
		if r.code == http.StatusTooManyRequests {
			got429++
		}
	}
	if got429 == 0 {
		t.Fatal("跨用户名的口令喷洒没有被拦截：10 次失败中一次 429 都没有")
	}
	// 阈值 3：前 3 次失败允许，第 4 次起进入封禁期。
	if got429 != 7 {
		t.Fatalf("429 次数 = %d，期望 7（第 4~10 次）", got429)
	}
}

// TestAuthSprayRetryAfterCountsDown 验证 Retry-After 是真实剩余秒数，
// 而不是写死的 1：封禁期内继续尝试时，它应随剩余时间递减。
func TestAuthSprayRetryAfterCountsDown(t *testing.T) {
	root := makeFixture(t)
	s := newTestServer(t, testConfig(t, root,
		"-a", "admin:secret@/:rw",
		"--auth-fail-limit", "100",
		"--auth-fail-ip-limit", "2",
		"--auth-fail-ip-window", "5s"))

	attempt := func() resp {
		return do(t, s, http.MethodGet, "/?json", nil, map[string]string{
			"Authorization": basicAuth("admin", "wrong"),
		})
	}
	// 前两次达到阈值，第三次触发 429。
	attempt()
	attempt()
	first := attempt()
	if first.code != http.StatusTooManyRequests {
		t.Fatalf("第 3 次失败返回 %d，期望 429", first.code)
	}
	firstRetry, err := strconv.Atoi(first.head.Get("Retry-After"))
	if err != nil {
		t.Fatalf("Retry-After = %q，不是整数: %v", first.head.Get("Retry-After"), err)
	}
	if firstRetry < 4 {
		t.Fatalf("首次 429 的 Retry-After = %d，期望接近窗口 5s", firstRetry)
	}

	// 等待一段时间后仍在封禁期内。封禁期内的失败不会延长截止时间，
	// 所以 Retry-After 必须变小。
	time.Sleep(1500 * time.Millisecond)
	second := attempt()
	if second.code != http.StatusTooManyRequests {
		t.Fatalf("封禁期内再次失败返回 %d，期望 429", second.code)
	}
	secondRetry, err := strconv.Atoi(second.head.Get("Retry-After"))
	if err != nil {
		t.Fatalf("Retry-After = %q，不是整数: %v", second.head.Get("Retry-After"), err)
	}
	if secondRetry >= firstRetry {
		t.Fatalf("Retry-After 没有随时间递减：第一次 %d，等待后 %d",
			firstRetry, secondRetry)
	}
}

// TestAuthSprayGlobalLimit 验证全局维度：不同来源 IP 的失败会合并计数。
func TestAuthSprayGlobalLimit(t *testing.T) {
	root := makeFixture(t)
	cfg := testConfig(t, root,
		"-a", "admin:secret@/:rw",
		"--auth-fail-limit", "100",
		"--auth-fail-ip-limit", "100",
		"--auth-fail-global-limit", "3",
		"--auth-fail-global-window", "1m")
	// 测试请求的 RemoteAddr 是 192.0.2.1；只有它被列为可信代理时，
	// X-Forwarded-For 才会被采信，从而模拟不同来源 IP。
	cfg.TrustedProxies = []string{"192.0.2.0/24"}
	s := newTestServer(t, cfg)

	var got429 int
	for i := 0; i < 6; i++ {
		r := do(t, s, http.MethodGet, "/?json", nil, map[string]string{
			"Authorization":   basicAuth(fmt.Sprintf("global-%02d", i), "same-wrong"),
			"X-Forwarded-For": fmt.Sprintf("10.20.0.%d", i+1),
		})
		if r.code == http.StatusTooManyRequests {
			got429++
		}
	}
	if got429 != 3 {
		t.Fatalf("跨 IP 的全局 429 次数 = %d，期望 3（第 4~6 次）", got429)
	}
}

// TestAuthSprayCorrectPasswordStillWorks 验证 NAT 场景：共享出口 IP 里
// 有人喷洒时，合法用户拿着正确密码仍然能登录（最多被拖慢一个节流步长）。
func TestAuthSprayCorrectPasswordStillWorks(t *testing.T) {
	root := makeFixture(t)
	s := newTestServer(t, testConfig(t, root,
		"-a", "admin:secret@/:rw",
		"--auth-fail-limit", "100",
		"--auth-fail-ip-limit", "2",
		"--auth-fail-ip-window", "1m"))

	for i := 0; i < 3; i++ {
		do(t, s, http.MethodGet, "/?json", nil, map[string]string{
			"Authorization": basicAuth(fmt.Sprintf("other-%d", i), "same-wrong"),
		})
	}

	good := do(t, s, http.MethodPost, "/fs/auth", nil, map[string]string{
		"Authorization": basicAuth("admin", "secret"),
		"X-Gofs-Ajax":   "1",
	})
	if good.code != http.StatusOK {
		t.Fatalf("IP 封禁期内正确密码登录返回 %d，期望 200（不能锁死合法用户）", good.code)
	}

	// 连续 3 次正确登录也必须全部成功（没有失败记录时不该有任何等待）。
	for i := 0; i < 3; i++ {
		if r := do(t, s, http.MethodPost, "/fs/auth", nil, map[string]string{
			"Authorization": basicAuth("admin", "secret"),
			"X-Gofs-Ajax":   "1",
		}); r.code != http.StatusOK {
			t.Fatalf("第 %d 次正确登录返回 %d，期望 200", i+1, r.code)
		}
	}
}

// TestAuthSprayDoesNotAffectTransfers 验证数据传输路径不受新限速影响：
// 即使来源 IP 已因认证失败进入封禁期，认证通过后的上传/下载仍然正常。
func TestAuthSprayDoesNotAffectTransfers(t *testing.T) {
	root := makeFixture(t)
	s := newTestServer(t, testConfig(t, root,
		"-a", "admin:secret@/:rw",
		"--allow-upload",
		"--auth-fail-limit", "100",
		"--auth-fail-ip-limit", "2",
		"--auth-fail-ip-window", "1m"))

	// 先把来源 IP 打进封禁期。
	for i := 0; i < 3; i++ {
		do(t, s, http.MethodGet, "/?json", nil, map[string]string{
			"Authorization": basicAuth(fmt.Sprintf("intruder-%d", i), "same-wrong"),
		})
	}
	if over, _ := s.authSprayThrottled("192.0.2.1"); !over {
		t.Fatal("前置条件不成立：来源 IP 未进入封禁期")
	}

	// 上传：走 authorize(/upload.bin)，不在 IP/全局维度的作用范围内。
	up := do(t, s, http.MethodPut, "/upload.bin", strings.NewReader("payload"),
		map[string]string{"Authorization": basicAuth("admin", "secret")})
	if up.code == http.StatusTooManyRequests {
		t.Fatal("认证通过的上传被 IP 限速误伤（429）")
	}
	if up.code != http.StatusCreated {
		t.Fatalf("上传返回 %d，期望 201", up.code)
	}

	// 下载：同样必须正常。
	down := do(t, s, http.MethodGet, "/upload.bin", nil,
		map[string]string{"Authorization": basicAuth("admin", "secret")})
	if down.code == http.StatusTooManyRequests {
		t.Fatal("认证通过的下载被 IP 限速误伤（429）")
	}
	if down.code != http.StatusOK || down.body != "payload" {
		t.Fatalf("下载返回 %d / %q，期望 200 / payload", down.code, down.body)
	}
}

// TestAuthSprayAlertIsStructuredAndMasked 验证超限告警：结构化、含来源 IP、
// 失败次数，且用户名只保留前缀。
func TestAuthSprayAlertIsStructuredAndMasked(t *testing.T) {
	root := makeFixture(t)
	s := newTestServer(t, testConfig(t, root,
		"-a", "admin:secret@/:rw",
		"--auth-fail-limit", "100",
		"--auth-fail-ip-limit", "3",
		"--auth-fail-ip-window", "1m"))

	var buf bytes.Buffer
	s.logger = &Logger{out: &buf}

	for i, user := range []string{"administrator", "adminops", "adminuser", "root"} {
		do(t, s, http.MethodGet, "/?json", nil, map[string]string{
			"Authorization": basicAuth(user, fmt.Sprintf("wrong-%d", i)),
		})
	}

	logs := buf.String()
	for _, want := range []string{
		`"event":"auth_fail_ip_threshold"`,
		`"src_ip":"192.0.2.1"`,
		`"failures":4`,
		`"limit":3`,
		`"adm…"`,
		`"roo…"`,
	} {
		if !strings.Contains(logs, want) {
			t.Errorf("告警日志缺少 %s，实际日志：\n%s", want, logs)
		}
	}
	if strings.Contains(logs, "administrator") || strings.Contains(logs, "adminops") {
		t.Errorf("告警日志泄露了完整用户名：\n%s", logs)
	}
}

// TestAuthSprayWindowSweep 验证过期清理：大量只失败一次的来源不会让 map
// 无限增长。
func TestAuthSprayWindowSweep(t *testing.T) {
	w := newAuthSprayWindow(2, 50*time.Millisecond)
	if w == nil {
		t.Fatal("窗口未创建")
	}
	now := time.Now()
	for i := 0; i < 5000; i++ {
		ip := fmt.Sprintf("10.%d.%d.%d", i/65536%256, i/256%256, i%256)
		w.record(ip, "u", now)
	}
	if len(w.entries) != 5000 {
		t.Fatalf("记录数 = %d，期望 5000", len(w.entries))
	}

	// 窗口已过：下一次记录会触发清理，旧的来源应被整批删除。
	time.Sleep(80 * time.Millisecond)
	w.record("198.51.100.1", "u", time.Now())
	if len(w.entries) > 1 {
		t.Fatalf("过期清理未生效：清理后仍有 %d 条记录", len(w.entries))
	}
}

// TestRetryAfterSeconds 钉住向上取整的边界。
func TestRetryAfterSeconds(t *testing.T) {
	cases := []struct {
		in   time.Duration
		want string
	}{
		{0, "1"},
		{-time.Second, "1"},
		{time.Millisecond, "1"},
		{time.Second, "1"},
		{time.Second + time.Millisecond, "2"},
		{61 * time.Second, "61"},
	}
	for _, c := range cases {
		if got := retryAfterSeconds(c.in); got != c.want {
			t.Errorf("retryAfterSeconds(%s) = %s，期望 %s", c.in, got, c.want)
		}
	}
}
