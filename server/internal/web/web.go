// Package web 内嵌 H5 前端产物：单二进制即可直接提供网页，根路径开箱可用。
// dist/ 在构建单二进制时用 apps/web 的构建产物覆盖（见 README「构建单二进制」）；
// 仓库内置占位页，未覆盖时也能编译、访问时给出指引。
package web

import (
	"embed"
	"io/fs"
)

//go:embed all:dist
var distFS embed.FS

// Dist 返回前端产物文件树。
func Dist() fs.FS {
	sub, err := fs.Sub(distFS, "dist")
	if err != nil {
		return distFS
	}
	return sub
}
