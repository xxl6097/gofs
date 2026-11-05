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

// fakeMultiRoot 模拟 gofs 的多根解析：首段命中挂载标签时交给另一个目录，
// 其余路径交给主目录。
type fakeMultiRoot struct {
	primary string
	mounts  map[string]string
}

func (f fakeMultiRoot) Resolve(urlPath string) (string, error) {
	clean := filepath.Clean("/" + strings.TrimPrefix(urlPath, "/"))
	trimmed := strings.TrimPrefix(clean, "/")
	first, rest := trimmed, "/"
	if i := strings.IndexByte(trimmed, '/'); i >= 0 {
		first, rest = trimmed[:i], trimmed[i:]
	}
	if root, ok := f.mounts[first]; ok {
		full := filepath.Join(root, filepath.FromSlash(rest))
		if full != root && !strings.HasPrefix(full, root+string(filepath.Separator)) {
			return "", os.ErrPermission
		}
		return full, nil
	}
	return filepath.Join(f.primary, filepath.FromSlash(clean)), nil
}

func (f fakeMultiRoot) IsRoot(abs string) bool {
	abs = filepath.Clean(abs)
	if abs == filepath.Clean(f.primary) {
		return true
	}
	for _, root := range f.mounts {
		if filepath.Clean(root) == abs {
			return true
		}
	}
	return false
}

// TestMountedSiteAllowedDirs 覆盖本功能的核心：账号的授权目录可以是附加站点
// 的标签（"/work"），目录校验、读写和目录浏览都要落到站点目录上。
func TestMountedSiteAllowedDirs(t *testing.T) {
	primary := t.TempDir()
	site := t.TempDir()
	if err := os.MkdirAll(filepath.Join(primary, "docs"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(primary, "docs", "p.txt"), []byte("primary"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(primary, "hello.txt"), []byte("primary-root"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(site, "hello.txt"), []byte("mounted"), 0644); err != nil {
		t.Fatal(err)
	}

	server, err := New(primary)
	if err != nil {
		t.Fatalf("创建服务器失败: %v", err)
	}
	server.SetResolver(fakeMultiRoot{primary: primary, mounts: map[string]string{"work": site}})
	server.SetPrefix("/dav/")

	// 挂载站点标签必须能通过目录校验（不能只 stat 主目录下的 work）。
	if err := server.AddAccount("alice", "secret", []string{"/work"}); err != nil {
		t.Fatalf("挂载站点目录校验失败: %v", err)
	}
	// 不存在的路径仍然要拒绝。
	if err := server.AddAccount("bob", "secret", []string{"/missing"}); err == nil {
		t.Error("不存在的目录不应通过校验")
	}

	do := func(method, path, body string, hdr map[string]string) *httptest.ResponseRecorder {
		var rd *strings.Reader
		if body == "" {
			rd = strings.NewReader("")
		} else {
			rd = strings.NewReader(body)
		}
		req := httptest.NewRequest(method, path, rd)
		req.SetBasicAuth("alice", "secret")
		for k, v := range hdr {
			req.Header.Set(k, v)
		}
		w := httptest.NewRecorder()
		server.ServeHTTP(w, req)
		return w
	}

	// 站点内的文件真的从站点目录读出来，而不是主目录。
	if w := do("GET", "/dav/work/hello.txt", "", nil); w.Code != http.StatusOK {
		t.Fatalf("GET /dav/work/hello.txt 期望 200，得到 %d", w.Code)
	} else if w.Body.String() != "mounted" {
		t.Fatalf("读到的不是站点文件: %q", w.Body.String())
	}

	// 主目录里的同名文件不在授权范围内。
	if w := do("GET", "/dav/hello.txt", "", nil); w.Code != http.StatusForbidden {
		t.Errorf("GET /dav/hello.txt 期望 403，得到 %d", w.Code)
	}

	// 浏览器打开站点根目录应列出站点内容。
	if w := do("GET", "/dav/work/", "", nil); w.Code != http.StatusOK ||
		!strings.Contains(w.Body.String(), "hello.txt") {
		t.Errorf("站点目录浏览失败: %d %q", w.Code, w.Body.String())
	}

	// 写入也落到站点目录。
	if w := do("PUT", "/dav/work/new.txt", "new", nil); w.Code != http.StatusCreated {
		t.Fatalf("PUT 到站点期望 201，得到 %d: %s", w.Code, w.Body.String())
	}
	if data, err := os.ReadFile(filepath.Join(site, "new.txt")); err != nil || string(data) != "new" {
		t.Fatalf("文件没有写到站点目录: %v %q", err, data)
	}

	// 站点根目录是挂载点，不能被 DELETE 端掉；里面的文件仍可删除。
	if w := do("DELETE", "/dav/work", "", nil); w.Code == http.StatusNoContent || w.Code == http.StatusOK {
		t.Errorf("DELETE 站点根目录不应成功，得到 %d", w.Code)
	}
	if _, err := os.Stat(site); err != nil {
		t.Fatalf("站点目录被删掉了: %v", err)
	}
	if w := do("DELETE", "/dav/work/new.txt", "", nil); w.Code != http.StatusNoContent {
		t.Errorf("DELETE 站点内文件期望 204，得到 %d", w.Code)
	}
}

// TestCopyDestinationChecksACL 目标路径也要过 ACL，不能借 COPY/MOVE 把
// 只授权目录里的内容写到别的目录（包括挂载站点）。
func TestCopyDestinationChecksACL(t *testing.T) {
	primary := t.TempDir()
	site := t.TempDir()
	if err := os.MkdirAll(filepath.Join(primary, "docs"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(primary, "docs", "a.txt"), []byte("A"), 0644); err != nil {
		t.Fatal(err)
	}

	server, err := New(primary)
	if err != nil {
		t.Fatalf("创建服务器失败: %v", err)
	}
	server.SetResolver(fakeMultiRoot{primary: primary, mounts: map[string]string{"work": site}})
	server.SetPrefix("/dav/")
	if err := server.AddAccount("alice", "secret", []string{"/docs"}); err != nil {
		t.Fatalf("添加账号失败: %v", err)
	}

	req := httptest.NewRequest("COPY", "/dav/docs/a.txt", nil)
	req.SetBasicAuth("alice", "secret")
	req.Header.Set("Destination", "http://example.com/dav/work/a.txt")
	w := httptest.NewRecorder()
	server.ServeHTTP(w, req)

	if w.Code != http.StatusForbidden {
		t.Fatalf("复制到未授权站点期望 403，得到 %d: %s", w.Code, w.Body.String())
	}
	if _, err := os.Stat(filepath.Join(site, "a.txt")); err == nil {
		t.Error("文件竟然被复制到了未授权的站点")
	}
	if _, err := os.Stat(filepath.Join(primary, "docs", "a.txt")); err != nil {
		t.Errorf("源文件被弄没了: %v", err)
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
