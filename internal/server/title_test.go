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
