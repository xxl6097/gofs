package server

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestRecursiveDeleteChecksDescendantACL 确认递归删除不能带走只读子项。
func TestRecursiveDeleteChecksDescendantACL(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "docs", "keep.txt"), []byte("KEEP"))
	writeFile(t, filepath.Join(root, "docs", "ok.txt"), []byte("ok"))
	s := newTestServer(t, testConfig(t, root, "-A", "--auth-fail-limit", "0",
		"-a", "alice:pw@/docs:rw,/docs/keep.txt:r"))
	alice := map[string]string{"Authorization": basicAuth("alice", "pw")}

	if got := do(t, s, http.MethodDelete, "/docs/keep.txt", nil, alice); got.code != http.StatusForbidden {
		t.Fatalf("直接删除只读子项返回 %d，期望 403", got.code)
	}
	got := do(t, s, http.MethodDelete, "/docs?recursive", nil, alice)
	if got.code == http.StatusNoContent {
		t.Fatalf("递归删除绕过了子项只读规则")
	}
	if _, err := os.Stat(filepath.Join(root, "docs", "keep.txt")); err != nil {
		t.Fatalf("只读子项被删除：%v", err)
	}
}

// TestMoveDirectoryChecksDescendantACL 确认目录移动不能把只读子项
// 搬到规则更宽的位置后再改写。
func TestMoveDirectoryChecksDescendantACL(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "docs", "keep.txt"), []byte("KEEP"))
	os.MkdirAll(filepath.Join(root, "other"), 0o755)
	s := newTestServer(t, testConfig(t, root, "-A", "--auth-fail-limit", "0",
		"-a", "alice:pw@/docs:rw,/docs/keep.txt:r,/other:rw"))
	alice := map[string]string{"Authorization": basicAuth("alice", "pw")}

	got := do(t, s, MethodMove, "/docs", nil, map[string]string{
		"Authorization": alice["Authorization"],
		"Destination":   "/other/docs",
	})
	if got.code == http.StatusCreated {
		t.Fatalf("目录移动绕过了后代 ACL")
	}
	if _, err := os.Stat(filepath.Join(root, "docs", "keep.txt")); err != nil {
		t.Errorf("源目录已被移动：%v", err)
	}
}

// TestRenameBatchChecksDestinationACL 确认批量改名不能把条目改到一个
// 被配置为只读的目标名上。
func TestRenameBatchChecksDestinationACL(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "docs", "a.txt"), []byte("A"))
	s := newTestServer(t, testConfig(t, root, "-A", "--auth-fail-limit", "0",
		"-a", "alice:pw@/docs:rw,/docs/locked.txt:r"))
	alice := map[string]string{"Authorization": basicAuth("alice", "pw")}

	body := `{"items":[{"path":"/docs/a.txt","name":"locked.txt"}]}`
	got := do(t, s, http.MethodPost, "/fs/rename-batch", strings.NewReader(body), alice)
	if got.code == http.StatusOK {
		t.Fatalf("批量改名把文件写到了只读目标路径：%s", got.body)
	}
	if _, err := os.Stat(filepath.Join(root, "docs", "a.txt")); err != nil {
		t.Errorf("源文件被改走了：%v", err)
	}
}
