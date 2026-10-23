package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/xxl6097/gofs/internal/auth"
	"github.com/xxl6097/gofs/internal/config"
	"github.com/xxl6097/gofs/internal/fsutil"
)

// settingsReply 是服务设置接口的响应。
type settingsReply struct {
	// Root 为当前服务根目录的绝对路径（仅管理员可见，非管理员不下发该字段）。
	Root string `json:"root,omitempty"`
	// RootSwitchable 表示当前账号是否有权切换根目录。
	RootSwitchable bool `json:"root_switchable"`
	// AllowRoots 为允许切换到的路径前缀，供界面提示「能切到哪」。
	// 非管理员不下发：它同样是磁盘布局信息。
	AllowRoots []string `json:"allow_roots,omitempty"`
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

	// PathPrefix 为当前生效的访问路径前缀（"" 表示服务在根）。
	PathPrefix string `json:"path_prefix"`
	// PathPrefixDefault 为启动参数里的取值，用于「恢复默认」。
	PathPrefixDefault string `json:"path_prefix_default"`
	// PathPrefixLocked 表示启动参数显式指定了前缀，因此**它压过页面上的改动**：
	// 页面改的本次生效，重启后仍回到参数值。
	//
	// 特意做成「参数优先」是因为前缀是本服务唯一会把页面自己锁在门外的设置：
	// 改成一个反向代理没路由的值之后，页面在两个前缀下都打不开，而页面正是
	// 改回它的唯一入口 —— 这时只能靠「带 --path-prefix 重启一次」自救。
	PathPrefixLocked bool `json:"path_prefix_locked"`

	// ListenAddr / ListenPort 为当前生效的监听地址与端口
	// （"" 表示所有网卡，"/" 开头为 unix socket）。
	ListenAddr string `json:"listen_addr"`
	ListenPort int    `json:"listen_port"`
	// ListenAddrDefault / ListenPortDefault 为启动参数里的取值，用于「恢复默认」。
	ListenAddrDefault string `json:"listen_addr_default"`
	ListenPortDefault int    `json:"listen_port_default"`
	// ListenLocked 表示启动参数显式给过监听地址或端口，因此它压过页面上的改动
	// （与 PathPrefixLocked 同一个道理：改错了只能靠启动参数救回来）。
	ListenLocked bool `json:"listen_locked"`
	// ListenEditable 表示当前能否在页面上改监听 —— 需要注入了 Relistener
	// **且**服务确实在自己监听（库方式嵌入、由宿主监听的实例改不了）。
	// 为 false 时界面不显示这一栏。
	ListenEditable bool `json:"listen_editable"`
	// ListenJumpURL 是改完之后浏览器应当跳去的绝对地址。
	//
	// 由 updateSettings 在**刚改完的那次响应**里给出，其它时候为空。为空表示
	// 「别跳」：新地址是 unix socket（浏览器够不到），或者当前请求根本不是
	// 直达监听端口（多半经过了反向代理 —— 那种部署下改监听端口只是改了上游
	// 那一侧，页面地址不该跟着动）。
	ListenJumpURL string `json:"listen_jump_url,omitempty"`
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
	// PathPrefix 是访问路径前缀；空串表示「服务在根」。
	// 用指针是为了区分「没传」与「显式改成空前缀」。
	PathPrefix *string `json:"path_prefix"`
	// ListenAddr / ListenPort 是监听地址与端口。两个都传才是一次完整的改地址；
	// 只传端口（很常见）时地址沿用当前值。
	ListenAddr *string `json:"listen_addr"`
	ListenPort *int    `json:"listen_port"`
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
		PathPrefix:             s.PathPrefix(),
		PathPrefixDefault:      s.cfg.PathPrefix,
		PathPrefixLocked:       s.cfg.PathPrefix != "",
	}
	if s.cfg.UploadDated() {
		rep.UploadDateDir = s.cfg.UploadDateDir(time.Now())
	}
	if !admin {
		// 非管理员不必知道服务跑在磁盘的哪个位置 —— 监听地址也可能是
		// unix socket 的绝对路径，同样属于主机布局信息。
		return rep
	}
	rep.ListenAddr = s.ListenBind()
	rep.ListenPort = s.ListenPort()
	rep.ListenAddrDefault = s.cfg.Bind
	rep.ListenPortDefault = s.cfg.Port
	rep.ListenLocked = s.cfg.BindSet || s.cfg.PortSet
	rep.ListenEditable = s.listenEditable()
	rep.Root = s.res.Root()
	// 有附加站点时不允许运行期换主根（换掉哪一个？其余的呢？）——
	// 与站点增删是两件事：站点随时可增删，主根要换得先把站点清掉。
	// 单文件模式同样不允许：换根会把“只暴露这一个文件”变成目录模式，
	// 是单文件约束的直接绕过（UI 隐藏之外还必须在服务端拒绝）。
	rep.RootSwitchable = s.cfg.AllowRootSwitch && !s.MultiRoot() && s.res.SingleFile() == ""
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
	resetRead := withBodyReadDeadline(w, apiBodyReadTimeout)
	defer resetRead()
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
		req.Title == nil && req.PathPrefix == nil &&
		req.ListenAddr == nil && req.ListenPort == nil &&
		req.AddSite == nil && req.RemoveSite == nil {
		http.Error(w, "400 Bad Request: 没有需要修改的设置项", http.StatusBadRequest)
		return
	}

	// listenJump 是本请求里改监听后要给前端的跳转地址（没改则空）。
	var listenJump string

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

	if req.PathPrefix != nil {
		prefix, err := config.NormalizePathPrefix(*req.PathPrefix)
		if err != nil {
			http.Error(w, "400 Bad Request: "+err.Error(), http.StatusBadRequest)
			return
		}
		old := s.PathPrefix()
		if err := s.settings.SetPathPrefix(prefix); err != nil {
			s.logger.Errorf("访问前缀落盘失败: %v", err)
			http.Error(w, "500 Internal Server Error: 前缀已生效，但写入设置文件失败："+
				err.Error(), http.StatusInternalServerError)
			return
		}
		s.logger.Infof("访问路径前缀：%q → %q", old, prefix)
		if old != prefix {
			s.migrateSessionCookie(w, r, old, prefix)
		}
	}

	if req.ListenAddr != nil || req.ListenPort != nil {
		jump, ok := s.changeListen(w, r, req)
		if !ok {
			return
		}
		listenJump = jump
	}

	rep := s.buildSettings(true)
	// 只随**本次**响应下发：只有「刚改完」这一次需要跳转，
	// 之后的 GET 不该再怂恿浏览器换地址。
	rep.ListenJumpURL = listenJump
	s.writeJSON(w, rep)
}

// listenEditable 表示当下能不能在页面上改监听。
//
// 注入过 Relistener 还不够：以 Handler 方式嵌在别人的 http.Server 上、
// 自己从不 Start 的实例也持有实现，但它没有自己的监听可换。所以要问一句
// CanRelisten —— 界面据此决定显不显示那一栏，免得给一个点了必然失败的按钮。
func (s *Server) listenEditable() bool {
	return s.cfg.Relistener != nil && s.cfg.Relistener.CanRelisten()
}

// changeListen 处理「改监听地址/端口」：校验 → 热重绑 → 落盘。
//
// 顺序不能调换：**先重绑成功，才写进 settings.json**。反过来的话，一个建不起来
// 的地址会被写进文件，下次重启就起不来了 —— 那是最难收拾的一种坏法。
func (s *Server) changeListen(w http.ResponseWriter, r *http.Request, req settingsRequest) (string, bool) {
	if !s.listenEditable() {
		http.Error(w, "409 Conflict: 本服务未启用运行期改监听地址"+
			"（未注入 Relistener，或以库方式嵌入由宿主监听）", http.StatusConflict)
		return "", false
	}

	// 只传其中一项时，另一项沿用当前值。
	bind, port := s.ListenBind(), s.ListenPort()
	if req.ListenAddr != nil {
		bind = *req.ListenAddr
	}
	if req.ListenPort != nil {
		port = *req.ListenPort
	}
	bind, port, err := config.NormalizeListen(bind, port)
	if err != nil {
		http.Error(w, "400 Bad Request: "+err.Error(), http.StatusBadRequest)
		return "", false
	}
	// 在调用 Relistener **之前**挡住「路径上是普通文件/目录/软链」的情况。
	// 内置实现自己也会拒绝（见 gofs.Server.listen），但自定义 Relistener
	// 未必知道这条约定 —— 而这里的一句 os.Remove 就足以删掉任意文件。
	// 允许：路径不存在（新建），或已经是 socket（残留/复用）。
	if strings.HasPrefix(bind, "/") {
		if fi, lerr := os.Lstat(bind); lerr == nil && fi.Mode()&os.ModeSocket == 0 {
			http.Error(w, fmt.Sprintf("400 Bad Request: %s 已存在且不是 socket，拒绝覆盖", bind),
				http.StatusBadRequest)
			return "", false
		}
	}
	if bind == s.ListenBind() && port == s.ListenPort() {
		return "", true // 没变，什么都不做
	}

	oldAddr := config.ListenAddr(s.ListenBind(), s.ListenPort())
	addr, err := s.cfg.Relistener.Relisten(r.Context(), bind, port)
	if err != nil {
		// 重绑失败时旧监听原封不动（这是 Relistener 的约定），照实说，
		// 并把底层错误原样带出来 —— address already in use 这类信息
		// 比任何我们编的话都有用。
		s.logger.Errorf("切换监听地址失败（旧地址 %s 未受影响）: %v", oldAddr, err)
		http.Error(w, "400 Bad Request: "+err.Error(), http.StatusBadRequest)
		return "", false
	}
	if err := s.settings.SetListen(bind, port); err != nil {
		s.logger.Errorf("监听地址落盘失败: %v", err)
		http.Error(w, "500 Internal Server Error: 监听地址已生效（"+addr+
			"），但写入设置文件失败："+err.Error(), http.StatusInternalServerError)
		return "", false
	}
	s.logger.Infof("监听地址：%s → %s", oldAddr, addr)
	return listenJumpURL(r, s.PathPrefix(), oldAddr, addr), true
}

// listenJumpURL 算出「改完监听之后浏览器该跳到哪」，空串表示别跳。
//
// 什么情况下不跳：
//
//   - 新地址是 unix socket —— 浏览器压根访问不到；
//   - 当前请求不是直达监听端口（r.Host 的端口与**旧**监听端口对不上）——
//     几乎可以肯定前面挡着反向代理。这时改监听端口只改了上游那一侧，
//     把页面地址也改过去只会让人更连不上；该做的是去改代理的 upstream。
func listenJumpURL(r *http.Request, prefix, oldAddr, newAddr string) string {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	_, newPort, err := net.SplitHostPort(newAddr)
	if err != nil {
		return "" // unix socket 之类，没有 host:port
	}
	// 用请求里的主机名而不是新监听地址里的那个：页面上填的常是 0.0.0.0，
	// 而用户当初访问用的域名/IP 才更可能对外可达。
	reqHost, reqPort, err := net.SplitHostPort(r.Host)
	if err != nil || reqPort != portOfAddr(oldAddr) {
		return ""
	}
	return scheme + "://" + net.JoinHostPort(reqHost, newPort) + prefix + "/"
}

// portOfAddr 取出地址里的端口串；取不到返回空串。
func portOfAddr(addr string) string {
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		return ""
	}
	return port
}

// migrateSessionCookie 在前缀变更后把会话 cookie 搬到新 Path 上。
//
// 浏览器的 cookie 是按 (name, domain, path) 三元组存的：Path=/files/ 的那条
// 在 /data/… 的请求上根本不会被带上，成员会发现自己「莫名其妙被登出了」。
// 会话本身还活着（会话表按 token 存，与路径无关），所以只要换个 Path 重发一次
// 就能无感续上；同时把旧 Path 那条清掉，免得它在 cookie 罐里躺到过期。
//
// 没有会话 cookie 的请求（curl、Basic 直连的脚本）自然什么都不做。
func (s *Server) migrateSessionCookie(w http.ResponseWriter, r *http.Request, oldPrefix, newPrefix string) {
	tok := sessionToken(r)
	if tok == "" {
		return
	}
	oldPath, newPath := cookiePathForPrefix(oldPrefix), cookiePathForPrefix(newPrefix)
	if oldPath != newPath {
		s.clearSessionCookieAt(w, r, oldPath)
	}
	s.setSessionCookie(w, r, tok)
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
	if s.res.SingleFile() != "" {
		return fsutil.Mount{}, errors.New("单文件模式不支持添加站点；请以目录模式启动后再管理站点")
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
	// 默认只允许启动根的同级目录；显式 --root-allow 可按前缀收窄/放宽，
	// --allow-any-site 才恢复“挂载主机任意目录”的旧行为。
	if !s.settings.AllowsSite(abs) {
		return fsutil.Mount{}, errors.New("目标目录超出白名单；当前可挂载范围：" +
			strings.Join(s.settings.SiteRoots(), "、") +
			"（可用 --root-allow 指定白名单，或 --allow-any-site 显式放开）")
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
	s.warnSiteACLOverlap(m.Label)
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

// warnSiteACLOverlap 在站点标签与现有 ACL 规则重叠时给出安全提示。
//
// 标签按 URL 首段路由，ACL 也按 URL 前缀匹配：一条 `/backup:rw` 规则在
// 名为 backup 的站点挂上之后会作用于新站点，而配置者很可能只想着主目录。
// 不强行拒绝（遮蔽同名目录是这套 URL 形状的既有取舍），但必须让运维看见。
func (s *Server) warnSiteACLOverlap(label string) {
	if s.auth == nil {
		return
	}
	if hits := s.auth.RulePathsUnder("/" + label); len(hits) > 0 {
		s.logger.Errorf("安全提示：站点标签 %q 与现有 ACL 规则 %s 重叠，"+
			"这些规则会同时作用于新站点；如非本意请换一个标签或调整规则",
			label, strings.Join(hits, "、"))
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
	// 与站点增删共用一把锁：否则「检查 MultiRoot() 为 false」与 SetRoot
	// 之间可能插进一个 AddSite，导致新主根与已有站点互相嵌套，破坏
	// Roots 依赖的「各根互不嵌套」不变量。
	s.sitesMu.Lock()
	defer s.sitesMu.Unlock()

	// 兜底：updateSettings 已经拦过一次，但这里再挡一道 ——
	// 换根会改 *Resolver 里的根，而多根用的是 *Roots，语义上根本不该走到这。
	if s.MultiRoot() {
		return errors.New("多根模式下不支持切换服务根目录")
	}
	if s.res.SingleFile() != "" {
		return errors.New("单文件模式不支持切换服务根目录；请以目录模式启动")
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
		// 未启用鉴权不等于「调用者是管理员」：GET 仍可用，但只有显式放开
		// 匿名管理时才下发服务根 / 站点列表等敏感信息（见 buildSettings）。
		if s.cfg.AllowAnonymousAdmin {
			return permResult{Perm: auth.PermReadWrite, Authenticated: true}, true
		}
		return permResult{Perm: auth.PermNone}, true
	}
	user, pass, hasCred, class := credentialsWithSource(r)
	ip, ok := s.checkAuthLimit(w, r, class, user)
	if !ok {
		return permResult{}, false
	}
	if !hasCred || user == "" || !s.auth.Verify(user, pass) {
		if hasCred {
			s.noteAuthFailure(ip, class, user)
		}
		if shouldChallenge(r) {
			w.Header().Set("WWW-Authenticate", `Basic realm="gofs", charset="UTF-8"`)
		}
		http.Error(w, "401 Unauthorized", http.StatusUnauthorized)
		return permResult{}, false
	}
	s.noteVerifiedUser(r, user)
	s.noteAuthSuccess(ip, class, user)
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
		// 未启用鉴权时默认不把任何人当作管理员：管理接口关闭，
		// 页面上也不该出现「服务设置」入口。确实需要无鉴权管理时，
		// 由 --allow-anonymous-admin 显式放开。
		return s.cfg.AllowAnonymousAdmin
	}
	// ⚠️ 分享令牌本身不是管理员身份。可读写分享在范围内的 Perm 就是
	// PermReadWrite，若直接拿 permOf(r, "/") 判断，一条 scope 为 / 的
	// 可读写分享会让陌生人解锁「服务设置」（换根、重启、卸载）、用户管理
	// 与密钥面板 —— 那不是“能改几个文件”，而是把整个服务交出去。
	//
	// 但也不能只看“请求里有没有分享令牌”：分享 Cookie 会随浏览器带到
	// 同路径的后续请求上，管理员打开一次只读分享后，自己的有效会话
	// 就会被这条规则误伤（设置/用户/密钥/升级/重启全部消失）。
	//
	// 正确判据是**真实身份**对根的权限，完全忽略分享令牌：只有存在一个
	// 已认证、且对 / 有读写权限的显式账号或会话时才是管理员；纯分享游客
	// 的 identityPermOf 只会得到匿名规则（通常为 None），仍然出局。
	// 匿名规则即使给了 rw，也不能让页面显示管理入口 —— 管理接口本身
	// 仍要求会话/账号，隐藏入口才不会出现“按钮在但点了 401”。
	perm, _, authenticated := s.identityPermOf(r, "/")
	return authenticated && perm == auth.PermReadWrite
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
	// 未启用鉴权时**不能**把调用者当成管理员。
	//
	// 管理接口能挂载服务根之外的目录、切换服务根、改监听地址、替换二进制
	// 甚至卸载服务 —— 这些已经超出「被服务的那棵目录」。无鉴权部署意味着
	// 任何能连上端口的人都能访问，默许他们做这些等于把整台机器交出去。
	// 需要这种部署时用 --allow-anonymous-admin 显式承担风险。
	if !s.auth.Enabled() {
		if s.cfg.AllowAnonymousAdmin {
			return true
		}
		http.Error(w, "403 Forbidden: 未启用鉴权，服务管理接口默认关闭"+
			"（如确需在无鉴权模式下管理，请显式使用 --allow-anonymous-admin）",
			http.StatusForbidden)
		return false
	}
	user, pass, hasCred, class := credentialsWithSource(r)
	ip, ok := s.checkAuthLimit(w, r, class, user)
	if !ok {
		return false
	}

	if hasCred {
		// 显式凭据：先验密码，再判断权限。密码错误才计入失败，
		// 一个合法的普通账号访问管理接口得到的是 403，而不是被封禁。
		if !s.auth.Verify(user, pass) {
			s.noteAuthFailure(ip, class, user)
			if shouldChallenge(r) {
				w.Header().Set("WWW-Authenticate", `Basic realm="gofs", charset="UTF-8"`)
			}
			http.Error(w, "401 Unauthorized", http.StatusUnauthorized)
			return false
		}
		s.noteVerifiedUser(r, user)
		s.noteAuthSuccess(ip, class, user)
		if s.isAdminRequest(r) {
			return true
		}
		http.Error(w, "403 Forbidden: "+action+"需要对服务根拥有读写权限",
			http.StatusForbidden)
		return false
	}

	// 没有 Basic 凭据时只能靠会话 cookie。无效 Cookie 不计入失败：
	// 它是 256 位随机值，伪造/过期都不构成口令猜测，计入只会让
	// 匿名请求有机会污染有效会话的限速桶。合法但非管理员的会话
	// 只回 403。
	if _, ok := s.auth.ResolveSession(sessionToken(r)); !ok {
		if shouldChallenge(r) {
			w.Header().Set("WWW-Authenticate", `Basic realm="gofs", charset="UTF-8"`)
		}
		http.Error(w, "401 Unauthorized", http.StatusUnauthorized)
		return false
	}
	if s.isAdminRequest(r) {
		return true
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
