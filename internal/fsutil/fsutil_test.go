package fsutil

import (
	"path/filepath"
	"testing"
)

// TestIsWithin 覆盖路径包含判断的边界。
//
// 重点是 root 为文件系统根 "/" 的情形：它是「可切换范围」的常见取值
// （启动根在一级目录下时，推导出的父目录就是 "/"），
// 一旦判错，白名单会从「放行整棵树」变成「谁都不放行」。
func TestIsWithin(t *testing.T) {
	if filepath.Separator != '/' {
		t.Skip("本用例按 POSIX 路径语义编写；Windows 卷根语义不同，不在覆盖范围")
	}

	cases := []struct {
		base string
		p    string
		want bool
	}{
		// 根："/" 应当放行其下的一切（含自身）。
		{"/", "/", true},
		{"/", "/data", true},
		{"/", "/Users/x/y", true},

		// 普通目录：自身与后代放行，同级或父级不放行。
		{"/srv/data", "/srv/data", true},
		{"/srv/data", "/srv/data/sub", true},
		{"/srv/data", "/srv", false},
		{"/srv/data", "/srv/datax", false},
		{"/srv", "/srv", true},
		{"/srv", "/srv/x", true},

		// 前缀相同但并非同一棵树：不能被字符串前缀骗过。
		{"/srv", "/srvx", false},
		{"/srv", "/srvx/y", false},

		// 空串一律不放行。
		{"", "/x", false},
		{"/x", "", false},
	}

	for _, c := range cases {
		if got := IsWithin(c.base, c.p); got != c.want {
			t.Errorf("IsWithin(%q, %q) = %v, want %v", c.base, c.p, got, c.want)
		}
	}
}
