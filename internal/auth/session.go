package auth

import (
	"crypto/rand"
	"encoding/base64"
	"time"
)

// 会话有效期。
//
// 为什么是服务端会话表，而不是签一个自包含的 token（JWT 那类）：
// 会话必须能被**立即撤销**。改密码、删账号、点登出之后，已经发出去的那张
// cookie 要当场失效 —— 自包含 token 在过期前谁也收不回来，只能等它自己到期。
// 代价是一张内存表，值得。
const (
	// sessionTTL 是会话的绝对寿命，从登录算起，不因活跃而延长。
	// 定 30 天是为了「记住我」这个场景：勾了之后不该隔几天又要重登。
	sessionTTL = 30 * 24 * time.Hour
	// sessionIdleTTL 是空闲上限：这么久没用过就作废。
	// 绝对寿命管「这把钥匙迟早要换」，空闲上限管「人走了别一直开着」。
	sessionIdleTTL = 7 * 24 * time.Hour
	// sessionMaxEntries 是会话表的上限，防止只登录不登出把它撑爆。
	// 满了先清一遍过期的；仍然满就拒绝新会话（宁可让人重登，也不无限吃内存）。
	sessionMaxEntries = 4096
	// sessionTokenBytes 是 token 的随机字节数。256 位，不可枚举。
	sessionTokenBytes = 32
)

// SessionTTL 返回会话的绝对寿命，供调用方设置 cookie 的 Max-Age。
func SessionTTL() time.Duration { return sessionTTL }

// Session 是一次登录产生的服务端会话。
type Session struct {
	User     string
	Created  time.Time
	LastSeen time.Time
}

// expired 判断会话是否已超出绝对寿命或空闲上限。
func (s *Session) expired(now time.Time) bool {
	return now.Sub(s.Created) > sessionTTL || now.Sub(s.LastSeen) > sessionIdleTTL
}

// newSessionLocked 为 user 新建一个会话，返回写入 cookie 的 token。
//
// 会话表满且清理不掉时返回 ok=false —— 调用方应当据此拒绝这次登录
// （能登录但拿不到会话，比明确失败更让人困惑）。
// 调用方需持有锁，故以此为名。
func (a *Authenticator) newSessionLocked(user string) (string, bool) {
	now := time.Now()
	// 惰性清理：只在表满时才扫一遍。为一个短生命周期对象常驻 goroutine
	// 不划算，而「满了才清」足以保证它不会无限增长。
	if len(a.sessions) >= sessionMaxEntries {
		a.pruneSessions(now)
		if len(a.sessions) >= sessionMaxEntries {
			return "", false
		}
	}
	token, err := newSessionToken()
	if err != nil {
		return "", false
	}
	a.sessions[token] = &Session{User: user, Created: now, LastSeen: now}
	return token, true
}

// newSessionToken 生成一个不可预测的 token。
func newSessionToken() (string, error) {
	b := make([]byte, sessionTokenBytes)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	// URL 安全编码：token 会出现在 cookie 值里，不能含 ; 或 , 这类分隔符。
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// pruneSessions 清掉过期的会话。调用方需持有锁。
func (a *Authenticator) pruneSessions(now time.Time) {
	for token, s := range a.sessions {
		if s.expired(now) {
			delete(a.sessions, token)
		}
	}
}

// NewSession 新建一个会话（并发安全）。
func (a *Authenticator) NewSession(user string) (string, bool) {
	if a == nil || user == "" {
		return "", false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.newSessionLocked(user)
}

// ResolveSession 用 token 换回用户名。
//
// 除了有效期，还会确认**该账号现在仍然存在** —— 删号之后那张 cookie
// 不该还能用。改密码走的是 DropUserSessions（见 user.go），
// 那条路径在改动的当下就把会话断了，所以这里不必再比密码。
func (a *Authenticator) ResolveSession(token string) (string, bool) {
	if a == nil || token == "" {
		return "", false
	}
	a.mu.Lock()
	defer a.mu.Unlock()

	s, ok := a.sessions[token]
	if !ok {
		return "", false
	}
	now := time.Now()
	if s.expired(now) {
		delete(a.sessions, token)
		return "", false
	}
	if a.lookupAccount(s.User) == nil {
		// 账号没了（被删掉或改了名），会话当场作废。
		delete(a.sessions, token)
		return "", false
	}
	// 滑动续期：只有活跃才需要更新，避免每个请求都写一次时间。
	if now.Sub(s.LastSeen) > time.Minute {
		s.LastSeen = now
	}
	return s.User, true
}

// DropSession 撤销单个会话（登出）。
func (a *Authenticator) DropSession(token string) {
	if a == nil || token == "" {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.sessions, token)
}

// DropUserSessions 撤销某个账号的全部会话。
//
// 改密码与删账号时必须调用：清空 pwCache 只能让**旧密码**不再通过校验，
// 已经发出去的 cookie 仍然能读到东西 —— 那正是「改了密码怎么还被登着」。
// 调用方需持有锁。
func (a *Authenticator) dropUserSessions(user string) {
	for token, s := range a.sessions {
		if s.User == user {
			delete(a.sessions, token)
		}
	}
}

// SessionCount 返回当前会话数（测试与诊断用）。
func (a *Authenticator) SessionCount() int {
	if a == nil {
		return 0
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	return len(a.sessions)
}

// PermFor 按**身份**计算对 urlPath 的权限，不校验密码。
//
// 与 Lookup 的区别：Lookup 在 authenticated=true 时仍会重新校验一遍密码
// （那是给每个请求都带 Basic 头的场景用的）。会话 cookie 里只有身份、
// 没有密码，所以另开这一条路径 —— 密码在登录那一刻已经验过了。
//
// 安全性由会话本身保证：token 不可枚举、有有效期、可撤销。
func (a *Authenticator) PermFor(user, urlPath string) Permission {
	if !a.Enabled() {
		return PermReadWrite
	}
	a.mu.RLock()
	defer a.mu.RUnlock()

	acc := a.lookupAccount(user)
	if acc == nil {
		return PermNone
	}
	return acc.match(urlPath)
}
