package server

import (
	"encoding/json"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
)

// TestManagementClosedWithoutAuth 钉住安全默认值：未启用鉴权时，HTTP 管理接口
// 不能把任何访问者当成管理员。
//
// 这些接口能挂载服务根之外的目录、切换服务根、改监听地址、签发长期凭证，
// 已经超出「被服务的那棵目录」。无鉴权部署意味着任何能连上端口的人都能访问，
// 默认放开等于把整台机器交出去。
func TestManagementClosedWithoutAuth(t *testing.T) {
	root := t.TempDir()
	other := t.TempDir()
	writeFile(t, filepath.Join(other, "leak.txt"), []byte("LEAKED"))

	// -A 打开全部操作权限，确保拒绝来自「无鉴权管理面关闭」，而不是功能开关。
	s := newTestServer(t, testConfigDefaultPolicy(t, root, "-A"))

	// GET 仍可用，但不下发服务根 / 站点列表等敏感信息。
	got := do(t, s, http.MethodGet, "/fs/settings", nil, nil)
	if got.code != http.StatusOK {
		t.Fatalf("GET /fs/settings 返回 %d：%s", got.code, got.body)
	}
	var view map[string]any
	if err := json.Unmarshal([]byte(got.body), &view); err != nil {
		t.Fatalf("解析设置失败：%v\n%s", err, got.body)
	}
	for _, field := range []string{"root", "roots", "allow_roots"} {
		if _, ok := view[field]; ok {
			t.Errorf("未鉴权设置响应不应包含 %q：%s", field, got.body)
		}
	}

	// 新增站点：必须拒绝，且不能真的挂载。
	body := `{"add_site":{"path":"` + other + `"}}`
	got = do(t, s, http.MethodPost, "/fs/settings", strings.NewReader(body), nil)
	if got.code != http.StatusForbidden {
		t.Errorf("未鉴权新增站点返回 %d，期望 403：%s", got.code, got.body)
	}
	if len(s.Mounts()) != 1 {
		t.Errorf("未鉴权请求竟然挂载了站点：%v", s.Mounts())
	}

	// 切换服务根：必须拒绝。
	body = `{"root":"` + other + `"}`
	got = do(t, s, http.MethodPut, "/fs/settings", strings.NewReader(body), nil)
	if got.code != http.StatusForbidden {
		t.Errorf("未鉴权切换根目录返回 %d，期望 403：%s", got.code, got.body)
	}
	if resolved, _ := filepath.EvalSymlinks(root); s.Root() != resolved {
		t.Errorf("未鉴权请求竟然切换了服务根：%s", s.Root())
	}

	// 密钥管理：必须拒绝（这里 -A 已打开 AllowKeys）。
	got = do(t, s, http.MethodPost, "/fs/keys", strings.NewReader(`{}`), nil)
	if got.code != http.StatusForbidden {
		t.Errorf("未鉴权创建密钥返回 %d，期望 403：%s", got.code, got.body)
	}

	// 页面不应给出「服务设置」入口。
	page := do(t, s, http.MethodGet, "/", nil, map[string]string{"Accept": "text/html"})
	if page.code != http.StatusOK {
		t.Fatalf("首页返回 %d：%s", page.code, page.body)
	}
	if strings.Contains(page.body, `"allow_settings":true`) {
		t.Errorf("未鉴权页面不应注入 allow_settings=true")
	}
}

// TestAnonymousAdminOptIn 确认显式开关仍保留旧的「无鉴权管理」用法。
func TestAnonymousAdminOptIn(t *testing.T) {
	root := t.TempDir()
	other := t.TempDir()
	s := newTestServer(t, testConfig(t, root, "-A")) // testConfig 带 --allow-anonymous-admin

	body := `{"add_site":{"path":"` + other + `"}}`
	got := do(t, s, http.MethodPost, "/fs/settings", strings.NewReader(body), nil)
	if got.code != http.StatusOK {
		t.Fatalf("显式放开后新增站点返回 %d：%s", got.code, got.body)
	}
	if len(s.Mounts()) != 2 {
		t.Errorf("站点没有挂上：%v", s.Mounts())
	}
}
