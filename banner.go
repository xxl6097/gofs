package gofs

import (
	"fmt"
	"io"
	"time"
)

// Banner 把启动信息写入 w，让使用者一眼看清关键配置。
// 监听地址取自实际的 listener，所以要在 Start 之后调用；
// 未启动时地址一栏显示配置里的目标地址。
func (s *Server) Banner(w io.Writer) {
	cfg := s.cfg

	addr := cfg.Addr()
	if ln := s.Addr(); ln != nil {
		addr = ln.String()
	}
	if cfg.IsUnixSocket() {
		addr += " (unix socket)"
	}

	// 直接列**实时的**挂载点，而不是 cfg.ServePaths：后者只有命令行给的那些，
	// 运行期新增（或从设置文件恢复）的站点会漏印甚至错位。
	roots := s.Mounts()

	perm := func(name string, on bool) string {
		if on {
			return name
		}
		return "-"
	}
	fmt.Fprintf(w, "gofs %s\n", cfg.Version)
	fmt.Fprintf(w, "  监听      : %s://%s\n", s.Scheme(), addr)
	// 主目录服务在 /，附加站点挂在 /<标签>/ —— 那才是浏览器里要敲的地址段。
	for i, m := range roots {
		name := "服务目录"
		if i > 0 {
			name = ""
		}
		line := m.Root
		if s.MultiRoot() {
			suffix := m.URLPath + "/"
			if m.Primary {
				suffix = "/"
			}
			line += "   →  " + suffix
		}
		fmt.Fprintf(w, "  %-9s : %s\n", name, line)
	}
	if len(roots) == 0 {
		// 兜底：理论上至少有一个（BuildRoots 不允许空），别让 banner 整个空掉。
		fmt.Fprintf(w, "  %-9s : %s\n", "服务目录", cfg.ServePath)
	}
	if cfg.PathPrefix != "" {
		fmt.Fprintf(w, "  路径前缀  : %s\n", cfg.PathPrefix)
	}
	fmt.Fprintf(w, "  权限      : upload=%s edit=%s delete=%s search=%s archive=%s extract=%s keys=%s\n",
		perm("on", cfg.AllowUpload),
		perm("on", cfg.AllowEdit),
		perm("on", cfg.AllowDelete),
		perm("on", cfg.AllowSearch),
		perm("on", cfg.AllowArchive),
		perm("on", cfg.AllowExtract),
		perm("on", cfg.AllowKeys),
	)
	// 鉴权是否生效要问 Authenticator，不能只数 cfg.AuthRules ——
	// 用户表里的账号同样能启用鉴权，只看启动参数会把
	// 「其实要登录」说成「任何人都可访问」。
	switch {
	case !s.inner.AuthEnabled():
		fmt.Fprintf(w, "  鉴权      : 未启用（任何人都可访问）\n")
	case len(cfg.AuthRules) > 0:
		fmt.Fprintf(w, "  鉴权      : 已启用（%d 条规则），页面使用 Basic 认证\n", len(cfg.AuthRules))
	default:
		fmt.Fprintf(w, "  鉴权      : 已启用（账号来自用户表），页面使用 Basic 认证\n")
	}
	if cfg.AllowKeys {
		keyLoc := cfg.KeyFile
		if keyLoc == "" {
			keyLoc = "仅内存（重启后失效）"
		}
		fmt.Fprintf(w, "  上传密钥  : %s\n", keyLoc)
	}
	// 用户管理只有在「已启用鉴权」时才可用，所以这里也按同一条件打印，
	// 免得看到一个用不了的路径。
	if cfg.AllowUserManage && s.inner.AuthEnabled() {
		userLoc := cfg.UserFile
		if userLoc == "" {
			userLoc = "仅内存（重启后失效）"
		}
		fmt.Fprintf(w, "  用户表    : %s\n", userLoc)
	}
	// 运行期设置（站点标题与附加站点）也是落盘的，位置值得一并印出来 ——
	// 否则「改完重启就没了」只能靠猜。
	if cfg.SettingsFile != "" {
		fmt.Fprintf(w, "  设置文件  : %s\n", cfg.SettingsFile)
	}
	// 把生效中的防护值打出来：这些默认值直接决定了服务被滥用时的表现，
	// 显式可见比藏在 --help 里更让人放心。
	//
	// ⚠️ 「无进展超时」要**分别**打上传与下载两项：它们是两个独立的开关
	//（--upload-read-timeout / --download-timeout），只是默认值恰好都是 2m。
	// 早先这里只打了 DownloadTimeout 一个，标着笼统的「无进展超时」——
	// 两个默认值相同，所以怎么调都看不出问题；一旦分别配置，
	// 横幅就在误导运维（他只改了上传，看到的却是下载那个值）。
	fmt.Fprintf(w, "  防护      : 上传≤%s  打包≤%s/%s  目录≤%s  并发%d  无进展超时 上传%s/下载%s  跨站校验%s\n",
		limitSize(cfg.UploadMaxSize),
		limitCount(cfg.ArchiveMaxItems), limitSize(cfg.ArchiveMaxBytes),
		limitCount(cfg.ListMaxEntries),
		cfg.MaxConcurrent,
		noProgress(cfg.UploadReadTimeout), noProgress(cfg.DownloadTimeout),
		perm("on", !cfg.DisableCSRFProtect),
	)
}

// noProgress 把「无进展超时」格式化成可读文案（0 表示不限制）。
func noProgress(d time.Duration) string {
	if d <= 0 {
		return "不限"
	}
	return d.Round(time.Second).String()
}

// limitSize 把字节上限格式化成可读文案，0 表示不限制。
func limitSize(n int64) string {
	if n <= 0 {
		return "不限"
	}
	const unit = 1024
	units := []string{"B", "KiB", "MiB", "GiB", "TiB", "PiB"}
	v := float64(n)
	i := 0
	for v >= unit && i < len(units)-1 {
		v /= unit
		i++
	}
	return fmt.Sprintf("%.0f%s", v, units[i])
}

// limitCount 把条目数上限格式化成可读文案，0 表示不限制。
func limitCount(n int) string {
	if n <= 0 {
		return "不限"
	}
	return fmt.Sprintf("%d条", n)
}
