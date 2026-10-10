package server

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/xxl6097/gofs/internal/office"
)

// writeDocx 在 root 下写一个最小可用的 docx。
func writeDocx(t *testing.T, root, rel string, body string) {
	t.Helper()
	p := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, err := zw.Create("word/document.xml")
	if err != nil {
		t.Fatal(err)
	}
	xml := `<?xml version="1.0"?><w:document xmlns:w="http://x"><w:body>` + body + `</w:body></w:document>`
	if _, err := w.Write([]byte(xml)); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
}

// officeDoc 解析一次响应体。
func officeDoc(t *testing.T, body string) office.Doc {
	t.Helper()
	var d office.Doc
	if err := json.Unmarshal([]byte(body), &d); err != nil {
		t.Fatalf("响应不是合法 JSON：%v\n%s", err, body)
	}
	return d
}

// TestOfficePreviewReturnsBlocks 覆盖正常路径。
func TestOfficePreviewReturnsBlocks(t *testing.T) {
	root := t.TempDir()
	writeDocx(t, root, "报告.docx",
		`<w:p><w:pPr><w:pStyle w:val="Heading1"/></w:pPr><w:r><w:t>标题</w:t></w:r></w:p>`+
			`<w:p><w:r><w:t>正文内容</w:t></w:r></w:p>`)

	s := newTestServer(t, testConfig(t, root, "-A"))
	got := do(t, s, http.MethodGet, "/__gofs__/office?path=/报告.docx", nil, nil)
	if got.code != http.StatusOK {
		t.Fatalf("返回 %d，期望 200：%s", got.code, got.body)
	}
	doc := officeDoc(t, got.body)
	if doc.Format != "docx" {
		t.Errorf("format = %q，期望 docx", doc.Format)
	}
	if len(doc.Parts) != 1 || len(doc.Parts[0].Blocks) != 2 {
		t.Fatalf("块结构不对：%+v", doc.Parts)
	}
	if doc.Parts[0].Blocks[0].Type != "heading" {
		t.Errorf("第一个块应当是标题：%+v", doc.Parts[0].Blocks[0])
	}
	if doc.Parts[0].Blocks[1].Runs[0].Text != "正文内容" {
		t.Errorf("正文内容不对：%+v", doc.Parts[0].Blocks[1])
	}
}

// TestOfficePreviewRequiresAuth 是这次特意加的：新端点绝不能重复
// /__gofs__/extract 那个鉴权绕过的错误。
//
// 预览等同于读取文件内容，所以必须按路径做读权限校验。
func TestOfficePreviewRequiresAuth(t *testing.T) {
	root := t.TempDir()
	const secret = "机密：并购方案"
	writeDocx(t, root, "private/秘密.docx",
		`<w:p><w:r><w:t>`+secret+`</w:t></w:r></w:p>`)
	if err := os.MkdirAll(filepath.Join(root, "docs"), 0o755); err != nil {
		t.Fatal(err)
	}

	s := newTestServer(t, testConfig(t, root, "-A", "-a", "boss:pw@/:rw"))

	t.Run("未登录不能预览", func(t *testing.T) {
		got := do(t, s, http.MethodGet, "/__gofs__/office?path=/private/秘密.docx", nil, nil)
		if got.code == http.StatusOK {
			t.Fatalf("未登录竟然拿到了文档内容：%s", got.body)
		}
		if strings.Contains(got.body, secret) {
			t.Fatalf("文档内容泄漏了：%s", got.body)
		}
		if got.code != http.StatusUnauthorized {
			t.Fatalf("返回 %d，期望 401", got.code)
		}
	})

	t.Run("登录后可以预览", func(t *testing.T) {
		got := do(t, s, http.MethodGet, "/__gofs__/office?path=/private/秘密.docx", nil,
			map[string]string{"Authorization": basicAuth("boss", "pw")})
		if got.code != http.StatusOK {
			t.Fatalf("带凭据返回 %d，期望 200：%s", got.code, got.body)
		}
		if !strings.Contains(got.body, secret) {
			t.Errorf("没拿到内容：%s", got.body)
		}
	})
}

// TestOfficePreviewChecksPathPerm 确认校验的是**那个路径**的读权限。
func TestOfficePreviewChecksPathPerm(t *testing.T) {
	root := t.TempDir()
	writeDocx(t, root, "private/x.docx", `<w:p><w:r><w:t>机密</w:t></w:r></w:p>`)
	if err := os.MkdirAll(filepath.Join(root, "docs"), 0o755); err != nil {
		t.Fatal(err)
	}

	// 只有 /docs 的读权限。
	s := newTestServer(t, testConfig(t, root, "-A", "-a", "guest:g@/docs:r"))
	got := do(t, s, http.MethodGet, "/__gofs__/office?path=/private/x.docx", nil,
		map[string]string{"Authorization": basicAuth("guest", "g")})
	if got.code == http.StatusOK {
		t.Fatalf("越权读到了：%s", got.body)
	}
	if strings.Contains(got.body, "机密") {
		t.Fatal("内容泄漏")
	}
}

// TestOfficePreviewRejectsUnsupported 确认非 Office 文件被明确拒绝。
func TestOfficePreviewRejectsUnsupported(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "a.txt"), []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "fake.docx"), []byte("我不是 zip"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "adir.docx"), 0o755); err != nil {
		t.Fatal(err)
	}
	s := newTestServer(t, testConfig(t, root, "-A"))

	t.Run("普通文本", func(t *testing.T) {
		got := do(t, s, http.MethodGet, "/__gofs__/office?path=/a.txt", nil, nil)
		if got.code != http.StatusUnsupportedMediaType {
			t.Fatalf("返回 %d，期望 415", got.code)
		}
	})
	t.Run("扩展名像 Office 但内容不是 zip", func(t *testing.T) {
		got := do(t, s, http.MethodGet, "/__gofs__/office?path=/fake.docx", nil, nil)
		if got.code != http.StatusUnsupportedMediaType {
			t.Fatalf("返回 %d，期望 415：%s", got.code, got.body)
		}
	})
	t.Run("目录", func(t *testing.T) {
		got := do(t, s, http.MethodGet, "/__gofs__/office?path=/adir.docx", nil, nil)
		if got.code != http.StatusBadRequest {
			t.Fatalf("返回 %d，期望 400", got.code)
		}
	})
	t.Run("缺 path 参数", func(t *testing.T) {
		got := do(t, s, http.MethodGet, "/__gofs__/office", nil, nil)
		if got.code != http.StatusBadRequest {
			t.Fatalf("返回 %d，期望 400", got.code)
		}
	})
	t.Run("路径里有 ..", func(t *testing.T) {
		got := do(t, s, http.MethodGet, "/__gofs__/office?path=/../evil.docx", nil, nil)
		if got.code != http.StatusBadRequest {
			t.Fatalf("返回 %d，期望 400", got.code)
		}
	})
	t.Run("方法不对", func(t *testing.T) {
		got := do(t, s, http.MethodPost, "/__gofs__/office?path=/a.txt", nil, nil)
		if got.code != http.StatusMethodNotAllowed {
			t.Fatalf("返回 %d，期望 405", got.code)
		}
	})
	t.Run("文件不存在", func(t *testing.T) {
		got := do(t, s, http.MethodGet, "/__gofs__/office?path=/nope.docx", nil, nil)
		if got.code != http.StatusNotFound {
			t.Fatalf("返回 %d，期望 404", got.code)
		}
	})
}

// TestOfficePreviewScriptStaysData 确认文档里的脚本只会作为**数据**回来。
//
// 注意这条测的不是「响应体里没有 <script> 字样」—— 那句话本身就在文档里，
// 原样出现在 JSON 字符串里是正确的（服务端刻意不转义 HTML，见
// writeJSONStatus 里的 SetEscapeHTML(false)）。
//
// 真正要保证的是两件事：
//  1. 它以 application/json + nosniff 下发，浏览器不会把它当页面解析；
//  2. 模型里没有任何「会变成标记」的字段 —— 它只是 Runs[].Text 这个字符串，
//     前端用 textContent 渲染。
//
// 换句话说：安全性来自「数据与标记在架构上分离」，不是靠内容过滤。
func TestOfficePreviewScriptStaysData(t *testing.T) {
	root := t.TempDir()
	writeDocx(t, root, "x.docx",
		`<w:p><w:r><w:t>&lt;script&gt;alert(1)&lt;/script&gt;</w:t></w:r></w:p>`)
	s := newTestServer(t, testConfig(t, root, "-A"))

	got := do(t, s, http.MethodGet, "/__gofs__/office?path=/x.docx", nil, nil)
	if got.code != http.StatusOK {
		t.Fatalf("返回 %d：%s", got.code, got.body)
	}

	// 1. 必须声明成 JSON，且禁止浏览器嗅探成 HTML。
	ct := got.head.Get("Content-Type")
	if !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("Content-Type = %q，期望 application/json", ct)
	}
	if got.head.Get("X-Content-Type-Options") != "nosniff" {
		t.Errorf("缺少 nosniff，浏览器可能把 JSON 嗅探成 HTML")
	}

	// 2. 脚本内容原样保存在数据字段里。
	doc := officeDoc(t, got.body)
	text := doc.Parts[0].Blocks[0].Runs[0].Text
	if text != `<script>alert(1)</script>` {
		t.Fatalf("文本被改写了：%q", text)
	}
	// 模型里除 Runs[].Text 之外没有别的承载文本的字段，也就没有
	// 「被当成标记渲染」的入口。
	if len(doc.Parts[0].Blocks[0].Runs) != 1 {
		t.Fatalf("run 结构不对：%+v", doc.Parts[0].Blocks[0])
	}
}

// TestOfficeFormatFor 覆盖扩展名映射。
func TestOfficeFormatFor(t *testing.T) {
	cases := map[string]string{
		"a.docx": "docx", "a.DOCX": "docx", "a.xlsx": "xlsx",
		"a.pptx": "pptx", "a.docm": "docx", "a.xlsm": "xlsx",
		"a.doc": "", "a.xls": "", "a.ppt": "", "a.odt": "", "a.txt": "",
	}
	for in, want := range cases {
		if got := officeFormatFor(in); got != want {
			t.Errorf("officeFormatFor(%q) = %q，期望 %q", in, got, want)
		}
	}
}
