package server

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/xxl6097/gofs/internal/config"
)

// titleServer 起一个设置文件指向临时路径的服务。
func titleServer(t *testing.T, settingsFile string, extra ...string) *Server {
	t.Helper()
	args := append([]string{"-A", "-a", "boss:pw@/:rw", "-a", "guest:g@/:r"}, extra...)
	cfg := testConfig(t, t.TempDir(), args...)
	cfg.SettingsFile = settingsFile
	return newTestServer(t, cfg)
}

// TestTitleRoundTripAndPersist 是这次的核心：改标题 → 落盘 → 新实例读回来。
//
// 「重启后还在」正是选择落盘的理由，所以这条必须钉住 ——
// 只测「接口返回了新标题」是不够的，那样内存里对、磁盘上没写也能过。
func TestTitleRoundTripAndPersist(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "settings.json")
	s := titleServer(t, file)

	got := do(t, s, http.MethodPut, "/fs/settings",
		strings.NewReader(`{"title":"我的网盘"}`), adminHdr())
	if got.code != http.StatusOK {
		t.Fatalf("改标题返回 %d：%s", got.code, got.body)
	}
	if !strings.Contains(got.body, `"title":"我的网盘"`) {
		t.Errorf("响应里没有新标题：%s", got.body)
	}

	// 落盘了没有。
	raw, err := os.ReadFile(file)
	if err != nil {
		t.Fatalf("设置文件没写出来: %v", err)
	}
	if !strings.Contains(string(raw), "我的网盘") {
		t.Errorf("设置文件内容不对：%s", raw)
	}

	// 新实例（＝重启）应当读到新标题，而不是启动参数里的默认值。
	s2 := titleServer(t, file)
	if got := s2.settings.Title(); got != "我的网盘" {
		t.Errorf("重启后标题 = %q，期望「我的网盘」", got)
	}
}

// TestTitleFallsBackWhenFileMissingOrBroken 确认坏文件不会拖垮服务。
func TestTitleFallsBackWhenFileMissingOrBroken(t *testing.T) {
	dir := t.TempDir()

	// 文件不存在：用启动参数里的默认值。
	missing := filepath.Join(dir, "nope.json")
	if got := titleServer(t, missing, "--title", "启动值").settings.Title(); got != "启动值" {
		t.Errorf("文件不存在时标题 = %q，期望「启动值」", got)
	}

	// 文件内容损坏：同样回落，而不是报错或起不来。
	broken := filepath.Join(dir, "broken.json")
	if err := os.WriteFile(broken, []byte("{ 这不是 json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := titleServer(t, broken, "--title", "启动值").settings.Title(); got != "启动值" {
		t.Errorf("文件损坏时标题 = %q，期望回落到「启动值」", got)
	}
}

// TestTitleEmptySettingsFileKeepsMemoryOnly 确认 --settings-file "" 时只存内存。
func TestTitleEmptySettingsFileKeepsMemoryOnly(t *testing.T) {
	s := titleServer(t, "")

	if got := do(t, s, http.MethodPut, "/fs/settings",
		strings.NewReader(`{"title":"只存内存"}`), adminHdr()); got.code != http.StatusOK {
		t.Fatalf("返回 %d：%s", got.code, got.body)
	}
	if got := s.settings.Title(); got != "只存内存" {
		t.Errorf("内存里的标题 = %q", got)
	}
}

// TestTitleValidation 覆盖校验：空、控制字符、超长。
func TestTitleValidation(t *testing.T) {
	s := titleServer(t, "")
	put := func(body string) resp {
		return do(t, s, http.MethodPut, "/fs/settings", strings.NewReader(body), adminHdr())
	}

	for _, tc := range []struct{ name, body string }{
		{"空标题", `{"title":""}`},
		{"只有空白", `{"title":"   "}`},
		{"含换行", `{"title":"a\nb"}`},
		{"含制表符", `{"title":"a\tb"}`},
		{"超长", `{"title":"` + strings.Repeat("字", maxTitleRunes+1) + `"}`},
	} {
		if got := put(tc.body); got.code != http.StatusBadRequest {
			t.Errorf("%s 返回 %d，期望 400：%s", tc.name, got.code, got.body)
		}
	}

	// 边界值要放行：正好 maxTitleRunes 个字符。
	ok := put(`{"title":"` + strings.Repeat("字", maxTitleRunes) + `"}`)
	if ok.code != http.StatusOK {
		t.Errorf("正好 %d 个字符被拒了：%d %s", maxTitleRunes, ok.code, ok.body)
	}
	// 前后空白要被规整掉，而不是当成标题的一部分。
	trimmed := put(`{"title":"  空格标题  "}`)
	if trimmed.code != http.StatusOK || !strings.Contains(trimmed.body, `"title":"空格标题"`) {
		t.Errorf("标题没被 trim：%d %s", trimmed.code, trimmed.body)
	}
}

// TestTitleOnlyAdmin 确认改标题也要管理员 —— 它影响的是所有人的界面。
func TestTitleOnlyAdmin(t *testing.T) {
	s := titleServer(t, "")

	if got := do(t, s, http.MethodPut, "/fs/settings",
		strings.NewReader(`{"title":"x"}`), nil); got.code != http.StatusUnauthorized {
		t.Errorf("匿名返回 %d，期望 401", got.code)
	}
	guest := map[string]string{"Authorization": basicAuth("guest", "g")}
	if got := do(t, s, http.MethodPut, "/fs/settings",
		strings.NewReader(`{"title":"x"}`), guest); got.code == http.StatusOK {
		t.Errorf("只读账号竟然能改标题：%s", got.body)
	}
}

// TestPageCarriesTitle 确认标题进了页面外壳（前端据此设置标签页与品牌名），
// 登录页那张空壳也要带上 —— 否则品牌名会先显示默认值、登录后才跳变。
func TestPageCarriesTitle(t *testing.T) {
	s := titleServer(t, "", "--title", "示例站点")

	got := do(t, s, http.MethodGet, "/", nil,
		map[string]string{"Authorization": basicAuth("boss", "pw"), "Accept": "text/html"})
	if !strings.Contains(got.body, `"title":"示例站点"`) {
		t.Errorf("目录页没带标题：%.300s", got.body)
	}

	// 未登录：拿到的是登录外壳，里面也要有标题。
	anon := do(t, s, http.MethodGet, "/", nil, map[string]string{"Accept": "text/html"})
	if !strings.Contains(anon.body, `"title":"示例站点"`) {
		t.Errorf("登录页外壳没带标题：%.300s", anon.body)
	}
}

// TestTitleDefaultsToGofs 确认没配任何东西时是 gofs，而不是空串。
func TestTitleDefaultsToGofs(t *testing.T) {
	if config.Defaults().Title != config.DefaultTitle {
		t.Errorf("默认标题 = %q", config.Defaults().Title)
	}
	// 被显式置空（或被上层塞了空白串）时也要兜底，否则标签页会变成「— 」。
	c := config.Defaults()
	c.Title = "   "
	if err := c.Normalize(); err != nil {
		t.Fatal(err)
	}
	if c.Title != config.DefaultTitle {
		t.Errorf("Normalize 后标题 = %q，期望 %q", c.Title, config.DefaultTitle)
	}
}

// TestLimitsNotPersistedUntilChanged 确认「没改过就不写盘」。
//
// 这条不是洁癖：如果无条件把当前值写进文件，「恢复启动时的默认值」之后
// 文件里会留下一份等于旧默认值的记录，运维以后改 --upload-max-size 就
// 会被它压住 —— 参数改了却不生效，很难查。
func TestLimitsNotPersistedUntilChanged(t *testing.T) {
	sf := filepath.Join(t.TempDir(), "settings.json")
	cfg := testConfig(t, t.TempDir(), "-A", "-a", "boss:pw@/:rw",
		"--upload-max-size", "1073741824", "--edit-max-size", "1048576")
	cfg.SettingsFile = sf
	s := newTestServer(t, cfg)
	hdr := adminSettingsHdr()

	// 只改标题：文件里不该出现两个上限。
	if got := do(t, s, http.MethodPut, "/fs/settings",
		strings.NewReader(`{"title":"只改标题"}`), hdr); got.code != http.StatusOK {
		t.Fatalf("改标题返回 %d：%s", got.code, got.body)
	}
	raw, err := os.ReadFile(sf)
	if err != nil {
		t.Fatal(err)
	}
	for _, unwanted := range []string{"upload_max_size", "edit_max_size"} {
		if strings.Contains(string(raw), unwanted) {
			t.Errorf("没改过的上限不该写进文件：%s", raw)
		}
	}
}

// TestLimitsRoundTripAndPersist 覆盖上限的落盘与重启恢复。
func TestLimitsRoundTripAndPersist(t *testing.T) {
	sf := filepath.Join(t.TempDir(), "settings.json")
	root := t.TempDir()
	cfg := testConfig(t, root, "-A", "-a", "boss:pw@/:rw",
		"--upload-max-size", "1073741824", "--edit-max-size", "1048576")
	cfg.SettingsFile = sf
	s := newTestServer(t, cfg)
	hdr := adminSettingsHdr()

	put := func(body string) resp {
		return do(t, s, http.MethodPut, "/fs/settings", strings.NewReader(body), hdr)
	}
	if got := put(`{"upload_max_size":10485760}`); got.code != http.StatusOK {
		t.Fatalf("改上传上限返回 %d：%s", got.code, got.body)
	}
	if got := put(`{"edit_max_size":5242880}`); got.code != http.StatusOK {
		t.Fatalf("改编辑上限返回 %d：%s", got.code, got.body)
	}

	// 文件里应当只有这两项（外加标题）。
	raw, err := os.ReadFile(sf)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"upload_max_size": 10485760`, `"edit_max_size": 5242880`} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("设置文件里缺少 %s：%s", want, raw)
		}
	}

	// 新实例（＝重启）用同一份文件与**同样的启动参数**：文件里的值胜出。
	cfg2 := testConfig(t, root, "-A", "-a", "boss:pw@/:rw",
		"--upload-max-size", "1073741824", "--edit-max-size", "1048576")
	cfg2.SettingsFile = sf
	s2 := newTestServer(t, cfg2)
	if got := s2.settings.UploadMaxSize(); got != 10485760 {
		t.Errorf("重启后上传上限 = %d，期望 10485760（文件优先）", got)
	}
	if got := s2.settings.EditMaxSize(); got != 5242880 {
		t.Errorf("重启后编辑上限 = %d，期望 5242880（文件优先）", got)
	}
}

// TestUploadMaxSizeZeroPersisted 确认「不限制」（0）能存能读 —— 这是两个上限
// 必须用指针而不是零值判断的原因。
func TestUploadMaxSizeZeroPersisted(t *testing.T) {
	sf := filepath.Join(t.TempDir(), "settings.json")
	root := t.TempDir()
	cfg := testConfig(t, root, "-A", "-a", "boss:pw@/:rw")
	cfg.SettingsFile = sf
	s := newTestServer(t, cfg)

	if got := do(t, s, http.MethodPut, "/fs/settings",
		strings.NewReader(`{"upload_max_size":0}`), adminSettingsHdr()); got.code != http.StatusOK {
		t.Fatalf("设为不限制返回 %d：%s", got.code, got.body)
	}
	raw, _ := os.ReadFile(sf)
	if !strings.Contains(string(raw), `"upload_max_size": 0`) {
		t.Fatalf("「不限制」没写进文件：%s", raw)
	}

	cfg2 := testConfig(t, root, "-A", "-a", "boss:pw@/:rw") // 启动参数是默认 10GiB
	cfg2.SettingsFile = sf
	s2 := newTestServer(t, cfg2)
	if got := s2.settings.UploadMaxSize(); got != 0 {
		t.Errorf("重启后上传上限 = %d，期望 0（不限制）", got)
	}
}

// TestEditMaxSizeZeroInFileIgnored 确认文件里被手工写成 0 的编辑上限被忽略。
//
// 0 会让每个文件都判成超限（全都 413），是个能把服务用死的值，
// 所以宁可回落到启动参数。
func TestEditMaxSizeZeroInFileIgnored(t *testing.T) {
	sf := filepath.Join(t.TempDir(), "settings.json")
	if err := os.WriteFile(sf, []byte(`{"version":3,"edit_max_size":0}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := testConfig(t, t.TempDir(), "-A", "-a", "boss:pw@/:rw", "--edit-max-size", "2097152")
	cfg.SettingsFile = sf
	s := newTestServer(t, cfg)

	if got := s.settings.EditMaxSize(); got != 2097152 {
		t.Errorf("编辑上限 = %d，期望回落到启动参数 2097152", got)
	}
}

// TestRestoreDefaultClearsPersistedLimit 确认「恢复启动时的默认值」之后，
// 文件里不再留那条记录 —— 于是运维之后改启动参数能生效。
func TestRestoreDefaultClearsPersistedLimit(t *testing.T) {
	sf := filepath.Join(t.TempDir(), "settings.json")
	root := t.TempDir()
	cfg := testConfig(t, root, "-A", "-a", "boss:pw@/:rw", "--upload-max-size", "1073741824")
	cfg.SettingsFile = sf
	s := newTestServer(t, cfg)
	hdr := adminSettingsHdr()

	if got := do(t, s, http.MethodPut, "/fs/settings",
		strings.NewReader(`{"upload_max_size":10485760}`), hdr); got.code != http.StatusOK {
		t.Fatalf("改动返回 %d", got.code)
	}
	// 恢复成启动参数的值。
	if got := do(t, s, http.MethodPut, "/fs/settings",
		strings.NewReader(`{"upload_max_size":1073741824}`), hdr); got.code != http.StatusOK {
		t.Fatalf("恢复默认返回 %d", got.code)
	}
	raw, _ := os.ReadFile(sf)
	if strings.Contains(string(raw), "upload_max_size") {
		t.Errorf("恢复默认后不该再留记录：%s", raw)
	}

	// 于是把启动参数改大，重启就生效（不被旧记录压住）。
	cfg2 := testConfig(t, root, "-A", "-a", "boss:pw@/:rw", "--upload-max-size", "2147483648")
	cfg2.SettingsFile = sf
	if got := newTestServer(t, cfg2).settings.UploadMaxSize(); got != 2147483648 {
		t.Errorf("改启动参数没生效：%d", got)
	}
}
