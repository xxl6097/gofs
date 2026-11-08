package server

import (
	"net/http"
	"path/filepath"
	"strings"
	"testing"
)

// TestPublicDir 启用鉴权 + 公开目录时，公开路径前缀匿名只读，其余路径正常鉴权。
func TestPublicDir(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "public", "a.txt"), []byte("A"))
	writeFile(t, filepath.Join(root, "private", "b.txt"), []byte("B"))
	writeFile(t, filepath.Join(root, "c.txt"), []byte("C"))

	s := newTestServer(t, testConfig(t, root, "-a", "admin:pw@/:rw"))
	// 通过 settings 设置公开目录（运行时配置）
	if err := s.settings.SetPublicDirs([]string{"/public"}); err != nil {
		t.Fatal(err)
	}

	// 公开目录：匿名可读。
	got := do(t, s, http.MethodGet, testHost+"/public/a.txt", nil, nil)
	if got.code != http.StatusOK || got.body != "A" {
		t.Errorf("/public/a.txt 匿名访问 = %d %q，期望 200 A", got.code, got.body)
	}

	// 非公开路径：匿名被拒。
	got = do(t, s, http.MethodGet, testHost+"/private/b.txt", nil, nil)
	if got.code != http.StatusUnauthorized {
		t.Errorf("/private/b.txt 匿名访问 = %d，期望 401", got.code)
	}
	got = do(t, s, http.MethodGet, testHost+"/c.txt", nil, nil)
	if got.code != http.StatusUnauthorized {
		t.Errorf("/c.txt 匿名访问 = %d，期望 401", got.code)
	}

	// 非公开路径：带正确凭据可读。
	got = do(t, s, http.MethodGet, testHost+"/private/b.txt", nil,
		map[string]string{"Authorization": basicAuth("admin", "pw")})
	if got.code != http.StatusOK || got.body != "B" {
		t.Errorf("/private/b.txt admin 访问 = %d %q，期望 200 B", got.code, got.body)
	}

	// 公开目录：匿名只读，写操作仍被拒。
	got = do(t, s, http.MethodDelete, testHost+"/public/a.txt", nil, nil)
	if got.code != http.StatusUnauthorized && got.code != http.StatusForbidden {
		t.Errorf("/public/a.txt 匿名删除 = %d，期望 401/403", got.code)
	}
}

// TestPublicDirMultiple 可指定多条公开路径前缀。
func TestPublicDirMultiple(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "docs", "a.txt"), []byte("A"))
	writeFile(t, filepath.Join(root, "public", "b.txt"), []byte("B"))
	writeFile(t, filepath.Join(root, "secret", "c.txt"), []byte("C"))

	s := newTestServer(t, testConfig(t, root, "-a", "admin:pw@/:rw"))
	if err := s.settings.SetPublicDirs([]string{"/docs", "/public"}); err != nil {
		t.Fatal(err)
	}

	for _, path := range []string{"/docs/a.txt", "/public/b.txt"} {
		got := do(t, s, http.MethodGet, testHost+path, nil, nil)
		if got.code != http.StatusOK {
			t.Errorf("%s 匿名访问 = %d，期望 200", path, got.code)
		}
	}

	got := do(t, s, http.MethodGet, testHost+"/secret/c.txt", nil, nil)
	if got.code != http.StatusUnauthorized {
		t.Errorf("/secret/c.txt 匿名访问 = %d，期望 401", got.code)
	}
}

// TestPublicDirAuthResponse 登录响应包含 public_dirs，前端据此提示「该目录公开访问」。
func TestPublicDirAuthResponse(t *testing.T) {
	root := t.TempDir()
	s := newTestServer(t, testConfig(t, root, "-a", "admin:pw@/:rw"))
	if err := s.settings.SetPublicDirs([]string{"/docs"}); err != nil {
		t.Fatal(err)
	}

	got := do(t, s, http.MethodPost, "/fs/auth", nil,
		map[string]string{"Authorization": basicAuth("admin", "pw")})
	if got.code != http.StatusOK {
		t.Fatalf("/fs/auth 返回 %d：%s", got.code, got.body)
	}
	if !strings.Contains(got.body, `"public_dirs"`) {
		t.Errorf("登录响应不含 public_dirs：%s", got.body)
	}
	if !strings.Contains(got.body, `"/docs"`) {
		t.Errorf("登录响应不含 /docs：%s", got.body)
	}
}

// TestPublicDirNoAuthUsersStillWork 公开目录与普通账号同时存在时不冲突。
func TestPublicDirNoAuthUsersStillWork(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "public", "a.txt"), []byte("A"))
	writeFile(t, filepath.Join(root, "private", "b.txt"), []byte("B"))

	s := newTestServer(t, testConfig(t, root, "-a", "alice:pass@/private:r"))
	if err := s.settings.SetPublicDirs([]string{"/public"}); err != nil {
		t.Fatal(err)
	}

	// alice 能读 /private，不能读 /public（因为她的规则只授予了 /private）。
	got := do(t, s, http.MethodGet, testHost+"/private/b.txt", nil,
		map[string]string{"Authorization": basicAuth("alice", "pass")})
	if got.code != http.StatusOK {
		t.Errorf("alice 读 /private/b.txt = %d，期望 200", got.code)
	}

	// 但是 alice 通过匿名规则也能读 /public（分享与显式身份取较高权限）。
	got = do(t, s, http.MethodGet, testHost+"/public/a.txt", nil,
		map[string]string{"Authorization": basicAuth("alice", "pass")})
	if got.code != http.StatusOK {
		t.Errorf("alice 读 /public/a.txt = %d，期望 200", got.code)
	}

	// 匿名可以读 /public。
	got = do(t, s, http.MethodGet, testHost+"/public/a.txt", nil, nil)
	if got.code != http.StatusOK {
		t.Errorf("匿名读 /public/a.txt = %d，期望 200", got.code)
	}
}
