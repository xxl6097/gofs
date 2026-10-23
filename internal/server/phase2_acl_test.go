package server

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestTrailingSpaceDoesNotGetBroaderPerm 确认 ACL 不会 trim 请求路径。
//
// alice 对 /docs 只读、只对 /docs/uploads 可写。`/docs/uploads ` 是另一个
// 条目，按 /docs:r 应当只读；修复前它会被 trim 成 /docs/uploads 并拿到 rw。
func TestTrailingSpaceDoesNotGetBroaderPerm(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "docs", "uploads", "ok.txt"), []byte("ok"))
	writeFile(t, filepath.Join(root, "docs", "uploads "), []byte("SECRET-SIBLING"))

	s := newTestServer(t, testConfig(t, root, "-A", "--auth-fail-limit", "0",
		"-a", "alice:pw@/docs:r,/docs/uploads:rw"))
	alice := map[string]string{"Authorization": basicAuth("alice", "pw")}

	// 正常目录仍然可写。
	if got := do(t, s, http.MethodPut, "/docs/uploads/new.txt",
		strings.NewReader("ok"), alice); got.code != http.StatusCreated {
		t.Fatalf("正常可写目录返回 %d：%s", got.code, got.body)
	}

	// 尾随空格的兄弟条目只应按 /docs:r 判定，不能被 PUT/MKCOL 创建或覆盖。
	if got := do(t, s, http.MethodPut, "/docs/uploads%20",
		strings.NewReader("HACKED"), alice); got.code != http.StatusForbidden {
		t.Fatalf("尾随空格路径 PUT 返回 %d，期望 403：%s", got.code, got.body)
	}
	if got := do(t, s, MethodMkcol, "/docs/uploads%20", nil, alice); got.code != http.StatusForbidden {
		t.Fatalf("尾随空格路径 MKCOL 返回 %d，期望 403：%s", got.code, got.body)
	}
	data, err := os.ReadFile(filepath.Join(root, "docs", "uploads "))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "SECRET-SIBLING" {
		t.Fatalf("兄弟条目被改写：%q", data)
	}
}
