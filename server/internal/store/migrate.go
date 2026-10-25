package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"sort"
	"strconv"
	"strings"
)

// Migration 是一个版本化迁移文件。
type Migration struct {
	Version int
	Name    string
	Up      string
}

// Migrate 按版本号顺序应用尚未执行的迁移，记录在 schema_migrations。
//
// 迁移文件用 go:embed 打包进二进制，不依赖运行机的文件系统；升级前应先备份，
// 回退采用「恢复备份 + 旧版本二进制」，不假设新 schema 能被旧程序读取。
func Migrate(ctx context.Context, db *DB, files fs.FS) error {
	if !db.Available() {
		return ErrUnavailable
	}
	migrations, err := loadMigrations(files)
	if err != nil {
		return err
	}

	// 迁移串行化：两个实例同时启动时只有一个执行 DDL。
	if _, err := db.SQL.ExecContext(ctx, `SELECT pg_advisory_lock($1)`, migrateLockKey); err != nil {
		return fmt.Errorf("获取迁移锁: %w", err)
	}
	defer func() {
		_, _ = db.SQL.ExecContext(context.WithoutCancel(ctx), `SELECT pg_advisory_unlock($1)`, migrateLockKey)
	}()

	if _, err := db.SQL.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS schema_migrations (
			version     integer PRIMARY KEY,
			name        text NOT NULL,
			applied_at  timestamptz NOT NULL DEFAULT now()
		)`); err != nil {
		return fmt.Errorf("建立 schema_migrations: %w", err)
	}

	applied, err := appliedVersions(ctx, db.SQL)
	if err != nil {
		return err
	}

	for _, m := range migrations {
		if applied[m.Version] {
			continue
		}
		if err := applyOne(ctx, db.SQL, m); err != nil {
			return err
		}
	}
	return nil
}

const migrateLockKey int64 = 4711001

func applyOne(ctx context.Context, db *sql.DB, m Migration) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("迁移 %04d 开启事务: %w", m.Version, err)
	}
	defer func() { _ = tx.Rollback() }()

	// 无参数的简单查询：PostgreSQL 允许一次发送多条以分号分隔的语句，
	// 因此迁移文件可以按自然 SQL 编写，不需要语句分隔器。
	if _, err := tx.ExecContext(ctx, m.Up); err != nil {
		return fmt.Errorf("迁移 %04d %s: %w", m.Version, m.Name, err)
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO schema_migrations (version, name) VALUES ($1, $2)`,
		m.Version, m.Name,
	); err != nil {
		return fmt.Errorf("记录迁移 %04d: %w", m.Version, err)
	}
	return tx.Commit()
}

func appliedVersions(ctx context.Context, db *sql.DB) (map[int]bool, error) {
	rows, err := db.QueryContext(ctx, `SELECT version FROM schema_migrations`)
	if err != nil {
		return nil, fmt.Errorf("读取已应用迁移: %w", err)
	}
	defer func() { _ = rows.Close() }()

	applied := make(map[int]bool)
	for rows.Next() {
		var v int
		if err := rows.Scan(&v); err != nil {
			return nil, err
		}
		applied[v] = true
	}
	return applied, rows.Err()
}

// loadMigrations 读取 0001_xxx.up.sql 形式的文件。只执行 Up 部分；
// 需要回退时按文档恢复升级前备份，不在运行期执行 Down。
func loadMigrations(files fs.FS) ([]Migration, error) {
	entries, err := fs.Glob(files, "*.sql")
	if err != nil {
		return nil, err
	}
	out := make([]Migration, 0, len(entries))
	for _, name := range entries {
		base := path.Base(name)
		if !strings.HasSuffix(base, ".up.sql") {
			continue
		}
		version, label, err := parseFileName(base)
		if err != nil {
			return nil, err
		}
		body, err := fs.ReadFile(files, name)
		if err != nil {
			return nil, err
		}
		out = append(out, Migration{Version: version, Name: label, Up: string(body)})
	}
	if len(out) == 0 {
		return nil, errors.New("没有找到任何迁移文件")
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Version < out[j].Version })
	for i := 1; i < len(out); i++ {
		if out[i].Version == out[i-1].Version {
			return nil, fmt.Errorf("迁移版本号重复: %04d", out[i].Version)
		}
	}
	return out, nil
}

func parseFileName(base string) (int, string, error) {
	trimmed := strings.TrimSuffix(base, ".up.sql")
	parts := strings.SplitN(trimmed, "_", 2)
	if len(parts) != 2 {
		return 0, "", fmt.Errorf("迁移文件名不符合 NNNN_name.up.sql: %s", base)
	}
	version, err := strconv.Atoi(parts[0])
	if err != nil {
		return 0, "", fmt.Errorf("迁移文件版本号不是数字: %s", base)
	}
	return version, strings.ReplaceAll(parts[1], "_", " "), nil
}
