package gofs

import (
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// newListenTestServer 造一个不用启动、只用来调 listen() 的实例。
func newListenTestServer(t *testing.T) *Server {
	t.Helper()
	srv, err := New(
		WithRoot(t.TempDir()),
		WithKeyFile(""),
		WithUserFile(""),
		WithSettingsFile(""),
		WithLog("none", ""),
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = srv.Close() })
	return srv
}

// shortSocketPath 返回一个足够短的 unix socket 路径（macOS 上限约 104 字节）。
func shortSocketPath(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "gofs-listen-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return filepath.Join(dir, "s.sock")
}

// TestListenRefusesRegularFile 钉住：listen(addr) 只能清理 socket，
// 绝不能删除普通文件。修复前这里会先 os.Remove 再 Listen。
func TestListenRefusesRegularFile(t *testing.T) {
	dir := t.TempDir()
	victim := filepath.Join(dir, "do-not-delete.txt")
	if err := os.WriteFile(victim, []byte("important"), 0o644); err != nil {
		t.Fatal(err)
	}

	srv := newListenTestServer(t)
	if _, err := srv.listen(victim, 0); err == nil {
		t.Fatal("期望拒绝把普通文件当 socket 覆盖")
	}
	if _, err := os.Stat(victim); err != nil {
		t.Fatalf("普通文件被删除了：%v", err)
	}
}

// TestListenRefusesSymlink 软链同样不能被当作「残留 socket」删掉。
func TestListenRefusesSymlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target.txt")
	if err := os.WriteFile(target, []byte("important"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link.sock")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("当前环境不支持软链：%v", err)
	}

	srv := newListenTestServer(t)
	if _, err := srv.listen(link, 0); err == nil {
		t.Fatal("期望拒绝覆盖软链")
	}
	if _, err := os.Lstat(link); err != nil {
		t.Errorf("软链被删除了：%v", err)
	}
	if _, err := os.Stat(target); err != nil {
		t.Errorf("软链目标被删除了：%v", err)
	}
}

// TestListenRefusesLiveSocket 确认不会抢走另一个进程正在使用的 socket。
func TestListenRefusesLiveSocket(t *testing.T) {
	sock := shortSocketPath(t)
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	srv := newListenTestServer(t)
	if _, err := srv.listen(sock, 0); err == nil || !strings.Contains(err.Error(), "正在被其它进程使用") {
		t.Fatalf("期望拒绝仍在服务的 socket，实际 err = %v", err)
	}
	// 原监听必须还活着。
	c, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatalf("原 socket 被破坏了：%v", err)
	}
	_ = c.Close()
}

// TestListenReplacesStaleSocket 确认合法用法没被误伤：崩溃留下的 socket
// 文件（没有进程在听）仍能被清理并重新绑定。
func TestListenReplacesStaleSocket(t *testing.T) {
	sock := shortSocketPath(t)
	addr, err := net.ResolveUnixAddr("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.ListenUnix("unix", addr)
	if err != nil {
		t.Fatal(err)
	}
	ln.SetUnlinkOnClose(false)
	if err := ln.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(sock); err != nil {
		t.Skipf("平台已自动清理 socket 文件：%v", err)
	}

	srv := newListenTestServer(t)
	newLn, err := srv.listen(sock, 0)
	if err != nil {
		t.Fatalf("清理残留 socket 失败：%v", err)
	}
	_ = newLn.Close()
}
