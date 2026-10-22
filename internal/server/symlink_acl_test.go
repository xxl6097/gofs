package server

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/xxl6097/gofs/internal/uploadkey"
)

// TestInRootSymlinkDoesNotBypassACL 确认权限按**真实目标**复核。
//
// `/docs/link.txt -> ../secret.txt`：请求路径命中 /docs:rw，但真实目标
// 在 alice 的 ACL 之外。修复前读取会返回 secret，写入会改到 secret。
func TestInRootSymlinkDoesNotBypassACL(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "secret.txt"), []byte("TOP-SECRET"))
	writeFile(t, filepath.Join(root, "docs", "ok.txt"), []byte("ok"))
	if err := os.Symlink(filepath.Join(root, "secret.txt"), filepath.Join(root, "docs", "link.txt")); err != nil {
		t.Skipf("当前环境不支持创建软链接：%v", err)
	}

	s := newTestServer(t, testConfig(t, root, "-A", "--auth-fail-limit", "0",
		"-a", "alice:pw@/docs:rw"))
	alice := map[string]string{"Authorization": basicAuth("alice", "pw")}

	if got := do(t, s, http.MethodGet, "/docs/link.txt", nil, alice); got.code == http.StatusOK ||
		strings.Contains(got.body, "TOP-SECRET") {
		t.Fatalf("软链越权读取成功（%d）：%s", got.code, got.body)
	}
	if got := do(t, s, http.MethodPut, "/docs/link.txt", strings.NewReader("HACKED"), alice); got.code == http.StatusCreated {
		t.Fatalf("软链越权写入成功：%s", got.body)
	}
	if data, _ := os.ReadFile(filepath.Join(root, "secret.txt")); string(data) == "HACKED" {
		t.Fatalf("软链目标被改写")
	}
}

// TestDeleteSymlinkOnlyRemovesLink 确认「删除链接本身」没有被误伤：
// 链接在用户有 rw 的目录里，操作对象是链接，不应按目标权限拒绝。
func TestDeleteSymlinkOnlyRemovesLink(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "secret.txt"), []byte("TOP-SECRET"))
	writeFile(t, filepath.Join(root, "docs", "ok.txt"), []byte("ok"))
	link := filepath.Join(root, "docs", "link.txt")
	if err := os.Symlink(filepath.Join(root, "secret.txt"), link); err != nil {
		t.Skipf("当前环境不支持创建软链接：%v", err)
	}

	s := newTestServer(t, testConfig(t, root, "-A", "--auth-fail-limit", "0",
		"-a", "alice:pw@/docs:rw"))
	alice := map[string]string{"Authorization": basicAuth("alice", "pw")}

	if got := do(t, s, http.MethodDelete, "/docs/link.txt", nil, alice); got.code != http.StatusNoContent {
		t.Fatalf("删除软链返回 %d，期望 204：%s", got.code, got.body)
	}
	if _, err := os.Lstat(link); !os.IsNotExist(err) {
		t.Errorf("软链没有被删除：%v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "secret.txt")); err != nil {
		t.Errorf("软链目标被误删：%v", err)
	}
}

// TestUploadKeyCannotEscapeScopeWithBackslash 确认 `%5C` 不会被当成 /。
//
// Unix 上 `\` 是普通文件名字符；scope 检查若把它当分隔符，
// `/uploads%5Cevil` 就会通过 /uploads 的检查却写到根目录下另一个文件。
func TestUploadKeyCannotEscapeScopeWithBackslash(t *testing.T) {
	root := t.TempDir()
	s := newTestServer(t, testConfig(t, root, "-A"))
	_, token, err := s.keys.Create(uploadkey.CreateOptions{
		Scope: "/uploads",
		Kind:  uploadkey.KindUpload,
	})
	if err != nil {
		t.Fatal(err)
	}

	got := do(t, s, http.MethodPut, "/uploads%5Cevil.txt?key="+token,
		strings.NewReader("OUTSIDE"), nil)
	if got.code == http.StatusCreated {
		t.Fatalf("上传密钥通过反斜杠越出了 scope：%s", got.body)
	}
	if _, err := os.Stat(filepath.Join(root, `uploads\evil.txt`)); err == nil {
		t.Fatalf("scope 之外出现了 uploads\\evil.txt")
	}
}
