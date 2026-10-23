package server

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type tarEntry struct {
	name  string
	body  string
	link  string
	mode  byte
	typef byte
}

func writeTarGz(t *testing.T, path string, entries []tarEntry) {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, e := range entries {
		h := &tar.Header{Name: e.name, Mode: 0o644, Typeflag: e.typef}
		if e.typef == 0 {
			h.Typeflag = tar.TypeReg
		}
		if e.typef == tar.TypeSymlink {
			h.Linkname = e.link
		} else {
			h.Size = int64(len(e.body))
		}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if h.Size > 0 {
			if _, err := tw.Write([]byte(e.body)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestExtractRejectsTruncatedListBeforeWriting 回归 MaxFiles 填充绕过。
//
// 旧实现把 List 截断后的前 N 条做 ACL 预检，然后对整包解压。用 N 个
// 会被跳过、且不计数次数的 symlink 填充，就能让后面的 regular 文件
// 完全不经过 ACL 检查。现在 List 截断即拒绝，不会再写。
func TestExtractRejectsTruncatedListBeforeWriting(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "docs", "secret.txt"), []byte("ORIGINAL"))
	entries := make([]tarEntry, 0, 6)
	for i := 0; i < 5; i++ {
		entries = append(entries, tarEntry{
			name: "p", link: "/tmp/nowhere", typef: tar.TypeSymlink,
		})
	}
	entries = append(entries, tarEntry{name: "secret.txt", body: "PWNED"})
	writeTarGz(t, filepath.Join(root, "docs", "evil.tar.gz"), entries)

	s := newTestServer(t, testConfig(t, root, "-A",
		"--extract-max-files", "5", "--auth-fail-limit", "0",
		"-a", "bob:pw@/docs:rw,/docs/secret.txt:r"))

	got := do(t, s, http.MethodPut, "/fs/extract",
		strings.NewReader(`{"path":"/docs/evil.tar.gz","dest":"/docs","overwrite":true}`),
		map[string]string{
			"Content-Type":  "application/json",
			"Authorization": basicAuth("bob", "pw"),
		})
	if got.code != http.StatusRequestEntityTooLarge {
		t.Fatalf("截断归档返回 %d，期望 413：%s", got.code, got.body)
	}
	data, err := os.ReadFile(filepath.Join(root, "docs", "secret.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "ORIGINAL" {
		t.Fatalf("只读文件被覆盖：%q", data)
	}
}

// TestExtractStillWorksUnderLimit 确认正常小归档不受 fail-closed 影响。
func TestExtractStillWorksUnderLimit(t *testing.T) {
	root := t.TempDir()
	writeTarGz(t, filepath.Join(root, "docs", "ok.tar.gz"), []tarEntry{
		{name: "a.txt", body: "A"},
		{name: "sub/b.txt", body: "B"},
	})
	s := newTestServer(t, testConfig(t, root, "-A",
		"-a", "bob:pw@/docs:rw"))

	got := do(t, s, http.MethodPut, "/fs/extract",
		strings.NewReader(`{"path":"/docs/ok.tar.gz","dest":"/docs/out"}`),
		map[string]string{
			"Content-Type":  "application/json",
			"Authorization": basicAuth("bob", "pw"),
		})
	if got.code != http.StatusOK {
		t.Fatalf("正常解压返回 %d：%s", got.code, got.body)
	}
	if _, err := os.Stat(filepath.Join(root, "docs", "out", "sub", "b.txt")); err != nil {
		t.Fatalf("解压结果缺失：%v", err)
	}
}
