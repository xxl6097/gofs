package server

import (
	"encoding/json"
	"net/http"
	"testing"
)

func TestSettingsDoesNotLeakListenAddrToNonAdmin(t *testing.T) {
	root := makeFixture(t)
	s := newTestServer(t, testConfig(t, root, "-A", "--auth-fail-limit", "0",
		"-a", "admin:adminpw@/:rw", "-a", "bob:bobpw@/:r"))

	bob := map[string]string{"Authorization": basicAuth("bob", "bobpw")}
	got := do(t, s, http.MethodGet, "/fs/settings", nil, bob)
	if got.code != http.StatusOK {
		t.Fatalf("非管理员读取设置返回 %d：%s", got.code, got.body)
	}
	var rep struct {
		Root       string `json:"root"`
		ListenAddr string `json:"listen_addr"`
		ListenPort int    `json:"listen_port"`
	}
	if err := json.Unmarshal([]byte(got.body), &rep); err != nil {
		t.Fatal(err)
	}
	if rep.ListenAddr != "" || rep.ListenPort != 0 || rep.Root != "" {
		t.Fatalf("非管理员拿到了主机布局信息：%+v", rep)
	}

	admin := map[string]string{"Authorization": basicAuth("admin", "adminpw")}
	got = do(t, s, http.MethodGet, "/fs/settings", nil, admin)
	if got.code != http.StatusOK {
		t.Fatalf("管理员读取设置返回 %d：%s", got.code, got.body)
	}
	if err := json.Unmarshal([]byte(got.body), &rep); err != nil {
		t.Fatal(err)
	}
	if rep.ListenAddr == "" || rep.Root == "" {
		t.Fatalf("管理员应当拿到监听地址与根目录：%+v", rep)
	}
}

func TestKeyListHidesPathFromNonAdmin(t *testing.T) {
	root := makeFixture(t)
	s := newTestServer(t, testConfig(t, root, "-A", "--auth-fail-limit", "0",
		"-a", "admin:adminpw@/:rw", "-a", "bob:bobpw@/docs:rw"))

	bob := map[string]string{"Authorization": basicAuth("bob", "bobpw")}
	got := do(t, s, http.MethodGet, "/fs/keys", nil, bob)
	if got.code != http.StatusOK {
		t.Fatalf("非管理员读取密钥列表返回 %d：%s", got.code, got.body)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal([]byte(got.body), &m); err != nil {
		t.Fatal(err)
	}
	if _, ok := m["path"]; ok {
		t.Fatal("非管理员拿到了密钥文件路径")
	}

	admin := map[string]string{"Authorization": basicAuth("admin", "adminpw")}
	got = do(t, s, http.MethodGet, "/fs/keys", nil, admin)
	if got.code != http.StatusOK {
		t.Fatalf("管理员读取密钥列表返回 %d：%s", got.code, got.body)
	}
	m = nil
	if err := json.Unmarshal([]byte(got.body), &m); err != nil {
		t.Fatal(err)
	}
	if _, ok := m["path"]; !ok {
		t.Fatal("管理员应当拿到密钥文件路径")
	}
}
