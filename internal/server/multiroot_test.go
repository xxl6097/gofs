package server

import (
	"bytes"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/xxl6097/gofs/internal/config"
)

// testConfigRoots 构造一个挂多个服务目录的配置（位置参数全给）。
func testConfigRoots(t *testing.T, roots []string, extra ...string) *config.Config {
	t.Helper()
	// 同 testConfig：三个存储文件都置空，免得写进开发者真实配置。
	args := []string{"-b", "127.0.0.1", "-p", "0", "-log-format", "none",
		"--no-upload-dated", "--key-file", "", "--user-file", "", "--settings-file", ""}
	args = append(args, extra...)
	args = append(args, roots...)
	cfg, err := config.Parse(args, io.Discard)
	if err != nil {
		t.Fatalf("解析配置失败: %v", err)
	}
	return cfg
}

// multiRootFixture 造主目录 data（服务在 /）与附加站点 backup（服务在 /backup）。
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

// TestMultiRootPrimaryAtRootExtrasUnderLabel 是这次的核心：主目录不带前缀、
// 附加站点带标签，且两者**互相看不见**。
func TestMultiRootPrimaryAtRootExtrasUnderLabel(t *testing.T) {
	s, _, _ := multiRootFixture(t, "-A")

	for _, tc := range []struct{ url, want string }{
		{"/notes.txt", "来自 data"},      // 主目录：服务在 /
		{"/backup/b.txt", "来自 backup"}, // 附加站点：带标签
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

	// 跨根不可见。
	for _, url := range []string{"/b.txt", "/backup/notes.txt"} {
		if got := do(t, s, http.MethodGet, url, nil, nil); got.code == http.StatusOK {
			t.Errorf("GET %s 竟然拿到了别的根的文件：%s", url, got.body)
		}
	}
}

// TestMultiRootRootShowsSites 确认根目录**列出附加站点**（挂载点可见），
// 同时主目录自己的真实内容照旧。
func TestMultiRootRootShowsSites(t *testing.T) {
	s, _, _ := multiRootFixture(t, "-A", "--allow-search")

	got := do(t, s, http.MethodGet, "/?json", nil, nil)
	if got.code != http.StatusOK {
		t.Fatalf("GET /?json 返回 %d：%s", got.code, got.body)
	}
	// 主目录的真实条目还在（不带前缀）。
	if !strings.Contains(got.body, `"path":"/notes.txt"`) {
		t.Errorf("根目录没列出主目录的真实内容：%s", got.body)
	}
	// 附加站点作为条目出现，并带 site 标记（前端据此打「站点」标、
	// 并隐藏会破坏它的操作）。
	if !strings.Contains(got.body, `"path":"/backup"`) {
		t.Errorf("附加站点没出现在根目录列表里：%s", got.body)
	}
	if !strings.Contains(got.body, `"site":true`) {
		t.Errorf("站点条目没有 site 标记：%s", got.body)
	}

	// 排序与主目录条目一致（目录在前、再按名字）：backup < notes.txt。
	iSite := strings.Index(got.body, `"path":"/backup"`)
	iFile := strings.Index(got.body, `"path":"/notes.txt"`)
	if iSite < 0 || iFile < 0 || iSite > iFile {
		t.Errorf("站点没有按统一顺序排进去（目录应在文件前）：%s", got.body)
	}

	// 子目录里**不该**再列一遍站点 —— 站点只挂在根这一层。
	sub := do(t, s, http.MethodGet, "/?json", nil, nil)
	_ = sub
	deep := do(t, s, http.MethodGet, "/nope/?json", nil, nil)
	if strings.Contains(deep.body, `"site":true`) {
		t.Errorf("子目录里不该出现站点条目：%s", deep.body)
	}

	// 搜索照旧可用（它作用在主目录这个真实目录上）。
	if s2 := do(t, s, http.MethodGet, "/?q=notes&json", nil, nil); s2.code != http.StatusOK {
		t.Errorf("主目录搜索返回 %d", s2.code)
	}
}

// TestRootSiteEntryCannotBeDeleted 确认「站点条目可见」没有打开删库的门：
// 删除/移动打到站点根目录上必须是 403（服务端按解析后的绝对路径判定）。
func TestRootSiteEntryCannotBeDeleted(t *testing.T) {
	s, _, backupDir := multiRootFixture(t, "-A", "--allow-delete")

	for _, m := range []string{http.MethodDelete, MethodMove} {
		req := do(t, s, m, "/backup?recursive", nil, nil)
		if req.code == http.StatusOK || req.code == http.StatusNoContent {
			t.Errorf("%s /backup 竟然成功了：%s", m, req.body)
		}
	}
	if _, err := os.Stat(filepath.Join(backupDir, "b.txt")); err != nil {
		t.Fatalf("站点里的文件被动了: %v", err)
	}
}

// TestRootSitesVisibleToAnyoneWhoCanListRoot 记录这套 URL 形状的一个必然结果：
// **能列出主目录的账号就能看到站点名**。
//
// 权限规则是最长前缀匹配、没有 deny，而站点路径都在 `/` 之下 —— 所以「读得到
// 主目录、却看不到某个站点」这个组合在当前形状下造不出来。反过来说，一个只被
// 授予某个站点路径的账号连主目录都列不了（下面第二条断言）。
func TestRootSitesVisibleToAnyoneWhoCanListRoot(t *testing.T) {
	base := t.TempDir()
	mainDir := filepath.Join(base, "main")
	publicDir := filepath.Join(base, "secret")
	for _, d := range []string{mainDir, publicDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	writeFile(t, filepath.Join(mainDir, "a.txt"), []byte("m"))

	// guest 只有 /public 的读权限，对 / 与 /secret 都没有规则命中。
	cfg := testConfigRoots(t, []string{mainDir, publicDir},
		"-A", "-a", "boss:pw@/:rw", "-a", "guest:g@/public:r")
	s := newTestServer(t, cfg)

	// 能列主目录的（管理员）看得见站点名 —— 这是形状决定的。
	admin := map[string]string{"Authorization": basicAuth("boss", "pw")}
	got := do(t, s, http.MethodGet, "/?json", nil, admin)
	if !strings.Contains(got.body, `"path":"/secret"`) {
		t.Errorf("管理员应当看到站点条目：%s", got.body)
	}

	// 只被授予某条路径的账号，连主目录都列不了 —— 于是也就看不到站点列表。
	guest := map[string]string{"Authorization": basicAuth("guest", "g")}
	if g := do(t, s, http.MethodGet, "/?json", nil, guest); g.code == http.StatusOK {
		t.Errorf("只授予 /public 的账号不该能列出主目录：%d %s", g.code, g.body)
	}
}

// TestMultiRootUnknownPrefixIsPrimaryRelative 混合形状下「未知前缀」不再特殊：
// 它就是主目录下的普通路径（这是所选 URL 形状的必然结果）。
func TestMultiRootUnknownPrefixIsPrimaryRelative(t *testing.T) {
	s, _, _ := multiRootFixture(t, "-A")

	// 不存在 → 404（不是 403，也不是「未知挂载点」）。
	if got := do(t, s, http.MethodGet, "/nope/x.txt", nil, nil); got.code != http.StatusNotFound {
		t.Errorf("GET /nope/x.txt 返回 %d，期望 404：%s", got.code, got.body)
	}
	// ⚠️ 但 PUT 会把它创建出来（主目录下的路径），这是真实的行为变化。
	if got := do(t, s, http.MethodPut, "/nope/x.txt", strings.NewReader("x"), nil); got.code != http.StatusCreated {
		t.Errorf("PUT /nope/x.txt 返回 %d，期望 201（它被当成主目录下的路径）", got.code)
	}
}

// TestMultiRootListingPathsCarryPrefixOnlyForExtras 列举里的 path：
// 主目录不带前缀、附加站点带标签。
func TestMultiRootListingPathsCarryPrefixOnlyForExtras(t *testing.T) {
	s, _, _ := multiRootFixture(t, "-A")

	main := do(t, s, http.MethodGet, "/?json", nil, nil)
	if !strings.Contains(main.body, `"path":"/notes.txt"`) ||
		strings.Contains(main.body, `"path":"/data/notes.txt"`) {
		t.Errorf("主目录条目不该带前缀：%s", main.body)
	}

	extra := do(t, s, http.MethodGet, "/backup/?json", nil, nil)
	if !strings.Contains(extra.body, `"path":"/backup/b.txt"`) {
		t.Errorf("附加站点条目应带标签：%s", extra.body)
	}
}

// TestMultiRootWritePaths 上传回包的 saved：主目录无前缀、站点带标签。
func TestMultiRootWritePaths(t *testing.T) {
	s, _, _ := multiRootFixture(t, "-A")

	upload := func(target, name string) string {
		var buf bytes.Buffer
		mw := multipart.NewWriter(&buf)
		fw, err := mw.CreateFormFile("file", name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := fw.Write([]byte("x")); err != nil {
			t.Fatal(err)
		}
		if err := mw.Close(); err != nil {
			t.Fatal(err)
		}
		got := do(t, s, http.MethodPost, target, &buf,
			map[string]string{"Content-Type": mw.FormDataContentType()})
		if got.code != http.StatusCreated && got.code != http.StatusOK {
			t.Fatalf("上传到 %s 返回 %d：%s", target, got.code, got.body)
		}
		return got.body
	}

	if body := upload("/backup/", "new.txt"); !strings.Contains(body, "/backup/new.txt") {
		t.Errorf("站点上传回包应带标签：%s", body)
	}
	if body := upload("/", "root.txt"); strings.Contains(body, "/data/root.txt") {
		t.Errorf("主目录上传回包不该带前缀：%s", body)
	}
}

// TestMultiRootSearchScoped 搜索在每个根内各自生效，结果带对应前缀。
func TestMultiRootSearchScoped(t *testing.T) {
	s, _, _ := multiRootFixture(t, "-A", "--allow-search")

	got := do(t, s, http.MethodGet, "/backup/?q=b&json", nil, nil)
	if got.code != http.StatusOK {
		t.Fatalf("搜索返回 %d：%s", got.code, got.body)
	}
	if !strings.Contains(got.body, "/backup/b.txt") {
		t.Errorf("搜索结果没带标签：%s", got.body)
	}
}

// TestMultiRootDeleteSiteRootRefused 确认删除打不到站点根目录上 ——
// 这是站点一旦可挂就必须堵住的洞。
func TestMultiRootDeleteSiteRootRefused(t *testing.T) {
	s, _, backupDir := multiRootFixture(t, "-A", "--allow-delete")

	got := do(t, s, http.MethodDelete, "/backup?recursive", nil, nil)
	if got.code != http.StatusForbidden {
		t.Fatalf("DELETE /backup 返回 %d，期望 403：%s", got.code, got.body)
	}
	// 目录必须还在。
	if _, err := os.Stat(backupDir); err != nil {
		t.Fatalf("站点目录被删掉了: %v", err)
	}
	// 里面的文件也还在。
	if _, err := os.Stat(filepath.Join(backupDir, "b.txt")); err != nil {
		t.Fatalf("站点里的文件被删掉了: %v", err)
	}
}

// TestMultiRootSettingsAdvertisesSites 设置接口要如实描述「谁是主、谁可移除」。
func TestMultiRootSettingsAdvertisesSites(t *testing.T) {
	s, dataDir, _ := multiRootFixture(t, "-A")
	hdr := map[string]string{"Authorization": basicAuth("boss", "pw")}
	// 解析成真实路径再比：Resolver 会把根解析掉，macOS 上 /var 是软链。
	if real, err := filepath.EvalSymlinks(dataDir); err == nil {
		dataDir = real
	}

	got := do(t, s, http.MethodGet, "/fs/settings", nil, hdr)
	if got.code != http.StatusOK {
		t.Fatalf("读取设置返回 %d：%s", got.code, got.body)
	}
	for _, want := range []string{
		`"multi_root":true`,
		`"root_switchable":false`, // 有站点时不换主根
		`"sites_editable":true`,
		`"url":"/"`,       // 主目录
		`"url":"/backup"`, // 附加站点
		`"primary":true`,
		`"label":"backup"`,
		`"root":"` + dataDir + `"`,
	} {
		if !strings.Contains(got.body, want) {
			t.Errorf("设置响应里缺少 %v：%s", want, got.body)
		}
	}

	// 有站点时改主根仍被拒。
	put := do(t, s, http.MethodPut, "/fs/settings",
		strings.NewReader(`{"root":"/tmp"}`), hdr)
	if put.code != http.StatusBadRequest {
		t.Errorf("有站点时改主根返回 %d，期望 400：%s", put.code, put.body)
	}
}

// TestMultiRootNestedRejected 嵌套的两个根直接拒绝启动。
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

// TestSingleRootUnchanged 是这次改动的回归底线：**只给一个目录时行为必须
// 与支持多站点之前一模一样** —— 服务在 /、没有前缀、换根仍可用。
func TestSingleRootUnchanged(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "notes.txt"), []byte("单根"))
	s := newTestServer(t, testConfig(t, root, "-A", "-a", "boss:pw@/:rw"))

	if s.MultiRoot() {
		t.Fatal("单根却被当成了多站点")
	}
	hdr := map[string]string{"Authorization": basicAuth("boss", "pw")}

	if got := do(t, s, http.MethodGet, "/notes.txt", nil, hdr); got.code != http.StatusOK {
		t.Errorf("GET /notes.txt 返回 %d", got.code)
	}
	got := do(t, s, http.MethodGet, "/?json", nil, hdr)
	if !strings.Contains(got.body, `"path":"/notes.txt"`) {
		t.Errorf("单根的条目 path 带了前缀？%s", got.body)
	}
	put := do(t, s, http.MethodPut, "/fs/settings",
		strings.NewReader(`{"root":"`+root+`"}`), hdr)
	if put.code != http.StatusOK {
		t.Errorf("单根下改根返回 %d，期望 200：%s", put.code, put.body)
	}
	set := do(t, s, http.MethodGet, "/fs/settings", nil, hdr)
	if strings.Contains(set.body, `"multi_root":true`) {
		t.Errorf("单根却下发了 multi_root=true：%s", set.body)
	}
	if !strings.Contains(set.body, `"sites_editable":true`) {
		t.Errorf("单根下应当允许增删站点：%s", set.body)
	}
}

// ---- 运行期增删站点 ----

// sitesServer 造一个「单根 + 可加站点」的服务，并给一个允许范围内的候选目录。
func sitesServer(t *testing.T, settingsFile string) (*Server, string, string) {
	t.Helper()
	base := t.TempDir()
	mainDir := filepath.Join(base, "main")
	siteDir := filepath.Join(base, "site")
	if err := os.MkdirAll(mainDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(siteDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(mainDir, "a.txt"), []byte("main"))
	writeFile(t, filepath.Join(siteDir, "s.txt"), []byte("site"))

	// --root-allow 指到 base，这样 main 与 site 都在允许范围内。
	// guest 是合法的只读账号，供「非管理员」用例使用。
	cfg := testConfigRoots(t, []string{mainDir}, "-A", "-a", "boss:pw@/:rw",
		"-a", "guest:g@/:r", "--root-allow", base)
	cfg.SettingsFile = settingsFile
	return newTestServer(t, cfg), mainDir, siteDir
}

func adminSettingsHdr() map[string]string {
	return map[string]string{"Authorization": basicAuth("boss", "pw")}
}

// TestAddSiteRoundTrip 覆盖新增：立刻可访问、出现在设置里、且主目录不受影响。
func TestAddSiteRoundTrip(t *testing.T) {
	s, mainDir, siteDir := sitesServer(t, "")
	hdr := adminSettingsHdr()

	put := do(t, s, http.MethodPut, "/fs/settings",
		strings.NewReader(`{"add_site":{"path":"`+siteDir+`"}}`), hdr)
	if put.code != http.StatusOK {
		t.Fatalf("新增站点返回 %d：%s", put.code, put.body)
	}
	if !strings.Contains(put.body, `"url":"/site"`) {
		t.Errorf("响应里没有新站点：%s", put.body)
	}

	// 立即可访问，且带标签。
	if got := do(t, s, http.MethodGet, "/site/s.txt", nil, hdr); got.code != http.StatusOK || got.body != "site" {
		t.Errorf("GET /site/s.txt = %d %q", got.code, got.body)
	}
	// 主目录不受影响（这正是选混合形状的理由）。
	if got := do(t, s, http.MethodGet, "/a.txt", nil, hdr); got.code != http.StatusOK || got.body != "main" {
		t.Errorf("主目录被影响了：%d %q", got.code, got.body)
	}
	_ = mainDir
}

// TestAddSitePersists 确认落盘：同一份设置文件起第二个实例，站点还在。
func TestAddSitePersists(t *testing.T) {
	sf := filepath.Join(t.TempDir(), "settings.json")
	s, mainDir, siteDir := sitesServer(t, sf)

	if put := do(t, s, http.MethodPut, "/fs/settings",
		strings.NewReader(`{"add_site":{"path":"`+siteDir+`","label":"备份"}}`), adminSettingsHdr()); put.code != http.StatusOK {
		t.Fatalf("新增站点返回 %d：%s", put.code, put.body)
	}

	// 新实例（＝重启）用同一份设置文件。
	cfg := testConfigRoots(t, []string{mainDir}, "-A", "-a", "boss:pw@/:rw",
		"--root-allow", filepath.Dir(siteDir))
	cfg.SettingsFile = sf
	s2 := newTestServer(t, cfg)

	if got := do(t, s2, http.MethodGet, "/备份/s.txt", nil, adminSettingsHdr()); got.code != http.StatusOK {
		t.Fatalf("重启后站点没恢复：%d %s", got.code, got.body)
	}
	set := do(t, s2, http.MethodGet, "/fs/settings", nil, adminSettingsHdr())
	if !strings.Contains(set.body, `"label":"备份"`) {
		t.Errorf("重启后标签变了：%s", set.body)
	}
}

// TestAddSiteRejects 覆盖各类拒绝，并确认错误原因原样带给管理员。
func TestAddSiteRejects(t *testing.T) {
	s, mainDir, siteDir := sitesServer(t, "")
	hdr := adminSettingsHdr()
	add := func(path, label string) resp {
		body := `{"add_site":{"path":"` + path + `","label":"` + label + `"}}`
		return do(t, s, http.MethodPut, "/fs/settings", strings.NewReader(body), hdr)
	}

	if got := add("", ""); got.code != http.StatusBadRequest {
		t.Errorf("空路径返回 %d，期望 400", got.code)
	}
	if got := add(filepath.Join(mainDir, "nope"), ""); got.code != http.StatusBadRequest ||
		!strings.Contains(got.body, "不存在") {
		t.Errorf("不存在的目录：%d %s", got.code, got.body)
	}
	if got := add(filepath.Join(mainDir, "a.txt"), ""); got.code != http.StatusBadRequest ||
		!strings.Contains(got.body, "不是目录") {
		t.Errorf("文件：%d %s", got.code, got.body)
	}
	// 显式写了 --root-allow 时按它收窄：白名单外的目录被拒。
	outside := t.TempDir()
	if got := add(outside, ""); got.code != http.StatusBadRequest ||
		!strings.Contains(got.body, "超出白名单") {
		t.Errorf("白名单外：%d %s", got.code, got.body)
	}
	// 主目录自己（重复）。
	if got := add(mainDir, ""); got.code != http.StatusBadRequest ||
		!strings.Contains(got.body, "已经在服务中") {
		t.Errorf("重复目录：%d %s", got.code, got.body)
	}
	// 主目录的子目录（嵌套）。
	sub := filepath.Join(mainDir, "sub")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	if got := add(sub, ""); got.code != http.StatusBadRequest ||
		!strings.Contains(got.body, "嵌套") {
		t.Errorf("嵌套：%d %s", got.code, got.body)
	}

	// 正常加一个，然后重复加它。
	if put := add(siteDir, ""); put.code != http.StatusOK {
		t.Fatalf("正常新增失败：%d %s", put.code, put.body)
	}
	if got := add(siteDir, ""); got.code != http.StatusBadRequest {
		t.Errorf("重复：%d %s", got.code, got.body)
	}
}

// TestAddSiteUnrestrictedByDefault 是这次行为变化的钉住点：**没写白名单时
// 不限目录** —— 只要你是管理员，就能把机器上任何已存在的目录挂成站点。
//
// 与「换根」的默认值刻意不同：换根是「在目录树里跳来跳去」，默认限在启动
// 主目录的兄弟目录内；而加站点正是为了免掉「改启动参数重启」这件事。
func TestAddSiteUnrestrictedByDefault(t *testing.T) {
	base := t.TempDir()
	mainDir := filepath.Join(base, "main")
	if err := os.MkdirAll(mainDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// 完全不相干的一处（另一个临时根，连父目录都不同）。
	far := filepath.Join(t.TempDir(), "far")
	if err := os.MkdirAll(far, 0o755); err != nil {
		t.Fatal(err)
	}

	// 注意：**不给** --root-allow。
	cfg := testConfigRoots(t, []string{mainDir}, "-A", "-a", "boss:pw@/:rw")
	s := newTestServer(t, cfg)

	got := do(t, s, http.MethodPut, "/fs/settings",
		strings.NewReader(`{"add_site":{"path":"`+far+`"}}`), adminSettingsHdr())
	if got.code != http.StatusOK {
		t.Fatalf("默认应不限目录，却返回 %d：%s", got.code, got.body)
	}
	if !strings.Contains(got.body, `"url":"/far"`) {
		t.Errorf("站点没挂上：%s", got.body)
	}
	// 换根仍然受限（默认＝主目录的父目录）—— 两套默认值是分开的。
	if !strings.Contains(got.body, `"allow_roots"`) {
		t.Errorf("换根的白名单不应消失：%s", got.body)
	}
	put := do(t, s, http.MethodPut, "/fs/settings",
		strings.NewReader(`{"root":"`+far+`"}`), adminSettingsHdr())
	if put.code != http.StatusBadRequest {
		t.Errorf("换根不该跟着放宽：%d %s", put.code, put.body)
	}
}

// TestAddSiteExplicitWhitelistTightens 确认显式白名单能收窄（想收紧的部署有这个开关）。
func TestAddSiteExplicitWhitelistTightens(t *testing.T) {
	base := t.TempDir()
	mainDir := filepath.Join(base, "main")
	inside := filepath.Join(base, "inside")
	for _, d := range []string{mainDir, inside} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	far := filepath.Join(t.TempDir(), "far")
	if err := os.MkdirAll(far, 0o755); err != nil {
		t.Fatal(err)
	}

	cfg := testConfigRoots(t, []string{mainDir}, "-A", "-a", "boss:pw@/:rw",
		"--root-allow", base)
	s := newTestServer(t, cfg)
	hdr := adminSettingsHdr()

	// 白名单内：可以。
	if got := do(t, s, http.MethodPut, "/fs/settings",
		strings.NewReader(`{"add_site":{"path":"`+inside+`"}}`), hdr); got.code != http.StatusOK {
		t.Fatalf("白名单内应可新增：%d %s", got.code, got.body)
	}
	// 白名单外：拒绝，并说明白名单内容。
	got := do(t, s, http.MethodPut, "/fs/settings",
		strings.NewReader(`{"add_site":{"path":"`+far+`"}}`), hdr)
	if got.code != http.StatusBadRequest || !strings.Contains(got.body, "超出白名单") {
		t.Errorf("白名单外应被拒：%d %s", got.code, got.body)
	}
	// 白名单要下发给界面（空数组表示不限制）。
	set := do(t, s, http.MethodGet, "/fs/settings", nil, hdr)
	if !strings.Contains(set.body, `"site_roots"`) {
		t.Errorf("设置里应带上白名单：%s", set.body)
	}
}

// TestRemoveSite 覆盖移除：立即生效、落盘、未知标签 404、主目录 400。
func TestRemoveSite(t *testing.T) {
	sf := filepath.Join(t.TempDir(), "settings.json")
	s, mainDir, siteDir := sitesServer(t, sf)
	hdr := adminSettingsHdr()

	if put := do(t, s, http.MethodPut, "/fs/settings",
		strings.NewReader(`{"add_site":{"path":"`+siteDir+`"}}`), hdr); put.code != http.StatusOK {
		t.Fatalf("新增失败：%s", put.body)
	}
	if got := do(t, s, http.MethodGet, "/site/s.txt", nil, hdr); got.code != http.StatusOK {
		t.Fatalf("新增后不可访问：%d", got.code)
	}

	// 未知标签：404（资源不存在）。
	if got := do(t, s, http.MethodPut, "/fs/settings",
		strings.NewReader(`{"remove_site":"nope"}`), hdr); got.code != http.StatusNotFound {
		t.Errorf("未知标签返回 %d，期望 404：%s", got.code, got.body)
	}
	// 主目录：400。
	prim := do(t, s, http.MethodGet, "/fs/settings", nil, hdr)
	if !strings.Contains(prim.body, `"removable":false`) {
		t.Errorf("主目录应当不可移除：%s", prim.body)
	}
	mainLabel := ""
	if i := strings.Index(prim.body, `"label":"`); i >= 0 {
		rest := prim.body[i+len(`"label":"`):]
		mainLabel = rest[:strings.Index(rest, `"`)]
	}
	if got := do(t, s, http.MethodPut, "/fs/settings",
		strings.NewReader(`{"remove_site":"`+mainLabel+`"}`), hdr); got.code != http.StatusBadRequest {
		t.Errorf("移除主目录返回 %d，期望 400：%s", got.code, got.body)
	}

	// 正常移除。
	if got := do(t, s, http.MethodPut, "/fs/settings",
		strings.NewReader(`{"remove_site":"site"}`), hdr); got.code != http.StatusOK {
		t.Fatalf("移除失败：%d %s", got.code, got.body)
	}
	// 移除后回到主目录下的普通路径（不存在 → 404），且形状回到单根。
	if got := do(t, s, http.MethodGet, "/site/s.txt", nil, hdr); got.code != http.StatusNotFound {
		t.Errorf("移除后仍可访问：%d", got.code)
	}
	set := do(t, s, http.MethodGet, "/fs/settings", nil, hdr)
	if strings.Contains(set.body, `"multi_root":true`) {
		t.Errorf("移除最后一个站点后应回到单根：%s", set.body)
	}
	if !strings.Contains(set.body, `"root_switchable":true`) {
		t.Errorf("回到单根后换根应恢复可用：%s", set.body)
	}

	// 落盘也要跟着更新：新实例不应再有这个站点。
	cfg := testConfigRoots(t, []string{mainDir}, "-A", "-a", "boss:pw@/:rw",
		"--root-allow", filepath.Dir(siteDir))
	cfg.SettingsFile = sf
	s2 := newTestServer(t, cfg)
	if got := do(t, s2, http.MethodGet, "/site/s.txt", nil, adminSettingsHdr()); got.code != http.StatusNotFound {
		t.Errorf("移除没有落盘：%d", got.code)
	}
}

// TestSitesRequireAdminAndSwitch 确认权限与开关。
func TestSitesRequireAdminAndSwitch(t *testing.T) {
	s, _, siteDir := sitesServer(t, "")
	body := `{"add_site":{"path":"` + siteDir + `"}}`

	if got := do(t, s, http.MethodPut, "/fs/settings", strings.NewReader(body), nil); got.code != http.StatusUnauthorized {
		t.Errorf("匿名返回 %d，期望 401", got.code)
	}
	guest := map[string]string{"Authorization": basicAuth("guest", "g")}
	if got := do(t, s, http.MethodPut, "/fs/settings", strings.NewReader(body), guest); got.code != http.StatusForbidden {
		t.Errorf("只读账号返回 %d，期望 403", got.code)
	}

	// --no-root-switch：整个站点增删都关掉。
	base := t.TempDir()
	mainDir := filepath.Join(base, "main")
	if err := os.MkdirAll(mainDir, 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := testConfigRoots(t, []string{mainDir}, "-A", "-a", "boss:pw@/:rw", "--no-root-switch")
	off := newTestServer(t, cfg)
	hdr := adminSettingsHdr()
	if got := do(t, off, http.MethodPut, "/fs/settings", strings.NewReader(body), hdr); got.code != http.StatusForbidden {
		t.Errorf("--no-root-switch 下新增返回 %d，期望 403：%s", got.code, got.body)
	}
	set := do(t, off, http.MethodGet, "/fs/settings", nil, hdr)
	if !strings.Contains(set.body, `"sites_editable":false`) {
		t.Errorf("--no-root-switch 下不该允许编辑站点：%s", set.body)
	}
}

// TestSiteLabelStableAcrossOps 标签在一串增删里保持稳定（书签依赖它）。
func TestSiteLabelStableAcrossOps(t *testing.T) {
	s, mainDir, _ := sitesServer(t, "")
	hdr := adminSettingsHdr()
	base := filepath.Dir(mainDir) // --root-allow 指的就是这里

	mk := func(name string) string {
		p := filepath.Join(base, name)
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatal(err)
		}
		return p
	}
	add := func(path string) {
		t.Helper()
		if got := do(t, s, http.MethodPut, "/fs/settings",
			strings.NewReader(`{"add_site":{"path":"`+path+`"}}`), hdr); got.code != http.StatusOK {
			t.Fatalf("新增 %s 失败：%s", path, got.body)
		}
	}

	add(mk("aaa"))
	add(mk("bbb"))
	// 删掉 aaa 再挂一个：bbb 的标签不能被重编号。
	if got := do(t, s, http.MethodPut, "/fs/settings",
		strings.NewReader(`{"remove_site":"aaa"}`), hdr); got.code != http.StatusOK {
		t.Fatalf("移除 aaa 失败：%s", got.body)
	}
	add(mk("ccc"))

	set := do(t, s, http.MethodGet, "/fs/settings", nil, hdr)
	for _, want := range []string{`"url":"/bbb"`, `"url":"/ccc"`} {
		if !strings.Contains(set.body, want) {
			t.Errorf("标签漂移了，缺少 %v：%s", want, set.body)
		}
	}
}

// TestConcurrentSitesAndReads 用 -race 跑：一边反复增删站点，一边读目录与设置。
//
// 站点集合是运行期可变的，而读路径（列举、设置、banner）全都无锁读它 ——
// 这条测试就是钉住「读不会被写坏、也不会有数据竞争」。
func TestConcurrentSitesAndReads(t *testing.T) {
	s, _, siteDir := sitesServer(t, "")
	hdr := adminSettingsHdr()
	addBody := `{"add_site":{"path":"` + siteDir + `"}}`

	// 标签**不回收**（旧书签不能被悄悄指到别处），所以第二次新增拿到的是
	// site-2、site-3…… 必须按响应里给的标签来删，不能写死 "site"。
	addedLabel := func(body string) string {
		// 直接解 JSON：主目录的 url 也是 "/…"，靠字符串找会先命中它。
		var doc struct {
			Roots []struct {
				Label     string `json:"label"`
				Removable bool   `json:"removable"`
			} `json:"roots"`
		}
		if err := json.Unmarshal([]byte(body), &doc); err != nil {
			return ""
		}
		for _, r := range doc.Roots {
			if r.Removable {
				return r.Label
			}
		}
		return ""
	}

	var wg sync.WaitGroup
	// stop 由写者跑完后关闭；读者靠它退出。不能先 wg.Wait() 再关（会死锁）。
	stop := make(chan struct{})

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
				do(t, s, http.MethodGet, "/?json", nil, hdr)
				do(t, s, http.MethodGet, "/fs/settings", nil, hdr)
				do(t, s, http.MethodGet, "/site/s.txt", nil, hdr)
			}
		}()
	}

	wg.Add(1)
	go func() {
		defer wg.Done()
		defer close(stop)
		for i := 0; i < 5; i++ {
			got := do(t, s, http.MethodPut, "/fs/settings",
				strings.NewReader(addBody), hdr)
			if got.code != http.StatusOK {
				t.Errorf("新增失败：%d %s", got.code, got.body)
				return
			}
			label := addedLabel(got.body)
			if label == "" {
				t.Errorf("新增响应里没有标签：%s", got.body)
				return
			}
			if got := do(t, s, http.MethodPut, "/fs/settings",
				strings.NewReader(`{"remove_site":"`+label+`"}`), hdr); got.code != http.StatusOK {
				t.Errorf("移除 %s 失败：%d %s", label, got.code, got.body)
				return
			}
		}
	}()

	wg.Wait()
}
