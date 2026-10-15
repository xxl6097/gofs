package server

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/xxl6097/gofs/internal/auth"
)

// TestAuthEndpointScopedLanding 验证只被授予子目录的账号能从登录响应里
// 拿到落地目录与可达路径。
//
// 背景：这样的账号停在根目录上读不到任何东西，而那次失败是 401，
// 前端把它当「登录已失效」—— 于是刚登录就被弹回登录框，怎么登都进不去
// （见 handleAuth 里 home 的说明）。前端要靠 home_path 换乘。
func TestAuthEndpointScopedLanding(t *testing.T) {
	root := makeFixture(t)
	s := newTestServer(t, testConfig(t, root, "-A", "-a", "u:p@/docs:rw"))
	hdr := map[string]string{"Authorization": basicAuth("u", "p")}

	r := do(t, s, http.MethodGet, "/fs/auth?path=/", nil, hdr)
	if r.code != 200 {
		t.Fatalf("状态码 = %d；%s", r.code, r.body)
	}
	var d struct {
		HomePath  string   `json:"home_path"`
		ReadPaths []string `json:"read_paths"`
	}
	if err := json.Unmarshal([]byte(r.body), &d); err != nil {
		t.Fatalf("解析响应失败: %v；%s", err, r.body)
	}
	if d.HomePath != "/docs" {
		t.Errorf("home_path = %q, 期望 /docs", d.HomePath)
	}
	if len(d.ReadPaths) != 1 || d.ReadPaths[0] != "/docs" {
		t.Errorf("read_paths = %v, 期望 [/docs]", d.ReadPaths)
	}

	// 根目录对它是读不到的（前端据此判断该不该把「根目录」画成链接）
	if r0 := do(t, s, http.MethodGet, "/?json", nil, hdr); r0.code == 200 {
		t.Errorf("根目录本应被拒绝，却返回了 %d：%.120s", r0.code, r0.body)
	}
	// 它自己的目录照常可用
	if rd := do(t, s, http.MethodGet, "/docs?json", nil, hdr); rd.code != 200 {
		t.Errorf("/docs 应可读，实际 %d：%.120s", rd.code, rd.body)
	}

	// 报告里的场景用的是「页面上新建的用户」（用户表），走同一条路：
	// 账号来自启动参数还是用户表，落地目录的算法必须一致。
	if err := s.auth.AddUser("bob", "bobpass1", []auth.Rule{
		{Path: "/docs", Perm: auth.PermReadWrite},
	}); err != nil {
		t.Fatalf("AddUser 失败: %v", err)
	}
	rb := do(t, s, http.MethodGet, "/fs/auth?path=/", nil, map[string]string{
		"Authorization": basicAuth("bob", "bobpass1"),
	})
	var db struct {
		HomePath  string   `json:"home_path"`
		ReadPaths []string `json:"read_paths"`
	}
	if err := json.Unmarshal([]byte(rb.body), &db); err != nil {
		t.Fatalf("解析响应失败: %v；%s", err, rb.body)
	}
	if db.HomePath != "/docs" || len(db.ReadPaths) != 1 || db.ReadPaths[0] != "/docs" {
		t.Errorf("用户表账号的落地信息 = %q %v, 期望 /docs [/docs]", db.HomePath, db.ReadPaths)
	}

	// 对根目录有权限的账号：落地就是根目录，read_paths 含 "/"
	s2 := newTestServer(t, testConfig(t, root, "-A", "-a", "admin:pw@/:rw"))
	r2 := do(t, s2, http.MethodGet, "/fs/auth", nil, map[string]string{
		"Authorization": basicAuth("admin", "pw"),
	})
	var d2 struct {
		HomePath  string   `json:"home_path"`
		ReadPaths []string `json:"read_paths"`
	}
	if err := json.Unmarshal([]byte(r2.body), &d2); err != nil {
		t.Fatalf("解析响应失败: %v；%s", err, r2.body)
	}
	if d2.HomePath != "/" {
		t.Errorf("admin 的 home_path = %q, 期望 /", d2.HomePath)
	}
	if len(d2.ReadPaths) != 1 || d2.ReadPaths[0] != "/" {
		t.Errorf("admin 的 read_paths = %v, 期望 [/]", d2.ReadPaths)
	}
}
