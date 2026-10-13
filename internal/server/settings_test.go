package server

import (
	"path/filepath"
	"testing"

	"github.com/xxl6097/gofs/internal/config"
)

// TestAllowsRootDerivedFromTopLevelStartRoot 回归「保存失败：400 目标目录超出允许范围」。
//
// 启动根在一级目录（如 /data）时，默认可切换范围推导为其父目录 "/"：
// 本应放行整棵树，却因为 within 把 "/" 拼成 "//" 而全部拒绝。
// 这里锁定住：此时任何绝对路径都应被放行。
func TestAllowsRootDerivedFromTopLevelStartRoot(t *testing.T) {
	if filepath.Separator != '/' {
		t.Skip("本用例按 POSIX 路径语义编写")
	}

	rs := newRuntimeSettings(&config.Config{}, "/data")

	if got := rs.AllowRoots(); len(got) != 1 || got[0] != "/" {
		t.Fatalf("AllowRoots() = %v, want [/]（启动根 /data 的父目录）", got)
	}
	for _, p := range []string{"/", "/data", "/data/sub", "/other/tree"} {
		if !rs.AllowsRoot(p) {
			t.Errorf("AllowsRoot(%q) = false，范围是 / 时应放行", p)
		}
	}
}

// TestAllowsRootNarrowedByExplicitAllowList 守住显式白名单的收窄语义：
// 写了 --root-allow 之后，范围外的路径必须被拒。
func TestAllowsRootNarrowedByExplicitAllowList(t *testing.T) {
	if filepath.Separator != '/' {
		t.Skip("本用例按 POSIX 路径语义编写")
	}

	rs := newRuntimeSettings(&config.Config{RootAllow: []string{"/srv"}}, "/srv/app")

	if !rs.AllowsRoot("/srv/app") || !rs.AllowsRoot("/srv/other") {
		t.Fatalf("/srv 之下应全部放行")
	}
	if rs.AllowsRoot("/etc") {
		t.Errorf("AllowsRoot(/etc) = true，应被 /srv 白名单拒绝")
	}
}
