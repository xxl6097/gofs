package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// renameItem 造一条请求项，省得每个用例都写一遍字面量。
func renameItem(path, name string) renameItemJSON {
	return renameItemJSON{Path: path, Name: name}
}

// renameBatchResult 是一次批量重命名的观察结果。
type renameBatchResult struct {
	code int
	body string
	resp renameRespJSON
}

// doRenameBatch 发一次批量重命名请求并解析响应。
//
// 415/401/403/405 这类拒绝是纯文本，解析不出来时不报错 —— 让用例自己去断
// code 与 body（那些路径下本来就没有逐项原因可看）。
func doRenameBatch(t *testing.T, s *Server, hdr map[string]string, items ...renameItemJSON) renameBatchResult {
	t.Helper()
	body, err := json.Marshal(renameReqJSON{Items: items})
	if err != nil {
		t.Fatalf("序列化请求失败: %v", err)
	}
	h := map[string]string{"Content-Type": "application/json"}
	for k, v := range hdr {
		h[k] = v
	}
	got := do(t, s, http.MethodPost, "/fs/rename-batch", bytes.NewReader(body), h)
	out := renameBatchResult{code: got.code, body: got.body}
	_ = json.Unmarshal([]byte(got.body), &out.resp)
	return out
}

// reasonsOf 把逐项原因拼成一行，方便断言里看得见全部原因。
func reasonsOf(items []renameItemErrJSON) string {
	var parts []string
	for _, it := range items {
		parts = append(parts, it.Path+"："+it.Reason)
	}
	return strings.Join(parts, "；")
}

// renameFixture 造一个带若干文件的目录。
func renameFixture(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for name, content := range files {
		writeFile(t, filepath.Join(root, name), []byte(content))
	}
	return root
}

// readRootFile 读根目录下的一个文件，读不到直接失败。
func readRootFile(t *testing.T, root, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(root, name))
	if err != nil {
		t.Fatalf("读 %s 失败: %v", name, err)
	}
	return string(b)
}

// requireAbsent 断言某个文件不在（改名之后原名不该留下）。
func requireAbsent(t *testing.T, root, name string) {
	t.Helper()
	if _, err := os.Stat(filepath.Join(root, name)); err == nil {
		t.Fatalf("%s 仍然存在（应当已被改走）", name)
	}
}

// --------------------------------------------------------------------- 正常路径

func TestRenameBatchBasic(t *testing.T) {
	root := renameFixture(t, map[string]string{"a.txt": "A", "b.txt": "B", "keep.txt": "K"})
	s := newTestServer(t, testConfig(t, root, "-A"))

	got := doRenameBatch(t, s, nil, renameItem("/a.txt", "x.txt"), renameItem("/b.txt", "y.txt"))
	if got.code != http.StatusOK || !got.resp.OK {
		t.Fatalf("返回 %d ok=%v：%s", got.code, got.resp.OK, got.body)
	}
	if len(got.resp.Renamed) != 2 {
		t.Fatalf("renamed = %v, 期望 2 项", got.resp.Renamed)
	}
	// 内容跟着走：改名不是「新建一个空的再把老的删掉」。
	if c := readRootFile(t, root, "x.txt"); c != "A" {
		t.Errorf("x.txt 内容 = %q, 期望 A", c)
	}
	if c := readRootFile(t, root, "y.txt"); c != "B" {
		t.Errorf("y.txt 内容 = %q, 期望 B", c)
	}
	requireAbsent(t, root, "a.txt")
	requireAbsent(t, root, "b.txt")
	if c := readRootFile(t, root, "keep.txt"); c != "K" {
		t.Errorf("没参与的文件被动了：%q", c)
	}
	// from/to 是前端要用的 URL 路径（它据此把结果对回预览行、并保持选中）。
	if got.resp.Renamed[0].To != "/x.txt" {
		t.Errorf("to = %q, 期望 /x.txt", got.resp.Renamed[0].To)
	}
}

// TestRenameBatchSwap 是「互相占位」那一档：A→B 与 B→A 谁都不能先动。
//
// 逐个发 MOVE 时这种请求必然失败（第一个就撞上「目标已存在」）。这里必须借道
// 临时名才能做对，所以断言的是**内容**：互换之后两个名字上的内容应当对调，
// 而不是变成「都变成同一份」或者丢了一份。
func TestRenameBatchSwap(t *testing.T) {
	root := renameFixture(t, map[string]string{"a.txt": "A", "b.txt": "B"})
	s := newTestServer(t, testConfig(t, root, "-A"))

	got := doRenameBatch(t, s, nil, renameItem("/a.txt", "b.txt"), renameItem("/b.txt", "a.txt"))
	if got.code != http.StatusOK || !got.resp.OK {
		t.Fatalf("返回 %d ok=%v：%s", got.code, got.resp.OK, got.body)
	}
	if c := readRootFile(t, root, "a.txt"); c != "B" {
		t.Errorf("互换后 a.txt 内容 = %q, 期望 B", c)
	}
	if c := readRootFile(t, root, "b.txt"); c != "A" {
		t.Errorf("互换后 b.txt 内容 = %q, 期望 A", c)
	}
	// 临时名不能留在目录里。
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".gofs") {
			t.Errorf("目录里留下了临时文件：%s", e.Name())
		}
	}
}

// TestRenameBatchChain 是「链式让位」：A→B 必须等 B→C 先把 B 腾出来。
func TestRenameBatchChain(t *testing.T) {
	root := renameFixture(t, map[string]string{"a.txt": "A", "b.txt": "B"})
	s := newTestServer(t, testConfig(t, root, "-A"))

	got := doRenameBatch(t, s, nil, renameItem("/a.txt", "b.txt"), renameItem("/b.txt", "c.txt"))
	if got.code != http.StatusOK || !got.resp.OK {
		t.Fatalf("返回 %d ok=%v：%s", got.code, got.resp.OK, got.body)
	}
	if c := readRootFile(t, root, "b.txt"); c != "A" {
		t.Errorf("b.txt 内容 = %q, 期望 A（原 a.txt）", c)
	}
	if c := readRootFile(t, root, "c.txt"); c != "B" {
		t.Errorf("c.txt 内容 = %q, 期望 B（原 b.txt）", c)
	}
	requireAbsent(t, root, "a.txt")
}

// TestRenameBatchCaseOnly 只改大小写。
//
// 在 macOS / Windows 这类大小写不敏感的文件系统上，目标是「源文件自己」，
// 校验时必须靠 os.SameFile 放行、执行时必须借道临时名（直接 Rename 不可靠）。
// 在大小写敏感的文件系统上它只是一次普通的改名 —— 两种都该成功。
func TestRenameBatchCaseOnly(t *testing.T) {
	root := renameFixture(t, map[string]string{"a.txt": "A"})
	s := newTestServer(t, testConfig(t, root, "-A"))

	got := doRenameBatch(t, s, nil, renameItem("/a.txt", "A.txt"))
	if got.code != http.StatusOK || !got.resp.OK {
		t.Fatalf("返回 %d ok=%v：%s", got.code, got.resp.OK, got.body)
	}
	if c := readRootFile(t, root, "A.txt"); c != "A" {
		t.Errorf("A.txt 内容 = %q, 期望 A", c)
	}
}

func TestRenameBatchDir(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "old", "inner.txt"), []byte("内"))
	s := newTestServer(t, testConfig(t, root, "-A"))

	got := doRenameBatch(t, s, nil, renameItem("/old", "new"))
	if got.code != http.StatusOK || !got.resp.OK {
		t.Fatalf("返回 %d ok=%v：%s", got.code, got.resp.OK, got.body)
	}
	if c := readRootFile(t, root, filepath.Join("new", "inner.txt")); c != "内" {
		t.Errorf("目录内容没跟着走：%q", c)
	}
	requireAbsent(t, root, "old")
}

// TestRenameBatchSkipsUnchanged 名称没变的项不算错误，但要如实回报成 skipped。
func TestRenameBatchSkipsUnchanged(t *testing.T) {
	root := renameFixture(t, map[string]string{"a.txt": "A", "b.txt": "B"})
	s := newTestServer(t, testConfig(t, root, "-A"))

	got := doRenameBatch(t, s, nil, renameItem("/a.txt", "a.txt"), renameItem("/b.txt", "c.txt"))
	if got.code != http.StatusOK || !got.resp.OK {
		t.Fatalf("返回 %d ok=%v：%s", got.code, got.resp.OK, got.body)
	}
	if len(got.resp.Skipped) != 1 || !strings.Contains(reasonsOf(got.resp.Skipped), "名称未改变") {
		t.Errorf("skipped = %s", reasonsOf(got.resp.Skipped))
	}
	if c := readRootFile(t, root, "c.txt"); c != "B" {
		t.Errorf("c.txt 内容 = %q, 期望 B", c)
	}

	// 全都是「名称未改变」时没有活可干，如实说，而不是回一个空的成功。
	got2 := doRenameBatch(t, s, nil, renameItem("/a.txt", "a.txt"))
	if got2.code != http.StatusBadRequest || !strings.Contains(got2.resp.Message, "名称未改变") {
		t.Errorf("返回 %d：%s", got2.code, got2.body)
	}
}

// --------------------------------------------------------------------- 整批不动

// TestRenameBatchConflictAtomic 是「整批不动」的核心断言。
//
// 一项撞上**没参与本批次**的已有文件时必须整批拒掉：合法的那些也得原样留着，
// 否则用户拿到的是一个改了一半的目录，比整体失败难收拾得多。
func TestRenameBatchConflictAtomic(t *testing.T) {
	root := renameFixture(t, map[string]string{"a.txt": "A", "b.txt": "B", "keep.txt": "K"})
	s := newTestServer(t, testConfig(t, root, "-A"))

	got := doRenameBatch(t, s, nil,
		renameItem("/b.txt", "c.txt"),
		renameItem("/a.txt", "keep.txt"))
	if got.code != http.StatusBadRequest {
		t.Fatalf("返回 %d, 期望 400：%s", got.code, got.body)
	}
	if len(got.resp.Errors) != 1 || got.resp.Errors[0].Reason != "目标同名已存在（未覆盖）" {
		t.Errorf("errors = %s", reasonsOf(got.resp.Errors))
	}
	if !strings.Contains(got.resp.Message, "未做任何改动") {
		t.Errorf("message = %q", got.resp.Message)
	}
	// 合法的那一项也不能改 —— 这正是这个接口存在的理由。
	requireAbsent(t, root, "c.txt")
	if c := readRootFile(t, root, "b.txt"); c != "B" {
		t.Errorf("b.txt 被动过了：%q", c)
	}
	if c := readRootFile(t, root, "keep.txt"); c != "K" {
		t.Errorf("keep.txt 被动过了：%q", c)
	}
}

// TestRenameBatchOneBadNameKillsAll 一项名字不合法，整批一条都不改。
func TestRenameBatchOneBadNameKillsAll(t *testing.T) {
	root := renameFixture(t, map[string]string{"a.txt": "A", "b.txt": "B"})
	s := newTestServer(t, testConfig(t, root, "-A", "--max-name-bytes", "32"))

	long := strings.Repeat("x", 40)
	got := doRenameBatch(t, s, nil, renameItem("/a.txt", "ok.txt"), renameItem("/b.txt", long))
	if got.code != http.StatusBadRequest {
		t.Fatalf("返回 %d, 期望 400：%s", got.code, got.body)
	}
	if !strings.Contains(reasonsOf(got.resp.Errors), "文件名过长") {
		t.Errorf("errors = %s", reasonsOf(got.resp.Errors))
	}
	requireAbsent(t, root, "ok.txt")
	if c := readRootFile(t, root, "a.txt"); c != "A" {
		t.Errorf("a.txt 被动过了：%q", c)
	}
}

// TestRenameBatchRejects 覆盖各种「请求本身不合法」。
func TestRenameBatchRejects(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "a.txt"), []byte("A"))
	writeFile(t, filepath.Join(root, "b.txt"), []byte("B"))
	writeFile(t, filepath.Join(root, "sub", "inner.txt"), []byte("I"))
	s := newTestServer(t, testConfig(t, root, "-A", "--max-name-bytes", "32"))
	long := strings.Repeat("x", 40)

	cases := []struct {
		name  string
		items []renameItemJSON
		want  string
	}{
		{"新名带斜杠", []renameItemJSON{renameItem("/a.txt", "sub/a.txt")}, "不能包含路径分隔符"},
		{"新名为空", []renameItemJSON{renameItem("/a.txt", "")}, "不能为空"},
		{"新名是 ..", []renameItemJSON{renameItem("/a.txt", "..")}, "新名称非法"},
		{"新名超长", []renameItemJSON{renameItem("/a.txt", long)}, "文件名过长"},
		{"路径为空", []renameItemJSON{renameItem("", "x.txt")}, "缺少路径"},
		{"路径是根", []renameItemJSON{renameItem("/", "x.txt")}, "缺少路径"},
		{"源不存在", []renameItemJSON{renameItem("/nope.txt", "x.txt")}, "源已不存在"},
		{"两项改成同名", []renameItemJSON{
			renameItem("/a.txt", "same.txt"), renameItem("/b.txt", "same.txt")}, "重名"},
		// 同一个源出现两次：两条指令互相打架，也不能被误报成「目录与其子项」。
		{"同一个路径出现两次", []renameItemJSON{
			renameItem("/a.txt", "x.txt"), renameItem("/a.txt", "y.txt")}, "出现了多次"},
		{"目录与它的子项同批", []renameItemJSON{
			renameItem("/sub", "sub2"), renameItem("/sub/inner.txt", "inner2.txt")},
			"不能同时重命名目录和它里面的条目"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := doRenameBatch(t, s, nil, tc.items...)
			if got.code != http.StatusBadRequest {
				t.Fatalf("返回 %d, 期望 400：%s", got.code, got.body)
			}
			if reasons := reasonsOf(got.resp.Errors); !strings.Contains(reasons, tc.want) {
				t.Errorf("errors = %s, 期望包含 %q", reasons, tc.want)
			}
		})
	}

	// 磁盘必须原样：上面每一条都被拒了，一个名字都不该变。
	for _, name := range []string{"a.txt", "b.txt", "sub/inner.txt"} {
		if _, err := os.Stat(filepath.Join(root, name)); err != nil {
			t.Errorf("%s 不见了：%v", name, err)
		}
	}
}

// TestRenameBatchBadRequestShape 项数与请求体本身的边界。
func TestRenameBatchBadRequestShape(t *testing.T) {
	root := renameFixture(t, map[string]string{"a.txt": "A"})
	s := newTestServer(t, testConfig(t, root, "-A"))

	// 空 items
	got := doRenameBatch(t, s, nil)
	if got.code != http.StatusBadRequest || !strings.Contains(got.resp.Message, "没有要重命名的项") {
		t.Errorf("空 items：%d %s", got.code, got.body)
	}

	// 超过上限：整批拒，而不是悄悄只做前 N 项。
	items := make([]renameItemJSON, maxRenameBatch+1)
	for i := range items {
		items[i] = renameItem("/a.txt", "x.txt")
	}
	got = doRenameBatch(t, s, nil, items...)
	if got.code != http.StatusBadRequest || !strings.Contains(got.resp.Message, "一次最多重命名") {
		t.Errorf("超上限：%d %s", got.code, got.body)
	}

	// 请求体不是 JSON / 为空
	for _, body := range []string{"{", ""} {
		r := do(t, s, http.MethodPost, "/fs/rename-batch", strings.NewReader(body),
			map[string]string{"Content-Type": "application/json"})
		if r.code != http.StatusBadRequest {
			t.Errorf("body=%q 返回 %d, 期望 400：%s", body, r.code, r.body)
		}
	}

	// 只接受 POST
	if r := do(t, s, http.MethodGet, "/fs/rename-batch", nil, nil); r.code != http.StatusMethodNotAllowed {
		t.Errorf("GET 返回 %d, 期望 405", r.code)
	}
}

// --------------------------------------------------------------------- 权限与范围

// TestRenameBatchRequiresWrite 只读账号改不了名，且源文件原样留着。
func TestRenameBatchRequiresWrite(t *testing.T) {
	root := renameFixture(t, map[string]string{"a.txt": "A"})
	s := newTestServer(t, testConfig(t, root, "-A", "-a", "ro:ro@/:r"))
	hdr := map[string]string{"Authorization": basicAuth("ro", "ro")}

	got := doRenameBatch(t, s, hdr, renameItem("/a.txt", "x.txt"))
	if got.code != http.StatusForbidden {
		t.Fatalf("返回 %d, 期望 403：%s", got.code, got.body)
	}
	if c := readRootFile(t, root, "a.txt"); c != "A" {
		t.Errorf("a.txt 被动过了：%q", c)
	}
}

// TestRenameBatchScopedAccount 只被授予 /docs 的账号：改得了 /docs 里的名字，
// 越界到别处的项会被拒 —— 而它不该因为「对根目录没有权限」而被整体挡在门外。
func TestRenameBatchScopedAccount(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "docs", "a.txt"), []byte("A"))
	writeFile(t, filepath.Join(root, "secret", "s.txt"), []byte("S"))
	s := newTestServer(t, testConfig(t, root, "-A", "-a", "bob:pw@/docs:rw"))
	hdr := map[string]string{"Authorization": basicAuth("bob", "pw")}

	got := doRenameBatch(t, s, hdr, renameItem("/docs/a.txt", "b.txt"))
	if got.code != http.StatusOK || !got.resp.OK {
		t.Fatalf("返回 %d ok=%v：%s", got.code, got.resp.OK, got.body)
	}
	if c := readRootFile(t, root, filepath.Join("docs", "b.txt")); c != "A" {
		t.Errorf("改名后的内容 = %q", c)
	}

	// 越界的项：没有权限的路径会被 authorize 挡下（与 MOVE 同一个口径）。
	out := doRenameBatch(t, s, hdr, renameItem("/secret/s.txt", "t.txt"))
	if out.code != http.StatusUnauthorized && out.code != http.StatusForbidden {
		t.Errorf("越界返回 %d, 期望 401/403：%s", out.code, out.body)
	}
	if c := readRootFile(t, root, filepath.Join("secret", "s.txt")); c != "S" {
		t.Errorf("越界的源被动过了：%q", c)
	}
}

// TestRenameBatchNoUploadFlag 没开 --allow-upload 时改名整批禁用（与 MOVE 同一道闸门）。
func TestRenameBatchNoUploadFlag(t *testing.T) {
	root := renameFixture(t, map[string]string{"a.txt": "A"})
	s := newTestServer(t, testConfig(t, root))

	got := doRenameBatch(t, s, nil, renameItem("/a.txt", "x.txt"))
	if got.code != http.StatusForbidden || !strings.Contains(got.body, "未开启重命名") {
		t.Fatalf("返回 %d：%s", got.code, got.body)
	}
	if c := readRootFile(t, root, "a.txt"); c != "A" {
		t.Errorf("a.txt 被动过了：%q", c)
	}
}

// TestRenameBatchRoots 站点根不能被改名，也不能被当成别的项的目标。
func TestRenameBatchRoots(t *testing.T) {
	s, _, backupDir := multiRootFixture(t, "-A")

	// /backup 是一个附加站点的挂载根：它代表的是**另一个服务目录**。
	got := doRenameBatch(t, s, nil, renameItem("/backup", "backup2"))
	if got.code != http.StatusBadRequest || !strings.Contains(reasonsOf(got.resp.Errors), "服务根目录") {
		t.Errorf("返回 %d：%s", got.code, got.body)
	}
	if _, err := os.Stat(backupDir); err != nil {
		t.Errorf("站点目录被动过了：%v", err)
	}
}

// TestRenameBatchOutOfScopeViaSymlink 软链指向服务根之外时，源被判越界。
func TestRenameBatchOutOfScopeViaSymlink(t *testing.T) {
	outside := t.TempDir()
	writeFile(t, filepath.Join(outside, "target.txt"), []byte("外面的"))
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "a.txt"), []byte("A"))
	if err := os.Symlink(outside, filepath.Join(root, "link")); err != nil {
		t.Skipf("本环境不支持创建软链：%v", err)
	}
	s := newTestServer(t, testConfig(t, root, "-A"))

	got := doRenameBatch(t, s, nil, renameItem("/link/target.txt", "moved.txt"))
	if got.code != http.StatusBadRequest || !strings.Contains(reasonsOf(got.resp.Errors), "超出服务范围") {
		t.Fatalf("返回 %d：%s", got.code, got.body)
	}
	if c := readRootFile(t, outside, "target.txt"); c != "外面的" {
		t.Errorf("服务根之外的文件被动过了：%q", c)
	}
}

// TestRenameBatchDirConflict 目录改成已有目录的名字也要被拒，且两个目录都完好。
//
// 不复用文件那份冲突用例是因为目录的失败方式不同：os.Rename 撞上一个非空目录
// 会报 ENOTEMPTY —— 那句内核错误对用户毫无意义，所以必须在校验趟就挡住。
func TestRenameBatchDirConflict(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "sub1", "a.txt"), []byte("A"))
	writeFile(t, filepath.Join(root, "sub2", "b.txt"), []byte("B"))
	s := newTestServer(t, testConfig(t, root, "-A"))

	got := doRenameBatch(t, s, nil, renameItem("/sub1", "sub2"))
	if got.code != http.StatusBadRequest || !strings.Contains(reasonsOf(got.resp.Errors), "目标同名已存在") {
		t.Fatalf("返回 %d：%s", got.code, got.body)
	}
	if c := readRootFile(t, root, filepath.Join("sub1", "a.txt")); c != "A" {
		t.Errorf("sub1 被动过了：%q", c)
	}
	if c := readRootFile(t, root, filepath.Join("sub2", "b.txt")); c != "B" {
		t.Errorf("sub2 被动过了：%q", c)
	}
}

// TestMoveRejectsOverlongName 是顺手补上的缺口：单条 MOVE 早先不校验名称长度，
// 超长名会被文件系统拒成 ENAMETOOLONG，透出去就是 500。
func TestMoveRejectsOverlongName(t *testing.T) {
	root := renameFixture(t, map[string]string{"a.txt": "A"})
	s := newTestServer(t, testConfig(t, root, "-A", "--max-name-bytes", "32"))

	long := strings.Repeat("x", 40) + ".txt"
	got := move(t, s, "/a.txt", "/"+long, false)
	if got.code != http.StatusBadRequest {
		t.Fatalf("返回 %d, 期望 400（不该是 500）：%s", got.code, got.body)
	}
	if !strings.Contains(got.body, "文件名过长") {
		t.Errorf("错误信息没点明原因：%s", got.body)
	}
	if c := readRootFile(t, root, "a.txt"); c != "A" {
		t.Errorf("被拒的移动把源文件弄没了：%q", c)
	}
}
