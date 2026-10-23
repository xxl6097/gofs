package server

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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

// TestDatedUploadDoesNotBypassACL 确认日期归档改写目标之后仍按真实路径复核权限。
//
// 根目录里 `2026 -> private`，bob 对 /2026 有写权限、对 /private 只有读权限。
// 修复前 PUT /x.txt 会被归档到 /2026/…/x.txt，跟随软链实际写进 private；
// 修复后必须 403，且 private 下不能出现文件。
func TestDatedUploadDoesNotBypassACL(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "private"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("private", filepath.Join(root, "2026")); err != nil {
		t.Skipf("当前环境不支持创建软链接：%v", err)
	}

	s := newTestServer(t, testConfigDated(t, root, "2006/01/02",
		"--allow-upload", "--auth-fail-limit", "0",
		"-a", "bob:pw@/x.txt:rw,/2026:rw,/private:r"))

	got := do(t, s, http.MethodPut, "/x.txt", strings.NewReader("HACKED"),
		map[string]string{"Authorization": basicAuth("bob", "pw")})
	if got.code == http.StatusCreated {
		t.Fatalf("日期归档绕过了 ACL：%s", got.body)
	}

	target := filepath.Join(root, "private", time.Now().Format("01/02"), "x.txt")
	if _, err := os.Stat(target); err == nil {
		t.Fatalf("ACL 之外的目录里出现了文件：%s", target)
	}
}

// TestDatedUploadKeyCannotEscapeScope 确认 ?dated=1 强制归档时，上传密钥也按
// 真实目标复核 scope。
//
// `/2026/2026 -> ../other`，密钥 scope=/2026：
// 请求 /2026/x.txt?dated=1 会被改写成 /2026/2026/…/x.txt，跟随软链写入 other/。
func TestDatedUploadKeyCannotEscapeScope(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "2026"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "other"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../other", filepath.Join(root, "2026", "2026")); err != nil {
		t.Skipf("当前环境不支持创建软链接：%v", err)
	}

	s := newTestServer(t, testConfigDated(t, root, "2006/01/02", "--allow-upload"))
	_, token, err := s.keys.Create(uploadkey.CreateOptions{
		Name: "scope-test", Scope: "/2026", Kind: uploadkey.KindUpload,
	})
	if err != nil {
		t.Fatal(err)
	}

	got := do(t, s, http.MethodPut, "/2026/x.txt?dated=1&key="+token,
		strings.NewReader("HACKED"), nil)
	if got.code == http.StatusCreated {
		t.Fatalf("日期归档让上传密钥越过了 scope：%s", got.body)
	}

	target := filepath.Join(root, "other", time.Now().Format("01/02"), "x.txt")
	if _, err := os.Stat(target); err == nil {
		t.Fatalf("scope 之外的目录里出现了文件：%s", target)
	}
}
