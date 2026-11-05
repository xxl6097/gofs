package server

import (
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// copyReq 发一次 COPY 请求，dest 是 URL 路径（会拼成绝对 URL）。
func copyReq(t *testing.T, s *Server, src, dest string, overwrite bool) resp {
	t.Helper()
	h := map[string]string{"Destination": testHost + dest}
	if overwrite {
		h["Overwrite"] = "T"
	}
	return do(t, s, MethodCopy, testHost+src, nil, h)
}

// TestCopyFileKeepsSource 是 COPY 的核心语义：目标出现新文件，源必须还在。
func TestCopyFileKeepsSource(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "a.txt"), []byte("hello"))
	s := newTestServer(t, testConfig(t, root, "-A"))

	got := copyReq(t, s, "/a.txt", "/b.txt", false)
	if got.code != http.StatusCreated {
		t.Fatalf("返回 %d，期望 201：%s", got.code, got.body)
	}

	src, err := os.ReadFile(filepath.Join(root, "a.txt"))
	if err != nil {
		t.Fatalf("源文件被弄没了：%v", err)
	}
	dst, err := os.ReadFile(filepath.Join(root, "b.txt"))
	if err != nil {
		t.Fatalf("目标文件没生成：%v", err)
	}
	if string(src) != "hello" || string(dst) != "hello" {
		t.Fatalf("内容不对：源=%q 目标=%q", src, dst)
	}
}

// TestCopyDirectoryRecursive 覆盖目录整棵复制（含子目录和文件）。
func TestCopyDirectoryRecursive(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "docs", "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(root, "docs", "a.txt"), []byte("A"))
	writeFile(t, filepath.Join(root, "docs", "sub", "b.txt"), []byte("B"))
	s := newTestServer(t, testConfig(t, root, "-A"))

	got := copyReq(t, s, "/docs", "/docs-copy", false)
	if got.code != http.StatusCreated {
		t.Fatalf("返回 %d，期望 201：%s", got.code, got.body)
	}
	for _, p := range []string{"docs-copy/a.txt", "docs-copy/sub/b.txt"} {
		if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(p))); err != nil {
			t.Errorf("缺少 %s：%v", p, err)
		}
	}
	// 源目录必须原样保留。
	if _, err := os.Stat(filepath.Join(root, "docs", "sub", "b.txt")); err != nil {
		t.Errorf("源目录被改动了：%v", err)
	}
}

// TestCopyIntoOwnSubtree 防无限递归：目录不能复制到它自己里面。
func TestCopyIntoOwnSubtree(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "a", "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(root, "a", "keep.txt"), []byte("重要数据"))
	s := newTestServer(t, testConfig(t, root, "-A"))

	for _, dest := range []string{"/a/sub", "/a/sub/deeper", "/a"} {
		got := copyReq(t, s, "/a", dest, false)
		if got.code != http.StatusBadRequest {
			t.Fatalf("复制到 %s 返回 %d，期望 400：%s", dest, got.code, got.body)
		}
		if !strings.Contains(got.body, "不能把目录复制到它自己里面") {
			t.Errorf("错误信息不明确：%s", got.body)
		}
		if data, err := os.ReadFile(filepath.Join(root, "a", "keep.txt")); err != nil ||
			string(data) != "重要数据" {
			t.Fatalf("源被破坏了：%v %q", err, data)
		}
	}
}

// TestCopyOverwrite 覆盖语义：默认不覆盖（412），Overwrite: T 时替换。
func TestCopyOverwrite(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "a.txt"), []byte("new"))
	writeFile(t, filepath.Join(root, "b.txt"), []byte("old"))
	s := newTestServer(t, testConfig(t, root, "-A"))

	got := copyReq(t, s, "/a.txt", "/b.txt", false)
	if got.code != http.StatusPreconditionFailed {
		t.Fatalf("不覆盖时返回 %d，期望 412", got.code)
	}
	if data, _ := os.ReadFile(filepath.Join(root, "b.txt")); string(data) != "old" {
		t.Fatalf("被拒绝的复制改写了目标：%q", data)
	}

	got = copyReq(t, s, "/a.txt", "/b.txt", true)
	if got.code != http.StatusNoContent {
		t.Fatalf("覆盖时返回 %d，期望 204：%s", got.code, got.body)
	}
	if data, _ := os.ReadFile(filepath.Join(root, "b.txt")); string(data) != "new" {
		t.Fatalf("覆盖后内容不对：%q", data)
	}
}

// TestCopyChecksDestinationPermission 目标也要过权限模型：
// 只对 /docs 有写权限的账号不能把文件复制到 /secret。
func TestCopyChecksDestinationPermission(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "docs"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "secret"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(root, "docs", "a.txt"), []byte("A"))
	s := newTestServer(t, testConfig(t, root, "-A", "-a", "bob:pw@/docs:rw"))

	got := copyReq(t, s, "/docs/a.txt", "/secret/a.txt", false)
	if got.code == http.StatusCreated || got.code == http.StatusNoContent {
		t.Errorf("只对 /docs 有写权限的账号把文件复制到了 /secret：%d", got.code)
	}
	if _, err := os.Stat(filepath.Join(root, "secret", "a.txt")); err == nil {
		t.Error("文件竟然真的落到 /secret 了")
	}
	if _, err := os.Stat(filepath.Join(root, "docs", "a.txt")); err != nil {
		t.Errorf("被拒绝的复制把源文件弄没了：%v", err)
	}
}

// TestCopySameDirBackup 同目录复制：默认拒绝，带 ?backup=1 时在原目录生成
// 一份带时间戳的备份副本，源文件保持不变。
func TestCopySameDirBackup(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "a.txt"), []byte("hello"))
	s := newTestServer(t, testConfig(t, root, "-A"))

	// 不带 backup 参数：目标就是自己，直接拒绝。
	got := do(t, s, MethodCopy, testHost+"/a.txt", nil,
		map[string]string{"Destination": testHost + "/a.txt"})
	if got.code != http.StatusBadRequest {
		t.Fatalf("同目录复制返回 %d，期望 400：%s", got.code, got.body)
	}

	got = do(t, s, MethodCopy, testHost+"/a.txt?backup=1", nil,
		map[string]string{"Destination": testHost + "/a.txt"})
	if got.code != http.StatusCreated {
		t.Fatalf("同目录备份返回 %d，期望 201：%s", got.code, got.body)
	}

	src, err := os.ReadFile(filepath.Join(root, "a.txt"))
	if err != nil || string(src) != "hello" {
		t.Fatalf("源文件被改动了：%v %q", err, src)
	}

	matches, err := filepath.Glob(filepath.Join(root, "a-*.txt"))
	if err != nil || len(matches) != 1 {
		t.Fatalf("备份文件数量 = %d，期望 1：%v %v", len(matches), matches, err)
	}
	data, err := os.ReadFile(matches[0])
	if err != nil || string(data) != "hello" {
		t.Fatalf("备份内容不对：%v %q", err, data)
	}

	gotPath := got.head.Get(copyBackupPathHeader)
	if gotPath == "" {
		t.Fatal("响应缺少备份路径头")
	}
	decoded, err := url.PathUnescape(gotPath)
	if err != nil {
		t.Fatalf("备份路径头无法解码：%v", err)
	}
	wantRel, _ := filepath.Rel(root, matches[0])
	if decoded != "/"+filepath.ToSlash(wantRel) {
		t.Errorf("备份路径头 = %q，期望 /%s", decoded, filepath.ToSlash(wantRel))
	}
}

// TestCopySameDirBackupDirectory 目录同目录备份同样整棵复制，源目录保留。
func TestCopySameDirBackupDirectory(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "docs", "a.txt"), []byte("A"))
	s := newTestServer(t, testConfig(t, root, "-A"))

	got := do(t, s, MethodCopy, testHost+"/docs?backup=1", nil,
		map[string]string{"Destination": testHost + "/docs"})
	if got.code != http.StatusCreated {
		t.Fatalf("目录同目录备份返回 %d，期望 201：%s", got.code, got.body)
	}
	matches, _ := filepath.Glob(filepath.Join(root, "docs-*"))
	if len(matches) != 1 {
		t.Fatalf("备份目录数量 = %d，期望 1：%v", len(matches), matches)
	}
	if data, err := os.ReadFile(filepath.Join(matches[0], "a.txt")); err != nil || string(data) != "A" {
		t.Fatalf("备份目录内容不对：%v %q", err, data)
	}
	if _, err := os.Stat(filepath.Join(root, "docs", "a.txt")); err != nil {
		t.Errorf("源目录被改动了：%v", err)
	}
}

// TestCopyRequiresUploadFlag 没开 --allow-upload 时复制被禁用（与 MOVE 同一道闸门）。
func TestCopyRequiresUploadFlag(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "a.txt"), []byte("A"))
	s := newTestServer(t, testConfig(t, root))

	got := copyReq(t, s, "/a.txt", "/b.txt", false)
	if got.code != http.StatusForbidden {
		t.Fatalf("返回 %d，期望 403：%s", got.code, got.body)
	}
}
