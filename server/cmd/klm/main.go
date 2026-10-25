// Command klm 是「库了么」服务端入口。
//
// 职责只有三件事：读配置、建数据库连接、把路由交给 bootstrap。
// 业务模块由 bootstrap 接线，入口不出现任何业务判断。
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/klm/server/internal/auth"
	"github.com/klm/server/internal/bootstrap"
	"github.com/klm/server/internal/config"
	"github.com/klm/server/internal/rules"
	"github.com/klm/server/internal/store"
)

func main() {
	if err := run(); err != nil {
		slog.Error("服务退出", "error", err)
		os.Exit(1)
	}
}

func run() error {
	cfg := config.Load()
	logger := newLogger()
	slog.SetDefault(logger)

	if err := cfg.EnsureDirs(); err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// 数据库不可达不阻塞启动：服务仍回答 /system/info 与健康检查，业务接口返回 503。
	db, err := store.Connect(ctx, cfg.DatabaseURL)
	if err != nil {
		logger.Warn("数据库暂不可用，服务以受限模式启动", "error", err)
	}
	defer func() { _ = db.Close() }()

	if err := bootstrap.Migrate(ctx, db, logger); err != nil {
		logger.Error("迁移失败", "error", err)
		return err
	}

	// 首次启动生成一次性初始化凭据；明文只在启动日志出现一次，不落库。
	if db.Available() {
		res, err := auth.New(db, logger, cfg).EnsureSetupCredential(ctx)
		switch {
		case err != nil:
			logger.Warn("初始化凭据处理失败", "error", err)
		case res.Secret != "":
			logger.Info("首次启动：请用一次性凭据完成初始化", "setup_secret", res.Secret)
		}
	}

	router := bootstrap.NewRouter(bootstrap.Deps{DB: db, Config: cfg, Logger: logger})

	// 规则 worker：同进程静态 worker（租约=SKIP LOCKED 原子认领）。
	// 周期：30s 跑队列 + 每分钟扫 date 规则；崩溃恢复靠 run_key 幂等与步骤留痕。
	rulesSvc := rules.NewService(db)
	workerCtx, workerStop := context.WithCancel(ctx)
	defer workerStop()
	go func() {
		ticker := time.NewTicker(rulesWorkerInterval)
		defer ticker.Stop()
		scanner := time.NewTicker(rulesScanInterval)
		defer scanner.Stop()
		logger.Info("规则 worker 已启动")
		for {
			select {
			case <-workerCtx.Done():
				return
			case <-ticker.C:
				rulesSvc.RunWorker(workerCtx)
			case <-scanner.C:
				rulesSvc.ScanDateRules(workerCtx)
			}
		}
	}()
	srv := &http.Server{
		Addr:              cfg.Addr,
		Handler:           router,
		ReadHeaderTimeout: 10 * time.Second,
		// 图片上传与导出下载耗时较长，写超时留足余量而不是无限等待。
		WriteTimeout: 5 * time.Minute,
		IdleTimeout:  2 * time.Minute,
	}

	errCh := make(chan error, 1)
	_ = workerStop
	go func() {
		logger.Info("服务启动", "addr", cfg.Addr, "api_version", cfg.APIVersion)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
	}

	// 优雅停止：给在途请求最多 20 秒收尾，超时后强制关闭而不是无限等待。
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	logger.Info("正在停止服务")
	return srv.Shutdown(shutdownCtx)
}

const (
	rulesWorkerInterval = 30 * time.Second
	rulesScanInterval   = time.Minute
)

func newLogger() *slog.Logger {
	level := slog.LevelInfo
	if os.Getenv("KLM_DEBUG") != "" {
		level = slog.LevelDebug
	}
	return slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: level}))
}
