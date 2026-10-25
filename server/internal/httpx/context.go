// Package httpx 里的请求上下文：从 context.Context 读取当前操作者。
//
// 服务层函数统一签名 func(ctx context.Context, ...) error，操作者经 context 传递，
// 不把 gin.Context 传进业务代码，也不用全局变量存当前用户。
package httpx

import "context"

type ctxKey int

const actorKey ctxKey = iota

// Actor 是本次请求的操作者。ID 为空表示未登录。
type Actor struct {
	UserID      string
	Username    string
	DisplayName string
	IsAdmin     bool
	// SessionID 是会话记录 ID，注销与「禁用账号即失效」按它撤销。
	SessionID string
}

// Authenticated 判断是否已登录。
func (a Actor) Authenticated() bool { return a.UserID != "" }

// ContextWithActor 把操作者写入请求上下文，由鉴权中间件调用。
func ContextWithActor(ctx context.Context, actor Actor) context.Context {
	return context.WithValue(ctx, actorKey, actor)
}

// ActorFrom 读取操作者，未登录时返回零值。
func ActorFrom(ctx context.Context) Actor {
	if v, ok := ctx.Value(actorKey).(Actor); ok {
		return v
	}
	return Actor{}
}
