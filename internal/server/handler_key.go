package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/xxl6097/gofs/internal/auth"
	"github.com/xxl6097/gofs/internal/fsutil"
	"github.com/xxl6097/gofs/internal/uploadkey"
)

// maxKeyTTLSeconds 是密钥/分享链接允许的最长有效期（10 年）。
//
// 存在是为了挡住整数溢出：请求里的秒数会乘成纳秒放进 time.Duration，
// 超过约 292 年就会绕成负数，而下游用的是 `TTL > 0` 判断 ——
// 结果是「请求一个超长有效期，拿到一把永不过期的钥匙」。
const maxKeyTTLSeconds = 10 * 365 * 24 * 3600

// keyView 是返回给前端的密钥视图。
// 刻意不含任何可用于认证的字段——明文只在创建时返回一次，摘要则永不外泄。
type keyView struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Prefix     string `json:"prefix"`
	Scope      string `json:"scope"`
	Kind       string `json:"kind"`
	Creator    string `json:"creator"`
	Status     string `json:"status"`
	CreatedAt  string `json:"created_at"`
	ExpiresAt  string `json:"expires_at,omitempty"`
	Permanent  bool   `json:"permanent"`
	RemainingS int64  `json:"remaining_seconds"`
	LastUsedAt string `json:"last_used_at,omitempty"`
	UseCount   int64  `json:"use_count"`
	Note       string `json:"note,omitempty"`
}

// keyCreateRequest 是创建密钥的请求体。
type keyCreateRequest struct {
	Name       string `json:"name"`
	Scope      string `json:"scope"`
	TTLSeconds int64  `json:"ttl_seconds"` // 0 表示永不过期
	Note       string `json:"note"`
	// Kind 为凭证类型："upload"（默认）、"read"（只读分享链接）
	// 或 "readwrite"（可读写分享链接）。
	Kind string `json:"kind"`
}

// handleKeys 管理上传密钥。
//
//	GET    列出全部密钥
//	POST   创建并返回一次性明文
//	DELETE 撤销（?id=）
//
// 密钥管理只接受账号凭据：若允许上传密钥来管理密钥，
// 一把只能传文件的凭证就能给自己签发新凭证，等于权限提升。
func (s *Server) handleKeys(w http.ResponseWriter, r *http.Request) {
	if s.keys == nil {
		http.Error(w, "404 Not Found: 本服务未启用上传密钥", http.StatusNotFound)
		return
	}
	if !s.cfg.AllowKeys {
		http.Error(w, "403 Forbidden: 未开启上传密钥（--allow-keys）", http.StatusForbidden)
		return
	}
	// 未启用鉴权时管理接口默认关闭：否则任何能连上端口的人都能签发
	// 长期有效的上传/分享凭证（见 requireAdminOr 的同一条策略）。
	if !s.auth.Enabled() && !s.cfg.AllowAnonymousAdmin {
		http.Error(w, "403 Forbidden: 未启用鉴权，密钥管理默认关闭"+
			"（如确需在无鉴权模式下管理，请显式使用 --allow-anonymous-admin）",
			http.StatusForbidden)
		return
	}

	// 密钥管理只接受账号凭据：若允许上传密钥来管理密钥，
	// 一把只能传文件的凭证就能给自己签发新凭证，等于权限提升。
	//
	// 这里刻意不用 authorize("/")：完全不相关的路径级权限不应成为
	// 管理密钥的门槛。先验凭据，再看是否在任一目录上有写权限。
	user, pass, hasCred := credentials(r)
	admin := !s.auth.Enabled()
	if s.auth.Enabled() {
		// 这个接口同样能验证口令，必须与 authorize 用同一套失败计数/封禁；
		// 否则 /fs/keys 会成为一条不限速的密码猜测入口。
		ip, ok := s.checkAuthLimit(w, r, user)
		if !ok {
			return
		}
		if !hasCred || !s.auth.Verify(user, pass) {
			if hasCred {
				s.noteAuthFailure(ip, user)
			}
			if shouldChallenge(r) {
				w.Header().Set("WWW-Authenticate", `Basic realm="gofs", charset="UTF-8"`)
			}
			http.Error(w, "401 Unauthorized", http.StatusUnauthorized)
			return
		}
		s.noteAuthSuccess(ip, user)
		admin = s.auth.Lookup("/", user, pass, true) == auth.PermReadWrite
	}
	if !s.auth.HasAnyWrite(user, pass, hasCred) {
		http.Error(w, "403 Forbidden: 管理上传密钥需要有可写的目录权限", http.StatusForbidden)
		return
	}

	switch r.Method {
	case http.MethodGet, http.MethodHead:
		s.keyList(w, user, admin)
	case http.MethodPost:
		s.keyCreate(w, r, user, pass)
	case http.MethodDelete:
		s.keyRevoke(w, r, user, admin)
	default:
		w.Header().Set("Allow", "GET, HEAD, POST, DELETE")
		http.Error(w, "405 Method Not Allowed", http.StatusMethodNotAllowed)
	}
}

// keyList 返回密钥列表。
//
// 非管理员只能看到**自己创建的**密钥：否则一个只在某个子目录有写权限的
// 账号可以看到全站密钥的范围、创建者等信息，并据此发起越权撤销。
func (s *Server) keyList(w http.ResponseWriter, user string, admin bool) {
	keys := s.keys.List()
	views := make([]keyView, 0, len(keys))
	for _, k := range keys {
		if !admin && k.Creator != user {
			continue
		}
		views = append(views, s.keyView(k))
	}
	out := map[string]any{
		"keys":       views,
		"persistent": s.keys.Persistent(),
		"total":      len(views),
	}
	// 密钥文件路径属于主机布局信息，只给管理员。
	if admin {
		out["path"] = s.keys.Path()
	}
	s.writeJSON(w, out)
}

// keyCreate 创建密钥并返回**只此一次**的明文。
//
// creator/password 来自 handleKeys 已校验过的账号凭据。范围授权只按这个
// 账号计算，**不能**走 checkPerm —— 后者会优先识别 ?share= / 分享 cookie，
// 于是一个临时分享令牌就能换来一把永久密钥。
func (s *Server) keyCreate(w http.ResponseWriter, r *http.Request, creator, password string) {
	var req keyCreateRequest
	resetRead := withBodyReadDeadline(w, apiBodyReadTimeout)
	defer resetRead()
	dec := json.NewDecoder(io.LimitReader(r.Body, 64<<10))
	if err := dec.Decode(&req); err != nil && !errors.Is(err, io.EOF) {
		http.Error(w, "400 Bad Request: 请求体不是合法 JSON", http.StatusBadRequest)
		return
	}
	if req.TTLSeconds < 0 {
		http.Error(w, "400 Bad Request: 有效期不能为负数", http.StatusBadRequest)
		return
	}
	// 上限不只是「别太过分」：这个数会被乘成纳秒塞进 time.Duration，
	// 而 Duration 是 int64 —— 超过约 292 年就溢出成负数，
	// 于是下面的 `TTL > 0` 判成 false，密钥**悄悄变成永不过期**。
	// 「请求 300 年有效期，拿到一把永久钥匙」是这里最坏的失败方式，
	// 所以宁可明确拒绝。
	if req.TTLSeconds > maxKeyTTLSeconds {
		http.Error(w, fmt.Sprintf("400 Bad Request: 有效期最多 %d 天（要永久请填 0）",
			maxKeyTTLSeconds/86400), http.StatusBadRequest)
		return
	}
	if err := uploadkey.ValidateScope(req.Scope); err != nil {
		http.Error(w, "400 Bad Request: "+err.Error(), http.StatusBadRequest)
		return
	}
	if fsutil.HasParentSegment(req.Scope) {
		http.Error(w, "400 Bad Request: 目标目录不允许出现 ..", http.StatusBadRequest)
		return
	}

	// 作用范围必须落在创建者自己够得着的地方，否则等于把权限借给了自己够不到的地方。
	//
	// 要求的权限按类型区分：上传密钥要**写**权限（能传才能授），
	// 只读分享要**读**权限（能看才能分享），可读写分享要**读写**权限
	// （授出写权限自己就得先有）。三者的共同原则都是「只能授出自己有的」。
	kind := uploadkey.KindUpload
	need := auth.PermReadWrite
	needDesc := "写"
	switch req.Kind {
	case uploadkey.KindRead:
		kind = uploadkey.KindRead
		need = auth.PermRead
		needDesc = "读"
	case uploadkey.KindReadWrite:
		// 只读账号不能签发可读写分享 —— 那等于绕过「只能授出自己有的」
		// 把自己没有的写权限借出去。
		kind = uploadkey.KindReadWrite
		need = auth.PermReadWrite
		needDesc = "读写"
	}
	scope := fsutil.CleanURLPath(req.Scope)
	// ⚠️ 只用账号凭据计算权限。checkPerm 会优先识别分享令牌，
	// 那会让一条临时分享变成永久密钥/分享（见 handler_key 顶部注释）。
	if sp := s.auth.Lookup(scope, creator, password, true); sp < need {
		http.Error(w, fmt.Sprintf("403 Forbidden: 你对 %s 没有%s权限，无法签发该范围的凭证", scope, needDesc),
			http.StatusForbidden)
		return
	}
	// 分享链接会把内容**公开**出去（拿到链接的人不需要登录），
	// 所以额外要求服务开启了上传/共享能力，避免在一个只读实例上
	// 悄悄开出一条公开通道。两种分享都算。
	if uploadkey.IsShareKind(kind) && !s.cfg.AllowUpload && !s.cfg.AllowKeys {
		http.Error(w, "403 Forbidden: 本服务未开启任何可写的功能，无法创建分享链接",
			http.StatusForbidden)
		return
	}

	k, token, err := s.keys.Create(uploadkey.CreateOptions{
		Name:    req.Name,
		Scope:   scope,
		TTL:     time.Duration(req.TTLSeconds) * time.Second,
		Creator: creator,
		Note:    req.Note,
		Kind:    kind,
	})
	if err != nil {
		switch {
		case errors.Is(err, uploadkey.ErrTooManyKeys):
			http.Error(w, "409 Conflict: "+err.Error(), http.StatusConflict)
		case errors.Is(err, uploadkey.ErrInvalidOption):
			http.Error(w, "400 Bad Request: "+err.Error(), http.StatusBadRequest)
		default:
			s.writeErr(w, err)
		}
		return
	}

	ttlDesc := "永不过期"
	if !k.Permanent() {
		ttlDesc = "有效期至 " + k.ExpiresAt.Format(time.RFC3339)
	}
	if uploadkey.IsShareKind(kind) {
		// 权限级别写进日志：事后审计「这条链接当初是只读还是可读写」
		// 只能靠这里，明文与摘要都不含这个信息。
		lvl := "只读"
		if k.ReadWrite() {
			lvl = "可读写"
		}
		s.logger.Infof("创建分享链接 %s（%s，范围 %s，%s，创建者 %s）",
			k.Prefix, lvl, k.Scope, ttlDesc, creator)
	} else {
		s.logger.Infof("签发上传密钥 %s（%s，范围 %s，%s，创建者 %s）",
			k.Prefix, k.Name, k.Scope, ttlDesc, creator)
	}

	s.writeJSONStatus(w, http.StatusCreated, map[string]any{
		"key":   s.keyView(k),
		"token": token,
		"warn":  "密钥明文只显示这一次，请立即复制保存；服务端只保存摘要，无法再次查看。",
		"usage": map[string]string{
			// 一键脚本：服务端把密钥与地址填好后下发一段可直接执行的脚本，
			// 适合批量传目录（保留层级），不用手改任何地方。
			//
			// 末尾那行注释是必要的：这条命令本身看不出文件会落到哪，
			// 而落点由密钥范围决定 —— 不写出来用户只能靠猜。
			"script": "bash <(curl -sS \"<服务地址>/up?key=" + token + "\") 文件或目录... [目标子目录]\n" +
				"# 目录连层级一起传；不带目标子目录就落到 " + scriptCommentText(k.Scope),
			"header": "curl -T local.txt -H 'X-Gofs-Upload-Key: " + token + "' \"<服务地址>/<目标路径>\"",
			"query":  "curl -T local.txt \"<服务地址>/<目标路径>?key=" + token + "\"",
			"scp":    "X-Gofs-Upload-Key 头可用于任何 HTTP 客户端；权限仅限上传，且局限在 " + scriptCommentText(k.Scope),
		},
	})
}

// keyRevoke 删除指定密钥。
//
// 非管理员只能撤销自己创建的密钥。只按 id 撤销意味着任何有写权限的
// 账号都能吊销管理员的分享链接或 CI 使用的上传密钥 —— 这是跨账号的
// 权限破坏。
func (s *Server) keyRevoke(w http.ResponseWriter, r *http.Request, user string, admin bool) {
	id := r.URL.Query().Get("id")
	if id == "" {
		http.Error(w, "400 Bad Request: 缺少 id 参数", http.StatusBadRequest)
		return
	}
	k, ok := s.keys.Get(id)
	if !ok {
		http.Error(w, "404 Not Found: 密钥不存在或已被撤销", http.StatusNotFound)
		return
	}
	if !admin && k.Creator != user {
		http.Error(w, "403 Forbidden: 只能撤销自己创建的密钥", http.StatusForbidden)
		return
	}
	if err := s.keys.Revoke(id); err != nil {
		if errors.Is(err, uploadkey.ErrNotFound) {
			http.Error(w, "404 Not Found: 密钥不存在或已被撤销", http.StatusNotFound)
			return
		}
		s.writeErr(w, err)
		return
	}
	s.logger.Infof("撤销上传密钥 %s", id)
	w.WriteHeader(http.StatusNoContent)
}

// keyView 把密钥记录转成前端视图。
func (s *Server) keyView(k *uploadkey.Key) keyView {
	now := time.Now()
	v := keyView{
		ID:         k.ID,
		Name:       k.Name,
		Prefix:     k.Prefix,
		Scope:      k.Scope,
		Kind:       k.KindOf(),
		Creator:    k.Creator,
		Status:     k.Status(now),
		CreatedAt:  k.CreatedAt.Format(time.RFC3339),
		Permanent:  k.Permanent(),
		RemainingS: int64(k.Remaining(now).Seconds()),
		UseCount:   k.UseCount,
		Note:       k.Note,
	}
	if !k.ExpiresAt.IsZero() {
		v.ExpiresAt = k.ExpiresAt.Format(time.RFC3339)
	}
	if !k.LastUsedAt.IsZero() {
		v.LastUsedAt = k.LastUsedAt.Format(time.RFC3339)
	}
	return v
}

// uploadKeyToken 从请求中提取上传密钥。
// 支持两种携带方式：请求头（推荐）与查询参数（便于 <a> 直链与 curl 一行搞定）。
func uploadKeyToken(r *http.Request) string {
	if v := r.Header.Get("X-Gofs-Upload-Key"); v != "" {
		return v
	}
	return r.URL.Query().Get("key")
}

// handleWithUploadKey 用上传密钥处理一次请求。
//
// 密钥的能力被刻意收得很窄：
//   - 只能上传（PUT / POST），不能读、不能删、不能改名、不能解压；
//   - 只能落在 Scope 限定的目录内（含日期归档后的路径）；
//   - 仍然受全局 --allow-upload 约束。
func (s *Server) handleWithUploadKey(w http.ResponseWriter, r *http.Request, urlPath, token string) {
	if s.keys == nil {
		http.Error(w, "401 Unauthorized: 本服务未启用上传密钥", http.StatusUnauthorized)
		return
	}

	k, err := s.keys.Verify(token)
	if err != nil {
		s.logger.Errorf("上传密钥校验失败（%s %s）：%v", r.Method, r.URL.Path, err)
		http.Error(w, "401 Unauthorized: "+err.Error(), http.StatusUnauthorized)
		return
	}

	// 令牌得是**上传**那一种。
	//
	// Verify 只回答「这串令牌有效吗」，不回答「它是哪一种」—— 少了这一判，
	// 一条发给第三方的**只读**分享链接（kind=read）就能反过来当写凭据用：
	// PUT/POST 全部走通，而且落在该链接自己的 scope 内，看上去还很「合规」。
	//
	// 反方向的同类判断在 authorize 里（上传密钥不能当分享令牌读内容），
	// 两个方向都要堵 —— 只堵一边等于没堵，而这正是当初漏掉的一边。
	if k.KindOf() != uploadkey.KindUpload {
		s.logger.Errorf("拒绝用只读分享令牌上传（%s %s）", r.Method, r.URL.Path)
		http.Error(w, "403 Forbidden: 该令牌是只读分享链接，不能用于上传",
			http.StatusForbidden)
		return
	}

	if r.Method != http.MethodPut && r.Method != http.MethodPost {
		http.Error(w, "403 Forbidden: 上传密钥只能用于上传（PUT/POST）", http.StatusForbidden)
		return
	}
	if !s.cfg.AllowUpload {
		http.Error(w, "403 Forbidden: 本服务未开启上传（--allow-upload）", http.StatusForbidden)
		return
	}
	// scope 之外，创建者当前 ACL 也是上限：签名时 /docs 可写，不代表
	// 之后仍能写 /docs/finance（更具体的只读规则可能后加/一直存在）。
	if !s.uploadKeyAllowed(r, k, urlPath) {
		http.Error(w, fmt.Sprintf("403 Forbidden: 该密钥只允许上传到 %s，或创建者已无对应权限", k.Scope),
			http.StatusForbidden)
		return
	}
	abs, err := s.res.Resolve(urlPath)
	if err != nil {
		http.Error(w, "403 Forbidden", http.StatusForbidden)
		return
	}
	// 软链/大小写别名可能把实际写入位置带到 scope/创建者权限之外。
	if realURL, within := s.realURLOf(abs); !within || !s.uploadKeyAllowed(r, k, realURL) {
		http.Error(w, fmt.Sprintf("403 Forbidden: 该密钥只允许上传到 %s，或创建者已无对应权限", k.Scope),
			http.StatusForbidden)
		return
	}

	p := permResult{
		Perm:          auth.PermReadWrite,
		User:          "key:" + k.Prefix,
		Authenticated: true,
		UploadKey:     k,
	}

	switch r.Method {
	case http.MethodPut:
		s.handlePut(w, r, urlPath, p)
	case http.MethodPost:
		s.handlePost(w, r, urlPath, p)
	}
}
