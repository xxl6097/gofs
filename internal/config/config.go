// Package config 定义 gofs 的运行时配置，并负责从命令行参数与环境变量加载。
//
// 参数风格参考 sigoden/dufs：位置参数为被服务的目录，其余为选项。
// 所有选项均可通过 GOFS_ 前缀的环境变量设置，命令行显式传入时以命令行为准。
package config

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// dateLayoutProbe 是校验日期布局字面量时使用的参照时刻。
var dateLayoutProbe = time.Date(2006, 1, 2, 15, 4, 5, 0, time.UTC)

// DefaultKeyFile 的返回值是上传密钥的默认持久化路径。
//
// 放在用户配置目录而不是服务目录，理由有两个：
// 一是不会污染被服务的文件树（否则密钥文件会出现在目录列表里），
// 二是不会因为换一个服务目录就丢掉已有密钥。
func DefaultKeyFile() string {
	dir, err := os.UserConfigDir()
	if err != nil || dir == "" {
		home, herr := os.UserHomeDir()
		if herr != nil {
			return ""
		}
		dir = filepath.Join(home, ".config")
	}
	return filepath.Join(dir, "gofs", "keys.json")
}

// 解压相关的硬性安全上限，可由命令行覆盖。
const (
	// DefaultExtractMaxTotal 单次解压允许写出的字节总量上限，默认 10 GiB。
	DefaultExtractMaxTotal int64 = 10 << 30
	// DefaultExtractMaxFiles 单次解压允许的文件条目数上限。
	DefaultExtractMaxFiles = 100000
	// DefaultExtractMaxRatio 单文件压缩比上限（防解压炸弹，100 倍）。
	DefaultExtractMaxRatio = 200
	// DefaultUploadDateLayout 为上传时默认使用的年/月/日三级目录布局。
	DefaultUploadDateLayout = "2006/01/02"
	// DefaultEditMaxSize 为在线编辑允许打开的最大文件尺寸（2 MiB）。
	DefaultEditMaxSize int64 = 2 << 20
)

// Config 保存完整的服务配置。
type Config struct {
	// ServePath 为被服务的根目录或单个文件。
	ServePath string
	// Bind 为监听地址，可为 IP、host:port，或以 / 开头的 unix socket 路径。
	Bind string
	// Port 为监听端口。
	Port int
	// PathPrefix 为访问路径前缀，例如 /dufs。
	PathPrefix string
	// Hidden 为目录列表中隐藏的文件名 glob（只匹配文件名，不匹配路径）。
	Hidden []string

	// AuthRules 为原始鉴权规则串，交由 auth 包解析。
	AuthRules []string

	// 权限开关。
	// AllowAll 打开全部开关。
	AllowAll bool

	// AllowEdit 表示允许在线编辑文本文件。
	// 未显式指定时跟随 AllowUpload（能上传即能改文件），可由 --allow-edit 单独控制。
	AllowEdit bool

	// EditMaxSize 为在线编辑允许打开的最大文件尺寸，超过则只能下载。
	EditMaxSize int64

	// AllowKeys 表示允许在页面上管理上传密钥。
	AllowKeys bool
	// KeyFile 为上传密钥的持久化路径；为空表示只保存在内存中，重启即失效。
	KeyFile      string
	AllowUpload  bool
	AllowDelete  bool
	AllowSearch  bool
	AllowArchive bool // 允许把目录打包成 zip 下载
	AllowExtract bool // 允许在线解压压缩包
	AllowSymlink bool // 允许符号链接指向根目录之外

	// 渲染模式。
	EnableCORS     bool
	RenderIndex    bool
	RenderTryIndex bool
	RenderSPA      bool

	// AssetsDir 为自定义前端资源目录，为空时使用内嵌资源。
	AssetsDir string

	// 日志。
	LogFormat string
	LogFile   string

	// Compress 为目录打包时的压缩级别：none / low / medium / high。
	Compress string

	// UploadDateLayout 为上传时自动归档的日期目录布局，写法是 Go 的时间布局字面量。
	// 默认 "2006/01/02"，即在上传目标目录下按 年/月/日 建三级目录存放文件；
	// 置为空串表示关闭归档，文件直接落在目标目录。
	UploadDateLayout string

	// TLS。
	TLSCert string
	TLSKey  string

	// 在线解压的安全上限。
	ExtractMaxTotal int64
	ExtractMaxFiles int
	ExtractMaxRatio int

	// ShowVersion 为 true 时打印版本并退出。
	ShowVersion bool
}

// Version 为当前版本号。
const Version = "0.1.0"

// ErrHelp 表示用户主动请求了帮助信息。
var ErrHelp = errors.New("help requested")

// envBool 读取布尔型环境变量。
func envBool(key string, def bool) bool {
	v, ok := os.LookupEnv(key)
	if !ok {
		return def
	}
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes", "on", "y":
		return true
	case "0", "false", "no", "off", "n":
		return false
	}
	return def
}

// envStr 读取字符串型环境变量。
func envStr(key, def string) string {
	if v, ok := os.LookupEnv(key); ok {
		return v
	}
	return def
}

// envInt 读取整型环境变量，非法值回落到默认值。
func envInt(key string, def int) int {
	if v, ok := os.LookupEnv(key); ok {
		if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil {
			return n
		}
	}
	return def
}

// envInt64 读取 64 位整型环境变量，非法值回落到默认值。
func envInt64(key string, def int64) int64 {
	if v, ok := os.LookupEnv(key); ok {
		if n, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64); err == nil {
			return n
		}
	}
	return def
}

// splitList 按逗号切分环境变量列表，忽略空项。
func splitList(s string) []string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// splitAuth 按 | 切分环境变量里的多条鉴权规则。
// 之所以不用逗号，是因为规则本身就以逗号分隔路径。
func splitAuth(s string) []string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	parts := strings.Split(s, "|")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// Parse 解析命令行参数与环境变量，返回最终配置。
// args 不含程序名本身（即传入 os.Args[1:]）。
func Parse(args []string, stdout io.Writer) (*Config, error) {
	cfg := &Config{
		ServePath:        envStr("GOFS_SERVE_PATH", "."),
		Bind:             envStr("GOFS_BIND", ""),
		Port:             envInt("GOFS_PORT", 5000),
		PathPrefix:       envStr("GOFS_PATH_PREFIX", ""),
		Hidden:           splitList(envStr("GOFS_HIDDEN", "")),
		AuthRules:        splitAuth(envStr("GOFS_AUTH", "")),
		AllowAll:         envBool("GOFS_ALLOW_ALL", false),
		AllowEdit:        envBool("GOFS_ALLOW_EDIT", false),
		EditMaxSize:      envInt64("GOFS_EDIT_MAX_SIZE", DefaultEditMaxSize),
		AllowKeys:        envBool("GOFS_ALLOW_KEYS", false),
		KeyFile:          envStr("GOFS_KEY_FILE", DefaultKeyFile()),
		AllowUpload:      envBool("GOFS_ALLOW_UPLOAD", false),
		AllowDelete:      envBool("GOFS_ALLOW_DELETE", false),
		AllowSearch:      envBool("GOFS_ALLOW_SEARCH", false),
		AllowArchive:     envBool("GOFS_ALLOW_ARCHIVE", false),
		AllowExtract:     envBool("GOFS_ALLOW_EXTRACT", false),
		AllowSymlink:     envBool("GOFS_ALLOW_SYMLINK", false),
		EnableCORS:       envBool("GOFS_ENABLE_CORS", false),
		RenderIndex:      envBool("GOFS_RENDER_INDEX", false),
		RenderTryIndex:   envBool("GOFS_RENDER_TRY_INDEX", false),
		RenderSPA:        envBool("GOFS_RENDER_SPA", false),
		AssetsDir:        envStr("GOFS_ASSETS", ""),
		LogFormat:        envStr("GOFS_LOG_FORMAT", ""),
		LogFile:          envStr("GOFS_LOG_FILE", ""),
		Compress:         envStr("GOFS_COMPRESS", "low"),
		UploadDateLayout: envStr("GOFS_UPLOAD_DATE_LAYOUT", DefaultUploadDateLayout),
		TLSCert:          envStr("GOFS_TLS_CERT", ""),
		TLSKey:           envStr("GOFS_TLS_KEY", ""),
		ExtractMaxTotal:  envInt64("GOFS_EXTRACT_MAX_TOTAL", DefaultExtractMaxTotal),
		ExtractMaxFiles:  envInt("GOFS_EXTRACT_MAX_FILES", DefaultExtractMaxFiles),
		ExtractMaxRatio:  envInt("GOFS_EXTRACT_MAX_RATIO", DefaultExtractMaxRatio),
	}

	fs := flag.NewFlagSet("gofs", flag.ContinueOnError)
	fs.SetOutput(stdout)
	fs.Usage = func() { fmt.Fprint(stdout, usageText()) }

	// 该开关以「否定形式」存在，便于在不改动 layout 默认值的前提下关闭归档。
	var noUploadDated bool

	fs.StringVar(&cfg.Bind, "b", cfg.Bind, "指定监听地址或 unix socket")
	fs.StringVar(&cfg.Bind, "bind", cfg.Bind, "指定监听地址或 unix socket")
	fs.IntVar(&cfg.Port, "p", cfg.Port, "指定监听端口")
	fs.IntVar(&cfg.Port, "port", cfg.Port, "指定监听端口")
	fs.StringVar(&cfg.PathPrefix, "path-prefix", cfg.PathPrefix, "指定访问路径前缀")
	fs.Func("hidden", "目录列表中隐藏的路径 glob，逗号分隔", func(v string) error {
		cfg.Hidden = append(cfg.Hidden, splitList(v)...)
		return nil
	})
	fs.Func("a", "添加鉴权规则，例如 user:pass@/dir1:rw,/dir2", func(v string) error {
		cfg.AuthRules = append(cfg.AuthRules, v)
		return nil
	})
	fs.Func("auth", "添加鉴权规则，例如 user:pass@/dir1:rw,/dir2", func(v string) error {
		cfg.AuthRules = append(cfg.AuthRules, v)
		return nil
	})
	fs.BoolVar(&cfg.AllowAll, "A", cfg.AllowAll, "允许所有操作")
	fs.BoolVar(&cfg.AllowAll, "allow-all", cfg.AllowAll, "允许所有操作")
	fs.BoolVar(&cfg.AllowUpload, "allow-upload", cfg.AllowUpload, "允许上传文件/目录")
	fs.BoolVar(&cfg.AllowEdit, "allow-edit", cfg.AllowEdit, "允许在线编辑文本文件（未指定时跟随 --allow-upload）")
	fs.Int64Var(&cfg.EditMaxSize, "edit-max-size", cfg.EditMaxSize, "在线编辑允许打开的最大文件尺寸（字节）")
	fs.BoolVar(&cfg.AllowKeys, "allow-keys", cfg.AllowKeys, "允许管理上传密钥（未指定时跟随 --allow-upload）")
	fs.StringVar(&cfg.KeyFile, "key-file", cfg.KeyFile, "上传密钥存储路径，置空则只保存在内存中")
	fs.BoolVar(&cfg.AllowDelete, "allow-delete", cfg.AllowDelete, "允许删除文件/目录")
	fs.BoolVar(&cfg.AllowSearch, "allow-search", cfg.AllowSearch, "允许搜索文件/目录")
	fs.BoolVar(&cfg.AllowArchive, "allow-archive", cfg.AllowArchive, "允许把目录打包成压缩包下载")
	fs.BoolVar(&cfg.AllowExtract, "allow-extract", cfg.AllowExtract, "允许在线解压压缩包")
	fs.BoolVar(&cfg.AllowSymlink, "allow-symlink", cfg.AllowSymlink, "允许符号链接指向根目录之外")
	fs.BoolVar(&cfg.EnableCORS, "enable-cors", cfg.EnableCORS, "开启 CORS，返回 Access-Control-Allow-Origin: *")
	fs.BoolVar(&cfg.RenderIndex, "render-index", cfg.RenderIndex, "请求目录时返回 index.html，不存在则 404")
	fs.BoolVar(&cfg.RenderTryIndex, "render-try-index", cfg.RenderTryIndex, "请求目录时优先返回 index.html，无则目录列表")
	fs.BoolVar(&cfg.RenderSPA, "render-spa", cfg.RenderSPA, "以 SPA 模式渲染，未命中路径回退到 index.html")
	fs.StringVar(&cfg.AssetsDir, "assets", cfg.AssetsDir, "自定义前端资源目录")
	fs.StringVar(&cfg.LogFormat, "log-format", cfg.LogFormat, "自定义 HTTP 日志格式，置空可关闭")
	fs.StringVar(&cfg.LogFile, "log-file", cfg.LogFile, "日志输出文件，默认 stdout")
	fs.StringVar(&cfg.Compress, "compress", cfg.Compress, "打包压缩级别：none/low/medium/high")
	fs.StringVar(&cfg.UploadDateLayout, "upload-date-layout", cfg.UploadDateLayout,
		"上传时自动归档的日期目录布局（Go 时间布局），如 2006/01/02 或 2006-01；置空则关闭")
	fs.BoolVar(&noUploadDated, "no-upload-dated", false, "关闭上传按日期自动归档（等同于 --upload-date-layout=''）")
	fs.StringVar(&cfg.TLSCert, "tls-cert", cfg.TLSCert, "HTTPS 证书路径")
	fs.StringVar(&cfg.TLSKey, "tls-key", cfg.TLSKey, "HTTPS 私钥路径")
	fs.Int64Var(&cfg.ExtractMaxTotal, "extract-max-total", cfg.ExtractMaxTotal, "单次解压写出字节上限")
	fs.IntVar(&cfg.ExtractMaxFiles, "extract-max-files", cfg.ExtractMaxFiles, "单次解压文件数上限")
	fs.IntVar(&cfg.ExtractMaxRatio, "extract-max-ratio", cfg.ExtractMaxRatio, "单文件压缩比上限（防解压炸弹）")
	fs.BoolVar(&cfg.ShowVersion, "V", false, "打印版本")
	fs.BoolVar(&cfg.ShowVersion, "version", false, "打印版本")

	// flag 包在遇到 -h/--help 时会返回 flag.ErrHelp。
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil, ErrHelp
		}
		return nil, err
	}

	// 位置参数：最后一个作为被服务路径。
	if rest := fs.Args(); len(rest) > 0 {
		cfg.ServePath = rest[len(rest)-1]
	}

	// --no-upload-dated 与显式的 --upload-date-layout 互斥，避免意图含糊。
	specified := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { specified[f.Name] = true })
	if noUploadDated {
		if specified["upload-date-layout"] {
			return nil, errors.New("--no-upload-dated 与 --upload-date-layout 不能同时使用")
		}
		cfg.UploadDateLayout = ""
	}

	// -A 是各项权限的并集。
	if cfg.AllowAll {
		cfg.AllowUpload = true
		cfg.AllowDelete = true
		cfg.AllowSearch = true
		cfg.AllowArchive = true
		cfg.AllowExtract = true
		cfg.AllowEdit = true
		cfg.AllowKeys = true
	} else {
		// 未显式指定 --allow-edit / --allow-keys 时跟随上传权限：
		// 能上传文件却没能力改文件、发密钥会很别扭。
		if !specified["allow-edit"] {
			cfg.AllowEdit = cfg.AllowUpload
		}
		if !specified["allow-keys"] {
			cfg.AllowKeys = cfg.AllowUpload
		}
	}

	if err := cfg.normalize(); err != nil {
		return nil, err
	}
	return cfg, nil
}

// normalize 做参数合法化与归一化。
func (c *Config) normalize() error {
	// 路径前缀统一为 "/xxx" 或空。
	p := strings.TrimSpace(c.PathPrefix)
	if p != "" && p != "/" {
		if !strings.HasPrefix(p, "/") {
			p = "/" + p
		}
		p = strings.TrimSuffix(p, "/")
	} else {
		p = ""
	}
	c.PathPrefix = p

	// 压缩级别校验。
	switch strings.ToLower(c.Compress) {
	case "none", "low", "medium", "high":
		c.Compress = strings.ToLower(c.Compress)
	default:
		return fmt.Errorf("非法的压缩级别 %q，可选：none / low / medium / high", c.Compress)
	}

	// 上传日期布局校验：用一个参照时刻试格式化，确认不会生成越界路径段。
	c.UploadDateLayout = strings.TrimSpace(c.UploadDateLayout)
	if c.UploadDateLayout != "" {
		probe := dateLayoutProbe.Format(c.UploadDateLayout)
		switch {
		case strings.Trim(probe, "/") == "":
			return fmt.Errorf("非法的上传日期布局 %q：格式化结果为空", c.UploadDateLayout)
		case strings.Contains(probe, ".."):
			return fmt.Errorf("非法的上传日期布局 %q：不允许出现 ..", c.UploadDateLayout)
		case strings.HasPrefix(probe, "/"), strings.HasPrefix(probe, "\\"):
			return fmt.Errorf("非法的上传日期布局 %q：不允许是绝对路径", c.UploadDateLayout)
		case strings.ContainsAny(probe, "\\:"):
			return fmt.Errorf("非法的上传日期布局 %q：不允许出现 \\ 或 :", c.UploadDateLayout)
		}
	}

	if c.Port < 0 || c.Port > 65535 {
		return fmt.Errorf("非法端口 %d", c.Port)
	}
	if c.TLSCert != "" || c.TLSKey != "" {
		if c.TLSCert == "" || c.TLSKey == "" {
			return errors.New("启用 HTTPS 需要同时指定 --tls-cert 与 --tls-key")
		}
	}
	if c.ExtractMaxFiles <= 0 {
		return errors.New("--extract-max-files 必须为正整数")
	}
	if c.ExtractMaxTotal <= 0 {
		return errors.New("--extract-max-total 必须为正整数")
	}
	if c.ExtractMaxRatio <= 0 {
		return errors.New("--extract-max-ratio 必须为正整数")
	}
	if c.EditMaxSize <= 0 {
		return errors.New("--edit-max-size 必须为正整数")
	}
	return nil
}

// UploadDated 表示是否开启了上传按日期归档。
func (c *Config) UploadDated() bool { return c.UploadDateLayout != "" }

// UploadDateDir 返回 t 时刻按配置布局生成的归档目录段（如 "2026/09/28"）。
// 未开启归档时返回空串。
func (c *Config) UploadDateDir(t time.Time) string {
	if c.UploadDateLayout == "" {
		return ""
	}
	return t.Format(c.UploadDateLayout)
}

// Addr 返回最终的监听地址。
func (c *Config) Addr() string {
	if strings.HasPrefix(c.Bind, "/") {
		return c.Bind // unix socket
	}
	if c.Bind != "" {
		if strings.Contains(c.Bind, ":") {
			return c.Bind
		}
		return fmt.Sprintf("%s:%d", c.Bind, c.Port)
	}
	return fmt.Sprintf("0.0.0.0:%d", c.Port)
}

// IsUnixSocket 判断是否监听 unix socket。
func (c *Config) IsUnixSocket() bool { return strings.HasPrefix(c.Bind, "/") }

// usageText 返回帮助文本。
func usageText() string {
	return `gofs - 一个参照 dufs 设计的 Go 文件服务器，支持在线解压压缩包

用法: gofs [选项] [serve-path]

参数:
  [serve-path]               要服务的路径，默认 "."

选项:
  -b, --bind <addrs>         监听地址或 unix socket
  -p, --port <port>          监听端口，默认 5000
      --path-prefix <path>   访问路径前缀
      --hidden <value>       目录列表中隐藏的路径 glob，例如 tmp,*.log
  -a, --auth <rules>         鉴权规则，例如 user:pass@/dir1:rw,/dir2
  -A, --allow-all            允许所有操作
      --allow-upload         允许上传
      --allow-edit           允许在线编辑文本文件
                             （未显式指定时跟随 --allow-upload 与 -A）
      --edit-max-size <n>    在线编辑允许打开的最大文件尺寸，默认 2MiB
      --allow-keys           允许管理上传密钥（未指定时跟随 --allow-upload）
      --key-file <path>      上传密钥存储路径，默认 <用户配置目录>/gofs/keys.json；
                             置空则只保存在内存中，重启即失效
      --allow-delete         允许删除
      --allow-search         允许搜索
      --allow-archive        允许目录打包为 zip 下载
      --allow-extract        允许在线解压压缩包（zip/tar/tar.gz/tgz/gz）
      --allow-symlink        允许符号链接指向根目录之外
      --enable-cors          开启 CORS
      --render-index         目录下 index.html 作为首页，不存在返回 404
      --render-try-index     优先 index.html，无则目录列表
      --render-spa           SPA 模式，未命中回退 index.html
      --assets <path>        自定义前端资源目录
      --log-format <format>  自定义 HTTP 日志格式
      --log-file <file>      日志文件
      --compress <level>     打包压缩级别：none/low/medium/high，默认 low
      --upload-date-layout <layout>
                             上传时自动归档的日期目录布局（Go 时间布局字面量），
                             默认 "2006/01/02"，即在目标目录下按 年/月/日 建三级目录；
                             置空（--upload-date-layout=""）则关闭
      --no-upload-dated      关闭上传按日期自动归档
      --tls-cert <path>      HTTPS 证书
      --tls-key <path>       HTTPS 私钥
      --extract-max-total <n> 解压写出字节上限，默认 10GiB
      --extract-max-files <n> 解压文件数上限，默认 100000
      --extract-max-ratio <n> 单文件压缩比上限，默认 200
  -V, --version              打印版本
  -h, --help                 打印帮助

示例:
  gofs                                 以只读模式服务当前目录
  gofs -A ./data                       服务 ./data 并允许所有操作
  gofs -A --allow-extract ./data       在上一行基础上开放在线解压
  gofs -a admin:123@/:rw -A ./data     需要账号密码
  gofs -b 127.0.0.1 -p 8080 ./public   指定监听地址与端口

在线解压:
  PUT    /__gofs__/extract       请求体 {"path":"a.zip","dest":"a","overwrite":false}
                                 返回 text/event-stream，逐条推送解压进度
  GET    /__gofs__/extract?path=a.zip          列出压缩包内容（JSON）
  GET    /__gofs__/extract?path=a.zip&file=x   从压缩包内流式下载单个文件
在线编辑:
  GET    /__gofs__/text?path=a.md         读取文本内容（含语言、换行、BOM、内容摘要）
  PUT    /__gofs__/text                   保存，请求体 {"path","content","base_hash","force"}
                                          摘要不匹配时返回 409，避免静默覆盖他人改动

上传密钥:
  GET    /__gofs__/keys                   列出全部密钥（不含明文）
  POST   /__gofs__/keys                   创建，请求体 {"name","scope","ttl_seconds"}
                                          响应中的 token 只返回这一次
  DELETE /__gofs__/keys?id=<id>           撤销
  使用   curl -T f.txt -H 'X-Gofs-Upload-Key: <token>' http://host/path
         或 curl -T f.txt 'http://host/path?key=<token>'
         密钥只允许上传，且只能落在 scope 限定的目录内

认证:
  页面使用 HTTP Basic 认证。浏览器导航未登录时返回应用外壳（200），
  由前端弹出登录框；应用内部请求会带 X-Gofs-Ajax 标记，
  此时 401 不附带 WWW-Authenticate 挑战，避免弹出浏览器原生登录框。
  命令行仍然照常使用 curl -u user:pass 或 --anyauth。
`
}
