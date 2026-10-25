package httpx

import (
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// UUIDParam 取路径参数并校验为合法 uuid。
// 路径里的资源地址不成立一律 404（不是格式错误）：既符合契约白名单，
// 也避免垃圾参数落到 SQL 把 uuid 列比较冒成 500。
func UUIDParam(c *gin.Context, name string) (string, *Error) {
	id := c.Param(name)
	if _, err := uuid.Parse(id); err != nil {
		return "", NotFound("资源不存在或已被删除")
	}
	return id, nil
}

// UUIDGuard 是路由级守卫：name 不是合法 uuid 时直接 404 并终止，
// 保证任何带资源 ID 的路由都不会把垃圾参数送进 SQL（uuid 列会 22P02 → 500）。
func UUIDGuard(name string) gin.HandlerFunc {
	return func(c *gin.Context) {
		if _, err := UUIDParam(c, name); err != nil {
			Abort(c, err)
			c.Abort()
			return
		}
		c.Next()
	}
}

// ValidateUUID 校验请求体里的 uuid 字段：非法一律 422，绝不让它进 SQL
// （uuid 列收到非 uuid 字符串会以 SQLSTATE 22P02 冒成 500）。
// required=false 时只校验非空值。
func ValidateUUID(name, value string, required bool) *Error {
	if value == "" {
		if required {
			return Validation("缺少必填字段 " + name)
		}
		return nil
	}
	if _, err := uuid.Parse(value); err != nil {
		return Validation("字段 " + name + " 必须是合法 ID")
	}
	return nil
}
