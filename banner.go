package gofs

import (
	"fmt"
	"io"
	"path/filepath"
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

	root := cfg.ServePath
	if abs, err := filepath.Abs(root); err == nil {
		root = abs
	}

	perm := func(name string, on bool) string {
		if on {
			return name
		}
		return "-"
	}
	fmt.Fprintf(w, "gofs %s\n", Version)
	fmt.Fprintf(w, "  监听      : %s://%s\n", s.Scheme(), addr)
	fmt.Fprintf(w, "  服务目录  : %s\n", root)
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
	// 把生效中的防护值打出来：这些默认值直接决定了服务被滥用时的表现，
	// 显式可见比藏在 --help 里更让人放心。
	fmt.Fprintf(w, "  防护      : 上传≤%s  打包≤%s/%s  目录≤%s  并发%d  无进展超时%s  跨站校验%s\n",
		limitSize(cfg.UploadMaxSize),
		limitCount(cfg.ArchiveMaxItems), limitSize(cfg.ArchiveMaxBytes),
		limitCount(cfg.ListMaxEntries),
		cfg.MaxConcurrent,
		cfg.DownloadTimeout,
		perm("on", !cfg.DisableCSRFProtect),
	)
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
