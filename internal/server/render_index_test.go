package server

import (
	"net/http"
	"path/filepath"
	"strings"
	"testing"
)

// TestRenderIndexSandboxedWhenWritable 确认可写部署里的 index.html
// 不能以同源页面执行。上传/编辑/解压任一开启都算「可写」。
func TestRenderIndexSandboxedWhenWritable(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "app", "index.html"), []byte("<html>hi</html>"))
	s := newTestServer(t, testConfig(t, root, "-A", "--render-try-index"))

	got := do(t, s, http.MethodGet, "/app/", nil, map[string]string{"Accept": "text/html"})
	if got.code != http.StatusOK {
		t.Fatalf("index 返回 %d：%s", got.code, got.body)
	}
	if csp := got.head.Get("Content-Security-Policy"); !strings.Contains(csp, "sandbox") ||
		strings.Contains(csp, "allow-same-origin") {
		t.Fatalf("可写部署的 index.html 沙箱不正确：%q", csp)
	}
}

// TestRenderIndexTrustedWhenReadOnly 确认纯只读部署不受影响。
func TestRenderIndexTrustedWhenReadOnly(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "app", "index.html"), []byte("<html>hi</html>"))
	s := newTestServer(t, testConfig(t, root, "--render-try-index"))

	got := do(t, s, http.MethodGet, "/app/", nil, map[string]string{"Accept": "text/html"})
	if got.code != http.StatusOK {
		t.Fatalf("index 返回 %d：%s", got.code, got.body)
	}
	if csp := got.head.Get("Content-Security-Policy"); strings.Contains(csp, "sandbox") {
		t.Errorf("只读部署的 index.html 不应被沙箱化：%q", csp)
	}
}

// TestUnsafeIndexOptIn 确认显式开关可以恢复同源执行。
func TestUnsafeIndexOptIn(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "app", "index.html"), []byte("<html>hi</html>"))
	s := newTestServer(t, testConfig(t, root, "-A", "--render-try-index", "--unsafe-index-html"))

	got := do(t, s, http.MethodGet, "/app/", nil, map[string]string{"Accept": "text/html"})
	if got.code != http.StatusOK {
		t.Fatalf("index 返回 %d：%s", got.code, got.body)
	}
	if csp := got.head.Get("Content-Security-Policy"); strings.Contains(csp, "sandbox") {
		t.Errorf("显式 --unsafe-index-html 后仍被沙箱化：%q", csp)
	}
}

// TestSPAFallbackSandboxedWhenWritable 确认 serveIndexFallback 同样受保护。
func TestSPAFallbackSandboxedWhenWritable(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "index.html"), []byte("<html>spa</html>"))
	s := newTestServer(t, testConfig(t, root, "-A", "--render-spa"))

	got := do(t, s, http.MethodGet, "/missing/path", nil, map[string]string{"Accept": "text/html"})
	if got.code != http.StatusOK {
		t.Fatalf("SPA 回退返回 %d：%s", got.code, got.body)
	}
	if csp := got.head.Get("Content-Security-Policy"); !strings.Contains(csp, "sandbox") {
		t.Errorf("SPA 回退的 index.html 缺少沙箱：%q", csp)
	}
}
