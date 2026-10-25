package item

import (
	"context"
	"errors"
	"os"
	"strings"
	"sync"

	"github.com/klm/server/internal/authz"
	"github.com/klm/server/internal/httpx"
	"github.com/klm/server/internal/labels"
	"github.com/klm/server/internal/labels/kod"
	"github.com/klm/server/internal/store"
)

// CodeResolver 把扫码得到的编码换成对象引用，供 labels 模块注入使用。
//
// 放在 item 包而不是 labels 包：扫码解析要查 item/location 表并复用本包的
// 权限判定，labels 模块刻意只持有接口、不碰 ORM（见 labels.CodeResolver）。
type CodeResolver struct {
	db    *store.DB
	authz *authz.Checker
	// instanceIDPath 是实例身份文件；读不到时不做实例比对（保持旧码可用）。
	instanceIDPath string

	instOnce sync.Once
	instID   string
}

// NewCodeResolver 构造扫码解析实现。
//
// az 为 nil 时按 deps 自己构造；instanceIDPath 为空表示不校验实例前缀。
func NewCodeResolver(db *store.DB, az *authz.Checker, instanceIDPath string) *CodeResolver {
	if az == nil {
		az = authz.New(db)
	}
	return &CodeResolver{db: db, authz: az, instanceIDPath: instanceIDPath}
}

// ResolveCode 按码里的对象类型查对象并做读权限判定。
//
// 不可见与不存在一律返回 labels.ErrCodeNotFound：契约要求 200 + resolved=null，
// 不泄露存在性（docs/接口契约约定.md 第 3 节）。
func (r *CodeResolver) ResolveCode(ctx context.Context, actorID string, p kod.QRPayload) (labels.ResolvedCode, error) {
	if r == nil || r.db == nil || !r.db.Available() || actorID == "" {
		return labels.ResolvedCode{}, labels.ErrCodeNotFound
	}
	// 别的实例的码与本地公开码可能同形，先按实例前缀排除。
	if own := r.instanceID(); own != "" && p.InstanceID != "" && p.InstanceID != own {
		return labels.ResolvedCode{}, labels.ErrCodeNotFound
	}
	switch p.Kind {
	case kod.KindItem:
		return r.resolveItem(ctx, actorID, p)
	case kod.KindLocation:
		return r.resolveLocation(ctx, actorID, p)
	default:
		// 库码（S）当前没有公开码列，保持 resolved=null，不发明新语义。
		return labels.ResolvedCode{}, labels.ErrCodeNotFound
	}
}

func (r *CodeResolver) resolveItem(ctx context.Context, actorID string, p kod.QRPayload) (labels.ResolvedCode, error) {
	var row struct {
		ID         string
		SpaceID    string
		Name       string
		PublicCode string
	}
	err := r.db.Gorm.WithContext(ctx).Raw(
		`SELECT id, space_id, name, public_code FROM item
		 WHERE public_code = ? AND deleted_at IS NULL LIMIT 1`, p.Code).Scan(&row).Error
	if err != nil {
		return labels.ResolvedCode{}, err
	}
	if row.ID == "" {
		return labels.ResolvedCode{}, labels.ErrCodeNotFound
	}
	if err := r.authz.Check(ctx, httpx.Actor{UserID: actorID}, row.SpaceID, authz.ActionRead); err != nil {
		if errors.Is(err, authz.ErrNoSpace) {
			return labels.ResolvedCode{}, labels.ErrCodeNotFound
		}
		return labels.ResolvedCode{}, err
	}
	return labels.ResolvedCode{
		Kind:     "item",
		ObjectID: row.ID,
		ItemID:   row.ID,
		SpaceID:  row.SpaceID,
		Name:     row.Name,
		Subtitle: row.PublicCode,
		PageURL:  "/items/" + row.ID,
	}, nil
}

// resolveLocation 解析位置码。位置是实例级共享目录（GET /locations 同口径），
// 没有 space_id、也不按库过滤，所以这里不做授权判定：登录即可读。
func (r *CodeResolver) resolveLocation(ctx context.Context, actorID string, p kod.QRPayload) (labels.ResolvedCode, error) {
	var row struct {
		ID   string
		Name string
		Code string
	}
	err := r.db.Gorm.WithContext(ctx).Raw(
		`SELECT id, name, code FROM location
		 WHERE code = ? AND deleted_at IS NULL LIMIT 1`, p.Code).Scan(&row).Error
	if err != nil {
		return labels.ResolvedCode{}, err
	}
	if row.ID == "" {
		return labels.ResolvedCode{}, labels.ErrCodeNotFound
	}
	// 位置管理页在 web 的 /bases，没有 /locations 路由。
	return labels.ResolvedCode{
		Kind:     "location",
		ObjectID: row.ID,
		Name:     row.Name,
		Subtitle: row.Code,
		PageURL:  "/bases",
	}, nil
}

// instanceID 读一次实例身份文件并缓存；文件缺失返回空串（不比对）。
func (r *CodeResolver) instanceID() string {
	if r.instanceIDPath == "" {
		return ""
	}
	r.instOnce.Do(func() {
		data, err := os.ReadFile(r.instanceIDPath)
		if err != nil {
			return
		}
		id := strings.TrimSpace(string(data))
		if kod.ValidInstanceID(id) {
			r.instID = id
		}
	})
	return r.instID
}
