package fsutil

import (
	"os"
	"path/filepath"
	"testing"
)

// TestIsWithin 覆盖路径包含判断的边界。
//
// 重点是 root 为文件系统根 "/" 的情形：它是「可切换范围」的常见取值
// （启动根在一级目录下时，推导出的父目录就是 "/"），
// 一旦判错，白名单会从「放行整棵树」变成「谁都不放行」。
func TestIsWithin(t *testing.T) {
	if filepath.Separator != '/' {
		t.Skip("本用例按 POSIX 路径语义编写；Windows 卷根语义不同，不在覆盖范围")
	}

	cases := []struct {
		base string
		p    string
		want bool
	}{
		// 根："/" 应当放行其下的一切（含自身）。
		{"/", "/", true},
		{"/", "/data", true},
		{"/", "/Users/x/y", true},

		// 普通目录：自身与后代放行，同级或父级不放行。
		{"/srv/data", "/srv/data", true},
		{"/srv/data", "/srv/data/sub", true},
		{"/srv/data", "/srv", false},
		{"/srv/data", "/srv/datax", false},
		{"/srv", "/srv", true},
		{"/srv", "/srv/x", true},

		// 前缀相同但并非同一棵树：不能被字符串前缀骗过。
		{"/srv", "/srvx", false},
		{"/srv", "/srvx/y", false},

		// 空串一律不放行。
		{"", "/x", false},
		{"/x", "", false},
	}

	for _, c := range cases {
		if got := IsWithin(c.base, c.p); got != c.want {
			t.Errorf("IsWithin(%q, %q) = %v, want %v", c.base, c.p, got, c.want)
		}
	}
}

// TestResolveBlocksSymlinkEscapeForNewPaths 是这次修的核心：
// 软链逃逸检查必须覆盖**还不存在**的路径。
//
// 叶子往往是新建的（PUT 新文件 / MKCOL 新目录 / MOVE 的目标），
// 而 EvalSymlinks 对不存在的路径直接返回 ENOENT —— 早先只在
// `err == nil` 时检查，于是整段检查被跳过，随后的 os.OpenFile 会跟着
// 父目录里的软链把内容写到服务根之外。
func TestResolveBlocksSymlinkEscapeForNewPaths(t *testing.T) {
	outside := t.TempDir() // 根**之外**的目录
	root := t.TempDir()
	// 在服务根里放一条指向外面的软链（镜像常见的「预置软链」场景）。
	if err := os.Symlink(outside, filepath.Join(root, "link")); err != nil {
		t.Skipf("本平台不支持创建软链: %v", err)
	}

	r, err := NewResolver(root, false) // allowSymlink = false
	if err != nil {
		t.Fatal(err)
	}

	for _, p := range []string{
		"/link/newfile.txt", // PUT 一个新文件
		"/link/newdir",      // MKCOL 一个新目录
		"/link/sub/deep/x",  // 多级新路径
	} {
		if got, err := r.Resolve(p); err == nil {
			t.Errorf("Resolve(%q) 放行了，返回 %q —— 应判为逃逸", p, got)
		}
	}

	// 对照：根之内新建路径必须照常放行（别矫枉过正）。
	if _, err := r.Resolve("/brandnew.txt"); err != nil {
		t.Errorf("根内的新建路径被误拒: %v", err)
	}

	// 对照：开启 allowSymlink 时按文档放行。
	ra, err := NewResolver(root, true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ra.Resolve("/link/newfile.txt"); err != nil {
		t.Errorf("allowSymlink=true 却被拒: %v", err)
	}
}

// TestEnsureWithinAllowsInRootSymlinkDir 确认这道检查不会误伤合法写入。
//
// 目标目录本身可以是一条指向**根内**别处的软链（解压目标常见）：
// 只解析 target 而不解析 root 的话，一边是真实路径、一边还是软链形式，
// 前缀比对必然失败，合法写入会被当成逃逸。
func TestEnsureWithinAllowsInRootSymlinkDir(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "real"), 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "link")
	if err := os.Symlink(filepath.Join(root, "real"), link); err != nil {
		t.Skipf("本平台不支持软链: %v", err)
	}
	r, err := NewResolver(root, false)
	if err != nil {
		t.Fatal(err)
	}
	// 写到「软链目录」下的新文件：解析后仍在根内，应当放行。
	if _, err := r.Resolve("/link/new.txt"); err != nil {
		t.Errorf("根内的软链目录被误拒: %v", err)
	}
	// 解压侧用的是同一个函数，这里直接调一次确认。
	if err := EnsureWithin(link, filepath.Join(link, "x.txt")); err != nil {
		t.Errorf("EnsureWithin 对根内软链目录误报逃逸: %v", err)
	}
}

// TestEnsureWithinRejectsDanglingSymlink 钉住软链写逃逸：
//
// 叶子是悬空软链时，EvalSymlinks 会失败；旧实现只解析「已存在的祖先」，
// 会把叶子原样拼回去并判定为在根内，随后的 OpenFile 却会跟随软链
// 在根外创建文件。
func TestEnsureWithinRejectsDanglingSymlink(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "root")
	outside := filepath.Join(base, "outside")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, "pwned.txt"), filepath.Join(root, "upload.txt")); err != nil {
		t.Skipf("本平台不支持软链: %v", err)
	}
	if _, err := NewResolver(root, false); err != nil {
		t.Fatal(err)
	}
	if err := EnsureWithin(root, filepath.Join(root, "upload.txt")); err == nil {
		t.Fatal("悬空软链被判定为根内路径，写入会逃逸到根外")
	}
}
