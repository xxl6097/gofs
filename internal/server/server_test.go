package server

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/xxl6097/gofs/internal/config"
)

// ---------------------------------------------------------------------------
// 测试脚手架
// ---------------------------------------------------------------------------

// testConfig 构造测试用配置。extra 为额外的命令行参数。
//
// 默认关闭「上传按日期归档」，以免干扰与上传路径无关的用例；
// 需要验证归档行为的用例请用 testConfigDated。
// 同时把密钥文件指向空串（仅内存），避免测试污染用户的 ~/.config/gofs/keys.json。
func testConfig(t *testing.T, root string, extra ...string) *config.Config {
	t.Helper()
	args := []string{"-b", "127.0.0.1", "-p", "0", "-log-format", "none",
		"--no-upload-dated", "--key-file", ""}
	args = append(args, extra...)
	args = append(args, root)
	cfg, err := config.Parse(args, io.Discard)
	if err != nil {
		t.Fatalf("解析配置失败: %v", err)
	}
	return cfg
}

// testConfigDated 构造开启了上传日期归档的配置。
func testConfigDated(t *testing.T, root, layout string, extra ...string) *config.Config {
	t.Helper()
	args := []string{"-b", "127.0.0.1", "-p", "0", "-log-format", "none",
		"--upload-date-layout", layout, "--key-file", ""}
	args = append(args, extra...)
	args = append(args, root)
	cfg, err := config.Parse(args, io.Discard)
	if err != nil {
		t.Fatalf("解析配置失败: %v", err)
	}
	return cfg
}

// todayDir 返回按给定布局计算出的「今天」目录段，用于断言落盘位置。
func todayDir(layout string) string {
	return time.Now().Format(layout)
}

// newTestServer 构造被测服务。
func newTestServer(t *testing.T, cfg *config.Config) *Server {
	t.Helper()
	s, err := New(cfg, os.DirFS(filepath.Join("..", "..", "assets")))
	if err != nil {
		t.Fatalf("创建服务失败: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// resp 为一次请求的观察结果。
type resp struct {
	code int
	body string
	head http.Header
}

// do 在进程内直接调用 handler，不经过真实网络。
func do(t *testing.T, s *Server, method, target string, body io.Reader, headers map[string]string) resp {
	t.Helper()
	r := httptest.NewRequest(method, target, body)
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	res := w.Result()
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return resp{code: res.StatusCode, body: string(b), head: res.Header}
}

func writeFile(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

// makeFixture 准备一套测试数据，返回根目录。
func makeFixture(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "docs", "readme.md"), []byte("# hello gofs\n"))
	writeFile(t, filepath.Join(root, "docs", "guide.txt"), []byte(strings.Repeat("line\n", 500)))
	writeFile(t, filepath.Join(root, "媒体", "中文文件.txt"), []byte("中文内容测试\n"))
	writeFile(t, filepath.Join(root, "notes.txt"), []byte("top level\n"))

	// sample.zip
	zbuf := &bytes.Buffer{}
	zw := zip.NewWriter(zbuf)
	for name, data := range map[string]string{
		"hello.txt":    "zip hello\n",
		"sub/deep.txt": "nested\n",
	} {
		f, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.Write([]byte(data)); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(root, "sample.zip"), zbuf.Bytes())

	// sample.tar.gz
	writeFile(t, filepath.Join(root, "sample.tar.gz"), makeTarGz(t, map[string]string{
		"a.txt":      "tar gz A\n",
		"deep/b.txt": "tar gz B\n",
		"中文名.txt":    "中文 tar.gz\n",
	}))

	// sample.tar
	writeFile(t, filepath.Join(root, "sample.tar"), makeTar(t, map[string]string{
		"plain.txt": "plain tar\n",
	}))

	// notes.txt.gz
	var gbuf bytes.Buffer
	gw := gzip.NewWriter(&gbuf)
	if _, err := gw.Write([]byte("gzip payload\n")); err != nil {
		t.Fatal(err)
	}
	if err := gw.Close(); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(root, "notes.txt.gz"), gbuf.Bytes())

	return root
}

// makeTar 生成未压缩 tar。
func makeTar(t *testing.T, files map[string]string) []byte {
	t.Helper()
	buf := &bytes.Buffer{}
	tw := tar.NewWriter(buf)
	for name, data := range files {
		if err := tw.WriteHeader(&tar.Header{
			Name: name, Mode: 0o644, Size: int64(len(data)), Typeflag: tar.TypeReg,
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(data)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// makeTarGz 生成 gzip 压缩的 tar。
func makeTarGz(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gw)
	for name, data := range files {
		if err := tw.WriteHeader(&tar.Header{
			Name: name, Mode: 0o644, Size: int64(len(data)), Typeflag: tar.TypeReg,
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(data)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// sseEvents 解析 SSE 响应为「事件名 -> 数据」的有序列表。
func sseEvents(t *testing.T, body string) []struct {
	name string
	data string
} {
	t.Helper()
	out := []struct {
		name string
		data string
	}{}
	for _, block := range strings.Split(body, "\n\n") {
		block = strings.TrimSpace(block)
		if block == "" {
			continue
		}
		var name, data string
		for _, line := range strings.Split(block, "\n") {
			switch {
			case strings.HasPrefix(line, "event: "):
				name = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: "):
				data += strings.TrimPrefix(line, "data: ")
			}
		}
		if name != "" {
			out = append(out, struct {
				name string
				data string
			}{name, data})
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// 目录列举
// ---------------------------------------------------------------------------

func TestListJSON(t *testing.T) {
	root := makeFixture(t)
	s := newTestServer(t, testConfig(t, root, "-A"))

	r := do(t, s, http.MethodGet, "/?json", nil, nil)
	if r.code != 200 {
		t.Fatalf("状态码 = %d, 期望 200；body=%s", r.code, r.body)
	}
	var listing struct {
		Path    string `json:"path"`
		Total   int    `json:"total"`
		Entries []struct {
			Name    string `json:"name"`
			IsDir   bool   `json:"is_dir"`
			Archive bool   `json:"archive"`
			Size    int64  `json:"size"`
		} `json:"entries"`
	}
	if err := json.Unmarshal([]byte(r.body), &listing); err != nil {
		t.Fatalf("JSON 解析失败: %v\n%s", err, r.body)
	}
	if listing.Path != "/" {
		t.Errorf("path = %q, 期望 /", listing.Path)
	}
	if listing.Total != 7 {
		t.Errorf("条目数 = %d, 期望 7", listing.Total)
	}
	// 目录应排在文件前面。
	if !listing.Entries[0].IsDir {
		t.Errorf("首项应为目录，实际为 %q", listing.Entries[0].Name)
	}
	// 压缩包应被标记。
	archiveCount := 0
	for _, e := range listing.Entries {
		if e.Archive {
			archiveCount++
		}
	}
	if archiveCount != 4 {
		t.Errorf("识别为压缩包的条目 = %d, 期望 4", archiveCount)
	}
}

func TestSimpleListing(t *testing.T) {
	root := makeFixture(t)
	s := newTestServer(t, testConfig(t, root, "-A"))

	r := do(t, s, http.MethodGet, "/?simple", nil, nil)
	if r.code != 200 {
		t.Fatalf("状态码 = %d, 期望 200", r.code)
	}
	if !strings.Contains(r.body, "docs/") {
		t.Errorf("simple 输出缺少 docs/：\n%s", r.body)
	}
	if !strings.Contains(r.body, "notes.txt") {
		t.Errorf("simple 输出缺少 notes.txt：\n%s", r.body)
	}
}

func TestSearch(t *testing.T) {
	root := makeFixture(t)
	s := newTestServer(t, testConfig(t, root, "-A"))

	r := do(t, s, http.MethodGet, "/?q=readme&json", nil, nil)
	if r.code != 200 {
		t.Fatalf("状态码 = %d, 期望 200", r.code)
	}
	var listing struct {
		Entries []struct {
			Name string `json:"name"`
			Path string `json:"path"`
		} `json:"entries"`
	}
	if err := json.Unmarshal([]byte(r.body), &listing); err != nil {
		t.Fatal(err)
	}
	if len(listing.Entries) != 1 || listing.Entries[0].Name != "readme.md" {
		t.Errorf("搜索结果 = %+v, 期望只命中 readme.md", listing.Entries)
	}
	if listing.Entries[0].Path != "/docs/readme.md" {
		t.Errorf("路径 = %q, 期望 /docs/readme.md", listing.Entries[0].Path)
	}
}

func TestSearchDisabled(t *testing.T) {
	root := makeFixture(t)
	s := newTestServer(t, testConfig(t, root)) // 默认无任何权限

	r := do(t, s, http.MethodGet, "/?q=readme", nil, nil)
	if r.code != http.StatusForbidden {
		t.Errorf("状态码 = %d, 期望 403", r.code)
	}
}

// ---------------------------------------------------------------------------
// 打包下载
// ---------------------------------------------------------------------------

func TestDownloadZip(t *testing.T) {
	root := makeFixture(t)
	s := newTestServer(t, testConfig(t, root, "-A"))

	r := do(t, s, http.MethodGet, "/docs?zip", nil, nil)
	if r.code != 200 {
		t.Fatalf("状态码 = %d, 期望 200", r.code)
	}
	if ct := r.head.Get("Content-Type"); ct != "application/zip" {
		t.Errorf("Content-Type = %q", ct)
	}
	if cd := r.head.Get("Content-Disposition"); !strings.Contains(cd, "docs.zip") {
		t.Errorf("Content-Disposition = %q", cd)
	}

	zr, err := zip.NewReader(bytes.NewReader([]byte(r.body)), int64(len(r.body)))
	if err != nil {
		t.Fatalf("产物不是合法 zip: %v", err)
	}
	got := map[string]bool{}
	for _, f := range zr.File {
		got[f.Name] = true
	}
	for _, want := range []string{"readme.md", "guide.txt"} {
		if !got[want] {
			t.Errorf("zip 中缺少 %q，实际含 %v", want, got)
		}
	}
}

func TestDownloadZipDisabled(t *testing.T) {
	root := makeFixture(t)
	s := newTestServer(t, testConfig(t, root)) // 未开启 archive

	r := do(t, s, http.MethodGet, "/docs?zip", nil, nil)
	if r.code != http.StatusForbidden {
		t.Errorf("状态码 = %d, 期望 403", r.code)
	}
}

// ---------------------------------------------------------------------------
// 打包下载：默认全部 / 选中则只打包所选
// ---------------------------------------------------------------------------

// zipEntries 解析打包产物里的条目名（已排序）。
func zipEntries(t *testing.T, body string) []string {
	t.Helper()
	zr, err := zip.NewReader(bytes.NewReader([]byte(body)), int64(len(body)))
	if err != nil {
		t.Fatalf("产物不是合法 zip: %v", err)
	}
	out := make([]string, 0, len(zr.File))
	for _, f := range zr.File {
		out = append(out, f.Name)
	}
	sort.Strings(out)
	return out
}

// wantEntries 断言 zip 里的条目集合恰好等于 want（顺序无关）。
func wantEntries(t *testing.T, got []string, want ...string) {
	t.Helper()
	sort.Strings(want)
	if len(got) != len(want) {
		t.Fatalf("zip 条目 = %v, 期望 %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("zip 条目 = %v, 期望 %v", got, want)
		}
	}
}

func TestDownloadZipWithoutPickPacksEverything(t *testing.T) {
	root := makeFixture(t)
	s := newTestServer(t, testConfig(t, root, "-A"))

	r := do(t, s, http.MethodGet, "/docs?zip", nil, nil)
	if r.code != 200 {
		t.Fatalf("状态码 = %d, 期望 200", r.code)
	}
	if got := r.head.Get("X-Gofs-Archive-Mode"); got != "all" {
		t.Errorf("X-Gofs-Archive-Mode = %q, 期望 all", got)
	}
	wantEntries(t, zipEntries(t, r.body), "readme.md", "guide.txt")
}

func TestDownloadZipSelectedViaQuery(t *testing.T) {
	root := makeFixture(t)
	s := newTestServer(t, testConfig(t, root, "-A"))

	// 只选一个：另一个必须不在包里。
	r := do(t, s, http.MethodGet, "/docs?zip&pick=/docs/readme.md", nil, nil)
	if r.code != 200 {
		t.Fatalf("状态码 = %d, 期望 200；%s", r.code, r.body)
	}
	if got := r.head.Get("X-Gofs-Archive-Mode"); got != "picked" {
		t.Errorf("X-Gofs-Archive-Mode = %q, 期望 picked", got)
	}
	wantEntries(t, zipEntries(t, r.body), "readme.md")
	// 单个条目时直接用它命名，下载下来更直观。
	if cd := r.head.Get("Content-Disposition"); !strings.Contains(cd, "readme.md.zip") {
		t.Errorf("Content-Disposition = %q, 期望含 readme.md.zip", cd)
	}

	// 选两个（含重复参数与重复项，应当去重）。
	r = do(t, s, http.MethodGet, "/docs?zip&pick=/docs/readme.md&pick=/docs/guide.txt&pick=/docs/readme.md", nil, nil)
	if r.code != 200 {
		t.Fatalf("状态码 = %d, 期望 200", r.code)
	}
	wantEntries(t, zipEntries(t, r.body), "readme.md", "guide.txt")
	// 多项时用目录名 + -selected。
	if cd := r.head.Get("Content-Disposition"); !strings.Contains(cd, "docs-selected.zip") {
		t.Errorf("Content-Disposition = %q, 期望含 docs-selected.zip", cd)
	}
}

func TestDownloadZipSelectedViaPost(t *testing.T) {
	root := makeFixture(t)
	s := newTestServer(t, testConfig(t, root, "-A"))

	body := strings.NewReader(`{"picks":["/docs/guide.txt"]}`)
	r := do(t, s, http.MethodPost, "/docs?zip", body,
		map[string]string{"Content-Type": "application/json"})
	if r.code != 200 {
		t.Fatalf("状态码 = %d, 期望 200；%s", r.code, r.body)
	}
	wantEntries(t, zipEntries(t, r.body), "guide.txt")

	// 换行、空格等噪音不该让它失败。
	body = strings.NewReader(`{"picks":["  /docs/readme.md  ","/docs/guide.txt"]}`)
	r = do(t, s, http.MethodPost, "/docs?zip", body,
		map[string]string{"Content-Type": "application/json"})
	if r.code != 200 {
		t.Fatalf("状态码 = %d, 期望 200；%s", r.code, r.body)
	}
	wantEntries(t, zipEntries(t, r.body), "readme.md", "guide.txt")

	// 非法 JSON 应给出 400 而不是 500。
	body = strings.NewReader(`{ oops`)
	r = do(t, s, http.MethodPost, "/docs?zip", body,
		map[string]string{"Content-Type": "application/json"})
	if r.code != http.StatusBadRequest {
		t.Errorf("非法 JSON 状态码 = %d, 期望 400", r.code)
	}

	// 打包目标是文件时应拒绝（POST 到文件原本是上传语义）。
	body = strings.NewReader(`{"picks":["/docs/readme.md"]}`)
	r = do(t, s, http.MethodPost, "/notes.txt?zip", body,
		map[string]string{"Content-Type": "application/json"})
	if r.code != http.StatusBadRequest {
		t.Errorf("对文件打包状态码 = %d, 期望 400", r.code)
	}
}

func TestDownloadZipSelectedDirectoryRecurses(t *testing.T) {
	root := makeFixture(t)
	s := newTestServer(t, testConfig(t, root, "-A"))

	// 选中子目录：目录本身与其中的文件都要进包。
	writeFile(t, filepath.Join(root, "docs", "sub", "deep.md"), []byte("# deep\n"))
	writeFile(t, filepath.Join(root, "docs", "sub", "inner", "x.txt"), []byte("x\n"))

	r := do(t, s, http.MethodGet, "/docs?zip&pick=/docs/sub", nil, nil)
	if r.code != 200 {
		t.Fatalf("状态码 = %d, 期望 200；%s", r.code, r.body)
	}
	wantEntries(t, zipEntries(t, r.body), "sub/", "sub/deep.md", "sub/inner/", "sub/inner/x.txt")

	// 混合：目录 + 同级文件。
	r = do(t, s, http.MethodGet, "/docs?zip&pick=/docs/sub&pick=/docs/readme.md", nil, nil)
	if r.code != 200 {
		t.Fatalf("状态码 = %d, 期望 200", r.code)
	}
	wantEntries(t, zipEntries(t, r.body),
		"sub/", "sub/deep.md", "sub/inner/", "sub/inner/x.txt", "readme.md")
}

func TestDownloadZipSelectedRelativeToCwd(t *testing.T) {
	root := makeFixture(t)
	s := newTestServer(t, testConfig(t, root, "-A"))

	// 不以 / 开头时按「相对当前目录」解析。
	r := do(t, s, http.MethodGet, "/docs?zip&pick=readme.md", nil, nil)
	if r.code != 200 {
		t.Fatalf("状态码 = %d, 期望 200；%s", r.code, r.body)
	}
	wantEntries(t, zipEntries(t, r.body), "readme.md")
}

func TestDownloadZipSelectedFromSearchCrossesDirs(t *testing.T) {
	root := makeFixture(t)
	s := newTestServer(t, testConfig(t, root, "-A"))

	// 站内搜索是跨目录的：命中项不在当前目录下也要能打包，
	// 包内路径退回「相对服务根」，避免同名文件互相覆盖。
	r := do(t, s, http.MethodGet, "/docs?zip&pick=/docs/readme.md&pick=/媒体/中文文件.txt", nil, nil)
	if r.code != 200 {
		t.Fatalf("状态码 = %d, 期望 200；%s", r.code, r.body)
	}
	wantEntries(t, zipEntries(t, r.body), "readme.md", "媒体/中文文件.txt")
}

func TestDownloadZipSelectedRejectsDotDot(t *testing.T) {
	root := makeFixture(t)
	s := newTestServer(t, testConfig(t, root, "-A"))

	// ".." 一律拒绝：它被 CleanURLPath 静默消解后会掩盖逃逸意图。
	for _, pick := range []string{"/docs/../notes.txt", "..%2Fnotes.txt", "../notes.txt"} {
		r := do(t, s, http.MethodGet, "/docs?zip&pick="+pick, nil, nil)
		if r.code != http.StatusBadRequest {
			t.Errorf("pick=%q 状态码 = %d, 期望 400", pick, r.code)
		}
	}
}

func TestDownloadZipSelectedAllInvalid(t *testing.T) {
	root := makeFixture(t)
	s := newTestServer(t, testConfig(t, root, "-A"))

	// 全部无效时应明确报错，而不是给出一个空 zip。
	for _, target := range []string{
		"/docs?zip&pick=/docs/nope.txt",
		"/docs?zip&pick=/etc/passwd",
		"/docs?zip&pick=/docs/sub-zero",
	} {
		r := do(t, s, http.MethodGet, target, nil, nil)
		if r.code != http.StatusBadRequest {
			t.Errorf("%s 状态码 = %d, 期望 400", target, r.code)
		}
	}

	// 但 pick 为空值只表示「没有选中项」，应当退回打包全部。
	r := do(t, s, http.MethodGet, "/docs?zip&pick=", nil, nil)
	if r.code != 200 {
		t.Fatalf("空 pick 状态码 = %d, 期望 200", r.code)
	}
	if got := r.head.Get("X-Gofs-Archive-Mode"); got != "all" {
		t.Errorf("空 pick 时 X-Gofs-Archive-Mode = %q, 期望 all", got)
	}
}

func TestDownloadZipSelectedPartialSkipIsReported(t *testing.T) {
	root := makeFixture(t)
	s := newTestServer(t, testConfig(t, root, "-A"))

	r := do(t, s, http.MethodGet,
		"/docs?zip&pick=/docs/readme.md&pick=/docs/nope.txt&pick=/docs/../notes.txt", nil, nil)
	if r.code != 200 {
		t.Fatalf("状态码 = %d, 期望 200；%s", r.code, r.body)
	}
	// 有效的照常打包。
	wantEntries(t, zipEntries(t, r.body), "readme.md")
	// 被跳过的两项要通过响应头如实告知。
	if got := r.head.Get("X-Gofs-Archive-Skipped"); got != "2" {
		t.Errorf("X-Gofs-Archive-Skipped = %q, 期望 2", got)
	}
}

func TestDownloadZipSelectedRespectsACL(t *testing.T) {
	root := makeFixture(t)
	// alice 只被授予 /docs 的读写权限。
	s := newTestServer(t, testConfig(t, root, "-A", "-a", "alice:pw@/docs:rw"))
	hdr := map[string]string{"Authorization": basicAuth("alice", "pw")}

	// 自己有权限的条目：正常打包。
	r := do(t, s, http.MethodGet, "/docs?zip&pick=/docs/readme.md", nil, hdr)
	if r.code != 200 {
		t.Fatalf("状态码 = %d, 期望 200；%s", r.code, r.body)
	}
	wantEntries(t, zipEntries(t, r.body), "readme.md")

	// 越权条目：必须被跳过，不能借打包把没权限的内容带出去。
	r = do(t, s, http.MethodGet, "/docs?zip&pick=/docs/readme.md&pick=/notes.txt", nil, hdr)
	if r.code != 200 {
		t.Fatalf("状态码 = %d, 期望 200", r.code)
	}
	wantEntries(t, zipEntries(t, r.body), "readme.md")
	if got := r.head.Get("X-Gofs-Archive-Skipped"); got != "1" {
		t.Errorf("X-Gofs-Archive-Skipped = %q, 期望 1", got)
	}
	// 全部越权时直接 400。
	r = do(t, s, http.MethodGet, "/docs?zip&pick=/notes.txt", nil, hdr)
	if r.code != http.StatusBadRequest {
		t.Errorf("全越权状态码 = %d, 期望 400", r.code)
	}

	// 未带凭据时对 /docs 也无权限。
	r = do(t, s, http.MethodGet, "/docs?zip&pick=/docs/readme.md", nil, nil)
	if r.code != http.StatusUnauthorized {
		t.Errorf("匿名状态码 = %d, 期望 401", r.code)
	}
}

func TestDownloadZipSelectedSkipsSymlink(t *testing.T) {
	root := makeFixture(t)
	if err := os.Symlink(filepath.Join(root, "notes.txt"), filepath.Join(root, "docs", "link.txt")); err != nil {
		t.Skipf("当前环境不支持符号链接: %v", err)
	}
	s := newTestServer(t, testConfig(t, root, "-A"))

	// 软链不该被打包（可能指向服务根之外）。
	r := do(t, s, http.MethodGet, "/docs?zip&pick=/docs/link.txt", nil, nil)
	if r.code != http.StatusBadRequest {
		t.Errorf("打包软链状态码 = %d, 期望 400", r.code)
	}
}

// ---------------------------------------------------------------------------
// 压缩包内容预览
// ---------------------------------------------------------------------------

func TestArchiveListAllFormats(t *testing.T) {
	root := makeFixture(t)
	s := newTestServer(t, testConfig(t, root, "-A"))

	cases := []struct {
		path       string
		wantFormat string
		wantEntry  string
	}{
		{"/sample.zip", "zip", "hello.txt"},
		{"/sample.tar.gz", "tar.gz", "a.txt"},
		{"/sample.tar", "tar", "plain.txt"},
		{"/notes.txt.gz", "gz", "notes.txt"},
	}
	for _, c := range cases {
		t.Run(c.wantFormat, func(t *testing.T) {
			r := do(t, s, http.MethodGet, "/__gofs__/extract?path="+c.path, nil, nil)
			if r.code != 200 {
				t.Fatalf("状态码 = %d, 期望 200；body=%s", r.code, r.body)
			}
			var d struct {
				Format  string `json:"format"`
				Total   int    `json:"total"`
				Entries []struct {
					Name string `json:"name"`
				} `json:"entries"`
			}
			if err := json.Unmarshal([]byte(r.body), &d); err != nil {
				t.Fatalf("JSON 解析失败: %v\n%s", err, r.body)
			}
			if d.Format != c.wantFormat {
				t.Errorf("format = %q, 期望 %q", d.Format, c.wantFormat)
			}
			found := false
			for _, e := range d.Entries {
				if e.Name == c.wantEntry {
					found = true
				}
			}
			if !found {
				t.Errorf("条目中未找到 %q，实际 = %+v", c.wantEntry, d.Entries)
			}
		})
	}
}

func TestArchiveEntryDownload(t *testing.T) {
	root := makeFixture(t)
	s := newTestServer(t, testConfig(t, root, "-A"))

	r := do(t, s, http.MethodGet, "/__gofs__/extract?path=/sample.zip&file=hello.txt", nil, nil)
	if r.code != 200 {
		t.Fatalf("状态码 = %d, 期望 200；%s", r.code, r.body)
	}
	if r.body != "zip hello\n" {
		t.Errorf("内容 = %q, 期望 %q", r.body, "zip hello\n")
	}
	if cd := r.head.Get("Content-Disposition"); !strings.Contains(cd, "hello.txt") {
		t.Errorf("Content-Disposition = %q", cd)
	}
}

func TestArchiveUnsupportedFormat(t *testing.T) {
	root := makeFixture(t)
	writeFile(t, filepath.Join(root, "fake.7z"), []byte("7z\xbc\xaf\x27\x1c\x00\x04"))
	s := newTestServer(t, testConfig(t, root, "-A"))

	r := do(t, s, http.MethodGet, "/__gofs__/extract?path=/fake.7z", nil, nil)
	if r.code != http.StatusUnsupportedMediaType {
		t.Errorf("状态码 = %d, 期望 415；body=%s", r.code, r.body)
	}
}

// ---------------------------------------------------------------------------
// 在线解压
// ---------------------------------------------------------------------------

// runExtract 发起解压请求并返回解析后的 SSE 事件。
func runExtract(t *testing.T, s *Server, reqBody string) ([]struct {
	name string
	data string
}, resp) {
	t.Helper()
	r := do(t, s, http.MethodPut, "/__gofs__/extract", strings.NewReader(reqBody),
		map[string]string{"Content-Type": "application/json"})
	return sseEvents(t, r.body), r
}

func TestExtractZip(t *testing.T) {
	root := makeFixture(t)
	s := newTestServer(t, testConfig(t, root, "-A"))

	events, r := runExtract(t, s, `{"path":"/sample.zip","dest":"/out"}`)
	if r.code != 200 {
		t.Fatalf("状态码 = %d, 期望 200；%s", r.code, r.body)
	}
	if ct := r.head.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Errorf("Content-Type = %q, 期望 text/event-stream", ct)
	}

	names := []string{}
	for _, e := range events {
		names = append(names, e.name)
	}
	if len(names) == 0 || names[0] != "start" {
		t.Fatalf("首个事件应为 start，实际 = %v", names)
	}
	if names[len(names)-1] != "done" {
		t.Fatalf("最后一个事件应为 done，实际 = %v（body=%s）", names, r.body)
	}

	// 校验落盘结果。
	for _, want := range []string{"hello.txt", filepath.Join("sub", "deep.txt")} {
		p := filepath.Join(root, "out", want)
		if _, err := os.Stat(p); err != nil {
			t.Errorf("期望文件不存在: %s (%v)", p, err)
		}
	}
	b, err := os.ReadFile(filepath.Join(root, "out", "hello.txt"))
	if err != nil || string(b) != "zip hello\n" {
		t.Errorf("解压内容 = %q, err=%v", b, err)
	}
}

func TestExtractTarGz(t *testing.T) {
	root := makeFixture(t)
	s := newTestServer(t, testConfig(t, root, "-A"))

	_, r := runExtract(t, s, `{"path":"/sample.tar.gz","dest":"/tgz"}`)
	if r.code != 200 || !strings.Contains(r.body, "event: done") {
		t.Fatalf("解压失败：code=%d body=%s", r.code, r.body)
	}
	for _, want := range []string{"a.txt", "中文名.txt", filepath.Join("deep", "b.txt")} {
		if _, err := os.Stat(filepath.Join(root, "tgz", want)); err != nil {
			t.Errorf("期望文件不存在: %s", want)
		}
	}
}

func TestExtractTar(t *testing.T) {
	root := makeFixture(t)
	s := newTestServer(t, testConfig(t, root, "-A"))

	_, r := runExtract(t, s, `{"path":"/sample.tar","dest":"/tarout"}`)
	if r.code != 200 || !strings.Contains(r.body, "event: done") {
		t.Fatalf("解压失败：code=%d body=%s", r.code, r.body)
	}
	if _, err := os.Stat(filepath.Join(root, "tarout", "plain.txt")); err != nil {
		t.Errorf("期望文件不存在: plain.txt")
	}
}

func TestExtractGz(t *testing.T) {
	root := makeFixture(t)
	s := newTestServer(t, testConfig(t, root, "-A"))

	_, r := runExtract(t, s, `{"path":"/notes.txt.gz","dest":"/gzout"}`)
	if r.code != 200 || !strings.Contains(r.body, "event: done") {
		t.Fatalf("解压失败：code=%d body=%s", r.code, r.body)
	}
	b, err := os.ReadFile(filepath.Join(root, "gzout", "notes.txt"))
	if err != nil {
		t.Fatalf("期望文件不存在: %v", err)
	}
	if string(b) != "gzip payload\n" {
		t.Errorf("解压内容 = %q", b)
	}
}

func TestExtractDefaultDest(t *testing.T) {
	root := makeFixture(t)
	s := newTestServer(t, testConfig(t, root, "-A"))

	// 不指定 dest，应落到同名目录 /sample
	_, r := runExtract(t, s, `{"path":"/sample.zip"}`)
	if r.code != 200 || !strings.Contains(r.body, "event: done") {
		t.Fatalf("解压失败：code=%d body=%s", r.code, r.body)
	}
	if _, err := os.Stat(filepath.Join(root, "sample", "hello.txt")); err != nil {
		t.Errorf("默认目标目录解压失败: %v", err)
	}
}

func TestExtractDisabled(t *testing.T) {
	root := makeFixture(t)
	s := newTestServer(t, testConfig(t, root, "--allow-upload"))

	r := do(t, s, http.MethodPut, "/__gofs__/extract",
		strings.NewReader(`{"path":"/sample.zip"}`),
		map[string]string{"Content-Type": "application/json"})
	if r.code != http.StatusForbidden {
		t.Errorf("状态码 = %d, 期望 403", r.code)
	}
}

func TestExtractOverwriteGuard(t *testing.T) {
	root := makeFixture(t)
	s := newTestServer(t, testConfig(t, root, "-A"))

	// 先放一个非空目录占位。
	writeFile(t, filepath.Join(root, "out", "existing.txt"), []byte("x"))

	_, r := runExtract(t, s, `{"path":"/sample.zip","dest":"/out"}`)
	if r.code != http.StatusConflict {
		t.Fatalf("状态码 = %d, 期望 409；%s", r.code, r.body)
	}

	// 显式覆盖应当成功。
	_, r2 := runExtract(t, s, `{"path":"/sample.zip","dest":"/out","overwrite":true}`)
	if r2.code != 200 || !strings.Contains(r2.body, "event: done") {
		t.Errorf("覆盖解压失败：code=%d body=%s", r2.code, r2.body)
	}
}

// ---------------------------------------------------------------------------
// 解压安全
// ---------------------------------------------------------------------------

func TestZipSlipBlocked(t *testing.T) {
	root := makeFixture(t)

	// 构造带路径穿越的 zip 与绝对路径条目。
	zbuf := &bytes.Buffer{}
	zw := zip.NewWriter(zbuf)
	for name, data := range map[string]string{
		"../../escaped.txt":             "should not escape\n",
		"/tmp/gofs-absolute-escape.txt": "absolute should not escape\n",
		"safe.txt":                      "ok\n",
	} {
		f, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.Write([]byte(data)); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(root, "evil.zip"), zbuf.Bytes())

	s := newTestServer(t, testConfig(t, root, "-A"))
	events, r := runExtract(t, s, `{"path":"/evil.zip","dest":"/safe-area"}`)
	if r.code != 200 {
		t.Fatalf("状态码 = %d", r.code)
	}

	// 合法条目应解出。
	if _, err := os.Stat(filepath.Join(root, "safe-area", "safe.txt")); err != nil {
		t.Errorf("合法条目未解出: %v", err)
	}
	// 含 .. 的条目必须被显式拒绝，而不是被 path.Clean 静默改写成合法名字后落盘。
	if _, err := os.Stat(filepath.Join(root, "safe-area", "escaped.txt")); err == nil {
		t.Errorf("含 .. 的条目被静默改写后落盘了，应当被拒绝")
	}
	// 逃逸文件不得出现在根目录之外。
	if _, err := os.Stat(filepath.Join(root, "escaped.txt")); err == nil {
		t.Errorf("Zip Slip 未被拦截：%s 被写到服务根", filepath.Join(root, "escaped.txt"))
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(root), "escaped.txt")); err == nil {
		t.Errorf("Zip Slip 未被拦截：文件逃逸到了服务根之外")
	}
	if _, err := os.Stat("/tmp/gofs-absolute-escape.txt"); err == nil {
		t.Errorf("绝对路径条目未被拦截")
	}

	// 两种越界条目都应在 warnings 中留下记录。
	var done string
	for _, e := range events {
		if e.name == "done" {
			done = e.data
		}
	}
	if !strings.Contains(done, "..") {
		t.Errorf("期望 warnings 中记录 .. 路径段，实际 = %s", done)
	}
	if !strings.Contains(done, "绝对路径") {
		t.Errorf("期望 warnings 中记录绝对路径，实际 = %s", done)
	}
}

func TestExtractAbortLeavesNoPartialFile(t *testing.T) {
	root := makeFixture(t)

	zbuf := &bytes.Buffer{}
	zw := zip.NewWriter(zbuf)
	f, _ := zw.Create("bomb.bin")
	// 8 MiB 零字节，压缩比极高，必然触发保护。
	_, _ = f.Write(make([]byte, 8<<20))
	_ = zw.Close()
	writeFile(t, filepath.Join(root, "bomb.zip"), zbuf.Bytes())

	s := newTestServer(t, testConfig(t, root, "-A", "--extract-max-ratio", "50"))
	_, r := runExtract(t, s, `{"path":"/bomb.zip","dest":"/abort"}`)
	if r.code != 200 {
		t.Fatalf("状态码 = %d", r.code)
	}
	if !strings.Contains(r.body, "event: error") {
		t.Fatalf("期望触发压缩比保护，实际 = %s", r.body)
	}
	// 中止后不得留下半截文件。
	if st, err := os.Stat(filepath.Join(root, "abort", "bomb.bin")); err == nil {
		t.Errorf("中止解压后残留了残缺文件 bomb.bin（%d 字节）", st.Size())
	}
}

func TestTarSymlinkSkipped(t *testing.T) {
	root := makeFixture(t)

	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gw)
	// 正常文件
	if err := tw.WriteHeader(&tar.Header{Name: "ok.txt", Mode: 0o644, Size: 3, Typeflag: tar.TypeReg}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write([]byte("ok\n")); err != nil {
		t.Fatal(err)
	}
	// 指向 /etc/passwd 的软链接
	if err := tw.WriteHeader(&tar.Header{
		Name: "passwd-link", Mode: 0o777, Typeflag: tar.TypeSymlink, Linkname: "/etc/passwd",
	}); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gw.Close(); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(root, "link.tar.gz"), buf.Bytes())

	s := newTestServer(t, testConfig(t, root, "-A"))
	_, r := runExtract(t, s, `{"path":"/link.tar.gz","dest":"/linkout"}`)
	if r.code != 200 || !strings.Contains(r.body, "event: done") {
		t.Fatalf("解压失败: %s", r.body)
	}
	if _, err := os.Stat(filepath.Join(root, "linkout", "ok.txt")); err != nil {
		t.Errorf("正常文件未解出: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(root, "linkout", "passwd-link")); err == nil {
		t.Errorf("软链接条目未被跳过")
	}
	if !strings.Contains(r.body, "已跳过链接条目") {
		t.Errorf("期望出现跳过链接的告警，实际 = %s", r.body)
	}
}

func TestExtractQuotaFiles(t *testing.T) {
	root := makeFixture(t)

	zbuf := &bytes.Buffer{}
	zw := zip.NewWriter(zbuf)
	for i := 0; i < 20; i++ {
		f, _ := zw.Create(fmt.Sprintf("f%02d.txt", i))
		_, _ = f.Write([]byte("x"))
	}
	_ = zw.Close()
	writeFile(t, filepath.Join(root, "many.zip"), zbuf.Bytes())

	s := newTestServer(t, testConfig(t, root, "-A", "--extract-max-files", "5"))
	_, r := runExtract(t, s, `{"path":"/many.zip","dest":"/quota"}`)
	if r.code != 200 {
		t.Fatalf("状态码 = %d", r.code)
	}
	if !strings.Contains(r.body, "event: error") {
		t.Errorf("期望触发条目数上限报错，实际 = %s", r.body)
	}
	if !strings.Contains(r.body, "条目数超过上限 5") {
		t.Errorf("错误信息未提及上限，实际 = %s", r.body)
	}
}

func TestExtractQuotaBytes(t *testing.T) {
	root := makeFixture(t)

	zbuf := &bytes.Buffer{}
	zw := zip.NewWriter(zbuf)
	f, _ := zw.Create("big.bin")
	_, _ = f.Write(bytes.Repeat([]byte("A"), 4096))
	_ = zw.Close()
	writeFile(t, filepath.Join(root, "sizeable.zip"), zbuf.Bytes())

	s := newTestServer(t, testConfig(t, root, "-A", "--extract-max-total", "1024"))
	_, r := runExtract(t, s, `{"path":"/sizeable.zip","dest":"/quota2"}`)
	if r.code != 200 {
		t.Fatalf("状态码 = %d", r.code)
	}
	if !strings.Contains(r.body, "event: error") {
		t.Errorf("期望触发总量上限报错，实际 = %s", r.body)
	}
}

func TestExtractQuotaRatio(t *testing.T) {
	root := makeFixture(t)

	zbuf := &bytes.Buffer{}
	zw := zip.NewWriter(zbuf)
	f, _ := zw.Create("bomb.bin")
	// 8 MiB 的零字节，压缩后体积极小，压缩比远超阈值。
	_, _ = f.Write(make([]byte, 8<<20))
	_ = zw.Close()
	writeFile(t, filepath.Join(root, "bomb.zip"), zbuf.Bytes())

	s := newTestServer(t, testConfig(t, root, "-A", "--extract-max-ratio", "50"))
	_, r := runExtract(t, s, `{"path":"/bomb.zip","dest":"/quota3"}`)
	if r.code != 200 {
		t.Fatalf("状态码 = %d", r.code)
	}
	if !strings.Contains(r.body, "event: error") || !strings.Contains(r.body, "解压炸弹") {
		t.Errorf("期望触发压缩比保护，实际 = %s", r.body)
	}
}

func TestExtractDestOutsideRoot(t *testing.T) {
	root := makeFixture(t)
	s := newTestServer(t, testConfig(t, root, "-A"))

	// dest 中含 .. 段必须被显式拒绝，而不是被静默规范化到根目录内。
	_, r := runExtract(t, s, `{"path":"/sample.zip","dest":"/../../tmp/escape-here"}`)
	if r.code != http.StatusBadRequest {
		t.Errorf("状态码 = %d, 期望 400；body=%s", r.code, r.body)
	}
	if _, err := os.Stat("/tmp/escape-here"); err == nil {
		t.Errorf("文件被解压到了服务根之外")
	}
	// 源路径同样不允许 ..
	_, r2 := runExtract(t, s, `{"path":"/../../etc/passwd","dest":"/x"}`)
	if r2.code != http.StatusBadRequest {
		t.Errorf("源路径含 .. 时状态码 = %d, 期望 400", r2.code)
	}
}

func TestExtractUnsupported(t *testing.T) {
	root := makeFixture(t)
	writeFile(t, filepath.Join(root, "noext.bin"), []byte("just plain text, not an archive"))
	s := newTestServer(t, testConfig(t, root, "-A"))

	_, r := runExtract(t, s, `{"path":"/noext.bin","dest":"/x"}`)
	if r.code != http.StatusUnsupportedMediaType {
		t.Errorf("状态码 = %d, 期望 415；body=%s", r.code, r.body)
	}
}

// ---------------------------------------------------------------------------
// 写操作
// ---------------------------------------------------------------------------

func TestUploadPut(t *testing.T) {
	root := makeFixture(t)
	s := newTestServer(t, testConfig(t, root, "-A"))

	r := do(t, s, http.MethodPut, "/upload/new.txt", strings.NewReader("uploaded body"),
		nil)
	if r.code != http.StatusCreated {
		t.Fatalf("状态码 = %d, 期望 201；%s", r.code, r.body)
	}
	b, err := os.ReadFile(filepath.Join(root, "upload", "new.txt"))
	if err != nil || string(b) != "uploaded body" {
		t.Errorf("上传内容 = %q, err = %v", b, err)
	}
}

func TestUploadAppend(t *testing.T) {
	root := makeFixture(t)
	s := newTestServer(t, testConfig(t, root, "-A"))

	do(t, s, http.MethodPut, "/append.txt", strings.NewReader("part1-"), nil)
	r := do(t, s, http.MethodPut, "/append.txt", strings.NewReader("part2"),
		map[string]string{"X-Update-Range": "append"})
	if r.code != http.StatusCreated {
		t.Fatalf("状态码 = %d", r.code)
	}
	b, _ := os.ReadFile(filepath.Join(root, "append.txt"))
	if string(b) != "part1-part2" {
		t.Errorf("追加结果 = %q, 期望 part1-part2", b)
	}
	if r.head.Get("X-Gofs-Offset") != "5" {
		t.Errorf("X-Gofs-Offset = %q, 期望 5", r.head.Get("X-Gofs-Offset"))
	}
}

func TestUploadDisabled(t *testing.T) {
	root := makeFixture(t)
	s := newTestServer(t, testConfig(t, root))

	r := do(t, s, http.MethodPut, "/x.txt", strings.NewReader("nope"), nil)
	if r.code != http.StatusForbidden {
		t.Errorf("状态码 = %d, 期望 403", r.code)
	}
}

func TestMkcolMoveDelete(t *testing.T) {
	root := makeFixture(t)
	s := newTestServer(t, testConfig(t, root, "-A"))

	// 建目录
	if r := do(t, s, MethodMkcol, "/newdir", nil, nil); r.code != http.StatusCreated {
		t.Fatalf("MKCOL 状态码 = %d", r.code)
	}
	// 重复建目录应冲突
	if r := do(t, s, MethodMkcol, "/newdir", nil, nil); r.code != http.StatusConflict {
		t.Errorf("重复 MKCOL 状态码 = %d, 期望 409", r.code)
	}
	// 移动
	r := do(t, s, MethodMove, "/notes.txt", nil, map[string]string{
		"Destination": "http://127.0.0.1:5599/newdir/renamed.txt",
	})
	if r.code != http.StatusCreated {
		t.Fatalf("MOVE 状态码 = %d；%s", r.code, r.body)
	}
	if _, err := os.Stat(filepath.Join(root, "newdir", "renamed.txt")); err != nil {
		t.Errorf("移动后文件不存在: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "notes.txt")); err == nil {
		t.Errorf("移动后原文件仍存在")
	}
	// 删除空目录
	if r := do(t, s, http.MethodDelete, "/newdir?recursive", nil, nil); r.code != http.StatusNoContent {
		t.Errorf("DELETE 状态码 = %d", r.code)
	}
	if _, err := os.Stat(filepath.Join(root, "newdir")); err == nil {
		t.Errorf("目录未被删除")
	}
}

func TestDeleteNonEmptyRequiresRecursive(t *testing.T) {
	root := makeFixture(t)
	s := newTestServer(t, testConfig(t, root, "-A"))

	r := do(t, s, http.MethodDelete, "/docs", nil, nil)
	if r.code != http.StatusConflict {
		t.Errorf("状态码 = %d, 期望 409（非空目录需 recursive）", r.code)
	}
	if r := do(t, s, http.MethodDelete, "/docs?recursive", nil, nil); r.code != http.StatusNoContent {
		t.Errorf("带 recursive 的删除状态码 = %d", r.code)
	}
}

func TestDeleteRootRefused(t *testing.T) {
	root := makeFixture(t)
	s := newTestServer(t, testConfig(t, root, "-A"))

	r := do(t, s, http.MethodDelete, "/?recursive", nil, nil)
	if r.code != http.StatusForbidden {
		t.Errorf("状态码 = %d, 期望 403", r.code)
	}
	if _, err := os.Stat(filepath.Join(root, "notes.txt")); err != nil {
		t.Errorf("根目录内容被删除了！")
	}
}

// ---------------------------------------------------------------------------
// 路径安全
// ---------------------------------------------------------------------------

func TestPathTraversal(t *testing.T) {
	root := makeFixture(t)
	s := newTestServer(t, testConfig(t, root, "-A"))

	cases := []string{
		"/../etc/passwd",
		"/..%2f..%2fetc%2fpasswd",
		"/docs/../../etc/passwd",
		"/docs/%2e%2e/%2e%2e/etc/passwd",
	}
	for _, c := range cases {
		r := do(t, s, http.MethodGet, c, nil, nil)
		if r.code == 200 && strings.Contains(r.body, "root:") {
			t.Errorf("路径穿越未被拦截: %s", c)
		}
	}
}

func TestSymlinkEscapeBlocked(t *testing.T) {
	root := makeFixture(t)
	outside := t.TempDir()
	writeFile(t, filepath.Join(outside, "secret.txt"), []byte("top secret\n"))
	if err := os.Symlink(filepath.Join(outside, "secret.txt"), filepath.Join(root, "link.txt")); err != nil {
		t.Skipf("当前环境不支持创建软链接: %v", err)
	}

	s := newTestServer(t, testConfig(t, root, "-A"))
	r := do(t, s, http.MethodGet, "/link.txt", nil, nil)
	if r.code == 200 && strings.Contains(r.body, "top secret") {
		t.Errorf("软链接逃逸未被拦截")
	}
}

// ---------------------------------------------------------------------------
// 鉴权
// ---------------------------------------------------------------------------

func TestAuthRequired(t *testing.T) {
	root := makeFixture(t)
	s := newTestServer(t, testConfig(t, root, "-a", "admin:secret@/:rw"))

	r := do(t, s, http.MethodGet, "/?json", nil, nil)
	if r.code != http.StatusUnauthorized {
		t.Fatalf("匿名访问状态码 = %d, 期望 401", r.code)
	}
	if !strings.Contains(r.head.Get("WWW-Authenticate"), "Basic") {
		t.Errorf("缺少 WWW-Authenticate 挑战头")
	}
}

func TestAuthCorrectCredentials(t *testing.T) {
	root := makeFixture(t)
	s := newTestServer(t, testConfig(t, root, "-a", "admin:secret@/:rw"))

	r := do(t, s, http.MethodGet, "/?json", nil, map[string]string{
		"Authorization": basicAuth("admin", "secret"),
	})
	if r.code != 200 {
		t.Fatalf("状态码 = %d, 期望 200；%s", r.code, r.body)
	}
}

func TestAuthWrongPassword(t *testing.T) {
	root := makeFixture(t)
	s := newTestServer(t, testConfig(t, root, "-a", "admin:secret@/:rw"))

	r := do(t, s, http.MethodGet, "/?json", nil, map[string]string{
		"Authorization": basicAuth("admin", "wrong"),
	})
	if r.code != http.StatusUnauthorized {
		t.Errorf("状态码 = %d, 期望 401", r.code)
	}
}

func TestAuthReadOnlyUser(t *testing.T) {
	root := makeFixture(t)
	s := newTestServer(t, testConfig(t, root, "-A",
		"-a", "guest:guest@/", "-a", "admin:admin@/:rw"))

	hdr := map[string]string{"Authorization": basicAuth("guest", "guest")}

	if r := do(t, s, http.MethodGet, "/?json", nil, hdr); r.code != 200 {
		t.Errorf("只读用户读取应成功，实际 = %d", r.code)
	}
	if r := do(t, s, http.MethodPut, "/nope.txt", strings.NewReader("x"), hdr); r.code != http.StatusForbidden {
		t.Errorf("只读用户写入状态码 = %d, 期望 403", r.code)
	}
	if r := do(t, s, http.MethodPut, "/ok.txt", strings.NewReader("x"),
		map[string]string{"Authorization": basicAuth("admin", "admin")}); r.code != http.StatusCreated {
		t.Errorf("管理员写入状态码 = %d, 期望 201", r.code)
	}
}

func TestAuthPathScoped(t *testing.T) {
	root := makeFixture(t)
	s := newTestServer(t, testConfig(t, root, "-A", "-a", "u:p@/docs:rw"))

	hdr := map[string]string{"Authorization": basicAuth("u", "p")}

	// /docs 下可写
	if r := do(t, s, http.MethodPut, "/docs/a.txt", strings.NewReader("x"), hdr); r.code != http.StatusCreated {
		t.Errorf("/docs 写入状态码 = %d, 期望 201", r.code)
	}
	// 其他路径无权
	r := do(t, s, http.MethodGet, "/?json", nil, hdr)
	if r.code != http.StatusForbidden && r.code != http.StatusUnauthorized {
		t.Errorf("越权访问状态码 = %d, 期望 403/401", r.code)
	}
}

func TestAuthAnonymousRule(t *testing.T) {
	root := makeFixture(t)
	s := newTestServer(t, testConfig(t, root, "-a", "@/:ro"))

	r := do(t, s, http.MethodGet, "/?json", nil, nil)
	if r.code != 200 {
		t.Fatalf("匿名只读访问状态码 = %d, 期望 200", r.code)
	}
	if r := do(t, s, http.MethodPut, "/x.txt", strings.NewReader("x"), nil); r.code != http.StatusForbidden {
		t.Errorf("匿名写入状态码 = %d, 期望 403", r.code)
	}
}

func basicAuth(user, pass string) string {
	req, _ := http.NewRequest(http.MethodGet, "/", nil)
	req.SetBasicAuth(user, pass)
	return req.Header.Get("Authorization")
}

// ---------------------------------------------------------------------------
// 其它
// ---------------------------------------------------------------------------

func TestHealth(t *testing.T) {
	root := makeFixture(t)
	s := newTestServer(t, testConfig(t, root))

	r := do(t, s, http.MethodGet, "/__gofs__/health", nil, nil)
	if r.code != 200 {
		t.Fatalf("状态码 = %d", r.code)
	}
	var d map[string]any
	if err := json.Unmarshal([]byte(r.body), &d); err != nil {
		t.Fatal(err)
	}
	if d["status"] != "ok" {
		t.Errorf("status = %v", d["status"])
	}
}

func TestHTMLPageRenders(t *testing.T) {
	root := makeFixture(t)
	s := newTestServer(t, testConfig(t, root, "-A"))

	r := do(t, s, http.MethodGet, "/", nil, nil)
	if r.code != 200 {
		t.Fatalf("状态码 = %d", r.code)
	}
	if ct := r.head.Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("Content-Type = %q", ct)
	}
	if !strings.Contains(r.body, "window.__GOFS__") && !strings.Contains(r.body, "gofs-data") {
		t.Errorf("页面未注入初始数据")
	}
	// 占位符必须被替换干净。
	if strings.Contains(r.body, "__GOFS_DATA__") {
		t.Errorf("__GOFS_DATA__ 占位符未被替换")
	}
	if strings.Contains(r.body, "__GOFS_ASSETS__") {
		t.Errorf("__GOFS_ASSETS__ 占位符未被替换")
	}
	// 页面里的 JSON 必须可解析。
	start := strings.Index(r.body, `<script id="gofs-data" type="application/json">`)
	if start < 0 {
		t.Fatalf("未找到数据脚本块")
	}
	start += len(`<script id="gofs-data" type="application/json">`)
	end := strings.Index(r.body[start:], "</script>")
	raw := r.body[start : start+end]
	var data struct {
		Path    string `json:"path"`
		Listing struct {
			Total int `json:"total"`
		} `json:"listing"`
		Perms map[string]bool `json:"perms"`
	}
	if err := json.Unmarshal([]byte(raw), &data); err != nil {
		t.Fatalf("注入的数据不是合法 JSON: %v\n%s", err, raw)
	}
	if data.Listing.Total != 7 {
		t.Errorf("注入的条目数 = %d, 期望 7", data.Listing.Total)
	}
	if !data.Perms["extract"] {
		t.Errorf("perms.extract 应为 true")
	}
}

func TestAssetsServed(t *testing.T) {
	root := makeFixture(t)
	s := newTestServer(t, testConfig(t, root, "-A"))

	for _, name := range []string{"app.js", "style.css"} {
		r := do(t, s, http.MethodGet, "/__gofs__/assets/"+name, nil, nil)
		if r.code != 200 {
			t.Errorf("%s 状态码 = %d", name, r.code)
		}
		if len(r.body) < 100 {
			t.Errorf("%s 内容过短", name)
		}
	}
	// 目录穿越尝试
	if r := do(t, s, http.MethodGet, "/__gofs__/assets/../server.go", nil, nil); r.code == 200 {
		t.Errorf("资源目录穿越未被拦截")
	}
}

func TestHiddenPaths(t *testing.T) {
	root := makeFixture(t)
	writeFile(t, filepath.Join(root, ".secret"), []byte("x"))
	writeFile(t, filepath.Join(root, "debug.log"), []byte("x"))

	s := newTestServer(t, testConfig(t, root, "-A", "--hidden", ".*,*.log"))

	r := do(t, s, http.MethodGet, "/?simple", nil, nil)
	if strings.Contains(r.body, ".secret") {
		t.Errorf("隐藏文件未生效：\n%s", r.body)
	}
	if strings.Contains(r.body, "debug.log") {
		t.Errorf("隐藏后缀未生效：\n%s", r.body)
	}
}

func TestPathPrefix(t *testing.T) {
	root := makeFixture(t)
	s := newTestServer(t, testConfig(t, root, "-A", "--path-prefix", "/gofs"))

	if r := do(t, s, http.MethodGet, "/gofs/?json", nil, nil); r.code != 200 {
		t.Errorf("带前缀访问状态码 = %d, 期望 200", r.code)
	}
	if r := do(t, s, http.MethodGet, "/?json", nil, nil); r.code != http.StatusNotFound {
		t.Errorf("无前缀访问状态码 = %d, 期望 404", r.code)
	}
	// 页面资源前缀应带上 path-prefix
	r := do(t, s, http.MethodGet, "/gofs/", nil, nil)
	if !strings.Contains(r.body, "/gofs/__gofs__/assets/") {
		t.Errorf("资源前缀未跟随 path-prefix")
	}
}

// ---------------------------------------------------------------------------
// 上传按年月日归档
// ---------------------------------------------------------------------------

// datedOnDisk 拼接出文件在磁盘上的预期位置。
func datedOnDisk(root, urlPath, layout string) string {
	return filepath.Join(root, filepath.FromSlash(
		strings.TrimPrefix(path.Join(path.Dir(urlPath), todayDir(layout), path.Base(urlPath)), "/")))
}

func TestUploadDatedRootFile(t *testing.T) {
	root := makeFixture(t)
	s := newTestServer(t, testConfigDated(t, root, "2006/01/02", "-A"))

	r := do(t, s, http.MethodPut, "/pic.jpg", strings.NewReader("image-bytes"), nil)
	if r.code != http.StatusCreated {
		t.Fatalf("状态码 = %d, 期望 201；%s", r.code, r.body)
	}

	wantURL := "/" + todayDir("2006/01/02") + "/pic.jpg"
	if got := r.head.Get("X-Gofs-Path"); got != wantURL {
		t.Errorf("X-Gofs-Path = %q, 期望 %q", got, wantURL)
	}
	b, err := os.ReadFile(datedOnDisk(root, "/pic.jpg", "2006/01/02"))
	if err != nil {
		t.Fatalf("文件未落到日期目录: %v", err)
	}
	if string(b) != "image-bytes" {
		t.Errorf("内容 = %q", b)
	}
	// 原路径不应存在，否则说明归档没生效。
	if _, err := os.Stat(filepath.Join(root, "pic.jpg")); err == nil {
		t.Errorf("文件同时落在了原路径，归档未生效")
	}
	// 三级目录必须逐级真的被建出来（年 → 年/月 → 年/月/日）。
	acc := ""
	for _, seg := range strings.Split(todayDir("2006/01/02"), "/") {
		acc = filepath.Join(acc, seg)
		st, err := os.Stat(filepath.Join(root, acc))
		if err != nil || !st.IsDir() {
			t.Errorf("日期目录 %q 未创建: %v", acc, err)
		}
	}
}

func TestUploadDatedSubdirNotArchived(t *testing.T) {
	root := makeFixture(t)
	s := newTestServer(t, testConfigDated(t, root, "2006/01/02", "-A"))

	// 归档只在「直接传到根目录」时生效；传进子目录就原地放 ——
	// 子目录本身已经是有意义的分层，再套一层年月日只会越陷越深。
	r := do(t, s, http.MethodPut, "/sub/deep/b.txt", strings.NewReader("x"), nil)
	if r.code != http.StatusCreated {
		t.Fatalf("状态码 = %d", r.code)
	}
	if got := r.head.Get("X-Gofs-Path"); got != "/sub/deep/b.txt" {
		t.Errorf("X-Gofs-Path = %q, 期望 /sub/deep/b.txt（子目录不归档）", got)
	}
	if _, err := os.Stat(filepath.Join(root, "sub", "deep", "b.txt")); err != nil {
		t.Errorf("文件未落在原路径: %v", err)
	}
	// 不能凭空造出日期目录
	if _, err := os.Stat(filepath.Join(root, "sub", "deep", filepath.FromSlash(todayDir("2006/01/02")))); err == nil {
		t.Errorf("子目录上传不应创建日期目录")
	}
}

func TestUploadDatedForceParam(t *testing.T) {
	root := makeFixture(t)
	s := newTestServer(t, testConfigDated(t, root, "2006/01/02", "-A"))

	// ?dated=1 显式要求归档：即使在子目录里也按年月日建三级目录。
	// 这条保留了「归档能力」本身，只是默认不再作用于子目录。
	r := do(t, s, http.MethodPut, "/sub/a.txt?dated=1", strings.NewReader("x"), nil)
	if r.code != http.StatusCreated {
		t.Fatalf("状态码 = %d；%s", r.code, r.body)
	}
	wantURL := "/sub/" + todayDir("2006/01/02") + "/a.txt"
	if got := r.head.Get("X-Gofs-Path"); got != wantURL {
		t.Errorf("X-Gofs-Path = %q, 期望 %q", got, wantURL)
	}
	if _, err := os.Stat(datedOnDisk(root, "/sub/a.txt", "2006/01/02")); err != nil {
		t.Errorf("dated=1 时文件未落到日期目录: %v", err)
	}
}

func TestUploadDatedCustomLayout(t *testing.T) {
	root := makeFixture(t)
	// 只到「年-月」一级，验证布局字面量可自定义。
	s := newTestServer(t, testConfigDated(t, root, "2006-01", "-A"))

	r := do(t, s, http.MethodPut, "/x.txt", strings.NewReader("x"), nil)
	if r.code != http.StatusCreated {
		t.Fatalf("状态码 = %d", r.code)
	}
	wantURL := "/" + todayDir("2006-01") + "/x.txt"
	if got := r.head.Get("X-Gofs-Path"); got != wantURL {
		t.Errorf("X-Gofs-Path = %q, 期望 %q", got, wantURL)
	}
	if _, err := os.Stat(datedOnDisk(root, "/x.txt", "2006-01")); err != nil {
		t.Errorf("文件未落到月份目录: %v", err)
	}
}

func TestUploadDatedDisabled(t *testing.T) {
	root := makeFixture(t)
	// testConfig 默认带 --no-upload-dated
	s := newTestServer(t, testConfig(t, root, "-A"))

	r := do(t, s, http.MethodPut, "/plain.txt", strings.NewReader("x"), nil)
	if r.code != http.StatusCreated {
		t.Fatalf("状态码 = %d", r.code)
	}
	if got := r.head.Get("X-Gofs-Path"); got != "/plain.txt" {
		t.Errorf("X-Gofs-Path = %q, 期望 /plain.txt", got)
	}
	if _, err := os.Stat(filepath.Join(root, "plain.txt")); err != nil {
		t.Errorf("关闭归档后文件应落在原路径: %v", err)
	}
	// 不应创建任何日期目录
	if _, err := os.Stat(filepath.Join(root, todayDir("2006/01/02"))); err == nil {
		t.Errorf("关闭归档后仍创建了日期目录")
	}
}

func TestUploadDatedAppendKeepsSameDir(t *testing.T) {
	root := makeFixture(t)
	s := newTestServer(t, testConfigDated(t, root, "2006/01/02", "-A"))

	do(t, s, http.MethodPut, "/log.txt", strings.NewReader("part1-"), nil)
	r := do(t, s, http.MethodPut, "/log.txt", strings.NewReader("part2"),
		map[string]string{"X-Update-Range": "append"})
	if r.code != http.StatusCreated {
		t.Fatalf("状态码 = %d；%s", r.code, r.body)
	}
	// 两次请求必须命中同一个日期目录，否则续传会把文件写散。
	b, err := os.ReadFile(datedOnDisk(root, "/log.txt", "2006/01/02"))
	if err != nil {
		t.Fatalf("文件不存在: %v", err)
	}
	if string(b) != "part1-part2" {
		t.Errorf("追加结果 = %q, 期望 part1-part2", b)
	}
}

func TestUploadDatedFormPost(t *testing.T) {
	root := makeFixture(t)
	s := newTestServer(t, testConfigDated(t, root, "2006/01/02", "-A"))

	// 传到根目录（不带 path 字段）→ 归档。
	form := func(fields map[string]string) (*bytes.Buffer, string) {
		var buf bytes.Buffer
		mw := multipart.NewWriter(&buf)
		fw, err := mw.CreateFormFile("file", "report.pdf")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := fw.Write([]byte("pdf")); err != nil {
			t.Fatal(err)
		}
		for k, v := range fields {
			if err := mw.WriteField(k, v); err != nil {
				t.Fatal(err)
			}
		}
		if err := mw.Close(); err != nil {
			t.Fatal(err)
		}
		return &buf, mw.FormDataContentType()
	}
	post := func(fields map[string]string) string {
		buf, ct := form(fields)
		r := do(t, s, http.MethodPost, "/", buf, map[string]string{"Content-Type": ct})
		if r.code != 200 {
			t.Fatalf("表单上传状态码 = %d；%s", r.code, r.body)
		}
		var d struct {
			Dest string `json:"dest"`
		}
		if err := json.Unmarshal([]byte(r.body), &d); err != nil {
			t.Fatalf("JSON 解析失败: %v\n%s", err, r.body)
		}
		return d.Dest
	}

	wantDir := "/" + todayDir("2006/01/02")
	if got := post(nil); got != wantDir {
		t.Errorf("dest = %q, 期望 %q", got, wantDir)
	}
	target := filepath.Join(root, filepath.FromSlash(todayDir("2006/01/02")), "report.pdf")
	if _, err := os.Stat(target); err != nil {
		t.Errorf("表单上传未归档: %v", err)
	}

	// 表单显式指定子路径 → 就是「传到子目录」，不归档（与 PUT 同一套规则）。
	if got := post(map[string]string{"path": "docs"}); got != "/docs" {
		t.Errorf("dest = %q, 期望 /docs（子目录不归档）", got)
	}
	if _, err := os.Stat(filepath.Join(root, "docs", "report.pdf")); err != nil {
		t.Errorf("表单指定子路径时文件未落在原目录: %v", err)
	}

	// 需要归档时显式传 dated=1（脚本/客户端想自己指定落点分层的情况）。
	if got := post(map[string]string{"path": "docs", "dated": "1"}); got != "/docs/"+todayDir("2006/01/02") {
		t.Errorf("dest = %q, 期望 /docs/%s", got, todayDir("2006/01/02"))
	}
}

func TestUploadDatedSkipParam(t *testing.T) {
	root := makeFixture(t)
	s := newTestServer(t, testConfigDated(t, root, "2006/01/02", "-A"))

	r := do(t, s, http.MethodPut, "/direct.txt?dated=0", strings.NewReader("x"), nil)
	if r.code != http.StatusCreated {
		t.Fatalf("状态码 = %d；%s", r.code, r.body)
	}
	if got := r.head.Get("X-Gofs-Path"); got != "/direct.txt" {
		t.Errorf("X-Gofs-Path = %q, 期望 /direct.txt（dated=0 应跳过归档）", got)
	}
	if _, err := os.Stat(filepath.Join(root, "direct.txt")); err != nil {
		t.Errorf("跳过归档后文件应落在原路径: %v", err)
	}
}

func TestUploadDatedPathHeaderIsASCII(t *testing.T) {
	root := makeFixture(t)
	s := newTestServer(t, testConfigDated(t, root, "2006/01/02", "-A"))

	r := do(t, s, http.MethodPut, "/报告.pdf", strings.NewReader("x"), nil)
	if r.code != http.StatusCreated {
		t.Fatalf("状态码 = %d；%s", r.code, r.body)
	}
	got := r.head.Get("X-Gofs-Path")
	want := "/" + todayDir("2006/01/02") + "/" + url.PathEscape("报告.pdf")
	if got != want {
		t.Errorf("X-Gofs-Path = %q, 期望 %q", got, want)
	}
	// HTTP 头只能承载 ASCII，含非 ASCII 字节会在传输中变成乱码。
	for i := 0; i < len(got); i++ {
		if got[i] > 127 {
			t.Fatalf("X-Gofs-Path 含非 ASCII 字节: %q", got)
		}
	}
	// 但磁盘上的文件名必须是原样的中文。
	if _, err := os.Stat(datedOnDisk(root, "/报告.pdf", "2006/01/02")); err != nil {
		t.Errorf("中文文件名未正确落盘: %v", err)
	}
}

func TestUploadDatedInjectedIntoPage(t *testing.T) {
	root := makeFixture(t)
	s := newTestServer(t, testConfigDated(t, root, "2006/01/02", "-A"))

	r := do(t, s, http.MethodGet, "/", nil, nil)
	if !strings.Contains(r.body, `"upload_dated":true`) {
		t.Errorf("页面未注入 upload_dated=true")
	}
	if !strings.Contains(r.body, `"upload_date_dir":"`+todayDir("2006/01/02")+`"`) {
		t.Errorf("页面未注入当天日期目录 %q", todayDir("2006/01/02"))
	}

	// 关闭归档时前端应收到 false，避免提示与实际落盘位置不符。
	s2 := newTestServer(t, testConfig(t, root, "-A"))
	r2 := do(t, s2, http.MethodGet, "/", nil, nil)
	if !strings.Contains(r2.body, `"upload_dated":false`) {
		t.Errorf("关闭归档时页面未注入 upload_dated=false")
	}
}

// ---------------------------------------------------------------------------
// 在线编辑
// ---------------------------------------------------------------------------

// 浏览器地址栏导航会带这个 Accept 头。
var htmlNav = map[string]string{"Accept": "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8"}

// 前端 fetch 会带这个标记，要求服务端不要用 401 挑战触发原生登录框。
var ajaxHdr = map[string]string{"X-Gofs-Ajax": "1"}

// readDoc 读取文本文件接口并解析响应。
func readDoc(t *testing.T, s *Server, path string, headers map[string]string) (int, map[string]any, string) {
	t.Helper()
	r := do(t, s, http.MethodGet, "/__gofs__/text?path="+url.QueryEscape(path), nil, headers)
	var d map[string]any
	if strings.HasPrefix(r.head.Get("Content-Type"), "application/json") {
		_ = json.Unmarshal([]byte(r.body), &d)
	}
	return r.code, d, r.body
}

func TestTextRead(t *testing.T) {
	root := makeFixture(t)
	writeFile(t, filepath.Join(root, "note.md"), []byte("# 标题\n\n正文内容\n"))
	s := newTestServer(t, testConfig(t, root, "-A"))

	code, d, raw := readDoc(t, s, "/note.md", nil)
	if code != 200 {
		t.Fatalf("状态码 = %d；%s", code, raw)
	}
	if d["language"] != "markdown" {
		t.Errorf("language = %v, 期望 markdown", d["language"])
	}
	if d["content"] != "# 标题\n\n正文内容\n" {
		t.Errorf("content = %q", d["content"])
	}
	if d["editable"] != true {
		t.Errorf("editable 应为 true")
	}
	if mt, ok := d["mtime_ms"].(float64); !ok || mt <= 0 {
		t.Errorf("mtime_ms 无效: %v", d["mtime_ms"])
	}
	if d["read_only"] == true {
		t.Errorf("有写权限时不应标记 read_only")
	}
}

func TestTextReadRejectsBinary(t *testing.T) {
	root := makeFixture(t)
	// 扩展名是 txt，但内容是二进制：必须靠内容嗅探拦下来，
	// 否则编辑器会把它当文本打开，保存后文件就毁了。
	writeFile(t, filepath.Join(root, "fake.txt"), []byte{0x00, 0x01, 0x02, 0xff, 0xfe})
	s := newTestServer(t, testConfig(t, root, "-A"))

	code, _, body := readDoc(t, s, "/fake.txt", nil)
	if code != http.StatusUnsupportedMediaType {
		t.Errorf("状态码 = %d, 期望 415；%s", code, body)
	}
}

func TestTextReadRejectsTooLarge(t *testing.T) {
	root := makeFixture(t)
	writeFile(t, filepath.Join(root, "big.txt"), bytes.Repeat([]byte("x"), 4096))
	s := newTestServer(t, testConfig(t, root, "-A", "--edit-max-size", "1024"))

	code, _, body := readDoc(t, s, "/big.txt", nil)
	if code != http.StatusRequestEntityTooLarge {
		t.Errorf("状态码 = %d, 期望 413；%s", code, body)
	}
}

func TestTextReadRejectsUTF16(t *testing.T) {
	root := makeFixture(t)
	// UTF-16LE 的 BOM + 内容
	writeFile(t, filepath.Join(root, "u16.txt"), []byte{0xFF, 0xFE, 'h', 0, 'i', 0})
	s := newTestServer(t, testConfig(t, root, "-A"))

	code, _, body := readDoc(t, s, "/u16.txt", nil)
	if code != http.StatusUnsupportedMediaType {
		t.Errorf("状态码 = %d, 期望 415；%s", code, body)
	}
}

func TestTextSave(t *testing.T) {
	root := makeFixture(t)
	target := filepath.Join(root, "edit.txt")
	writeFile(t, target, []byte("原始内容\n"))
	s := newTestServer(t, testConfig(t, root, "-A"))

	code, d, _ := readDoc(t, s, "/edit.txt", nil)
	if code != 200 {
		t.Fatalf("读取失败: %d", code)
	}
	baseHash, _ := d["hash"].(string)
	if baseHash == "" {
		t.Fatalf("读取响应缺少 hash，乐观锁将无法工作")
	}

	payload, _ := json.Marshal(map[string]any{
		"path":      "/edit.txt",
		"content":   "修改后的内容\n第二行\n",
		"base_hash": baseHash,
	})
	r := do(t, s, http.MethodPut, "/__gofs__/text", bytes.NewReader(payload),
		map[string]string{"Content-Type": "application/json"})
	if r.code != 200 {
		t.Fatalf("保存状态码 = %d；%s", r.code, r.body)
	}

	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "修改后的内容\n第二行\n" {
		t.Errorf("磁盘内容 = %q", got)
	}
	// 返回的 hash 必须变化，否则连续保存第二次会被误判为冲突
	var out map[string]any
	_ = json.Unmarshal([]byte(r.body), &out)
	newHash, _ := out["hash"].(string)
	if newHash == "" || newHash == baseHash {
		t.Errorf("保存后 hash 未更新（%q → %q），乐观锁会失效", baseHash, newHash)
	}

	// 用新 hash 再存一次应当成功
	payload2, _ := json.Marshal(map[string]any{
		"path":      "/edit.txt",
		"content":   "第三次内容\n",
		"base_hash": newHash,
	})
	if r2 := do(t, s, http.MethodPut, "/__gofs__/text", bytes.NewReader(payload2),
		map[string]string{"Content-Type": "application/json"}); r2.code != 200 {
		t.Errorf("连续保存失败: %d；%s", r2.code, r2.body)
	}
}

func TestTextSaveConflict(t *testing.T) {
	root := makeFixture(t)
	writeFile(t, filepath.Join(root, "c.txt"), []byte("v1\n"))
	s := newTestServer(t, testConfig(t, root, "-A"))

	// 用一个过期的 base hash 模拟「编辑期间被别人改过」
	payload, _ := json.Marshal(map[string]any{
		"path":      "/c.txt",
		"content":   "v2\n",
		"base_hash": "0000000000000000",
	})
	r := do(t, s, http.MethodPut, "/__gofs__/text", bytes.NewReader(payload),
		map[string]string{"Content-Type": "application/json"})
	if r.code != http.StatusConflict {
		t.Fatalf("状态码 = %d, 期望 409；%s", r.code, r.body)
	}
	b, _ := os.ReadFile(filepath.Join(root, "c.txt"))
	if string(b) != "v1\n" {
		t.Errorf("冲突时不应写入，磁盘内容 = %q", b)
	}
	// 冲突响应应带上当前 hash，前端可据此提示
	if !strings.Contains(r.body, "current_hash") {
		t.Errorf("冲突响应缺少 current_hash: %s", r.body)
	}

	// force 应当覆盖成功
	payload2, _ := json.Marshal(map[string]any{
		"path":      "/c.txt",
		"content":   "v2\n",
		"base_hash": "0000000000000000",
		"force":     true,
	})
	r2 := do(t, s, http.MethodPut, "/__gofs__/text", bytes.NewReader(payload2),
		map[string]string{"Content-Type": "application/json"})
	if r2.code != 200 {
		t.Fatalf("强制覆盖状态码 = %d；%s", r2.code, r2.body)
	}
	b2, _ := os.ReadFile(filepath.Join(root, "c.txt"))
	if string(b2) != "v2\n" {
		t.Errorf("强制覆盖后内容 = %q", b2)
	}
}

func TestTextSavePreservesCRLF(t *testing.T) {
	root := makeFixture(t)
	target := filepath.Join(root, "win.txt")
	writeFile(t, target, []byte("a\r\nb\r\n"))

	s := newTestServer(t, testConfig(t, root, "-A"))
	code, d, _ := readDoc(t, s, "/win.txt", nil)
	if code != 200 {
		t.Fatalf("读取失败: %d", code)
	}
	if d["newline"] != "\r\n" {
		t.Errorf("newline = %q, 期望 CRLF", d["newline"])
	}

	// 浏览器 textarea 提交时 CRLF 已被规范化成 LF
	payload, _ := json.Marshal(map[string]any{
		"path":      "/win.txt",
		"content":   "a\nb\nc\n",
		"base_hash": d["hash"],
	})
	r := do(t, s, http.MethodPut, "/__gofs__/text", bytes.NewReader(payload),
		map[string]string{"Content-Type": "application/json"})
	if r.code != 200 {
		t.Fatalf("保存失败: %d；%s", r.code, r.body)
	}

	got, _ := os.ReadFile(target)
	if string(got) != "a\r\nb\r\nc\r\n" {
		t.Errorf("保存后换行风格未保留：%q（期望 CRLF）", got)
	}
}

func TestTextSavePreservesBOM(t *testing.T) {
	root := makeFixture(t)
	target := filepath.Join(root, "bom.txt")
	writeFile(t, target, append([]byte{0xEF, 0xBB, 0xBF}, []byte("内容\n")...))

	s := newTestServer(t, testConfig(t, root, "-A"))
	code, d, _ := readDoc(t, s, "/bom.txt", nil)
	if code != 200 {
		t.Fatalf("读取失败: %d", code)
	}
	if d["bom"] != "utf-8" {
		t.Errorf("bom = %v, 期望 utf-8", d["bom"])
	}
	if d["content"] != "内容\n" {
		t.Errorf("应剥离 BOM 后返回内容，实际 %q", d["content"])
	}

	payload, _ := json.Marshal(map[string]any{
		"path":      "/bom.txt",
		"content":   "改过了\n",
		"base_hash": d["hash"],
	})
	if r := do(t, s, http.MethodPut, "/__gofs__/text", bytes.NewReader(payload),
		map[string]string{"Content-Type": "application/json"}); r.code != 200 {
		t.Fatalf("保存失败: %d", r.code)
	}

	raw, _ := os.ReadFile(target)
	if !bytes.HasPrefix(raw, []byte{0xEF, 0xBB, 0xBF}) {
		t.Errorf("保存后 BOM 丢失了")
	}
	if string(raw[3:]) != "改过了\n" {
		t.Errorf("内容 = %q", raw[3:])
	}
}

func TestTextSaveRejectsNUL(t *testing.T) {
	root := makeFixture(t)
	writeFile(t, filepath.Join(root, "n.txt"), []byte("abc\n"))
	s := newTestServer(t, testConfig(t, root, "-A"))

	payload, _ := json.Marshal(map[string]any{
		"path":    "/n.txt",
		"content": "abc\x00def",
	})
	r := do(t, s, http.MethodPut, "/__gofs__/text", bytes.NewReader(payload),
		map[string]string{"Content-Type": "application/json"})
	if r.code != http.StatusBadRequest {
		t.Errorf("状态码 = %d, 期望 400", r.code)
	}
}

func TestTextDisabled(t *testing.T) {
	root := makeFixture(t)
	writeFile(t, filepath.Join(root, "a.txt"), []byte("x\n"))
	// 未开启上传 → 编辑也随之关闭
	s := newTestServer(t, testConfig(t, root))

	if code, _, _ := readDoc(t, s, "/a.txt", nil); code != http.StatusForbidden {
		t.Errorf("读取状态码 = %d, 期望 403", code)
	}
	payload, _ := json.Marshal(map[string]any{"path": "/a.txt", "content": "y\n"})
	r := do(t, s, http.MethodPut, "/__gofs__/text", bytes.NewReader(payload),
		map[string]string{"Content-Type": "application/json"})
	if r.code != http.StatusForbidden {
		t.Errorf("保存状态码 = %d, 期望 403", r.code)
	}
}

func TestTextReadOnlyUserCannotSave(t *testing.T) {
	root := makeFixture(t)
	writeFile(t, filepath.Join(root, "r.txt"), []byte("x\n"))
	s := newTestServer(t, testConfig(t, root, "-A", "-a", "guest:g@/", "-a", "admin:a@/:rw"))

	hdr := map[string]string{"Authorization": basicAuth("guest", "g")}
	code, d, _ := readDoc(t, s, "/r.txt", hdr)
	if code != 200 {
		t.Fatalf("只读用户应能读取，实际 %d", code)
	}
	if d["read_only"] != true {
		t.Errorf("只读用户应看到 read_only=true")
	}

	payload, _ := json.Marshal(map[string]any{"path": "/r.txt", "content": "y\n"})
	hdr2 := map[string]string{
		"Authorization": basicAuth("guest", "g"),
		"Content-Type":  "application/json",
	}
	r := do(t, s, http.MethodPut, "/__gofs__/text", bytes.NewReader(payload), hdr2)
	if r.code != http.StatusForbidden {
		t.Errorf("只读用户保存状态码 = %d, 期望 403", r.code)
	}
}

func TestTextPathTraversal(t *testing.T) {
	root := makeFixture(t)
	s := newTestServer(t, testConfig(t, root, "-A"))

	for _, p := range []string{"/../../etc/passwd", "/docs/../../../etc/passwd"} {
		code, _, _ := readDoc(t, s, p, nil)
		if code == 200 {
			t.Errorf("路径穿越未被拦截: %s", p)
		}
	}
}

func TestListingMarksEditable(t *testing.T) {
	root := makeFixture(t)
	writeFile(t, filepath.Join(root, "readme.md"), []byte("# hi\n"))
	writeFile(t, filepath.Join(root, "main.go"), []byte("package main\n"))
	writeFile(t, filepath.Join(root, "pic.png"), []byte{0x89, 'P', 'N', 'G'})
	s := newTestServer(t, testConfig(t, root, "-A"))

	r := do(t, s, http.MethodGet, "/?json", nil, nil)
	var listing struct {
		Entries []struct {
			Name     string `json:"name"`
			Editable bool   `json:"editable"`
			Lang     string `json:"lang"`
		} `json:"entries"`
	}
	if err := json.Unmarshal([]byte(r.body), &listing); err != nil {
		t.Fatal(err)
	}

	got := map[string]struct {
		editable bool
		lang     string
	}{}
	for _, e := range listing.Entries {
		got[e.Name] = struct {
			editable bool
			lang     string
		}{e.Editable, e.Lang}
	}
	if g := got["readme.md"]; !g.editable || g.lang != "markdown" {
		t.Errorf("readme.md 标记错误: %+v", g)
	}
	if g := got["main.go"]; !g.editable || g.lang != "go" {
		t.Errorf("main.go 标记错误: %+v", g)
	}
	if g := got["pic.png"]; g.editable {
		t.Errorf("pic.png 不应被标记为可编辑")
	}
}

// ---------------------------------------------------------------------------
// Basic 认证与登录
// ---------------------------------------------------------------------------

func TestAuthEndpoint(t *testing.T) {
	root := makeFixture(t)
	s := newTestServer(t, testConfig(t, root, "-a", "admin:secret@/:rw"))

	// 未启用鉴权时应当明确告诉前端不用登录
	s2 := newTestServer(t, testConfig(t, root))
	r0 := do(t, s2, http.MethodGet, "/__gofs__/auth", nil, nil)
	if r0.code != 200 || !strings.Contains(r0.body, `"auth_on":false`) {
		t.Errorf("未启用鉴权时响应异常: %d %s", r0.code, r0.body)
	}

	// 正确凭据
	r1 := do(t, s, http.MethodGet, "/__gofs__/auth", nil, map[string]string{
		"Authorization": basicAuth("admin", "secret"),
	})
	if r1.code != 200 {
		t.Fatalf("正确凭据状态码 = %d；%s", r1.code, r1.body)
	}
	if !strings.Contains(r1.body, `"user":"admin"`) {
		t.Errorf("未返回用户名: %s", r1.body)
	}

	// 错误密码
	r2 := do(t, s, http.MethodGet, "/__gofs__/auth", nil, map[string]string{
		"Authorization": basicAuth("admin", "wrong"),
	})
	if r2.code != http.StatusUnauthorized {
		t.Errorf("错误密码状态码 = %d, 期望 401", r2.code)
	}
	// 登录接口本身不应发挑战头，否则浏览器会弹原生框
	if r2.head.Get("WWW-Authenticate") != "" {
		t.Errorf("登录接口不应返回 WWW-Authenticate 挑战")
	}

	// 无凭据
	if r3 := do(t, s, http.MethodGet, "/__gofs__/auth", nil, nil); r3.code != http.StatusUnauthorized {
		t.Errorf("无凭据状态码 = %d, 期望 401", r3.code)
	}
}

func TestAuthEndpointPathScoped(t *testing.T) {
	root := makeFixture(t)
	// 账号对根目录只读、对 /docs 可写
	s := newTestServer(t, testConfig(t, root, "-A", "-a", "u:p@/:ro,/docs:rw"))
	hdr := map[string]string{"Authorization": basicAuth("u", "p")}

	// 不带 path：按根目录算 → 只读
	r := do(t, s, http.MethodGet, "/__gofs__/auth", nil, hdr)
	if r.code != 200 {
		t.Fatalf("状态码 = %d；%s", r.code, r.body)
	}
	if !strings.Contains(r.body, `"write":false`) {
		t.Errorf("按根目录算应为只读: %s", r.body)
	}

	// 带 path=/docs：站在自己有权限的目录里应拿到完整按钮，
	// 否则受限账号在自己目录里会被当成只读，什么操作都做不了。
	r2 := do(t, s, http.MethodGet, "/__gofs__/auth?path=/docs", nil, hdr)
	if !strings.Contains(r2.body, `"write":true`) {
		t.Errorf("在 /docs 下应拿到写权限: %s", r2.body)
	}
	if !strings.Contains(r2.body, `"edit":true`) {
		t.Errorf("在 /docs 下应能在线编辑: %s", r2.body)
	}

	// 越界路径不能借机提升权限
	r3 := do(t, s, http.MethodGet, "/__gofs__/auth?path=/other", nil, hdr)
	if !strings.Contains(r3.body, `"write":false`) {
		t.Errorf("越界路径不应有写权限: %s", r3.body)
	}
	// 含 .. 的路径直接忽略，按根目录算
	r4 := do(t, s, http.MethodGet, "/__gofs__/auth?path=/../../etc", nil, hdr)
	if r4.code != 200 || !strings.Contains(r4.body, `"write":false`) {
		t.Errorf("含 .. 的路径不应提升权限: %d %s", r4.code, r4.body)
	}
}

// TestAuthEndpointReturnsConfig 验证登录接口会把「未登录外壳里刻意没下发」
// 的配置一并补齐，否则前端登录后仍然不知道这些能力可用。
func TestAuthEndpointReturnsConfig(t *testing.T) {
	root := makeFixture(t)
	s := newTestServer(t, testConfigDated(t, root, "2006/01/02", "-A",
		"-a", "admin:pw@/:rw"))

	r := do(t, s, http.MethodGet, "/__gofs__/auth", nil, map[string]string{
		"Authorization": basicAuth("admin", "pw"),
	})
	if r.code != 200 {
		t.Fatalf("状态码 = %d", r.code)
	}
	var d map[string]any
	if err := json.Unmarshal([]byte(r.body), &d); err != nil {
		t.Fatalf("解析失败: %v\n%s", err, r.body)
	}
	if d["upload_dated"] != true {
		t.Errorf("未回传 upload_dated: %s", r.body)
	}
	if d["upload_date_dir"] != todayDir("2006/01/02") {
		t.Errorf("upload_date_dir = %v, 期望 %v", d["upload_date_dir"], todayDir("2006/01/02"))
	}
	if d["allow_keys"] != true {
		t.Errorf("未回传 allow_keys: %s", r.body)
	}
	if _, ok := d["edit_max_size"]; !ok {
		t.Errorf("未回传 edit_max_size: %s", r.body)
	}

	// 只读账号不应拿到密钥管理能力
	s2 := newTestServer(t, testConfig(t, root, "-A", "-a", "guest:g@/"))
	r2 := do(t, s2, http.MethodGet, "/__gofs__/auth", nil, map[string]string{
		"Authorization": basicAuth("guest", "g"),
	})
	if strings.Contains(r2.body, `"allow_keys":true`) {
		t.Errorf("只读账号不应拿到 allow_keys: %s", r2.body)
	}
}

// TestAuthShellRestoresIdentity 检查刷新场景所依赖的能力：
// 服务端只给空壳，前端必须能用本地凭据换来身份与权限。
func TestAuthShellRestoresIdentity(t *testing.T) {
	root := makeFixture(t)
	s := newTestServer(t, testConfig(t, root, "-A", "-a", "admin:pw@/:rw"))
	hdr := map[string]string{"Authorization": basicAuth("admin", "pw")}

	// 1) 未登录导航：外壳里没有用户名，权限位全 false
	shell := do(t, s, http.MethodGet, "/", nil, htmlNav)
	if !strings.Contains(shell.body, `"user":""`) {
		t.Errorf("外壳不应包含用户名: %s", shell.body)
	}
	if !strings.Contains(shell.body, `"write":false`) {
		t.Errorf("外壳的权限位应全为 false: %s", shell.body)
	}

	// 2) 拿本地凭据换身份：应当拿到用户名与真实权限
	restored := do(t, s, http.MethodGet, "/__gofs__/auth?path=/", nil, hdr)
	var d map[string]any
	if err := json.Unmarshal([]byte(restored.body), &d); err != nil {
		t.Fatal(err)
	}
	if d["user"] != "admin" {
		t.Errorf("user = %v, 期望 admin", d["user"])
	}
	perms, _ := d["perms"].(map[string]any)
	if perms == nil || perms["write"] != true {
		t.Errorf("恢复的权限不正确: %s", restored.body)
	}
}

func TestAuthVerifyIgnoresPathScope(t *testing.T) {
	root := makeFixture(t)
	// 账号只被授予 /docs 权限
	s := newTestServer(t, testConfig(t, root, "-a", "dev:pw@/docs:rw"))

	// 登录校验只看账号密码是否有效，不要求对某个路径有权限，
	// 否则这种受限账号会永远登不进来。
	r := do(t, s, http.MethodGet, "/__gofs__/auth", nil, map[string]string{
		"Authorization": basicAuth("dev", "pw"),
	})
	if r.code != 200 {
		t.Errorf("受限账号应能登录，实际 %d；%s", r.code, r.body)
	}
}

func TestAuthGateForNavigation(t *testing.T) {
	root := makeFixture(t)
	s := newTestServer(t, testConfig(t, root, "-a", "admin:secret@/:rw"))

	// 浏览器的页面导航：返回应用外壳而不是 401，
	// 这样前端才有机会弹出自己的登录框。
	r := do(t, s, http.MethodGet, "/", nil, htmlNav)
	if r.code != 200 {
		t.Fatalf("导航状态码 = %d, 期望 200", r.code)
	}
	if !strings.HasPrefix(r.head.Get("Content-Type"), "text/html") {
		t.Errorf("Content-Type = %q", r.head.Get("Content-Type"))
	}
	if !strings.Contains(r.body, `"auth_required":true`) {
		t.Errorf("外壳未标记 auth_required")
	}
	// 外壳里不能泄露任何真实数据
	if strings.Contains(r.body, "readme.md") {
		t.Errorf("未登录的外壳泄露了文件名")
	}
	// 也不应带挑战头，否则浏览器会抢先弹出原生框
	if r.head.Get("WWW-Authenticate") != "" {
		t.Errorf("导航外壳不应带挑战头")
	}

	// 带 ?json 的数据请求仍应 401
	r2 := do(t, s, http.MethodGet, "/?json", nil, nil)
	if r2.code != http.StatusUnauthorized {
		t.Errorf("数据请求状态码 = %d, 期望 401", r2.code)
	}

	// 直接指向文件的导航走挑战，让浏览器弹原生框
	r3 := do(t, s, http.MethodGet, "/notes.txt", nil, htmlNav)
	if r3.code != http.StatusUnauthorized {
		t.Errorf("文件导航状态码 = %d, 期望 401", r3.code)
	}
	if !strings.Contains(r3.head.Get("WWW-Authenticate"), "Basic") {
		t.Errorf("文件导航应带 Basic 挑战头")
	}

	// 登录后导航正常
	r4 := do(t, s, http.MethodGet, "/", nil, map[string]string{
		"Accept":        "text/html",
		"Authorization": basicAuth("admin", "secret"),
	})
	if r4.code != 200 || strings.Contains(r4.body, `"auth_required":true`) {
		t.Errorf("登录后仍返回空壳")
	}
	if !strings.Contains(r4.body, "readme.md") && !strings.Contains(r4.body, `"total"`) {
		t.Errorf("登录后应注入真实数据")
	}
}

func TestAuthChallengeSuppressedForAjax(t *testing.T) {
	root := makeFixture(t)
	s := newTestServer(t, testConfig(t, root, "-a", "admin:secret@/:rw"))

	// 应用内部请求：不带挑战，由前端自己处理登录
	r := do(t, s, http.MethodGet, "/?json", nil, ajaxHdr)
	if r.code != http.StatusUnauthorized {
		t.Fatalf("状态码 = %d, 期望 401", r.code)
	}
	if r.head.Get("WWW-Authenticate") != "" {
		t.Errorf("Ajax 请求不应带挑战头，否则会弹出原生登录框")
	}

	// 普通请求仍带挑战（curl / 浏览器下载都靠它）
	r2 := do(t, s, http.MethodGet, "/?json", nil, nil)
	if !strings.Contains(r2.head.Get("WWW-Authenticate"), "Basic") {
		t.Errorf("普通请求应带挑战头")
	}
}

func TestAuthShellHasNoConfigLeak(t *testing.T) {
	root := makeFixture(t)
	s := newTestServer(t, testConfigDated(t, root, "2006/01/02", "-A",
		"-a", "admin:secret@/:rw"))

	r := do(t, s, http.MethodGet, "/", nil, htmlNav)
	if r.code != 200 {
		t.Fatalf("状态码 = %d", r.code)
	}
	// 未登录时连归档目录、权限位这类配置都不该暴露
	if strings.Contains(r.body, `"upload_dated":true`) {
		t.Errorf("未登录的外壳泄露了上传归档配置")
	}
	if strings.Contains(r.body, `"edit":true`) {
		t.Errorf("未登录的外壳泄露了权限配置")
	}
}

// ---------------------------------------------------------------------------
// 上传密钥
// ---------------------------------------------------------------------------

// createKey 通过接口创建一把密钥，返回 (id, token)。
func createKey(t *testing.T, s *Server, name, scope string, ttlSeconds int64) (string, string) {
	t.Helper()
	return createKeyAs(t, s, name, scope, ttlSeconds, nil)
}

// createKeyAs 在给定认证头下创建密钥。
func createKeyAs(t *testing.T, s *Server, name, scope string, ttlSeconds int64, hdr map[string]string) (string, string) {
	t.Helper()
	payload, _ := json.Marshal(map[string]any{
		"name": name, "scope": scope, "ttl_seconds": ttlSeconds,
	})
	headers := map[string]string{"Content-Type": "application/json"}
	for k, v := range hdr {
		headers[k] = v
	}
	r := do(t, s, http.MethodPost, "/__gofs__/keys", bytes.NewReader(payload), headers)
	if r.code != http.StatusCreated {
		t.Fatalf("创建密钥状态码 = %d；%s", r.code, r.body)
	}
	var d struct {
		Key   map[string]any `json:"key"`
		Token string         `json:"token"`
	}
	if err := json.Unmarshal([]byte(r.body), &d); err != nil {
		t.Fatalf("解析失败: %v\n%s", err, r.body)
	}
	if d.Token == "" {
		t.Fatalf("未返回明文")
	}
	return d.Key["id"].(string), d.Token
}

func TestKeyCreateListRevoke(t *testing.T) {
	root := makeFixture(t)
	s := newTestServer(t, testConfig(t, root, "-A"))

	id, token := createKey(t, s, "备份", "/uploads", 3600)
	if !strings.HasPrefix(token, "gofs_") {
		t.Errorf("明文缺少前缀: %q", token)
	}

	// 列表里不能出现明文
	r := do(t, s, http.MethodGet, "/__gofs__/keys", nil, nil)
	if r.code != 200 {
		t.Fatalf("列表状态码 = %d", r.code)
	}
	if strings.Contains(r.body, token) {
		t.Errorf("列表接口泄露了密钥明文")
	}
	if !strings.Contains(r.body, `"name":"备份"`) {
		t.Errorf("列表缺少已创建的密钥: %s", r.body)
	}
	if !strings.Contains(r.body, `"status":"active"`) {
		t.Errorf("状态标记不正确: %s", r.body)
	}

	// 撤销
	if dr := do(t, s, http.MethodDelete, "/__gofs__/keys?id="+id, nil, nil); dr.code != http.StatusNoContent {
		t.Errorf("撤销状态码 = %d", dr.code)
	}
	// 撤销后无法再用
	pr := do(t, s, http.MethodPut, "/uploads/x.txt", strings.NewReader("x"),
		map[string]string{"X-Gofs-Upload-Key": token})
	if pr.code != http.StatusUnauthorized {
		t.Errorf("撤销后使用状态码 = %d, 期望 401", pr.code)
	}
}

func TestKeyUpload(t *testing.T) {
	root := makeFixture(t)
	s := newTestServer(t, testConfig(t, root, "-A"))

	_, token := createKey(t, s, "脚本", "/", 3600)

	// 请求头方式
	r := do(t, s, http.MethodPut, "/script-upload.txt", strings.NewReader("hello"),
		map[string]string{"X-Gofs-Upload-Key": token})
	if r.code != http.StatusCreated {
		t.Fatalf("上传状态码 = %d；%s", r.code, r.body)
	}
	b, err := os.ReadFile(filepath.Join(root, "script-upload.txt"))
	if err != nil || string(b) != "hello" {
		t.Errorf("文件内容 = %q, err = %v", b, err)
	}

	// 查询参数方式
	r2 := do(t, s, http.MethodPut, "/query-upload.txt?key="+token, strings.NewReader("world"), nil)
	if r2.code != http.StatusCreated {
		t.Fatalf("查询参数上传状态码 = %d；%s", r2.code, r2.body)
	}
	b2, _ := os.ReadFile(filepath.Join(root, "query-upload.txt"))
	if string(b2) != "world" {
		t.Errorf("文件内容 = %q", b2)
	}
}

func TestKeyScopeEnforced(t *testing.T) {
	root := makeFixture(t)
	s := newTestServer(t, testConfig(t, root, "-A"))

	_, token := createKey(t, s, "限定目录", "/uploads", 3600)
	hdr := map[string]string{"X-Gofs-Upload-Key": token}

	// 范围内：允许
	if r := do(t, s, http.MethodPut, "/uploads/ok.txt", strings.NewReader("x"), hdr); r.code != http.StatusCreated {
		t.Errorf("范围内上传状态码 = %d, 期望 201；%s", r.code, r.body)
	}
	// 范围外：拒绝
	r := do(t, s, http.MethodPut, "/other/nope.txt", strings.NewReader("x"), hdr)
	if r.code != http.StatusForbidden {
		t.Errorf("范围外上传状态码 = %d, 期望 403；%s", r.code, r.body)
	}
	if _, err := os.Stat(filepath.Join(root, "other")); err == nil {
		t.Errorf("越界文件被写入了")
	}
	// 前缀相近但不是子目录
	if r := do(t, s, http.MethodPut, "/uploads2/nope.txt", strings.NewReader("x"), hdr); r.code != http.StatusForbidden {
		t.Errorf("相似前缀目录状态码 = %d, 期望 403", r.code)
	}
}

func TestKeyCannotBeUsedForOtherOperations(t *testing.T) {
	root := makeFixture(t)
	// 开启鉴权，"别的操作需要账号"这件事才有意义。
	s := newTestServer(t, testConfig(t, root, "-A", "-a", "admin:pw@/:rw"))

	_, token := createKeyAs(t, s, "只上传", "/", 3600,
		map[string]string{"Authorization": basicAuth("admin", "pw")})
	hdr := map[string]string{"X-Gofs-Upload-Key": token}

	// 密钥不能读文件列表
	if r := do(t, s, http.MethodGet, "/?json", nil, hdr); r.code != http.StatusForbidden {
		t.Errorf("读取列表状态码 = %d, 期望 403", r.code)
	}
	// 不能读单个文件
	if r := do(t, s, http.MethodGet, "/notes.txt", nil, hdr); r.code != http.StatusForbidden {
		t.Errorf("读取文件状态码 = %d, 期望 403", r.code)
	}
	// 不能删除
	if r := do(t, s, http.MethodDelete, "/notes.txt", nil, hdr); r.code != http.StatusForbidden {
		t.Errorf("删除状态码 = %d, 期望 403", r.code)
	}
	// 不能建目录
	if r := do(t, s, MethodMkcol, "/newdir", nil, hdr); r.code != http.StatusForbidden {
		t.Errorf("建目录状态码 = %d, 期望 403", r.code)
	}
	// 不能读在线编辑接口
	if r := do(t, s, http.MethodGet, "/__gofs__/text?path=/notes.txt", nil, hdr); r.code != http.StatusUnauthorized {
		t.Errorf("编辑接口状态码 = %d, 期望 401", r.code)
	}
	// 不能管理密钥（否则等于权限提升）
	if r := do(t, s, http.MethodGet, "/__gofs__/keys", nil, hdr); r.code != http.StatusUnauthorized {
		t.Errorf("密钥管理状态码 = %d, 期望 401", r.code)
	}
	// 不能解压
	exHdr := map[string]string{"X-Gofs-Upload-Key": token, "Content-Type": "application/json"}
	if r := do(t, s, http.MethodPut, "/__gofs__/extract",
		strings.NewReader(`{"path":"/sample.zip"}`), exHdr); r.code == 200 {
		t.Errorf("上传密钥不应能调用解压接口")
	}
}

func TestKeyExpired(t *testing.T) {
	root := makeFixture(t)
	s := newTestServer(t, testConfig(t, root, "-A"))

	_, token := createKey(t, s, "短期", "/", 1)
	time.Sleep(1100 * time.Millisecond)

	r := do(t, s, http.MethodPut, "/late.txt", strings.NewReader("x"),
		map[string]string{"X-Gofs-Upload-Key": token})
	if r.code != http.StatusUnauthorized {
		t.Errorf("过期密钥状态码 = %d, 期望 401；%s", r.code, r.body)
	}
	if !strings.Contains(r.body, "过期") {
		t.Errorf("错误信息未说明已过期: %s", r.body)
	}
}

func TestKeyInvalidToken(t *testing.T) {
	root := makeFixture(t)
	s := newTestServer(t, testConfig(t, root, "-A"))

	r := do(t, s, http.MethodPut, "/x.txt", strings.NewReader("x"),
		map[string]string{"X-Gofs-Upload-Key": "gofs_forged-token-value"})
	if r.code != http.StatusUnauthorized {
		t.Errorf("伪造密钥状态码 = %d, 期望 401", r.code)
	}
}

func TestKeyRequiresWritePermissionToCreate(t *testing.T) {
	root := makeFixture(t)
	s := newTestServer(t, testConfig(t, root, "-A",
		"-a", "guest:g@/", "-a", "admin:a@/:rw"))

	// 只读账号不能签发密钥
	payload, _ := json.Marshal(map[string]any{"name": "x", "scope": "/"})
	r := do(t, s, http.MethodPost, "/__gofs__/keys", bytes.NewReader(payload),
		map[string]string{
			"Authorization": basicAuth("guest", "g"),
			"Content-Type":  "application/json",
		})
	if r.code != http.StatusForbidden {
		t.Errorf("只读账号签发密钥状态码 = %d, 期望 403", r.code)
	}
}

func TestKeyScopeCappedByCreatorPermission(t *testing.T) {
	root := makeFixture(t)
	s := newTestServer(t, testConfig(t, root, "-A",
		"-a", "dev:pw@/docs:rw", "-a", "admin:a@/:rw"))

	// 账号只对 /docs 有权限，不能签发覆盖全盘的密钥
	payload, _ := json.Marshal(map[string]any{"name": "越权", "scope": "/"})
	r := do(t, s, http.MethodPost, "/__gofs__/keys", bytes.NewReader(payload),
		map[string]string{
			"Authorization": basicAuth("dev", "pw"),
			"Content-Type":  "application/json",
		})
	if r.code != http.StatusForbidden {
		t.Errorf("越权签发状态码 = %d, 期望 403；%s", r.code, r.body)
	}

	// 但可以签发自己够得到的范围
	payload2, _ := json.Marshal(map[string]any{"name": "合规", "scope": "/docs"})
	r2 := do(t, s, http.MethodPost, "/__gofs__/keys", bytes.NewReader(payload2),
		map[string]string{
			"Authorization": basicAuth("dev", "pw"),
			"Content-Type":  "application/json",
		})
	if r2.code != http.StatusCreated {
		t.Errorf("合规签发状态码 = %d, 期望 201；%s", r2.code, r2.body)
	}
}

func TestKeyWithDateLayout(t *testing.T) {
	root := makeFixture(t)
	s := newTestServer(t, testConfigDated(t, root, "2006/01/02", "-A"))

	_, token := createKey(t, s, "归档", "/upload", 3600)
	// 密钥的 scope 是子目录，要归档得显式带 dated=1。
	r := do(t, s, http.MethodPut, "/upload/movie.mp4?dated=1", strings.NewReader("data"),
		map[string]string{"X-Gofs-Upload-Key": token})
	if r.code != http.StatusCreated {
		t.Fatalf("上传状态码 = %d；%s", r.code, r.body)
	}
	// 归档后的路径仍应落在密钥范围内
	want := filepath.Join(root, "upload", filepath.FromSlash(todayDir("2006/01/02")), "movie.mp4")
	if _, err := os.Stat(want); err != nil {
		t.Errorf("归档后文件未落盘: %v", err)
	}

	// 不带 dated 时（子目录默认不归档）同样不能越出密钥范围
	r2 := do(t, s, http.MethodPut, "/upload/plain.mp4", strings.NewReader("data"),
		map[string]string{"X-Gofs-Upload-Key": token})
	if r2.code != http.StatusCreated {
		t.Fatalf("上传状态码 = %d；%s", r2.code, r2.body)
	}
	if _, err := os.Stat(filepath.Join(root, "upload", "plain.mp4")); err != nil {
		t.Errorf("子目录上传未落在密钥范围内: %v", err)
	}
}

func TestKeysDisabled(t *testing.T) {
	root := makeFixture(t)
	// 不开 -A 也不开上传 → 密钥管理关闭
	s := newTestServer(t, testConfig(t, root))

	if r := do(t, s, http.MethodGet, "/__gofs__/keys", nil, nil); r.code != http.StatusForbidden {
		t.Errorf("状态码 = %d, 期望 403", r.code)
	}
	// 显式开启但未授予上传时，密钥可用但仍受 --allow-upload 约束
	s2 := newTestServer(t, testConfig(t, root, "--allow-keys"))
	id, token := createKey(t, s2, "k", "/", 3600)
	if id == "" {
		t.Fatal("未创建成功")
	}
	r := do(t, s2, http.MethodPut, "/x.txt", strings.NewReader("x"),
		map[string]string{"X-Gofs-Upload-Key": token})
	if r.code != http.StatusForbidden {
		t.Errorf("全局未开启上传时，密钥上传状态码 = %d, 期望 403", r.code)
	}
}

// ---------------------------------------------------------------------------
// 服务端防护：上传上限、跨站校验、认证限速、资源上限、日志转义
// ---------------------------------------------------------------------------

func TestUploadSizeLimitRejectsAndCleansUp(t *testing.T) {
	root := makeFixture(t)
	// 上限设成 1 KiB，便于用小请求触发。
	s := newTestServer(t, testConfig(t, root, "-A", "--upload-max-size", "1024"))

	big := strings.Repeat("x", 4096)
	r := do(t, s, http.MethodPut, "/big.bin", strings.NewReader(big), nil)
	if r.code != http.StatusRequestEntityTooLarge {
		t.Fatalf("状态码 = %d, 期望 413；%s", r.code, r.body)
	}
	// 关键：超限后不能把半截文件留在磁盘上，否则反复超限照样能填满磁盘。
	if _, err := os.Stat(filepath.Join(root, "big.bin")); err == nil {
		t.Errorf("超限上传留下了残缺文件，磁盘仍可被逐步填满")
	}

	// 上限之内的应当正常写入。
	ok := strings.Repeat("y", 512)
	if r := do(t, s, http.MethodPut, "/small.bin", strings.NewReader(ok), nil); r.code != http.StatusCreated {
		t.Fatalf("未超限的上传状态码 = %d, 期望 201；%s", r.code, r.body)
	}
	if b, err := os.ReadFile(filepath.Join(root, "small.bin")); err != nil || len(b) != 512 {
		t.Errorf("写入内容不正确: %v, len=%d", err, len(b))
	}
}

func TestUploadAppendKeepsExistingOnLimit(t *testing.T) {
	root := makeFixture(t)
	s := newTestServer(t, testConfig(t, root, "-A", "--upload-max-size", "1024"))
	writeFile(t, filepath.Join(root, "part.bin"), []byte("base"))

	// append 模式超限：已有内容不能被删掉（断点续传依赖它）。
	r := do(t, s, http.MethodPut, "/part.bin", strings.NewReader(strings.Repeat("z", 4096)),
		map[string]string{"X-Update-Range": "append"})
	if r.code != http.StatusRequestEntityTooLarge {
		t.Fatalf("状态码 = %d, 期望 413", r.code)
	}
	b, err := os.ReadFile(filepath.Join(root, "part.bin"))
	if err != nil {
		t.Fatalf("原有文件被删除了: %v", err)
	}
	if !strings.HasPrefix(string(b), "base") {
		t.Errorf("原有内容被破坏: %q", b)
	}
}

func TestFormUploadSizeLimit(t *testing.T) {
	root := makeFixture(t)
	s := newTestServer(t, testConfig(t, root, "-A", "--upload-max-size", "2048"))

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, err := mw.CreateFormFile("file", "big.txt")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fw.Write([]byte(strings.Repeat("q", 8192))); err != nil {
		t.Fatal(err)
	}
	if err := mw.Close(); err != nil {
		t.Fatal(err)
	}

	r := do(t, s, http.MethodPost, "/", &buf,
		map[string]string{"Content-Type": mw.FormDataContentType()})
	if r.code != http.StatusRequestEntityTooLarge {
		t.Fatalf("状态码 = %d, 期望 413；%s", r.code, r.body)
	}
}

func TestCSRFBlocksCrossOriginWrite(t *testing.T) {
	root := makeFixture(t)
	s := newTestServer(t, testConfig(t, root, "-A"))

	// 浏览器发起的跨站写请求一定带 Origin，且指向攻击者站点。
	hdr := map[string]string{"Origin": "https://evil.example"}
	if r := do(t, s, http.MethodPut, "/pwn.txt", strings.NewReader("x"), hdr); r.code != http.StatusForbidden {
		t.Errorf("跨站 PUT 状态码 = %d, 期望 403", r.code)
	}
	if r := do(t, s, http.MethodDelete, "/notes.txt", nil, hdr); r.code != http.StatusForbidden {
		t.Errorf("跨站 DELETE 状态码 = %d, 期望 403", r.code)
	}
	if r := do(t, s, MethodMkcol, "/pwn", nil, hdr); r.code != http.StatusForbidden {
		t.Errorf("跨站 MKCOL 状态码 = %d, 期望 403", r.code)
	}
	// 跨站请求不应产生任何副作用。
	if _, err := os.Stat(filepath.Join(root, "pwn.txt")); err == nil {
		t.Errorf("跨站 PUT 竟然写成功了")
	}
	if _, err := os.Stat(filepath.Join(root, "notes.txt")); err != nil {
		t.Errorf("跨站 DELETE 竟然删掉了文件: %v", err)
	}

	// Referer 同样能识别跨站来源。
	if r := do(t, s, http.MethodPut, "/pwn2.txt", strings.NewReader("x"),
		map[string]string{"Referer": "https://evil.example/attack.html"}); r.code != http.StatusForbidden {
		t.Errorf("带跨站 Referer 的 PUT 状态码 = %d, 期望 403", r.code)
	}
	// Origin: null（沙箱 iframe / file://）也应当拒绝。
	if r := do(t, s, http.MethodPut, "/pwn3.txt", strings.NewReader("x"),
		map[string]string{"Origin": "null"}); r.code != http.StatusForbidden {
		t.Errorf("Origin: null 的 PUT 状态码 = %d, 期望 403", r.code)
	}
}

func TestCSRFAllowsSameOriginAndNonBrowser(t *testing.T) {
	root := makeFixture(t)
	s := newTestServer(t, testConfig(t, root, "-A"))

	// 同源写请求必须放行，否则页面自己就用不了。
	if r := do(t, s, http.MethodPut, "/ok.txt", strings.NewReader("x"),
		map[string]string{"Origin": "http://" + "example.com"}); r.code != http.StatusCreated {
		t.Errorf("无 Origin 的 PUT 状态码 = %d, 期望 201", r.code)
	}

	// 非浏览器客户端（curl / 脚本）不带 Origin：不能误伤。
	if r := do(t, s, http.MethodDelete, "/notes.txt", nil, nil); r.code != http.StatusNoContent {
		t.Errorf("无 Origin 的 DELETE 状态码 = %d, 期望 204", r.code)
	}

	// 读方法不做来源校验。
	if r := do(t, s, http.MethodGet, "/?json",
		nil, map[string]string{"Origin": "https://evil.example"}); r.code != 200 {
		t.Errorf("跨站 GET 状态码 = %d, 期望 200（读操作不校验来源）", r.code)
	}
}

func TestCSRFCanBeDisabled(t *testing.T) {
	root := makeFixture(t)
	s := newTestServer(t, testConfig(t, root, "-A", "--no-csrf-protect"))

	r := do(t, s, http.MethodPut, "/x.txt", strings.NewReader("x"),
		map[string]string{"Origin": "https://evil.example"})
	if r.code != http.StatusCreated {
		t.Errorf("关闭校验后状态码 = %d, 期望 201", r.code)
	}
}

func TestAuthFailLimitBlocksBruteForce(t *testing.T) {
	root := makeFixture(t)
	s := newTestServer(t, testConfig(t, root, "-A",
		"-a", "alice:secret@/:rw",
		"--auth-fail-limit", "3",
		"--auth-fail-window", "5m"))

	bad := map[string]string{"Authorization": basicAuth("alice", "wrong")}
	for i := 0; i < 3; i++ {
		if r := do(t, s, http.MethodGet, "/?json", nil, bad); r.code != http.StatusUnauthorized {
			t.Fatalf("第 %d 次尝试状态码 = %d, 期望 401", i+1, r.code)
		}
	}
	// 达到阈值后再来（哪怕密码是对的）也应被临时封禁。
	r := do(t, s, http.MethodGet, "/?json", nil, bad)
	if r.code != http.StatusTooManyRequests {
		t.Fatalf("超限后状态码 = %d, 期望 429", r.code)
	}
	if ra := r.head.Get("Retry-After"); ra == "" {
		t.Errorf("429 响应缺少 Retry-After")
	}
	good := map[string]string{"Authorization": basicAuth("alice", "secret")}
	if r := do(t, s, http.MethodGet, "/?json", nil, good); r.code != http.StatusTooManyRequests {
		t.Errorf("封禁期间正确密码也应为 429，实际 = %d", r.code)
	}
}

func TestAuthFailResetsOnSuccess(t *testing.T) {
	root := makeFixture(t)
	s := newTestServer(t, testConfig(t, root, "-A",
		"-a", "alice:secret@/:rw",
		"--auth-fail-limit", "3",
		"--auth-fail-window", "5m"))

	bad := map[string]string{"Authorization": basicAuth("alice", "wrong")}
	good := map[string]string{"Authorization": basicAuth("alice", "secret")}
	do(t, s, http.MethodGet, "/?json", nil, bad)
	do(t, s, http.MethodGet, "/?json", nil, bad)

	// 中间成功登录一次，失败计数应当清零。
	if r := do(t, s, http.MethodGet, "/?json", nil, good); r.code != 200 {
		t.Fatalf("正确凭据状态码 = %d, 期望 200", r.code)
	}
	do(t, s, http.MethodGet, "/?json", nil, bad)
	do(t, s, http.MethodGet, "/?json", nil, bad)
	if r := do(t, s, http.MethodGet, "/?json", nil, good); r.code != 200 {
		t.Errorf("成功登录后计数未清零，状态码 = %d", r.code)
	}
}

func TestAuthBlockExpires(t *testing.T) {
	root := makeFixture(t)
	s := newTestServer(t, testConfig(t, root, "-A",
		"-a", "alice:secret@/:rw",
		"--auth-fail-limit", "2",
		"--auth-fail-window", "80ms"))

	bad := map[string]string{"Authorization": basicAuth("alice", "wrong")}
	do(t, s, http.MethodGet, "/?json", nil, bad)
	do(t, s, http.MethodGet, "/?json", nil, bad)
	if r := do(t, s, http.MethodGet, "/?json", nil, bad); r.code != http.StatusTooManyRequests {
		t.Fatalf("状态码 = %d, 期望 429", r.code)
	}

	time.Sleep(120 * time.Millisecond)
	good := map[string]string{"Authorization": basicAuth("alice", "secret")}
	if r := do(t, s, http.MethodGet, "/?json", nil, good); r.code != 200 {
		t.Errorf("封禁到期后状态码 = %d, 期望 200", r.code)
	}
}

func TestListMaxEntriesTruncates(t *testing.T) {
	root := t.TempDir()
	for i := 0; i < 30; i++ {
		writeFile(t, filepath.Join(root, fmt.Sprintf("f%02d.txt", i)), []byte("x"))
	}
	s := newTestServer(t, testConfig(t, root, "-A", "--list-max-entries", "10"))

	r := do(t, s, http.MethodGet, "/?json", nil, nil)
	if r.code != 200 {
		t.Fatalf("状态码 = %d", r.code)
	}
	var d struct {
		Total     int  `json:"total"`
		TotalAll  int  `json:"total_all"`
		Truncated bool `json:"truncated"`
	}
	if err := json.Unmarshal([]byte(r.body), &d); err != nil {
		t.Fatal(err)
	}
	if d.Total != 10 {
		t.Errorf("返回条目数 = %d, 期望 10", d.Total)
	}
	if d.TotalAll != 30 {
		t.Errorf("total_all = %d, 期望 30", d.TotalAll)
	}
	if !d.Truncated {
		t.Errorf("缺少截断标记，前端无法提示用户")
	}
}

func TestArchiveLimits(t *testing.T) {
	root := makeFixture(t)

	// 条目数上限。
	s := newTestServer(t, testConfig(t, root, "-A", "--archive-max-items", "1"))
	if r := do(t, s, http.MethodGet, "/docs?zip", nil, nil); r.code != http.StatusRequestEntityTooLarge {
		t.Errorf("条目超限状态码 = %d, 期望 413；%s", r.code, r.body)
	}

	// 字节数上限。
	s2 := newTestServer(t, testConfig(t, root, "-A", "--archive-max-bytes", "8"))
	if r := do(t, s2, http.MethodGet, "/docs?zip", nil, nil); r.code != http.StatusRequestEntityTooLarge {
		t.Errorf("字节超限状态码 = %d, 期望 413", r.code)
	}

	// 选中模式下超限同样应给出 413，而不是「全部跳过」的 400。
	s3 := newTestServer(t, testConfig(t, root, "-A", "--archive-max-items", "1"))
	r := do(t, s3, http.MethodGet, "/?zip&pick=/docs", nil, nil)
	if r.code != http.StatusRequestEntityTooLarge {
		t.Errorf("所选超限状态码 = %d, 期望 413；%s", r.code, r.body)
	}
}

func TestLongNameRejected(t *testing.T) {
	root := makeFixture(t)
	s := newTestServer(t, testConfig(t, root, "-A", "--max-name-bytes", "32"))

	long := strings.Repeat("a", 200) + ".txt"
	r := do(t, s, http.MethodPut, "/"+long, strings.NewReader("x"), nil)
	if r.code != http.StatusBadRequest {
		t.Errorf("超长文件名状态码 = %d, 期望 400（不应是 500）", r.code)
	}
}

func TestInlineContentGuards(t *testing.T) {
	root := makeFixture(t)
	writeFile(t, filepath.Join(root, "evil.html"), []byte("<script>alert(1)</script>"))
	writeFile(t, filepath.Join(root, "pic.png"), []byte("not really a png"))
	s := newTestServer(t, testConfig(t, root, "-A"))

	// 上传的网页在同源下执行脚本 = 存储型 XSS，必须沙箱化。
	r := do(t, s, http.MethodGet, "/evil.html", nil, nil)
	if csp := r.head.Get("Content-Security-Policy"); !strings.Contains(csp, "sandbox") {
		t.Errorf("HTML 响应缺少 CSP sandbox，实际 = %q", csp)
	}
	if r.head.Get("X-Content-Type-Options") != "nosniff" {
		t.Errorf("缺少 nosniff")
	}

	// 普通文件也要有 nosniff，避免浏览器按内容猜类型。
	if r := do(t, s, http.MethodGet, "/pic.png", nil, nil); r.head.Get("X-Content-Type-Options") != "nosniff" {
		t.Errorf("普通文件缺少 nosniff")
	}

	// SVG 同样是可执行文档。
	writeFile(t, filepath.Join(root, "x.svg"), []byte(`<svg xmlns="http://www.w3.org/2000/svg"/>`))
	if r := do(t, s, http.MethodGet, "/x.svg", nil, nil); !strings.Contains(r.head.Get("Content-Security-Policy"), "sandbox") {
		t.Errorf("SVG 响应缺少 CSP sandbox")
	}

	// 关闭开关后不再注入（保留连脚本一起预览的能力）。
	s2 := newTestServer(t, testConfig(t, root, "-A", "--no-html-sandbox"))
	if r := do(t, s2, http.MethodGet, "/evil.html", nil, nil); r.head.Get("Content-Security-Policy") != "" {
		t.Errorf("关闭后仍注入了 CSP")
	}
}

func TestHashSizeLimit(t *testing.T) {
	root := makeFixture(t)
	s := newTestServer(t, testConfig(t, root, "-A", "--hash-max-size", "16"))

	if r := do(t, s, http.MethodGet, "/docs/guide.txt?hash", nil, nil); r.code != http.StatusRequestEntityTooLarge {
		t.Errorf("大文件取摘要状态码 = %d, 期望 413", r.code)
	}
	if r := do(t, s, http.MethodGet, "/notes.txt?hash", nil, nil); r.code != 200 {
		t.Errorf("小文件取摘要状态码 = %d, 期望 200", r.code)
	}
}

func TestConcurrencyLimitReturns503(t *testing.T) {
	root := makeFixture(t)
	// 上限设为 1，再用一个占住名额的请求去挤。
	s := newTestServer(t, testConfig(t, root, "-A", "--max-concurrent", "1"))

	entered := make(chan struct{})
	leave := make(chan struct{})

	// 直接占用闸门，模拟一个仍在处理中的慢请求。
	s.guard.slots <- struct{}{}
	go func() {
		close(entered)
		<-leave
	}()

	r := do(t, s, http.MethodGet, "/?json", nil, nil)
	<-entered
	close(leave)
	if r.code != http.StatusServiceUnavailable {
		t.Errorf("超出并发上限的状态码 = %d, 期望 503", r.code)
	}
	if r.head.Get("Retry-After") == "" {
		t.Errorf("503 响应缺少 Retry-After")
	}
}

func TestLogSanitizesControlChars(t *testing.T) {
	// 文件名可以含换行，直接写进日志就能伪造出额外的日志行。
	injected := "a\n127.0.0.1 - - \"GET /admin\" 200\rb\x1b[31m"
	got := sanitizeLogLine(injected)
	if strings.ContainsAny(got, "\n\r\x1b") {
		t.Errorf("日志未清理控制字符: %q", got)
	}
	// 正常内容原样保留。
	if sanitizeLogLine("GET /a.bin 200") != "GET /a.bin 200" {
		t.Errorf("正常日志被改动")
	}

	r := httptest.NewRequest(http.MethodGet, "/a%0Ab?x=1", nil)
	r.Header.Set("User-Agent", "ua\x1b[0m\nFAKE")
	var buf bytes.Buffer
	lg := &Logger{out: &buf, enabled: true, format: defaultLogFormat}
	lg.Log(r, 200, "", time.Now())
	line := buf.String()
	if strings.Count(line, "\n") != 1 {
		t.Errorf("日志被注入成多行: %q", line)
	}
	if strings.Contains(line, "\x1b") {
		t.Errorf("日志未清理 ANSI 转义: %q", line)
	}
}

// ---------------------------------------------------------------------------
// 运行期设置：切换服务根目录、调整单文件上传上限
// ---------------------------------------------------------------------------

const jsonHeader = "application/json"

// resolved 返回路径解析软链后的真实路径，与 Resolver.Root() 的口径一致。
// macOS 上 TempDir 位于 /var → /private/var 的软链之下，不解析就会对不上。
func resolved(t *testing.T, p string) string {
	t.Helper()
	real, err := filepath.EvalSymlinks(p)
	if err != nil {
		t.Fatalf("解析软链失败 %s: %v", p, err)
	}
	return real
}

// settingsOf 读取当前设置。
func settingsOf(t *testing.T, s *Server, hdr map[string]string) map[string]any {
	t.Helper()
	r := do(t, s, http.MethodGet, "/__gofs__/settings", nil, hdr)
	if r.code != 200 {
		t.Fatalf("读取设置失败 %d: %s", r.code, r.body)
	}
	var d map[string]any
	if err := json.Unmarshal([]byte(r.body), &d); err != nil {
		t.Fatalf("解析设置失败: %v\n%s", err, r.body)
	}
	return d
}

// putSettings 提交一次设置修改。
func putSettings(t *testing.T, s *Server, body string, hdr map[string]string) resp {
	t.Helper()
	h := map[string]string{"Content-Type": jsonHeader}
	for k, v := range hdr {
		h[k] = v
	}
	return do(t, s, http.MethodPut, "/__gofs__/settings", strings.NewReader(body), h)
}

func TestSettingsRevealCurrentValues(t *testing.T) {
	base := t.TempDir()
	inner := filepath.Join(base, "inner")
	writeFile(t, filepath.Join(inner, "a.txt"), []byte("a\n"))
	s := newTestServer(t, testConfig(t, inner, "-A"))

	d := settingsOf(t, s, nil)
	if d["root"] != resolved(t, inner) {
		t.Errorf("root = %v, 期望 %s", d["root"], resolved(t, inner))
	}
	if d["root_switchable"] != true {
		t.Errorf("默认应允许切换根目录")
	}
	roots, _ := d["allow_roots"].([]any)
	if len(roots) != 1 || roots[0] != resolved(t, base) {
		t.Errorf("允许范围 = %v, 期望 [%s]", roots, resolved(t, base))
	}
	// 上限必须同时给出「当前值」与「启动默认值」，界面才能提供「恢复默认」。
	if _, ok := d["upload_max_size"].(float64); !ok {
		t.Errorf("缺少 upload_max_size: %v", d["upload_max_size"])
	}
	if _, ok := d["upload_max_size_default"].(float64); !ok {
		t.Errorf("缺少 upload_max_size_default")
	}
}

func TestSettingsSwitchRootWithinAllowedRange(t *testing.T) {
	base := t.TempDir()
	inner := filepath.Join(base, "inner")
	sibling := filepath.Join(base, "sibling")
	writeFile(t, filepath.Join(inner, "a.txt"), []byte("inner\n"))
	writeFile(t, filepath.Join(sibling, "b.txt"), []byte("sibling\n"))

	s := newTestServer(t, testConfig(t, inner, "-A"))

	r := putSettings(t, s, `{"root":`+jsonString(sibling)+`}`, nil)
	if r.code != 200 {
		t.Fatalf("切换根目录状态码 = %d: %s", r.code, r.body)
	}

	// 服务内容应当立刻变成新根下的。
	r = do(t, s, http.MethodGet, "/?json", nil, nil)
	if r.code != 200 {
		t.Fatalf("列举状态码 = %d", r.code)
	}
	if !strings.Contains(r.body, "b.txt") || strings.Contains(r.body, "a.txt") {
		t.Errorf("切换后内容不对: %s", r.body)
	}

	// 相对路径按「当前根」解释：先切到父目录，再用相对路径下来。
	if r := putSettings(t, s, `{"root":`+jsonString(base)+`}`, nil); r.code != 200 {
		t.Fatalf("切到父目录失败 %d: %s", r.code, r.body)
	}
	if r := putSettings(t, s, `{"root":"inner"}`, nil); r.code != 200 {
		t.Fatalf("相对路径切换失败 %d: %s", r.code, r.body)
	}
	if d := settingsOf(t, s, nil); d["root"] != resolved(t, inner) {
		t.Errorf("相对路径解析错误: %v", d["root"])
	}
	// 反过来，「..」也应当能从子目录回到父目录。
	if r := putSettings(t, s, `{"root":".."}`, nil); r.code != 200 {
		t.Fatalf("用 .. 返回父目录失败 %d: %s", r.code, r.body)
	}
	if d := settingsOf(t, s, nil); d["root"] != resolved(t, base) {
		t.Errorf(".. 解析错误: %v", d["root"])
	}
}

func TestSettingsSwitchRootRejectsOutsideRange(t *testing.T) {
	base := t.TempDir()
	inner := filepath.Join(base, "inner")
	outside := t.TempDir()
	writeFile(t, filepath.Join(inner, "a.txt"), []byte("a\n"))
	writeFile(t, filepath.Join(outside, "secret.txt"), []byte("secret\n"))

	s := newTestServer(t, testConfig(t, inner, "-A"))

	r := putSettings(t, s, `{"root":`+jsonString(outside)+`}`, nil)
	if r.code != 400 {
		t.Fatalf("越界切换状态码 = %d, 期望 400: %s", r.code, r.body)
	}
	// 根目录不能被改动。
	if d := settingsOf(t, s, nil); d["root"] != resolved(t, inner) {
		t.Errorf("越界请求改动了根目录: %v", d["root"])
	}
	// 目标内容也不能变得可见。
	if r := do(t, s, http.MethodGet, "/?json", nil, nil); strings.Contains(r.body, "secret.txt") {
		t.Errorf("越界目录的内容被泄露")
	}
}

func TestSettingsSwitchRootRejectsSymlinkEscape(t *testing.T) {
	base := t.TempDir()
	inner := filepath.Join(base, "inner")
	outside := t.TempDir()
	writeFile(t, filepath.Join(inner, "a.txt"), []byte("a\n"))
	writeFile(t, filepath.Join(outside, "secret.txt"), []byte("secret\n"))

	// 在允许范围内放一个指向范围外的软链 —— 不做软链解析就能绕过白名单。
	link := filepath.Join(base, "escape")
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("无法创建软链: %v", err)
	}

	s := newTestServer(t, testConfig(t, inner, "-A"))
	r := putSettings(t, s, `{"root":`+jsonString(link)+`}`, nil)
	if r.code != 400 {
		t.Fatalf("软链逃逸状态码 = %d, 期望 400: %s", r.code, r.body)
	}

	// 用 .. 拼出来的路径同样要判越界。
	r = putSettings(t, s, `{"root":`+jsonString(filepath.Join(base, "..", "..", "etc"))+`}`, nil)
	if r.code == 200 {
		t.Errorf("含 .. 的越界路径竟然被接受: %s", r.body)
	}
}

func TestSettingsSwitchRootValidation(t *testing.T) {
	base := t.TempDir()
	inner := filepath.Join(base, "inner")
	writeFile(t, filepath.Join(inner, "a.txt"), []byte("a\n"))
	writeFile(t, filepath.Join(base, "afile.txt"), []byte("f\n"))
	s := newTestServer(t, testConfig(t, inner, "-A"))

	for _, tc := range []struct {
		name string
		body string
		want string
	}{
		{"空路径", `{"root":"   "}`, "不能为空"},
		{"不存在的目录", `{"root":` + jsonString(filepath.Join(base, "nope")) + `}`, "不存在"},
		{"指向文件而非目录", `{"root":` + jsonString(filepath.Join(base, "afile.txt")) + `}`, "不是目录"},
	} {
		r := putSettings(t, s, tc.body, nil)
		if r.code != 400 {
			t.Errorf("%s: 状态码 = %d, 期望 400", tc.name, r.code)
			continue
		}
		if !strings.Contains(r.body, tc.want) {
			t.Errorf("%s: 提示信息里应有 %q，实际 %s", tc.name, tc.want, r.body)
		}
	}
}

func TestSettingsUploadLimitTakesEffect(t *testing.T) {
	root := makeFixture(t)
	s := newTestServer(t, testConfig(t, root, "-A"))

	// 放宽到 1 MiB：原本能过的 2 MiB 就该被拒。
	if r := putSettings(t, s, `{"upload_max_size":1048576}`, nil); r.code != 200 {
		t.Fatalf("设置上限失败 %d: %s", r.code, r.body)
	}
	if r := do(t, s, http.MethodPut, "/big.bin", strings.NewReader(strings.Repeat("x", 2<<20)), nil); r.code != http.StatusRequestEntityTooLarge {
		t.Errorf("超限上传状态码 = %d, 期望 413", r.code)
	}
	if _, err := os.Stat(filepath.Join(root, "big.bin")); err == nil {
		t.Errorf("超限上传留下了残缺文件")
	}
	if r := do(t, s, http.MethodPut, "/ok.bin", strings.NewReader(strings.Repeat("y", 1<<20)), nil); r.code != http.StatusCreated {
		t.Errorf("限内上传状态码 = %d, 期望 201", r.code)
	}

	// 设为 0 = 不限制。
	if r := putSettings(t, s, `{"upload_max_size":0}`, nil); r.code != 200 {
		t.Fatalf("设为不限制失败 %d: %s", r.code, r.body)
	}
	if r := do(t, s, http.MethodPut, "/big2.bin", strings.NewReader(strings.Repeat("x", 3<<20)), nil); r.code != http.StatusCreated {
		t.Errorf("不限制后上传状态码 = %d, 期望 201", r.code)
	}

	// 负数没有意义。
	if r := putSettings(t, s, `{"upload_max_size":-1}`, nil); r.code != 400 {
		t.Errorf("负数上限状态码 = %d, 期望 400", r.code)
	}
}

func TestSettingsEmptyPatchRejected(t *testing.T) {
	root := makeFixture(t)
	s := newTestServer(t, testConfig(t, root, "-A"))

	if r := putSettings(t, s, `{}`, nil); r.code != 400 {
		t.Errorf("空 patch 状态码 = %d, 期望 400", r.code)
	}
	if r := putSettings(t, s, `not json`, nil); r.code != 400 {
		t.Errorf("非法 JSON 状态码 = %d, 期望 400", r.code)
	}
}

func TestSettingsRootSwitchDisabledByFlag(t *testing.T) {
	base := t.TempDir()
	inner := filepath.Join(base, "inner")
	sibling := filepath.Join(base, "sibling")
	writeFile(t, filepath.Join(inner, "a.txt"), []byte("a\n"))
	writeFile(t, filepath.Join(sibling, "b.txt"), []byte("b\n"))

	s := newTestServer(t, testConfig(t, inner, "-A", "--no-root-switch"))

	d := settingsOf(t, s, nil)
	if d["root_switchable"] != false {
		t.Errorf("--no-root-switch 下 root_switchable 应为 false")
	}
	r := putSettings(t, s, `{"root":`+jsonString(sibling)+`}`, nil)
	if r.code != http.StatusForbidden {
		t.Errorf("禁用换根后仍可切换，状态码 = %d: %s", r.code, r.body)
	}
	// 上传上限不受该开关影响。
	if r := putSettings(t, s, `{"upload_max_size":1048576}`, nil); r.code != 200 {
		t.Errorf("禁用换根不该影响改上限: %d %s", r.code, r.body)
	}
}

func TestSettingsRequiresAdmin(t *testing.T) {
	base := t.TempDir()
	writeFile(t, filepath.Join(base, "docs", "a.txt"), []byte("a\n"))
	s := newTestServer(t, testConfig(t, base, "-A",
		"-a", "boss:secret@/:rw",
		"-a", "guest:guest@/docs:r"))

	guest := map[string]string{"Authorization": basicAuth("guest", "guest")}
	boss := map[string]string{"Authorization": basicAuth("boss", "secret")}

	// 只读账号：能读（只拿到非敏感部分），不能改。
	d := settingsOf(t, s, guest)
	if d["root"] != nil && d["root"] != "" {
		t.Errorf("非管理员不该看到服务根目录: %v", d["root"])
	}
	if d["root_switchable"] != false {
		t.Errorf("非管理员不该具备换根能力")
	}
	if r := putSettings(t, s, `{"upload_max_size":1}`, guest); r.code != http.StatusForbidden {
		t.Errorf("只读账号改设置状态码 = %d, 期望 403: %s", r.code, r.body)
	}

	// 管理员：全部放行。
	if d := settingsOf(t, s, boss); d["root_switchable"] != true {
		t.Errorf("管理员应可换根")
	}
	if r := putSettings(t, s, `{"upload_max_size":1048576}`, boss); r.code != 200 {
		t.Errorf("管理员改设置失败 %d: %s", r.code, r.body)
	}

	// 未带凭据 → 401（而不是 403，好让前端弹登录框）。
	if r := do(t, s, http.MethodGet, "/__gofs__/settings", nil, nil); r.code != http.StatusUnauthorized {
		t.Errorf("未登录读取状态码 = %d, 期望 401", r.code)
	}
}

func TestSettingsPageFlagShownOnlyToAdmin(t *testing.T) {
	base := t.TempDir()
	writeFile(t, filepath.Join(base, "docs", "a.txt"), []byte("a\n"))
	s := newTestServer(t, testConfig(t, base, "-A",
		"-a", "boss:secret@/:rw",
		"-a", "guest:guest@/docs:r"))

	// 页面注入的 allow_settings 决定「设置」入口显隐，
	// 它必须与接口的实际拦截一致，否则会出现「按钮在但点了 403」。
	// 注意要请求 HTML 页面而不是 ?json —— 后者是纯列表接口，不含页面状态。
	body := func(hdr map[string]string) string {
		r := do(t, s, http.MethodGet, "/", nil, hdr)
		return r.body
	}
	if !strings.Contains(body(map[string]string{"Authorization": basicAuth("boss", "secret")}),
		`"allow_settings":true`) {
		t.Errorf("管理员页面未注入 allow_settings=true")
	}
	if strings.Contains(body(map[string]string{"Authorization": basicAuth("guest", "guest")}),
		`"allow_settings":true`) {
		t.Errorf("只读账号不该拿到 allow_settings=true")
	}
}

func TestSettingsAuthResponseCarriesFlag(t *testing.T) {
	base := t.TempDir()
	writeFile(t, filepath.Join(base, "docs", "a.txt"), []byte("a\n"))
	s := newTestServer(t, testConfig(t, base, "-A",
		"-a", "boss:secret@/:rw",
		"-a", "guest:guest@/docs:r"))

	// 登录响应要带上 allow_settings 与 upload_max_size，
	// 否则前端登录后仍然不知道该不该显示「设置」入口。
	r := do(t, s, http.MethodGet, "/__gofs__/auth", nil,
		map[string]string{"Authorization": basicAuth("boss", "secret")})
	if !strings.Contains(r.body, `"allow_settings":true`) {
		t.Errorf("管理员登录响应缺少 allow_settings: %s", r.body)
	}
	if !strings.Contains(r.body, `"upload_max_size":`) {
		t.Errorf("登录响应缺少 upload_max_size: %s", r.body)
	}

	r = do(t, s, http.MethodGet, "/__gofs__/auth", nil,
		map[string]string{"Authorization": basicAuth("guest", "guest")})
	if strings.Contains(r.body, `"allow_settings":true`) {
		t.Errorf("只读账号登录响应不该给 allow_settings=true: %s", r.body)
	}
}

// TestSettingsConcurrentSwitchRoot 用 -race 跑时才有意义：
// 切换根目录会改 Resolver 的共享状态，而请求正在并发读取它。
func TestSettingsConcurrentSwitchRoot(t *testing.T) {
	base := t.TempDir()
	inner := filepath.Join(base, "inner")
	sibling := filepath.Join(base, "sibling")
	writeFile(t, filepath.Join(inner, "a.txt"), []byte("a\n"))
	writeFile(t, filepath.Join(sibling, "b.txt"), []byte("b\n"))

	s := newTestServer(t, testConfig(t, inner, "-A"))
	h := s.Handler()

	var wg sync.WaitGroup
	stop := make(chan struct{})

	// 持续切换根目录。
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			target := inner
			if i%2 == 0 {
				target = sibling
			}
			body := strings.NewReader(`{"root":` + jsonString(target) + `}`)
			req := httptest.NewRequest(http.MethodPut, "/__gofs__/settings", body)
			req.Header.Set("Content-Type", jsonHeader)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
		}
	}()

	// 同时不停地列举目录与读设置。
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				rec := httptest.NewRecorder()
				h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/?json", nil))
				rec2 := httptest.NewRecorder()
				h.ServeHTTP(rec2, httptest.NewRequest(http.MethodGet, "/__gofs__/settings", nil))
			}
		}()
	}

	time.Sleep(150 * time.Millisecond)
	close(stop)
	wg.Wait()
}

// jsonString 把字符串编成一个 JSON 字面量（路径可能含反斜杠、引号）。
func jsonString(s string) string {
	b, err := json.Marshal(s)
	if err != nil {
		return `""`
	}
	return string(b)
}
