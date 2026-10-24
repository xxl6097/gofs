package server

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestCorrectPasswordWorksDuringLockout 验证新策略：锁定只改变错误口令的
// 响应（401 -> 429），正确口令始终能登录，并在成功后清零当前来源的锁。
func TestCorrectPasswordWorksDuringLockout(t *testing.T) {
	s := newTestServer(t, testConfig(t, t.TempDir(),
		"-a", "admin:secret@/:rw",
		"--auth-fail-limit", "3",
		"--auth-lock-base-delay", "5m",
		"--auth-lock-max-delay", "30m"))

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
	// 阈值那一次仍回 401；下一次失败进入「错误口令回 429」的状态。
	if code := attempt("wrong"); code != http.StatusTooManyRequests {
		t.Fatalf("锁定后的错误口令返回 %d，期望 429", code)
	}
	// 正确密码不受锁定影响。
	if code := attempt("secret"); code != http.StatusOK {
		t.Fatalf("锁定期内正确密码返回 %d，期望 200", code)
	}
	// 成功后计数清零，再输错回到普通 401。
	if code := attempt("wrong"); code != http.StatusUnauthorized {
		t.Fatalf("成功清零后的错误口令返回 %d，期望 401", code)
	}
}
