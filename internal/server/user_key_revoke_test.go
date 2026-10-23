package server

import (
	"net/http"
	"strings"
	"testing"

	"github.com/xxl6097/gofs/internal/auth"
	"github.com/xxl6097/gofs/internal/uploadkey"
)

// newUserKeyFixture 准备一个「管理员 + bob 用户 + bob 的两把钥匙」的场景。
func newUserKeyFixture(t *testing.T) (*Server, map[string]string, []*uploadkey.Key) {
	t.Helper()
	root := makeFixture(t)
	s := newTestServer(t, testConfig(t, root, "-A",
		"-a", "admin:adminpw@/:rw", "--auth-fail-limit", "0"))
	if err := s.auth.AddUser("bob", "bobpw1", []auth.Rule{
		{Path: "/docs", Perm: auth.PermReadWrite},
	}); err != nil {
		t.Fatalf("AddUser: %v", err)
	}

	var keys []*uploadkey.Key
	for _, scope := range []string{"/docs", "/"} {
		k, _, err := s.keys.Create(uploadkey.CreateOptions{
			Name: "bob-key", Scope: scope, Creator: "bob",
		})
		if err != nil {
			t.Fatalf("Create key: %v", err)
		}
		keys = append(keys, k)
	}
	return s, map[string]string{"Authorization": basicAuth("admin", "adminpw")}, keys
}

// TestDeleteUserRevokesKeys 确认删号会撤销该账号签发的密钥/分享。
//
// 不撤销的话，会话断掉只是「退出登录」，攻击者仍能用此前签发的 bearer
// 凭据继续访问 —— 删除被入侵账号并不等于收回访问权。
func TestDeleteUserRevokesKeys(t *testing.T) {
	s, admin, keys := newUserKeyFixture(t)

	got := do(t, s, http.MethodDelete, "/fs/users?name=bob", nil, admin)
	if got.code != http.StatusOK {
		t.Fatalf("删除用户返回 %d：%s", got.code, got.body)
	}
	for _, k := range keys {
		if _, ok := s.keys.Get(k.ID); ok {
			t.Errorf("账号删除后密钥 %s 仍然有效", k.ID)
		}
	}
}

// TestPasswordResetRevokesKeys 确认重置密码会撤销该账号此前签发的凭据。
func TestPasswordResetRevokesKeys(t *testing.T) {
	s, _, keys := newUserKeyFixture(t)

	body := `{"name":"bob","password":"newpw123","rules":[{"path":"/docs","perm":"rw"}]}`
	headers := map[string]string{
		"Authorization": basicAuth("admin", "adminpw"),
		"Content-Type":  "application/json",
	}
	got := do(t, s, http.MethodPut, "/fs/users", strings.NewReader(body), headers)
	if got.code != http.StatusOK {
		t.Fatalf("重置密码返回 %d：%s", got.code, got.body)
	}
	for _, k := range keys {
		if _, ok := s.keys.Get(k.ID); ok {
			t.Errorf("改密后密钥 %s 仍然有效", k.ID)
		}
	}
}
