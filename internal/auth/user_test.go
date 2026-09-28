package auth

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func mustOpen(t *testing.T, rules []string, userFile string) *Authenticator {
	t.Helper()
	a, err := Open(rules, userFile)
	if err != nil {
		t.Fatalf("Open(%v, %q) 失败: %v", rules, userFile, err)
	}
	if a == nil {
		t.Fatalf("Open(%v, %q) 返回 nil（应当启用鉴权）", rules, userFile)
	}
	return a
}

func tmpUserFile(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "users.json")
}

// ------------------------------------------------------------------ 基本行为

func TestUserStoreAddAndLogin(t *testing.T) {
	a := mustOpen(t, []string{"admin:secret@/:rw"}, tmpUserFile(t))

	if err := a.AddUser("alice", "alicepass", []Rule{
		{Path: "/docs", Perm: PermReadWrite},
		{Path: "/", Perm: PermRead},
	}); err != nil {
		t.Fatalf("AddUser 失败: %v", err)
	}

	if !a.Verify("alice", "alicepass") {
		t.Errorf("新用户的凭据校验失败")
	}
	if a.Verify("alice", "wrong") {
		t.Errorf("错误密码不应通过")
	}
	// 路径语义：/docs 最长前缀命中可读写，其余位置只读
	if got := a.Lookup("/docs/a.txt", "alice", "alicepass", true); got != PermReadWrite {
		t.Errorf("/docs/a.txt = %v, 期望可读写", got)
	}
	if got := a.Lookup("/docs", "alice", "alicepass", true); got != PermReadWrite {
		t.Errorf("/docs = %v, 期望可读写", got)
	}
	if got := a.Lookup("/other/a.txt", "alice", "alicepass", true); got != PermRead {
		t.Errorf("/other/a.txt = %v, 期望只读", got)
	}
	// 前缀必须按「目录段」匹配，/docs2 不是 /docs 的子路径
	if got := a.Lookup("/docs2/a.txt", "alice", "alicepass", true); got != PermRead {
		t.Errorf("/docs2/a.txt = %v, 期望只读（不能按裸前缀匹配）", got)
	}
	// 启动参数的账号不受影响
	if got := a.Lookup("/x", "admin", "secret", true); got != PermReadWrite {
		t.Errorf("admin 的权限被影响: %v", got)
	}
	if !a.HasAnyWrite("alice", "alicepass", true) {
		t.Errorf("alice 在 /docs 上可写，HasAnyWrite 应为 true")
	}
}

func TestUserStorePasswordIsHashedOnDisk(t *testing.T) {
	file := tmpUserFile(t)
	a := mustOpen(t, []string{"admin:secret@/:rw"}, file)
	if err := a.AddUser("alice", "super-secret-pw", []Rule{{Path: "/", Perm: PermReadWrite}}); err != nil {
		t.Fatal(err)
	}

	data, err := os.ReadFile(file)
	if err != nil {
		t.Fatalf("用户表未落盘: %v", err)
	}
	if strings.Contains(string(data), "super-secret-pw") {
		t.Errorf("用户表里出现了明文密码:\n%s", data)
	}
	for _, want := range []string{`"salt"`, `"hash"`, `"iter"`} {
		if !strings.Contains(string(data), want) {
			t.Errorf("用户表缺少 %s 字段:\n%s", want, data)
		}
	}
	if !strings.Contains(string(data), `"perm": "rw"`) {
		t.Errorf("用户表未记录规则:\n%s", data)
	}
}

func TestUserStorePersistenceAcrossReload(t *testing.T) {
	file := tmpUserFile(t)
	a := mustOpen(t, []string{"admin:secret@/:rw"}, file)
	if err := a.AddUser("alice", "alicepass", []Rule{{Path: "/docs", Perm: PermReadWrite}}); err != nil {
		t.Fatal(err)
	}

	b := mustOpen(t, []string{"admin:secret@/:rw"}, file)
	if !b.Verify("alice", "alicepass") {
		t.Errorf("重载后密码校验失败")
	}
	if got := b.Lookup("/docs/x", "alice", "alicepass", true); got != PermReadWrite {
		t.Errorf("重载后规则丢失: %v", got)
	}
	if got := b.Lookup("/x", "alice", "alicepass", true); got != PermNone {
		t.Errorf("重载后多出了权限: %v", got)
	}
}

func TestUserStoreUpdateAndDelete(t *testing.T) {
	file := tmpUserFile(t)
	a := mustOpen(t, []string{"admin:secret@/:rw"}, file)
	if err := a.AddUser("alice", "alicepass", []Rule{{Path: "/docs", Perm: PermReadWrite}}); err != nil {
		t.Fatal(err)
	}

	// 只改规则，密码留空表示不动
	if err := a.UpdateUser("alice", "", []Rule{{Path: "/docs", Perm: PermRead}}); err != nil {
		t.Fatalf("UpdateUser 失败: %v", err)
	}
	if !a.Verify("alice", "alicepass") {
		t.Errorf("密码不该被清掉")
	}
	if got := a.Lookup("/docs/x", "alice", "alicepass", true); got != PermRead {
		t.Errorf("规则未更新: %v", got)
	}

	// 改密码后旧密码要**立刻**失效（密码校验有缓存，必须一起失效）
	if err := a.UpdateUser("alice", "newpass123", []Rule{{Path: "/docs", Perm: PermRead}}); err != nil {
		t.Fatalf("UpdateUser 失败: %v", err)
	}
	if !a.Verify("alice", "newpass123") {
		t.Errorf("新密码无效")
	}
	if a.Verify("alice", "alicepass") {
		t.Errorf("旧密码仍然有效 —— 密码校验缓存没有随改动失效")
	}

	if err := a.DeleteUser("alice"); err != nil {
		t.Fatalf("DeleteUser 失败: %v", err)
	}
	if a.Verify("alice", "newpass123") {
		t.Errorf("删除后仍能登录")
	}
	if len(a.Users()) != 1 {
		t.Errorf("删除后剩余账号数 = %d, 期望 1", len(a.Users()))
	}
}

// ------------------------------------------------------------------ 输入校验

func TestUserStoreRejectsBadInput(t *testing.T) {
	a := mustOpen(t, []string{"admin:secret@/:rw"}, tmpUserFile(t))

	cases := []struct {
		name    string
		user    string
		pw      string
		rules   []Rule
		wantSub string
	}{
		{"空用户名", "", "goodpass1", []Rule{{Path: "/", Perm: PermRead}}, "不能为空"},
		{"用户名含空格", "a b", "goodpass1", []Rule{{Path: "/", Perm: PermRead}}, "空白"},
		{"用户名含冒号", "a:b", "goodpass1", []Rule{{Path: "/", Perm: PermRead}}, "不能包含"},
		{"用户名含 @", "a@b", "goodpass1", []Rule{{Path: "/", Perm: PermRead}}, "不能包含"},
		{"密码过短", "bob", "123", []Rule{{Path: "/", Perm: PermRead}}, "至少"},
		{"没有规则", "bob", "goodpass1", nil, "至少要给"},
		{"同一路径重复", "bob", "goodpass1",
			[]Rule{{Path: "/a", Perm: PermReadWrite}, {Path: "/a", Perm: PermRead}}, "重复"},
		{"路径含 ..", "bob", "goodpass1", []Rule{{Path: "/../etc", Perm: PermReadWrite}}, ".."},
		{"权限非法", "bob", "goodpass1", []Rule{{Path: "/", Perm: PermNone}}, "只能是"},
	}
	for _, c := range cases {
		err := a.AddUser(c.user, c.pw, c.rules)
		if err == nil {
			t.Errorf("%s：应当被拒绝，却成功了", c.name)
			continue
		}
		if !strings.Contains(err.Error(), c.wantSub) {
			t.Errorf("%s：错误信息 %q 未包含 %q", c.name, err.Error(), c.wantSub)
		}
	}
	if len(a.Users()) != 1 {
		t.Errorf("失败的创建不应留下账号，当前 %d 个", len(a.Users()))
	}
}

func TestUserStoreRejectsDuplicateAndStartupNames(t *testing.T) {
	a := mustOpen(t, []string{"admin:secret@/:rw"}, tmpUserFile(t))
	if err := a.AddUser("alice", "alicepass", []Rule{{Path: "/", Perm: PermRead}}); err != nil {
		t.Fatal(err)
	}
	if err := a.AddUser("alice", "otherpass", []Rule{{Path: "/", Perm: PermRead}}); err == nil {
		t.Errorf("重名创建应当被拒绝")
	}
	err := a.AddUser("admin", "otherpass", []Rule{{Path: "/", Perm: PermRead}})
	if err == nil {
		t.Fatalf("与启动参数重名应当被拒绝")
	}
	if !strings.Contains(err.Error(), "启动参数") {
		t.Errorf("错误信息应点明来源：%v", err)
	}
}

func TestStartupAccountIsReadOnlyInPage(t *testing.T) {
	a := mustOpen(t, []string{"admin:secret@/:rw"}, tmpUserFile(t))

	if err := a.UpdateUser("admin", "", []Rule{{Path: "/", Perm: PermRead}}); err == nil {
		t.Errorf("改启动参数的账号应当被拒绝")
	}
	if err := a.DeleteUser("admin"); err == nil {
		t.Errorf("删启动参数的账号应当被拒绝")
	}
	// 启动参数的账号必须被标记出来（界面上要显示「来自启动参数」）
	var found bool
	for _, u := range a.Users() {
		if u.Name == "admin" {
			found = true
			if !u.FromStartup {
				t.Errorf("admin 应标记 FromStartup")
			}
		}
	}
	if !found {
		t.Errorf("列表里没有 admin")
	}
}

// ------------------------------------------------------------------ 防呆

// TestGuardKeepsAtLeastOneRootAdmin 覆盖「把最后一个根管理员删掉 / 降权」。
//
// 这种部署形态下没有启动参数的具名管理员（只有匿名只读规则），
// 唯一的根管理员只能是新建的 —— 一旦删掉，页面上就再没有入口能管理了。
func TestGuardKeepsAtLeastOneRootAdmin(t *testing.T) {
	file := tmpUserFile(t)
	// 只有匿名只读：鉴权已启用，但没有任何具名账号
	a := mustOpen(t, []string{"@/:r"}, file)

	if err := a.AddUser("boss", "bosspass1", []Rule{{Path: "/", Perm: PermReadWrite}}); err != nil {
		t.Fatalf("AddUser 失败: %v", err)
	}

	if err := a.UpdateUser("boss", "", []Rule{{Path: "/docs", Perm: PermReadWrite}}); err == nil {
		t.Errorf("把唯一的根管理员降权应当被拒绝")
	}
	if err := a.DeleteUser("boss"); err == nil {
		t.Errorf("删掉唯一的根管理员应当被拒绝")
	}
	if !a.Verify("boss", "bosspass1") {
		t.Errorf("被拒绝的操作不该真的改动数据")
	}

	// 再建一个根管理员之后，降权/删除就都合法了
	if err := a.AddUser("boss2", "boss2pass", []Rule{{Path: "/", Perm: PermReadWrite}}); err != nil {
		t.Fatal(err)
	}
	if err := a.UpdateUser("boss", "", []Rule{{Path: "/docs", Perm: PermReadWrite}}); err != nil {
		t.Errorf("有兜底管理员后降权应当允许: %v", err)
	}
	// 此时 boss2 成了唯一的根管理员，删它必须再次被拦住
	if err := a.DeleteUser("boss2"); err == nil {
		t.Errorf("boss2 是唯一的根管理员，删除应当被拒绝")
	}
	// 把 boss 重新提回根管理员，再删 boss2 就应当允许
	if err := a.UpdateUser("boss", "", []Rule{{Path: "/", Perm: PermReadWrite}}); err != nil {
		t.Fatalf("重新提权失败: %v", err)
	}
	if err := a.DeleteUser("boss2"); err != nil {
		t.Errorf("还有另一个根管理员时删除应当允许: %v", err)
	}
}

// 启动参数里的 :r 与 :ro 都表示只读；写成 :r 时不能把 ":r" 当成目录名的一部分。
func TestParseAcceptsColonRAlias(t *testing.T) {
	a := mustOpen(t, []string{"bob:pw@/docs:r", "eve:pw@/logs:ro", "amy:pw@/pub"}, "")

	if got := a.Lookup("/docs/a.txt", "bob", "pw", true); got != PermRead {
		t.Errorf("/docs:r 应解析为「/docs 只读」，实际 %v", got)
	}
	if got := a.Lookup("/docs:r/a.txt", "bob", "pw", true); got != PermNone {
		t.Errorf(":r 被当成了目录名的一部分（出现了 /docs:r 这条规则）")
	}
	if got := a.Lookup("/logs/a.txt", "eve", "pw", true); got != PermRead {
		t.Errorf("/logs:ro 应解析为只读，实际 %v", got)
	}
	// 省略后缀同样是只读
	if got := a.Lookup("/pub/a.txt", "amy", "pw", true); got != PermRead {
		t.Errorf("/pub 应解析为只读，实际 %v", got)
	}
}

// 反过来：只要启动参数里有具名管理员，删用户表里的账号就不受这条限制。
func TestGuardDoesNotBlockWhenStartupAdminExists(t *testing.T) {
	a := mustOpen(t, []string{"admin:secret@/:rw"}, tmpUserFile(t))
	if err := a.AddUser("boss", "bosspass1", []Rule{{Path: "/", Perm: PermReadWrite}}); err != nil {
		t.Fatal(err)
	}
	if err := a.DeleteUser("boss"); err != nil {
		t.Errorf("有启动参数管理员兜底时删除应当允许: %v", err)
	}
}

// ------------------------------------------------------------------ 其他

func TestUserFileEntryIgnoredWhenNameTakenByStartup(t *testing.T) {
	file := tmpUserFile(t)
	// 手工写一份与启动参数重名的用户表
	doc := `{"version":1,"users":[{"name":"admin","salt":"AAAAAAAAAAAAAAAAAAAAAA==",` +
		`"hash":"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=","iter":1000,` +
		`"rules":[{"path":"/","perm":"r"}]}]}`
	if err := os.WriteFile(file, []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}

	a := mustOpen(t, []string{"admin:secret@/:rw"}, file)
	// 以启动参数为准：密码仍然是 secret，权限仍然是 rw
	if !a.Verify("admin", "secret") {
		t.Errorf("应当以启动参数的密码为准")
	}
	if got := a.Lookup("/x", "admin", "secret", true); got != PermReadWrite {
		t.Errorf("应当以启动参数的权限为准，实际 %v", got)
	}
	if got := a.Lookup("/x", "admin", "AAAAAAAA", true); got != PermNone {
		t.Errorf("用户表里同名条目不应生效")
	}
}

func TestOpenRejectsCorruptUserFile(t *testing.T) {
	dir := t.TempDir()
	cases := map[string]string{
		"不是 JSON": `not json`,
		"版本过高":    `{"version":999,"users":[]}`,
		"用户名为空":   `{"version":1,"users":[{"name":"","salt":"AAAAAAAAAAAAAAAAAAAAAA==","hash":"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=","iter":1000,"rules":[{"path":"/","perm":"r"}]}]}`,
		"没有规则":    `{"version":1,"users":[{"name":"a","salt":"AAAAAAAAAAAAAAAAAAAAAA==","hash":"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=","iter":1000,"rules":[]}]}`,
		"盐值太短":    `{"version":1,"users":[{"name":"a","salt":"AA==","hash":"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=","iter":1000,"rules":[{"path":"/","perm":"r"}]}]}`,
		"迭代轮数越界":  `{"version":1,"users":[{"name":"a","salt":"AAAAAAAAAAAAAAAAAAAAAA==","hash":"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=","iter":5,"rules":[{"path":"/","perm":"r"}]}]}`,
	}
	for name, content := range cases {
		file := filepath.Join(dir, "u.json")
		if err := os.WriteFile(file, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := Open([]string{"admin:secret@/:rw"}, file); err == nil {
			t.Errorf("%s：应当启动失败，却通过了", name)
		}
	}
}

func TestAnonymousRulesExposed(t *testing.T) {
	a := mustOpen(t, []string{"@/public:r", "admin:secret@/:rw"}, tmpUserFile(t))
	rules := a.AnonymousRules()
	if len(rules) != 1 || rules[0].Path != "/public" || rules[0].Perm != PermRead {
		t.Errorf("匿名规则 = %+v", rules)
	}
	// 匿名规则不能混进 Users()（它不是账号）
	for _, u := range a.Users() {
		if u.Name == "" {
			t.Errorf("Users() 里混进了匿名条目")
		}
	}
}

func TestMemoryOnlyStoreWhenNoFile(t *testing.T) {
	a := mustOpen(t, []string{"admin:secret@/:rw"}, "")
	if a.UserFile() != "" {
		t.Errorf("UserFile 应为空串")
	}
	if err := a.AddUser("alice", "alicepass", []Rule{{Path: "/", Perm: PermRead}}); err != nil {
		t.Fatalf("仅内存模式也应当能建用户: %v", err)
	}
	if !a.Verify("alice", "alicepass") {
		t.Errorf("仅内存模式下登录失败")
	}
}
