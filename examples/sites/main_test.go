package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/xxl6097/gofs"
)

// mkdir 建目录并写一个文件，返回目录路径。
func mkdir(t *testing.T, parent, name, file, content string) string {
	t.Helper()
	dir := filepath.Join(parent, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if file != "" {
		if err := os.WriteFile(filepath.Join(dir, file), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// demo 起一套**与 main 完全相同**的接线（走同一个 build），供下面的用例驱动。
func demo(t *testing.T, o buildOptions) (*gofs.Server, *httptest.Server) {
	t.Helper()
	if o.bind == "" {
		o.bind = "127.0.0.1"
	}
	// 站点会落盘：给每个测试一个临时设置文件，别写进开发者的真实配置目录
	// （默认路径由 os.UserConfigDir() 决定，测试不该碰它）。
	if o.settingsFile == "" {
		o.settingsFile = filepath.Join(t.TempDir(), "settings.json")
	}
	srv, err := build(o)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	t.Cleanup(func() { _ = srv.Close() })
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return srv, ts
}

// call 发一个带管理员凭据的请求。
func call(t *testing.T, ts *httptest.Server, method, path string, body io.Reader) (int, string) {
	t.Helper()
	req, err := http.NewRequest(method, ts.URL+path, body)
	if err != nil {
		t.Fatal(err)
	}
	req.SetBasicAuth("admin", "admin123")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	res, err := ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return res.StatusCode, string(b)
}

// TestDemoServesEachSite 演示的核心：主目录在 /、站点在 /<标签>/。
func TestDemoServesEachSite(t *testing.T) {
	base := t.TempDir()
	root := mkdir(t, base, "data", "a.txt", "主目录")
	site := mkdir(t, base, "backup", "b.txt", "站点")

	srv, ts := demo(t, buildOptions{roots: []string{root, site}, manage: true, verbose: false})

	// 服务端认到的挂载点：第一个是主，URL 是 /。
	ms := srv.Mounts()
	if len(ms) != 2 || !ms[0].Primary || ms[0].URLPath != "/" {
		t.Fatalf("挂载点不对：%+v", ms)
	}
	if ms[1].URLPath != "/backup" {
		t.Errorf("站点 URL = %q，期望 /backup", ms[1].URLPath)
	}

	for _, tc := range []struct{ path, want string }{
		{"/a.txt", "主目录"},       // 主目录不带前缀
		{"/backup/b.txt", "站点"}, // 站点带标签
	} {
		code, body := call(t, ts, http.MethodGet, tc.path, nil)
		if code != http.StatusOK {
			t.Errorf("GET %s 返回 %d：%s", tc.path, code, body)
			continue
		}
		if body != tc.want {
			t.Errorf("GET %s = %q，期望 %q", tc.path, body, tc.want)
		}
	}
}

// TestDemoScopesPerSite 演示「按站点分权限」：uploader 只在**自己那个站点**有权限。
//
// 目录特意叫 uploads，好让 build() 推导出的规则（/<目录名>:rw）落在 /uploads。
func TestDemoScopesPerSite(t *testing.T) {
	base := t.TempDir()
	root := mkdir(t, base, "data", "a.txt", "主目录")
	up := mkdir(t, base, "uploads", "u.txt", "上传站")

	_, ts := demo(t, buildOptions{roots: []string{root, up}, manage: true, verbose: false})

	ask := func(user, pass, path string) int {
		req, err := http.NewRequest(http.MethodGet, ts.URL+path, nil)
		if err != nil {
			t.Fatal(err)
		}
		req.SetBasicAuth(user, pass)
		res, err := ts.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		return res.StatusCode
	}

	// uploader 只能碰自己的站点。
	if got := ask("uploader", "upload123", "/uploads/u.txt"); got != http.StatusOK {
		t.Errorf("uploader 读自己的站点返回 %d，期望 200", got)
	}
	if got := ask("uploader", "upload123", "/a.txt"); got != http.StatusForbidden &&
		got != http.StatusUnauthorized {
		t.Errorf("uploader 读主目录返回 %d，期望被拒", got)
	}
	// admin 两边都行。
	for _, p := range []string{"/a.txt", "/uploads/u.txt"} {
		if got := ask("admin", "admin123", p); got != http.StatusOK {
			t.Errorf("admin 读 %s 返回 %d，期望 200", p, got)
		}
	}
}

// TestDemoManageSitesOnPage 演示运行期增删站点：范围内可以加，范围外拒绝。
func TestDemoManageSitesOnPage(t *testing.T) {
	base := t.TempDir()
	root := mkdir(t, base, "data", "a.txt", "主目录")
	inside := mkdir(t, base, "extra", "e.txt", "范围内")
	outside := mkdir(t, t.TempDir(), "far", "f.txt", "范围外")

	// 给了 -allow 就按它收窄：base/extra 可挂，别处的 far 不行。
	// （不给 -allow 的话默认不限目录，见 README。）
	_, ts := demo(t, buildOptions{
		roots:   []string{root},
		allow:   []string{base},
		manage:  true,
		verbose: false,
	})

	// 一开始只有主目录。
	code, body := call(t, ts, http.MethodGet, "/fs/settings", nil)
	if code != http.StatusOK {
		t.Fatalf("读设置返回 %d：%s", code, body)
	}
	if !strings.Contains(body, `"sites_editable":true`) {
		t.Errorf("应当允许增删站点：%s", body)
	}

	// 范围内：加得上，且立刻可访问。
	code, body = call(t, ts, http.MethodPut, "/fs/settings",
		strings.NewReader(`{"add_site":{"path":"`+inside+`"}}`))
	if code != http.StatusOK {
		t.Fatalf("新增站点返回 %d：%s", code, body)
	}
	if !strings.Contains(body, `"url":"/extra"`) {
		t.Errorf("新站点没出现在设置里：%s", body)
	}
	if c, b := call(t, ts, http.MethodGet, "/extra/e.txt", nil); c != http.StatusOK || b != "范围内" {
		t.Errorf("GET /extra/e.txt = %d %q", c, b)
	}
	// 主目录地址没被影响 —— 这正是「主目录钉在 /」的意义。
	if c, b := call(t, ts, http.MethodGet, "/a.txt", nil); c != http.StatusOK || b != "主目录" {
		t.Errorf("主目录被影响了：%d %q", c, b)
	}

	// 范围外：拒绝，且说明可用范围。
	code, body = call(t, ts, http.MethodPut, "/fs/settings",
		strings.NewReader(`{"add_site":{"path":"`+outside+`"}}`))
	if code != http.StatusBadRequest {
		t.Fatalf("范围外的新增返回 %d，期望 400：%s", code, body)
	}
	if !strings.Contains(body, "超出白名单") {
		t.Errorf("错误信息没说清原因：%s", body)
	}

	// 移除：立刻消失，回到单根形状。
	code, body = call(t, ts, http.MethodPut, "/fs/settings",
		strings.NewReader(`{"remove_site":"extra"}`))
	if code != http.StatusOK {
		t.Fatalf("移除返回 %d：%s", code, body)
	}
	if c, _ := call(t, ts, http.MethodGet, "/extra/e.txt", nil); c != http.StatusNotFound {
		t.Errorf("移除后仍可访问：%d", c)
	}
	var doc struct {
		MultiRoot      bool `json:"multi_root"`
		RootSwitchable bool `json:"root_switchable"`
	}
	_, body = call(t, ts, http.MethodGet, "/fs/settings", nil)
	if err := json.Unmarshal([]byte(body), &doc); err != nil {
		t.Fatal(err)
	}
	if doc.MultiRoot || !doc.RootSwitchable {
		t.Errorf("移除最后一个站点后应回到单根且换根可用：%s", body)
	}
}

// TestDemoNoManage 演示 -no-manage：开关关掉后页面上不能增删，接口明确拒绝。
func TestDemoNoManage(t *testing.T) {
	base := t.TempDir()
	root := mkdir(t, base, "data", "a.txt", "主目录")
	extra := mkdir(t, base, "extra", "e.txt", "x")

	_, ts := demo(t, buildOptions{roots: []string{root}, allow: []string{base}, manage: false, verbose: false})

	_, body := call(t, ts, http.MethodGet, "/fs/settings", nil)
	if !strings.Contains(body, `"sites_editable":false`) {
		t.Errorf("-no-manage 下不该允许编辑站点：%s", body)
	}
	code, body := call(t, ts, http.MethodPut, "/fs/settings",
		strings.NewReader(`{"add_site":{"path":"`+extra+`"}}`))
	if code != http.StatusForbidden {
		t.Errorf("新增返回 %d，期望 403：%s", code, body)
	}
}

// TestDemoStaticSitesUnaffectedByNoManage 确认 -no-manage 只关「页面增删」，
// 不影响命令行静态挂的那些站点。
func TestDemoStaticSitesUnaffectedByNoManage(t *testing.T) {
	base := t.TempDir()
	root := mkdir(t, base, "data", "a.txt", "主目录")
	site := mkdir(t, base, "backup", "b.txt", "站点")

	_, ts := demo(t, buildOptions{roots: []string{root, site}, manage: false, verbose: false})

	if c, b := call(t, ts, http.MethodGet, "/backup/b.txt", nil); c != http.StatusOK || b != "站点" {
		t.Errorf("静态站点不可用：%d %q", c, b)
	}
}
