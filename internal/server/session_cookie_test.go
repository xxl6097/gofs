package server

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// newSessionServer 起一个开了鉴权（admin/secret 可读写根目录）、服务 root 的服务。
func newSessionServer(t *testing.T, root string, extra ...string) *Server {
	t.Helper()
	args := []string{"-a", "admin:secret@/:rw"}
	args = append(args, extra...)
	return newTestServer(t, testConfig(t, root, args...))
}

// login 走一次登录接口，返回会话 cookie。
func login(t *testing.T, s *Server, user, pass string) *http.Cookie {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, "/__gofs__/auth", nil)
	r.SetBasicAuth(user, pass)
	r.Header.Set("X-Gofs-Ajax", "1")
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	res := w.Result()
	defer res.Body.Close()

	if res.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(res.Body)
		t.Fatalf("登录返回 %d：%s", res.StatusCode, b)
	}
	for _, c := range res.Cookies() {
		if c.Name == sessionCookieName {
			return c
		}
	}
	t.Fatal("登录响应里没有会话 cookie")
	return nil
}

// withCookie 发一个带会话 cookie 的请求。
func withCookie(t *testing.T, s *Server, method, target string, c *http.Cookie, headers map[string]string) resp {
	t.Helper()
	r := httptest.NewRequest(method, target, nil)
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	if c != nil {
		r.AddCookie(c)
	}
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	res := w.Result()
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return resp{code: res.StatusCode, body: string(b), head: res.Header}
}

// TestLoginSetsSessionCookie 覆盖 cookie 本身的属性。
//
// HttpOnly 与 SameSite 是这套方案的安全前提，写错了不会有人发现 ——
// 功能全都照常工作，只是防护没了。所以在这里钉死。
func TestLoginSetsSessionCookie(t *testing.T) {
	root := t.TempDir()
	s := newSessionServer(t, root)

	r := httptest.NewRequest(http.MethodGet, "/__gofs__/auth", nil)
	r.SetBasicAuth("admin", "secret")
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	res := w.Result()
	defer res.Body.Close()

	if res.StatusCode != http.StatusOK {
		t.Fatalf("登录返回 %d", res.StatusCode)
	}
	// 直接看原始 Set-Cookie 头 —— 有些属性在解析成 http.Cookie 时体现不出来。
	raw := strings.Join(res.Header.Values("Set-Cookie"), "\n")
	if raw == "" {
		t.Fatal("登录响应没有 Set-Cookie")
	}
	for _, want := range []string{sessionCookieName + "=", "HttpOnly", "SameSite=Lax", "Path=/"} {
		if !strings.Contains(raw, want) {
			t.Errorf("Set-Cookie 缺少 %q：%s", want, raw)
		}
	}
	// 纯 HTTP 下不能带 Secure，否则浏览器根本不会存这个 cookie。
	if strings.Contains(raw, "Secure") {
		t.Errorf("纯 HTTP 部署不该带 Secure：%s", raw)
	}

	c := login(t, s, "admin", "secret")
	if c.Value == "" {
		t.Fatal("cookie 值为空")
	}
	if !c.HttpOnly {
		t.Error("cookie 不是 HttpOnly —— JS 就能把 token 读走")
	}
}

// TestSessionCookieAuthenticatesRequests 是本次改动的核心：
// **不带任何 Authorization 头**、只带会话 cookie 的请求，也要能读到文件。
//
// 这正是浏览器加载 <video src> 时的情况 —— 没有 cookie 方案，
// 那个请求只会拿到 401，大视频也就没法流式播放。
func TestSessionCookieAuthenticatesRequests(t *testing.T) {
	root := t.TempDir()
	payload := strings.Repeat("gofs", 4096)
	if err := os.WriteFile(filepath.Join(root, "movie.bin"), []byte(payload), 0o644); err != nil {
		t.Fatalf("准备文件: %v", err)
	}
	s := newSessionServer(t, root)

	// 不带 cookie：应当 401。
	if got := withCookie(t, s, http.MethodGet, "/movie.bin", nil, nil); got.code != http.StatusUnauthorized {
		t.Fatalf("不带 cookie 返回 %d，期望 401", got.code)
	}

	c := login(t, s, "admin", "secret")
	got := withCookie(t, s, http.MethodGet, "/movie.bin", c, nil)
	if got.code != http.StatusOK {
		t.Fatalf("带 cookie 返回 %d，期望 200", got.code)
	}
	if got.body != payload {
		t.Fatalf("内容不对：拿到 %d 字节，期望 %d", len(got.body), len(payload))
	}
}

// TestSessionCookieSupportsRangeRequests 是至关重要的那一条。
//
// 大视频能播的前提就是 206 + Content-Range：播放器靠它跳转、拖动进度条。
// 如果这里退化成一个不带 Range 支持的 200，视频仍然「能打开」，
// 但拖不动、而且整个文件会被当成一个响应体发出去 —— 4.6 GiB 直接废掉。
func TestSessionCookieSupportsRangeRequests(t *testing.T) {
	root := t.TempDir()
	payload := []byte("0123456789abcdefghijklmnopqrstuvwxyz")
	if err := os.WriteFile(filepath.Join(root, "movie.bin"), payload, 0o644); err != nil {
		t.Fatalf("准备文件: %v", err)
	}
	s := newSessionServer(t, root)
	c := login(t, s, "admin", "secret")

	got := withCookie(t, s, http.MethodGet, "/movie.bin", c,
		map[string]string{"Range": "bytes=10-19"})
	if got.code != http.StatusPartialContent {
		t.Fatalf("Range 请求返回 %d，期望 206", got.code)
	}
	if got.body != "abcdefghij" {
		t.Fatalf("分片内容 = %q，期望 %q", got.body, "abcdefghij")
	}
	if cr := got.head.Get("Content-Range"); !strings.HasPrefix(cr, "bytes 10-19/") {
		t.Fatalf("Content-Range = %q，不像是正确的分片响应", cr)
	}
	if ar := got.head.Get("Accept-Ranges"); ar != "bytes" {
		t.Fatalf("Accept-Ranges = %q，期望 bytes", ar)
	}
}

// TestLogoutRevokesSession 确认登出之后 cookie 立刻作废。
func TestLogoutRevokesSession(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "a.txt"), []byte("hi"), 0o644); err != nil {
		t.Fatalf("准备文件: %v", err)
	}
	s := newSessionServer(t, root)
	c := login(t, s, "admin", "secret")

	if got := withCookie(t, s, http.MethodGet, "/a.txt", c, nil); got.code != http.StatusOK {
		t.Fatalf("登出前返回 %d，期望 200", got.code)
	}

	// 登出：请求里带上要撤销的那张 cookie。
	r := httptest.NewRequest(http.MethodDelete, "/__gofs__/auth", nil)
	r.AddCookie(c)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	res := w.Result()
	_ = res.Body.Close()
	if res.StatusCode != http.StatusNoContent {
		t.Fatalf("登出返回 %d，期望 204", res.StatusCode)
	}
	// 响应要带一个清空 cookie 的头。
	raw := strings.Join(res.Header.Values("Set-Cookie"), "\n")
	if !strings.Contains(raw, sessionCookieName+"=;") && !strings.Contains(raw, "Max-Age=0") && !strings.Contains(raw, "Max-Age=-1") {
		t.Errorf("登出没有清 cookie：%q", raw)
	}

	// 同一张 cookie 不能再用 —— 这是「退出登录」的实质。
	if got := withCookie(t, s, http.MethodGet, "/a.txt", c, nil); got.code != http.StatusUnauthorized {
		t.Fatalf("登出后同一 cookie 返回 %d，期望 401", got.code)
	}
}

// TestForgeCookieRejected 确认乱编的 cookie 进不来。
func TestForgeCookieRejected(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "a.txt"), []byte("hi"), 0o644); err != nil {
		t.Fatalf("准备文件: %v", err)
	}
	s := newSessionServer(t, root)

	for _, v := range []string{"guess", strings.Repeat("A", 43), "admin"} {
		c := &http.Cookie{Name: sessionCookieName, Value: v}
		if got := withCookie(t, s, http.MethodGet, "/a.txt", c, nil); got.code != http.StatusUnauthorized {
			t.Fatalf("伪造 cookie %q 返回 %d，期望 401", v, got.code)
		}
	}
}

// TestSessionCookiePathFollowsPrefix 确认部署在访问前缀下时 cookie 路径正确。
//
// 写错的话症状很隐蔽：登录看起来成功，但前缀下的请求收不到 cookie，
// 于是「登录了却一直 401」。
func TestSessionCookiePathFollowsPrefix(t *testing.T) {
	root := t.TempDir()
	s := newSessionServer(t, root, "--path-prefix", "/files")

	r := httptest.NewRequest(http.MethodGet, "/files/__gofs__/auth", nil)
	r.SetBasicAuth("admin", "secret")
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	res := w.Result()
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("登录返回 %d", res.StatusCode)
	}
	raw := strings.Join(res.Header.Values("Set-Cookie"), "\n")
	if !strings.Contains(raw, "Path=/files/") {
		t.Fatalf("cookie 路径没跟随前缀：%q", raw)
	}
}

// TestLoginRateLimited 确认登录接口有限速闸门。
//
// 改动前它没有 —— 而这个接口现在会下发一张 30 天有效的通行证，
// 不限速的话字典攻击的收益比逐个请求试密码高得多。
func TestLoginRateLimited(t *testing.T) {
	root := t.TempDir()
	s := newSessionServer(t, root, "--auth-fail-limit", "3", "--auth-fail-window", "5m")

	attempt := func() int {
		r := httptest.NewRequest(http.MethodGet, "/__gofs__/auth", nil)
		r.SetBasicAuth("admin", "wrong-password")
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, r)
		res := w.Result()
		_ = res.Body.Close()
		return res.StatusCode
	}
	// 前几次是 401（密码错），到点之后应当变成 429。
	codes := make([]int, 0, 8)
	for i := 0; i < 8; i++ {
		codes = append(codes, attempt())
	}
	last := codes[len(codes)-1]
	if last != http.StatusTooManyRequests {
		t.Fatalf("连续失败后仍未限速，状态码序列：%v", codes)
	}
	for i, c := range codes {
		if c != http.StatusUnauthorized && c != http.StatusTooManyRequests {
			t.Fatalf("第 %d 次返回 %d，序列：%v", i, c, codes)
		}
	}
}

// TestCookieDoesNotLeakAcrossUsers 确认会话身份不会被显式凭据串台。
//
// credsOf 在带显式凭据时**不**返回 cookieUser。若实现成「两者合并」，
// 一个只读账号配上一张管理员 cookie 就可能拿到管理员权限。
func TestCookieDoesNotLeakAcrossUsers(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "a.txt"), []byte("hi"), 0o644); err != nil {
		t.Fatalf("准备文件: %v", err)
	}
	// admin 可读写根；guest 只读根。
	s := newSessionServer(t, root)

	adminCookie := login(t, s, "admin", "secret")

	// 带着 admin 的 cookie，但显式用另一个不存在的账号发请求。
	// 显式凭据优先 —— 结果必须是 401，而不是「悄悄用 cookie 里的 admin 放行」。
	r := httptest.NewRequest(http.MethodGet, "/a.txt", nil)
	r.AddCookie(adminCookie)
	r.SetBasicAuth("admin", "wrong-password")
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	res := w.Result()
	_ = res.Body.Close()
	if res.StatusCode == http.StatusOK {
		t.Fatal("显式凭据错误，却因为带了有效 cookie 而被放行")
	}
}

// TestCookieAuthorizesWrite 确认会话身份不只是能读，写权限也照常生效。
func TestCookieAuthorizesWrite(t *testing.T) {
	root := t.TempDir()
	s := newSessionServer(t, root, "--allow-upload")
	c := login(t, s, "admin", "secret")

	r := httptest.NewRequest(http.MethodPut, "/new.txt", strings.NewReader("data"))
	r.AddCookie(c)
	r.Header.Set("X-Gofs-Ajax", "1")
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	res := w.Result()
	_ = res.Body.Close()
	if res.StatusCode != http.StatusCreated && res.StatusCode != http.StatusNoContent && res.StatusCode != http.StatusOK {
		t.Fatalf("用 cookie 上传返回 %d，期望成功", res.StatusCode)
	}
	if _, err := os.Stat(filepath.Join(root, "new.txt")); err != nil {
		t.Fatalf("文件没有落盘: %v", err)
	}
}

// TestSessionSurvivesAcrossRequests 确认同一张 cookie 能被反复使用
// （而不是一次性的），否则页面里每个媒体请求都会失败。
func TestSessionSurvivesAcrossRequests(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "a.txt"), []byte("hi"), 0o644); err != nil {
		t.Fatalf("准备文件: %v", err)
	}
	s := newSessionServer(t, root)
	c := login(t, s, "admin", "secret")

	for i := 0; i < 5; i++ {
		if got := withCookie(t, s, http.MethodGet, "/a.txt", c, nil); got.code != http.StatusOK {
			t.Fatalf("第 %d 次请求返回 %d，期望 200（会话不该是一次性的）", i, got.code)
		}
	}
	// 顺带确认没有为每次请求都新建会话。
	if n := s.auth.SessionCount(); n != 1 {
		t.Fatalf("会话数 = %d，期望 1", n)
	}
}
