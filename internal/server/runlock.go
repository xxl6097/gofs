package server

import (
	"fmt"
	"sync"
	"sync/atomic"
	"time"
)

// runGate 是「同一时刻只允许一次」的闸门，外加「当前这次是什么时候开始的」。
//
// 目前有两处用它：升级与卸载。两者都是**不可逆**的操作，两个管理员同时点
// 会在实现方那边交叉着干同一件事（升级是并发写同一个二进制，卸载是并发删
// 同一批东西）—— 那是最直接地把状态弄坏的方式。用一把进程内的闸门串行化，
// 第二个请求直接拒绝。
//
// 对比：重启**不用**它。重启是幂等的（重启两次＝重启），实现方也多半返回得
// 很快，加锁反而会挡住「第一次其实失败了、想再试一次」。
//
// 为什么要记开始时刻：被拒的那个请求只拿到 409 是没法排查的 ——
// 「另一个人刚点了」和「上一次卡住了、闸门一直没还」看起来一模一样，
// 但处理方式完全不同（等一会儿 vs 重启服务）。把已运行时长说出来，
// 管理员一眼就能分辨。
type runGate struct {
	mu      sync.Mutex
	startNs atomic.Int64 // 当前持闸者的开始时刻（UnixNano）；0 表示没有在跑
}

// tryAcquire 尝试取得闸门，成功时记下开始时刻。
func (g *runGate) tryAcquire() bool {
	if !g.mu.TryLock() {
		return false
	}
	g.startNs.Store(time.Now().UnixNano())
	return true
}

// release 交还闸门。必须与成功的 tryAcquire 配对（通常 defer）。
func (g *runGate) release() {
	g.startNs.Store(0)
	g.mu.Unlock()
}

// startedAt 返回当前持闸者的开始时刻；没人在跑时返回零值。
func (g *runGate) startedAt() time.Time {
	if n := g.startNs.Load(); n != 0 {
		return time.Unix(0, n)
	}
	return time.Time{}
}

// busyMessage 生成闸门被占用时的 409 正文。
//
// what 是动作名（「升级」「卸载」），拼出来形如
// 「409 Conflict: 已经有一次升级在进行中（开始于 15:04:05，已运行 6m3s）」。
func (g *runGate) busyMessage(what string) string {
	msg := "409 Conflict: 已经有一次" + what + "在进行中"
	if since := g.startedAt(); !since.IsZero() {
		msg += fmt.Sprintf("（开始于 %s，已运行 %s）",
			since.Format("15:04:05"), time.Since(since).Round(time.Second))
	}
	return msg
}

// 闸门直接挂在 Server 上（upgradeGate / uninstallGate 两个字段），
// 不放进包级 map —— 早先用的是 `sync.Map` 按 *Server 索引：
// 同一进程里起过又丢掉多少实例，表里就永远留着多少个条目（测试里尤其明显）。
// 挂到实例上既没有这个泄漏，也不必再靠指针当键。
