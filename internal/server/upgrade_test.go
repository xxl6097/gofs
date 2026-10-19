package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/xxl6097/gofs/internal/config"
)

// fakeUpgrader 是测试用的实现：gofs 只定义接口，实现由调用方给。
type fakeUpgrader struct {
	rel    *config.Release
	cerr   error
	uerr   error
	calls  int
	updRel *config.Release

	mu       sync.Mutex
	progress []config.UpgradeProgress
}

func (f *fakeUpgrader) Check(context.Context) (*config.Release, error) {
	f.calls++
	return f.rel, f.cerr
}

func (f *fakeUpgrader) Upgrade(_ context.Context, rel *config.Release, onProgress func(config.UpgradeProgress)) error {
	f.updRel = rel
	if f.uerr != nil {
		return f.uerr
	}
	for _, p := range f.progress {
		if onProgress != nil {
			onProgress(p)
		}
	}
	return nil
}

// upgradeServer 造一个注入了 Upgrader 的实例。
func upgradeServer(t *testing.T, u *fakeUpgrader) *Server {
	t.Helper()
	root := t.TempDir()
	cfg := testConfig(t, root, "-A", "-a", "boss:pw@/:rw")
	cfg.Upgrader = u
	return newTestServer(t, cfg)
}

// adminHdr 是管理员的凭据头。
func adminHdr() map[string]string {
	return map[string]string{"Authorization": basicAuth("boss", "pw")}
}

// TestUpgradeCheckReportsRelease 覆盖「检查到新版本」。
func TestUpgradeCheckReportsRelease(t *testing.T) {
	u := &fakeUpgrader{rel: &config.Release{
		Version: "9.9.9", Notes: "修了几个 bug", PageURL: "https://example.com/v9",
	}}
	s := upgradeServer(t, u)

	got := do(t, s, http.MethodGet, "/fs/upgrade", nil, adminHdr())
	if got.code != http.StatusOK {
		t.Fatalf("返回 %d：%s", got.code, got.body)
	}
	var d struct {
		Current    string            `json:"current"`
		Updateable bool              `json:"updateable"`
		Latest     string            `json:"latest"`
		Release    *config.Release   `json:"release"`
		AllowAny   map[string]string `json:"-"`
	}
	if err := json.Unmarshal([]byte(got.body), &d); err != nil {
		t.Fatalf("解析失败：%v\n%s", err, got.body)
	}
	if d.Current != config.DefaultVersion {
		t.Errorf("current = %q，期望 %q", d.Current, config.DefaultVersion)
	}
	if !d.Updateable || d.Latest != "9.9.9" {
		t.Errorf("没有报出新版本：%s", got.body)
	}
	if d.Release == nil || d.Release.Notes != "修了几个 bug" {
		t.Errorf("发布说明丢了：%s", got.body)
	}
}

// TestUpgradeCheckUpToDate 确认没有更新时明确返回「已是最新」。
func TestUpgradeCheckUpToDate(t *testing.T) {
	s := upgradeServer(t, &fakeUpgrader{rel: nil})

	got := do(t, s, http.MethodGet, "/fs/upgrade", nil, adminHdr())
	if got.code != http.StatusOK {
		t.Fatalf("返回 %d", got.code)
	}
	if !strings.Contains(got.body, `"updateable":false`) {
		t.Fatalf("没有表示「已是最新」：%s", got.body)
	}
}

// releasesPageUpgrader 给 fakeUpgrader 加上可选的 ReleasesPage 能力。
type releasesPageUpgrader struct {
	*fakeUpgrader
	url string
}

func (u *releasesPageUpgrader) ReleasesURL() string { return u.url }

// TestUpgradeCheckReportsReleasesURL 覆盖「已是最新时也要有去发布页的入口」。
//
// 已是最新时 Check 返回 nil，响应里没有 Release、也就没有 PageURL；
// 升级器若实现了可选的 ReleasesPage，服务端要把单独的 releases_url 带上，
// 界面才能在那句话旁边挂一个链接。没实现的升级器行为保持不变。
func TestUpgradeCheckReportsReleasesURL(t *testing.T) {
	newServer := func(u config.Upgrader) *Server {
		t.Helper()
		cfg := testConfig(t, t.TempDir(), "-A", "-a", "boss:pw@/:rw")
		cfg.Upgrader = u
		return newTestServer(t, cfg)
	}

	// 实现了 ReleasesPage：下发地址
	const page = "https://github.com/xxl6097/gofs/releases"
	s := newServer(&releasesPageUpgrader{fakeUpgrader: &fakeUpgrader{rel: nil}, url: page})
	got := do(t, s, http.MethodGet, "/fs/upgrade", nil, adminHdr())
	if got.code != http.StatusOK {
		t.Fatalf("返回 %d：%s", got.code, got.body)
	}
	if !strings.Contains(got.body, `"releases_url":"`+page+`"`) {
		t.Errorf("没有下发发布页地址：%s", got.body)
	}

	// 没实现：不回报这个字段
	s2 := newServer(&fakeUpgrader{rel: nil})
	if got2 := do(t, s2, http.MethodGet, "/fs/upgrade", nil, adminHdr()); strings.Contains(got2.body, "releases_url") {
		t.Errorf("未实现 ReleasesPage 时不该有 releases_url：%s", got2.body)
	}

	// 实现但返回空串（或纯空白）：也不回报 —— 空 href 比没有链接更糟
	s3 := newServer(&releasesPageUpgrader{fakeUpgrader: &fakeUpgrader{rel: nil}, url: "  "})
	if got3 := do(t, s3, http.MethodGet, "/fs/upgrade", nil, adminHdr()); strings.Contains(got3.body, "releases_url") {
		t.Errorf("空地址不该下发：%s", got3.body)
	}
}

// TestUpgradeCheckSurfacesError 确认检查失败时把原因原样给管理员 ——
// 换成一句笼统的「检查失败」只会让人更难排查。
func TestUpgradeCheckSurfacesError(t *testing.T) {
	s := upgradeServer(t, &fakeUpgrader{cerr: errors.New("连不上更新源：dial tcp timeout")})

	got := do(t, s, http.MethodGet, "/fs/upgrade", nil, adminHdr())
	if got.code != http.StatusBadGateway {
		t.Fatalf("返回 %d，期望 502", got.code)
	}
	if !strings.Contains(got.body, "连不上更新源") {
		t.Fatalf("错误原因被吞了：%s", got.body)
	}
}

// TestUpgradeRunStreamsProgress 覆盖升级：进度要经 SSE 推给前端。
func TestUpgradeRunStreamsProgress(t *testing.T) {
	u := &fakeUpgrader{progress: []config.UpgradeProgress{
		{Stage: config.StageDownloading, Loaded: 50, Total: 100},
		{Stage: config.StageReplacing, Message: "写入文件"},
	}}
	s := upgradeServer(t, u)

	body, _ := json.Marshal(map[string]any{
		"release": map[string]any{"version": "9.9.9"},
	})
	got := do(t, s, http.MethodPost, "/fs/upgrade", bytes.NewReader(body), adminHdr())
	if got.code != http.StatusOK {
		t.Fatalf("返回 %d：%s", got.code, got.body)
	}
	if ct := got.head.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Errorf("Content-Type = %q，期望 SSE", ct)
	}
	for _, want := range []string{"event: progress", "downloading", "replacing", "event: done"} {
		if !strings.Contains(got.body, want) {
			t.Errorf("SSE 里缺少 %q：\n%s", want, got.body)
		}
	}
	// 实现方确实拿到了要升级的版本。
	if u.updRel == nil || u.updRel.Version != "9.9.9" {
		t.Errorf("Upgrade 收到的版本 = %+v", u.updRel)
	}
}

// TestUpgradeRunReportsFailure 确认实现返回错误时通过 SSE 报给前端，
// 而不是静默地结束流。
func TestUpgradeRunReportsFailure(t *testing.T) {
	u := &fakeUpgrader{uerr: errors.New("校验失败：摘要对不上")}
	s := upgradeServer(t, u)

	body, _ := json.Marshal(map[string]any{"release": map[string]any{"version": "9.9.9"}})
	got := do(t, s, http.MethodPost, "/fs/upgrade", bytes.NewReader(body), adminHdr())
	if !strings.Contains(got.body, "event: error") {
		t.Fatalf("失败没有报给前端：%s", got.body)
	}
	if !strings.Contains(got.body, "摘要对不上") {
		t.Errorf("失败原因被吞了：%s", got.body)
	}
	// 不能同时报 done —— 那会让前端以为升级成功了。
	if strings.Contains(got.body, "event: done") {
		t.Errorf("失败时不该报 done：%s", got.body)
	}
}

// TestUpgradeRequiresReleaseVersion 确认没带 release.version 时拒绝，
// 而不是让实现方拿到一个空版本去猜。
func TestUpgradeRequiresReleaseVersion(t *testing.T) {
	s := upgradeServer(t, &fakeUpgrader{})

	got := do(t, s, http.MethodPost, "/fs/upgrade",
		strings.NewReader(`{"release":{"version":""}}`), adminHdr())
	if got.code != http.StatusBadRequest {
		t.Fatalf("返回 %d，期望 400：%s", got.code, got.body)
	}
}

// TestUpgradeAdminOnly 是这次的安全底线。
//
// 升级会替换正在运行的二进制 —— 这是这台机器上权限最高的操作之一，
// 绝不能开放给普通账号，哪怕是只读账号。
func TestUpgradeAdminOnly(t *testing.T) {
	root := t.TempDir()
	cfg := testConfig(t, root, "-A",
		"-a", "boss:pw@/:rw", "-a", "guest:g@/docs:r")
	cfg.Upgrader = &fakeUpgrader{rel: &config.Release{Version: "9.9.9"}}
	s := newTestServer(t, cfg)

	t.Run("匿名", func(t *testing.T) {
		if got := do(t, s, http.MethodGet, "/fs/upgrade", nil, nil); got.code != http.StatusUnauthorized {
			t.Fatalf("匿名返回 %d，期望 401", got.code)
		}
	})
	t.Run("只读账号", func(t *testing.T) {
		h := map[string]string{"Authorization": basicAuth("guest", "g")}
		if got := do(t, s, http.MethodGet, "/fs/upgrade", nil, h); got.code == http.StatusOK {
			t.Fatalf("只读账号竟然能检查更新：%s", got.body)
		}
		if got := do(t, s, http.MethodPost, "/fs/upgrade",
			strings.NewReader(`{"release":{"version":"9.9.9"}}`), h); got.code == http.StatusOK {
			t.Fatalf("只读账号竟然能升级：%s", got.body)
		}
	})
	t.Run("管理员", func(t *testing.T) {
		if got := do(t, s, http.MethodGet, "/fs/upgrade", nil, adminHdr()); got.code != http.StatusOK {
			t.Fatalf("管理员返回 %d：%s", got.code, got.body)
		}
	})
}

// TestUpgradeDisabledWithoutUpgrader 确认没注入实现时接口不可用 ——
// 而不是给一个必然失败的空壳。
func TestUpgradeDisabledWithoutUpgrader(t *testing.T) {
	root := t.TempDir()
	s := newTestServer(t, testConfig(t, root, "-A", "-a", "boss:pw@/:rw"))

	if got := do(t, s, http.MethodGet, "/fs/upgrade", nil, adminHdr()); got.code != http.StatusNotFound {
		t.Fatalf("返回 %d，期望 404：%s", got.code, got.body)
	}
}

// TestPageHidesUpgradeWhenNotInjected 确认页面数据里带了开关，
// 前端才不会渲染一个点了必然 404 的按钮。
func TestPageHidesUpgradeWhenNotInjected(t *testing.T) {
	root := t.TempDir()

	off := newTestServer(t, testConfig(t, root, "-A", "-a", "boss:pw@/:rw"))
	got := do(t, off, http.MethodGet, "/", nil,
		map[string]string{"Authorization": basicAuth("boss", "pw"), "Accept": "text/html"})
	if !strings.Contains(got.body, `"allow_upgrade":false`) {
		t.Errorf("未注入时页面仍显示升级入口：%.300s", got.body)
	}

	cfg := testConfig(t, root, "-A", "-a", "boss:pw@/:rw")
	cfg.Upgrader = &fakeUpgrader{}
	on := newTestServer(t, cfg)
	got = do(t, on, http.MethodGet, "/", nil,
		map[string]string{"Authorization": basicAuth("boss", "pw"), "Accept": "text/html"})
	if !strings.Contains(got.body, `"allow_upgrade":true`) {
		t.Errorf("注入了却没开开关：%.300s", got.body)
	}
}
