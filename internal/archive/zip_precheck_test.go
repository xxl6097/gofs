package archive

import (
	"archive/zip"
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
)

// TestCheckZipDirectoryLimits 确认 zip.NewReader 之前的有界预检能挡住
// 「大条目数 / 大中央目录 / Zip64 声明」这几类内存放大。
func TestCheckZipDirectoryLimits(t *testing.T) {
	dir := t.TempDir()

	// 一个正常的小 zip 必须通过。
	okPath := filepath.Join(dir, "ok.zip")
	f, err := os.Create(okPath)
	if err != nil {
		t.Fatal(err)
	}
	zw := zip.NewWriter(f)
	w, err := zw.Create("a.txt")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = w.Write([]byte("hello"))
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	rf, err := os.Open(okPath)
	if err != nil {
		t.Fatal(err)
	}
	st, _ := rf.Stat()
	if err := CheckZipDirectory(rf, st.Size(), 100, 1<<20); err != nil {
		t.Fatalf("正常 zip 被拒绝：%v", err)
	}
	rf.Close()

	eocd := func(entries uint16, dirSize uint32) []byte {
		b := make([]byte, 22)
		copy(b, []byte{'P', 'K', 5, 6})
		binary.LittleEndian.PutUint16(b[10:12], entries)
		binary.LittleEndian.PutUint32(b[12:16], dirSize)
		return b
	}
	cases := []struct {
		name string
		data []byte
	}{
		{"条目数超限", eocd(101, 64)},
		{"Zip64 条目数", eocd(0xFFFF, 64)},
		{"中央目录超限", eocd(1, 2<<20)},
		{"Zip64 中央目录", eocd(1, 0xFFFFFFFF)},
	}
	for _, c := range cases {
		p := filepath.Join(dir, c.name+".zip")
		if err := os.WriteFile(p, c.data, 0o600); err != nil {
			t.Fatal(err)
		}
		f, err := os.Open(p)
		if err != nil {
			t.Fatal(err)
		}
		if err := CheckZipDirectory(f, int64(len(c.data)), 100, 1<<20); err == nil {
			t.Errorf("%s：预检没有拒绝", c.name)
		}
		f.Close()
	}
}
