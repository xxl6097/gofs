package server

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/xxl6097/gofs/internal/config"
)

var errNotFound = errors.New("下载失败：连不上更新源")

// blockingUpgrader 的 Upgrade 会一直卡住，直到测试放行。
type blockingUpgrader struct {
	started chan struct{} // 进入 Upgrade 时关闭，通知测试「已经拿着锁跑起来了」
	release chan struct{} // 测试关闭它来放行
}

func (b *blockingUpgrader) Check(context.Context) (*config.Release, error) {
	return &config.Release{Version: "9.9.9"}, nil
}

func (b *blockingUpgrader) Upgrade(context.Context, *config.Release,
	func(config.UpgradeProgress)) error {
	close(b.started)
	<-b.release
	return nil
}

func blockingUpgradeServer(t *testing.T, u *blockingUpgrader) *Server {
	t.Helper()
	root := t.TempDir()
	cfg := testConfig(t, root, "-A", "-a", "boss:pw@/:rw")
	cfg.Upgrader = u
	return newTestServer(t, cfg)
}

// TestUpgradeLockHeldWhileRunning 确认「同一时刻只允许一次升级」确实生效。
func TestUpgradeLockHeldWhileRunning(t *testing.T) {
	u := &blockingUpgrader{started: make(chan struct{}), release: make(chan struct{})}
	s := blockingUpgradeServer(t, u)
	body := `{"release":{"version":"9.9.9"}}`

	done := make(chan int, 1)
	go func() {
		r := do(t, s, "POST", "/fs/upgrade", strings.NewReader(body), adminHdr())
		done <- r.code
	}()

	<-u.started // 第一次已经拿着锁在跑了
	got := do(t, s, "POST", "/fs/upgrade", strings.NewReader(body), adminHdr())
	if got.code != 409 {
		t.Fatalf("并发第二次返回 %d，期望 409：%s", got.code, got.body)
	}
	// 只说 409 是没法排查的：要能区分「别人刚点了」和「上一次卡住了」，
	// 所以正文里必须带上已运行多久。
	for _, want := range []string{"已经有一次升级在进行中", "已运行"} {
		if !strings.Contains(got.body, want) {
			t.Errorf("409 正文缺少 %q：%s", want, got.body)
		}
	}

	close(u.release)
	if code := <-done; code != 200 {
		t.Fatalf("第一次升级返回 %d，期望 200", code)
	}
}

// TestUpgradeLockReleasedAfterRun 是这次要钉住的那条：
// 一次升级跑完之后锁必须释放，下一次点「开始升级」不能再拿到 409。
//
// 如果这里失败，症状就是「点了就报 升级失败：HTTP 409，而且从此永远 409」——
// 只能靠重启服务恢复。
func TestUpgradeLockReleasedAfterRun(t *testing.T) {
	u := &blockingUpgrader{started: make(chan struct{}), release: make(chan struct{})}
	s := blockingUpgradeServer(t, u)
	body := `{"release":{"version":"9.9.9"}}`

	done := make(chan int, 1)
	go func() {
		r := do(t, s, "POST", "/fs/upgrade", strings.NewReader(body), adminHdr())
		done <- r.code
	}()
	<-u.started
	close(u.release)
	if code := <-done; code != 200 {
		t.Fatalf("第一次升级返回 %d，期望 200", code)
	}

	// 第二次：锁应该已经还回来了。用一个立刻返回的实现，避免又卡住。
	cfg := testConfig(t, t.TempDir(), "-A", "-a", "boss:pw@/:rw")
	cfg.Upgrader = &fakeUpgrader{rel: &config.Release{Version: "9.9.9"}}
	s2 := newTestServer(t, cfg)
	got := do(t, s2, "POST", "/fs/upgrade", strings.NewReader(body), adminHdr())
	if got.code == 409 {
		t.Fatalf("上一次已结束，却又报 409 —— 锁泄漏了：%s", got.body)
	}
}

// TestUpgradeErrorSurfacedToAdmin 确认实现方返回的错误被原样送到前端，
// 而不是只剩一个状态码 —— 「升级失败：HTTP 409」这种提示排查不了任何问题。
func TestUpgradeErrorSurfacedToAdmin(t *testing.T) {
	root := t.TempDir()
	cfg := testConfig(t, root, "-A", "-a", "boss:pw@/:rw")
	cfg.Upgrader = &fakeUpgrader{rel: &config.Release{Version: "9.9.9"}, uerr: errNotFound}
	s := newTestServer(t, cfg)

	got := do(t, s, "POST", "/fs/upgrade",
		strings.NewReader(`{"release":{"version":"9.9.9"}}`), adminHdr())
	if !strings.Contains(got.body, errNotFound.Error()) {
		t.Errorf("实现方的错误没透出来：%s", got.body)
	}
}
