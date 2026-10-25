// Package store 提供数据库连接与版本化迁移。
//
// 数据库不可达时服务仍可启动并回答 /system/info 与健康检查，业务接口返回
// SERVICE_UNAVAILABLE。这样错误配置的地址不会表现为「服务不存在」，也便于
// 先起容器后连库。
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// ErrUnavailable 表示数据库当前不可达。
var ErrUnavailable = errors.New("数据库不可用")

// DB 持有 *gorm.DB。Connect 失败时 DB 为 nil，调用方必须先检查 Available。
type DB struct {
	Gorm *gorm.DB
	SQL  *sql.DB
}

// Connect 建立连接池并做一次真实探测。探测失败不返回错误本身，
// 而是返回一个不可用句柄，让进程继续提供只读的系统信息接口。
func Connect(ctx context.Context, dsn string) (*DB, error) {
	if dsn == "" {
		return &DB{}, nil
	}

	sqlDB, err := sql.Open("pgx", dsn)
	if err != nil {
		return &DB{}, fmt.Errorf("打开连接池: %w", err)
	}
	sqlDB.SetMaxOpenConns(25)
	sqlDB.SetMaxIdleConns(5)
	sqlDB.SetConnMaxLifetime(time.Hour)

	pingCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if err := sqlDB.PingContext(pingCtx); err != nil {
		_ = sqlDB.Close()
		return &DB{}, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}

	gormDB, err := gorm.Open(postgres.New(postgres.Config{Conn: sqlDB}), &gorm.Config{
		Logger:                 logger.Default.LogMode(logger.Warn),
		SkipDefaultTransaction: true,
	})
	if err != nil {
		_ = sqlDB.Close()
		return &DB{}, fmt.Errorf("初始化 GORM: %w", err)
	}
	return &DB{Gorm: gormDB, SQL: sqlDB}, nil
}

// Available 表示连接可用。
func (d *DB) Available() bool { return d != nil && d.Gorm != nil }

// Close 关闭连接池。
func (d *DB) Close() error {
	if d == nil || d.SQL == nil {
		return nil
	}
	return d.SQL.Close()
}

// Ping 用于健康检查。
func (d *DB) Ping(ctx context.Context) error {
	if !d.Available() {
		return ErrUnavailable
	}
	return d.SQL.PingContext(ctx)
}

// Tx 在同一事务内执行 fn。业务写操作必须经此入口，保证余额、流水与
// 幂等结果同事务提交。
func (d *DB) Tx(ctx context.Context, fn func(tx *gorm.DB) error) error {
	if !d.Available() {
		return ErrUnavailable
	}
	return d.Gorm.WithContext(ctx).Transaction(fn)
}
