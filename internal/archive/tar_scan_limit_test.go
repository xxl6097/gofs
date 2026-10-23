package archive

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
)

type zeroReader struct{}

func (zeroReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 0
	}
	return len(p), nil
}

// makeScanBomb 造一个「前面一个巨大条目、后面一个正常条目」的 tar.gz。
//
// tar 是流式的，要找到 after.txt 必须先把 big.bin 的内容整段跳过 ——
// 也就是完整解压它。没有扫描预算时，一个几十 KB 的包就能让服务端
// 解压几百 MB 甚至更多。
func makeScanBomb(t *testing.T) string {
	t.Helper()
	const bigSize = 8 << 20

	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)

	if err := tw.WriteHeader(&tar.Header{Name: "big.bin", Mode: 0o644, Size: bigSize}); err != nil {
		t.Fatal(err)
	}
	if _, err := io.CopyN(tw, zeroReader{}, bigSize); err != nil {
		t.Fatal(err)
	}
	if err := tw.WriteHeader(&tar.Header{Name: "after.txt", Mode: 0o644, Size: 2}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write([]byte("ok")); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}

	path := filepath.Join(t.TempDir(), "bomb.tar.gz")
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestTarScanBudgetStopsGzipBomb 确认 List / OpenEntry 的扫描量受
// MaxTotalBytes 约束，而不是只受 MaxFiles 约束。
func TestTarScanBudgetStopsGzipBomb(t *testing.T) {
	rd, err := Open(makeScanBomb(t))
	if err != nil {
		t.Fatal(err)
	}
	defer rd.Close()

	ls, ok := rd.(LimitSetter)
	if !ok {
		t.Fatal("tar Reader 未实现 LimitSetter")
	}

	// 预算足够：正常列出两个条目。
	ls.SetLimits(Limits{MaxTotalBytes: 32 << 20, MaxFiles: 10})
	entries, err := rd.List(context.Background())
	if err != nil {
		t.Fatalf("预算足够时 List 失败: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("List 返回 %d 项，期望 2", len(entries))
	}

	// 预算不足：列表必须中止，而不是把 big.bin 解压完。
	ls.SetLimits(Limits{MaxTotalBytes: 32 << 20, MaxScanBytes: 64 << 10, MaxFiles: 10})
	if _, err := rd.List(context.Background()); !errors.Is(err, ErrScanTooLarge) {
		t.Fatalf("List 错误 = %v，期望 ErrScanTooLarge", err)
	}

	// 定位 big.bin 之后的条目同样要扫描 big.bin，必须被预算挡住。
	if _, _, err := rd.OpenEntry(context.Background(), "after.txt"); !errors.Is(err, ErrScanTooLarge) {
		t.Fatalf("OpenEntry 错误 = %v，期望 ErrScanTooLarge", err)
	}
}
