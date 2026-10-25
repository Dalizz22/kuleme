// Package appdeps 定义各业务模块注册路由时能拿到的共享依赖。
//
// 单独成包是为了打断 import 环：bootstrap 需要 import 各模块，各模块的
// Register 又需要这个依赖类型。模块之间不允许互相 import handler。
package appdeps

import (
	"log/slog"

	"github.com/klm/server/internal/config"
	"github.com/klm/server/internal/httpx"
	"github.com/klm/server/internal/store"
)

// Deps 是模块注册时能拿到的全部依赖。
//
// 只放跨模块共享的东西：数据库、配置、日志、鉴权。模块自己的 service 在模块内构造。
type Deps struct {
	DB     *store.DB
	Config config.Config
	Logger *slog.Logger
	// Auth 是会话鉴权实现，供需要登录态的模块挂 httpx.Auth 中间件。
	// bootstrap 构造一次；为 nil 时模块应退化为显式读取 CurrentActor 并拒绝匿名。
	Auth httpx.AuthCarrier
}

// Available 报告数据库当前是否可用，模块据此返回 SERVICE_UNAVAILABLE。
func (d Deps) Available() bool { return d.DB != nil && d.DB.Available() }
