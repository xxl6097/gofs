package gofs

import (
	"embed"
	"fmt"
	"io/fs"
)

// assetsFS 内嵌前端资源，保证单二进制分发。
//
//go:embed all:assets
var assetsFS embed.FS

// Assets 返回内嵌的前端资源，根目录即 HTTP 路径根。
//
// 想在内嵌资源的基础上改前端时可以拿它做参照；
// 要整体替换则用 WithAssetsDir 指向磁盘目录。
func Assets() (fs.FS, error) {
	sub, err := fs.Sub(assetsFS, "assets")
	if err != nil {
		return nil, fmt.Errorf("加载内嵌资源失败: %w", err)
	}
	return sub, nil
}
