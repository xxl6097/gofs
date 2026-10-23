package server

import (
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/xxl6097/gofs/internal/auth"
	"github.com/xxl6097/gofs/internal/uploadkey"
)

// TestPasswordChangeEndpoint 覆盖账号自助改密的完整流程：
// 未登录拒绝、旧密码错误拒绝、成功后旧密码/旧会话/旧密钥全部失效、
// 新密码可以登录，并且改动落盘。
func TestPasswordChangeEndpoint(t *testing.T) {
	root := t.TempDir()
	userFile := filepath.Join(t.TempDir(), "users.json")
	s := newTestServer(t, testConfig(t, root,
		"-a", "admin:secret@/:rw",
		"--user-file", userFile,
		"--auth-fail-limit", "0"))

	if err := s.auth.AddUser("alice", "alicepass", []auth.Rule{
		{Path: "/docs", Perm: auth.PermReadWrite},
	}); err != nil {
		t.Fatalf("AddUser: %v", err)
	}
	// alice 此前签发的一把上传密钥：改密后必须一并撤销。
	key, _, err := s.keys.Create(uploadkey.CreateOptions{
		Name: "ci", Scope: "/docs", Creator: "alice",
	})
	if err != nil {
		t.Fatalf("创建密钥: %v", err)
	}

	cookie := login(t, s, "alice", "alicepass")
	post := func(c *http.Cookie, body string) resp {
		headers := map[string]string{"Content-Type": "application/json"}
		if c != nil {
			headers["Cookie"] = c.Name + "=" + c.Value
		}
		return do(t, s, http.MethodPost, "/fs/auth/password",
			strings.NewReader(body), headers)
	}

	// 没有会话也没有 Basic 凭据：401。
	if got := post(nil, `{"current_password":"alicepass","new_password":"newpass123"}`); got.code != http.StatusUnauthorized {
		t.Fatalf("未登录改密返回 %d，期望 401：%s", got.code, got.body)
	}

	// 旧密码不对：403，且原密码、原会话、密钥都不能被这次失败影响。
	got := post(cookie, `{"current_password":"wrong-pass","new_password":"newpass123"}`)
	if got.code != http.StatusForbidden {
		t.Fatalf("旧密码错误返回 %d，期望 403：%s", got.code, got.body)
	}
	if !s.auth.Verify("alice", "alicepass") {
		t.Fatal("校验失败的改密不应改动密码")
	}
	if _, ok := s.keys.Get(key.ID); !ok {
		t.Fatal("校验失败的改密不应撤销密钥")
	}
	if r := withCookie(t, s, http.MethodGet, "/fs/auth", cookie, nil); r.code != http.StatusOK {
		t.Fatalf("校验失败的改密不应让会话失效：%d", r.code)
	}

	// 正确改密：204、清 cookie、撤销密钥、旧会话失效。
	got = post(cookie, `{"current_password":"alicepass","new_password":"newpass123"}`)
	if got.code != http.StatusNoContent {
		t.Fatalf("改密返回 %d，期望 204：%s", got.code, got.body)
	}
	raw := strings.Join(got.head.Values("Set-Cookie"), "\n")
	if !strings.Contains(raw, sessionCookieName+"=;") &&
		!strings.Contains(raw, "Max-Age=0") &&
		!strings.Contains(raw, "Max-Age=-1") {
		t.Errorf("改密响应没有清掉会话 cookie：%q", raw)
	}
	if _, ok := s.keys.Get(key.ID); ok {
		t.Error("改密后该账号此前签发的密钥仍然有效")
	}
	if r := withCookie(t, s, http.MethodGet, "/fs/auth", cookie, nil); r.code != http.StatusUnauthorized {
		t.Errorf("改密后旧会话仍然有效：%d", r.code)
	}

	// 旧密码登不上、新密码能登上。
	if r := do(t, s, http.MethodGet, "/fs/auth", nil, map[string]string{
		"Authorization": basicAuth("alice", "alicepass"),
		"X-Gofs-Ajax":   "1",
	}); r.code != http.StatusUnauthorized {
		t.Errorf("旧密码仍然能登录：%d", r.code)
	}
	if r := do(t, s, http.MethodGet, "/fs/auth", nil, map[string]string{
		"Authorization": basicAuth("alice", "newpass123"),
		"X-Gofs-Ajax":   "1",
	}); r.code != http.StatusOK {
		t.Errorf("新密码登录失败：%d %s", r.code, r.body)
	} else if !strings.Contains(r.body, `"user_from_startup":false`) {
		t.Errorf("用户表账号的登录响应应标记 user_from_startup=false：%s", r.body)
	}

	// 落盘：重新加载用户表仍认新密码。
	reloaded, err := auth.Open([]string{"admin:secret@/:rw"}, userFile)
	if err != nil {
		t.Fatalf("重新加载用户表失败: %v", err)
	}
	if !reloaded.Verify("alice", "newpass123") {
		t.Error("重启后新密码无效 —— 用户表没有落盘")
	}
}

// TestPasswordChangeWithBasicAuth 确认非浏览器客户端可以只用 Authorization
// 头改密（不需要先换会话 cookie）。
func TestPasswordChangeWithBasicAuth(t *testing.T) {
	s := newTestServer(t, testConfig(t, t.TempDir(), "-a", "admin:secret@/:rw"))
	if err := s.auth.AddUser("bob", "bobpass1", []auth.Rule{
		{Path: "/", Perm: auth.PermReadWrite},
	}); err != nil {
		t.Fatalf("AddUser: %v", err)
	}

	got := do(t, s, http.MethodPost, "/fs/auth/password",
		strings.NewReader(`{"current_password":"bobpass1","new_password":"bobpass2"}`),
		map[string]string{
			"Authorization": basicAuth("bob", "bobpass1"),
			"Content-Type":  "application/json",
		})
	if got.code != http.StatusNoContent {
		t.Fatalf("Basic 改密返回 %d：%s", got.code, got.body)
	}
	if !s.auth.Verify("bob", "bobpass2") {
		t.Error("Basic 改密后新密码无效")
	}
}

// TestPasswordChangeRejectsStartupAccount 确认来自启动参数的账号不能在
// 运行期改密 —— 它的密码以命令行为准，改了也会在重启时被覆盖。
func TestPasswordChangeRejectsStartupAccount(t *testing.T) {
	s := newTestServer(t, testConfig(t, t.TempDir(), "-a", "admin:secret@/:rw"))
	cookie := login(t, s, "admin", "secret")

	got := do(t, s, http.MethodPost, "/fs/auth/password",
		strings.NewReader(`{"current_password":"secret","new_password":"newpass123"}`),
		map[string]string{
			"Cookie":       cookie.Name + "=" + cookie.Value,
			"Content-Type": "application/json",
		})
	if got.code != http.StatusBadRequest || !strings.Contains(got.body, "启动参数") {
		t.Fatalf("启动参数账号改密返回 %d：%s", got.code, got.body)
	}
	// 被拒绝的请求不能把会话弄丢。
	if r := withCookie(t, s, http.MethodGet, "/fs/auth", cookie, nil); r.code != http.StatusOK {
		t.Errorf("被拒绝的改密让会话失效了：%d", r.code)
	}

	// Basic 密码不对时必须先回 401，不能因为用户名是启动参数账号就回
	// 「来自启动参数」—— 那等于用一个不需要口令的错误泄露账号是否存在。
	if r := do(t, s, http.MethodPost, "/fs/auth/password",
		strings.NewReader(`{"current_password":"x","new_password":"newpass123"}`),
		map[string]string{
			"Authorization": basicAuth("admin", "wrong"),
			"Content-Type":  "application/json",
		}); r.code != http.StatusUnauthorized {
		t.Errorf("Basic 密码错误返回 %d，期望 401：%s", r.code, r.body)
	}

	// 只有 POST。
	if r := do(t, s, http.MethodGet, "/fs/auth/password", nil, nil); r.code != http.StatusMethodNotAllowed {
		t.Errorf("GET 改密接口返回 %d，期望 405", r.code)
	}

	// 登录响应要告诉前端「这是启动参数账号」，否则页面会画出一个
	// 点了必然失败的「改密」入口。
	if r := do(t, s, http.MethodGet, "/fs/auth", nil, map[string]string{
		"Authorization": basicAuth("admin", "secret"),
		"X-Gofs-Ajax":   "1",
	}); !strings.Contains(r.body, `"user_from_startup":true`) {
		t.Errorf("启动参数账号的登录响应缺少 user_from_startup=true：%s", r.body)
	}
}
