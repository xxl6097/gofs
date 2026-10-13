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

// fakeUninstaller 是卸载实现的替身。
//
// 它**只**实现 Uninstaller —— 卸载不从 Restarter 上断言，所以这是合法的
// 注入值；「能卸载但不能重启」也正是独立扩展点要支持的部署。
type fakeUninstaller struct {
	uninstalls  int
	uninstallEr error
	// started / release 用来把卸载卡住，测单飞闸门（照 upgrade_lock_test 的写法）。
	// startedOnce 保证只关一次：闸门释放之后测试还会再打一次，
	// 那次也会进到这里。
	started     chan struct{}
	startedOnce sync.Once
	release     chan struct{}
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

// uninstallServer 造一个注入了卸载实现的实例。
// r 为 nil 表示这个部署不提供卸载。
func uninstallServer(t *testing.T, u config.Uninstaller) (*Server, *fakeUninstaller) {
	t.Helper()
	fu, _ := u.(*fakeUninstaller)
	cfg := testConfig(t, t.TempDir(), "-A", "-a", "boss:pw@/:rw", "-a", "guest:g@/:r")
	cfg.Uninstaller = u
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
}

// TestUninstallIndependentOfRestarter 钉住「卸载是独立扩展点」这条设计。
//
// 三种组合都要成立，它们在真实部署里都常见：
//   - 只卸载不重启 —— 独立注入之前根本做不到（能力绑在 Restarter 上，
//     想卸载就必须连「重启」按钮一起露出来）；
//   - 只重启不卸载 —— 绝大多数部署；
//   - 两个都没有 —— /fs/uninstall 必须 404、界面不显示按钮。
func TestUninstallIndependentOfRestarter(t *testing.T) {
	hdr := adminSettingsHdr()

	// ① 只卸载不重启。
	s, _ := uninstallServer(t, &fakeUninstaller{})
	if got := do(t, s, http.MethodPost, "/fs/uninstall", nil, hdr); got.code != http.StatusOK {
		t.Errorf("只注入卸载时 /fs/uninstall 返回 %d，期望 200：%s", got.code, got.body)
	}
	// 重启仍然是「未启用」—— 不该被卸载的注入顺带点亮。
	if got := do(t, s, http.MethodPost, "/fs/restart", nil, hdr); got.code != http.StatusNotFound {
		t.Errorf("没注入 Restarter，重启却返回 %d，期望 404", got.code)
	}
	page := do(t, s, http.MethodGet, "/", nil,
		map[string]string{"Authorization": basicAuth("boss", "pw"), "Accept": "text/html"})
	if !strings.Contains(page.body, `"allow_uninstall":true`) {
		t.Errorf("页面没下发 allow_uninstall：%.300s", page.body)
	}
	if !strings.Contains(page.body, `"allow_restart":false`) {
		t.Errorf("页面不该下发 allow_restart=true：%.300s", page.body)
	}

	// ② 只重启不卸载（最常见的部署）。
	s2, _ := uninstallServer(t, nil)
	s2.cfg.Restarter = &fakeRestarter{}
	if got := do(t, s2, http.MethodPost, "/fs/uninstall", nil, hdr); got.code != http.StatusNotFound {
		t.Errorf("只注入重启时 /fs/uninstall 返回 %d，期望 404：%s", got.code, got.body)
	}
	page2 := do(t, s2, http.MethodGet, "/", nil,
		map[string]string{"Authorization": basicAuth("boss", "pw"), "Accept": "text/html"})
	if !strings.Contains(page2.body, `"allow_restart":true`) {
		t.Errorf("重启能力不该被卸载的缺失影响：%.300s", page2.body)
	}
	if !strings.Contains(page2.body, `"allow_uninstall":false`) {
		t.Errorf("没注入卸载时页面应下发 false：%.300s", page2.body)
	}

	// ③ 两个都没注入。
	s3, _ := uninstallServer(t, nil)
	got := do(t, s3, http.MethodPost, "/fs/uninstall", nil, hdr)
	if got.code != http.StatusNotFound {
		t.Errorf("都没注入时返回 %d，期望 404：%s", got.code, got.body)
	}
	if !strings.Contains(got.body, "未注入 Uninstaller") {
		t.Errorf("404 文案要点明缺的是 Uninstaller：%s", got.body)
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

// TestPageAdvertisesUninstall 确认登录响应也带上了开关。
//
// 页面外壳那条在 TestUninstallIndependentOfRestarter 里已经覆盖；这里专测
// 登录响应 —— 它是「从登录框进来」时补数据用的，漏了的话用户得手动刷新
// 一次才看得到按钮（allow_restart 当初就是这么漏的）。
func TestPageAdvertisesUninstall(t *testing.T) {
	s, _ := uninstallServer(t, &fakeUninstaller{})

	auth := do(t, s, http.MethodGet, "/fs/auth", nil, adminSettingsHdr())
	if !strings.Contains(auth.body, `"allow_uninstall":true`) {
		t.Errorf("登录响应没带 allow_uninstall：%s", auth.body)
	}

	s2, _ := uninstallServer(t, nil)
	auth2 := do(t, s2, http.MethodGet, "/fs/auth", nil, adminSettingsHdr())
	if !strings.Contains(auth2.body, `"allow_uninstall":false`) {
		t.Errorf("未注入卸载时登录响应应下发 false：%s", auth2.body)
	}
}
