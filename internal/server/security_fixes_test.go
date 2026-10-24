package server

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/xxl6097/gofs/internal/archive"
	"github.com/xxl6097/gofs/internal/uploadkey"
)

// TestSandboxUsesActualContentType 确认沙箱判据是最终 Content-Type，
// 而不是只看扩展名白名单。
//
// .shtml / .xht 是浏览器会当文档执行的类型，.md 与无扩展名文件则会被
// http.ServeContent 嗅探成 text/html；只认 .html 的旧实现会让它们
// 在同源下执行脚本。
func TestSandboxUsesActualContentType(t *testing.T) {
	root := makeFixture(t)
	payload := []byte("<!DOCTYPE html><script>alert(1)</script>")
	for _, name := range []string{"evil.shtml", "evil.xht", "evil.md", "evil"} {
		writeFile(t, filepath.Join(root, name), payload)
	}
	writeFile(t, filepath.Join(root, "plain.txt"), []byte("just text"))

	s := newTestServer(t, testConfig(t, root, "-A"))
	for _, name := range []string{"evil.shtml", "evil.xht", "evil.md", "evil"} {
		r := do(t, s, http.MethodGet, "/"+name, nil, nil)
		csp := r.head.Get("Content-Security-Policy")
		if !strings.Contains(csp, "sandbox") {
			t.Errorf("%s 缺少 CSP sandbox，实际 = %q", name, csp)
		}
		if !strings.Contains(csp, "allow-scripts") {
			t.Errorf("%s 应允许脚本但不允许同源，实际 = %q", name, csp)
		}
		if strings.Contains(csp, "allow-same-origin") {
			t.Errorf("%s 的沙箱带了 allow-same-origin，实际 = %q", name, csp)
		}
	}
	if csp := do(t, s, http.MethodGet, "/plain.txt", nil, nil).head.Get("Content-Security-Policy"); strings.Contains(csp, "sandbox") {
		t.Errorf("普通文本不应被沙箱化，实际 = %q", csp)
	}
}

// TestAdminEndpointsAreRateLimited 确认 /fs/settings 这类「也能验密码」的
// 接口与 authorize 共用失败计数，而不是一条不限速的口令猜测入口。
func TestAdminEndpointsAreRateLimited(t *testing.T) {
	root := makeFixture(t)
	s := newTestServer(t, testConfig(t, root, "-A", "-a", "boss:pw@/:rw"))

	var last int
	for i := 0; i < 15; i++ {
		last = do(t, s, http.MethodGet, "/fs/settings", nil, map[string]string{
			"Authorization": basicAuth("boss", fmt.Sprintf("wrong-%d", i)),
		}).code
	}
	if last != http.StatusTooManyRequests {
		t.Fatalf("/fs/settings 连续失败 15 次后状态码 = %d，期望 429", last)
	}
}

// TestForwardedForIgnoredWithoutTrustedProxy 确认默认不采信客户端伪造的
// X-Forwarded-For：轮换它不能绕过认证限速。
func TestForwardedForIgnoredWithoutTrustedProxy(t *testing.T) {
	root := makeFixture(t)
	s := newTestServer(t, testConfig(t, root, "-A", "-a", "boss:pw@/:rw"))

	var last int
	for i := 0; i < 15; i++ {
		last = do(t, s, http.MethodGet, "/", nil, map[string]string{
			"Accept":          "application/json",
			"Authorization":   basicAuth("boss", fmt.Sprintf("wrong-%d", i)),
			"X-Forwarded-For": fmt.Sprintf("10.9.%d.%d", i/256, i%256),
		}).code
	}
	if last != http.StatusTooManyRequests {
		t.Fatalf("伪造 XFF 后状态码 = %d，期望仍然 429", last)
	}
}

// TestForwardedForHonoredFromTrustedProxy 确认显式配置可信代理后，
// 代理提供的客户端地址会被用于限速。
func TestForwardedForHonoredFromTrustedProxy(t *testing.T) {
	root := makeFixture(t)
	cfg := testConfig(t, root, "-A", "-a", "boss:pw@/:rw")
	// 测试请求的 RemoteAddr 固定是 192.0.2.1:1234。
	cfg.TrustedProxies = []string{"192.0.2.0/24"}
	s := newTestServer(t, cfg)

	var last int
	for i := 0; i < 15; i++ {
		last = do(t, s, http.MethodGet, "/", nil, map[string]string{
			"Accept":          "application/json",
			"Authorization":   basicAuth("boss", fmt.Sprintf("wrong-%d", i)),
			"X-Forwarded-For": fmt.Sprintf("10.8.0.%d", i+1),
		}).code
	}
	if last == http.StatusTooManyRequests {
		t.Fatalf("可信代理下每个客户端只失败一次，不该被节流，实际 = %d", last)
	}
}

// TestLoginGETWithQueryCredsDoesNotSetCookie 确认查询串登录不会建立会话，
// 从而无法用 <img src="...?user=...&pass=..."> 做登录 CSRF。
func TestLoginGETWithQueryCredsDoesNotSetCookie(t *testing.T) {
	root := makeFixture(t)
	s := newTestServer(t, testConfig(t, root, "-A", "-a", "boss:pw@/:rw"))

	r := httptest.NewRequest(http.MethodGet, "/fs/auth?user=boss&pass=pw", nil)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("GET /fs/auth 状态码 = %d，期望 200：%s", w.Code, w.Body.String())
	}
	if cookies := w.Result().Cookies(); len(cookies) != 0 {
		t.Fatalf("GET + 查询串凭据不应下发 cookie，实际 %d 个", len(cookies))
	}
}

// TestKeysCannotBeManagedAcrossAccounts 确认非管理员既看不到、也撤销不了
// 别人创建的密钥；否则任何有写权限的账号都能吊销管理员的分享链接。
func TestKeysCannotBeManagedAcrossAccounts(t *testing.T) {
	root := makeFixture(t)
	s := newTestServer(t, testConfig(t, root, "-A",
		"-a", "boss:pw@/:rw", "-a", "bob:bpw@/docs:rw"))

	k, _, err := s.keys.Create(uploadkey.CreateOptions{Name: "boss-key", Scope: "/", Creator: "boss"})
	if err != nil {
		t.Fatal(err)
	}
	bob := map[string]string{"Authorization": basicAuth("bob", "bpw")}
	if body := do(t, s, http.MethodGet, "/fs/keys", nil, bob).body; strings.Contains(body, k.ID) {
		t.Fatalf("bob 看到了 boss 的密钥：%s", body)
	}
	if got := do(t, s, http.MethodDelete, "/fs/keys?id="+k.ID, nil, bob).code; got != http.StatusForbidden {
		t.Fatalf("bob 撤销 boss 的密钥返回 %d，期望 403", got)
	}
	boss := map[string]string{"Authorization": basicAuth("boss", "pw")}
	if got := do(t, s, http.MethodDelete, "/fs/keys?id="+k.ID, nil, boss).code; got != http.StatusNoContent {
		t.Fatalf("boss 撤销自己的密钥返回 %d，期望 204", got)
	}
}

// TestExtractTargetsRespectFileLevelACL 确认解压前的逐条目复核能识别
// 「目录可写、单个文件只读」的规则。
func TestExtractTargetsRespectFileLevelACL(t *testing.T) {
	root := makeFixture(t)
	s := newTestServer(t, testConfig(t, root, "-A",
		"-a", "bob:bpw@/docs:rw,/docs/secret.txt:r"))
	r := httptest.NewRequest(http.MethodGet, "/docs", nil)
	r.SetBasicAuth("bob", "bpw")

	if _, ok := s.extractTargetsAllowed(r, "/docs", []archive.Entry{{Name: "ok.txt"}}); !ok {
		t.Fatal("可写文件的条目被误拒")
	}
	target, ok := s.extractTargetsAllowed(r, "/docs", []archive.Entry{{Name: "secret.txt"}})
	if ok {
		t.Fatal("只读文件 secret.txt 的条目未被拒绝")
	}
	if target != "/docs/secret.txt" {
		t.Fatalf("拒绝路径 = %q，期望 /docs/secret.txt", target)
	}
}
