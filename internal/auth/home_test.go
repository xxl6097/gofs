package auth

import (
	"reflect"
	"testing"
)

// TestHomePath 验证落地目录的挑选顺序：可读写优先，其次取最靠上的一层。
//
// 这个值直接决定「只被授予子目录的账号登录后落在哪儿」——挑错了就是
// 落在读不到的目录上，前端随即被判 401 并弹回登录框。
func TestHomePath(t *testing.T) {
	a := mustOpen(t, []string{
		"admin:secret@/:rw",
		"ro:ro@/:r",
		// 先只读、后读写，且读写的路径更长：验证「权限优先于路径长度」
		"mixed:mixed@/:r,/docs/sub:rw",
		"docs:docs@/docs:rw,/pic:r",
		"nested:nested@/docs/sub:rw,/docs:r",
	}, "")

	cases := []struct {
		user, pass, want string
	}{
		{"admin", "secret", "/"},
		{"ro", "ro", "/"},
		{"mixed", "mixed", "/docs/sub"},
		{"docs", "docs", "/docs"}, // 同为可读时取最短（最靠上）
		{"nested", "nested", "/docs/sub"},
	}
	for _, c := range cases {
		if got := a.HomePath(c.user, c.pass, true); got != c.want {
			t.Errorf("HomePath(%s) = %q, 期望 %q", c.user, got, c.want)
		}
	}

	// 密码不对、账号不存在、匿名：都拿不到落地目录
	if got := a.HomePath("docs", "wrong", true); got != "" {
		t.Errorf("密码错误时应返回空串，实际 %q", got)
	}
	if got := a.HomePath("nobody", "x", true); got != "" {
		t.Errorf("账号不存在时应返回空串，实际 %q", got)
	}
	if got := a.HomePath("", "", false); got != "" {
		t.Errorf("没有匿名规则时应返回空串，实际 %q", got)
	}
}

// TestHomePathWithoutAuth 未启用鉴权时到处都是可读的，落回根目录。
func TestHomePathWithoutAuth(t *testing.T) {
	a, err := Open(nil, "")
	if err != nil {
		t.Fatalf("Open 失败: %v", err)
	}
	if a != nil {
		t.Fatalf("没有规则时应当返回 nil（表示未启用鉴权），实际 %v", a)
	}
	// nil 接收者也要能用：调用方（登录接口）只在启用鉴权时才走到这里，
	// 但空指针崩溃比返回一个安全默认值糟糕得多。
	if got := a.HomePath("", "", false); got != "/" {
		t.Errorf("未启用鉴权时 HomePath = %q, 期望 /", got)
	}
	if got := a.ReadPaths("", "", false); got != nil {
		t.Errorf("未启用鉴权时 ReadPaths = %v, 期望 nil", got)
	}
}

// TestReadPathsOrderedByLength 长的路径在前，前端按此判断可达性。
func TestReadPathsOrderedByLength(t *testing.T) {
	a := mustOpen(t, []string{"admin:secret@/:rw"}, "")
	if err := a.AddUser("alice", "alicepass", []Rule{
		{Path: "/docs/sub", Perm: PermReadWrite},
		{Path: "/docs", Perm: PermRead},
		{Path: "/pic", Perm: PermRead},
	}); err != nil {
		t.Fatalf("AddUser 失败: %v", err)
	}

	got := a.ReadPaths("alice", "alicepass", true)
	want := []string{"/docs/sub", "/docs", "/pic"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ReadPaths = %v, 期望 %v", got, want)
	}
	if got := a.ReadPaths("alice", "wrong", true); got != nil {
		t.Errorf("密码错误时应返回 nil，实际 %v", got)
	}
	// 匿名规则（这里是启动参数里的 admin）与具名账号互不干扰
	if got := a.ReadPaths("admin", "secret", true); !reflect.DeepEqual(got, []string{"/"}) {
		t.Errorf("admin 的 ReadPaths = %v, 期望 [/]", got)
	}
}
