package webdav

import (
	"io/ioutil"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWebDAVServer(t *testing.T) {
	// 创建临时目录
	tmpDir, err := ioutil.TempDir("", "webdav-test-*")
	if err != nil {
		t.Fatalf("创建临时目录失败: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	// 创建测试子目录
	docsDir := filepath.Join(tmpDir, "docs")
	sharedDir := filepath.Join(tmpDir, "shared")
	if err := os.MkdirAll(docsDir, 0755); err != nil {
		t.Fatalf("创建 docs 目录失败: %v", err)
	}
	if err := os.MkdirAll(sharedDir, 0755); err != nil {
		t.Fatalf("创建 shared 目录失败: %v", err)
	}

	// 创建服务器
	server, err := New(tmpDir)
	if err != nil {
		t.Fatalf("创建服务器失败: %v", err)
	}

	// 添加账号
	err = server.AddAccount("testuser", "testpass", []string{"/docs"})
	if err != nil {
		t.Fatalf("添加账号失败: %v", err)
	}

	t.Run("认证测试", func(t *testing.T) {
		req := httptest.NewRequest("PROPFIND", "/docs/", nil)
		w := httptest.NewRecorder()

		// 无认证
		server.ServeHTTP(w, req)
		if w.Code != http.StatusUnauthorized {
			t.Errorf("期望状态码 401，得到 %d", w.Code)
		}

		// 错误密码
		req.SetBasicAuth("testuser", "wrongpass")
		w = httptest.NewRecorder()
		server.ServeHTTP(w, req)
		if w.Code != http.StatusUnauthorized {
			t.Errorf("期望状态码 401，得到 %d", w.Code)
		}

		// 正确认证
		req.SetBasicAuth("testuser", "testpass")
		w = httptest.NewRecorder()
		server.ServeHTTP(w, req)
		if w.Code == http.StatusUnauthorized {
			t.Errorf("不应返回 401")
		}
	})

	t.Run("路径权限测试", func(t *testing.T) {
		// 允许的路径
		req := httptest.NewRequest("PROPFIND", "/docs/", nil)
		req.SetBasicAuth("testuser", "testpass")
		w := httptest.NewRecorder()
		server.ServeHTTP(w, req)
		if w.Code == http.StatusForbidden {
			t.Error("不应拒绝访问 /docs/")
		}

		// 不允许的路径
		req = httptest.NewRequest("PROPFIND", "/shared/", nil)
		req.SetBasicAuth("testuser", "testpass")
		w = httptest.NewRecorder()
		server.ServeHTTP(w, req)
		if w.Code != http.StatusForbidden {
			t.Errorf("应该拒绝访问 /shared/，得到状态码 %d", w.Code)
		}
	})
}

// TestPrefixedMount 覆盖挂载前缀的两种职责：请求路径要剥掉 "/dav/" 才能
// 映射到磁盘，目录 ACL 也要拿剥掉前缀后的路径来比对。
func TestPrefixedMount(t *testing.T) {
	tmpDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(tmpDir, "docs"), 0755); err != nil {
		t.Fatalf("创建 docs 目录失败: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(tmpDir, "shared"), 0755); err != nil {
		t.Fatalf("创建 shared 目录失败: %v", err)
	}
	if err := os.WriteFile(filepath.Join(tmpDir, "docs", "a.txt"), []byte("hello"), 0644); err != nil {
		t.Fatalf("写入测试文件失败: %v", err)
	}

	server, err := New(tmpDir)
	if err != nil {
		t.Fatalf("创建服务器失败: %v", err)
	}
	server.SetPrefix("/dav/")
	if err := server.AddAccount("alice", "secret", []string{"/docs"}); err != nil {
		t.Fatalf("添加账号失败: %v", err)
	}

	do := func(method, path string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, nil)
		req.SetBasicAuth("alice", "secret")
		w := httptest.NewRecorder()
		server.ServeHTTP(w, req)
		return w
	}

	// 前缀内的授权目录：能真正读到磁盘上的 docs/a.txt。
	if w := do("GET", "/dav/docs/a.txt"); w.Code != http.StatusOK {
		t.Fatalf("GET /dav/docs/a.txt 期望 200，得到 %d", w.Code)
	} else if w.Body.String() != "hello" {
		t.Fatalf("文件内容不对: %q", w.Body.String())
	}

	// 浏览器直接 GET 目录：给出可点击的文件列表，而不是 WebDAV 的 405。
	if w := do("GET", "/dav/docs/"); w.Code != http.StatusOK {
		t.Fatalf("GET /dav/docs/ 期望 200，得到 %d", w.Code)
	} else if body := w.Body.String(); !strings.Contains(body, "a.txt") ||
		!strings.Contains(body, `href="./a.txt"`) {
		t.Fatalf("目录列表应有 a.txt 链接，实际: %q", body)
	}

	// 前缀内、但不在授权目录：403。
	if w := do("GET", "/dav/shared/"); w.Code != http.StatusForbidden {
		t.Errorf("GET /dav/shared/ 期望 403，得到 %d", w.Code)
	}

	// 根路径不在授权范围：403（不是 401/404），并在响应里说明授权目录，
	// 否则用户只看到「无权访问」，不知道要连 /dav/docs/。
	if w := do("PROPFIND", "/dav/"); w.Code != http.StatusForbidden {
		t.Errorf("PROPFIND /dav/ 期望 403，得到 %d", w.Code)
	} else if !strings.Contains(w.Body.String(), "/docs") {
		t.Errorf("403 响应应提示授权目录，实际: %q", w.Body.String())
	}
}

func TestAccountManagement(t *testing.T) {
	tmpDir, err := ioutil.TempDir("", "webdav-test-*")
	if err != nil {
		t.Fatalf("创建临时目录失败: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	// 创建测试目录
	testDir := filepath.Join(tmpDir, "test")
	if err := os.MkdirAll(testDir, 0755); err != nil {
		t.Fatalf("创建测试目录失败: %v", err)
	}

	server, err := New(tmpDir)
	if err != nil {
		t.Fatalf("创建服务器失败: %v", err)
	}

	t.Run("添加账号", func(t *testing.T) {
		err := server.AddAccount("user1", "pass1", []string{"/test"})
		if err != nil {
			t.Errorf("添加账号失败: %v", err)
		}

		// 重复添加
		err = server.AddAccount("user1", "pass1", []string{"/test"})
		if err == nil {
			t.Error("应该拒绝重复的用户名")
		}
	})

	t.Run("列出账号", func(t *testing.T) {
		accounts := server.ListAccounts()
		if len(accounts) != 1 {
			t.Errorf("期望 1 个账号，得到 %d", len(accounts))
		}
		if accounts[0].Username != "user1" {
			t.Errorf("期望用户名 'user1'，得到 '%s'", accounts[0].Username)
		}
		if accounts[0].PasswordHash != "" {
			t.Error("列出的账号不应包含密码哈希")
		}
	})

	t.Run("更新账号", func(t *testing.T) {
		err := server.UpdateAccount("user1", []string{"/test", "/"})
		if err != nil {
			t.Errorf("更新账号失败: %v", err)
		}

		// 更新不存在的账号
		err = server.UpdateAccount("nonexistent", []string{"/test"})
		if err == nil {
			t.Error("应该拒绝更新不存在的账号")
		}
	})

	t.Run("修改密码", func(t *testing.T) {
		err := server.ChangePassword("user1", "newpass")
		if err != nil {
			t.Errorf("修改密码失败: %v", err)
		}

		// 验证新密码
		if !server.authenticate("user1", "newpass") {
			t.Error("新密码应该有效")
		}
		if server.authenticate("user1", "pass1") {
			t.Error("旧密码应该无效")
		}
	})

	t.Run("删除账号", func(t *testing.T) {
		err := server.RemoveAccount("user1")
		if err != nil {
			t.Errorf("删除账号失败: %v", err)
		}

		accounts := server.ListAccounts()
		if len(accounts) != 0 {
			t.Errorf("期望 0 个账号，得到 %d", len(accounts))
		}
	})
}

func TestPathPermissions(t *testing.T) {
	tmpDir, err := ioutil.TempDir("", "webdav-test-*")
	if err != nil {
		t.Fatalf("创建临时目录失败: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	// 创建测试目录结构
	dirs := []string{"docs", "docs/private", "shared", "admin"}
	for _, dir := range dirs {
		if err := os.MkdirAll(filepath.Join(tmpDir, dir), 0755); err != nil {
			t.Fatalf("创建目录 %s 失败: %v", dir, err)
		}
	}

	server, err := New(tmpDir)
	if err != nil {
		t.Fatalf("创建服务器失败: %v", err)
	}

	// 添加测试账号
	err = server.AddAccount("user1", "pass1", []string{"/docs", "/shared"})
	if err != nil {
		t.Fatalf("添加账号失败: %v", err)
	}

	tests := []struct {
		name     string
		path     string
		expected bool
	}{
		{"根目录", "/", false},
		{"允许的目录 - docs", "/docs", true},
		{"允许的子目录", "/docs/private", true},
		{"允许的目录 - shared", "/shared", true},
		{"不允许的目录", "/admin", false},
		{"路径遍历尝试", "/docs/../admin", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := server.isPathAllowed("user1", tt.path)
			if result != tt.expected {
				t.Errorf("isPathAllowed(%q) = %v, 期望 %v", tt.path, result, tt.expected)
			}
		})
	}
}
