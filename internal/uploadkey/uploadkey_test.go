package uploadkey

import (
	"encoding/json"
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
	// 改一个字符也应失败
	tampered := token[:len(token)-1] + "X"
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
