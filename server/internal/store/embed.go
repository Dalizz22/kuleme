package store

import (
	"embed"
	"fmt"
	"io/fs"
)

// 迁移文件的唯一可编辑位置是 server/migrations/。go:embed 不能引用包外路径，
// 因此 internal/store/migrations/ 保存一份逐字节相同的副本供打包。
// 修改 server/migrations/ 后必须把改动复制到 internal/store/migrations/（两处内容不一致
// 就会出现「二进制里的迁移与源文件不同步」，这也是启动时校验的原因）。
//
//go:embed all:migrations
var migrationFiles embed.FS

// MigrationFS 把迁移文件打包进二进制：部署时不需要额外带 migrations 目录，
// 也不会出现「二进制与迁移文件版本不一致」。启动时若目录缺失会立刻失败，
// 而不是带着空迁移集静默跑起来。
var MigrationFS fs.FS = mustSub()

func mustSub() fs.FS {
	sub, err := fs.Sub(migrationFiles, "migrations")
	if err != nil {
		panic(fmt.Sprintf("迁移文件未打包：internal/store/migrations 缺失或为空，请确认它与 server/migrations 内容一致后重新编译: %v", err))
	}
	return sub
}
