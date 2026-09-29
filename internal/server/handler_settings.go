package server

import (
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/xxl6097/gofs/internal/auth"
	"github.com/xxl6097/gofs/internal/fsutil"
)

// settingsReply 是服务设置接口的响应。
type settingsReply struct {
	// Root 为当前服务根目录的绝对路径（仅管理员可见）。
	Root string `json:"root"`
	// RootSwitchable 表示当前账号是否有权切换根目录。
	RootSwitchable bool `json:"root_switchable"`
	// AllowRoots 为允许切换到的路径前缀，供界面提示「能切到哪」。
	AllowRoots []string `json:"allow_roots"`
	// UploadMaxSize 为当前生效的单文件上传上限，0 表示不限制。
	UploadMaxSize int64 `json:"upload_max_size"`
	// UploadMaxSizeDefault 为启动参数里的取值，用于「恢复默认」。
	UploadMaxSizeDefault int64 `json:"upload_max_size_default"`
	// UploadMaxSizeUnlimited 表示当前是否处于「不限制」状态。
	UploadMaxSizeUnlimited bool `json:"upload_max_size_unlimited"`
	// UploadDateDir 为当前的上传归档目录（未开启归档时为空串）。
	UploadDateDir string `json:"upload_date_dir,omitempty"`
}

// settingsRequest 是修改服务设置的请求体。
//
// 字段都用指针：需要区分「没传这个字段」（不动）与「传了 0 / 空串」
// （显式设为不限制 / 触发「不能为空」的错误）。
type settingsRequest struct {
	Root          *string `json:"root"`
	UploadMaxSize *int64  `json:"upload_max_size"`
}

// handleSettings 读取或修改服务级设置（根目录、单文件上传上限）。
//
// 这两项原本只能通过启动参数决定，改一次就得重启进程。把它们开放到
// 页面上之后，换个数据目录、临时放宽上传上限都不需要中断服务。
func (s *Server) handleSettings(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet, http.MethodHead:
		p, ok := s.requireLogin(w, r)
		if !ok {
			return
		}
		s.writeJSON(w, s.buildSettings(p.Perm == auth.PermReadWrite))
	case http.MethodPut, http.MethodPost:
		s.updateSettings(w, r)
	default:
		w.Header().Set("Allow", "GET, HEAD, PUT, POST")
		http.Error(w, "405 Method Not Allowed", http.StatusMethodNotAllowed)
	}
}

// buildSettings 组装当前设置。admin 决定是否下发根目录等敏感信息。
func (s *Server) buildSettings(admin bool) settingsReply {
	rep := settingsReply{
		UploadMaxSize:          s.settings.UploadMaxSize(),
		UploadMaxSizeDefault:   s.cfg.UploadMaxSize,
		UploadMaxSizeUnlimited: s.settings.UploadMaxSize() == 0,
	}
	if s.cfg.UploadDated() {
		rep.UploadDateDir = s.cfg.UploadDateDir(time.Now())
	}
	if !admin {
		// 非管理员不必知道服务跑在磁盘的哪个位置。
		return rep
	}
	rep.Root = s.res.Root()
	rep.RootSwitchable = s.cfg.AllowRootSwitch
	rep.AllowRoots = s.settings.AllowRoots()
	return rep
}

func (s *Server) updateSettings(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}

	var req settingsRequest
	dec := json.NewDecoder(io.LimitReader(r.Body, 8<<10))
	if err := dec.Decode(&req); err != nil {
		if errors.Is(err, io.EOF) {
			http.Error(w, "400 Bad Request: 请求体为空", http.StatusBadRequest)
			return
		}
		http.Error(w, "400 Bad Request: 请求体不是合法 JSON", http.StatusBadRequest)
		return
	}
	if req.Root == nil && req.UploadMaxSize == nil {
		http.Error(w, "400 Bad Request: 没有需要修改的设置项", http.StatusBadRequest)
		return
	}

	if req.Root != nil {
		if !s.cfg.AllowRootSwitch {
			http.Error(w, "403 Forbidden: 服务已通过 --no-root-switch 禁止切换根目录",
				http.StatusForbidden)
			return
		}
		if err := s.switchRoot(*req.Root); err != nil {
			http.Error(w, "400 Bad Request: "+err.Error(), http.StatusBadRequest)
			return
		}
	}

	if req.UploadMaxSize != nil {
		if *req.UploadMaxSize < 0 {
			http.Error(w, "400 Bad Request: 上传上限不能为负数（0 表示不限制）",
				http.StatusBadRequest)
			return
		}
		old := s.settings.UploadMaxSize()
		s.settings.SetUploadMaxSize(*req.UploadMaxSize)
		s.logger.Infof("单文件上传上限：%s → %s",
			humanSize(old), humanSize(s.settings.UploadMaxSize()))
	}

	s.writeJSON(w, s.buildSettings(true))
}

// switchRoot 把服务根目录切换到 newRoot。
//
// 三层校验，缺一不可：
//  1. 路径必须落在允许范围内（默认是启动根的父目录树）——
//     否则「改根目录」就等于给了一个把整个文件系统暴露出去的开关；
//  2. 目标必须存在且是目录（或单个文件，进入单文件模式）；
//  3. 规范化要用 fsutil.NormalizePath，先把软链解析掉 ——
//     否则在允许范围内放一个指向 /etc 的软链就能绕过第 1 条。
func (s *Server) switchRoot(newRoot string) error {
	newRoot = strings.TrimSpace(newRoot)
	if newRoot == "" {
		return errors.New("根目录不能为空")
	}
	if strings.ContainsRune(newRoot, 0) {
		return errors.New("根目录含有非法字符")
	}

	// 相对路径按「当前根」解释，这样在页面上填个子目录名就能直接下去。
	target := newRoot
	if !filepath.IsAbs(target) {
		target = filepath.Join(s.res.Root(), target)
	}
	abs, err := fsutil.NormalizePath(target)
	if err != nil {
		return err
	}

	if !s.settings.AllowsRoot(abs) {
		return errors.New("目标目录超出允许范围；可切换的路径前缀：" +
			strings.Join(s.settings.AllowRoots(), "、") +
			"（启动时用 --root-allow 可放开更多位置）")
	}

	info, err := os.Stat(abs)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return errors.New("目录不存在：" + abs)
		}
		return err
	}
	if !info.IsDir() {
		return errors.New("目标不是目录：" + abs)
	}

	old := s.res.Root()
	if err := s.res.SetRoot(abs); err != nil {
		return err
	}
	s.logger.Infof("服务根目录：%s → %s", old, abs)
	return nil
}

// requireLogin 要求请求已通过认证（不要求管理权限）。
//
// 判定用 auth.Verify 而不是「对根的权限」：一个只被授予 /docs 只读权限的
// 账号也是合法用户，它应该能读到自己能用哪些能力，而不是收到 401 被
// 前端弹一次登录框（凭据明明是好的）。敏感的根目录信息由 buildSettings
// 按权限决定是否下发。
func (s *Server) requireLogin(w http.ResponseWriter, r *http.Request) (permResult, bool) {
	if !s.auth.Enabled() {
		return permResult{Perm: auth.PermReadWrite, Authenticated: true}, true
	}
	user, pass, ok := credentials(r)
	if !ok || user == "" || !s.auth.Verify(user, pass) {
		if shouldChallenge(r) {
			w.Header().Set("WWW-Authenticate", `Basic realm="gofs", charset="UTF-8"`)
		}
		http.Error(w, "401 Unauthorized", http.StatusUnauthorized)
		return permResult{}, false
	}
	perm := s.auth.Lookup("/", user, pass, true)
	return permResult{Perm: perm, User: user, Authenticated: true}, true
}

// isAdminRequest 判断请求是否来自「对服务根拥有读写权限」的账号。
//
// 与 requireAdmin 共用同一套判定，区别只在于不写响应：页面渲染时需要的
// 是「要不要显示设置入口」，而不是当场拒绝。两处共用可以避免权限逻辑漂移
// （改了拦截规则却忘了同步显示规则，会出现「按钮在但点了 403」）。
func (s *Server) isAdminRequest(r *http.Request) bool {
	if !s.auth.Enabled() {
		return true
	}
	user, pass, ok := credentials(r)
	return s.auth.Lookup("/", user, pass, ok) == auth.PermReadWrite
}

// requireAdmin 要求请求来自「对服务根拥有读写权限」的账号。
//
// 服务级设置（根目录、上传上限）影响所有人，所以只放开给管理员；
// 一个只被授予 /docs 只读权限的账号不应该能把根目录换到别处。
func (s *Server) requireAdmin(w http.ResponseWriter, r *http.Request) bool {
	return s.requireAdminOr(w, r, "修改服务设置")
}

// requireAdminOr 与 requireAdmin 同一套判定，只是把 403 文案换成具体的操作名，
// 这样用户看到的提示能直接指出「是哪个动作需要管理员」。
func (s *Server) requireAdminOr(w http.ResponseWriter, r *http.Request, action string) bool {
	// 未启用鉴权时服务本身就是全开放的，不存在额外的管理员概念。
	if !s.auth.Enabled() {
		return true
	}
	if s.isAdminRequest(r) {
		return true
	}
	if _, _, ok := credentials(r); !ok {
		// 没带凭据：让他先登录，而不是直接判定「无权限」。
		if shouldChallenge(r) {
			w.Header().Set("WWW-Authenticate", `Basic realm="gofs", charset="UTF-8"`)
		}
		http.Error(w, "401 Unauthorized", http.StatusUnauthorized)
		return false
	}
	http.Error(w, "403 Forbidden: "+action+"需要对服务根拥有读写权限",
		http.StatusForbidden)
	return false
}

// isUserManageAllowed 判断「这个请求该不该看到用户管理入口」。
//
// 与 requireUserManage 共用同一套条件（已开放 + 已启用鉴权 + 管理员），
// 只是不写响应：页面渲染问的是「显不显示按钮」。
// 两处共用可以避免「按钮在但点了 403」这类权限逻辑漂移。
func (s *Server) isUserManageAllowed(r *http.Request) bool {
	if !s.cfg.AllowUserManage || !s.auth.Enabled() {
		return false
	}
	return s.isAdminRequest(r)
}
