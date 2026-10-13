package server

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/xxl6097/gofs/internal/config"
)

// fakeUninstaller 同时实现 Restarter 与 Uninstaller —— 「两个都要」的部署形态。
type fakeUninstaller struct {
	restarts    int
	uninstalls  int
	uninstallEr error
	// started / release 用来把卸载卡住，测单飞闸门（照 upgrade_lock_test 的写法）。
	// startedOnce 保证只关一次：闸门释放之后测试还会再打一次，
	// 那次也会进到这里。
	started     chan struct{}
	startedOnce sync.Once
	release     chan struct{}
}

func (f *fakeUninstaller) Restart(context.Context) error {
	f.restarts++
	return nil
}

func (f *fakeUninstaller) Uninstall(context.Context) error {
	f.uninstalls++
	if f.started != nil {
		f.startedOnce.Do(func() { close(f.started) })
	}
	if f.release != nil {
		<-f.release
	}
	return f.uninstallEr
}

// uninstallServer 造一个注入了 fakeUninstaller 的实例。
func uninstallServer(t *testing.T, r config.Restarter) (*Server, *fakeUninstaller) {
	t.Helper()
	fu, _ := r.(*fakeUninstaller)
	cfg := testConfig(t, t.TempDir(), "-A", "-a", "boss:pw@/:rw", "-a", "guest:g@/:r")
	cfg.Restarter = r
	return newTestServer(t, cfg), fu
}

// TestUninstallRequiresAdmin 是安全底线：卸载是本服务最不可逆的操作，
// 比升级还彻底 —— 它连「换回来」的机会都不留。
func TestUninstallRequiresAdmin(t *testing.T) {
	s, fu := uninstallServer(t, &fakeUninstaller{})

	if got := do(t, s, http.MethodPost, "/fs/uninstall", nil, nil); got.code != http.StatusUnauthorized {
		t.Errorf("匿名返回 %d，期望 401", got.code)
	}
	guest := map[string]string{"Authorization": basicAuth("guest", "g")}
	if got := do(t, s, http.MethodPost, "/fs/uninstall", nil, guest); got.code == http.StatusOK {
		t.Errorf("只读账号竟然能卸载：%s", got.body)
	}
	if fu.uninstalls != 0 {
		t.Errorf("被拒的请求不该调到实现（调了 %d 次）", fu.uninstalls)
	}
}

// TestUninstallInvokesImplementation 覆盖成功路径。
func TestUninstallInvokesImplementation(t *testing.T) {
	s, fu := uninstallServer(t, &fakeUninstaller{})

	got := do(t, s, http.MethodPost, "/fs/uninstall", nil, adminSettingsHdr())
	if got.code != http.StatusOK {
		t.Fatalf("返回 %d：%s", got.code, got.body)
	}
	if body := strings.TrimSpace(got.body); body != `{"ok":true}` {
		t.Errorf("响应体 = %q", body)
	}
	if fu.uninstalls != 1 {
		t.Errorf("实现被调了 %d 次，期望 1", fu.uninstalls)
	}
	if fu.restarts != 0 {
		t.Errorf("卸载不该顺带重启（Restart 被调了 %d 次）", fu.restarts)
	}
}

// TestUninstallDisabledWhenNotImplemented 是这次改动的关键契约：
// 只有 Restarter **没有**实现 Uninstaller 时，接口必须 404、按钮也不显示。
//
// 用类型断言而不是一个开关字段，就是为了让「能力」和「按钮」不可能对不上。
func TestUninstallDisabledWhenNotImplemented(t *testing.T) {
	// ① 完全没注入 Restarter。
	s, _ := uninstallServer(t, nil)
	if got := do(t, s, http.MethodPost, "/fs/uninstall", nil, adminSettingsHdr()); got.code != http.StatusNotFound {
		t.Errorf("未注入时返回 %d，期望 404：%s", got.code, got.body)
	}

	// ② 注入了 Restarter，但它没有实现 Uninstaller（这就是绝大多数部署）。
	s2, _ := uninstallServer(t, &fakeRestarter{})
	got := do(t, s2, http.MethodPost, "/fs/uninstall", nil, adminSettingsHdr())
	if got.code != http.StatusNotFound {
		t.Errorf("Restarter 未实现 Uninstaller 时返回 %d，期望 404：%s", got.code, got.body)
	}
	// 文案要点明两种可能，否则使用者会以为「重启能用，卸载凭什么不行」。
	if !strings.Contains(got.body, "Uninstaller") {
		t.Errorf("404 文案没有点明缺的是 Uninstaller：%s", got.body)
	}
}

// TestUninstallSurfacesImplementationError 确认实现方的错误原样透出。
func TestUninstallSurfacesImplementationError(t *testing.T) {
	s, _ := uninstallServer(t, &fakeUninstaller{
		uninstallEr: errors.New("删除 /usr/local/bin/gofs 失败: permission denied"),
	})

	got := do(t, s, http.MethodPost, "/fs/uninstall", nil, adminSettingsHdr())
	if got.code != http.StatusInternalServerError {
		t.Fatalf("返回 %d，期望 500：%s", got.code, got.body)
	}
	// 原样透出比任何我们编的话都有用 —— 运维要照着它去修权限。
	if !strings.Contains(got.body, "permission denied") {
		t.Errorf("错误没有原样透出：%s", got.body)
	}
}

// TestUninstallMethodNotAllowed 确认只有 POST。
func TestUninstallMethodNotAllowed(t *testing.T) {
	s, _ := uninstallServer(t, &fakeUninstaller{})

	for _, m := range []string{http.MethodGet, http.MethodPut, http.MethodDelete} {
		got := do(t, s, m, "/fs/uninstall", nil, adminSettingsHdr())
		if got.code != http.StatusMethodNotAllowed {
			t.Errorf("%s 返回 %d，期望 405", m, got.code)
		}
		if got.head.Get("Allow") != "POST" {
			t.Errorf("%s 的 Allow = %q", m, got.head.Get("Allow"))
		}
	}
}

// TestUninstallSingleFlight 确认同一时刻只允许一次卸载。
//
// 卸载**不幂等**（重启才是）：两个管理员同时点，实现方那边会交叉着删同一批
// 东西。所以这里照升级的做法串行化，并且把「已运行多久」带进 409 ——
// 光有状态码分不清「别人刚点了一下」和「上一次卡住了」。
func TestUninstallSingleFlight(t *testing.T) {
	fu := &fakeUninstaller{started: make(chan struct{}), release: make(chan struct{})}
	s, _ := uninstallServer(t, fu)

	first := make(chan resp, 1)
	go func() {
		first <- do(t, s, http.MethodPost, "/fs/uninstall", nil, adminSettingsHdr())
	}()

	select {
	case <-fu.started:
	case <-time.After(3 * time.Second):
		t.Fatal("第一次卸载没有开始")
	}

	second := do(t, s, http.MethodPost, "/fs/uninstall", nil, adminSettingsHdr())
	if second.code != http.StatusConflict {
		t.Fatalf("并发第二次返回 %d，期望 409：%s", second.code, second.body)
	}
	for _, want := range []string{"已经有一次卸载在进行中", "已运行"} {
		if !strings.Contains(second.body, want) {
			t.Errorf("409 文案缺少 %q：%s", want, second.body)
		}
	}

	close(fu.release)
	if got := <-first; got.code != http.StatusOK {
		t.Errorf("第一次卸载返回 %d：%s", got.code, got.body)
	}

	// 闸门要还回来，否则一次误触之后这个功能就永久失效了。
	third := do(t, s, http.MethodPost, "/fs/uninstall", nil, adminSettingsHdr())
	if third.code != http.StatusOK {
		t.Errorf("跑完之后闸门没释放，返回 %d：%s", third.code, third.body)
	}
}

// TestPageAdvertisesUninstall 确认页面外壳与登录响应都带上了开关。
//
// 两个地方都要有：页面外壳是直接访问时用的，登录响应是「从登录框进来」时
// 补数据用的 —— 漏了后者，用户得手动刷新一次才看得到按钮。
func TestPageAdvertisesUninstall(t *testing.T) {
	s, _ := uninstallServer(t, &fakeUninstaller{})
	hdr := map[string]string{"Authorization": basicAuth("boss", "pw"), "Accept": "text/html"}

	page := do(t, s, http.MethodGet, "/", nil, hdr)
	for _, want := range []string{`"allow_restart":true`, `"allow_uninstall":true`} {
		if !strings.Contains(page.body, want) {
			t.Errorf("页面没带 %s：%.300s", want, page.body)
		}
	}

	auth := do(t, s, http.MethodGet, "/fs/auth", nil, adminSettingsHdr())
	for _, want := range []string{`"allow_restart":true`, `"allow_uninstall":true`} {
		if !strings.Contains(auth.body, want) {
			t.Errorf("登录响应没带 %s：%s", want, auth.body)
		}
	}

	// 没实现 Uninstaller 的部署：开关为 false（按钮就不显示）。
	s2, _ := uninstallServer(t, &fakeRestarter{})
	page2 := do(t, s2, http.MethodGet, "/", nil, hdr)
	if !strings.Contains(page2.body, `"allow_uninstall":false`) {
		t.Errorf("未实现卸载时页面应下发 false：%.300s", page2.body)
	}
	if !strings.Contains(page2.body, `"allow_restart":true`) {
		t.Errorf("重启能力不该被卸载的缺失影响：%.300s", page2.body)
	}
}
