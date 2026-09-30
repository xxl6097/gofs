package server

import (
	"bytes"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// createKeyUsage 创建密钥并返回响应里的 usage 示例与明文。
func createKeyUsage(t *testing.T, s *Server, scope string) (map[string]string, string) {
	t.Helper()
	payload, _ := json.Marshal(map[string]any{
		"name": "用法示例", "scope": scope, "ttl_seconds": 0,
	})
	r := do(t, s, http.MethodPost, "/__gofs__/keys", bytes.NewReader(payload),
		map[string]string{"Content-Type": "application/json"})
	if r.code != http.StatusCreated {
		t.Fatalf("创建密钥状态码 = %d；%s", r.code, r.body)
	}
	var d struct {
		Token string            `json:"token"`
		Usage map[string]string `json:"usage"`
	}
	if err := json.Unmarshal([]byte(r.body), &d); err != nil {
		t.Fatalf("解析失败: %v\n%s", err, r.body)
	}
	return d.Usage, d.Token
}

func keysOf(m map[string]string) string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return strings.Join(out, ",")
}

// TestKeyUsageIncludesBatchScript 确认创建密钥时会给出批量上传示例。
//
// 这是「生成密钥后照抄就能用」的那条路径，示例里的 token 与目标目录
// 必须是这一把密钥的真实值 —— 拼错了用户会拿到一条永远 401 的命令。
func TestKeyUsageIncludesBatchScript(t *testing.T) {
	root := t.TempDir()
	s := newTestServer(t, testConfig(t, root, "-A"))

	usage, token := createKeyUsage(t, s, "/docs")

	script, ok := usage["script"]
	if !ok {
		t.Fatalf("usage 里没有 script 示例，现有键：%s", keysOf(usage))
	}
	// 加新示例时别把老的挤掉。
	for _, k := range []string{"header", "query", "scp", "script"} {
		if _, ok := usage[k]; !ok {
			t.Errorf("usage 缺少 %q，现有键：%s", k, keysOf(usage))
		}
	}

	if !strings.Contains(script, token) {
		t.Errorf("批量示例里没有本次的 token：\n%s", script)
	}
	// 示例换成了「一键脚本」形态：一条命令从服务端取回填好密钥的脚本再执行。
	// 端点路径与携带密钥的方式都要在，少了任何一个照抄都会失败。
	for _, want := range []string{"/up?key=", "curl", "bash <("} {
		if !strings.Contains(script, want) {
			t.Errorf("一键脚本示例里缺少 %q：\n%s", want, script)
		}
	}
	// 密钥只能写进自己的范围，而这条命令本身看不出落点 ——
	// 所以示例必须把范围写出来，否则用户只能靠猜。
	if !strings.Contains(script, "/docs") {
		t.Errorf("示例没说明文件会落到哪个范围（/docs）：\n%s", script)
	}
}

// TestKeyUsageScriptForRootScope 确认根范围的密钥落点是服务根。
func TestKeyUsageScriptForRootScope(t *testing.T) {
	root := t.TempDir()
	s := newTestServer(t, testConfig(t, root, "-A"))

	usage, _ := createKeyUsage(t, s, "/")
	script := usage["script"]
	if !strings.Contains(script, "<服务地址>/") {
		t.Errorf("根范围密钥的端点地址应当是 <服务地址>/：\n%s", script)
	}
	if !strings.Contains(script, "落到 /") {
		t.Errorf("没说明根范围的落点：\n%s", script)
	}
}

// TestKeyUsageScriptEndpointWorks 把「示例」和「端点」接起来：
// 照示例里的地址去请求，必须真的拿到脚本。
//
// 上面那条只断言字符串长得对 —— 可示例里拼错一个路径，
// 字符串断言照样过，用户却拿到 404。
func TestKeyUsageScriptEndpointWorks(t *testing.T) {
	root := t.TempDir()
	s := newTestServer(t, testConfig(t, root, "-A", "--allow-upload"))

	usage, token := createKeyUsage(t, s, "/docs")
	script := usage["script"]

	// 从示例里抠出端点路径（/up?key=...），按它去请求。
	i := strings.Index(script, upScriptName)
	if i < 0 {
		t.Fatalf("示例里没有端点路径 %s：\n%s", upScriptName, script)
	}
	target := script[i:]
	if j := strings.IndexAny(target, " \n\")"); j >= 0 {
		target = target[:j]
	}

	got := fetchScript(t, s, target, nil)
	if got.code != http.StatusOK {
		t.Fatalf("照示例请求端点返回 %d，期望 200：%s", got.code, got.body)
	}
	if !strings.Contains(got.body, "KEY='"+token+"'") {
		t.Error("拿到的脚本里不是这把密钥")
	}
}

// TestScopeDir 覆盖「范围 → 上传目标目录」的换算。
func TestScopeDir(t *testing.T) {
	cases := map[string]string{
		"":       "/",
		"/":      "/",
		"/docs":  "/docs/",
		"/docs/": "/docs/",
		"/a/b":   "/a/b/",
		"/a/b/":  "/a/b/",
	}
	for in, want := range cases {
		if got := scopeDir(in); got != want {
			t.Errorf("scopeDir(%q) = %q，期望 %q", in, got, want)
		}
	}
}

// postForm 用 multipart 表单上传若干文件，可带一个 path 字段。
func postForm(t *testing.T, s *Server, urlPath, token, pathField string, files map[string]string) resp {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	for name, content := range files {
		fw, err := mw.CreateFormFile("file", name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := fw.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	if pathField != "" {
		if err := mw.WriteField("path", pathField); err != nil {
			t.Fatal(err)
		}
	}
	if err := mw.Close(); err != nil {
		t.Fatal(err)
	}
	hdr := map[string]string{"Content-Type": mw.FormDataContentType()}
	if token != "" {
		hdr["X-Gofs-Upload-Key"] = token
	}
	return do(t, s, http.MethodPost, urlPath, &buf, hdr)
}

// TestKeyBatchUploadPreservesLayout 是这次文档所描述能力的端到端断言：
// 照示例那样用 path 字段上传，文件要带着层级落盘。
//
// 只断言「示例字符串长什么样」不够 —— 真正要保证的是照着做确实能成。
func TestKeyBatchUploadPreservesLayout(t *testing.T) {
	root := t.TempDir()
	s := newTestServer(t, testConfig(t, root, "-A", "--allow-upload"))
	_, token := createKey(t, s, "批量", "/", 0)

	r := postForm(t, s, "/", token, "apps/web", map[string]string{
		"index.html": "<h1>hi</h1>",
		"top.txt":    "top",
	})
	if r.code < 200 || r.code >= 300 {
		t.Fatalf("批量上传返回 %d：%s", r.code, r.body)
	}

	// 两个文件都落在 path 指定的子目录里。
	// 服务端会把多部分文件名压平到基本名，层级只能来自 path ——
	// 这正是文档要讲清 path 用法的原因。
	for name, want := range map[string]string{"index.html": "<h1>hi</h1>", "top.txt": "top"} {
		p := filepath.Join(root, "apps", "web", name)
		got, err := os.ReadFile(p)
		if err != nil {
			t.Fatalf("没找到 %s：%v", p, err)
		}
		if string(got) != want {
			t.Errorf("%s 内容 = %q，期望 %q", p, got, want)
		}
	}
}

// TestKeyBatchUploadRejectsTraversal 确认 path 字段挡住 .. 穿越。
func TestKeyBatchUploadRejectsTraversal(t *testing.T) {
	root := t.TempDir()
	s := newTestServer(t, testConfig(t, root, "-A", "--allow-upload"))
	_, token := createKey(t, s, "穿越", "/", 0)

	r := postForm(t, s, "/", token, "../escape", map[string]string{"x.txt": "x"})
	if r.code != http.StatusBadRequest {
		t.Fatalf("带 .. 的 path 返回 %d，期望 400：%s", r.code, r.body)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(root), "escape", "x.txt")); err == nil {
		t.Fatal("文件被写到了服务根之外")
	}
}

// TestKeyBatchUploadRespectsScope 确认 path 逃不出密钥范围。
func TestKeyBatchUploadRespectsScope(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "docs"), 0o755); err != nil {
		t.Fatal(err)
	}
	s := newTestServer(t, testConfig(t, root, "-A", "--allow-upload"))

	// 只覆盖 /docs 的密钥。
	_, token := createKey(t, s, "限定", "/docs", 0)

	// 打到 /other/ 上：超出范围，应当 403。
	r := postForm(t, s, "/other/", token, "", map[string]string{"x.txt": "x"})
	if r.code != http.StatusForbidden {
		t.Fatalf("越界上传返回 %d，期望 403：%s", r.code, r.body)
	}
	if _, err := os.Stat(filepath.Join(root, "other", "x.txt")); err == nil {
		t.Fatal("越界文件仍然落盘了")
	}

	// 打到自己的范围里：应当成功。
	ok := postForm(t, s, "/docs/", token, "", map[string]string{"y.txt": "y"})
	if ok.code < 200 || ok.code >= 300 {
		t.Fatalf("范围内上传返回 %d：%s", ok.code, ok.body)
	}
	if _, err := os.Stat(filepath.Join(root, "docs", "y.txt")); err != nil {
		t.Fatalf("范围内上传没有落盘：%v", err)
	}
}
