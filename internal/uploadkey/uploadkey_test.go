package uploadkey

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func newStore(t *testing.T, path string) *Store {
	t.Helper()
	s, err := New(path)
	if err != nil {
		t.Fatalf("创建存储失败: %v", err)
	}
	return s
}

func TestCreateAndVerify(t *testing.T) {
	s := newStore(t, "")

	k, token, err := s.Create(CreateOptions{Name: "备份脚本", Scope: "/uploads", TTL: time.Hour, Creator: "admin"})
	if err != nil {
		t.Fatalf("创建失败: %v", err)
	}
	if !strings.HasPrefix(token, TokenPrefix) {
		t.Errorf("明文缺少前缀: %q", token)
	}
	if k.Name != "备份脚本" || k.Creator != "admin" || k.Scope != "/uploads" {
		t.Errorf("字段不正确: %+v", k)
	}
	// 明文绝不能出现在记录里——只保存摘要。
	raw, _ := json.Marshal(k)
	if strings.Contains(string(raw), strings.TrimPrefix(token, TokenPrefix)) {
		t.Errorf("记录中泄露了密钥明文: %s", raw)
	}
	if k.Hash == "" || k.Hash == token {
		t.Errorf("摘要不正确")
	}

	got, err := s.Verify(token)
	if err != nil {
		t.Fatalf("校验失败: %v", err)
	}
	if got.ID != k.ID {
		t.Errorf("校验返回了错误的记录")
	}
	if got.UseCount != 1 {
		t.Errorf("使用次数 = %d, 期望 1", got.UseCount)
	}
	if got.LastUsedAt.IsZero() {
		t.Errorf("最后使用时间未记录")
	}

	// 再校验一次，次数累加
	if _, err := s.Verify(token); err != nil {
		t.Fatalf("二次校验失败: %v", err)
	}
	if k2, _ := s.Get(k.ID); k2.UseCount != 2 {
		t.Errorf("使用次数 = %d, 期望 2", k2.UseCount)
	}
}

func TestVerifyRejects(t *testing.T) {
	s := newStore(t, "")
	_, token, _ := s.Create(CreateOptions{Name: "k"})

	if _, err := s.Verify(""); err != ErrEmptyToken {
		t.Errorf("空密钥应返回 ErrEmptyToken，实际 %v", err)
	}
	if _, err := s.Verify("gofs_notavalidtoken"); err != ErrNotFound {
		t.Errorf("伪造密钥应返回 ErrNotFound，实际 %v", err)
	}
	// 改一个字符也应失败。
	//
	// ⚠️ 必须换成一个**确定为不同**的字符。写死成 "X" 的话，
	// token 末位本身是随机 base64url，有 1/64 的概率本来就是 'X' ——
	// 那样 tampered 与 token 完全相同，校验当然通过，
	// 测试就会无缘无故地红一次（约 1.6% 的运行）。
	last := token[len(token)-1]
	repl := byte('X')
	if last == repl {
		repl = 'Y'
	}
	tampered := token[:len(token)-1] + string(repl)
	if tampered == token {
		t.Fatal("构造出的「篡改版」和原 token 一样，这条测试没测到东西")
	}
	if _, err := s.Verify(tampered); err == nil {
		t.Errorf("被篡改的密钥不应通过校验")
	}
}

func TestExpiry(t *testing.T) {
	s := newStore(t, "")

	k, token, err := s.Create(CreateOptions{Name: "短期", TTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	if k.Permanent() {
		t.Errorf("设置了 TTL 就不应标记为永不过期")
	}
	if k.Expired(time.Now()) {
		t.Errorf("刚创建的密钥不应已过期")
	}
	if got := k.Remaining(time.Now()); got <= 0 || got > time.Hour {
		t.Errorf("剩余时长异常: %v", got)
	}
	// 过期后校验必须失败
	if !k.Expired(time.Now().Add(2 * time.Hour)) {
		t.Errorf("两小时后应判定为过期")
	}
	if got := k.Status(time.Now().Add(2 * time.Hour)); got != "expired" {
		t.Errorf("状态 = %q", got)
	}
	if _, err := s.Verify(token); err != nil {
		t.Errorf("未过期时应当可用: %v", err)
	}

	// 永不过期
	k2, _, _ := s.Create(CreateOptions{Name: "永久", TTL: 0})
	if !k2.Permanent() {
		t.Errorf("TTL=0 应为永不过期")
	}
	if k2.Expired(time.Now().Add(100 * 365 * 24 * time.Hour)) {
		t.Errorf("永不过期的密钥不应过期")
	}
	if got := k2.Remaining(time.Now()); got != 0 {
		t.Errorf("永不过期的剩余时长应为 0，实际 %v", got)
	}
}

func TestAllowsPath(t *testing.T) {
	cases := []struct {
		scope string
		path  string
		want  bool
	}{
		{"/", "/a.txt", true},
		{"/", "/deep/dir/a.txt", true},
		{"", "/a.txt", true},
		{"/uploads", "/uploads/a.txt", true},
		{"/uploads", "/uploads/2026/09/28/a.txt", true},
		{"/uploads", "/uploads", true},
		{"/uploads", "/upload/a.txt", false}, // 前缀相近但不同目录
		{"/uploads", "/etc/passwd", false},
		{"/uploads", "//uploads/a.txt", true},    // 重复斜杠应被归一化
		{"/uploads", "/uploads/../etc/x", false}, // .. 折叠后越界
		{"/a/b", "/a/b/c.txt", true},
		{"/a/b", "/a/bc/c.txt", false},
	}
	for _, c := range cases {
		k := &Key{Scope: c.scope}
		if got := k.AllowsPath(c.path); got != c.want {
			t.Errorf("scope=%q path=%q → %v, 期望 %v", c.scope, c.path, got, c.want)
		}
	}
}

func TestRevoke(t *testing.T) {
	s := newStore(t, "")
	k, token, _ := s.Create(CreateOptions{Name: "临时"})

	if err := s.Revoke(k.ID); err != nil {
		t.Fatalf("撤销失败: %v", err)
	}
	if _, err := s.Verify(token); err != ErrNotFound {
		t.Errorf("撤销后应无法使用，实际 %v", err)
	}
	if err := s.Revoke(k.ID); err != ErrNotFound {
		t.Errorf("重复撤销应返回 ErrNotFound，实际 %v", err)
	}
	if s.Count() != 0 {
		t.Errorf("撤销后数量 = %d", s.Count())
	}
}

func TestPersistence(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sub", "keys.json")

	s := newStore(t, path)
	k, token, err := s.Create(CreateOptions{Name: "持久化", Scope: "/data", TTL: time.Hour, Creator: "u"})
	if err != nil {
		t.Fatalf("创建失败: %v", err)
	}
	if !s.Persistent() {
		t.Errorf("应标记为持久化")
	}

	// 文件权限必须是 0600（摘要等同敏感信息）
	st, err := os.Stat(path)
	if err != nil {
		t.Fatalf("密钥文件未生成: %v", err)
	}
	if perm := st.Mode().Perm(); perm != 0o600 {
		t.Errorf("文件权限 = %o, 期望 600", perm)
	}

	// 重新加载后仍可校验
	s2 := newStore(t, path)
	if s2.Count() != 1 {
		t.Fatalf("重载后数量 = %d", s2.Count())
	}
	got, err := s2.Verify(token)
	if err != nil {
		t.Fatalf("重载后校验失败: %v", err)
	}
	if got.ID != k.ID || got.Name != "持久化" || got.Creator != "u" || got.Scope != "/data" {
		t.Errorf("重载后字段不一致: %+v", got)
	}
}

func TestListOrderAndCopy(t *testing.T) {
	s := newStore(t, "")
	if _, _, err := s.Create(CreateOptions{Name: "第一个"}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(2 * time.Millisecond)
	if _, _, err := s.Create(CreateOptions{Name: "第二个"}); err != nil {
		t.Fatal(err)
	}

	list := s.List()
	if len(list) != 2 {
		t.Fatalf("数量 = %d", len(list))
	}
	if list[0].Name != "第二个" {
		t.Errorf("应按创建时间倒序，实际首项为 %q", list[0].Name)
	}
	// 返回的必须是副本，改动不应影响存储
	list[0].Name = "被外部改坏"
	if got, _ := s.Get(list[0].ID); got.Name == "被外部改坏" {
		t.Errorf("List 返回的不是副本")
	}
}

func TestScopeNormalization(t *testing.T) {
	s := newStore(t, "")
	cases := map[string]string{
		"":          "/",
		"/":         "/",
		"uploads":   "/uploads",
		"/uploads/": "/uploads",
		"/a/b/":     "/a/b",
	}
	for in, want := range cases {
		k, _, err := s.Create(CreateOptions{Name: "x", Scope: in})
		if err != nil {
			t.Fatal(err)
		}
		if k.Scope != want {
			t.Errorf("scope %q → %q, 期望 %q", in, k.Scope, want)
		}
	}
}

func TestDefaultName(t *testing.T) {
	s := newStore(t, "")
	k, _, _ := s.Create(CreateOptions{})
	if k.Name != "未命名密钥" {
		t.Errorf("默认名称 = %q", k.Name)
	}
}

func TestVisiblePrefixHidesSecret(t *testing.T) {
	s := newStore(t, "")
	k, token, _ := s.Create(CreateOptions{Name: "x"})

	if !strings.HasPrefix(k.Prefix, TokenPrefix) {
		t.Errorf("前缀缺少标识: %q", k.Prefix)
	}
	// 前缀必须明显短于完整密钥，不能凭它反推出密钥
	if len(k.Prefix) >= len(token) {
		t.Errorf("前缀 %q 过长，可能泄露密钥", k.Prefix)
	}
	if !strings.HasPrefix(token, k.Prefix) {
		t.Errorf("前缀应与明文开头一致，便于人工核对")
	}
}

func TestConcurrentVerify(t *testing.T) {
	s := newStore(t, "")
	_, token, _ := s.Create(CreateOptions{Name: "并发"})

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := s.Verify(token); err != nil {
				t.Errorf("并发校验失败: %v", err)
			}
		}()
	}
	wg.Wait()

	list := s.List()
	if len(list) != 1 {
		t.Fatalf("数量 = %d", len(list))
	}
	if list[0].UseCount != 50 {
		t.Errorf("并发使用次数 = %d, 期望 50", list[0].UseCount)
	}
}

func TestPrune(t *testing.T) {
	s := newStore(t, "")
	// 造一个很久以前过期的密钥
	k, _, _ := s.Create(CreateOptions{Name: "老的", TTL: time.Millisecond})
	time.Sleep(5 * time.Millisecond)
	// 未超过宽限期，不应被清理
	if n := s.Prune(time.Hour); n != 0 {
		t.Errorf("宽限期内不应清理，实际清理 %d 个", n)
	}
	// 宽限期为 0 时应被清理
	if n := s.Prune(0); n != 1 {
		t.Errorf("应清理 1 个，实际 %d 个", n)
	}
	if _, ok := s.Get(k.ID); ok {
		t.Errorf("被清理的密钥仍然存在")
	}
}

func TestFlushWithoutChanges(t *testing.T) {
	dir := t.TempDir()
	s := newStore(t, filepath.Join(dir, "k.json"))
	if err := s.Flush(); err != nil {
		t.Errorf("无改动时 Flush 不应报错: %v", err)
	}
}

// TestKindOf 钉住三种凭证类型的归一化，以及「历史数据当作上传密钥」这条兼容。
func TestKindOf(t *testing.T) {
	cases := []struct {
		kind      string
		want      string
		wantShare bool
		wantRW    bool
	}{
		{KindUpload, KindUpload, false, false},
		{"", KindUpload, false, false}, // 历史数据没有 kind 字段
		{KindRead, KindRead, true, false},
		{KindReadWrite, KindReadWrite, true, true},
		{"乱写的", KindUpload, false, false},
	}
	for _, c := range cases {
		k := &Key{Kind: c.kind}
		if got := k.KindOf(); got != c.want {
			t.Errorf("Kind=%q → KindOf=%q，期望 %q", c.kind, got, c.want)
		}
		if got := k.IsShare(); got != c.wantShare {
			t.Errorf("Kind=%q → IsShare=%v，期望 %v", c.kind, got, c.wantShare)
		}
		if got := k.ReadWrite(); got != c.wantRW {
			t.Errorf("Kind=%q → ReadWrite=%v，期望 %v", c.kind, got, c.wantRW)
		}
	}
}

// TestShareKindsGetDefaultName 确认两种分享都有「分享链接」这个默认名。
func TestShareKindsGetDefaultName(t *testing.T) {
	s, err := New("")
	if err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{KindRead, KindReadWrite} {
		k, _, err := s.Create(CreateOptions{Scope: "/x", Kind: kind})
		if err != nil {
			t.Fatalf("Create(%s): %v", kind, err)
		}
		if k.Name != "分享链接" {
			t.Errorf("kind=%s 的默认名 = %q，期望「分享链接」", kind, k.Name)
		}
	}
}

// TestRevokeByCreator 确认按创建者撤销只影响该账号名下的密钥。
func TestRevokeByCreator(t *testing.T) {
	s := newStore(t, "")
	bobKey, _, err := s.Create(CreateOptions{Name: "bob-key", Scope: "/", Creator: "bob"})
	if err != nil {
		t.Fatal(err)
	}
	aliceKey, _, err := s.Create(CreateOptions{Name: "alice-key", Scope: "/", Creator: "alice"})
	if err != nil {
		t.Fatal(err)
	}

	n, err := s.RevokeByCreator("bob")
	if err != nil {
		t.Fatalf("RevokeByCreator: %v", err)
	}
	if n != 1 {
		t.Fatalf("撤销数量 = %d，期望 1", n)
	}
	if _, ok := s.Get(bobKey.ID); ok {
		t.Error("bob 的密钥仍然存在")
	}
	if _, ok := s.Get(aliceKey.ID); !ok {
		t.Error("alice 的密钥被误删")
	}
	if n, err := s.RevokeByCreator(""); n != 0 || err != nil {
		t.Errorf("空创建者不应撤销任何东西：n=%d err=%v", n, err)
	}
}

// TestKeyQuota 确认单账号配额生效，且不会误伤其它账号。
func TestKeyQuota(t *testing.T) {
	s := newStore(t, "")
	for i := 0; i < MaxKeysPerCreator; i++ {
		if _, _, err := s.Create(CreateOptions{Name: "k", Scope: "/", Creator: "bob"}); err != nil {
			t.Fatalf("第 %d 个密钥创建失败: %v", i+1, err)
		}
	}
	if _, _, err := s.Create(CreateOptions{Name: "over", Scope: "/", Creator: "bob"}); !errors.Is(err, ErrTooManyKeys) {
		t.Fatalf("超出配额时错误 = %v，期望 ErrTooManyKeys", err)
	}
	if _, _, err := s.Create(CreateOptions{Name: "alice", Scope: "/", Creator: "alice"}); err != nil {
		t.Fatalf("其它账号不应被配额误伤: %v", err)
	}
}

// TestKeyFieldLimits 确认名称/备注/scope 的超长输入会被拒绝。
func TestKeyFieldLimits(t *testing.T) {
	s := newStore(t, "")
	cases := []CreateOptions{
		{Name: strings.Repeat("n", MaxKeyNameRunes+1), Scope: "/"},
		{Name: "n", Scope: "/", Note: strings.Repeat("x", MaxKeyNoteRunes+1)},
		{Name: "n", Scope: "/" + strings.Repeat("a", MaxKeyScopeRunes)},
	}
	for i, opt := range cases {
		if _, _, err := s.Create(opt); !errors.Is(err, ErrInvalidOption) {
			t.Errorf("用例 %d 错误 = %v，期望 ErrInvalidOption", i, err)
		}
	}
}

// TestStartupPrunesExpired 确认启动时会清掉已过期的密钥。
func TestStartupPrunesExpired(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "keys.json")
	s := newStore(t, path)
	if _, _, err := s.Create(CreateOptions{Name: "expired", Scope: "/", TTL: time.Millisecond}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(5 * time.Millisecond)

	s2 := newStore(t, path)
	if got := s2.Count(); got != 0 {
		t.Fatalf("启动后过期密钥数量 = %d，期望 0", got)
	}
}

// TestCreateRejectsControlCharsInScope 确认 scope 里的控制字符在入口被拒绝。
//
// scope 会被渲染进 /up 脚本的注释；注释不认引号，换行会提前结束注释，
// 把后面的内容变成可执行命令。必须在创建时就拒绝，而不是依赖某个输出路径。
func TestCreateRejectsControlCharsInScope(t *testing.T) {
	s := newStore(t, "")
	cases := []string{
		"/docs/\ntouch /tmp/pwn #",
		"/docs/\r\nx",
		"/docs/\tx",
		"/docs/\x00x",
		"/docs/\x1bx",
	}
	for i, scope := range cases {
		if _, _, err := s.Create(CreateOptions{Name: "x", Scope: scope}); !errors.Is(err, ErrInvalidOption) {
			t.Errorf("用例 %d（%q）错误 = %v，期望 ErrInvalidOption", i, scope, err)
		}
	}
	// 普通空格、中文与引号仍然允许。
	if _, _, err := s.Create(CreateOptions{Name: "x", Scope: "/目录 名/it's"}); err != nil {
		t.Errorf("合法 scope 被拒绝: %v", err)
	}
}
