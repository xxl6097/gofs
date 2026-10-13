package gofs_test

import (
	"context"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/xxl6097/gofs"
)

// freePort 拿一个当前空闲的端口号。
//
// 先 Listen 再关掉，「关掉」与「被测代码去 Listen」之间有被别的进程抢走的
// 理论窗口 —— 对这个用途足够了，测试里要的只是「差不多能用的具体端口」。
func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("取空闲端口: %v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()
	return port
}

// listenPort 返回服务器当前监听的端口号。
func listenPort(t *testing.T, srv *gofs.Server) int {
	t.Helper()
	addr := srv.Addr()
	if addr == nil {
		t.Fatal("服务没有在监听")
	}
	return addr.(*net.TCPAddr).Port
}

// TestRelistenMovesPort 是这次的核心：热重绑之后新端口能用、旧端口不再接受
// 新连接，而且**服务没有被当成「已结束」**。
//
// 最后一条是最容易写错、后果也最严重的地方：serve 返回时会关 Done，而
// RunCLI 的 `select { <-ctx.Done / <-srv.Done }` 一看 Done 关了就走 Wait()
// 返回 —— 进程直接退出。热重绑主动关掉旧监听，旧监听上的 Serve 于是返回
// net.ErrClosed（不是 ErrServerClosed），若不区分「被换掉的监听」与「服务
// 结束」，一次改端口就等于让服务自杀。
func TestRelistenMovesPort(t *testing.T) {
	srv, root := newServer(t)
	if err := os.WriteFile(filepath.Join(root, "hello.txt"), []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := srv.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	oldPort := listenPort(t, srv)

	newPort := freePort(t)
	addr, err := srv.Relisten(context.Background(), "127.0.0.1", newPort)
	if err != nil {
		t.Fatalf("Relisten: %v", err)
	}
	if want := net.JoinHostPort("127.0.0.1", itoa(newPort)); addr != want {
		t.Errorf("Relisten 返回 %q，期望 %q", addr, want)
	}

	// 新端口可用。
	if got := listenPort(t, srv); got != newPort {
		t.Fatalf("Addr 报告的端口 = %d，期望 %d", got, newPort)
	}
	client := &http.Client{Timeout: 3 * time.Second}
	code, body := get(t, client, "http://127.0.0.1:"+itoa(newPort)+"/hello.txt")
	if code != http.StatusOK || body != "hi" {
		t.Errorf("新端口返回 %d %q", code, body)
	}

	// 旧端口不再接受**新**连接。
	if _, err := dialRefused(oldPort); err == nil {
		t.Errorf("旧端口 %d 仍在接受连接", oldPort)
	}

	// 关键：服务没把自己当成「已结束」。
	select {
	case <-srv.Done:
		t.Fatal("热重绑把 Done 关掉了 —— RunCLI 会据此认为服务已停止并退出进程")
	case <-time.After(200 * time.Millisecond):
	}

	// 优雅关闭仍然正常收尾。
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	select {
	case <-srv.Done:
	case <-time.After(3 * time.Second):
		t.Fatal("Shutdown 之后 Done 没关闭")
	}
}

// TestRelistenFailureKeepsOldListener 确认失败时旧监听原封不动。
//
// 顺序反了（先关旧的再建新的）就会出现「新地址建不起来、旧地址也没了」，
// 而页面往往正是唯一能改回来的入口 —— 那就只能去 SSH 改文件了。
func TestRelistenFailureKeepsOldListener(t *testing.T) {
	srv, root := newServer(t)
	if err := os.WriteFile(filepath.Join(root, "hello.txt"), []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := srv.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	oldPort := listenPort(t, srv)

	// 占住一个端口，让重绑必然失败。
	blocker, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer blocker.Close()
	busyPort := blocker.Addr().(*net.TCPAddr).Port

	_, err = srv.Relisten(context.Background(), "127.0.0.1", busyPort)
	if err == nil {
		t.Fatal("绑定被占用的端口竟然成功了")
	}
	if !strings.Contains(err.Error(), "监听") {
		t.Errorf("错误文案没沿用根包的「监听 … 失败」前缀：%v", err)
	}

	// 旧监听照旧服务。
	if got := listenPort(t, srv); got != oldPort {
		t.Errorf("失败后监听端口变成了 %d，期望仍是 %d", got, oldPort)
	}
	client := &http.Client{Timeout: 3 * time.Second}
	if code, _ := get(t, client, "http://127.0.0.1:"+itoa(oldPort)+"/hello.txt"); code != http.StatusOK {
		t.Errorf("失败后旧端口返回 %d，期望 200", code)
	}
	select {
	case <-srv.Done:
		t.Fatal("失败的 Relisten 不该关闭 Done")
	case <-time.After(100 * time.Millisecond):
	}
}

// TestRelistenWithoutStart 确认没有自己的监听时明确报错（而不是假装成功）。
func TestRelistenWithoutStart(t *testing.T) {
	srv, _ := newServer(t)
	if srv.CanRelisten() {
		t.Error("没 Start 就宣称能重绑")
	}
	if _, err := srv.Relisten(context.Background(), "127.0.0.1", 12345); err == nil {
		t.Error("没 Start 时 Relisten 应当报错")
	}
}

// TestRelistenUnixSocketBothWays 覆盖 tcp 与 unix socket 互转。
//
// 两个方向都要干净：切走时要删掉旧 socket 文件（Go 关 unix listener 不会删，
// 留着会变成下次启动时的「残留 socket」），切进来时要真的建出文件。
func TestRelistenUnixSocketBothWays(t *testing.T) {
	srv, root := newServer(t)
	if err := os.WriteFile(filepath.Join(root, "hello.txt"), []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := srv.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}

	sock := filepath.Join(t.TempDir(), "gofs.sock")
	if _, err := srv.Relisten(context.Background(), sock, 0); err != nil {
		t.Fatalf("切到 unix socket 失败: %v", err)
	}
	st, err := os.Stat(sock)
	if err != nil {
		t.Fatalf("socket 文件没建出来: %v", err)
	}
	if st.Mode().Perm() != 0o660 {
		t.Errorf("socket 权限 = %v，期望 0660", st.Mode().Perm())
	}
	if url := srv.URL(); !strings.HasPrefix(url, "http+unix://") {
		t.Errorf("URL = %q，期望 http+unix:// 前缀", url)
	}
	if got := getViaUnix(t, sock, "/hello.txt"); got != http.StatusOK {
		t.Errorf("unix socket 上访问返回 %d", got)
	}

	// 切回 TCP：socket 文件要清掉。
	newPort := freePort(t)
	if _, err := srv.Relisten(context.Background(), "127.0.0.1", newPort); err != nil {
		t.Fatalf("切回 tcp 失败: %v", err)
	}
	if _, err := os.Stat(sock); !os.IsNotExist(err) {
		t.Errorf("切走后 socket 文件还在（stat err = %v）", err)
	}
	if url := srv.URL(); strings.HasPrefix(url, "http+unix://") {
		t.Errorf("URL = %q，切回 tcp 后不该还是 unix", url)
	}
	client := &http.Client{Timeout: 3 * time.Second}
	if code, _ := get(t, client, "http://127.0.0.1:"+itoa(newPort)+"/hello.txt"); code != http.StatusOK {
		t.Errorf("切回 tcp 后访问返回 %d", code)
	}
}

// TestBindSetPortSetFromOptions 确认三个 Option 都置上了「显式指定」标记 ——
// 落盘规则靠它决定重启后要不要采纳文件里的值（恢复入口）。
func TestBindSetPortSetFromOptions(t *testing.T) {
	srv, _ := newServer(t, gofs.WithBind("127.0.0.1"), gofs.WithPort(0))
	cfg := srv.Config()
	if !cfg.BindSet || !cfg.PortSet {
		t.Errorf("WithBind/WithPort 没置上标记：BindSet=%v PortSet=%v", cfg.BindSet, cfg.PortSet)
	}

	srv2, _ := newServer(t) // 只有 newServer 默认的 WithPort(0)
	if srv2.Config().PortSet != true {
		t.Error("newServer 里的 WithPort(0) 应当置上 PortSet")
	}
}

// ---- 小工具 ----

func itoa(n int) string { return strconv.Itoa(n) }

// dialRefused 尝试连接端口，返回错误。
func dialRefused(port int) (net.Conn, error) {
	return net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), time.Second)
}

// getViaUnix 通过 unix socket 发一个 GET，返回状态码。
func getViaUnix(t *testing.T, sock, path string) int {
	t.Helper()
	client := &http.Client{
		Timeout: 3 * time.Second,
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, "unix", sock)
			},
		},
	}
	resp, err := client.Get("http://gofs" + path)
	if err != nil {
		t.Fatalf("unix GET: %v", err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode
}
