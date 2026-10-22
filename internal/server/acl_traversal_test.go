package server

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestACLRejectsEncodedDotDot 确认服务端用**同一个规范路径**做鉴权与落盘解析。
//
// 背景：ServeMux 只对转义后的路径做规范化，`/docs/%2e%2e/secret.txt` 不会
// 触发重定向，而 r.URL.Path 已经解码成 `/docs/../secret.txt`。早先鉴权按
// 原始路径匹配（命中 /docs），Resolve 却把 `..` 折叠掉了 —— 受限账号因此
// 可以读写自己目录之外的文件。
func TestACLRejectsEncodedDotDot(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "docs", "ok.txt"), []byte("ok"))
	writeFile(t, filepath.Join(root, "secret.txt"), []byte("TOP-SECRET"))

	// 关掉认证失败封禁：本用例会连续发多次越权请求，被封禁后拿到的 429
	// 会掩盖真正要断言的东西（越权请求本身是否放行）。
	s := newTestServer(t, testConfig(t, root, "-A",
		"--auth-fail-limit", "0",
		"-a", "alice:pw@/docs:rw",
		"-a", "reader:r@/docs:r"))

	alice := map[string]string{"Authorization": basicAuth("alice", "pw")}
	reader := map[string]string{"Authorization": basicAuth("reader", "r")}

	// 正常访问自己的目录仍然可用。
	if got := do(t, s, http.MethodGet, "/docs/ok.txt", nil, alice); got.code != http.StatusOK {
		t.Fatalf("正常读取 /docs/ok.txt 返回 %d：%s", got.code, got.body)
	}

	// 读：普通越界与编码越界都必须被拒绝。
	for _, path := range []string{
		"/secret.txt",
		"/docs/%2e%2e/secret.txt",
		"/docs/%2E%2E/secret.txt",
		"/docs/./%2e%2e/secret.txt",
		"/docs/%2e%2e%2fsecret.txt",
	} {
		for name, hdr := range map[string]map[string]string{"reader": reader, "alice": alice} {
			got := do(t, s, http.MethodGet, path, nil, hdr)
			if got.code == http.StatusOK || strings.Contains(got.body, "TOP-SECRET") {
				t.Errorf("%s 通过 %s 读到了 ACL 之外的文件（%d）：%s", name, path, got.code, got.body)
			}
		}
	}

	// 写：编码越界不能创建文件。
	got := do(t, s, http.MethodPut, "/docs/%2e%2e/pwn.txt", strings.NewReader("pwned"), alice)
	if got.code == http.StatusCreated {
		t.Errorf("编码越界 PUT 竟然成功：%s", got.body)
	}
	if _, err := os.Stat(filepath.Join(root, "pwn.txt")); err == nil {
		t.Errorf("编码越界 PUT 在 /docs 之外创建了 pwn.txt")
	}

	// 删除：编码越界不能删除文件。
	got = do(t, s, http.MethodDelete, "/docs/%2e%2e/secret.txt", nil, alice)
	if got.code == http.StatusNoContent {
		t.Errorf("编码越界 DELETE 竟然成功")
	}
	if _, err := os.Stat(filepath.Join(root, "secret.txt")); err != nil {
		t.Errorf("/docs 之外的 secret.txt 被删除了")
	}

	// 多站点边界同样不能靠编码 .. 穿越：折叠后会落到另一个站点的标签下。
	base := t.TempDir()
	dataDir := filepath.Join(base, "data")
	backupDir := filepath.Join(base, "backup")
	writeFile(t, filepath.Join(dataDir, "docs", "ok.txt"), []byte("ok"))
	writeFile(t, filepath.Join(backupDir, "o.txt"), []byte("other-site"))
	multi := newTestServer(t, testConfigRoots(t,
		[]string{dataDir, backupDir}, "-A", "-a", "alice:pw@/docs:rw"))

	got = do(t, multi, http.MethodGet, "/docs/%2e%2e/backup/o.txt", nil, alice)
	if got.code == http.StatusOK || strings.Contains(got.body, "other-site") {
		t.Errorf("编码越界跨站点读取成功（%d）：%s", got.code, got.body)
	}
}
