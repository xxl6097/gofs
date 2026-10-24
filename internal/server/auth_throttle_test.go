package server

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestLoginCorrectPasswordNotLockedOut 是「账号锁定 DoS」的回归测试。
//
// 攻击者不需要任何有效凭据，只要知道用户名，连续发错误口令：
//   - 超过阈值后，猜错的请求仍然被限速（429）；
//   - 但**正确密码必须能登录**。早先的实现在整个统计窗口内直接拒绝，
//     十次乱编的口令就能让 admin 连自己的正确密码都登不上。
func TestLoginCorrectPasswordNotLockedOut(t *testing.T) {
	s := newTestServer(t, testConfig(t, t.TempDir(),
		"-a", "admin:secret@/:rw",
		"--auth-fail-limit", "3",
		"--auth-fail-window", "5m"))

	attempt := func(password string) int {
		r := httptest.NewRequest(http.MethodGet, "/fs/auth", nil)
		r.SetBasicAuth("admin", password)
		r.Header.Set("X-Gofs-Ajax", "1")
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, r)
		return w.Code
	}

	for i := 0; i < 3; i++ {
		if code := attempt("wrong"); code != http.StatusUnauthorized {
			t.Fatalf("第 %d 次错误口令返回 %d，期望 401", i+1, code)
		}
	}
	// 达到阈值后的下一次错误请求应被限速（先等待，再校验，再回 429）。
	if code := attempt("wrong"); code != http.StatusTooManyRequests {
		t.Fatalf("超限后的错误口令返回 %d，期望 429", code)
	}

	// 关键断言：正确密码不被别人的错误尝试锁死。
	if code := attempt("secret"); code != http.StatusOK {
		t.Fatalf("正确密码被错误口令锁死，返回 %d（期望 200）", code)
	}
}
