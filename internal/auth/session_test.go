package auth

import (
	"strings"
	"sync"
	"testing"
	"time"
)

// newSessionAuth 起一个带会话能力的鉴权器（账号来自启动参数）。
func newSessionAuth(t *testing.T) *Authenticator {
	t.Helper()
	a, err := Open([]string{"admin:secret@/:rw", "bob:bobpass@/docs:r"}, tmpUserFile(t))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return a
}

// TestSessionRoundTrip 覆盖最基本的一条：建了能换回来。
func TestSessionRoundTrip(t *testing.T) {
	a := newSessionAuth(t)
	token, ok := a.NewSession("admin")
	if !ok {
		t.Fatal("NewSession 失败")
	}
	if token == "" {
		t.Fatal("token 为空")
	}
	user, ok := a.ResolveSession(token)
	if !ok || user != "admin" {
		t.Fatalf("ResolveSession = (%q, %v)，期望 (\"admin\", true)", user, ok)
	}
}

// TestSessionTokenUnpredictable 确认 token 够长且每次不同。
//
// 这是整套 cookie 鉴权的根基：token 就是通行证，可枚举等于谁都能进。
func TestSessionTokenUnpredictable(t *testing.T) {
	a := newSessionAuth(t)
	const n = 200
	seen := make(map[string]bool, n)
	for i := 0; i < n; i++ {
		token, ok := a.NewSession("admin")
		if !ok {
			t.Fatal("NewSession 失败")
		}
		if seen[token] {
			t.Fatalf("第 %d 次生成的 token 与之前重复", i)
		}
		seen[token] = true
		// 32 字节 base64url 无填充 = 43 个字符。够短于此说明熵不足。
		if len(token) < 40 {
			t.Fatalf("token 只有 %d 字符，太短：%q", len(token), token)
		}
		// 不能含 cookie 值里的分隔符。
		if strings.ContainsAny(token, ";,\"\\ ") {
			t.Fatalf("token 含非法字符：%q", token)
		}
	}
}

// TestSessionInvalidToken 确认乱编的 token 换不到身份。
func TestSessionInvalidToken(t *testing.T) {
	a := newSessionAuth(t)
	for _, bad := range []string{"", "guess", strings.Repeat("a", 43)} {
		if user, ok := a.ResolveSession(bad); ok {
			t.Fatalf("ResolveSession(%q) 竟然成功，返回 %q", bad, user)
		}
	}
}

// TestDropSession 确认登出之后 token 立即失效。
func TestDropSession(t *testing.T) {
	a := newSessionAuth(t)
	token, _ := a.NewSession("admin")
	a.DropSession(token)
	if _, ok := a.ResolveSession(token); ok {
		t.Fatal("DropSession 之后 token 仍然可用")
	}
}

// TestChangingPasswordKillsSessions 是这个功能最容易漏的一条：
// 改密码之后，已经发出去的 cookie 必须当场失效。
//
// 只清 pwCache 是不够的 —— 它管的是「旧密码还能不能通过校验」，
// 而 cookie 里根本没有密码。漏了这条的症状就是「我都改密码了，怎么还被登着」。
func TestChangingPasswordKillsSessions(t *testing.T) {
	a := newSessionAuth(t)
	// 用用户表里的账号：启动参数来的账号改不了口令（要改命令行）。
	if err := a.AddUser("dave", "davepass", []Rule{{Path: "/", Perm: PermReadWrite}}); err != nil {
		t.Fatalf("建用户: %v", err)
	}
	token, ok := a.NewSession("dave")
	if !ok {
		t.Fatal("NewSession 失败")
	}
	if _, ok := a.ResolveSession(token); !ok {
		t.Fatal("前置条件不成立：会话应当有效")
	}

	if err := a.UpdateUser("dave", "brand-new-pass", []Rule{{Path: "/", Perm: PermReadWrite}}); err != nil {
		t.Fatalf("改密码: %v", err)
	}
	if _, ok := a.ResolveSession(token); ok {
		t.Fatal("改密码之后旧会话仍然有效 —— 这正是要防的")
	}
	// 顺便确认新密码确实生效了，否则这条测试可能因为改密码本身失败而「通过」。
	if !a.Verify("dave", "brand-new-pass") {
		t.Fatal("新密码登不上，说明改密码本身就没成功")
	}
}

// TestDeletingUserKillsSessions 确认删号之后旧会话失效。
func TestDeletingUserKillsSessions(t *testing.T) {
	b := newSessionAuth(t)
	if err := b.AddUser("carol", "carolpass", []Rule{{Path: "/", Perm: PermReadWrite}}); err != nil {
		t.Fatalf("建用户: %v", err)
	}
	token, ok := b.NewSession("carol")
	if !ok {
		t.Fatal("NewSession 失败")
	}
	if err := b.DeleteUser("carol"); err != nil {
		t.Fatalf("删用户: %v", err)
	}
	if _, ok := b.ResolveSession(token); ok {
		t.Fatal("删号之后旧会话仍然有效")
	}
}

// TestSessionForVanishedUser 兜住「绕过 DropUserSessions 把账号弄没了」的情况：
// ResolveSession 自己也要确认账号还在。
func TestSessionForVanishedUser(t *testing.T) {
	a := newSessionAuth(t)
	// 直接塞一个不存在账号的会话，模拟账号在别处被移除。
	token, ok := a.NewSession("ghost")
	if !ok {
		t.Fatal("NewSession 失败")
	}
	if _, ok := a.ResolveSession(token); ok {
		t.Fatal("不存在账号的会话不该解析成功")
	}
	// 且应当顺手把它清掉。
	if n := a.SessionCount(); n != 0 {
		t.Fatalf("失效会话没有被清理，还剩 %d 个", n)
	}
}

// TestPermForUsesIdentityWithoutPassword 是会话鉴权的核心语义：
// 按身份给权限，但**不验密码** —— cookie 里本来就没有密码。
func TestPermForUsesIdentityWithoutPassword(t *testing.T) {
	a := newSessionAuth(t)
	cases := []struct {
		user, path string
		want       Permission
	}{
		{"admin", "/", PermReadWrite},
		{"admin", "/any/where", PermReadWrite},
		{"bob", "/docs", PermRead},
		{"bob", "/docs/deep/file.txt", PermRead},
		{"bob", "/elsewhere", PermNone},
		{"nobody", "/", PermNone},
	}
	for _, tc := range cases {
		if got := a.PermFor(tc.user, tc.path); got != tc.want {
			t.Errorf("PermFor(%q, %q) = %v，期望 %v", tc.user, tc.path, got, tc.want)
		}
	}
}

// TestPermForDisabledAuth 确认没开鉴权时一律放行。
func TestPermForDisabledAuth(t *testing.T) {
	a, err := Open(nil, "")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if a != nil {
		t.Fatal("没有规则时 Open 应当返回 nil")
	}
	// nil 接收者也要能安全调用。
	if got := a.PermFor("anyone", "/"); got != PermReadWrite {
		t.Fatalf("鉴权关闭时 PermFor = %v，期望 PermReadWrite", got)
	}
}

// TestSessionExpiry 确认过期会话不再可用。
//
// 直接改 LastSeen/Created 而不是 sleep —— 30 天的绝对寿命没法等。
func TestSessionExpiry(t *testing.T) {
	a := newSessionAuth(t)

	t.Run("空闲超时", func(t *testing.T) {
		token, _ := a.NewSession("admin")
		a.mu.Lock()
		a.sessions[token].LastSeen = time.Now().Add(-sessionIdleTTL - time.Minute)
		a.mu.Unlock()
		if _, ok := a.ResolveSession(token); ok {
			t.Fatal("空闲超时的会话仍然有效")
		}
	})

	t.Run("绝对寿命", func(t *testing.T) {
		token, _ := a.NewSession("admin")
		a.mu.Lock()
		a.sessions[token].Created = time.Now().Add(-sessionTTL - time.Minute)
		a.mu.Unlock()
		if _, ok := a.ResolveSession(token); ok {
			t.Fatal("超过绝对寿命的会话仍然有效")
		}
	})
}

// TestSessionTableBounded 确认会话表有上限：只登录不登出也撑不爆它。
//
// 语义是「满了先清过期的；仍然满就拒绝新会话」—— 宁可让人重登，
// 也不无限吃内存。这里所有会话都是新鲜的，清不掉，所以到顶就该拒绝。
func TestSessionTableBounded(t *testing.T) {
	a := newSessionAuth(t)
	for i := 0; i < sessionMaxEntries; i++ {
		if _, ok := a.NewSession("admin"); !ok {
			t.Fatalf("第 %d 次就建不出会话了，离上限还早", i)
		}
	}
	if n := a.SessionCount(); n != sessionMaxEntries {
		t.Fatalf("会话数 %d，期望正好到上限 %d", n, sessionMaxEntries)
	}
	// 顶上再要一个：应当被拒绝，而不是把表撑大。
	if _, ok := a.NewSession("admin"); ok {
		t.Fatal("会话表已满且无过期项，仍然发出了新会话")
	}
	if n := a.SessionCount(); n > sessionMaxEntries {
		t.Fatalf("会话数 %d 超过了上限 %d", n, sessionMaxEntries)
	}

	// 但如果其中有过了期的，清理后应当能腾出位置。
	a.mu.Lock()
	for token := range a.sessions {
		a.sessions[token].LastSeen = time.Now().Add(-sessionIdleTTL - time.Minute)
		break
	}
	a.mu.Unlock()
	if _, ok := a.NewSession("admin"); !ok {
		t.Fatal("有过期会话却仍然拒绝新会话 —— 惰性清理没生效")
	}
}

// TestSessionConcurrent 确认并发下没有数据竞争（配合 -race 跑）。
func TestSessionConcurrent(t *testing.T) {
	a := newSessionAuth(t)
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 40; j++ {
				token, ok := a.NewSession("admin")
				if !ok {
					continue
				}
				a.ResolveSession(token)
				a.PermFor("bob", "/docs")
				a.DropSession(token)
			}
		}()
	}
	wg.Wait()
}
