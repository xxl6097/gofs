package gofs_test

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/xxl6097/gofs"
)

// newServer 起一个只服务临时目录的实例。
//
// 密钥与用户表都改到临时目录：默认值指向用户配置目录，
// 跑测试时不该去碰开发者本机的真实文件。
func newServer(t *testing.T, opts ...gofs.Option) (*gofs.Server, string) {
	t.Helper()
	root := t.TempDir()
	state := t.TempDir()

	base := []gofs.Option{
		gofs.WithRoot(root),
		gofs.WithPort(0),
		gofs.WithKeyFile(filepath.Join(state, "keys.json")),
		gofs.WithUserFile(filepath.Join(state, "users.json")),
	}
	srv, err := gofs.New(append(base, opts...)...)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = srv.Close() })
	return srv, root
}

// get 发一个请求并返回状态码与响应体。
func get(t *testing.T, client *http.Client, url string) (int, string) {
	t.Helper()
	resp, err := client.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("读响应体: %v", err)
	}
	return resp.StatusCode, string(body)
}

// TestStartServesAndShutsDown 覆盖最常见的用法：端口 0 启动、访问、优雅关闭。
func TestStartServesAndShutsDown(t *testing.T) {
	srv, root := newServer(t)
	if err := os.WriteFile(filepath.Join(root, "hello.txt"), []byte("hi"), 0o644); err != nil {
		t.Fatalf("准备测试文件: %v", err)
	}

	if err := srv.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if srv.Addr() == nil {
		t.Fatal("Start 之后 Addr 不该为 nil")
	}
	if port := srv.Addr().(*net.TCPAddr).Port; port == 0 {
		t.Fatal("Port 为 0 时应拿到系统分配的真实端口")
	}

	client := &http.Client{Timeout: 5 * time.Second}
	if code, body := get(t, client, srv.URL()+"/"); code != http.StatusOK {
		t.Fatalf("根目录返回 %d，期望 200：%s", code, body)
	}
	if code, body := get(t, client, srv.URL()+"/hello.txt"); code != http.StatusOK || body != "hi" {
		t.Fatalf("下载文件返回 %d / %q，期望 200 / \"hi\"", code, body)
	}

	// 重复 Start 要挡住。
	if err := srv.Start(); err != gofs.ErrStarted {
		t.Fatalf("重复 Start 返回 %v，期望 ErrStarted", err)
	}

	url := srv.URL()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	if err := srv.Wait(); err != nil {
		t.Fatalf("Wait 在主动关闭后应返回 nil，得到 %v", err)
	}
	if _, err := client.Get(url + "/"); err == nil {
		t.Fatal("关闭之后还能连上")
	}
}

// TestHandlerMountedOnExternalMux 覆盖「只借用 HTTP 层」的用法。
func TestHandlerMountedOnExternalMux(t *testing.T) {
	srv, root := newServer(t, gofs.WithPathPrefix("/files"))
	if err := os.WriteFile(filepath.Join(root, "note.txt"), []byte("x"), 0o644); err != nil {
		t.Fatalf("准备测试文件: %v", err)
	}

	mux := http.NewServeMux()
	mux.Handle("/files/", srv.Handler())
	ts := httptest.NewServer(mux)
	defer ts.Close()

	code, body := get(t, ts.Client(), ts.URL+"/files/")
	if code != http.StatusOK {
		t.Fatalf("挂载路径返回 %d，期望 200：%s", code, body)
	}
	if !strings.Contains(body, "note.txt") {
		t.Fatalf("目录列表里没有 note.txt：%s", body)
	}
}

// shortTempDir 返回一个路径尽量短的临时目录。
//
// t.TempDir() 会把测试名嵌进路径里，unix socket 的 sun_path 只有 104 字节
// （macOS）/ 108 字节（Linux），名字稍长就会超限 —— 那是测试本身的问题，
// 不该被误当成「平台不支持」跳过。
func shortTempDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "gofs")
	if err != nil {
		t.Fatalf("创建临时目录: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

// TestUnixSocketReclaimsStaleFile 确认残留 socket 文件会被清掉后重新监听。
func TestUnixSocketReclaimsStaleFile(t *testing.T) {
	sock := filepath.Join(shortTempDir(t), "gofs.sock")
	// 先放一个残留文件，模拟上次没清干净的场景。
	if err := os.WriteFile(sock, nil, 0o600); err != nil {
		t.Fatalf("准备残留 socket: %v", err)
	}

	srv, _ := newServer(t, gofs.WithUnixSocket(sock))
	if err := srv.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}

	client := &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, "unix", sock)
			},
		},
		Timeout: 5 * time.Second,
	}
	if code, body := get(t, client, "http://unix/"); code != http.StatusOK {
		t.Fatalf("unix socket 返回 %d，期望 200：%s", code, body)
	}
	if got := srv.URL(); !strings.HasPrefix(got, "http+unix://") {
		t.Fatalf("URL 返回 %q，期望 http+unix:// 前缀", got)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
}

// TestRunStopsOnContextCancel 确认 Run 跟随 ctx 退出且不吞错误。
func TestRunStopsOnContextCancel(t *testing.T) {
	srv, _ := newServer(t)
	ctx, cancel := context.WithCancel(context.Background())

	errCh := make(chan error, 1)
	go func() { errCh <- srv.Run(ctx) }()

	// 等服务真正起来。
	deadline := time.Now().Add(5 * time.Second)
	for srv.Addr() == nil {
		if time.Now().After(deadline) {
			t.Fatal("Run 迟迟没有开始监听")
		}
		time.Sleep(10 * time.Millisecond)
	}

	cancel()
	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("Run 因 ctx 取消退出时应返回 nil，得到 %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("Run 没有在 ctx 取消后退出")
	}
}

// TestNewValidatesConfig 确认 New 会跑 Normalize —— 非法配置要当场报错，
// 而不是等到运行期才出问题。
func TestNewValidatesConfig(t *testing.T) {
	cases := []struct {
		name string
		opt  gofs.Option
	}{
		{"非法压缩级别", gofs.WithTune(func(c *gofs.Config) { c.Compress = "bogus" })},
		{"非法端口", gofs.WithPort(70000)},
		{"只给了证书没给私钥", gofs.WithTLS("cert.pem", "")},
		{"上传日期布局越界", gofs.WithUploadDateLayout("../2006")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := gofs.New(gofs.WithRoot(t.TempDir()), tc.opt); err == nil {
				t.Fatal("非法配置应当返回错误")
			}
		})
	}
}

// TestAllowAllExpandsPermissions 确认 -A 的语义在库侧同样生效。
func TestAllowAllExpandsPermissions(t *testing.T) {
	srv, _ := newServer(t, gofs.WithAllowAll())
	cfg := srv.Config()
	for name, on := range map[string]bool{
		"upload":  cfg.AllowUpload,
		"edit":    cfg.AllowEdit,
		"delete":  cfg.AllowDelete,
		"search":  cfg.AllowSearch,
		"archive": cfg.AllowArchive,
		"extract": cfg.AllowExtract,
		"keys":    cfg.AllowKeys,
	} {
		if !on {
			t.Errorf("WithAllowAll 之后 %s 权限仍是关闭的", name)
		}
	}
}

// TestDefaultIsReadOnly 确认默认配置不带任何写权限 ——
// 这是库的安全默认值，回归了会很危险。
func TestDefaultIsReadOnly(t *testing.T) {
	cfg := gofs.Default()
	if cfg.AllowUpload || cfg.AllowDelete || cfg.AllowEdit || cfg.AllowExtract {
		t.Fatalf("默认配置不该带写权限: %+v", cfg)
	}
	if cfg.ServePath != "." {
		t.Fatalf("默认服务目录是 %q，期望 \".\"", cfg.ServePath)
	}
}
