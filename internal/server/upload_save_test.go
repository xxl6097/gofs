package server

import (
	"bytes"
	"crypto/sha256"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"testing"
)

// TestSavePartRenamesTempFile 覆盖表单上传大文件的快路径。
//
// multipart 对超过 maxFormMemory 的字段已经写过一次临时文件了；savePart 应当
// 直接把它 rename 到目标，而不是再整份拷贝一遍（那是 1GB 慢 4.5 倍的来源）。
func TestSavePartRenamesTempFile(t *testing.T) {
	dir := t.TempDir()
	tmp, err := os.CreateTemp(dir, "part-*")
	if err != nil {
		t.Fatal(err)
	}
	payload := []byte("hello rename path")
	if _, err := tmp.Write(payload); err != nil {
		t.Fatal(err)
	}
	tmpName := tmp.Name()
	if err := tmp.Close(); err != nil {
		t.Fatal(err)
	}

	src, err := os.Open(tmpName)
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()

	dst := filepath.Join(dir, "final.bin")
	if err := savePart(dst, src); err != nil {
		t.Fatalf("savePart: %v", err)
	}

	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, payload) {
		t.Errorf("内容 = %q, 期望 %q", got, payload)
	}
	// rename 不改权限，savePart 必须补成普通上传文件的 0644。
	st, err := os.Stat(dst)
	if err != nil {
		t.Fatal(err)
	}
	if perm := st.Mode().Perm(); perm != 0o644 {
		t.Errorf("权限 = %o, 期望 644", perm)
	}
	// 临时文件应当已被移走（rename 而非拷贝）。
	if _, err := os.Stat(tmpName); !os.IsNotExist(err) {
		t.Errorf("临时文件 %s 仍存在，说明走的是拷贝而非 rename", tmpName)
	}
}

// TestSavePartCopiesGenericReader 覆盖回退分支：内容不是 *os.File 时按字节拷贝。
// （小字段在 multipart 里是内存 section，正是这种情况。）
func TestSavePartCopiesGenericReader(t *testing.T) {
	dir := t.TempDir()
	payload := []byte("in-memory section")
	dst := filepath.Join(dir, "copied.bin")

	if err := savePart(dst, bytes.NewReader(payload)); err != nil {
		t.Fatalf("savePart: %v", err)
	}
	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, payload) {
		t.Errorf("内容 = %q, 期望 %q", got, payload)
	}
}

// TestUploadFormLargeFileIsIntact 端到端：字段大于 maxFormMemory 时会落临时文件，
// 走 rename 快路径 —— 内容必须与上传的逐字节一致。
func TestUploadFormLargeFileIsIntact(t *testing.T) {
	if testing.Short() {
		t.Skip("需要写一个 >32MiB 的表单体")
	}
	root := makeFixture(t)
	s := newTestServer(t, testConfig(t, root, "-A"))

	// 略大于 32MiB，确保 multipart 把它落到临时文件（而不是留在内存里），
	// 否则测不到 rename 那条路径。
	size := maxFormMemory + (1 << 20)
	payload := bytes.Repeat([]byte("abcdefgh"), size/8)
	want := sha256.Sum256(payload)

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, err := mw.CreateFormFile("file", "bigform.bin")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fw.Write(payload); err != nil {
		t.Fatal(err)
	}
	if err := mw.Close(); err != nil {
		t.Fatal(err)
	}

	r := do(t, s, http.MethodPost, "/", &buf,
		map[string]string{"Content-Type": mw.FormDataContentType()})
	if r.code != http.StatusOK {
		t.Fatalf("状态码 = %d；%s", r.code, r.body)
	}

	f, err := os.Open(filepath.Join(root, "bigform.bin"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		t.Fatal(err)
	}
	if got := h.Sum(nil); !bytes.Equal(got, want[:]) {
		t.Errorf("落盘内容校验和不符（长度可能被截断）")
	}
}
