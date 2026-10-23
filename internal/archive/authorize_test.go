package archive

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func writeZipFixture(t *testing.T, path string, files map[string]string) {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, body := range files {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
}

func writeTarFixture(t *testing.T, path string, files map[string]string) {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for name, body := range files {
		if err := tw.WriteHeader(&tar.Header{
			Name: name, Mode: 0o644, Size: int64(len(body)),
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestExtractAuthorizeRejectsEntry 确认目标文件在落盘前会被逐条目拒绝。
func TestExtractAuthorizeRejectsEntry(t *testing.T) {
	for _, format := range []string{"zip", "tar"} {
		t.Run(format, func(t *testing.T) {
			dir := t.TempDir()
			src := filepath.Join(dir, "x."+format)
			if format == "zip" {
				writeZipFixture(t, src, map[string]string{"ok.txt": "ok", "blocked.txt": "bad"})
			} else {
				writeTarFixture(t, src, map[string]string{"ok.txt": "ok", "blocked.txt": "bad"})
			}
			rd, err := Open(src)
			if err != nil {
				t.Fatal(err)
			}
			defer rd.Close()

			dest := filepath.Join(dir, "out")
			denied := errors.New("denied")
			_, err = rd.Extract(context.Background(), Options{
				Dest: dest,
				Authorize: func(rel string) error {
					if rel == "blocked.txt" {
						return denied
					}
					return nil
				},
			}, nil)
			if !errors.Is(err, denied) {
				t.Fatalf("Extract 错误 = %v，期望 denied", err)
			}
			if _, err := os.Stat(filepath.Join(dest, "blocked.txt")); err == nil {
				t.Fatal("被拒绝的文件仍然落盘")
			}
		})
	}
}

// TestListTruncationFlag 确认 List 能区分“恰好到上限”和“还有更多”。
func TestListTruncationFlag(t *testing.T) {
	type truncatedReporter interface{ Truncated() bool }

	for _, format := range []string{"zip", "tar"} {
		t.Run(format, func(t *testing.T) {
			dir := t.TempDir()
			src := filepath.Join(dir, "x."+format)
			files := map[string]string{"a.txt": "a", "b.txt": "b", "c.txt": "c"}
			if format == "zip" {
				writeZipFixture(t, src, files)
			} else {
				writeTarFixture(t, src, files)
			}
			rd, err := Open(src)
			if err != nil {
				t.Fatal(err)
			}
			defer rd.Close()
			ls, ok := rd.(LimitSetter)
			if !ok {
				t.Fatal("Reader 未实现 LimitSetter")
			}
			tr, ok := rd.(truncatedReporter)
			if !ok {
				t.Fatal("Reader 未实现 Truncated")
			}

			ls.SetLimits(Limits{MaxFiles: 2})
			entries, err := rd.List(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if len(entries) != 2 || !tr.Truncated() {
				t.Fatalf("MaxFiles=2：len=%d truncated=%v，期望 2/true", len(entries), tr.Truncated())
			}

			ls.SetLimits(Limits{MaxFiles: 3})
			entries, err = rd.List(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if len(entries) != 3 || tr.Truncated() {
				t.Fatalf("MaxFiles=3：len=%d truncated=%v，期望 3/false", len(entries), tr.Truncated())
			}
		})
	}
}
