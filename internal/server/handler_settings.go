package server

import (
	"encoding/json"
	"errors"
	"fmt"
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

	// EditMaxSize 为当前生效的在线编辑大小上限，0 表示不限制。
	EditMaxSize int64 `json:"edit_max_size"`
	// EditMaxSizeDefault 为启动参数里的取值，用于「恢复默认」。
	EditMaxSizeDefault int64 `json:"edit_max_size_default"`
	// Title 为站点标题（浏览器标签页与顶栏品牌名）。
	Title string `json:"title"`
	// TitleDefault 为启动参数里的取值，用于「恢复默认」。
	TitleDefault string `json:"title_default"`
	// MultiRoot 表示当前挂了多个服务目录（此时 URL 带标签前缀，
	// 且 RootSwitchable 恒为 false）。
	MultiRoot bool `json:"multi_root"`
	// Roots 为全部挂载点（[0] 是主目录），供界面展示与增删站点。
	Roots []rootInfo `json:"roots,omitempty"`
	// SiteRoots 为显式配置的白名单前缀；空数组表示不限制目录。
	SiteRoots []string `json:"site_roots,omitempty"`
	// SitesEditable 表示当前是否允许在页面上增删站点。
	// 与根目录可切换同属一个开关家族（--no-root-switch）；主目录是单文件
	// 模式时也不允许 —— 那时整台服务只暴露一个文件，加站点没有意义。
	SitesEditable bool `json:"sites_editable"`
	// UploadDateDir 为当前的上传归档目录（未开启归档时为空串）。
	UploadDateDir string `json:"upload_date_dir,omitempty"`
}

// rootInfo 描述一个挂载点。主目录的 URL 是 "/"，附加站点是 "/<标签>"。
type rootInfo struct {
	Label string `json:"label"`
	Root  string `json:"root"`
	URL   string `json:"url"`
	// Primary 标记这是主服务目录（服务在 /，不可移除）。
	Primary bool `json:"primary,omitempty"`
	// Removable 表示界面上可以移除这一项（主目录恒为 false）。
	Removable bool `json:"removable"`
}

// settingsRequest 是修改服务设置的请求体。
//
// 字段都用指针：需要区分「没传这个字段」（不动）与「传了 0 / 空串」
// （显式设为不限制 / 触发「不能为空」的错误）。
type settingsRequest struct {
	Root          *string `json:"root"`
	UploadMaxSize *int64  `json:"upload_max_size"`
	EditMaxSize   *int64  `json:"edit_max_size"`
	Title         *string `json:"title"`
	// AddSite / RemoveSite 是**即时生效**的站点增删：它们直接改服务端状态
	// 并落盘，不走「取消 / 保存」那套（那样还得做待定操作与回滚）。
	AddSite    *siteAddRequest `json:"add_site"`
	RemoveSite *string         `json:"remove_site"`
}

// siteAddRequest 是新增站点的请求体。Label 可留空，按目录名自动分配。
type siteAddRequest struct {
	Path  string `json:"path"`
	Label string `json:"label"`
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
		EditMaxSize:            s.settings.EditMaxSize(),
		EditMaxSizeDefault:     s.cfg.EditMaxSize,
		Title:                  s.settings.Title(),
		TitleDefault:           s.cfg.Title,
	}
	if s.cfg.UploadDated() {
		rep.UploadDateDir = s.cfg.UploadDateDir(time.Now())
	}
	if !admin {
		// 非管理员不必知道服务跑在磁盘的哪个位置。
		return rep
	}
	rep.Root = s.res.Root()
	// 有附加站点时不允许运行期换主根（换掉哪一个？其余的呢？）——
	// 与站点增删是两件事：站点随时可增删，主根要换得先把站点清掉。
	rep.RootSwitchable = s.cfg.AllowRootSwitch && !s.MultiRoot()
	rep.MultiRoot = s.MultiRoot()
	rep.SitesEditable = s.cfg.AllowRootSwitch && s.res.SingleFile() == ""
	rep.SiteRoots = s.settings.SiteRoots()
	for _, m := range s.Mounts() {
		url := "/" + m.Label
		if m.Label == s.res.Primary().Label {
			url = "/"
		}
		rep.Roots = append(rep.Roots, rootInfo{
			Label: m.Label, Root: m.Res.Root(), URL: url,
			Primary:   m.Label == s.res.Primary().Label,
			Removable: m.Label != s.res.Primary().Label,
		})
	}
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
	if req.Root == nil && req.UploadMaxSize == nil && req.EditMaxSize == nil &&
		req.Title == nil && req.AddSite == nil && req.RemoveSite == nil {
		http.Error(w, "400 Bad Request: 没有需要修改的设置项", http.StatusBadRequest)
		return
	}

	// ---- 站点增删（即时生效，独立于下面的「取消 / 保存」）----
	if req.AddSite != nil {
		m, err := s.addSite(req.AddSite.Path, req.AddSite.Label)
		if err != nil {
			s.writeErrForSite(w, err)
			return
		}
		s.logger.Infof("新增站点：/%s → %s", m.Label, m.Res.Root())
	}
	if req.RemoveSite != nil {
		if err := s.removeSite(*req.RemoveSite); err != nil {
			s.writeErrForSite(w, err)
			return
		}
		s.logger.Infof("移除站点：/%s", *req.RemoveSite)
	}

	if req.Title != nil {
		title, err := normalizeTitle(*req.Title)
		if err != nil {
			http.Error(w, "400 Bad Request: "+err.Error(), http.StatusBadRequest)
			return
		}
		old := s.settings.Title()
		if err := s.settings.SetTitle(title); err != nil {
			// 内存里已经生效，只是没落盘 —— 照实说，别让管理员以为「重启后还在」。
			s.logger.Errorf("站点标题落盘失败: %v", err)
			http.Error(w, "500 Internal Server Error: 标题已生效，但写入设置文件失败："+
				err.Error(), http.StatusInternalServerError)
			return
		}
		s.logger.Infof("站点标题：%q → %q", old, title)
	}

	if req.Root != nil {
		// 多根模式先拦：这时 `--no-root-switch` 与否都不重要，
		// 换根在这个模式下根本没有定义。
		if s.MultiRoot() {
			http.Error(w, "400 Bad Request: 多根模式下不支持运行期切换服务根目录",
				http.StatusBadRequest)
			return
		}
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
		if err := s.settings.SetUploadMaxSize(*req.UploadMaxSize); err != nil {
			// 与标题同理：内存里已经生效，只是没落盘 —— 照实说，
			// 别让管理员以为「重启后还在」。
			s.logger.Errorf("上传上限落盘失败: %v", err)
			http.Error(w, "500 Internal Server Error: 上限已生效，但写入设置文件失败："+
				err.Error(), http.StatusInternalServerError)
			return
		}
		s.logger.Infof("单文件上传上限：%s → %s",
			humanSize(old), humanSize(s.settings.UploadMaxSize()))
	}

	if req.EditMaxSize != nil {
		// 编辑上限**没有「不限制」这一档**：打开一个文件要把整份内容读进内存，
		// 放开等于给一个几 GB 的「文本」文件留个口子。
		// 启动参数 --edit-max-size 也一直是「必须为正整数」，这里保持一致 ——
		// 否则「0 表示不限制」和「0 会被判成超过上限」会互相打架，
		// 结果是把所有文件都判成超限（每个都 413）。
		if *req.EditMaxSize <= 0 {
			http.Error(w, "400 Bad Request: 编辑上限必须为正整数（不支持不限制）",
				http.StatusBadRequest)
			return
		}
		old := s.settings.EditMaxSize()
		if err := s.settings.SetEditMaxSize(*req.EditMaxSize); err != nil {
			s.logger.Errorf("编辑上限落盘失败: %v", err)
			http.Error(w, "500 Internal Server Error: 上限已生效，但写入设置文件失败："+
				err.Error(), http.StatusInternalServerError)
			return
		}
		s.logger.Infof("在线编辑上限：%s → %s",
			humanSize(old), humanSize(s.settings.EditMaxSize()))
	}

	s.writeJSON(w, s.buildSettings(true))
}

// maxTitleRunes 是站点标题允许的最大字符数（按字符数，不是字节数）。
//
// 给 60 是因为顶栏是定高的：再长就会把工具栏挤变形，而标签页里也早就被
// 浏览器截断了。真正想写长的应该改前端资源，而不是塞进这个字段。
const maxTitleRunes = 60

// normalizeTitle 校验并规整站点标题。
//
// 标题是管理员填的自由文本，前端一律用 textContent 写入（不拼 HTML），
// 所以这里不担心注入；要挡的是两类会让界面坏掉、事后又很难查的输入：
//   - 控制字符：浏览器标签页里会被压成一行，进日志还会破坏「一行一条」；
//   - 过长的标题：见 maxTitleRunes。
//
// 空标题直接报错而不是当作「恢复默认」：静默地把标题换掉比报错更意外。
// 想回默认值，界面上有「恢复启动时的默认值」。
func normalizeTitle(raw string) (string, error) {
	t := strings.TrimSpace(raw)
	if t == "" {
		return "", errors.New("标题不能为空（想用启动参数里的默认值，请点「恢复启动时的默认值」）")
	}
	for _, r := range t {
		if r < 0x20 || r == 0x7f {
			return "", errors.New("标题不能包含换行、制表等控制字符")
		}
	}
	if n := len([]rune(t)); n > maxTitleRunes {
		return "", fmt.Errorf("标题最长 %d 个字符（当前 %d）", maxTitleRunes, n)
	}
	return t, nil
}

// addSite 新增一个附加站点。
//
// 安全边界复用「可切换范围」（--root-allow / 启动根的父目录）—— 把机器上
// 任意目录通过网页暴露出去是权限最高的操作之一，不能只靠「你是管理员」。
func (s *Server) addSite(path, wantLabel string) (fsutil.Mount, error) {
	if !s.cfg.AllowRootSwitch {
		return fsutil.Mount{}, errSiteDisabled
	}
	path = strings.TrimSpace(path)
	if path == "" {
		return fsutil.Mount{}, errors.New("目录不能为空")
	}
	if strings.ContainsRune(path, 0) {
		return fsutil.Mount{}, errors.New("目录含有非法字符")
	}
	// ⚠️ 先规范化（解析软链）再查白名单：否则在白名单内放一个指向 /etc 的
	// 软链就能绕过边界 —— 与 switchRoot 同一套顺序。
	abs, err := fsutil.NormalizePath(path)
	if err != nil {
		return fsutil.Mount{}, err
	}
	// 默认不限目录（门槛是「你是管理员」）；只有部署方**显式**写了
	// --root-allow / RootAllow 时，才按那份白名单收窄。
	if !s.settings.AllowsSite(abs) {
		return fsutil.Mount{}, errors.New("目标目录超出白名单；当前白名单：" +
			strings.Join(s.settings.SiteRoots(), "、") +
			"（由 --root-allow / RootAllow 指定；去掉它即不再限制目录）")
	}
	info, err := os.Stat(abs)
	if errors.Is(err, fs.ErrNotExist) {
		return fsutil.Mount{}, errors.New("目录不存在：" + abs)
	}
	if err != nil {
		return fsutil.Mount{}, err
	}
	if !info.IsDir() {
		return fsutil.Mount{}, errors.New("目标不是目录：" + abs)
	}

	// 串行化增删，避免两个请求同时改挂载表再各写一次设置文件。
	s.sitesMu.Lock()
	defer s.sitesMu.Unlock()

	m, err := s.res.AddSite(abs, wantLabel)
	if err != nil {
		return fsutil.Mount{}, err
	}
	// 先改内存再落盘：落盘失败时站点**已经生效**，照实告诉调用方，
	// 别让人以为「没加成」而反复重试（与 SetTitle 同一口径）。
	if err := s.persistSites(); err != nil {
		s.logger.Errorf("站点已生效但落盘失败: %v", err)
		return m, fmt.Errorf("站点已生效，但写入设置文件失败：%w", err)
	}
	return m, nil
}

// removeSite 移除一个附加站点。
func (s *Server) removeSite(label string) error {
	if !s.cfg.AllowRootSwitch {
		return errSiteDisabled
	}
	s.sitesMu.Lock()
	defer s.sitesMu.Unlock()

	if err := s.res.RemoveSite(label); err != nil {
		return err
	}
	if err := s.persistSites(); err != nil {
		s.logger.Errorf("站点已移除但落盘失败: %v", err)
		return fmt.Errorf("站点已移除，但写入设置文件失败：%w", err)
	}
	return nil
}

// persistSites 把当前的附加站点写进设置文件。
func (s *Server) persistSites() error {
	mounts := s.Mounts()
	recs := make([]siteRecord, 0, len(mounts))
	for i := 1; i < len(mounts); i++ { // 跳过主目录：它由命令行决定
		recs = append(recs, siteRecord{
			Path:  mounts[i].Res.Root(),
			Label: mounts[i].Label,
		})
	}
	return s.settings.SetSites(recs)
}

// errSiteDisabled 表示本服务不允许运行期改动服务目录。
var errSiteDisabled = errors.New("本服务已通过 --no-root-switch 禁止运行期改动服务目录")

// writeErrForSite 把站点增删的错误映射成合适的状态码。
//
// 「没有这个站点」是 404（资源不存在），其余（不在白名单、不存在、不是目录、
// 重复、嵌套、被开关禁掉）都是 400/403 —— 关键是**把原因原样带给管理员**，
// 否则他只能看到一个「失败」。
func (s *Server) writeErrForSite(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, errSiteDisabled):
		http.Error(w, "403 Forbidden: "+err.Error(), http.StatusForbidden)
	case errors.Is(err, fsutil.ErrUnknownSite):
		http.Error(w, "404 Not Found: "+err.Error(), http.StatusNotFound)
	default:
		http.Error(w, "400 Bad Request: "+err.Error(), http.StatusBadRequest)
	}
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
	// 兜底：updateSettings 已经拦过一次，但这里再挡一道 ——
	// 换根会改 *Resolver 里的根，而多根用的是 *Roots，语义上根本不该走到这。
	if s.MultiRoot() {
		return errors.New("多根模式下不支持切换服务根目录")
	}
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
//
// 判定走 permOf（因而认得会话 cookie），**不能**只看 Basic 头：
// 页面导航是浏览器直接发起的，既不会自动带 Authorization 头，JS 也加不上，
// 登录态只能靠 cookie 传过来。只看 Basic 头的话，用 cookie 登录的管理员
// 刷新页面后会被判成非管理员 —— 表现就是「设置」按钮莫名消失，
// 而其它按钮都正常（它们走 XHR，permOf 本来就认 cookie）。
func (s *Server) isAdminRequest(r *http.Request) bool {
	if !s.auth.Enabled() {
		return true
	}
	perm, _, _ := s.permOf(r, "/")
	return perm == auth.PermReadWrite
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
