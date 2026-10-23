package server

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/xxl6097/gofs/internal/uploadkey"
)

func createKeyWithBasic(t *testing.T, s *Server, user, pass, body string) (string, resp) {
	t.Helper()
	got := do(t, s, http.MethodPost, "/fs/keys", strings.NewReader(body),
		map[string]string{
			"Content-Type":  "application/json",
			"Authorization": basicAuth(user, pass),
		})
	if got.code != http.StatusCreated {
		return "", got
	}
	var d struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal([]byte(got.body), &d); err != nil {
		t.Fatalf("解析创建响应失败: %v\n%s", err, got.body)
	}
	return d.Token, got
}

// TestUploadKeyFollowsCreatorACL 确认密钥/分享在每次使用时复核创建者 ACL。
//
// alice 对 /docs 可写、对 /docs/finance 只读。scope=/docs 的 key/share
// 不能因此获得 /docs/finance 的写权限。
func TestUploadKeyFollowsCreatorACL(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "docs", "finance", "report.xlsx"), []byte("ORIGINAL"))
	writeFile(t, filepath.Join(root, "docs", "ok.txt"), []byte("ok"))
	s := newTestServer(t, testConfig(t, root, "-A", "--auth-fail-limit", "0",
		"-a", "alice:pw@/docs:rw,/docs/finance:r"))

	keyToken, got := createKeyWithBasic(t, s, "alice", "pw", `{"name":"k","scope":"/docs"}`)
	if keyToken == "" {
		t.Fatalf("创建密钥失败（%d）：%s", got.code, got.body)
	}
	keyHdr := map[string]string{"X-Gofs-Upload-Key": keyToken}

	if r := do(t, s, http.MethodPut, "/docs/finance/report.xlsx",
		strings.NewReader("HACKED"), keyHdr); r.code != http.StatusForbidden {
		t.Fatalf("key 写只读子目录返回 %d，期望 403：%s", r.code, r.body)
	}
	if r := do(t, s, http.MethodPut, "/docs/ok.txt",
		strings.NewReader("OK2"), keyHdr); r.code != http.StatusCreated {
		t.Fatalf("key 写可写目录返回 %d，期望 201：%s", r.code, r.body)
	}
	data, _ := os.ReadFile(filepath.Join(root, "docs", "finance", "report.xlsx"))
	if string(data) != "ORIGINAL" {
		t.Fatalf("只读子目录被 key 覆盖：%q", data)
	}

	shareToken, got := createKeyWithBasic(t, s, "alice", "pw",
		`{"name":"s","scope":"/docs","kind":"readwrite"}`)
	if shareToken == "" {
		t.Fatalf("创建可读写分享失败（%d）：%s", got.code, got.body)
	}
	if r := do(t, s, http.MethodPut, "/docs/finance/report.xlsx?share="+shareToken,
		strings.NewReader("HACKED"), nil); r.code < 400 {
		t.Fatalf("分享写只读子目录返回 %d，期望失败：%s", r.code, r.body)
	}
	if r := do(t, s, http.MethodPut, "/docs/ok2.txt?share="+shareToken,
		strings.NewReader("OK3"), nil); r.code != http.StatusCreated {
		t.Fatalf("分享写可写目录返回 %d，期望 201：%s", r.code, r.body)
	}
	data, _ = os.ReadFile(filepath.Join(root, "docs", "finance", "report.xlsx"))
	if string(data) != "ORIGINAL" {
		t.Fatalf("只读子目录被分享覆盖：%q", data)
	}
}

// TestRulesChangeRevokesOnlyUncoveredKeys 确认规则变更只撤销失效的凭据。
func TestRulesChangeRevokesOnlyUncoveredKeys(t *testing.T) {
	s, _, keys := newUserKeyFixture(t)
	docsKey, rootKey := keys[0], keys[1]
	readShare, _, err := s.keys.Create(uploadkey.CreateOptions{
		Name: "read", Scope: "/docs", Kind: uploadkey.KindRead, Creator: "bob",
	})
	if err != nil {
		t.Fatal(err)
	}
	rwShare, _, err := s.keys.Create(uploadkey.CreateOptions{
		Name: "rw", Scope: "/docs", Kind: uploadkey.KindReadWrite, Creator: "bob",
	})
	if err != nil {
		t.Fatal(err)
	}

	// 保留 /docs:rw，去掉对 / 的权限：/docs 的 key 应保留，/ 的 key 应撤销。
	body := `{"name":"bob","rules":[{"path":"/docs","perm":"rw"},{"path":"/tmp","perm":"r"}]}`
	headers := map[string]string{
		"Authorization": basicAuth("admin", "adminpw"),
		"Content-Type":  "application/json",
	}
	if got := do(t, s, http.MethodPut, "/fs/users", strings.NewReader(body), headers); got.code != http.StatusOK {
		t.Fatalf("修改规则返回 %d：%s", got.code, got.body)
	}
	if _, ok := s.keys.Get(docsKey.ID); !ok {
		t.Error("仍在权限范围内的 /docs key 被误删")
	}
	if _, ok := s.keys.Get(rootKey.ID); ok {
		t.Error("不再覆盖权限的 / key 未被撤销")
	}

	// 再把 /docs 降为只读：剩下的 /docs key 也应撤销。
	body = `{"name":"bob","rules":[{"path":"/docs","perm":"r"}]}`
	if got := do(t, s, http.MethodPut, "/fs/users", strings.NewReader(body), headers); got.code != http.StatusOK {
		t.Fatalf("再次修改规则返回 %d：%s", got.code, got.body)
	}
	if _, ok := s.keys.Get(docsKey.ID); ok {
		t.Error("降为只读后 /docs key 仍然有效")
	}
	if _, ok := s.keys.Get(rwShare.ID); ok {
		t.Error("降为只读后可读写分享仍然有效")
	}
	if _, ok := s.keys.Get(readShare.ID); !ok {
		t.Error("只读分享在仍有读权限时被误删")
	}
}

// TestOrphanKeyDeniedWhenAuthEnabled 确认无创建者的历史密钥在启用鉴权后被拒绝。
func TestOrphanKeyDeniedWhenAuthEnabled(t *testing.T) {
	root := t.TempDir()
	s := newTestServer(t, testConfig(t, root, "-A", "--auth-fail-limit", "0",
		"-a", "boss:pw@/:rw"))
	_, token, err := s.keys.Create(uploadkey.CreateOptions{Name: "orphan", Scope: "/docs"})
	if err != nil {
		t.Fatal(err)
	}
	if r := do(t, s, http.MethodPut, "/docs/x.txt",
		strings.NewReader("x"), map[string]string{"X-Gofs-Upload-Key": token}); r.code < 400 {
		t.Fatalf("无创建者密钥在鉴权开启时仍可使用：%d %s", r.code, r.body)
	}

	// 未启用鉴权时没有 ACL 可复核，保持原有行为。
	s2 := newTestServer(t, testConfig(t, root, "-A"))
	_, token2, err := s2.keys.Create(uploadkey.CreateOptions{Name: "anon", Scope: "/docs"})
	if err != nil {
		t.Fatal(err)
	}
	if r := do(t, s2, http.MethodPut, "/docs/y.txt",
		strings.NewReader("y"), map[string]string{"X-Gofs-Upload-Key": token2}); r.code != http.StatusCreated {
		t.Fatalf("未启用鉴权时密钥返回 %d，期望 201：%s", r.code, r.body)
	}
}
