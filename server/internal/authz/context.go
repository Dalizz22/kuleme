package authz

import "github.com/gin-gonic/gin"

// ginHTTP 是 gin.Context 的别名，避免在业务包里散落 gin 导入路径。
// 只有路由层需要一个 HTTP 上下文时用它；纯服务函数一律用 context.Context。
type ginHTTP = gin.Context
