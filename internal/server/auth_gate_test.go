package server

import (
	"net/http"
	"strings"
	"testing"
)

// TestAuthGatePrecheckIsRateLimited 确认应用外壳预检不会成为
// 「不限速的 PBKDF2 入口」：连续错误口令必须先触发 429。
func TestAuthGatePrecheckIsRateLimited(t *testing.T) {
	root := t.TempDir()
	s := newTestServer(t, testConfig(t, root,
		"--auth-fail-limit", "2",
		"-a", "boss:pw@/:rw"))

	var last int
	for i := 0; i < 8; i++ {
		last = do(t, s, http.MethodGet, "/", nil, map[string]string{
			"Accept":        "text/html",
			"Authorization": basicAuth("boss", "wrong-"+strings.Repeat("x", i+1)),
		}).code
		if last == http.StatusTooManyRequests {
			break
		}
	}
	if last != http.StatusTooManyRequests {
		t.Fatalf("外壳预检未触发认证失败限速，最后状态 %d", last)
	}
}
