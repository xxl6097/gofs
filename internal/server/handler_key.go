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
	// Kind 为凭证类型："upload"（默认）或 "read"（分享链接）。
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

	// 密钥管理只接受账号凭据：若允许上传密钥来管理密钥，
	// 一把只能传文件的凭证就能给自己签发新凭证，等于权限提升。
	//
	// 这里刻意不用 authorize("/")：完全不相关的路径级权限不应成为
	// 管理密钥的门槛。先验凭据，再看是否在任一目录上有写权限。
	user, pass, hasCred := credentials(r)
	if s.auth.Enabled() {
		if !hasCred || !s.auth.Verify(user, pass) {
			if shouldChallenge(r) {
				w.Header().Set("WWW-Authenticate", `Basic realm="gofs", charset="UTF-8"`)
			}
			http.Error(w, "401 Unauthorized", http.StatusUnauthorized)
			return
		}
	}
	if !s.auth.HasAnyWrite(user, pass, hasCred) {
		http.Error(w, "403 Forbidden: 管理上传密钥需要有可写的目录权限", http.StatusForbidden)
		return
	}

	switch r.Method {
	case http.MethodGet, http.MethodHead:
		s.keyList(w)
	case http.MethodPost:
		s.keyCreate(w, r, user)
	case http.MethodDelete:
		s.keyRevoke(w, r)
	default:
		w.Header().Set("Allow", "GET, HEAD, POST, DELETE")
		http.Error(w, "405 Method Not Allowed", http.StatusMethodNotAllowed)
	}
}

// keyList 返回全部密钥。
func (s *Server) keyList(w http.ResponseWriter) {
	keys := s.keys.List()
	views := make([]keyView, 0, len(keys))
	for _, k := range keys {
		views = append(views, s.keyView(k))
	}
	s.writeJSON(w, map[string]any{
		"keys":       views,
		"persistent": s.keys.Persistent(),
		"path":       s.keys.Path(),
		"total":      len(views),
	})
}

// keyCreate 创建密钥并返回**只此一次**的明文。
func (s *Server) keyCreate(w http.ResponseWriter, r *http.Request, creator string) {
	var req keyCreateRequest
	dec := json.NewDecoder(io.LimitReader(r.Body, 64<<10))
	if err := dec.Decode(&req); err != nil && !errors.Is(err, io.EOF) {
		http.Error(w, "400 Bad Request: 请求体不是合法 JSON", http.StatusBadRequest)
		return
	}
	if req.TTLSeconds < 0 {
		http.Error(w, "400 Bad Request: 有效期不能为负数", http.StatusBadRequest)
		return
	}
	if fsutil.HasParentSegment(req.Scope) {
		http.Error(w, "400 Bad Request: 目标目录不允许出现 ..", http.StatusBadRequest)
		return
	}

	// 作用范围必须落在创建者自己够得着的地方，否则等于把权限借给了自己够不到的地方。
	//
	// 要求的权限按类型区分：上传密钥要**写**权限（能传才能授），
	// 分享链接要**读**权限（能看才能分享）。两边的共同原则都是
	// 「只能授出自己有的」。
	kind := uploadkey.KindUpload
	need := auth.PermReadWrite
	needDesc := "写"
	if req.Kind == uploadkey.KindRead {
		kind = uploadkey.KindRead
		need = auth.PermRead
		needDesc = "读"
	}
	scope := fsutil.CleanURLPath(req.Scope)
	if sp := s.checkPerm(r, scope); sp < need {
		http.Error(w, fmt.Sprintf("403 Forbidden: 你对 %s 没有%s权限，无法签发该范围的凭证", scope, needDesc),
			http.StatusForbidden)
		return
	}
	// 分享链接会把内容**公开**出去（拿到链接的人不需要登录），
	// 所以额外要求服务开启了上传/共享能力，避免在一个只读实例上
	// 悄悄开出一条公开通道。
	if kind == uploadkey.KindRead && !s.cfg.AllowUpload && !s.cfg.AllowKeys {
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
		s.writeErr(w, err)
		return
	}

	ttlDesc := "永不过期"
	if !k.Permanent() {
		ttlDesc = "有效期至 " + k.ExpiresAt.Format(time.RFC3339)
	}
	if kind == uploadkey.KindRead {
		s.logger.Infof("创建分享链接 %s（范围 %s，%s，创建者 %s）", k.Prefix, k.Scope, ttlDesc, creator)
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
				"# 目录连层级一起传；不带目标子目录就落到 " + k.Scope,
			"header": "curl -T local.txt -H 'X-Gofs-Upload-Key: " + token + "' \"<服务地址>/<目标路径>\"",
			"query":  "curl -T local.txt \"<服务地址>/<目标路径>?key=" + token + "\"",
			"scp":    "X-Gofs-Upload-Key 头可用于任何 HTTP 客户端；权限仅限上传，且局限在 " + k.Scope,
		},
	})
}

// keyRevoke 删除指定密钥。
func (s *Server) keyRevoke(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("id")
	if id == "" {
		http.Error(w, "400 Bad Request: 缺少 id 参数", http.StatusBadRequest)
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

	if r.Method != http.MethodPut && r.Method != http.MethodPost {
		http.Error(w, "403 Forbidden: 上传密钥只能用于上传（PUT/POST）", http.StatusForbidden)
		return
	}
	if !s.cfg.AllowUpload {
		http.Error(w, "403 Forbidden: 本服务未开启上传（--allow-upload）", http.StatusForbidden)
		return
	}
	if !k.AllowsPath(r.URL.Path) {
		http.Error(w, fmt.Sprintf("403 Forbidden: 该密钥只允许上传到 %s", k.Scope), http.StatusForbidden)
		return
	}
	if _, err := s.res.Resolve(urlPath); err != nil {
		http.Error(w, "403 Forbidden", http.StatusForbidden)
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
