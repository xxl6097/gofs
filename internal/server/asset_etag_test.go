package server

import (
	"io"
	"testing"
	"testing/fstest"
	"time"
)

// etagOf 用一个全新的 Server 算 ETag（避开按名字的缓存）。
//
// modTime 传零值即模拟内嵌资源（embed.FS 的 ModTime 恒为零），
// 传真实时间即模拟 --assets 指向的磁盘目录。
func etagOf(t *testing.T, name, body string, modTime time.Time) string {
	t.Helper()
	fsys := fstest.MapFS{name: &fstest.MapFile{Data: []byte(body), ModTime: modTime}}
	f, err := fsys.Open(name)
	if err != nil {
		t.Fatalf("打开测试文件: %v", err)
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	rs, ok := f.(io.ReadSeeker)
	if !ok {
		t.Fatal("测试文件不是 ReadSeeker")
	}
	return (&Server{}).assetETag(name, st, rs)
}

// TestAssetETagFollowsContent 钉住这次修掉的坑：
// 内嵌资源内容变了但**字节数没变**时，ETag 也必须变。
//
// 用 modTime+size 拼时这里会拿到同一个 ETag，浏览器于是永远收 304、
// 继续跑旧 JS —— 表现就是「前端改了、也部署了，页面还是老样子」。
func TestAssetETagFollowsContent(t *testing.T) {
	a := "var x = 1;\n" // 11 字节
	b := "var y = 2;\n" // 同样 11 字节，内容不同
	if len(a) != len(b) {
		t.Fatalf("用例要求两份内容等长：%d vs %d", len(a), len(b))
	}

	ta := etagOf(t, "app.js", a, time.Time{})
	tb := etagOf(t, "app.js", b, time.Time{})
	if ta == tb {
		t.Errorf("等长但内容不同，ETag 却相同：%s —— 浏览器会一直用缓存的旧文件", ta)
	}

	// 内容一致必须稳定，否则每次请求都重新下载，缓存等于没有。
	if again := etagOf(t, "app.js", a, time.Time{}); again != ta {
		t.Errorf("同样内容的 ETag 不稳定：%s vs %s", ta, again)
	}

	// 长度不同也要变。
	if tc := etagOf(t, "app.js", a+"\n", time.Time{}); tc == ta {
		t.Errorf("内容变长，ETag 却没变：%s", ta)
	}
}

// TestAssetETagUsesModTimeForDiskAssets 磁盘资源（--assets）保持原来的
// modTime+size 行为：那边的 mtime 是真的，作者靠它做「改前端不用重启」的
// 热更新；改成内容散列会让每次请求都重读一遍文件。
func TestAssetETagUsesModTimeForDiskAssets(t *testing.T) {
	t1 := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	t2 := t1.Add(time.Second)

	// 内容一模一样、只有 mtime 不同 —— 磁盘资源必须给出不同的 ETag。
	if a, b := etagOf(t, "app.js", "same\n", t1), etagOf(t, "app.js", "same\n", t2); a == b {
		t.Errorf("磁盘资源的 ETag 没跟着 mtime 变：%s", a)
	}
	// 同一 mtime + 同一内容 → 稳定。
	if a, b := etagOf(t, "app.js", "same\n", t1), etagOf(t, "app.js", "same\n", t1); a != b {
		t.Errorf("磁盘资源的 ETag 不稳定：%s vs %s", a, b)
	}
}
