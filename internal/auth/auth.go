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
//
// 账号有两个来源，合并后一起参与匹配：
//   - **启动参数**（-a / GOFS_AUTH）：启动后不可改，界面上标记为「来自启动参数」；
//   - **用户表**（users.json，见 user.go）：页面上可增删改，密码只存 PBKDF2 摘要。
//
// 所有查询方法都是并发安全的（改动只发生在管理接口里，用写锁串行化）。
package auth

import (
	"crypto/subtle"
	"fmt"
	"sort"
	"strings"
	"sync"
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

// String 返回权限的可读形式（也是 API / 界面里用的取值）。
func (p Permission) String() string {
	switch p {
	case PermReadWrite:
		return "rw"
	case PermRead:
		return "r"
	default:
		return "none"
	}
}

// pathRule 为单条路径规则。
type pathRule struct {
	Path string
	Perm Permission
}

// account 为单个账号（或匿名）的规则集合。
//
// 密码有两种形态，二选一：
//   - password：明文，只可能来自启动参数（用户自己写在命令行里，加密存储没有意义）；
//   - hash：PBKDF2 摘要，来自页面上创建的用户。
type account struct {
	Username string
	password string
	hash     *passwordHash
	Rules    []pathRule
	anon     bool

	// fromStartup 标记账号来自 -a / GOFS_AUTH，界面上只读。
	fromStartup bool
	createdAt   int64 // Unix 秒；启动参数来的账号为 0
	updatedAt   int64
}

// Authenticator 保存解析后的全部鉴权规则。
type Authenticator struct {
	// mu 保护下面所有字段。管理接口会改写它们，而每个请求都要读。
	mu      sync.RWMutex
	users   []*account // 来自用户表的账号（可改）
	anonAcc *account   // 匿名规则（只有启动参数能定义）
	static  []*account // 来自启动参数的账号（只读）
	enabled bool

	userFile string   // 用户表落盘路径，空串表示仅内存
	pwCache  *pwCache // 「账号+密码」校验结果的缓存，见 pwCache 的注释
}

// Parse 解析全部鉴权规则串。
func Parse(rules []string) (*Authenticator, error) {
	return Open(rules, "")
}

// Open 解析启动参数里的规则，并加载用户表。
//
// userFile 为空时用户表只存在于内存中（重启即失效），
// 与上传密钥的 --key-file 语义保持一致。
func Open(rules []string, userFile string) (*Authenticator, error) {
	a := &Authenticator{userFile: userFile, pwCache: newPWCache()}
	for _, raw := range rules {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		acc, err := parseRule(raw)
		if err != nil {
			return nil, err
		}
		acc.fromStartup = true
		if acc.anon {
			if a.anonAcc != nil {
				a.anonAcc.Rules = mergeRules(a.anonAcc.Rules, acc.Rules)
				continue
			}
			a.anonAcc = acc
			continue
		}
		if err := a.mergeStatic(acc); err != nil {
			return nil, err
		}
	}

	if userFile != "" {
		users, err := loadUsers(userFile)
		if err != nil {
			return nil, err
		}
		for _, u := range users {
			acc, err := u.toAccount()
			if err != nil {
				return nil, fmt.Errorf("用户表 %s 中的账号 %q 无效: %w", userFile, u.Name, err)
			}
			// 与启动参数重名时**以启动参数为准**并跳过，而不是启动失败：
			// 用户表的文件可能比命令行先存在，谁对谁错很难说，但
			// 「命令行是权威」这条规则简单且不会把服务卡死。
			if a.findStatic(acc.Username) != nil {
				continue
			}
			a.users = append(a.users, acc)
		}
	}

	a.refreshEnabled()
	if !a.enabled {
		return nil, nil
	}
	return a, nil
}

// refreshEnabled 重新计算「是否有任何鉴权规则」。调用方需持有写锁。
func (a *Authenticator) refreshEnabled() {
	a.enabled = a.anonAcc != nil || len(a.static) > 0 || len(a.users) > 0
}

// findStatic 按用户名查启动参数里的账号。调用方需持有锁。
func (a *Authenticator) findStatic(name string) *account {
	for _, acc := range a.static {
		if acc.Username == name {
			return acc
		}
	}
	return nil
}

// findUser 按用户名查用户表里的账号。调用方需持有锁。
func (a *Authenticator) findUser(name string) *account {
	for _, acc := range a.users {
		if acc.Username == name {
			return acc
		}
	}
	return nil
}

// mergeRules 合并两组路径规则，保持「长的在前」，便于最长前缀匹配。
func mergeRules(dst, src []pathRule) []pathRule {
	out := append(append([]pathRule{}, dst...), src...)
	sort.SliceStable(out, func(i, j int) bool { return len(out[i].Path) > len(out[j].Path) })
	return out
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
			acc.password = cred[i+1:]
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
		case strings.HasSuffix(item, ":ro"), strings.HasSuffix(item, ":r"):
			// :ro 是 dufs 的写法；:r 是页面上「只读」的取值，一并接受 ——
			// 否则 `bob:pw@/docs:r` 会被当成一个叫「/docs:r」的目录，
			// bob 就莫名其妙什么权限都没有了（这种静默失败很难排查）。
			perm = PermRead
			if strings.HasSuffix(item, ":ro") {
				item = strings.TrimSuffix(item, ":ro")
			} else {
				item = strings.TrimSuffix(item, ":r")
			}
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

// mergeStatic 把新账号并入启动参数集合；同名账号的规则会累加。
func (a *Authenticator) mergeStatic(in *account) error {
	if exist := a.findStatic(in.Username); exist != nil {
		if exist.password != in.password {
			return fmt.Errorf("账号 %q 出现了不一致的密码", in.Username)
		}
		exist.Rules = mergeRules(exist.Rules, in.Rules)
		return nil
	}
	a.static = append(a.static, in)
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
func (a *Authenticator) Enabled() bool {
	if a == nil {
		return false
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.enabled
}

// Verify 判断一组凭据是否对应某个已知的具名账号。
//
// 与 Lookup 的区别：Verify 只回答「这对账号密码是否有效」，
// 不关心该账号对哪个路径有权限。登录框需要的是前者——
// 若用 Lookup("/") 校验，一个只被授予 /docs 权限的账号会因为对根目录无权限而登录失败。
func (a *Authenticator) Verify(username, password string) bool {
	if !a.Enabled() {
		return true
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	if acc := a.lookupAccount(username); acc != nil {
		return a.passwordMatches(acc, password)
	}
	return false
}

// lookupAccount 按用户名找账号（先用户表再启动参数，重名以启动参数为准）。
// 调用方需持有锁。
func (a *Authenticator) lookupAccount(username string) *account {
	if username == "" {
		return nil
	}
	if acc := a.findUser(username); acc != nil {
		if a.findStatic(username) == nil {
			return acc
		}
	}
	return a.findStatic(username)
}

// subtleEqual 做定长时间比较，避免通过响应耗时逐字节猜测密码。
func subtleEqual(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

// passwordMatches 校验密码。明文账号直接定长比较；用户表的账号走 PBKDF2。
//
// 这里带一层结果缓存：PBKDF2 是按「故意很慢」设计的，而 HTTP Basic 意味着
// **每个请求都要验一次密码**，真按 21 万轮算的话每个请求要多花约 100ms。
// 缓存键是「用户名 + 密码」的摘要，命中就完全跳过计算；
// 任何一次用户表改动都会清空缓存（见 invalidate()）。
func (a *Authenticator) passwordMatches(acc *account, password string) bool {
	if acc.hash == nil {
		return subtleEqual(acc.password, password)
	}
	key := pwCacheKey(acc.Username, password)
	if ok, hit := a.pwCache.get(key); hit {
		return ok
	}
	ok := acc.hash.verify(password)
	a.pwCache.put(key, ok)
	return ok
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
	a.mu.RLock()
	defer a.mu.RUnlock()

	var acc *account
	if authenticated {
		acc = a.lookupAccount(username)
		if acc != nil && !a.passwordMatches(acc, password) {
			return false
		}
	} else {
		acc = a.anonAcc
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
	a.mu.RLock()
	defer a.mu.RUnlock()

	// 先尝试具名账号，再回落到匿名规则。
	if authenticated {
		acc := a.lookupAccount(username)
		if acc == nil {
			return PermNone
		}
		if !a.passwordMatches(acc, password) {
			return PermNone
		}
		return acc.match(urlPath)
	}
	if a.anonAcc != nil {
		return a.anonAcc.match(urlPath)
	}
	return PermNone
}

// NeedsAuth 判断该路径是否要求客户端提供凭据（用于决定是否回 401 挑战）。
func (a *Authenticator) NeedsAuth(urlPath string) bool {
	if !a.Enabled() {
		return false
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	if a.anonAcc != nil && a.anonAcc.match(urlPath) != PermNone {
		return false
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
