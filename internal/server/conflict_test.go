package server

import (
	"archive/tar"
	"bytes"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestBackupFileName 验证时间戳后缀的位置与边界。
func TestBackupFileName(t *testing.T) {
	const ts = "20261027-153045"
	cases := []struct {
		name  string
		isDir bool
		n     int
		max   int
		want  string
	}{
		{"report.pdf", false, 1, 0, "report-20261027-153045.pdf"},
		{"README", false, 1, 0, "README-20261027-153045"},
		{".gitignore", false, 1, 0, ".gitignore-20261027-153045"},
		{"docs", true, 1, 0, "docs-20261027-153045"},
		{"a.txt", false, 2, 0, "a-20261027-153045-2.txt"},
		// 超过上限时截断主名，但后缀与扩展名完整保留。
		{"abcdefghij.txt", false, 1, 25, "abcde-20261027-153045.txt"},
	}
	for _, c := range cases {
		if got := backupFileName(c.name, c.isDir, ts, c.n, c.max); got != c.want {
			t.Errorf("backupFileName(%q,%v,%d,%d) = %q, 期望 %q",
				c.name, c.isDir, c.n, c.max, got, c.want)
		}
	}
}

// TestStatQueryBypassesSPA 验证重名探测不会被 SPA 回退伪装成 200。
func TestStatQueryBypassesSPA(t *testing.T) {
	root := makeFixture(t)
	s := newTestServer(t, testConfig(t, root, "-A", "--render-spa"))

	if r := do(t, s, http.MethodHead, "/missing.txt?stat=1", nil, nil); r.code != http.StatusNotFound {
		t.Errorf("不存在的路径返回 %d, 期望 404（SPA 回退不应生效）", r.code)
	}
	if r := do(t, s, http.MethodHead, "/notes.txt?stat=1", nil, nil); r.code != http.StatusOK {
		t.Errorf("存在的路径返回 %d, 期望 200", r.code)
	}
}

// TestUploadReplaceConflict 验证 conflict=replace 先删除原文件再写入新内容。
func TestUploadReplaceConflict(t *testing.T) {
	root := makeFixture(t)
	s := newTestServer(t, testConfig(t, root, "-A"))

	r := do(t, s, http.MethodPut, "/notes.txt?conflict=replace",
		strings.NewReader("new content"), nil)
	if r.code != http.StatusCreated {
		t.Fatalf("状态码 = %d, 期望 201；%s", r.code, r.body)
	}
	got, err := os.ReadFile(filepath.Join(root, "notes.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "new content" {
		t.Errorf("内容 = %q, 期望 %q", got, "new content")
	}
	// 替换不应该留下备份文件。
	if matches, _ := filepath.Glob(filepath.Join(root, "notes-*.txt")); len(matches) != 0 {
		t.Errorf("replace 不应产生备份：%v", matches)
	}
}

// TestUploadBackupConflict 验证 conflict=backup 把原文件改名后再写入。
func TestUploadBackupConflict(t *testing.T) {
	root := makeFixture(t)
	s := newTestServer(t, testConfig(t, root, "-A"))

	r := do(t, s, http.MethodPut, "/notes.txt?conflict=backup",
		strings.NewReader("new content"), nil)
	if r.code != http.StatusCreated {
		t.Fatalf("状态码 = %d, 期望 201；%s", r.code, r.body)
	}
	got, err := os.ReadFile(filepath.Join(root, "notes.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "new content" {
		t.Errorf("新文件内容 = %q, 期望 %q", got, "new content")
	}
	matches, _ := filepath.Glob(filepath.Join(root, "notes-*.txt"))
	if len(matches) != 1 {
		t.Fatalf("备份文件数量 = %d, 期望 1：%v", len(matches), matches)
	}
	old, err := os.ReadFile(matches[0])
	if err != nil {
		t.Fatal(err)
	}
	if string(old) != "top level\n" {
		t.Errorf("备份内容 = %q, 期望原内容", old)
	}
}

// TestUploadReplaceDirectoryConflict 验证同名目录也能被替换（递归删除后写成文件）。
func TestUploadReplaceDirectoryConflict(t *testing.T) {
	root := makeFixture(t)
	s := newTestServer(t, testConfig(t, root, "-A"))

	r := do(t, s, http.MethodPut, "/docs?conflict=replace",
		strings.NewReader("now a file"), nil)
	if r.code != http.StatusCreated {
		t.Fatalf("状态码 = %d, 期望 201；%s", r.code, r.body)
	}
	st, err := os.Stat(filepath.Join(root, "docs"))
	if err != nil {
		t.Fatal(err)
	}
	if st.IsDir() {
		t.Fatal("替换后 docs 仍应是文件，不是目录")
	}
	got, _ := os.ReadFile(filepath.Join(root, "docs"))
	if string(got) != "now a file" {
		t.Errorf("内容 = %q", got)
	}
}

// TestUploadBackupDirectoryConflict 验证同名目录会整体改名保留。
func TestUploadBackupDirectoryConflict(t *testing.T) {
	root := makeFixture(t)
	s := newTestServer(t, testConfig(t, root, "-A"))

	r := do(t, s, http.MethodPut, "/docs?conflict=backup",
		strings.NewReader("now a file"), nil)
	if r.code != http.StatusCreated {
		t.Fatalf("状态码 = %d, 期望 201；%s", r.code, r.body)
	}
	matches, _ := filepath.Glob(filepath.Join(root, "docs-*"))
	if len(matches) != 1 {
		t.Fatalf("目录备份数量 = %d, 期望 1：%v", len(matches), matches)
	}
	if _, err := os.Stat(filepath.Join(matches[0], "readme.md")); err != nil {
		t.Errorf("备份目录里的原文件丢失: %v", err)
	}
	if st, err := os.Stat(filepath.Join(root, "docs")); err != nil || st.IsDir() {
		t.Fatalf("原路径应变成普通文件: st=%v err=%v", st, err)
	}
}

// TestUploadConflictIgnoredOnAppend 验证断点续传不会被 conflict 参数清空。
func TestUploadConflictIgnoredOnAppend(t *testing.T) {
	root := makeFixture(t)
	s := newTestServer(t, testConfig(t, root, "-A"))

	r := do(t, s, http.MethodPut, "/notes.txt?conflict=replace",
		strings.NewReader("tail"), map[string]string{"X-Update-Range": "append"})
	if r.code != http.StatusCreated {
		t.Fatalf("状态码 = %d, 期望 201；%s", r.code, r.body)
	}
	got, _ := os.ReadFile(filepath.Join(root, "notes.txt"))
	if string(got) != "top level\ntail" {
		t.Errorf("续传后内容 = %q，说明原文件被替换或截断了", got)
	}
}

// TestUploadFormBackupConflict 验证 multipart 表单上传同样支持备份。
func TestUploadFormBackupConflict(t *testing.T) {
	root := makeFixture(t)
	s := newTestServer(t, testConfig(t, root, "-A"))

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	if err := mw.WriteField("conflict", "backup"); err != nil {
		t.Fatal(err)
	}
	fw, err := mw.CreateFormFile("file", "notes.txt")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fw.Write([]byte("form content")); err != nil {
		t.Fatal(err)
	}
	if err := mw.Close(); err != nil {
		t.Fatal(err)
	}

	r := do(t, s, http.MethodPost, "/", &buf,
		map[string]string{"Content-Type": mw.FormDataContentType()})
	if r.code != http.StatusOK {
		t.Fatalf("状态码 = %d, 期望 200；%s", r.code, r.body)
	}
	got, _ := os.ReadFile(filepath.Join(root, "notes.txt"))
	if string(got) != "form content" {
		t.Errorf("新文件内容 = %q", got)
	}
	matches, _ := filepath.Glob(filepath.Join(root, "notes-*.txt"))
	if len(matches) != 1 {
		t.Fatalf("备份文件数量 = %d, 期望 1：%v", len(matches), matches)
	}
	old, _ := os.ReadFile(matches[0])
	if string(old) != "top level\n" {
		t.Errorf("备份内容 = %q", old)
	}
}

// TestExtractConflictCheck 验证 check=1 会列出同名目标而不落盘。
func TestExtractConflictCheck(t *testing.T) {
	root := makeFixture(t)
	writeFile(t, filepath.Join(root, "out", "hello.txt"), []byte("old"))
	s := newTestServer(t, testConfig(t, root, "-A"))

	r := do(t, s, http.MethodGet, "/fs/extract?path=/sample.zip&dest=/out&check=1",
		nil, map[string]string{"Accept": "application/json"})
	if r.code != http.StatusOK {
		t.Fatalf("状态码 = %d, 期望 200；%s", r.code, r.body)
	}
	var got extractCheck
	if err := json.Unmarshal([]byte(r.body), &got); err != nil {
		t.Fatalf("响应不是 JSON: %v；%s", err, r.body)
	}
	if !got.DestNonEmpty {
		t.Errorf("dest_nonempty = false, 期望 true")
	}
	if len(got.Conflicts) != 1 {
		t.Fatalf("冲突数 = %d, 期望 1：%+v", len(got.Conflicts), got.Conflicts)
	}
	if got.Conflicts[0].Name != "hello.txt" {
		t.Errorf("冲突条目 = %q, 期望 hello.txt", got.Conflicts[0].Name)
	}
	// 预检不能改动磁盘。
	if b, _ := os.ReadFile(filepath.Join(root, "out", "hello.txt")); string(b) != "old" {
		t.Errorf("预检改动了目标文件: %q", b)
	}
	if _, err := os.Stat(filepath.Join(root, "out", "sub", "deep.txt")); !os.IsNotExist(err) {
		t.Errorf("预检不应该解压出文件")
	}
}

// TestExtractReplaceConflict 验证解压时替换同名文件。
func TestExtractReplaceConflict(t *testing.T) {
	root := makeFixture(t)
	writeFile(t, filepath.Join(root, "out", "hello.txt"), []byte("old"))
	s := newTestServer(t, testConfig(t, root, "-A"))

	_, r := runExtract(t, s, `{"path":"/sample.zip","dest":"/out","conflict":"replace"}`)
	if r.code != http.StatusOK || !strings.Contains(r.body, "event: done") {
		t.Fatalf("解压失败：code=%d body=%s", r.code, r.body)
	}
	got, _ := os.ReadFile(filepath.Join(root, "out", "hello.txt"))
	if string(got) != "zip hello\n" {
		t.Errorf("内容 = %q, 期望 zip hello", got)
	}
	if matches, _ := filepath.Glob(filepath.Join(root, "out", "hello-*.txt")); len(matches) != 0 {
		t.Errorf("replace 不应产生备份：%v", matches)
	}
}

// TestExtractBackupConflict 验证解压时先备份同名文件。
func TestExtractBackupConflict(t *testing.T) {
	root := makeFixture(t)
	writeFile(t, filepath.Join(root, "out", "hello.txt"), []byte("old"))
	s := newTestServer(t, testConfig(t, root, "-A"))

	_, r := runExtract(t, s, `{"path":"/sample.zip","dest":"/out","conflict":"backup"}`)
	if r.code != http.StatusOK || !strings.Contains(r.body, "event: done") {
		t.Fatalf("解压失败：code=%d body=%s", r.code, r.body)
	}
	got, _ := os.ReadFile(filepath.Join(root, "out", "hello.txt"))
	if string(got) != "zip hello\n" {
		t.Errorf("新文件内容 = %q", got)
	}
	matches, _ := filepath.Glob(filepath.Join(root, "out", "hello-*.txt"))
	if len(matches) != 1 {
		t.Fatalf("备份文件数量 = %d, 期望 1：%v", len(matches), matches)
	}
	old, _ := os.ReadFile(matches[0])
	if string(old) != "old" {
		t.Errorf("备份内容 = %q, 期望 old", old)
	}
}

// TestExtractSymlinkDoesNotTouchExistingTarget 验证包内被跳过的条目
// 不会触发同名处理：tar 的软链条目在 Extract 中直接跳过，用户已有的
// 同名文件既不能被删除，也不能被改名。
func TestExtractSymlinkDoesNotTouchExistingTarget(t *testing.T) {
	root := makeFixture(t)

	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	if err := tw.WriteHeader(&tar.Header{
		Name: "notes.txt", Linkname: "/etc/passwd", Mode: 0o777, Typeflag: tar.TypeSymlink,
	}); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(root, "links.tar"), buf.Bytes())

	s := newTestServer(t, testConfig(t, root, "-A"))
	_, r := runExtract(t, s, `{"path":"/links.tar","dest":"/","conflict":"backup"}`)
	if r.code != http.StatusOK || !strings.Contains(r.body, "event: done") {
		t.Fatalf("解压失败：code=%d body=%s", r.code, r.body)
	}
	got, err := os.ReadFile(filepath.Join(root, "notes.txt"))
	if err != nil || string(got) != "top level\n" {
		t.Errorf("原文件被改动了: %q err=%v", got, err)
	}
	if matches, _ := filepath.Glob(filepath.Join(root, "notes-*.txt")); len(matches) != 0 {
		t.Errorf("被跳过的软链条目不应触发备份：%v", matches)
	}
}

// TestExtractReplaceDirectoryConflict 验证压缩包里的文件可以替换同名目录。
func TestExtractReplaceDirectoryConflict(t *testing.T) {
	root := makeFixture(t)
	writeFile(t, filepath.Join(root, "out", "hello.txt", "inside.txt"), []byte("x"))
	s := newTestServer(t, testConfig(t, root, "-A"))

	_, r := runExtract(t, s, `{"path":"/sample.zip","dest":"/out","conflict":"replace"}`)
	if r.code != http.StatusOK || !strings.Contains(r.body, "event: done") {
		t.Fatalf("解压失败：code=%d body=%s", r.code, r.body)
	}
	st, err := os.Stat(filepath.Join(root, "out", "hello.txt"))
	if err != nil || st.IsDir() {
		t.Fatalf("同名目录没有被替换成文件: st=%v err=%v", st, err)
	}
	got, _ := os.ReadFile(filepath.Join(root, "out", "hello.txt"))
	if string(got) != "zip hello\n" {
		t.Errorf("内容 = %q", got)
	}
}

// TestExtractBackupDirectoryConflict 验证压缩包里的文件会把同名目录整体改名保留。
func TestExtractBackupDirectoryConflict(t *testing.T) {
	root := makeFixture(t)
	writeFile(t, filepath.Join(root, "out", "hello.txt", "inside.txt"), []byte("x"))
	s := newTestServer(t, testConfig(t, root, "-A"))

	_, r := runExtract(t, s, `{"path":"/sample.zip","dest":"/out","conflict":"backup"}`)
	if r.code != http.StatusOK || !strings.Contains(r.body, "event: done") {
		t.Fatalf("解压失败：code=%d body=%s", r.code, r.body)
	}
	matches, _ := filepath.Glob(filepath.Join(root, "out", "hello.txt-*"))
	if len(matches) != 1 {
		t.Fatalf("目录备份数量 = %d, 期望 1：%v", len(matches), matches)
	}
	if _, err := os.Stat(filepath.Join(matches[0], "inside.txt")); err != nil {
		t.Errorf("备份目录内容丢失: %v", err)
	}
	got, _ := os.ReadFile(filepath.Join(root, "out", "hello.txt"))
	if string(got) != "zip hello\n" {
		t.Errorf("新文件内容 = %q", got)
	}
}

// TestExtractAncestorFileConflict 验证祖先同名文件也会被处理：
// 包内有 sub/deep.txt，而目标目录里 sub 是一个普通文件。
func TestExtractAncestorFileConflict(t *testing.T) {
	root := makeFixture(t)
	writeFile(t, filepath.Join(root, "out", "sub"), []byte("i am a file"))
	s := newTestServer(t, testConfig(t, root, "-A"))

	_, r := runExtract(t, s, `{"path":"/sample.zip","dest":"/out","conflict":"backup"}`)
	if r.code != http.StatusOK || !strings.Contains(r.body, "event: done") {
		t.Fatalf("解压失败：code=%d body=%s", r.code, r.body)
	}
	got, err := os.ReadFile(filepath.Join(root, "out", "sub", "deep.txt"))
	if err != nil || string(got) != "nested\n" {
		t.Fatalf("压缩包内容没有落盘: %q err=%v", got, err)
	}
	matches, _ := filepath.Glob(filepath.Join(root, "out", "sub-*"))
	if len(matches) != 1 {
		t.Fatalf("祖先文件备份数量 = %d, 期望 1：%v", len(matches), matches)
	}
	old, _ := os.ReadFile(matches[0])
	if string(old) != "i am a file" {
		t.Errorf("备份内容 = %q", old)
	}
}

// TestConflictMethodBackupRootFolder 验证文件夹上传只备份根文件夹。
func TestConflictMethodBackupRootFolder(t *testing.T) {
	root := makeFixture(t)
	writeFile(t, filepath.Join(root, "proj", "old.txt"), []byte("old"))
	writeFile(t, filepath.Join(root, "proj", "sub", "nested.txt"), []byte("nested"))
	s := newTestServer(t, testConfig(t, root, "-A"))

	r := do(t, s, MethodConflict, "/proj?conflict=backup", nil, nil)
	if r.code != http.StatusNoContent {
		t.Fatalf("状态码 = %d, 期望 204；%s", r.code, r.body)
	}
	matches, _ := filepath.Glob(filepath.Join(root, "proj-*"))
	if len(matches) != 1 {
		t.Fatalf("根文件夹备份数量 = %d, 期望 1：%v", len(matches), matches)
	}
	if got, _ := os.ReadFile(filepath.Join(matches[0], "old.txt")); string(got) != "old" {
		t.Errorf("备份内容缺失: %q", got)
	}
	if _, err := os.Stat(filepath.Join(matches[0], "sub", "nested.txt")); err != nil {
		t.Errorf("备份目录结构缺失: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "proj")); !os.IsNotExist(err) {
		t.Errorf("原根文件夹仍然存在")
	}

	// 之后普通上传不应再触发逐文件备份。
	for _, name := range []string{"a.txt", "sub/b.txt"} {
		rr := do(t, s, http.MethodPut, "/proj/"+name, strings.NewReader("new"), nil)
		if rr.code != http.StatusCreated {
			t.Fatalf("上传 %s 失败：%d %s", name, rr.code, rr.body)
		}
	}
	if matches, _ := filepath.Glob(filepath.Join(root, "proj", "*-*")); len(matches) != 0 {
		t.Errorf("普通上传不应再产生逐文件备份：%v", matches)
	}
}

// TestConflictMethodReplaceRootFolder 验证文件夹上传可以整体替换根文件夹。
func TestConflictMethodReplaceRootFolder(t *testing.T) {
	root := makeFixture(t)
	writeFile(t, filepath.Join(root, "proj", "old.txt"), []byte("old"))
	s := newTestServer(t, testConfig(t, root, "-A"))

	if r := do(t, s, MethodConflict, "/proj?conflict=replace", nil, nil); r.code != http.StatusNoContent {
		t.Fatalf("状态码 = %d, 期望 204；%s", r.code, r.body)
	}
	if _, err := os.Stat(filepath.Join(root, "proj")); !os.IsNotExist(err) {
		t.Fatalf("替换后根文件夹仍然存在")
	}
	if r := do(t, s, http.MethodPut, "/proj/new.txt", strings.NewReader("new"), nil); r.code != http.StatusCreated {
		t.Fatalf("上传失败：%d %s", r.code, r.body)
	}
	if matches, _ := filepath.Glob(filepath.Join(root, "proj-*")); len(matches) != 0 {
		t.Errorf("replace 不应产生备份：%v", matches)
	}
}

// TestConflictMethodValidation 验证根目录与非法参数被拒绝。
func TestConflictMethodValidation(t *testing.T) {
	root := makeFixture(t)
	s := newTestServer(t, testConfig(t, root, "-A"))

	if r := do(t, s, MethodConflict, "/?conflict=backup", nil, nil); r.code != http.StatusForbidden {
		t.Errorf("对服务根执行 CONFLICT 返回 %d, 期望 403", r.code)
	}
	if r := do(t, s, MethodConflict, "/docs?conflict=banana", nil, nil); r.code != http.StatusBadRequest {
		t.Errorf("非法 conflict 返回 %d, 期望 400", r.code)
	}
	if r := do(t, s, MethodConflict, "/docs", nil, nil); r.code != http.StatusBadRequest {
		t.Errorf("缺少 conflict 返回 %d, 期望 400", r.code)
	}
}
