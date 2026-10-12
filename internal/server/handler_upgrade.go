package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/xxl6097/gofs/internal/config"
)

// handleUpgrade 提供「检查更新」与「执行升级」两个动作。
//
//	GET  /fs/upgrade          检查有没有新版本
//	POST /fs/upgrade          执行升级，SSE 推送进度
//
// 实际的检查与升级逻辑**不在这里** —— 由调用方通过 config.Upgrader 注入。
// 服务端只负责：鉴权、串行化、把进度推给前端。这样更新源与校验策略
// 由部署方决定，库不替他们拍板（见 config.Upgrader 的注释）。
//
// 权限：只有**管理员**（对服务根有读写权限）能用。升级会替换正在运行的
// 二进制，是这台机器上权限最高的操作之一，不能开放给普通账号。
func (s *Server) handleUpgrade(w http.ResponseWriter, r *http.Request) {
	if s.cfg.Upgrader == nil {
		http.Error(w, "404 Not Found: 本服务未启用在线升级（未注入 Upgrader）",
			http.StatusNotFound)
		return
	}
	if !s.requireAdmin(w, r) {
		return
	}

	switch r.Method {
	case http.MethodGet, http.MethodHead:
		s.upgradeCheck(w, r)
	case http.MethodPost:
		s.upgradeRun(w, r)
	default:
		w.Header().Set("Allow", "GET, HEAD, POST")
		http.Error(w, "405 Method Not Allowed", http.StatusMethodNotAllowed)
	}
}

// upgradeCheck 查询是否有可用更新。
func (s *Server) upgradeCheck(w http.ResponseWriter, r *http.Request) {
	rel, err := s.cfg.Upgrader.Check(r.Context())
	if err != nil {
		// 错误原样回给管理员：查不到多半是网络或凭证问题，
		// 换成一句笼统的「检查失败」只会让人更难排查。
		s.writeJSONStatus(w, http.StatusBadGateway, map[string]any{
			"current": s.cfg.Version,
			"error":   err.Error(),
		})
		return
	}

	reply := map[string]any{
		"current":    s.cfg.Version,
		"updateable": rel != nil,
	}
	if rel != nil {
		reply["release"] = rel
		reply["latest"] = rel.Version
	}
	s.writeJSON(w, reply)
}

// upgradeLocks 保证同一时刻只有一次升级在跑。
//
// 两个管理员同时点「升级」会并发地写同一个二进制 —— 那是把文件写坏
// 最直接的方式。用一把进程内的锁串行化，第二个请求直接拒绝。
var upgradeLocks sync.Map // *Server -> *upgradeState

// upgradeState 是那把锁，外加「当前这次是什么时候开始的」。
//
// 为什么要记开始时刻：被拒的那个请求只拿到 409 是没法排查的 ——
// 「另一个人刚点了」和「上一次卡住了、锁一直没还」看起来一模一样，
// 但处理方式完全不同（等一会儿 vs 重启服务）。把已运行时长说出来，
// 管理员一眼就能分辨。
type upgradeState struct {
	mu      sync.Mutex
	startNs atomic.Int64 // 当前持锁者的开始时刻（UnixNano）；0 表示没有在跑
}

func (st *upgradeState) startedAt() time.Time {
	if n := st.startNs.Load(); n != 0 {
		return time.Unix(0, n)
	}
	return time.Time{}
}

func (s *Server) upgradeLock() *upgradeState {
	v, _ := upgradeLocks.LoadOrStore(s, &upgradeState{})
	return v.(*upgradeState)
}

// upgradeRun 执行升级，并通过 SSE 推送进度。
func (s *Server) upgradeRun(w http.ResponseWriter, r *http.Request) {
	st := s.upgradeLock()
	if !st.mu.TryLock() {
		msg := "409 Conflict: 已经有一次升级在进行中"
		if since := st.startedAt(); !since.IsZero() {
			msg += fmt.Sprintf("（开始于 %s，已运行 %s）",
				since.Format("15:04:05"), time.Since(since).Round(time.Second))
		}
		http.Error(w, msg, http.StatusConflict)
		return
	}
	st.startNs.Store(time.Now().UnixNano())
	defer func() {
		st.startNs.Store(0)
		st.mu.Unlock()
	}()

	// 先取一次目标版本：调用方从请求体里带上 Check 返回的那个版本，
	// 避免「检查完到点升级之间版本又变了」造成的困惑。
	var req struct {
		Release *config.Release `json:"release"`
	}
	if r.Body != nil {
		dec := json.NewDecoder(io.LimitReader(r.Body, 64<<10))
		if err := dec.Decode(&req); err != nil && !errors.Is(err, io.EOF) {
			http.Error(w, "400 Bad Request: 请求体不是合法 JSON", http.StatusBadRequest)
			return
		}
	}
	if req.Release == nil || req.Release.Version == "" {
		http.Error(w, "400 Bad Request: 缺少 release.version", http.StatusBadRequest)
		return
	}

	// 连上 SSE 之后才开始干活，这样进度不会丢。
	sse, err := newSSEWriter(w)
	if err != nil {
		http.Error(w, "500 Internal Server Error: "+err.Error(), http.StatusInternalServerError)
		return
	}

	// onProgress 可能被实现方从别的 goroutine 调用，用锁保住 SSE 写入。
	// 写失败只记日志：客户端断开不该让升级半途而废 ——
	// 二进制已经下到一半了，停下来对它没有任何好处。
	var plock sync.Mutex
	sse.event("progress", config.UpgradeProgress{Stage: config.StageChecking, Message: "开始升级"})
	onProgress := func(p config.UpgradeProgress) {
		plock.Lock()
		defer plock.Unlock()
		sse.event("progress", p)
	}

	s.logger.Infof("开始升级到 %s（当前 %s）", req.Release.Version, s.cfg.Version)
	err = s.cfg.Upgrader.Upgrade(r.Context(), req.Release, onProgress)
	if err != nil {
		s.logger.Errorf("升级到 %s 失败: %v", req.Release.Version, err)
		sse.event("error", map[string]any{
			"message": err.Error(),
		})
		return
	}

	s.logger.Infof("升级到 %s 完成，等待重启生效", req.Release.Version)
	sse.event("done", map[string]any{
		"version": req.Release.Version,
		"message": "升级完成，重启服务后生效",
	})
}
