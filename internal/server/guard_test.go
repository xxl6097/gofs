package server

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// writeSizeRecorder 记录每次 Write 的字节数，用来验证搬运时的分块大小。
type writeSizeRecorder struct {
	sizes []int
	buf   bytes.Buffer
}

func (r *writeSizeRecorder) Write(p []byte) (int, error) {
	r.sizes = append(r.sizes, len(p))
	return r.buf.Write(p)
}

// TestCopyUploadUsesLargeChunks 钉住上传搬运的分块大小。
//
// io.Copy 默认一次只搬 32KB，一个 1GiB 的文件就是三万余次 write(2)。
// 在页缓存能吸收一切的机器上这不痛不痒，但在「每次写都有固定开销」的存储上
// （限速云盘 / 网络卷 / 日志落在慢盘的文件系统）会把上传死死压住 ——
// 这正是 uploadCopyBuf 存在的理由（见 guard.go 的说明）。
//
// 这个用例同时防住那个坑：*os.File 实现了 io.ReaderFrom，io.Copy 会改走
// f.ReadFrom(r)，缓冲区被**无声忽略**。所以 copyUpload 必须把目标包一层
// onlyWriter；哪天有人「顺手」把这层去掉又不写测试，这里就会红。
func TestCopyUploadUsesLargeChunks(t *testing.T) {
	const total = 5 << 20
	dst := &writeSizeRecorder{}
	n, err := copyUpload(dst, bytes.NewReader(make([]byte, total)))
	if err != nil {
		t.Fatalf("copyUpload: %v", err)
	}
	if n != total || dst.buf.Len() != total {
		t.Fatalf("搬运了 %d 字节，写进 %d，期望 %d", n, dst.buf.Len(), total)
	}
	if len(dst.sizes) == 0 {
		t.Fatal("一次都没写")
	}
	// 32KB 默认下这里会是 160 次；大缓冲下应当是个位数。
	for i, s := range dst.sizes {
		if s < 256<<10 {
			t.Fatalf("第 %d 次只写了 %d 字节（接近默认的 32KB），说明缓冲区没生效", i, s)
		}
	}
	if len(dst.sizes) > 8 {
		t.Fatalf("写了 %d 次，分块太小（%v）", len(dst.sizes), dst.sizes)
	}
}

// TestCopyUploadToRealFile 覆盖真实落盘：*os.File 会命中 io.ReaderFrom 那条路，
// 上面那个用例用的是自定义 Writer，盖不到它。
func TestCopyUploadToRealFile(t *testing.T) {
	payload := bytes.Repeat([]byte("gofs"), 200_000) // 800KB，非整数倍块长
	p := filepath.Join(t.TempDir(), "out.bin")
	f, err := os.OpenFile(p, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	n, err := copyUpload(f, bytes.NewReader(payload))
	if err != nil {
		t.Fatalf("copyUpload: %v", err)
	}
	f.Close()
	if n != int64(len(payload)) {
		t.Fatalf("搬运 %d 字节，期望 %d", n, len(payload))
	}
	got, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("落盘内容不一致：得到 %d 字节，期望 %d", len(got), len(payload))
	}
}
