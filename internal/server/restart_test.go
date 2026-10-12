package server

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/xxl6097/gofs/internal/config"
)

// fakeRestarter 记录被调用了几次，并可指定返回值。
type fakeRestarter struct {
	calls int
	err   error
}

func (f *fakeRestarter) Restart(context.Context) error {
	f.calls++
	return f.err
}

func restartServer(t *testing.T, r config.Restarter) (*Server, *fakeRestarter) {
	t.Helper()
	fr, _ := r.(*fakeRestarter)
	cfg := testConfig(t, t.TempDir(), "-A", "-a", "boss:pw@/:rw", "-a", "guest:g@/:r")
	cfg.Restarter = r
	return newTestServer(t, cfg), fr
}

// TestRestartRequiresAdmin 是安全底线：重启会掐断所有进行中的传输，
// 和升级一样属于权限最高的操作之一。
func TestRestartRequiresAdmin(t *testing.T) {
	s, fr := restartServer(t, &fakeRestarter{})

	if got := do(t, s, http.MethodPost, "/fs/restart", nil, nil); got.code != http.StatusUnauthorized {
		t.Errorf("匿名返回 %d，期望 401", got.code)
	}
	if got := do(t, s, http.MethodPost, "/fs/restart", nil,
		map[string]string{"Authorization": basicAuth("guest", "g")}); got.code == http.StatusOK {
		t.Errorf("只读账号竟然能重启：%s", got.body)
	}
	if fr.calls != 0 {
		t.Errorf("鉴权没过却调用了 Restarter %d 次", fr.calls)
	}
}

// TestRestartInvokesRestarter 覆盖正常路径。
func TestRestartInvokesRestarter(t *testing.T) {
	s, fr := restartServer(t, &fakeRestarter{})

	got := do(t, s, http.MethodPost, "/fs/restart", nil, adminHdr())
	if got.code != http.StatusOK {
		t.Fatalf("管理员返回 %d：%s", got.code, got.body)
	}
	if fr.calls != 1 {
		t.Errorf("Restarter 被调用 %d 次，期望 1", fr.calls)
	}
}

// TestRestartDisabledWithoutRestarter 确认没注入时接口不可用 ——
// 而不是给一个点了必然失败的空壳。
func TestRestartDisabledWithoutRestarter(t *testing.T) {
	s := newTestServer(t, testConfig(t, t.TempDir(), "-A", "-a", "boss:pw@/:rw"))

	if got := do(t, s, http.MethodPost, "/fs/restart", nil, adminHdr()); got.code != http.StatusNotFound {
		t.Errorf("未注入时返回 %d，期望 404：%s", got.code, got.body)
	}
}

// TestRestartSurfacesImplError 确认实现方的错误原样回给管理员。
func TestRestartSurfacesImplError(t *testing.T) {
	s, _ := restartServer(t, &fakeRestarter{err: errors.New("重执行失败：权限不足")})

	got := do(t, s, http.MethodPost, "/fs/restart", nil, adminHdr())
	if got.code != http.StatusInternalServerError {
		t.Fatalf("返回 %d，期望 500：%s", got.code, got.body)
	}
	if !strings.Contains(got.body, "重执行失败") {
		t.Errorf("实现方的错误没透出来：%s", got.body)
	}
}

// TestRestartMethodNotAllowed 确认只认 POST（重启不该被 GET 之类误触发）。
func TestRestartMethodNotAllowed(t *testing.T) {
	s, fr := restartServer(t, &fakeRestarter{})

	if got := do(t, s, http.MethodGet, "/fs/restart", nil, adminHdr()); got.code != http.StatusMethodNotAllowed {
		t.Errorf("GET 返回 %d，期望 405", got.code)
	}
	if fr.calls != 0 {
		t.Errorf("GET 却触发了重启 %d 次", fr.calls)
	}
}

// TestPageAdvertisesRestart 确认页面数据里带了开关，前端才知道要不要显示
// 「重启」——而且它与 allow_upgrade 相互独立（只重启不升级的部署很常见）。
func TestPageAdvertisesRestart(t *testing.T) {
	// 只注入 Restarter：allow_restart=true、allow_upgrade=false。
	cfg := testConfig(t, t.TempDir(), "-A", "-a", "boss:pw@/:rw")
	cfg.Restarter = &fakeRestarter{}
	s := newTestServer(t, cfg)

	got := do(t, s, http.MethodGet, "/", nil,
		map[string]string{"Authorization": basicAuth("boss", "pw"), "Accept": "text/html"})
	if !strings.Contains(got.body, `"allow_restart":true`) {
		t.Errorf("页面没有注入 allow_restart=true：%.300s", got.body)
	}
	if !strings.Contains(got.body, `"allow_upgrade":false`) {
		t.Errorf("未注入 Upgrader 却当成有升级：%.300s", got.body)
	}
}
