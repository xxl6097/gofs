package textfile

import (
	"bytes"
	"strings"
	"testing"
)

func TestLanguage(t *testing.T) {
	cases := map[string]string{
		"a.txt":          "plaintext",
		"README.md":      "markdown",
		"README":         "markdown",
		"Makefile":       "makefile",
		"Dockerfile":     "dockerfile",
		"config.json":    "json",
		"data.yaml":      "yaml",
		"page.html":      "html",
		"feed.xml":       "xml",
		"main.go":        "go",
		"app.ts":         "typescript",
		"index.d.ts":     "typescript",
		"style.css":      "css",
		"script.sh":      "shell",
		".gitignore":     "gitignore",
		".env":           "dotenv",
		".env.local":     "dotenv",
		"notes.txt.gz":   "", // 压缩包不是文本
		"photo.jpg":      "",
		"archive.zip":    "",
		"binary.exe":     "",
		"CMakeLists.txt": "cmake",
		"component.vue":  "html",
		"query.sql":      "sql",
		"schema.proto":   "protobuf",
		"values.tfvars":  "hcl",
		"deploy.tf":      "hcl",
	}
	for name, want := range cases {
		if got := Language(name); got != want {
			t.Errorf("Language(%q) = %q, 期望 %q", name, got, want)
		}
	}
}

func TestIsEditableName(t *testing.T) {
	editable := []string{"a.txt", "README.md", "main.go", ".env", "Makefile"}
	for _, n := range editable {
		if !IsEditableName(n) {
			t.Errorf("%q 应可编辑", n)
		}
	}
	notEditable := []string{"a.jpg", "b.png", "c.zip", "d.mp4", "e.pdf", "f.exe"}
	for _, n := range notEditable {
		if IsEditableName(n) {
			t.Errorf("%q 不应被标记为可编辑", n)
		}
	}
}

func TestSniff(t *testing.T) {
	if !Sniff([]byte("hello 世界\n")) {
		t.Errorf("普通文本应判定为文本")
	}
	if !Sniff(nil) {
		t.Errorf("空内容应判定为文本")
	}
	// 含 NUL 字节 → 二进制
	if Sniff([]byte("abc\x00def")) {
		t.Errorf("含 NUL 的内容应判定为二进制")
	}
	// PNG 文件头
	png := []byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a, 0, 0, 0, 13}
	if Sniff(png) {
		t.Errorf("PNG 头应判定为二进制")
	}
	// 大量非法 UTF-8 字节
	bad := bytes.Repeat([]byte{0xff, 0xfe, 0xfd, 0xfc}, 100)
	if Sniff(bad) {
		t.Errorf("大量非法 UTF-8 应判定为二进制")
	}
	// 带制表符与换行的文本仍然算文本
	if !Sniff([]byte("a\tb\r\nc\x0bd\n")) {
		t.Errorf("含制表符与换行的文本应判定为文本")
	}
}

func TestNewline(t *testing.T) {
	cases := map[string]string{
		"a\nb\nc\n":       NewlineLF,
		"a\r\nb\r\nc\r\n": NewlineCRLF,
		"a\rb\rc\r":       NewlineCR,
		"":                NewlineLF,
		"single":          NewlineLF,
	}
	for in, want := range cases {
		if got := Newline([]byte(in)); got != want {
			t.Errorf("Newline(%q) = %q, 期望 %q", in, got, want)
		}
	}
}

func TestApplyNewline(t *testing.T) {
	// 浏览器 textarea 提交时 CRLF 会被规范化成 LF，
	// 保存时要按原文件风格还原，避免整份文件的换行都被改掉。
	if got := ApplyNewline("a\nb\n", NewlineCRLF); got != "a\r\nb\r\n" {
		t.Errorf("转 CRLF 失败: %q", got)
	}
	if got := ApplyNewline("a\r\nb\r\n", NewlineLF); got != "a\nb\n" {
		t.Errorf("转 LF 失败: %q", got)
	}
	// 混合换行也应被统一
	if got := ApplyNewline("a\r\nb\rc\n", NewlineLF); got != "a\nb\nc\n" {
		t.Errorf("混合换行归一化失败: %q", got)
	}
	if got := ApplyNewline("a\rb\r", NewlineCRLF); got != "a\r\nb\r\n" {
		t.Errorf("CR 转 CRLF 失败: %q", got)
	}
}

func TestNormalizeNewline(t *testing.T) {
	if got := NormalizeNewline("a\r\nb\rc"); got != "a\nb\nc" {
		t.Errorf("NormalizeNewline = %q", got)
	}
}

func TestTrimAndBOMKind(t *testing.T) {
	utf8BOM := append([]byte{0xEF, 0xBB, 0xBF}, []byte("hello")...)
	bom, rest := TrimBOM(utf8BOM)
	if string(rest) != "hello" {
		t.Errorf("剥离 BOM 后内容 = %q", rest)
	}
	if BOMKind(bom) != "utf-8" {
		t.Errorf("BOM 类型 = %q, 期望 utf-8", BOMKind(bom))
	}

	// 无 BOM
	bom2, rest2 := TrimBOM([]byte("hello"))
	if len(bom2) != 0 || string(rest2) != "hello" {
		t.Errorf("无 BOM 时不应改动内容")
	}
	if BOMKind(nil) != "" {
		t.Errorf("无 BOM 时类型应为空")
	}

	// UTF-16 应被识别出来（服务端会拒绝编辑这类文件）
	bom3, _ := TrimBOM([]byte{0xFF, 0xFE, 'h', 0, 'i', 0})
	if BOMKind(bom3) != "utf-16le" {
		t.Errorf("UTF-16LE BOM 类型 = %q", BOMKind(bom3))
	}
}

func TestSniffLargeInput(t *testing.T) {
	// 只嗅探前 8 KiB：头部干净、尾部有 NUL 的内容仍会被判为文本，
	// 这是刻意的取舍——完整扫描大文件的开销不值得。
	clean := bytes.Repeat([]byte("a"), 20<<10)
	if !Sniff(clean) {
		t.Errorf("纯文本大文件应判定为文本")
	}
	head := bytes.Repeat([]byte("a"), 100)
	dirty := append(head, make([]byte, 10)...) // 头部范围内就有 NUL
	if Sniff(dirty) {
		t.Errorf("头部含 NUL 应判定为二进制")
	}
}

func TestLanguageCaseInsensitive(t *testing.T) {
	if got := Language("SCRIPT.SH"); got != "shell" {
		t.Errorf("大写扩展名未识别: %q", got)
	}
	if got := Language("path/to/DEEP/File.JSON"); got != "json" {
		t.Errorf("带路径的文件名未识别: %q", got)
	}
	if !strings.HasPrefix(Language("x.md"), "mark") {
		t.Errorf("markdown 识别异常")
	}
}
