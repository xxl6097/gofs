package server

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/xxl6097/gofs/internal/config"
)

// secureConfig 返回一个**没有** --allow-insecure-auth 的测试配置。
//
// testConfig 默认打开明文认证，避免几百个既有用例都要改；传输安全的
// 默认值由这里单独覆盖。
func secureConfig(t *testing.T, extra ...string) *Server {
	t.Helper()
	cfg := testConfig(t, t.TempDir(), extra...)
	cfg.AllowInsecureAuth = false
	return newTestServer(t, cfg)
}

// doFrom 与 do 相同，但能指定直连对端地址（用于回环/代理场景）。
func doFrom(t *testing.T, s *Server, remote, method, target string, headers map[string]string) resp {
	t.Helper()
	r := httptest.NewRequest(method, target, nil)
	r.RemoteAddr = remote
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	res := w.Result()
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return resp{code: res.StatusCode, body: string(b), head: res.Header}
}

// TestHealthRequiresAuth 确认启用鉴权后 /fs/health 不再对匿名开放。
func TestHealthRequiresAuth(t *testing.T) {
	s := newTestServer(t, testConfig(t, t.TempDir(), "-a", "admin:secret@/:rw"))

	if got := do(t, s, http.MethodGet, "/fs/health", nil,
		map[string]string{"X-Gofs-Ajax": "1"}); got.code != http.StatusUnauthorized {
		t.Fatalf("匿名健康检查返回 %d，期望 401", got.code)
	}
	if got := do(t, s, http.MethodGet, "/fs/health", nil, map[string]string{
		"Authorization": basicAuth("admin", "secret"),
		"X-Gofs-Ajax":   "1",
	}); got.code != http.StatusOK {
		t.Fatalf("登录后的健康检查返回 %d：%s", got.code, got.body)
	}

	token, ok := s.auth.NewSession("admin")
	if !ok {
		t.Fatal("NewSession 失败")
	}
	if got := do(t, s, http.MethodGet, "/fs/health", nil, map[string]string{
		"Cookie":      sessionCookieName + "=" + token,
		"X-Gofs-Ajax": "1",
	}); got.code != http.StatusOK {
		t.Fatalf("会话身份的健康检查返回 %d：%s", got.code, got.body)
	}
}

// TestPlainHTTPRejectsBasicCredentials 是本次修复的核心：
// 默认配置下，非回环的明文 HTTP 不允许提交 Basic/查询串口令。
func TestPlainHTTPRejectsBasicCredentials(t *testing.T) {
	s := secureConfig(t, "-a", "admin:secret@/:rw")

	got := do(t, s, http.MethodGet, "/fs/auth", nil, map[string]string{
		"Authorization": basicAuth("admin", "secret"),
		"X-Gofs-Ajax":   "1",
	})
	if got.code != http.StatusForbidden {
		t.Fatalf("明文 HTTP 的 Basic 登录返回 %d，期望 403：%s", got.code, got.body)
	}
	if !strings.Contains(got.body, "明文 HTTP") {
		t.Errorf("403 响应没有说明原因：%s", got.body)
	}
	if c := got.head.Get("Set-Cookie"); c != "" {
		t.Errorf("明文 HTTP 上的登录请求不应产生任何 Set-Cookie：%s", c)
	}

	// 查询串口令同样是明文，必须拒绝。
	if got := do(t, s, http.MethodGet, "/?user=admin&pass=secret", nil, nil); got.code != http.StatusForbidden {
		t.Fatalf("明文 HTTP 的查询串凭据返回 %d，期望 403", got.code)
	}

	// 即使账号密码正确，健康检查也不能绕过传输限制。
	if got := do(t, s, http.MethodGet, "/fs/health", nil, map[string]string{
		"Authorization": basicAuth("admin", "secret"),
		"X-Gofs-Ajax":   "1",
	}); got.code != http.StatusForbidden {
		t.Fatalf("明文 HTTP 的健康检查 Basic 返回 %d，期望 403", got.code)
	}
}

// TestPlainHTTPIgnoresSessionCookie 确认抓包得到的会话 cookie 不能在
// 明文连接上复用；同时不能把用户直接挡在登录页之外。
func TestPlainHTTPIgnoresSessionCookie(t *testing.T) {
	s := secureConfig(t, "-a", "admin:secret@/:rw")
	token, ok := s.auth.NewSession("admin")
	if !ok {
		t.Fatal("NewSession 失败")
	}

	got := do(t, s, http.MethodGet, "/fs/auth", nil, map[string]string{
		"Cookie":      sessionCookieName + "=" + token,
		"X-Gofs-Ajax": "1",
	})
	if got.code != http.StatusUnauthorized {
		t.Fatalf("明文 HTTP 上用会话 cookie 刷新身份返回 %d，期望 401", got.code)
	}
	raw := strings.Join(got.head.Values("Set-Cookie"), "\n")
	if !strings.Contains(raw, sessionCookieName+"=;") &&
		!strings.Contains(raw, "Max-Age=0") &&
		!strings.Contains(raw, "Max-Age=-1") {
		t.Errorf("明文 HTTP 上应清掉不可信的会话 cookie：%q", raw)
	}

	// 受保护的文件请求也一样：会话不被采信。
	if got := do(t, s, http.MethodGet, "/?json", nil, map[string]string{
		"Cookie": sessionCookieName + "=" + token,
	}); got.code == http.StatusOK {
		t.Fatalf("明文 HTTP 上用抓包得到的 cookie 读到了目录内容")
	}
}

// TestLoopbackAllowsPlainHTTPAuth 确认本机回环不受影响：默认监听
// 127.0.0.1 的本地部署仍然可以用 Basic 登录。
func TestLoopbackAllowsPlainHTTPAuth(t *testing.T) {
	s := secureConfig(t, "-a", "admin:secret@/:rw")

	got := doFrom(t, s, "127.0.0.1:1234", http.MethodGet, "/fs/auth",
		map[string]string{
			"Authorization": basicAuth("admin", "secret"),
			"X-Gofs-Ajax":   "1",
		})
	if got.code != http.StatusOK {
		t.Fatalf("回环地址的 Basic 登录返回 %d，期望 200：%s", got.code, got.body)
	}
	if got := doFrom(t, s, "127.0.0.1:1234", http.MethodGet, "/fs/health",
		map[string]string{
			"Authorization": basicAuth("admin", "secret"),
			"X-Gofs-Ajax":   "1",
		}); got.code != http.StatusOK {
		t.Fatalf("回环地址的健康检查返回 %d，期望 200", got.code)
	}
}

// TestTrustedProxyProtoControlsAuth 确认 TLS 终止在可信代理时，
// 只有代理明确声明 https 才接受口令。
func TestTrustedProxyProtoControlsAuth(t *testing.T) {
	cfg := testConfig(t, t.TempDir(), "-a", "admin:secret@/:rw")
	cfg.AllowInsecureAuth = false
	cfg.TrustedProxies = []string{"192.0.2.0/24"}
	s := newTestServer(t, cfg)

	httpsReq := map[string]string{
		"Authorization":     basicAuth("admin", "secret"),
		"X-Gofs-Ajax":       "1",
		"X-Forwarded-For":   "203.0.113.7",
		"X-Forwarded-Proto": "https",
	}
	if got := doFrom(t, s, "192.0.2.1:1234", http.MethodGet, "/fs/auth", httpsReq); got.code != http.StatusOK {
		t.Fatalf("可信代理声明 https 时登录返回 %d，期望 200：%s", got.code, got.body)
	}

	httpReq := map[string]string{
		"Authorization":     basicAuth("admin", "secret"),
		"X-Gofs-Ajax":       "1",
		"X-Forwarded-For":   "203.0.113.7",
		"X-Forwarded-Proto": "http",
	}
	if got := doFrom(t, s, "192.0.2.1:1234", http.MethodGet, "/fs/auth", httpReq); got.code != http.StatusForbidden {
		t.Fatalf("可信代理声明 http 时登录返回 %d，期望 403", got.code)
	}
}

// TestDefaultConfigAllowsPlainHTTPAuth 确认默认配置下远程明文 HTTP 也能
// 登录（HTTP 开箱即用）。收紧模式见 secureConfig 的测试。
func TestDefaultConfigAllowsPlainHTTPAuth(t *testing.T) {
	root := t.TempDir()
	cfg, err := config.Parse([]string{
		"-b", "127.0.0.1", "-p", "0", "--log-format", "none",
		"--no-upload-dated", "--key-file", "", "--user-file", "", "--settings-file", "",
		"-a", "admin:secret@/:rw", root,
	}, io.Discard)
	if err != nil {
		t.Fatalf("解析配置失败: %v", err)
	}
	if !cfg.AllowInsecureAuth {
		t.Fatal("默认 AllowInsecureAuth 应为 true（HTTP 开箱即用）")
	}
	s := newTestServer(t, cfg)

	if got := do(t, s, http.MethodGet, "/fs/auth", nil, map[string]string{
		"Authorization": basicAuth("admin", "secret"),
		"X-Gofs-Ajax":   "1",
	}); got.code != http.StatusOK {
		t.Fatalf("默认配置下远程明文 Basic 登录返回 %d，期望 200：%s", got.code, got.body)
	}
}

// TestAllowInsecureAuthEscapeHatch 确认显式开关仍然可用（受控内网）。
func TestAllowInsecureAuthEscapeHatch(t *testing.T) {
	s := newTestServer(t, testConfig(t, t.TempDir(), "-a", "admin:secret@/:rw"))
	if got := do(t, s, http.MethodGet, "/fs/auth", nil, map[string]string{
		"Authorization": basicAuth("admin", "secret"),
		"X-Gofs-Ajax":   "1",
	}); got.code != http.StatusOK {
		t.Fatalf("--allow-insecure-auth 下登录返回 %d，期望 200", got.code)
	}
}
