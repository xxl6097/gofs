package server

import (
	"net/http"
	"strings"
	"time"

	"github.com/xxl6097/gofs/internal/auth"
	"github.com/xxl6097/gofs/internal/fsutil"
)

// handleAuth 校验 Basic 凭据，供前端登录框使用。
//
// 与普通接口有三点不同：
//   - 不返回 WWW-Authenticate 挑战，否则浏览器会在页面上弹出脱离
//     应用上下文的原生登录框，打断应用自己的登录流程；
//   - 校验只看「账号密码是否有效」（auth.Verify），不要求对某个路径有权限，
//     否则一个只被授予 /docs 权限的账号会因为对根目录无权限而登录失败；
//   - 失败信息以 JSON 返回，方便前端直接展示。
func (s *Server) handleAuth(w http.ResponseWriter, r *http.Request) {
	// DELETE 是登出：撤销服务端会话并让浏览器丢掉 cookie。
	// 必须由服务端做 —— cookie 是 HttpOnly 的，JS 删不掉它。
	if r.Method == http.MethodDelete {
		s.handleLogout(w, r)
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodPost {
		w.Header().Set("Allow", "GET, POST, DELETE")
		http.Error(w, "405 Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}

	// 未启用鉴权时前端不必显示登录入口。
	if !s.auth.Enabled() {
		s.writeJSON(w, map[string]any{
			"auth_on":       false,
			"authenticated": true,
			"anonymous":     true,
			"perms":         s.buildPerms(permResult{Perm: auth.PermReadWrite}),
		})
		return
	}

	// 与 authorize 用同一套限速：这个接口会下发会话 cookie，不设闸门
	// 等于把口令交给字典攻击 —— 登录成功一次就能拿到一张长期通行证，
	// 比逐个请求试密码划算得多。
	user, pass, ok, class := credentialsWithSource(r)
	ip := s.clientIP(r)
	passwordLogin := ok && user != ""

	if passwordLogin {
		// 超过失败阈值后先等一小段，再照常验密码：限速要拖慢字典攻击，
		// 但不能让攻击者仅凭用户名就把合法账号锁死（正确密码永远能进）。
		already, remain := s.authThrottled(ip, class, user)
		if already {
			if !s.waitAuthThrottle(r, remain) {
				return
			}
		}
		if !s.auth.Verify(user, pass) {
			s.noteAuthFailure(ip, class, user)
			s.logger.Errorf("登录失败：账号 %q 凭据不正确", truncateAuthUser(user))
			// 密码已经验过且失败，此时回 429 不会误伤拿对密码的人。
			// 达到阈值的那一次仍回 401，下一次才开始回 429。
			if already {
				w.Header().Set("Retry-After", "1")
				s.writeJSONStatus(w, http.StatusTooManyRequests, map[string]any{
					"message": "认证失败次数过多，请稍后再试",
				})
				return
			}
			s.writeJSONStatus(w, http.StatusUnauthorized, map[string]any{
				"message": "用户名或密码不正确",
			})
			return
		}
		s.noteVerifiedUser(r, user)
		s.noteAuthSuccess(ip, class, user)
	} else {
		// 没有显式凭据时，允许用现有的 HttpOnly 会话刷新身份。这样页面
		// 刷新/重新打开时不再需要从 localStorage 读取 Basic 密码。
		u, valid := s.auth.ResolveSession(s.sessionToken(r))
		if !valid || u == "" {
			s.writeJSONStatus(w, http.StatusUnauthorized, map[string]any{
				"message": "请输入用户名与密码",
			})
			return
		}
		user = u
	}

	// 下发会话 cookie。之后浏览器**自己**发起的请求（<video>/<img> 的加载、
	// <a download> 的下载）会带上它，于是 Range 流式播放与原生下载都可用，
	// 不再需要把整个文件读成 blob —— 那是大文件播不了的根本原因。
	//
	// ⚠️ GET 且凭据来自查询串时**不下发**会话：跨站页面可以用
	// <img src=".../fs/auth?user=attacker&pass=x"> 让受害者浏览器
	// 登录到攻击者账号（登录 CSRF）。查询串是 `<img>` 唯一能带上的
	// 凭据形式；而 Authorization 头跨站设置不了，POST 又有同源校验。
	// 这类 GET 仍然可以做一次凭据校验，只是不产生会话。
	if passwordLogin {
		fromQuery := class == authClassQuery
		// GET + Basic 仍可校验凭据，但只有明确带 X-Gofs-Ajax（应用自己的登录框）
		// 或 POST 才建立会话。否则 `https://user:pass@host/fs/auth` 这类顶层导航
		// 会让受害者静默登录到攻击者账号（登录 CSRF）；非浏览器客户端请改用 POST。
		issueSession := r.Method == http.MethodPost ||
			(r.Method == http.MethodGet && r.Header.Get("X-Gofs-Ajax") == "1")
		if !fromQuery && issueSession {
			// 会话表满时宁可拒绝登录，也不发一张无法撤销的通行证。
			token, ok := s.auth.NewSession(user)
			if !ok {
				s.writeJSONStatus(w, http.StatusServiceUnavailable, map[string]any{
					"message": "会话数已达上限，请稍后再试",
				})
				return
			}
			remember := false
			switch strings.ToLower(strings.TrimSpace(r.URL.Query().Get("remember"))) {
			case "1", "true", "yes", "on":
				remember = true
			}
			s.setSessionCookie(w, r, token, remember)
		}
		s.logger.Infof("登录成功：%s", user)
	}

	// 该账号的权限用于决定页面按钮显隐。
	// 默认按根目录计算，但对根目录无权限时退化为只读，避免整页按钮全灭。
	// 调用方可以带 ?path= 指定自己实际所在的目录：一个只被授予 /docs 权限的
	// 账号站在 /docs 里时，理应拿到完整的按钮，而不是被当成只读。
	// 会话与 Basic 都按**已确认的账号名**计算，避免再验一次密码。
	perm := s.auth.PermFor(user, "/")
	if p := r.URL.Query().Get("path"); p != "" && !fsutil.HasParentSegment(p) {
		if scoped := s.auth.PermFor(user, p); scoped != auth.PermNone {
			perm = scoped
		}
	}
	if perm == auth.PermNone {
		perm = auth.PermRead
	}

	// 登录后该落到哪个目录、哪些上级地址读得到。
	//
	// 只被授予 /docs 的账号站在根目录上什么都读不到，而前端把这次失败当成
	// 401（服务端对「匿名也没权限的路径」回的就是 401）→ 判定为「登录已失效」
	// → 清凭据 + 重新弹登录框。表现就是「新建一个限制在子目录的账号，
	// 登录进去就提示打开目录失败」，而且怎么登都进不去。
	//
	// home_path 让前端换乘到它自己的目录；read_paths 让它别把必被拒绝的
	// 上级（面包屑上的「根目录」、「上一级」按钮）画成能点的链接 ——
	// 那两处是同一个坑的另一半。
	home := s.auth.HomePathFor(user)
	readPaths := s.auth.ReadPathsFor(user)

	// 除了权限，还要把「未登录外壳里刻意没下发」的配置一并补齐，
	// 否则前端登录后仍然不知道上传归档、在线编辑、密钥等能力是否可用。
	//
	// allow_settings 要按**服务根**的权限算，而不是调用方所在的路径：
	// 只有能读写整个服务的账号才该看到「服务设置」入口。
	admin := s.auth.PermFor(user, "/") == auth.PermReadWrite
	reply := map[string]any{
		"auth_on":       true,
		"authenticated": true,
		"user":          user,
		// 来自启动参数的账号密码以命令行为准，但可以保存为覆盖值。
		// 前端据此在改密弹窗里说明覆盖值的行为。
		"user_from_startup": s.auth.FromStartup(user),
		// 用户表为空表示仅内存存储：改密在重启后会失效。
		"password_persistent": s.auth.UserFile() != "",
		"anonymous":           false,
		"perms":               s.buildPerms(permResult{Perm: perm, User: user, Authenticated: true}),
		"upload_dated":        s.cfg.UploadDated(),
		"allow_keys":          s.cfg.AllowKeys && perm == auth.PermReadWrite,
		"edit_max_size":       s.settings.EditMaxSize(),
		"upload_max_size":     s.settings.UploadMaxSize(),
		"allow_settings":      admin,
		"allow_users":         s.cfg.AllowUserManage && admin,
		// 与 edit_max_size 同理：登录响应里必须补齐。页面外壳走的是
		// renderAuthGate（数据全空），登录后再由 applySession 合并 ——
		// 漏了这一行，「升级与重启」就只有直接带会话访问时才出现，
		// 从登录框进来的反而看不到。
		"allow_upgrade": s.cfg.Upgrader != nil,
		"allow_restart": s.cfg.Restarter != nil,
		// 卸载是独立的扩展点，单独注入（与 Restarter 无关）。
		"allow_uninstall": s.canUninstall(),
		// 多根模式：界面据此把「服务根目录」那一项变成只读的挂载列表。
		"multi_root": s.MultiRoot(),
		// 落地目录与可达路径，见上面 home 的说明。
		"home_path":  home,
		"read_paths": readPaths,
	}
	if s.cfg.UploadDated() {
		reply["upload_date_dir"] = s.cfg.UploadDateDir(time.Now())
	}
	s.writeJSON(w, reply)
}

// handleLogout 撤销会话并清掉 cookie。
//
// 为什么要服务端参与：cookie 是 HttpOnly 的，JS 读不到也就删不掉。
// 只清 localStorage 的话，服务端那张会话还活着 —— 谁拿到 cookie 谁就还能读，
// 「登出」会变成一句假话。
//
// 无论会话是否存在都返回 204：登出是个应该永远成功的动作，
// 为一个已经过期的会话报错只会让前端多写一段没意义的处理。
func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if token := s.sessionToken(r); token != "" {
		s.auth.DropSession(token)
	}
	s.clearSessionCookie(w, r)
	w.WriteHeader(http.StatusNoContent)
}
