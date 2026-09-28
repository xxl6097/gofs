// Package auth 实现基于路径的账号权限控制。
//
// 规则语法参考 dufs：
//
//	user:pass@/dir1:rw,/dir2      user 在 /dir1 下可读写，在 /dir2 下只读
//	guest:guest@/                 匿名外的 guest 账号对整个根目录只读
//	@/                            整个根目录匿名可见（只读）
//
// 说明：
//   - 以 @ 分隔「账号」与「路径列表」；账号缺省表示匿名用户。
//   - 账号内以 : 分隔用户名与密码；密码缺省表示空密码。
//   - 路径后缀 :rw 表示可读写，:ro 或省略表示只读。
//   - 多条规则按「最长前缀优先」匹配，未命中任何规则即拒绝。
//
// 鉴权只是权限上限，最终仍受全局 --allow-* 开关约束。
package auth

import (
	"crypto/subtle"
	"fmt"
	"sort"
	"strings"
)

// Permission 表示对某个路径的授权级别。
type Permission int

const (
	// PermNone 未授权。
	PermNone Permission = iota
	// PermRead 只读。
	PermRead
	// PermReadWrite 可读写。
	PermReadWrite
)

// pathRule 为单条路径规则。
type pathRule struct {
	Path string
	Perm Permission
}

// account 为单个账号（或匿名）的规则集合。
type account struct {
	Username string
	Password string
	Rules    []pathRule
	anon     bool
}

// Authenticator 保存解析后的全部鉴权规则。
type Authenticator struct {
	accounts []*account
	enabled  bool
}

// Parse 解析全部鉴权规则串。
func Parse(rules []string) (*Authenticator, error) {
	a := &Authenticator{}
	for _, raw := range rules {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		acc, err := parseRule(raw)
		if err != nil {
			return nil, err
		}
		if err := a.merge(acc); err != nil {
			return nil, err
		}
	}
	a.enabled = len(a.accounts) > 0
	if !a.enabled {
		return nil, nil
	}
	return a, nil
}

// parseRule 解析单条规则串。
func parseRule(raw string) (*account, error) {
	at := strings.Index(raw, "@")
	if at < 0 {
		return nil, fmt.Errorf("鉴权规则 %q 缺少 @ 分隔符，正确形式如 user:pass@/dir:rw", raw)
	}
	cred := raw[:at]
	pathSpec := raw[at+1:]

	acc := &account{}
	if cred == "" {
		acc.anon = true
	} else {
		if i := strings.Index(cred, ":"); i >= 0 {
			acc.Username = cred[:i]
			acc.Password = cred[i+1:]
		} else {
			acc.Username = cred
		}
		if acc.Username == "" {
			return nil, fmt.Errorf("鉴权规则 %q 的用户名为空", raw)
		}
	}

	if strings.TrimSpace(pathSpec) == "" {
		pathSpec = "/"
	}
	for _, item := range strings.Split(pathSpec, ",") {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		perm := PermRead
		switch {
		case strings.HasSuffix(item, ":rw"):
			perm = PermReadWrite
			item = strings.TrimSuffix(item, ":rw")
		case strings.HasSuffix(item, ":ro"):
			perm = PermRead
			item = strings.TrimSuffix(item, ":ro")
		}
		p := normalizePath(item)
		acc.Rules = append(acc.Rules, pathRule{Path: p, Perm: perm})
	}
	if len(acc.Rules) == 0 {
		return nil, fmt.Errorf("鉴权规则 %q 未包含任何有效路径", raw)
	}
	// 路径长的排前面，便于按最长前缀匹配。
	sort.SliceStable(acc.Rules, func(i, j int) bool {
		return len(acc.Rules[i].Path) > len(acc.Rules[j].Path)
	})
	return acc, nil
}

// merge 把新账号并入集合；同名账号的规则会累加。
func (a *Authenticator) merge(in *account) error {
	for _, exist := range a.accounts {
		if exist.anon == in.anon && exist.Username == in.Username {
			if !in.anon && exist.Password != in.Password {
				return fmt.Errorf("账号 %q 出现了不一致的密码", in.Username)
			}
			exist.Rules = append(exist.Rules, in.Rules...)
			sort.SliceStable(exist.Rules, func(i, j int) bool {
				return len(exist.Rules[i].Path) > len(exist.Rules[j].Path)
			})
			return nil
		}
	}
	a.accounts = append(a.accounts, in)
	return nil
}

// normalizePath 归一化规则中的路径。
func normalizePath(p string) string {
	p = strings.TrimSpace(p)
	if p == "" {
		return "/"
	}
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	if len(p) > 1 {
		p = strings.TrimSuffix(p, "/")
	}
	return p
}

// Enabled 表示是否存在任何鉴权规则。
func (a *Authenticator) Enabled() bool { return a != nil && a.enabled }

// Verify 判断一组凭据是否对应某个已知的具名账号。
//
// 与 Lookup 的区别：Verify 只回答「这对账号密码是否有效」，
// 不关心该账号对哪个路径有权限。登录框需要的是前者——
// 若用 Lookup("/") 校验，一个只被授予 /docs 权限的账号会因为对根目录无权限而登录失败。
func (a *Authenticator) Verify(username, password string) bool {
	if !a.Enabled() {
		return true
	}
	for _, acc := range a.accounts {
		if acc.anon || acc.Username != username {
			continue
		}
		return subtleEqual(acc.Password, password)
	}
	return false
}

// subtleEqual 做定长时间比较，避免通过响应耗时逐字节猜测密码。
func subtleEqual(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

// HasAnyWrite 判断该账号是否在**任意**一条规则的路径上具备写权限。
//
// 用于与具体路径无关的资源（例如上传密钥管理）：这类接口不该要求
// 调用者对根目录有权限——一个只被授予 /docs 的账号理应能签发
// 局限在 /docs 内的密钥。具体能签发多大范围的密钥，再由调用者
// 对该范围的 checkPerm 结果决定。
func (a *Authenticator) HasAnyWrite(username, password string, authenticated bool) bool {
	if !a.Enabled() {
		return true
	}
	var acc *account
	if authenticated {
		for _, x := range a.accounts {
			if x.anon || x.Username != username {
				continue
			}
			if !subtleEqual(x.Password, password) {
				return false
			}
			acc = x
			break
		}
	} else {
		for _, x := range a.accounts {
			if x.anon {
				acc = x
				break
			}
		}
	}
	if acc == nil {
		return false
	}
	for _, r := range acc.Rules {
		if r.Perm == PermReadWrite {
			return true
		}
	}
	return false
}

// Lookup 在提供的凭据下计算 urlPath 的权限。
// 若未配置鉴权，返回 PermReadWrite（由全局开关再收敛）。
func (a *Authenticator) Lookup(urlPath, username, password string, authenticated bool) Permission {
	if !a.Enabled() {
		return PermReadWrite
	}
	// 先尝试具名账号，再回落到匿名规则。
	if authenticated {
		for _, acc := range a.accounts {
			if acc.anon || acc.Username != username {
				continue
			}
			if acc.Password != password {
				return PermNone
			}
			return acc.match(urlPath)
		}
		return PermNone
	}
	for _, acc := range a.accounts {
		if acc.anon {
			return acc.match(urlPath)
		}
	}
	return PermNone
}

// NeedsAuth 判断该路径是否要求客户端提供凭据（用于决定是否回 401 挑战）。
func (a *Authenticator) NeedsAuth(urlPath string) bool {
	if !a.Enabled() {
		return false
	}
	for _, acc := range a.accounts {
		if acc.anon && acc.match(urlPath) != PermNone {
			return false
		}
	}
	return true
}

// match 按最长前缀匹配路径规则。
func (acc *account) match(urlPath string) Permission {
	urlPath = normalizePath(urlPath)
	for _, r := range acc.Rules {
		if r.Path == "/" {
			return r.Perm
		}
		if urlPath == r.Path || strings.HasPrefix(urlPath, r.Path+"/") {
			return r.Perm
		}
	}
	return PermNone
}
