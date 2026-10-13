package server

import (
	"context"
	"net/http"
	"time"

	"github.com/xxl6097/gofs/internal/config"
)

// uninstallTimeout 是留给实现的卸载时长上限。
//
// 给 5 分钟是因为卸载多半要停服务、删文件、注销 systemd 单元，
// 比一次重启慢得多；但也不该无限等 —— 卡住的卸载要有个了断。
const uninstallTimeout = 5 * time.Minute

// uninstaller 取出「能做卸载」的 Restarter 实现。
//
// 卸载能力是**从 Restarter 的值上断言出来的可选扩展**（见 config.Uninstaller）：
// 用类型断言而非单独一个字段，好处是「这个值的方法集」就是能力声明 ——
// 页面据此决定显不显示按钮，不会出现点了必然失败的按钮。
func (s *Server) uninstaller() (config.Uninstaller, bool) {
	if s.cfg.Restarter == nil {
		return nil, false
	}
	u, ok := s.cfg.Restarter.(config.Uninstaller)
	return u, ok
}

// canUninstall 表示这次部署要不要在页面上露出「卸载」。
//
// 下发标志的地方（页面外壳、登录响应）都走它，别各自写一遍类型断言 ——
// 两处判断一旦走岔，就会出现「按钮显示了但接口 404」这种最难查的组合。
func (s *Server) canUninstall() bool {
	_, ok := s.uninstaller()
	return ok
}

// handleUninstall 执行一次服务卸载。
//
//	POST /fs/uninstall
//
// 「卸载具体做什么」**不在这里** —— 由调用方通过实现了 config.Uninstaller
// 的 Restarter 注入。库自己**不删任何东西**：删二进制、注销 systemd 单元、
// 清配置目录、通知编排器 …… 各家不同，与升级同一个理由，怎么做由部署方
// 按自己的威胁模型决定。
//
// 权限：只有**管理员**能用。这是本服务最不可逆的操作 —— 比升级（换掉自己的
// 二进制）还彻底，因为它连「换回来」的机会都不留。
func (s *Server) handleUninstall(w http.ResponseWriter, r *http.Request) {
	// 先判能力再判鉴权，与 /fs/restart 同一个顺序：未启用时 404 比 401 更准确，
	// 而且不必让一个没用这个功能的部署先过一遍鉴权。
	un, ok := s.uninstaller()
	if !ok {
		http.Error(w, "404 Not Found: 本服务未启用卸载"+
			"（未注入 Restarter，或它没有实现 Uninstaller）", http.StatusNotFound)
		return
	}
	if !s.requireAdmin(w, r) {
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		http.Error(w, "405 Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}

	// 与重启不同，这里**要**串行化：卸载不幂等。两个管理员同时点，实现方
	// 那边会交叉着删同一批东西 —— 升级那边是「把同一个二进制写坏」，
	// 这里是「删到一半互相打架」，都够难收拾的。
	g := gateFor(&uninstallGates, s)
	if !g.tryAcquire() {
		http.Error(w, g.busyMessage("卸载"), http.StatusConflict)
		return
	}
	defer g.release()

	s.logger.Infof("收到卸载请求，交给 Uninstaller 处理")

	// 刻意**脱开请求上下文**：卸载可能要跑好一会儿，而浏览器一关连接
	// （或用户按了停止）就会取消 r.Context() —— 那会让卸载半途而废，
	// 留下一个删了一半的状态。给一个自己的超时，不跟着请求走。
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), uninstallTimeout)
	defer cancel()

	// ⚠️ 这个响应是尽力而为：实现方若删掉自己的二进制再退出，进程在写出
	// 响应之前就没了，客户端看到的是连接中断。前端必须把「连接断开」当成
	// 卸载的正常表现，并靠轮询 /fs/health **没**回来确认它确实停了。
	if err := un.Uninstall(ctx); err != nil {
		s.logger.Errorf("卸载失败: %v", err)
		s.writeJSONStatus(w, http.StatusInternalServerError, map[string]any{
			"error": err.Error(),
		})
		return
	}

	s.logger.Infof("卸载请求已受理")
	s.writeJSON(w, map[string]any{"ok": true})
}
