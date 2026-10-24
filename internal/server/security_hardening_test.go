package server

import (
	"bytes"
	"crypto/tls"
	"errors"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestInvalidCookiesDoNotLockOutSessions 钉住凭据类别隔离：
// 10 个坏 Cookie 不能再把同一 IP 上的有效会话打成 429。
func TestInvalidCookiesDoNotLockOutSessions(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "a.txt"), []byte("hi"))
	s := newTestServer(t, testConfig(t, root, "-a", "admin:secret@/:rw"))
	sess := login(t, s, "admin", "secret")

	junk := &http.Cookie{Name: sessionCookieName, Value: "junk"}
	for i := 0; i < 10; i++ {
		withCookie(t, s, http.MethodGet, "/a.txt", junk,
			map[string]string{"X-Gofs-Ajax": "1"})
	}
	got := withCookie(t, s, http.MethodGet, "/a.txt", sess,
		map[string]string{"X-Gofs-Ajax": "1"})
	if got.code != http.StatusOK {
		t.Fatalf("坏 Cookie 锁死了有效会话：%d %s", got.code, got.body)
	}
}

// TestShareCookieDoesNotDowngradeAdmin 钉住“分享与显式身份取较高权限”：
// 管理员打开只读分享后，自己的写操作与管理接口仍应可用。
func TestShareCookieDoesNotDowngradeAdmin(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "a.txt"), []byte("hi"))
	s := newTestServer(t, testConfig(t, root, "-A", "-a", "boss:pw@/:rw"))
	sess := login(t, s, "boss", "pw")
	tok := newShare(t, s, "/")

	// 直接模拟浏览器里已经存在的分享 Cookie（攻击者可以诱使浏览器存下它）。
	shareCookie := &http.Cookie{Name: shareCookieName, Value: tok}

	put := httptest.NewRequest(http.MethodPut, "/new.txt", strings.NewReader("x"))
	put.Header.Set("X-Gofs-Ajax", "1")
	put.AddCookie(sess)
	put.AddCookie(shareCookie)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, put)
	res := w.Result()
	_ = res.Body.Close()
	if res.StatusCode != http.StatusCreated {
		t.Fatalf("管理员被只读分享降级：PUT = %d", res.StatusCode)
	}

	settings := httptest.NewRequest(http.MethodPost, "/fs/settings",
		strings.NewReader(`{"upload_max_size":1048576}`))
	settings.Header.Set("Content-Type", "application/json")
	settings.Header.Set("X-Gofs-Ajax", "1")
	settings.AddCookie(sess)
	settings.AddCookie(shareCookie)
	w = httptest.NewRecorder()
	s.Handler().ServeHTTP(w, settings)
	res = w.Result()
	_ = res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("管理员带分享 Cookie 时改设置 = %d，期望 200", res.StatusCode)
	}

	// 纯分享访客仍然不能写。
	pure := do(t, s, http.MethodPut, "/blocked.txt?share="+tok, strings.NewReader("x"), nil)
	if pure.code == http.StatusCreated || pure.code == http.StatusOK {
		t.Fatalf("只读分享访客写成功了：%d", pure.code)
	}
}

// TestCrossSiteQueryCredentialsDoNotLockAccount 确认跨站 <img> 形式
// 的查询串凭据不再计入账号失败节流。
func TestCrossSiteQueryCredentialsDoNotLockAccount(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "a.txt"), []byte("hi"))
	s := newTestServer(t, testConfig(t, root,
		"-a", "admin:secret@/:rw",
		"--auth-fail-limit", "3", "--auth-fail-window", "5m"))

	for i := 0; i < 8; i++ {
		do(t, s, http.MethodGet, "/?user=admin&pass=wrong", nil,
			map[string]string{"Sec-Fetch-Site": "cross-site"})
	}
	got := do(t, s, http.MethodGet, "/?json", nil,
		map[string]string{"Authorization": basicAuth("admin", "secret")})
	if got.code != http.StatusOK {
		t.Fatalf("跨站查询串凭据把账号锁死了：%d %s", got.code, got.body)
	}
}

// TestValidUserWithoutPathPermissionIsNotAuthFailure 确认“密码正确但对
// 该路径无权限”不会把账号拖进节流窗口。
func TestValidUserWithoutPathPermissionIsNotAuthFailure(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "docs", "a.txt"), []byte("hi"))
	s := newTestServer(t, testConfig(t, root,
		"-a", "boss:pw@/:rw", "-a", "bob:bpw@/docs:rw",
		"--auth-fail-limit", "3", "--auth-fail-window", "5m"))

	for i := 0; i < 6; i++ {
		do(t, s, http.MethodGet, "/", nil, map[string]string{
			"Accept":        "text/html",
			"Authorization": basicAuth("bob", "bpw"),
			"X-Gofs-Ajax":   "1",
		})
	}
	login := do(t, s, http.MethodGet, "/fs/auth", nil,
		map[string]string{"Authorization": basicAuth("bob", "bpw"), "X-Gofs-Ajax": "1"})
	if login.code != http.StatusOK {
		t.Fatalf("合法用户因路径无权限被锁：%d %s", login.code, login.body)
	}
}

// TestSingleFileModeRejectsRootSwitchAndSite 确认单文件模式的边界不能从
// /fs/settings 绕过。
func TestSingleFileModeRejectsRootSwitchAndSite(t *testing.T) {
	dir := t.TempDir()
	public := filepath.Join(dir, "public.txt")
	if err := os.WriteFile(public, []byte("public"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "secret.txt"), []byte("secret"), 0o644); err != nil {
		t.Fatal(err)
	}
	s := newTestServer(t, testConfig(t, public, "-A", "-a", "boss:pw@/:rw"))

	body := `{"root":` + strconvQuote(dir) + `}`
	got := do(t, s, http.MethodPut, "/fs/settings", strings.NewReader(body),
		map[string]string{"Authorization": basicAuth("boss", "pw"), "Content-Type": "application/json"})
	if got.code == http.StatusOK {
		t.Fatalf("单文件模式换根成功了：%s", got.body)
	}
	add := `{"add_site":{"path":` + strconvQuote(dir) + `}}`
	got = do(t, s, http.MethodPut, "/fs/settings", strings.NewReader(add),
		map[string]string{"Authorization": basicAuth("boss", "pw"), "Content-Type": "application/json"})
	if got.code == http.StatusOK {
		t.Fatalf("单文件模式加站点成功了：%s", got.body)
	}
}

// TestAssetsDirInsideServedRootRejected 确认自定义 assets 落在服务根内时
// 默认拒绝启动。
func TestAssetsDirInsideServedRootRejected(t *testing.T) {
	root := t.TempDir()
	assets := filepath.Join(root, "assets")
	if err := os.MkdirAll(assets, 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := testConfig(t, root, "-A")
	cfg.AssetsDir = assets
	if _, err := New(cfg, os.DirFS(filepath.Join("..", "..", "assets"))); err == nil {
		t.Fatal("服务根内的 assets 目录未被拒绝")
	}
}

// TestRedactURIHidesPathShareAndEncodedQuery 确认日志脱敏覆盖路径形式
// 分享令牌与百分号编码的敏感参数名。
func TestRedactURIHidesPathShareAndEncodedQuery(t *testing.T) {
	got := redactURI("/s/gofs_secret/docs/a.txt?pa%73s=hunter2&x=1")
	if strings.Contains(got, "gofs_secret") || strings.Contains(got, "hunter2") {
		t.Fatalf("日志仍含凭据：%s", got)
	}
	if !strings.Contains(got, "x=1") {
		t.Fatalf("非敏感参数被误删：%s", got)
	}
}

// TestFormUploadDoesNotCreateDirBeforeRealPermCheck 钉住表单上传的顺序：
// 一个不含文件的请求不能借 path= 与软链在只读目录下建目录。
func TestFormUploadDoesNotCreateDirBeforeRealPermCheck(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "docs", "a.txt"), []byte("a"))
	if err := os.MkdirAll(filepath.Join(root, "private"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(root, "private"), filepath.Join(root, "docs", "link")); err != nil {
		t.Skipf("本环境不支持软链：%v", err)
	}
	s := newTestServer(t, testConfig(t, root, "-A",
		"-a", "alice:pw@/docs:rw,/private:r"))

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	_ = mw.WriteField("path", "link/evil")
	_ = mw.Close()

	r := httptest.NewRequest(http.MethodPost, "/docs", &buf)
	r.Header.Set("Content-Type", mw.FormDataContentType())
	r.Header.Set("Authorization", basicAuth("alice", "pw"))
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	res := w.Result()
	_ = res.Body.Close()
	if res.StatusCode != http.StatusBadRequest && res.StatusCode != http.StatusForbidden {
		t.Fatalf("无文件上传返回 %d，期望 400/403", res.StatusCode)
	}
	if _, err := os.Stat(filepath.Join(root, "private", "evil")); err == nil {
		t.Fatal("在只读目录下创建了 evil 目录")
	}
}

// TestAuthLockKeyIsFixedLength 确认超长用户名不会直接变成超长 map 键，
// 且同一用户名在不同来源 IP 下得到不同的键。
func TestAuthLockKeyIsFixedLength(t *testing.T) {
	key := authLockKey(testClientIP, strings.Repeat("a", 60_000))
	if len(key) != 64 {
		t.Fatalf("锁定键长度 = %d，期望固定 64（SHA-256 hex）", len(key))
	}
	if same := authLockKey(testClientIP, "alice"); same == authLockKey("198.51.100.7", "alice") {
		t.Fatal("不同来源 IP 的同一用户名不应共用锁定键")
	}
	if same := authLockKey("1.2.3.4a", "bc"); same == authLockKey("1.2.3.4", "abc") {
		t.Fatal("IP 与用户名拼接出现歧义碰撞")
	}
}

func strconvQuote(s string) string {
	return `"` + strings.ReplaceAll(strings.ReplaceAll(s, `\`, `\\`), `"`, `\"`) + `"`
}

// TestHostPrefixedSessionCookieIgnoresLegacy 钉住 HTTPS 根路径下的
// __Host- Cookie：旧名字不再被采信，子域无法用 Domain Cookie 覆盖会话。
func TestHostPrefixedSessionCookieIgnoresLegacy(t *testing.T) {
	s := newTestServer(t, testConfig(t, t.TempDir(), "-A", "-a", "boss:pw@/:rw"))

	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.TLS = &tls.ConnectionState{}
	w := httptest.NewRecorder()
	s.setSessionCookie(w, r, "tok", false)
	raw := strings.Join(w.Result().Header.Values("Set-Cookie"), "\n")
	for _, want := range []string{sessionCookieHostName + "=tok", "Secure", "Path=/"} {
		if !strings.Contains(raw, want) {
			t.Fatalf("Set-Cookie 缺少 %q：%s", want, raw)
		}
	}

	r2 := httptest.NewRequest(http.MethodGet, "/", nil)
	r2.TLS = &tls.ConnectionState{}
	r2.AddCookie(&http.Cookie{Name: sessionCookieName, Value: "attacker"})
	if got := s.sessionToken(r2); got != "" {
		t.Fatalf("HTTPS 根路径仍采信了旧 Cookie：%q", got)
	}
	r2.AddCookie(&http.Cookie{Name: sessionCookieHostName, Value: "tok"})
	if got := s.sessionToken(r2); got != "tok" {
		t.Fatalf("__Host- Cookie 没被采信：%q", got)
	}
}

// TestAuthorizedOpenDetectsReplacement 钉住 TOCTOU 复核：
// 授权后被换掉的文件不能继续按旧权限打开。
func TestAuthorizedOpenDetectsReplacement(t *testing.T) {
	dir := t.TempDir()
	a := filepath.Join(dir, "a.txt")
	b := filepath.Join(dir, "b.txt")
	if err := os.WriteFile(a, []byte("a"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(b, []byte("b"), 0o644); err != nil {
		t.Fatal(err)
	}
	snap := snapshotAuthorized(a)
	if _, err := snap.openRead(); err != nil {
		t.Fatalf("授权后立即打开失败: %v", err)
	}
	if err := os.Rename(b, a); err != nil {
		t.Fatal(err)
	}
	if _, err := snap.openRead(); !errors.Is(err, errPathChanged) {
		t.Fatalf("替换后的文件未被识别：%v", err)
	}
}

// TestAuthorizedOpenRejectsSymlinkAtCreation 确认新建路径在授权后变成
// 软链时不会被跟随写入。
func TestAuthorizedOpenRejectsSymlinkAtCreation(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target.txt")
	link := filepath.Join(dir, "new.txt")
	if err := os.WriteFile(target, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	snap := snapshotAuthorized(link)
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("本环境不支持软链：%v", err)
	}
	if _, err := snap.openWrite(os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644); !errors.Is(err, errPathChanged) {
		t.Fatalf("软链未被拒绝：%v", err)
	}
}

// TestHostShareCookieRejectsLegacyRootScope 确认 HTTPS 根路径下旧名字里
// 的根范围分享令牌不再被采信，避免子域 Cookie tossing。
func TestHostShareCookieRejectsLegacyRootScope(t *testing.T) {
	s := newTestServer(t, testConfig(t, t.TempDir(), "-A", "-a", "boss:pw@/:rw"))
	tok := newShare(t, s, "/")
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.TLS = &tls.ConnectionState{}
	r.AddCookie(&http.Cookie{Name: shareCookieName, Value: tok})
	if got := s.shareTokenOf(r); got != "" {
		t.Fatalf("HTTPS 根路径仍采信旧名根分享：%q", got)
	}
	r.AddCookie(&http.Cookie{Name: shareCookieHostName, Value: tok})
	if got := s.shareTokenOf(r); got != tok {
		t.Fatalf("__Host- 分享 Cookie 没被采信：%q", got)
	}
}
