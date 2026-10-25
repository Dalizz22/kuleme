package httpx

import (
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"
)

// 分页默认值来自接口契约：page 从 1 开始，page_size 默认 20、最大 100。
const (
	defaultPageSize = 20
	maxPageSize     = 100
)

// Pagination 是解析后的分页参数。
type Pagination struct {
	Page     int
	PageSize int
	Offset   int
}

// ParsePagination 解析查询参数并夹取到合法范围。
func ParsePagination(c *gin.Context) Pagination {
	page := atoiDefault(c.Query("page"), 1)
	if page < 1 {
		page = 1
	}
	size := atoiDefault(c.Query("page_size"), defaultPageSize)
	if size < 1 {
		size = defaultPageSize
	}
	if size > maxPageSize {
		size = maxPageSize
	}
	return Pagination{Page: page, PageSize: size, Offset: (page - 1) * size}
}

// Limit 供 SQL LIMIT 使用。
func (p Pagination) Limit() int { return p.PageSize }

// IncludeDescendants 读取树查询范围开关，默认包含后代。
func IncludeDescendants(c *gin.Context) bool {
	raw := c.Query("include_descendants")
	if raw == "" {
		return true
	}
	v, err := strconv.ParseBool(raw)
	if err != nil {
		return true
	}
	return v
}

// BoolParam 读取可选布尔查询参数。
func BoolParam(c *gin.Context, key string, fallback bool) bool {
	raw := c.Query(key)
	if raw == "" {
		return fallback
	}
	v, err := strconv.ParseBool(raw)
	if err != nil {
		return fallback
	}
	return v
}

func atoiDefault(raw string, fallback int) int {
	if raw == "" {
		return fallback
	}
	v, err := strconv.Atoi(raw)
	if err != nil {
		return fallback
	}
	return v
}

// RetryAfter 设置 429 的等待提示。
func RetryAfter(c *gin.Context, seconds int) {
	c.Header("Retry-After", strconv.Itoa(seconds))
	Abort(c, RateLimited("操作过于频繁，请稍后再试"))
}

// NoContent 用于删除成功。
func NoContent(c *gin.Context) { c.Status(http.StatusNoContent) }
