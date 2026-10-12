package server

import (
	"bytes"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/xxl6097/gofs/internal/config"
)

// testConfigRoots 构造一个挂了多个服务目录的配置（位置参数全给）。
func testConfigRoots(t *testing.T, roots []string, extra ...string) *config.Config {
	t.Helper()
	args := []string{"-b", "127.0.0.1", "-p", "0", "-log-format", "none",
		"--no-upload-dated", "--key-file", ""}
	args = append(args, extra...)
	args = append(args, roots...)
	cfg, err := config.Parse(args, io.Discard)
	if err != nil {
		t.Fatalf("解析配置失败: %v", err)
	}
	return cfg
}

// multiRootFixture 造两个互不相干的根：data 与 backup，各放一个文件。
// 返回 (服务, data 目录, backup 目录)。
func multiRootFixture(t *testing.T, extra ...string) (*Server, string, string) {
	t.Helper()
	base := t.TempDir()
	dataDir := filepath.Join(base, "data")
	backupDir := filepath.Join(base, "backup")
	writeFile(t, filepath.Join(dataDir, "notes.txt"), []byte("来自 data"))
	writeFile(t, filepath.Join(backupDir, "b.txt"), []byte("来自 backup"))

	cfg := testConfigRoots(t, []string{dataDir, backupDir}, extra...)
	return newTestServer(t, cfg), dataDir, backupDir
}

// TestMultiRootServesEachRootUnderLabel 是这次的核心：每个根在自己的标签下
// 可取到正确内容，且**互相看不见**。
func TestMultiRootServesEachRootUnderLabel(t *testing.T) {
	s, _, _ := multiRootFixture(t, "-A")

	for _, tc := range []struct{ url, want string }{
		{"/data/notes.txt", "来自 data"},
		{"/backup/b.txt", "来自 backup"},
	} {
		got := do(t, s, http.MethodGet, tc.url, nil, nil)
		if got.code != http.StatusOK {
			t.Errorf("GET %s 返回 %d：%s", tc.url, got.code, got.body)
			continue
		}
		if got.body != tc.want {
			t.Errorf("GET %s = %q，期望 %q", tc.url, got.body, tc.want)
		}
	}

	// 跨根不可见：data 下没有 b.txt，backup 下没有 notes.txt。
	for _, url := range []string{"/data/b.txt", "/backup/notes.txt"} {
		if got := do(t, s, http.MethodGet, url, nil, nil); got.code == http.StatusOK {
			t.Errorf("GET %s 竟然拿到了别的根的文件：%s", url, got.body)
		}
	}
}

// TestMultiRootRootIndexListsMounts 虚拟根 `/` 给出挂载点列表，
// 而不是 404 —— 它是面包屑「根目录」的落点。
func TestMultiRootRootIndexListsMounts(t *testing.T) {
	s, _, _ := multiRootFixture(t, "-A")

	got := do(t, s, http.MethodGet, "/?json", nil, nil)
	if got.code != http.StatusOK {
		t.Fatalf("GET /?json 返回 %d：%s", got.code, got.body)
	}
	for _, want := range []string{`"name":"data"`, `"name":"backup"`,
		`"path":"/data"`, `"path":"/backup"`, `"is_dir":true`} {
		if !strings.Contains(got.body, want) {
			t.Errorf("挂载点列表里缺少 %v：%s", want, got.body)
		}
	}

	// 浏览器拿到的应当是一个正常页面，不是错误。
	html := do(t, s, http.MethodGet, "/", nil, map[string]string{"Accept": "text/html"})
	if html.code != http.StatusOK {
		t.Errorf("GET / （HTML）返回 %d", html.code)
	}

	// 虚拟根上没有真实目录，搜索/打包/解压都要明确拒绝，而不是 500。
	for _, q := range []string{"?q=x", "?zip", "?extract"} {
		if got := do(t, s, http.MethodGet, "/"+q, nil, nil); got.code != http.StatusBadRequest {
			t.Errorf("GET /%s 返回 %d，期望 400", q, got.code)
		}
	}
}

// TestMultiRootUnknownPrefix404 未知首段是「没这个挂载点」（404），
// 不是「没权限」（403）—— 混在一起会把打错地址说成没权限。
func TestMultiRootUnknownPrefix404(t *testing.T) {
	s, _, _ := multiRootFixture(t, "-A")

	for _, m := range []string{http.MethodGet, http.MethodPut, http.MethodDelete} {
		got := do(t, s, m, "/nope/x.txt", nil, nil)
		if got.code != http.StatusNotFound {
			t.Errorf("%s /nope/x.txt 返回 %d，期望 404：%s", m, got.code, got.body)
		}
	}
}

// TestMultiRootListingPathsCarryLabel 列举结果里的 path 必须带标签，
// 否则前端点进去会掉到别的根上。
func TestMultiRootListingPathsCarryLabel(t *testing.T) {
	s, _, _ := multiRootFixture(t, "-A")

	got := do(t, s, http.MethodGet, "/data/?json", nil, nil)
	if got.code != http.StatusOK {
		t.Fatalf("返回 %d：%s", got.code, got.body)
	}
	if !strings.Contains(got.body, `"path":"/data/notes.txt"`) {
		t.Errorf("条目 path 没带标签：%s", got.body)
	}
	if strings.Contains(got.body, `"path":"/notes.txt"`) {
		t.Errorf("条目 path 丢了标签（会被当成根下的文件）：%s", got.body)
	}
}

// TestMultiRootWritePathsCarryLabel 上传回包里的 saved 也要带标签 ——
// 前端据此刷新列表，缺了标签就会去错的目录找。
func TestMultiRootWritePathsCarryLabel(t *testing.T) {
	s, _, _ := multiRootFixture(t, "-A")

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, err := mw.CreateFormFile("file", "new.txt")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fw.Write([]byte("x")); err != nil {
		t.Fatal(err)
	}
	if err := mw.Close(); err != nil {
		t.Fatal(err)
	}

	got := do(t, s, http.MethodPost, "/backup/", &buf,
		map[string]string{"Content-Type": mw.FormDataContentType()})
	if got.code != http.StatusCreated && got.code != http.StatusOK {
		t.Fatalf("上传返回 %d：%s", got.code, got.body)
	}
	if !strings.Contains(got.body, "/backup/new.txt") {
		t.Errorf("上传回包里的路径没带标签：%s", got.body)
	}
}

// TestMultiRootSearchWorks 搜索在每个根内各自生效（Search 走的是 Rel，
// 天然带标签）。
func TestMultiRootSearchWorks(t *testing.T) {
	s, _, _ := multiRootFixture(t, "-A", "--allow-search")

	got := do(t, s, http.MethodGet, "/backup/?q=b&json", nil, nil)
	if got.code != http.StatusOK {
		t.Fatalf("搜索返回 %d：%s", got.code, got.body)
	}
	if !strings.Contains(got.body, "/backup/b.txt") {
		t.Errorf("搜索结果没带标签：%s", got.body)
	}
}

// TestMultiRootSettingsDisablesSwitch 多根下「服务根目录」只读：
// 下发挂载点列表、root_switchable=false，PUT root 明确报错。
func TestMultiRootSettingsDisablesSwitch(t *testing.T) {
	s, _, _ := multiRootFixture(t, "-A")
	hdr := map[string]string{"Authorization": basicAuth("boss", "pw")}

	got := do(t, s, http.MethodGet, "/fs/settings", nil, hdr)
	if got.code != http.StatusOK {
		t.Fatalf("读取设置返回 %d：%s", got.code, got.body)
	}
	for _, want := range []string{`"multi_root":true`, `"root_switchable":false`,
		`"label":"data"`, `"label":"backup"`, `"url":"/data"`} {
		if !strings.Contains(got.body, want) {
			t.Errorf("设置响应里缺少 %v：%s", want, got.body)
		}
	}

	// 改根：明确 400，而不是静默忽略或 500。
	put := do(t, s, http.MethodPut, "/fs/settings",
		strings.NewReader(`{"root":"/tmp"}`), hdr)
	if put.code != http.StatusBadRequest {
		t.Fatalf("多根下改根返回 %d，期望 400：%s", put.code, put.body)
	}
	if !strings.Contains(put.body, "多根") {
		t.Errorf("错误信息没说清原因：%s", put.body)
	}

	// 相对路径也一样被拦（不能因为「相对路径要先拼当前根」就漏过去）。
	rel := do(t, s, http.MethodPut, "/fs/settings",
		strings.NewReader(`{"root":"sub"}`), hdr)
	if rel.code != http.StatusBadRequest {
		t.Errorf("相对路径改根返回 %d，期望 400：%s", rel.code, rel.body)
	}

	// 其余设置项不受影响：标题仍可改。
	ok := do(t, s, http.MethodPut, "/fs/settings",
		strings.NewReader(`{"title":"多根站点"}`), hdr)
	if ok.code != http.StatusOK {
		t.Errorf("多根下改标题返回 %d，期望 200：%s", ok.code, ok.body)
	}
}

// TestMultiRootNestedRejected 嵌套的两个根直接拒绝启动，别让用户猜。
func TestMultiRootNestedRejected(t *testing.T) {
	base := t.TempDir()
	outer := filepath.Join(base, "outer")
	inner := filepath.Join(outer, "inner")
	if err := os.MkdirAll(inner, 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := testConfigRoots(t, []string{outer, inner}, "-A")
	if _, err := New(cfg, os.DirFS(filepath.Join("..", "..", "assets"))); err == nil {
		t.Fatal("嵌套的根应当拒绝启动")
	}
}

// TestSingleRootUnchanged 是这次改动的回归底线：**只给一个目录时，
// 行为必须与支持多根之前一模一样** —— 服务在 /、没有前缀、换根仍可用。
func TestSingleRootUnchanged(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "notes.txt"), []byte("单根"))
	s := newTestServer(t, testConfig(t, root, "-A", "-a", "boss:pw@/:rw"))

	if s.MultiRoot() {
		t.Fatal("单根却被当成了多根")
	}
	hdr := map[string]string{"Authorization": basicAuth("boss", "pw")}

	// 没有前缀：直接服务在 /。
	if got := do(t, s, http.MethodGet, "/notes.txt", nil, hdr); got.code != http.StatusOK {
		t.Errorf("GET /notes.txt 返回 %d", got.code)
	}
	// 根目录列举不带任何前缀。
	got := do(t, s, http.MethodGet, "/?json", nil, hdr)
	if !strings.Contains(got.body, `"path":"/notes.txt"`) {
		t.Errorf("单根的条目 path 带了前缀？%s", got.body)
	}
	// 换根仍然可用。
	put := do(t, s, http.MethodPut, "/fs/settings",
		strings.NewReader(`{"root":"`+root+`"}`), hdr)
	if put.code != http.StatusOK {
		t.Errorf("单根下改根返回 %d，期望 200：%s", put.code, put.body)
	}
	// 设置里也不该出现多根的标记。
	set := do(t, s, http.MethodGet, "/fs/settings", nil, hdr)
	if strings.Contains(set.body, `"multi_root":true`) {
		t.Errorf("单根却下发了 multi_root=true：%s", set.body)
	}
}
