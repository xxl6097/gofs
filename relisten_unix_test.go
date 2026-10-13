package gofs_test

import (
	"context"
	"net"
	"os"
	"testing"

	"github.com/xxl6097/gofs"
)

// TestRelistenSameUnixSocketPathKeepsServing 是安全/可用性回归：
// 重绑到**同一个** unix socket 路径时，新的 socket 文件不能被删掉。
//
// Go 的 UnixListener.Close() 会把路径 unlink 掉。Relisten 的顺序是
// 「先建新监听、再关旧的」，而那时路径已经属于新监听 —— 旧监听一关，
// 新监听的 socket 文件就没了，之后所有新连接 ENOENT，只有重启能救。
//
// 触发它并不需要什么恶意操作：页面上「只改端口」就会走到这里
// （unix socket 下端口是无效字段，但界面允许改）。
func TestRelistenSameUnixSocketPathKeepsServing(t *testing.T) {
	sock := "/tmp/gofs-relisten-test.sock" // 必须短：unix 路径有 ~104 字节上限
	_ = os.Remove(sock)
	t.Cleanup(func() { _ = os.Remove(sock) })

	srv, err := gofs.New(gofs.WithRoot(t.TempDir()), gofs.WithUnixSocket(sock),
		gofs.WithKeyFile(""), gofs.WithUserFile(""), gofs.WithSettingsFile(""))
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}

	// 只改端口（对 unix socket 无意义）。
	if _, err := srv.Relisten(context.Background(), sock, 6500); err != nil {
		t.Fatalf("Relisten: %v", err)
	}
	if _, err := os.Stat(sock); err != nil {
		t.Fatalf("重绑后 socket 文件没了，新连接会 ENOENT: %v", err)
	}
	c, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatalf("重绑后连不上: %v", err)
	}
	_ = c.Close()
}
