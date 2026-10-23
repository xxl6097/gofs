package archive

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"encoding/binary"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestCheckZipDirectoryRejectsRebasedEOCD 钉住预检对 baseOffset 回退的拒绝：
// EOCD 声明 1 条、64 字节目录，但真实记录在别处时不能再通过。
func TestCheckZipDirectoryRejectsRebasedEOCD(t *testing.T) {
	var buf bytes.Buffer
	// 两段 46 字节的“中央目录头”，让 Go 的 reader 有东西可读。
	for i := 0; i < 2; i++ {
		buf.Write([]byte{'P', 'K', 1, 2})
		buf.Write(make([]byte, 42))
	}
	// EOCD 放在末尾：entries=1, dirSize=46, dirOffset=0。
	eocd := make([]byte, 22)
	copy(eocd, []byte{'P', 'K', 5, 6})
	binary.LittleEndian.PutUint16(eocd[8:10], 1)
	binary.LittleEndian.PutUint16(eocd[10:12], 1)
	binary.LittleEndian.PutUint32(eocd[12:16], 46)
	binary.LittleEndian.PutUint32(eocd[16:20], 0)
	buf.Write(eocd)

	path := filepath.Join(t.TempDir(), "rebased.zip")
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := CheckZipDirectory(f, int64(buf.Len()), hardMaxZipEntries, hardMaxZipDirBytes); err == nil {
		t.Fatal("被重定位的中央目录没有被拒绝")
	}
}

// TestZipSizeOverflow 确认 zip64 的超大声明值不会变成负数绕过上限。
func TestZipSizeOverflow(t *testing.T) {
	f := &zip.File{FileHeader: zip.FileHeader{Name: "bomb.bin"}}
	f.UncompressedSize64 = 1 << 63
	e := zipEntry(f)
	if e.Size >= 0 {
		t.Fatalf("溢出大小应标记为未知，实际 %d", e.Size)
	}
	if zipSize(math.MaxInt64) != math.MaxInt64 {
		t.Fatal("普通大小不应被改变")
	}
}

// TestTarEntryClipsLongNames 确认列表不会保留 1 MiB 的 PAX 名字。
func TestTarEntryClipsLongNames(t *testing.T) {
	h := &tar.Header{
		Name:     strings.Repeat("a", 1<<20),
		Linkname: strings.Repeat("b", 1<<20),
		Mode:     0o644,
		Size:     0,
	}
	e := tarEntry(h)
	if len(e.Name) > maxEntryNameBytes+8 {
		t.Fatalf("条目名未截断：%d 字节", len(e.Name))
	}
	if len(e.LinkTarget) > maxEntryNameBytes+8 {
		t.Fatalf("链接名未截断：%d 字节", len(e.LinkTarget))
	}
}
