package server

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/xxl6097/gofs/internal/auth"
)

// userRuleJSON 是路径规则在接口上的表示。
type userRuleJSON struct {
	Path string `json:"path"`
	Perm string `json:"perm"` // "rw" / "r"
}

// userJSON 是账号在接口上的表示。**不含任何密码材料**。
type userJSON struct {
	Name        string         `json:"name"`
	Rules       []userRuleJSON `json:"rules"`
	FromStartup bool           `json:"from_startup"`
	CreatedAt   string         `json:"created_at,omitempty"`
	UpdatedAt   string         `json:"updated_at,omitempty"`
}

// usersReply 是用户管理接口的响应。
type usersReply struct {
	Users []userJSON `json:"users"`
	// Anonymous 为匿名访问的规则（启动参数决定，界面只读展示）。
	Anonymous []userRuleJSON `json:"anonymous"`
	// Editable 表示当前部署是否允许在页面上增删改用户。
	Editable bool `json:"editable"`
	// UserFile 为用户表落盘路径（空串表示仅内存）。
	UserFile string `json:"user_file"`
	// AuthOn 表示服务是否启用了鉴权。没启用时无法创建用户（见 updateUsers）。
	AuthOn bool `json:"auth_on"`
}

// usersRequest 是新建/修改用户的请求体。
//
// Password 用指针是为了区分两种语义：
//   - 新建：必须给，且不能为空；
//   - 修改：nil 或空串 = 不改密码。
type usersRequest struct {
	Name     string         `json:"name"`
	Password *string        `json:"password"`
	Rules    []userRuleJSON `json:"rules"`
}

// handleUsers 管理用户表（增删改查）。
//
// 账号的另一半来源是启动参数（-a / GOFS_AUTH）：那部分在这里是**只读**的，
// 界面上会标出来，改动一律拒绝 —— 命令行才是它们的权威来源。
func (s *Server) handleUsers(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet, http.MethodHead:
		// 连列表都只给管理员：账号名本身也是信息，没必要让只有某个子目录
		// 只读权限的账号看到「这台服务上还有谁」。
		if !s.requireUserManage(w, r) {
			return
		}
		s.writeJSON(w, s.buildUsers())
	case http.MethodPost:
		if !s.requireUserManage(w, r) {
			return
		}
		var req usersRequest
		if !s.decodeJSON(w, r, &req) {
			return
		}
		if req.Password == nil {
			http.Error(w, "400 Bad Request: 新建用户必须提供密码", http.StatusBadRequest)
			return
		}
		if err := s.auth.AddUser(req.Name, *req.Password, toAuthRules(req.Rules)); err != nil {
			s.userError(w, err)
			return
		}
		s.logger.Infof("新建用户 %q，规则 %s", req.Name, describeRules(req.Rules))
		s.writeJSON(w, s.buildUsers())
	case http.MethodPut:
		if !s.requireUserManage(w, r) {
			return
		}
		var req usersRequest
		if !s.decodeJSON(w, r, &req) {
			return
		}
		pw := ""
		if req.Password != nil {
			pw = *req.Password
		}
		if err := s.auth.UpdateUser(req.Name, pw, toAuthRules(req.Rules)); err != nil {
			s.userError(w, err)
			return
		}
		note := ""
		if pw != "" {
			note = "，并重置了密码"
		}
		s.logger.Infof("修改用户 %q%s，规则 %s", req.Name, note, describeRules(req.Rules))
		s.writeJSON(w, s.buildUsers())
	case http.MethodDelete:
		if !s.requireUserManage(w, r) {
			return
		}
		name := strings.TrimSpace(r.URL.Query().Get("name"))
		if name == "" {
			http.Error(w, "400 Bad Request: 缺少 name 参数", http.StatusBadRequest)
			return
		}
		if err := s.auth.DeleteUser(name); err != nil {
			s.userError(w, err)
			return
		}
		s.logger.Infof("删除用户 %q", name)
		s.writeJSON(w, s.buildUsers())
	default:
		w.Header().Set("Allow", "GET, HEAD, POST, PUT, DELETE")
		http.Error(w, "405 Method Not Allowed", http.StatusMethodNotAllowed)
	}
}

// requireUserManage 要求「功能已开放 + 请求者是管理员」。
func (s *Server) requireUserManage(w http.ResponseWriter, r *http.Request) bool {
	if !s.cfg.AllowUserManage {
		http.Error(w, "403 Forbidden: 服务已通过 --no-user-manage 禁止在页面上管理用户",
			http.StatusForbidden)
		return false
	}
	if !s.auth.Enabled() {
		// 没有启用鉴权时，创建第一个用户的副作用是**立刻要求所有人登录** ——
		// 包括正在操作的这个「匿名管理员」，他会马上把自己关在门外。
		// 所以这条路必须由命令行开启：先用 -a 配一个账号，再来页面加其他人。
		http.Error(w, "400 Bad Request: 服务未启用鉴权，请先用 -a 'user:pass@/' 配置一个账号"+
			"（启用鉴权后才能在页面上创建其他用户）", http.StatusBadRequest)
		return false
	}
	return s.requireAdminOr(w, r, "管理用户")
}

// buildUsers 组装用户列表。调用方必须已经确认过「请求者是管理员」。
func (s *Server) buildUsers() usersReply {
	rep := usersReply{
		UserFile:  s.auth.UserFile(),
		Editable:  true,
		AuthOn:    s.auth.Enabled(),
		Anonymous: toRuleJSON(s.auth.AnonymousRules()),
	}
	for _, u := range s.auth.Users() {
		item := userJSON{
			Name:        u.Name,
			Rules:       toRuleJSON(u.Rules),
			FromStartup: u.FromStartup,
		}
		if !u.CreatedAt.IsZero() {
			item.CreatedAt = u.CreatedAt.Format(time.RFC3339)
		}
		if !u.UpdatedAt.IsZero() {
			item.UpdatedAt = u.UpdatedAt.Format(time.RFC3339)
		}
		rep.Users = append(rep.Users, item)
	}
	return rep
}

// decodeJSON 解析请求体，出错时写出 4xx 并返回 false。
func (s *Server) decodeJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(io.LimitReader(r.Body, 64<<10))
	if err := dec.Decode(v); err != nil {
		if errors.Is(err, io.EOF) {
			http.Error(w, "400 Bad Request: 请求体为空", http.StatusBadRequest)
			return false
		}
		http.Error(w, "400 Bad Request: 请求体不是合法 JSON", http.StatusBadRequest)
		return false
	}
	return true
}

// userError 把 auth 包的业务错误映射成 HTTP 状态码。
//
// 「已存在 / 不存在 / 来自启动参数 / 最后一个管理员」这些都是**可与用户沟通的
// 冲突**，用 400 + 明确文案比 500 合适得多 —— 界面上会把这段文字直接显示出来。
func (s *Server) userError(w http.ResponseWriter, err error) {
	http.Error(w, "400 Bad Request: "+err.Error(), http.StatusBadRequest)
}

func toAuthRules(in []userRuleJSON) []auth.Rule {
	out := make([]auth.Rule, 0, len(in))
	for _, r := range in {
		perm := auth.PermRead
		if strings.EqualFold(strings.TrimSpace(r.Perm), "rw") {
			perm = auth.PermReadWrite
		}
		out = append(out, auth.Rule{Path: r.Path, Perm: perm})
	}
	return out
}

func toRuleJSON(in []auth.Rule) []userRuleJSON {
	out := make([]userRuleJSON, 0, len(in))
	for _, r := range in {
		out = append(out, userRuleJSON{Path: r.Path, Perm: r.Perm.String()})
	}
	return out
}

func describeRules(rules []userRuleJSON) string {
	parts := make([]string, 0, len(rules))
	for _, r := range rules {
		parts = append(parts, r.Path+":"+r.Perm)
	}
	return strings.Join(parts, ",")
}
