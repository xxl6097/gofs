package server

import (
	"net/http"
	"strings"
	"testing"
)

// TestAuthFailLimitNotResetByOtherAccount 确认「持有任意有效账号」不能清零
// 别的账号的失败计数。
//
// 修复前计数只按 IP 记录：攻击者每猜错一次 boss，就用自己合法的 alice
// 账号发一次请求，整个 IP 的失败记录被删除，封禁永远不触发 —— 相当于
// 对 boss 拥有无限次口令猜测机会。
func TestAuthFailLimitNotResetByOtherAccount(t *testing.T) {
	root := makeFixture(t)
	s := newTestServer(t, testConfig(t, root, "-A",
		"-a", "alice:apw@/:r",
		"-a", "boss:boss-secret@/:rw",
		"--auth-fail-limit", "3",
		"--auth-fail-window", "5m"))

	alice := map[string]string{"Authorization": basicAuth("alice", "apw")}

	banned := false
	for i := 0; i < 12; i++ {
		// 猜错 boss 一次。
		got := do(t, s, http.MethodGet, "/?json", nil, map[string]string{
			"Authorization": basicAuth("boss", "wrong-"+strings.Repeat("x", i+1)),
		})
		if got.code == http.StatusTooManyRequests {
			banned = true
		}
		// 穿插一次 alice 的合法请求。修复前这一步会清空 IP 计数。
		if r := do(t, s, http.MethodGet, "/?json", nil, alice); r.code != http.StatusOK {
			t.Fatalf("alice 的合法请求返回 %d，期望 200", r.code)
		}
		if banned {
			break
		}
	}
	if !banned {
		t.Fatal("穿插其它账号的成功请求后始终没有触发封禁 —— 失败计数被跨账号清零了")
	}

	// 封禁只针对被猜测的账号，不应误伤同 IP 的其它账号。
	if r := do(t, s, http.MethodGet, "/?json", nil,
		map[string]string{"Authorization": basicAuth("boss", "boss-secret")}); r.code != http.StatusTooManyRequests {
		t.Fatalf("被封禁账号的正确密码返回 %d，期望 429", r.code)
	}
	if r := do(t, s, http.MethodGet, "/?json", nil, alice); r.code != http.StatusOK {
		t.Fatalf("封禁误伤了同 IP 的其它账号：alice 返回 %d", r.code)
	}
}
