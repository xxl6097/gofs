package archive

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// TestListAndOpenEntryLimits 确认列表和单条目预览也受 ExtractMax* 约束，
// 而不是只有真正解压时才限额。
func TestListAndOpenEntryLimits(t *testing.T) {
	dir := t.TempDir()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, name := range []string{"a.txt", "b.txt"} {
		data := []byte("0123456789")
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(data))}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(data); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "x.tar")
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}

	rd, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer rd.Close()
	ls, ok := rd.(LimitSetter)
	if !ok {
		t.Fatal("tar Reader 未实现 LimitSetter")
	}

	ls.SetLimits(Limits{MaxFiles: 1})
	entries, err := rd.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("List 返回 %d 项，期望受 MaxFiles=1 限制", len(entries))
	}

	ls.SetLimits(Limits{MaxEntryBytes: 4})
	if _, _, err := rd.OpenEntry(context.Background(), "a.txt"); !errors.Is(err, ErrEntryTooLarge) {
		t.Fatalf("OpenEntry 错误 = %v，期望 ErrEntryTooLarge", err)
	}
}
