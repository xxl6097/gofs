package server

import (
	"net/http"
)

// handleRestart 执行一次服务重启。
//
//	POST /fs/restart
//
// 「怎么重启」**不在这里** —— 由调用方通过 config.Restarter 注入。
// 服务端只负责：鉴权、把结果告诉调用方。原地重执行、通知编排器、
// 退出交给 supervisor 都行，库不替部署方拍板（见 config.Restarter 的注释）。
//
// 权限：只有**管理员**（对服务根有读写权限）能用。重启会掐断正在进行的
// 上传与下载，和升级一样属于这台机器上权限最高的操作之一。
func (s *Server) handleRestart(w http.ResponseWriter, r *http.Request) {
	if s.cfg.Restarter == nil {
		http.Error(w, "404 Not Found: 本服务未启用重启（未注入 Restarter）",
			http.StatusNotFound)
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

	s.logger.Infof("收到重启请求，交给 Restarter 处理")

	// 不设「同一时刻只允许一次」的锁：重启本身是幂等的（重启两次＝重启），
	// 而且实现方多半返回得很快（重执行或发个信号），拿锁反而会挡住
	// 「第一次其实失败了、想再试一次」。
	//
	// ⚠️ 这个响应是尽力而为：如果实现方是 syscall.Exec 原地重执行，
	// 进程镜像在写出响应之前就被换掉了，客户端看到的是连接中断。
	// 前端必须把「连接断开」也当成重启的正常表现（见 settings.js）。
	if err := s.cfg.Restarter.Restart(r.Context()); err != nil {
		s.logger.Errorf("重启失败: %v", err)
		s.writeJSONStatus(w, http.StatusInternalServerError, map[string]any{
			"error": err.Error(),
		})
		return
	}

	s.logger.Infof("重启请求已受理")
	s.writeJSON(w, map[string]any{"ok": true})
}
