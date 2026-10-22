package server

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestCaseVariantCannotBypassReadOnlyRule 确认 ACL 按磁盘真实名字匹配。
//
// 在大小写不敏感的文件系统（macOS/Windows 默认）上，
// `/docs/secret.txt` 会打开 `/docs/Secret.txt`。如果 ACL 按请求里的写法
// 做字符串前缀匹配，就会命中更宽的 `/docs:rw`，绕过 `/docs/Secret.txt:r`。
func TestCaseVariantCannotBypassReadOnlyRule(t *testing.T) {
	root := t.TempDir()
	probe := filepath.Join(root, "Probe.txt")
	if err := os.WriteFile(probe, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	a, e1 := os.Stat(probe)
	b, e2 := os.Stat(filepath.Join(root, "probe.txt"))
	if e1 != nil || e2 != nil || !os.SameFile(a, b) {
		t.Skip("当前文件系统区分大小写")
	}

	writeFile(t, filepath.Join(root, "docs", "Secret.txt"), []byte("ORIGINAL"))
	s := newTestServer(t, testConfig(t, root, "-A", "--auth-fail-limit", "0",
		"-a", "alice:pw@/docs:rw,/docs/Secret.txt:r"))
	alice := map[string]string{"Authorization": basicAuth("alice", "pw")}

	// 正常大小写：只读规则生效。
	if got := do(t, s, http.MethodPut, "/docs/Secret.txt", strings.NewReader("HACKED"), alice); got.code == http.StatusCreated {
		t.Fatalf("只读规则未生效：%s", got.body)
	}
	// 变大小写：必须同样被拒绝。
	if got := do(t, s, http.MethodPut, "/docs/secret.txt", strings.NewReader("HACKED"), alice); got.code == http.StatusCreated {
		t.Fatalf("大小写变体绕过了只读规则：%s", got.body)
	}
	if data, _ := os.ReadFile(filepath.Join(root, "docs", "Secret.txt")); string(data) == "HACKED" {
		t.Fatalf("只读文件被改写")
	}

	// 正常访问自己的目录仍然可用。
	if got := do(t, s, http.MethodGet, "/docs/Secret.txt", nil, alice); got.code != http.StatusOK {
		t.Fatalf("正常读取返回 %d：%s", got.code, got.body)
	}
}
