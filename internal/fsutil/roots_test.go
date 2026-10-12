package fsutil

import (
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// mkRoot 建一个临时目录并返回路径（末尾带基名，便于控制标签）。
func mkRoot(t *testing.T, parent, name string) string {
	t.Helper()
	p := filepath.Join(parent, name)
	if err := os.MkdirAll(p, 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func mustRoots(t *testing.T, paths ...string) *Roots {
	t.Helper()
	r, err := BuildRoots(paths, false)
	if err != nil {
		t.Fatalf("BuildRoots(%v): %v", paths, err)
	}
	return r
}

// under 用挂载点自己的 Root() 拼路径。
//
// ⚠️ 不能拿建目录时的原路径去比：Resolver 会把根解析成真实路径
// （macOS 上 /var 是指向 /private/var 的软链），两边会差一截。
func under(m Mount, rel string) string {
	if rel == "" {
		return m.Res.Root()
	}
	return filepath.Join(m.Res.Root(), filepath.FromSlash(rel))
}

// TestBuildRootsPrimaryServesAtRoot 确认主目录不带前缀、附加站点带标签。
func TestBuildRootsPrimaryServesAtRoot(t *testing.T) {
	base := t.TempDir()
	main := mkRoot(t, base, "data")
	backup := mkRoot(t, base, "backup")
	writeFile(t, filepath.Join(main, "notes.txt"), "m")
	writeFile(t, filepath.Join(main, "backup", "shadow.txt"), "主目录里的 backup")
	writeFile(t, filepath.Join(backup, "b.txt"), "b")

	r := mustRoots(t, main, backup)
	if got := r.Labels(); len(got) != 1 || got[0] != "backup" {
		t.Fatalf("附加站点标签 = %v，期望 [backup]", got)
	}
	if !r.HasExtras() {
		t.Fatal("有附加站点，HasExtras 却是 false")
	}

	for _, tc := range []struct{ url, want string }{
		{"/notes.txt", filepath.Join(main, "notes.txt")}, // 主目录：无前缀
		{"/", main}, // 主目录本身
		{"/backup/b.txt", filepath.Join(backup, "b.txt")}, // 附加站点：带标签
		{"/backup", backup}, // 附加站点本身
	} {
		got, err := r.Resolve(tc.url)
		if err != nil {
			t.Errorf("Resolve(%q) 出错: %v", tc.url, err)
			continue
		}
		if got != tc.want {
			t.Errorf("Resolve(%q) = %q，期望 %q", tc.url, got, tc.want)
		}
	}

	// ⚠️ 遮蔽：主目录里也有个 backup/，但 /backup 已经被站点占了。
	got, err := r.Resolve("/backup/shadow.txt")
	if err != nil {
		t.Fatalf("Resolve 出错: %v", err)
	}
	if got != filepath.Join(backup, "shadow.txt") {
		t.Errorf("站点没遮蔽主目录的同名子目录：%q", got)
	}
}

// TestResolveUnknownPrefixIsPrimaryRelative 确认「未知前缀」不再特殊 ——
// 它就是主目录下的普通路径（混合形状的必然结果）。
func TestResolveUnknownPrefixIsPrimaryRelative(t *testing.T) {
	base := t.TempDir()
	main := mkRoot(t, base, "data")
	r := mustRoots(t, main, mkRoot(t, base, "backup"))

	for _, tc := range []struct{ url, want string }{
		{"/nope", filepath.Join(main, "nope")},
		{"/nope/x", filepath.Join(main, "nope", "x")},
	} {
		got, err := r.Resolve(tc.url)
		if err != nil {
			t.Errorf("Resolve(%q) 出错: %v", tc.url, err)
			continue
		}
		if got != tc.want {
			t.Errorf("Resolve(%q) = %q，期望 %q", tc.url, got, tc.want)
		}
	}
}

// TestResolveCleansParentSegments 确认 `..` 只被消解、不会越出主目录。
func TestResolveCleansParentSegments(t *testing.T) {
	base := t.TempDir()
	main := mkRoot(t, base, "data")
	r := mustRoots(t, main)

	got, err := r.Resolve("/../etc/passwd")
	if err != nil {
		t.Fatalf("Resolve 出错: %v", err)
	}
	// 归一化后是 /etc/passwd，落在主目录之内 —— 与不支持多站点时一致。
	if want := filepath.Join(main, "etc", "passwd"); got != want {
		t.Errorf("Resolve = %q，期望 %q", got, want)
	}
}

// TestRelMapsPrimaryAndExtras 覆盖反向映射的两种前缀。
func TestRelMapsPrimaryAndExtras(t *testing.T) {
	base := t.TempDir()
	main := mkRoot(t, base, "data")
	backup := mkRoot(t, base, "backup")
	r := mustRoots(t, main, backup)

	for _, tc := range []struct{ abs, want string }{
		{filepath.Join(main, "a.txt"), "/a.txt"}, // 主目录：无前缀
		{filepath.Join(main, "sub", "b.txt"), "/sub/b.txt"},
		{main, "/"},
		{filepath.Join(backup, "x.txt"), "/backup/x.txt"}, // 附加站点：带标签
		{backup, "/backup"},
	} {
		got, err := r.Rel(tc.abs)
		if err != nil {
			t.Errorf("Rel(%q) 出错: %v", tc.abs, err)
			continue
		}
		if got != tc.want {
			t.Errorf("Rel(%q) = %q，期望 %q", tc.abs, got, tc.want)
		}
	}

	if _, err := r.Rel(filepath.Join(base, "outside.txt")); !errors.Is(err, ErrOutsideRoots) {
		t.Errorf("根外路径的错 = %v，期望 ErrOutsideRoots", err)
	}
}

// TestSetRootMovesPrimary 确认单根时的运行期换根照旧可用（这是
// TestSingleRootUnchanged 与 7 个换根测试能不改就绿的根本原因）。
func TestSetRootMovesPrimary(t *testing.T) {
	base := t.TempDir()
	first := mkRoot(t, base, "one")
	second := mkRoot(t, base, "two")
	r := mustRoots(t, first)

	if err := r.SetRoot(second); err != nil {
		t.Fatalf("SetRoot: %v", err)
	}
	if got := r.Root(); got != second {
		t.Errorf("Root() = %q，期望 %q", got, second)
	}
	got, err := r.Resolve("/x")
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(second, "x"); got != want {
		t.Errorf("换根后 Resolve = %q，期望 %q", got, want)
	}
}

// TestBuildRootsLabels 覆盖附加站点的标签分配：默认取目录名、重名加后缀、
// 避开保留字。
func TestBuildRootsLabels(t *testing.T) {
	base := t.TempDir()
	main := mkRoot(t, base, "main")

	// 重名：先到先得，后来的加 -2。
	a := mkRoot(t, filepath.Join(base, "one"), "data")
	b := mkRoot(t, filepath.Join(base, "two"), "data")
	r := mustRoots(t, main, a, b)
	if got := r.Labels(); len(got) != 2 || got[0] != "data" || got[1] != "data-2" {
		t.Fatalf("标签 = %v，期望 [data data-2]", got)
	}
	// 稳定性：同样输入每次一样，否则重启一次书签就换了。
	for i := 0; i < 3; i++ {
		if again := mustRoots(t, main, a, b).Labels(); again[1] != "data-2" {
			t.Fatalf("标签不稳定：%v", again)
		}
	}

	// 保留字：/fs/* 是内部接口、/s/* 是分享地址，目录叫 fs 只能拿 fs-2。
	fsDir := mkRoot(t, base, "fs")
	sDir := mkRoot(t, base, "s")
	r2 := mustRoots(t, main, fsDir, sDir)
	if got := r2.Labels(); got[0] != "fs-2" || got[1] != "s-2" {
		t.Fatalf("标签 = %v，期望 [fs-2 s-2]", got)
	}
}

// TestBuildRootsRejectsNested 互相嵌套的根必须拒绝启动。
func TestBuildRootsRejectsNested(t *testing.T) {
	base := t.TempDir()
	outer := mkRoot(t, base, "outer")
	inner := mkRoot(t, outer, "inner")

	if _, err := BuildRoots([]string{outer, inner}, false); err == nil {
		t.Fatal("嵌套的根应当报错")
	}
	if _, err := BuildRoots([]string{inner, outer}, false); err == nil {
		t.Fatal("嵌套的根（顺序相反）应当报错")
	}
	// 主目录与附加站点之间同样不能嵌套。
	main := mkRoot(t, base, "main")
	if _, err := BuildRoots([]string{main, filepath.Join(main, "sub")}, false); err == nil {
		t.Fatal("附加站点嵌在主目录里应当报错")
	}
}

// TestBuildRootsDedupSamePath 同一路径只留一个。
func TestBuildRootsDedupSamePath(t *testing.T) {
	base := t.TempDir()
	p := mkRoot(t, base, "data")
	link := filepath.Join(base, "alias")
	if err := os.Symlink(p, link); err != nil {
		t.Skipf("本平台不支持软链: %v", err)
	}
	r := mustRoots(t, p, p, link)
	if len(r.Mounts()) != 1 {
		t.Fatalf("同一路径应去重成一个挂载点，实际 %d 个", len(r.Mounts()))
	}
	if r.HasExtras() {
		t.Fatal("去重后不该有附加站点")
	}
}

// TestAddSite 覆盖运行期新增：标签分配、去重、嵌套、以及遮蔽。
func TestAddSite(t *testing.T) {
	base := t.TempDir()
	main := mkRoot(t, base, "main")
	writeFile(t, filepath.Join(main, "a.txt"), "m")
	r := mustRoots(t, main)

	// 默认标签取目录名。
	site := mkRoot(t, base, "backup")
	m, err := r.AddSite(site, "")
	if err != nil {
		t.Fatalf("AddSite: %v", err)
	}
	if m.Label != "backup" {
		t.Errorf("标签 = %q，期望 backup", m.Label)
	}
	if got, _ := r.Resolve("/backup/a.txt"); got != filepath.Join(site, "a.txt") {
		t.Errorf("新增站点没接上路由：%q", got)
	}

	// 重复目录：拒绝。
	if _, err := r.AddSite(site, ""); !errors.Is(err, ErrDuplicateSite) {
		t.Errorf("重复目录的错 = %v，期望 ErrDuplicateSite", err)
	}
	// 嵌套（两个方向）：拒绝。
	if _, err := r.AddSite(filepath.Join(site, "sub"), ""); !errors.Is(err, ErrNestedRoot) {
		t.Errorf("嵌套（在站点之下）的错 = %v，期望 ErrNestedRoot", err)
	}
	sub := mkRoot(t, main, "sub")
	if _, err := r.AddSite(sub, ""); !errors.Is(err, ErrNestedRoot) {
		t.Errorf("嵌套（在主目录之下）的错 = %v，期望 ErrNestedRoot", err)
	}

	// 指定标签可用就用；与已有标签冲突则自动加后缀。
	other := mkRoot(t, base, "other")
	m2, err := r.AddSite(other, "自定义")
	if err != nil {
		t.Fatalf("AddSite: %v", err)
	}
	if m2.Label != "自定义" {
		t.Errorf("指定标签 = %q，期望 自定义", m2.Label)
	}
	third := mkRoot(t, base, "third")
	m3, _ := r.AddSite(third, "自定义")
	if m3.Label != "自定义-2" {
		t.Errorf("标签冲突应加后缀，实际 %q", m3.Label)
	}
	// 已有的标签不能被后来的站点抢走（书签稳定）。
	if got, _ := r.Resolve("/自定义/a.txt"); got != filepath.Join(other, "a.txt") {
		t.Errorf("先来的站点被抢了标签：%q", got)
	}
}

// TestRemoveSite 覆盖移除：未知标签、主标签、以及删后不重编号。
func TestRemoveSite(t *testing.T) {
	base := t.TempDir()
	main := mkRoot(t, base, "main")
	a := mkRoot(t, base, "aaa")
	b := mkRoot(t, base, "bbb")
	r := mustRoots(t, main, a, b)

	if err := r.RemoveSite("nope"); !errors.Is(err, ErrUnknownSite) {
		t.Errorf("未知标签的错 = %v，期望 ErrUnknownSite", err)
	}
	if err := r.RemoveSite(r.Primary().Label); !errors.Is(err, ErrPrimarySite) {
		t.Errorf("移除主目录的错 = %v，期望 ErrPrimarySite", err)
	}

	// 删掉中间那个：幸存者的标签不能被重编号（书签必须还有效）。
	if err := r.RemoveSite("aaa"); err != nil {
		t.Fatalf("RemoveSite: %v", err)
	}
	if got := r.Labels(); len(got) != 1 || got[0] != "bbb" {
		t.Fatalf("标签 = %v，期望 [bbb]", got)
	}
	if got, _ := r.Resolve("/bbb/x"); got != filepath.Join(b, "x") {
		t.Errorf("幸存站点路由错了：%q", got)
	}
	// 已移除的标签：/aaa 退回主目录下的普通路径（不再是站点）。
	if got, err := r.Resolve("/aaa/x"); err != nil || got != filepath.Join(main, "aaa", "x") {
		t.Errorf("移除后 Resolve = %q, %v；期望退回主目录", got, err)
	}

	// 删掉最后一个：回到「只有主目录」的形状，换根重新可用。
	if err := r.RemoveSite("bbb"); err != nil {
		t.Fatalf("RemoveSite: %v", err)
	}
	if r.HasExtras() {
		t.Fatal("删光了还有 HasExtras")
	}
	if err := r.SetRoot(main); err != nil {
		t.Errorf("回到单根后 SetRoot 应可用: %v", err)
	}
}

// TestRemovedLabelNotRecycled 确认被移除的标签不会被后续分配复用 ——
// 否则一张旧书签会被悄悄指到另一个目录上。
func TestRemovedLabelNotRecycled(t *testing.T) {
	base := t.TempDir()
	main := mkRoot(t, base, "main")
	r := mustRoots(t, main)

	first := mkRoot(t, base, "data")
	m1, _ := r.AddSite(first, "site")
	if m1.Label != "site" {
		t.Fatalf("标签 = %q", m1.Label)
	}
	if err := r.RemoveSite("site"); err != nil {
		t.Fatal(err)
	}
	// 再挂一个别的目录、并请求同一个标签：不该拿到 site。
	other := mkRoot(t, base, "other")
	m2, _ := r.AddSite(other, "site")
	if m2.Label == "site" {
		t.Error("已移除的标签被复用了 —— 旧书签会指向新目录")
	}
}

// TestIsRoot 覆盖删除/移动的根保护要用的判定。
func TestIsRoot(t *testing.T) {
	base := t.TempDir()
	main := mkRoot(t, base, "main")
	site := mkRoot(t, base, "site")
	r := mustRoots(t, main, site)

	if !r.IsRoot(main) || !r.IsRoot(site) {
		t.Error("挂载根本身应当被认出来")
	}
	if r.IsRoot(filepath.Join(main, "sub")) {
		t.Error("根下面的普通目录不该算根")
	}
	if r.IsRoot(base) {
		t.Error("父目录不该算根")
	}
}

// TestRootsConcurrentAddRemoveAndRead 用 -race 跑：读（Resolve/Rel/IsRoot）
// 与增删站点并发时不能有数据竞争，也不能 panic。
func TestRootsConcurrentAddRemoveAndRead(t *testing.T) {
	base := t.TempDir()
	main := mkRoot(t, base, "main")
	writeFile(t, filepath.Join(main, "a.txt"), "m")
	r := mustRoots(t, main)

	dirs := make([]string, 0, 8)
	for i := 0; i < 8; i++ {
		dirs = append(dirs, mkRoot(t, base, "site"+string(rune('a'+i))))
	}

	var wg sync.WaitGroup
	stop := make(chan struct{})

	// 读者
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				_, _ = r.Resolve("/a.txt")
				_, _ = r.Rel(filepath.Join(main, "a.txt"))
				_ = r.IsRoot(main)
				_ = r.Labels()
			}
		}()
	}

	// 增删者
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i, d := range dirs {
			m, err := r.AddSite(d, "")
			if err != nil {
				continue
			}
			if i%2 == 0 {
				_ = r.RemoveSite(m.Label)
			}
		}
	}()

	wg.Wait()
	close(stop)
}

// writeFile 是本包测试里的小工具。
func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
