package server

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// move 发一次 MOVE 请求，dest 是 URL 路径（会拼成绝对 URL）。
func move(t *testing.T, s *Server, src, dest string, overwrite bool) resp {
	t.Helper()
	h := map[string]string{"Destination": testHost + dest}
	if overwrite {
		h["Overwrite"] = "T"
	}
	return do(t, s, MethodMove, testHost+src, nil, h)
}

// TestMoveIntoOwnSubtree 是这次补的防护的核心。
//
// 把目录移进它自己的子目录必须被挡掉。改之前这里返回的是 500
// 加一句裸内核错误（"rename ...: invalid argument"）——
// 那是 os.Rename 的 EINVAL 直接透出来的，用户看不懂。
//
// 更危险的是那条跨设备兜底：它做的是「复制到目标、再删掉源」，
// 一旦被触发就是把自己的内容复制进自己里面、然后把源删掉。
func TestMoveIntoOwnSubtree(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "a", "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	const content = "重要数据"
	if err := os.WriteFile(filepath.Join(root, "a", "keep.txt"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	s := newTestServer(t, testConfig(t, root, "-A"))

	cases := []struct {
		name string
		dest string
	}{
		{"移进直接子目录", "/a/sub"},
		{"移进更深的后代", "/a/sub/deeper"},
		{"移到自己身上", "/a"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := move(t, s, "/a", tc.dest, false)
			if got.code != http.StatusBadRequest {
				t.Fatalf("返回 %d，期望 400：%s", got.code, got.body)
			}
			// 报错得是人话，不是 errno。
			if !strings.Contains(got.body, "不能把目录移动到它自己里面") {
				t.Errorf("错误信息不明确：%s", got.body)
			}
			// 关键：源必须完好。这条防的是「先复制再删源」那种回归。
			got2, err := os.ReadFile(filepath.Join(root, "a", "keep.txt"))
			if err != nil {
				t.Fatalf("源文件被弄丢了：%v", err)
			}
			if string(got2) != content {
				t.Fatalf("源文件内容被改了：%q", got2)
			}
		})
	}
}

// TestMoveToSiblingAndChild 确认正当的移动没被上面那条防护误伤。
func TestMoveToSiblingAndChild(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "a"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "b"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "a", "f.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "loose.txt"), []byte("y"), 0o644); err != nil {
		t.Fatal(err)
	}
	s := newTestServer(t, testConfig(t, root, "-A"))

	t.Run("文件移到兄弟目录", func(t *testing.T) {
		if got := move(t, s, "/loose.txt", "/b/loose.txt", false); got.code != http.StatusCreated {
			t.Fatalf("返回 %d，期望 201：%s", got.code, got.body)
		}
		if _, err := os.Stat(filepath.Join(root, "b", "loose.txt")); err != nil {
			t.Fatalf("目标不存在：%v", err)
		}
		if _, err := os.Stat(filepath.Join(root, "loose.txt")); err == nil {
			t.Fatal("源还在")
		}
	})

	t.Run("目录移到兄弟目录，子树跟着走", func(t *testing.T) {
		if err := os.WriteFile(filepath.Join(root, "a", "deep.txt"), []byte("z"), 0o644); err != nil {
			t.Fatal(err)
		}
		if got := move(t, s, "/a", "/b/a", false); got.code != http.StatusCreated {
			t.Fatalf("返回 %d，期望 201：%s", got.code, got.body)
		}
		for _, rel := range []string{"b/a/f.txt", "b/a/deep.txt"} {
			if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(rel))); err != nil {
				t.Errorf("%s 没跟着走：%v", rel, err)
			}
		}
		if _, err := os.Stat(filepath.Join(root, "a")); err == nil {
			t.Error("源目录还在")
		}
	})
}

// TestMoveDestinationExists 确认目标已存在时默认不覆盖，且源文件保留。
//
// 静默覆盖用户数据比失败更糟，所以 Overwrite 默认是 F。
func TestMoveDestinationExists(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "b"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "f.txt"), []byte("新的"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "b", "f.txt"), []byte("已有的"), 0o644); err != nil {
		t.Fatal(err)
	}
	s := newTestServer(t, testConfig(t, root, "-A"))

	got := move(t, s, "/f.txt", "/b/f.txt", false)
	if got.code != http.StatusPreconditionFailed {
		t.Fatalf("返回 %d，期望 412：%s", got.code, got.body)
	}
	// 源必须还在 —— 覆盖失败后把源删掉是最糟的失败模式。
	if _, err := os.Stat(filepath.Join(root, "f.txt")); err != nil {
		t.Fatal("冲突后源文件没了")
	}
	// 目标内容不能被动过。
	body, err := os.ReadFile(filepath.Join(root, "b", "f.txt"))
	if err != nil || string(body) != "已有的" {
		t.Fatalf("目标被覆盖了：%q %v", body, err)
	}

	// 显式要求覆盖时才替换。
	if got := move(t, s, "/f.txt", "/b/f.txt", true); got.code != http.StatusCreated {
		t.Fatalf("带 Overwrite 返回 %d，期望 201：%s", got.code, got.body)
	}
	body, _ = os.ReadFile(filepath.Join(root, "b", "f.txt"))
	if string(body) != "新的" {
		t.Fatalf("覆盖后内容 = %q，期望「新的」", body)
	}
}

// TestMoveDotDotStaysInside 确认 URL 里的 .. 逃不出去。
//
// 注意这**不是**靠 403 挡的：CleanURLPath 会先把 /../x 归约成 /x，
// 所以它根本没被当成越界，而是老老实实落在根目录里。
// 把这个行为钉住 —— 哪天有人把清洗去掉，这里会立刻变红。
func TestMoveDotDotStaysInside(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "f.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	s := newTestServer(t, testConfig(t, root, "-A"))

	if got := move(t, s, "/f.txt", "/../escaped.txt", false); got.code != http.StatusCreated {
		t.Fatalf("返回 %d，期望 201：%s", got.code, got.body)
	}
	// 关键：落在服务根**里面**，而不是外面。
	if _, err := os.Stat(filepath.Join(root, "escaped.txt")); err != nil {
		t.Fatalf("文件没落在服务根内：%v", err)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(root), "escaped.txt")); err == nil {
		t.Fatal("文件逃到了服务根之外")
	}
}

// TestMoveOutOfScopeViaSymlink 覆盖真正的越界路径。
//
// URL 里的 .. 会被清洗掉，所以唯一能触发 ErrEscaped 的现实途径是软链 ——
// 这也正是那条分支存在的理由：目标要报「超出服务范围」，
// 而不是和「路径写错了」共用一句笼统的提示。
func TestMoveOutOfScopeViaSymlink(t *testing.T) {
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "target.txt"), []byte("外面的"), 0o644); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "f.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	// 指向服务根之外的软链；默认 allowSymlink=false，解析时会被判越界。
	if err := os.Symlink(outside, filepath.Join(root, "link")); err != nil {
		t.Skipf("本环境不支持创建软链：%v", err)
	}
	s := newTestServer(t, testConfig(t, root, "-A"))

	got := move(t, s, "/f.txt", "/link/target.txt", false)
	if got.code != http.StatusForbidden {
		t.Fatalf("返回 %d，期望 403：%s", got.code, got.body)
	}
	if !strings.Contains(got.body, "超出服务范围") {
		t.Errorf("错误信息没点明越界：%s", got.body)
	}
	// 源文件必须还在，外面的目标也不能被动。
	if _, err := os.Stat(filepath.Join(root, "f.txt")); err != nil {
		t.Fatal("越界被拒后源文件没了")
	}
	body, _ := os.ReadFile(filepath.Join(outside, "target.txt"))
	if string(body) != "外面的" {
		t.Fatalf("服务根之外的文件被改动了：%q", body)
	}
}

// TestMoveRootRejected 确认服务根不能被移走。
func TestMoveRootRejected(t *testing.T) {
	root := t.TempDir()
	s := newTestServer(t, testConfig(t, root, "-A"))

	got := move(t, s, "/", "/elsewhere", false)
	if got.code != http.StatusForbidden {
		t.Fatalf("返回 %d，期望 403：%s", got.code, got.body)
	}
}

// TestInsideOrSame 是路径关系判断的单元版。
//
// 平台路径分隔符不同，靠字符串前缀判「在不在里面」很容易漏
// （/a/bc 会被误判为在 /a/b 里），所以这里逐条钉住。
func TestInsideOrSame(t *testing.T) {
	sep := string(filepath.Separator)
	base := sep + "root"
	cases := []struct {
		src, dst string
		want     bool
	}{
		{base, base, true},
		{base, filepath.Join(base, "sub"), true},
		{base, filepath.Join(base, "sub", "deep"), true},
		{filepath.Join(base, "a"), filepath.Join(base, "a"), true},
		{filepath.Join(base, "a"), filepath.Join(base, "a", "b"), true},
		// 兄弟目录不算
		{filepath.Join(base, "a"), filepath.Join(base, "b"), false},
		// 前缀相同但不是子路径 —— 这条最容易写错
		{filepath.Join(base, "a"), filepath.Join(base, "ab"), false},
		// 父目录不算
		{filepath.Join(base, "a"), base, false},
	}
	for _, tc := range cases {
		if got := insideOrSame(tc.src, tc.dst); got != tc.want {
			t.Errorf("insideOrSame(%q, %q) = %v，期望 %v", tc.src, tc.dst, got, tc.want)
		}
	}
}
