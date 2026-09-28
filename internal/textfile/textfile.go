// Package textfile 负责判断「哪些文件可以在线编辑」，
// 并提供编码、换行符、BOM 等文本处理辅助。
//
// 判断分两步：
//  1. 列表阶段只按文件名/扩展名判断（不读文件内容，避免列举目录时产生大量磁盘 IO）
//  2. 真正打开时再做一次内容嗅探（Sniff），二进制文件一律拒绝，防止编辑器把图片写坏
package textfile

import (
	"bytes"
	"path"
	"strings"
	"unicode/utf8"
)

// MaxEditableSize 默认允许在线打开的最大文件尺寸。
// 超过这个体积的文本（例如几 GB 的日志）不适合塞进浏览器文本框。
const MaxEditableSize int64 = 2 << 20 // 2 MiB

// MaxSaveSize 保存时的内容上限，留出一定余量避免客户端构造超大请求。
const MaxSaveSize int64 = 16 << 20 // 16 MiB

// nameLanguages 完整文件名（小写）到语言的映射。
// 这些文件通常没有扩展名，必须靠名字识别。
var nameLanguages = map[string]string{
	"makefile":       "makefile",
	"gnumakefile":    "makefile",
	"dockerfile":     "dockerfile",
	"containerfile":  "dockerfile",
	"vagrantfile":    "ruby",
	"jenkinsfile":    "groovy",
	"rakefile":       "ruby",
	"gemfile":        "ruby",
	"procfile":       "yaml",
	"brewfile":       "ruby",
	"cmakelists.txt": "cmake",
	"license":        "plaintext",
	"licence":        "plaintext",
	"copying":        "plaintext",
	"notice":         "plaintext",
	"authors":        "plaintext",
	"changelog":      "markdown",
	"readme":         "markdown",
	"todo":           "plaintext",
	"hosts":          "plaintext",
	".gitignore":     "gitignore",
	".gitattributes": "gitignore",
	".dockerignore":  "gitignore",
	".npmignore":     "gitignore",
	".editorconfig":  "ini",
	".babelrc":       "json",
	".eslintrc":      "json",
	".prettierrc":    "json",
	".bashrc":        "shell",
	".zshrc":         "shell",
	".profile":       "shell",
	".bash_profile":  "shell",
	".htaccess":      "apache",
}

// prefixLanguages 按文件名前缀识别（用于 .env.local、README.md 这类）。
var prefixLanguages = []struct {
	prefix string
	lang   string
}{
	{".env", "dotenv"},
	{"readme.", "markdown"},
	{"changelog.", "markdown"},
	{"license.", "plaintext"},
	{".gitignore", "gitignore"},
	{".envrc", "shell"},
}

// extLanguages 扩展名（小写、不含点）到语言的映射。
var extLanguages = map[string]string{
	// 纯文本与文档
	"txt": "plaintext", "text": "plaintext", "log": "plaintext", "out": "plaintext",
	"err": "plaintext", "list": "plaintext", "dump": "plaintext", "nfo": "plaintext",
	"md": "markdown", "markdown": "markdown", "mdx": "markdown", "mkd": "markdown",
	"rst": "rst", "adoc": "asciidoc", "org": "org", "tex": "latex", "bib": "bibtex",
	"csv": "csv", "tsv": "csv", "srt": "plaintext", "vtt": "plaintext",
	"diff": "diff", "patch": "diff",

	// 标记与网页
	"htm": "html", "html": "html", "xhtml": "html", "shtml": "html",
	"vue": "html", "svelte": "html", "astro": "html", "hbs": "handlebars",
	"ejs": "html", "pug": "pug", "jade": "pug", "erb": "erb",
	"xml": "xml", "xsl": "xml", "xslt": "xml", "xsd": "xml", "dtd": "xml",
	"svg": "xml", "plist": "xml", "rss": "xml", "atom": "xml", "wsdl": "xml",
	"xaml": "xml", "csproj": "xml", "pom": "xml", "gradle": "groovy",

	// 样式
	"css": "css", "scss": "scss", "sass": "sass", "less": "less", "styl": "stylus",

	// 脚本与编程语言
	"js": "javascript", "mjs": "javascript", "cjs": "javascript", "jsx": "javascript",
	"ts": "typescript", "tsx": "typescript", "mts": "typescript", "cts": "typescript",
	"coffee": "coffeescript",
	"json":   "json", "jsonc": "json", "json5": "json", "ndjson": "json", "jsonl": "json",
	"geojson": "json", "webmanifest": "json", "har": "json",
	"yaml": "yaml", "yml": "yaml",
	"toml": "toml", "ini": "ini", "cfg": "ini", "conf": "ini",
	"properties": "properties", "env": "dotenv", "htaccess": "apache",
	"sh": "shell", "bash": "shell", "zsh": "shell", "fish": "shell",
	"ksh": "shell", "csh": "shell", "bat": "bat", "cmd": "bat", "ps1": "powershell",
	"psm1": "powershell", "psd1": "powershell",
	"py": "python", "pyi": "python", "pyw": "python", "ipynb": "json",
	"rb": "ruby", "gemspec": "ruby", "rake": "ruby", "ru": "ruby",
	"php": "php", "phtml": "php",
	"pl": "perl", "pm": "perl", "t": "perl",
	"lua": "lua", "r": "r", "jl": "julia", "tcl": "tcl",
	"go": "go", "mod": "go", "sum": "plaintext",
	"java": "java", "kt": "kotlin", "kts": "kotlin", "scala": "scala", "sc": "scala",
	"groovy": "groovy", "clj": "clojure", "cljs": "clojure", "edn": "clojure",
	"c": "c", "h": "c", "cc": "cpp", "cpp": "cpp", "cxx": "cpp", "c++": "cpp",
	"hpp": "cpp", "hh": "cpp", "hxx": "cpp", "inl": "cpp",
	"cs": "csharp", "fs": "fsharp", "fsx": "fsharp", "vb": "vbnet",
	"m": "objectivec", "mm": "objectivec",
	"swift": "swift", "dart": "dart", "rs": "rust", "zig": "zig",
	"nim": "nim", "d": "d", "hs": "haskell", "elm": "elm", "ex": "elixir", "exs": "elixir",
	"erl": "erlang", "hrl": "erlang", "ml": "ocaml", "mli": "ocaml",
	"asm": "assembly", "s": "assembly",
	"v": "verilog", "sv": "verilog", "vhd": "vhdl",
	"sol": "solidity", "move": "move", "cairo": "cairo",

	// 查询与接口描述
	"sql": "sql", "ddl": "sql", "dml": "sql",
	"graphql": "graphql", "gql": "graphql", "proto": "protobuf", "thrift": "thrift",
	"avsc": "json", "raml": "yaml",

	// 基础设施
	"tf": "hcl", "tfvars": "hcl", "hcl": "hcl",
	"nomad": "hcl", "tfstate": "json",
	"dockerfile": "dockerfile", "containerfile": "dockerfile",
	"k8s": "yaml", "helmignore": "gitignore",
	"cmake": "cmake", "mk": "makefile", "mak": "makefile", "am": "makefile",
	"service": "ini", "socket": "ini", "desktop": "ini", "repo": "ini",
}

// Language 返回文件名对应的语言标识；未识别时返回空串。
func Language(name string) string {
	base := path.Base(strings.ReplaceAll(name, "\\", "/"))
	lower := strings.ToLower(base)

	if lang, ok := nameLanguages[lower]; ok {
		return lang
	}
	for _, p := range prefixLanguages {
		if strings.HasPrefix(lower, p.prefix) {
			return p.lang
		}
	}

	// 复合扩展名优先，例如 .tar.gz 场景下先看 ".d.ts"。
	if strings.HasSuffix(lower, ".d.ts") {
		return "typescript"
	}
	ext := strings.TrimPrefix(path.Ext(lower), ".")
	if ext == "" {
		return ""
	}
	if lang, ok := extLanguages[ext]; ok {
		return lang
	}
	return ""
}

// IsEditableName 判断某文件名是否属于默认可在线编辑的文本类型。
// 只看名字，不读内容，因此可在列目录时低成本调用。
func IsEditableName(name string) bool {
	return Language(name) != ""
}

// Sniff 判断一段内容是否像纯文本。
// 判定规则：不含 NUL 字节，且 UTF-8 解码失败率极低。
func Sniff(data []byte) bool {
	if len(data) == 0 {
		return true
	}
	// 只嗅探前 8 KiB 足够。
	sample := data
	if len(sample) > 8<<10 {
		sample = sample[:8<<10]
	}
	if bytes.IndexByte(sample, 0) >= 0 {
		return false
	}
	// 统计控制字符与非法 UTF-8。
	bad := 0
	for len(sample) > 0 {
		r, size := utf8.DecodeRune(sample)
		if r == utf8.RuneError && size == 1 {
			bad++
		} else if r < 0x20 && r != '\n' && r != '\r' && r != '\t' && r != '\f' {
			bad++
		}
		sample = sample[size:]
	}
	// 允许极少量噪音（例如个别损坏字节），超过 5% 判定为二进制。
	threshold := len(data) / 20
	if threshold < 4 {
		threshold = 4
	}
	return bad <= threshold
}

// 常见 BOM 前缀。
var (
	bomUTF8    = []byte{0xEF, 0xBB, 0xBF}
	bomUTF16LE = []byte{0xFF, 0xFE}
	bomUTF16BE = []byte{0xFE, 0xFF}
)

// TrimBOM 剥离开头的 BOM，返回 BOM 本身与剩余内容。
func TrimBOM(data []byte) (bom, rest []byte) {
	switch {
	case bytes.HasPrefix(data, bomUTF8):
		return bomUTF8, data[len(bomUTF8):]
	case bytes.HasPrefix(data, bomUTF16LE):
		return bomUTF16LE, data[len(bomUTF16LE):]
	case bytes.HasPrefix(data, bomUTF16BE):
		return bomUTF16BE, data[len(bomUTF16BE):]
	}
	return nil, data
}

// BOMKind 返回 BOM 的可读名称，无 BOM 返回空串。
func BOMKind(bom []byte) string {
	switch {
	case bytes.Equal(bom, bomUTF8):
		return "utf-8"
	case bytes.Equal(bom, bomUTF16LE):
		return "utf-16le"
	case bytes.Equal(bom, bomUTF16BE):
		return "utf-16be"
	}
	return ""
}

// 换行风格常量。
const (
	// NewlineLF 是 Unix 换行。
	NewlineLF = "\n"
	// NewlineCRLF 是 Windows 换行。
	NewlineCRLF = "\r\n"
	// NewlineCR 是经典 Mac 换行。
	NewlineCR = "\r"
)

// Newline 检测内容主要使用的换行风格，默认返回 LF。
func Newline(data []byte) string {
	var crlf, lf, cr int
	for i := 0; i < len(data); i++ {
		switch data[i] {
		case '\r':
			if i+1 < len(data) && data[i+1] == '\n' {
				crlf++
				i++
			} else {
				cr++
			}
		case '\n':
			lf++
		}
	}
	switch {
	case crlf == 0 && cr == 0:
		return NewlineLF
	case crlf >= lf && crlf >= cr:
		return NewlineCRLF
	case cr > lf:
		return NewlineCR
	default:
		return NewlineLF
	}
}

// NormalizeNewline 把内容统一为 LF，便于比较与处理。
func NormalizeNewline(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	return strings.ReplaceAll(s, "\r", "\n")
}

// ApplyNewline 把内容中的 LF 换成指定的换行风格。
//
// 用途：浏览器 <textarea> 提交时会把 CRLF 规范化为 LF，
// 若原文件是 Windows 换行，直接保存会让整份文件的换行全变，
// 在 git diff 里表现为「整个文件都被改了」。这里按原风格还原。
func ApplyNewline(s, newline string) string {
	s = NormalizeNewline(s)
	if newline == NewlineCRLF {
		return strings.ReplaceAll(s, "\n", "\r\n")
	}
	return s
}
