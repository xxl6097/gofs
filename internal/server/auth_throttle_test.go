package server

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// TestLoginRecoversAfterLockoutBackoff 验证账号锁定是「有期限」的。
//
// 新策略下锁定期内连正确密码也会直接 429（这正是锁定的含义）；
// 必须等退避期结束后才能重新登录。管理员可以用 --auth-lock-exempt
// 的恢复通道提前解锁（见 auth_lock_test.go）。
func TestLoginRecoversAfterLockoutBackoff(t *testing.T) {
	s := newTestServer(t, testConfig(t, t.TempDir(),
		"-a", "admin:secret@/:rw",
		"--auth-fail-limit", "3",
		"--auth-lock-base-delay", "80ms",
		"--auth-lock-max-delay", "1s"))

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
	// 阈值那一次仍回 401；下一次开始进入锁定。
	if code := attempt("wrong"); code != http.StatusTooManyRequests {
		t.Fatalf("锁定后的错误口令返回 %d，期望 429", code)
	}
	// 锁定期内不接受正确密码 —— 否则锁定没有意义。
	if code := attempt("secret"); code != http.StatusTooManyRequests {
		t.Fatalf("锁定期内正确密码返回 %d，期望 429", code)
	}

	// 等过首次退避时长后恢复。
	time.Sleep(120 * time.Millisecond)
	if code := attempt("secret"); code != http.StatusOK {
		t.Fatalf("退避期后正确密码返回 %d，期望 200", code)
	}
}
