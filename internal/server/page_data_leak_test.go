package server

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/xxl6097/gofs/internal/config"
)

// pageJSON 从 HTML 页面里抠出注入的 gofs-data JSON。
func pageJSON(t *testing.T, html string) map[string]any {
	t.Helper()
	const open = `<script id="gofs-data" type="application/json">`
	i := strings.Index(html, open)
	if i < 0 {
		t.Fatalf("页面里没有 gofs-data：%.200s", html)
	}
	rest := html[i+len(open):]
	j := strings.Index(rest, "</script>")
	if j < 0 {
		t.Fatal("gofs-data 没有闭合")
	}
	var d map[string]any
	if err := json.Unmarshal([]byte(rest[:j]), &d); err != nil {
		t.Fatalf("解析页面数据失败：%v\n%s", err, rest[:j])
	}
	return d
}

// jsonMap 解析一个 JSON 响应体。
func jsonMap(t *testing.T, raw string) map[string]any {
	t.Helper()
	var d map[string]any
	if err := json.Unmarshal([]byte(raw), &d); err != nil {
		t.Fatalf("解析 JSON 失败：%v\n%s", err, raw)
	}
	return d
}

// assertNoGlobalConfig 断言页面数据里不含任何全局配置字段。
func assertNoGlobalConfig(t *testing.T, data map[string]any) {
	t.Helper()
	for _, k := range []string{
		"version", "title", "auth_on", "auth_required", "user", "anonymous",
		"user_from_startup", "password_persistent",
		"extract_limits", "upload_dated", "upload_date_dir", "upload_date_layout",
		"edit_max_size", "upload_max_size",
		"allow_keys", "allow_settings", "allow_upgrade", "allow_restart",
		"allow_uninstall", "allow_users",
	} {
		if v, ok := data[k]; ok {
			t.Errorf("未认证页面泄露了全局配置 %q = %v", k, v)
		}
	}
}

// TestAuthGateMinimalFields 锁定未登录外壳的字段集合：
// 只允许渲染登录页真正需要的字段，其余一概不下发。
func TestAuthGateMinimalFields(t *testing.T) {
	root := makeFixture(t)
	s := newTestServer(t, testConfig(t, root,
		"-A", "-a", "admin:secret@/:rw"))

	got := do(t, s, http.MethodGet, "/", nil,
		map[string]string{"Accept": "text/html"})
	if got.code != http.StatusOK {
		t.Fatalf("登录外壳返回 %d", got.code)
	}
	data := pageJSON(t, got.body)

	allowed := map[string]bool{
		"title": true, "path_prefix": true, "path": true,
		"auth_on": true, "auth_required": true,
	}
	for k := range data {
		if !allowed[k] {
			t.Errorf("未登录外壳泄露字段 %q：%v", k, data[k])
		}
	}
	for _, k := range []string{"perms", "listing"} {
		if _, ok := data[k]; ok {
			t.Errorf("未登录外壳不应下发 %q", k)
		}
	}
	if data["auth_required"] != true {
		t.Errorf("登录外壳缺少 auth_required：%v", data)
	}
}

// TestAuthenticatedPageHasFullConfig 确认已认证会话仍拿到完整字段（B）。
func TestAuthenticatedPageHasFullConfig(t *testing.T) {
	root := makeFixture(t)
	s := newTestServer(t, testConfig(t, root,
		"-A", "-a", "admin:secret@/:rw"))
	cookie := login(t, s, "admin", "secret")
	hdr := map[string]string{
		"Accept": "text/html",
		"Cookie": cookie.Name + "=" + cookie.Value,
	}

	got := do(t, s, http.MethodGet, "/", nil, hdr)
	if got.code != http.StatusOK {
		t.Fatalf("已认证页面返回 %d", got.code)
	}
	data := pageJSON(t, got.body)
	for _, k := range []string{
		"version", "title", "path_prefix", "path", "name", "listing", "perms",
		"user", "anonymous", "auth_on", "auth_required",
		"user_from_startup", "password_persistent",
		"extract_limits", "upload_dated", "upload_date_layout",
		"edit_max_size", "upload_max_size",
		"allow_keys", "allow_settings", "allow_upgrade", "allow_restart",
		"allow_uninstall", "allow_users",
	} {
		if _, ok := data[k]; !ok {
			t.Errorf("已认证页面缺少字段 %q", k)
		}
	}
	if data["user"] != "admin" {
		t.Errorf("user = %v，期望 admin", data["user"])
	}
	if data["auth_required"] == true {
		t.Error("已认证页面仍标记 auth_required")
	}

	// 从登录框进来时页面外壳是最小字段，登录后 /fs/auth 必须补齐配置。
	info := do(t, s, http.MethodGet, "/fs/auth", nil, map[string]string{
		"Cookie":      cookie.Name + "=" + cookie.Value,
		"X-Gofs-Ajax": "1",
	})
	if info.code != http.StatusOK {
		t.Fatalf("/fs/auth 返回 %d", info.code)
	}
	authData := jsonMap(t, info.body)
	for _, k := range []string{
		"version", "extract_limits", "upload_date_layout",
		"edit_max_size", "upload_max_size", "perms",
	} {
		if _, ok := authData[k]; !ok {
			t.Errorf("/fs/auth 缺少字段 %q（登录后配置会残缺）", k)
		}
	}
}

// TestSharePageHasNoGlobalConfig 确认匿名打开分享链接时只拿到
// 该分享范围内的列表与权限（C）。
func TestSharePageHasNoGlobalConfig(t *testing.T) {
	for _, tc := range []struct {
		name  string
		share func(*testing.T, *Server, string) string
	}{
		{"只读分享", newShare},
		{"可读写分享", newShareRW},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, _ := shareServer(t)
			tok := tc.share(t, s, "/docs")
			got := do(t, s, http.MethodGet, "/docs/?share="+tok, nil,
				map[string]string{"Accept": "text/html"})
			if got.code != http.StatusOK {
				t.Fatalf("分享页返回 %d：%s", got.code, got.body)
			}
			data := pageJSON(t, got.body)
			assertNoGlobalConfig(t, data)
			if _, ok := data["listing"]; !ok {
				t.Error("分享页缺少 listing")
			}
			perms, _ := data["perms"].(map[string]any)
			if perms == nil || perms["read"] != true {
				t.Errorf("分享页权限不正确：%v", data["perms"])
			}
			if tc.name == "可读写分享" && perms["write"] != true {
				t.Errorf("可读写分享缺少写权限：%v", perms)
			}
		})
	}
}

// TestAnonymousRulePageHasNoGlobalConfig 确认匿名规则（未登录但可读）
// 同样只拿到列表与权限，不泄露全局配置。
func TestAnonymousRulePageHasNoGlobalConfig(t *testing.T) {
	root := makeFixture(t)
	s := newTestServer(t, testConfig(t, root, "-a", "@/:r"))

	got := do(t, s, http.MethodGet, "/", nil,
		map[string]string{"Accept": "text/html"})
	if got.code != http.StatusOK {
		t.Fatalf("匿名规则页面返回 %d：%s", got.code, got.body)
	}
	data := pageJSON(t, got.body)
	assertNoGlobalConfig(t, data)
	perms, _ := data["perms"].(map[string]any)
	if perms == nil || perms["read"] != true {
		t.Errorf("匿名规则页面权限不正确：%v", data["perms"])
	}
}

// TestExtractZeroLimitsDeny 钉住 D：三个上限任一为 0 都视为未配置，
// 必须拒绝解压，不能退化成无限制。
func TestExtractZeroLimitsDeny(t *testing.T) {
	cases := []struct {
		name string
		zero func(*config.Config)
	}{
		{"max_files=0", func(c *config.Config) { c.ExtractMaxFiles = 0 }},
		{"max_total=0", func(c *config.Config) { c.ExtractMaxTotal = 0 }},
		{"max_ratio=0", func(c *config.Config) { c.ExtractMaxRatio = 0 }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			makeZip(t, root, "a.zip", "secret")
			cfg := testConfig(t, root, "-A")
			tc.zero(cfg)
			s := newTestServer(t, cfg)

			// 页面不能给出解压入口。
			page := do(t, s, http.MethodGet, "/", nil,
				map[string]string{"Accept": "text/html"})
			perms := pagePerms(t, page.body)
			if v, _ := perms["extract"].(bool); v {
				t.Error("上限未配置时 perms.extract 仍为 true")
			}

			// 列表 / 预览也必须拒绝。
			if got := do(t, s, http.MethodGet,
				"/fs/extract?path=/a.zip", nil, nil); got.code != http.StatusServiceUnavailable {
				t.Fatalf("未配置上限时列列表返回 %d，期望 503：%s", got.code, got.body)
			}

			// 真正解压必须拒绝，且不能在磁盘上留下任何东西。
			body := `{"path":"/a.zip","dest":"/out"}`
			if got := do(t, s, http.MethodPut, "/fs/extract",
				strings.NewReader(body),
				map[string]string{"Content-Type": "application/json"}); got.code != http.StatusServiceUnavailable {
				t.Fatalf("未配置上限时解压返回 %d，期望 503：%s", got.code, got.body)
			}
			if _, err := os.Stat(filepath.Join(root, "out")); !os.IsNotExist(err) {
				t.Errorf("被拒绝的解压在磁盘上留下了输出目录：%v", err)
			}
		})
	}
}
