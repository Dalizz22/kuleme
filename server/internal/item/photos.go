// 物品照片关联/解除/排序/主图。
//
// 语义（api/openapi.yaml /items/{id}/photos*）：
//   - 上传先落私有文件（space_id 为空、attached=false），关联成功才回填库归属；
//   - 同一文件已挂在别的物品 → 409 PHOTO_ALREADY_LINKED，不静默改挂；
//   - 跨库关联 → 403（文件已归属别的库，或目标库无写权限）；
//   - 主图唯一（is_primary 单条），
//   - 解除关联不删文件：文件重新变回「上传者的可追踪私有文件」，由上传者自行删除或重挂。
package item

import (
	"context"
	"errors"
	"strings"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/klm/server/internal/authz"
	"github.com/klm/server/internal/httpx"
)

// CodePhotoAlreadyLinked 见 docs/接口契约约定.md §5（409 业务阻止原因）。
const CodePhotoAlreadyLinked = "PHOTO_ALREADY_LINKED"

// invalidPhoto 是照片入参的校验错误：本轮写路径校验类统一 422（契约要求
// 排序/关联的入参问题回 422 VALIDATION_FAILED，不再用遗留的 400）。
func invalidPhoto(message string) *httpx.Error {
	return httpx.New(422, httpx.CodeValidationFailed, message)
}

// photoContentURL 是受控访问地址：/files/{id}/content 走 media 的下载授权。
func photoContentURL(fileID string) string {
	return "/api/v1/files/" + fileID + "/content"
}

// photoView 是 PhotoRef（openapi components.schemas.PhotoRef）。
type photoView struct {
	FileID    string `json:"file_id"`
	IsPrimary bool   `json:"is_primary"`
	Sort      int    `json:"sort"`
	URL       string `json:"url"`
}

type photoLinkInput struct {
	FileIDs       []string `json:"file_ids"`
	PrimaryFileID string   `json:"primary_file_id"`
}

type photoOrderInput struct {
	FileIDs []string `json:"file_ids"`
}

// normalizeFileIDs 去重前的入参校验：非空、合法 uuid、不许重复。
func normalizeFileIDs(in []string) ([]string, error) {
	if len(in) == 0 {
		return nil, invalidPhoto("file_ids 不能为空")
	}
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, raw := range in {
		id := strings.TrimSpace(raw)
		if _, err := uuid.Parse(id); err != nil {
			return nil, invalidPhoto("file_ids 含非法文件 ID")
		}
		if _, dup := seen[id]; dup {
			return nil, invalidPhoto("file_ids 含重复文件 ID")
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	return out, nil
}

// validatePhotoOrder 要求 file_ids 恰好是现有照片的完整集合（不缺项、不多项）。
func validatePhotoOrder(existing, want []string) error {
	if len(existing) != len(want) {
		return invalidPhoto("file_ids 必须是该物品现有照片的完整集合")
	}
	set := make(map[string]struct{}, len(existing))
	for _, id := range existing {
		set[id] = struct{}{}
	}
	for _, id := range want {
		if _, ok := set[id]; !ok {
			return invalidPhoto("file_ids 必须是该物品现有照片的完整集合")
		}
	}
	return nil
}

// itemSpaceForWrite 校验物品存在且调用者有该库写权限；跨库/无权一律 404（不泄露存在性）。
func (s *itemService) itemSpaceForWrite(ctx context.Context, actor httpx.Actor, itemID string) (string, error) {
	if _, err := uuid.Parse(itemID); err != nil {
		return "", httpx.NotFound("物品不存在")
	}
	var spaceID string
	res := s.db.Gorm.WithContext(ctx).Raw(
		`SELECT i.space_id::text FROM item i WHERE i.id = ? AND i.deleted_at IS NULL`, itemID).Scan(&spaceID)
	if res.Error != nil {
		return "", res.Error
	}
	if res.RowsAffected == 0 {
		return "", httpx.NotFound("物品不存在")
	}
	if err := s.authz.Check(ctx, actor, spaceID, authz.ActionWrite); err != nil {
		if errors.Is(err, authz.ErrNoSpace) {
			return "", httpx.NotFound("物品不存在")
		}
		return "", err
	}
	return spaceID, nil
}

// photoList 按 sort 升序返回（openapi：恰好一条 is_primary=true）。
func (s *itemService) photoList(ctx context.Context, itemID string) ([]photoView, error) {
	rows := []photoView{}
	if err := s.db.Gorm.WithContext(ctx).Raw(
		`SELECT file_id::text AS file_id, is_primary, sort FROM item_photo
		  WHERE item_id = ? ORDER BY sort, file_id`, itemID).Scan(&rows).Error; err != nil {
		return nil, err
	}
	for i := range rows {
		rows[i].URL = photoContentURL(rows[i].FileID)
	}
	return rows, nil
}

// ListPhotos 读物品全部照片（刷新后的照片面板入口）。
// 物品可见性与 GET /items/:id 一致：非成员/不存在/无读权一律 404。
func (s *itemService) ListPhotos(ctx context.Context, actor httpx.Actor, itemID string) ([]photoView, error) {
	if _, err := s.Get(ctx, actor, itemID); err != nil {
		return nil, err
	}
	return s.photoList(ctx, itemID)
}

// LinkPhotos 关联照片：校验归属 → 写关联 → 回填库归属，全在同一事务。
func (s *itemService) LinkPhotos(ctx context.Context, actor httpx.Actor, itemID string, in photoLinkInput) ([]photoView, error) {
	spaceID, err := s.itemSpaceForWrite(ctx, actor, itemID)
	if err != nil {
		return nil, err
	}
	ids, err := normalizeFileIDs(in.FileIDs)
	if err != nil {
		return nil, err
	}
	primary := strings.TrimSpace(in.PrimaryFileID)
	if primary == "" {
		primary = ids[0]
	} else if _, err := uuid.Parse(primary); err != nil {
		return nil, invalidPhoto("primary_file_id 非法")
	} else if !containsID(ids, primary) {
		return nil, invalidPhoto("primary_file_id 必须出现在 file_ids 中")
	}
	err = s.db.Gorm.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		return s.linkPhotosTx(ctx, tx, actor, itemID, spaceID, ids, primary)
	})
	if err != nil {
		return nil, err
	}
	return s.photoList(ctx, itemID)
}

// linkPhotosTx 是关联的事务体（独立方法便于集成测试直接打规则）。
// 不负责权限判定：调用方已确认对 spaceID 有写权限。
func (s *itemService) linkPhotosTx(ctx context.Context, tx *gorm.DB, actor httpx.Actor, itemID, spaceID string, ids []string, primary string) error {
	type fileRow struct {
		ID         string
		SpaceID    string
		Kind       string
		UploadedBy string
		LinkedTo   string
	}
	var files []fileRow
	if err := tx.WithContext(ctx).Raw(
		`SELECT f.id::text AS id, COALESCE(f.space_id::text, '') AS space_id, f.kind::text AS kind,
		        COALESCE(f.uploaded_by::text, '') AS uploaded_by,
		        COALESCE((SELECT ip.item_id::text FROM item_photo ip WHERE ip.file_id = f.id LIMIT 1), '') AS linked_to
		   FROM file_object f WHERE f.id IN ?`, ids).Scan(&files).Error; err != nil {
		return err
	}
	if len(files) != len(ids) {
		return httpx.NotFound("文件不存在")
	}
	for _, f := range files {
		if f.Kind != "photo" {
			return invalidPhoto("只能关联照片类型的文件")
		}
		// 未归属的私有文件只允许上传者本人认领（连 admin 也不例外）。
		// 否则任意库的 writer 拿别人的私有 file_id 挂到自己物品，全库成员就获得了读权。
		if f.SpaceID == "" && f.UploadedBy != actor.UserID {
			return httpx.Forbidden("文件不属于该库")
		}
		// 跨库关联：文件已归属别的库。归属本库才允许。
		if f.SpaceID != "" && f.SpaceID != spaceID {
			return httpx.Forbidden("文件不属于该库")
		}
		// 已挂别的物品：绝不静默改挂（原物品的读权不因这次请求改变）。
		if f.LinkedTo != "" && f.LinkedTo != itemID {
			return httpx.New(409, CodePhotoAlreadyLinked, "照片已关联到其他物品，不能静默改挂")
		}
	}
	var maxSort int
	if err := tx.WithContext(ctx).Raw(
		`SELECT COALESCE(MAX(sort), 0) FROM item_photo WHERE item_id = ?`, itemID).Scan(&maxSort).Error; err != nil {
		return err
	}
	linked := make(map[string]struct{}, len(files))
	for _, f := range files {
		if f.LinkedTo == itemID {
			linked[f.ID] = struct{}{}
		}
		// 该文件尚未挂在本物品：追加到末尾（sort 递增）。
		if f.LinkedTo == "" {
			maxSort++
			if err := tx.WithContext(ctx).Exec(
				`INSERT INTO item_photo (item_id, file_id, sort, is_primary)
				 VALUES (?, ?, ?, false)
				 ON CONFLICT (item_id, file_id) DO NOTHING`, itemID, f.ID, maxSort).Error; err != nil {
				return err
			}
			linked[f.ID] = struct{}{}
		}
	}
	// 回填库归属：关联成功才让文件进该库（上传成功 ≠ 已关联）。
	if err := tx.WithContext(ctx).Exec(
		`UPDATE file_object SET space_id = ?, attached = true WHERE id IN ? AND kind = 'photo'`,
		spaceID, ids).Error; err != nil {
		return err
	}
	// 主图唯一：先清空再设，避免撞 item_photo_primary_key 唯一索引。
	if _, ok := linked[primary]; ok {
		if err := tx.WithContext(ctx).Exec(
			`UPDATE item_photo SET is_primary = false WHERE item_id = ?`, itemID).Error; err != nil {
			return err
		}
		if err := tx.WithContext(ctx).Exec(
			`UPDATE item_photo SET is_primary = true WHERE item_id = ? AND file_id = ?`,
			itemID, primary).Error; err != nil {
			return err
		}
	}
	return nil
}

// UnlinkPhoto 解除关联：不删文件，主图被删后由服务端指定下一张。
func (s *itemService) UnlinkPhoto(ctx context.Context, actor httpx.Actor, itemID, fileID string) ([]photoView, error) {
	if _, err := s.itemSpaceForWrite(ctx, actor, itemID); err != nil {
		return nil, err
	}
	if _, err := uuid.Parse(fileID); err != nil {
		return nil, httpx.NotFound("照片未关联到该物品")
	}
	err := s.db.Gorm.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		return s.unlinkPhotoTx(ctx, tx, itemID, fileID)
	})
	if err != nil {
		return nil, err
	}
	return s.photoList(ctx, itemID)
}

func (s *itemService) unlinkPhotoTx(ctx context.Context, tx *gorm.DB, itemID, fileID string) error {
	var wasPrimary bool
	if err := tx.WithContext(ctx).Raw(
		`SELECT is_primary FROM item_photo WHERE item_id = ? AND file_id = ?`,
		itemID, fileID).Scan(&wasPrimary).Error; err != nil {
		return err
	}
	res := tx.WithContext(ctx).Exec(
		`DELETE FROM item_photo WHERE item_id = ? AND file_id = ?`, itemID, fileID)
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return httpx.NotFound("照片未关联到该物品")
	}
	if wasPrimary {
		if err := tx.WithContext(ctx).Exec(
			`UPDATE item_photo SET is_primary = true
			  WHERE item_id = ?
			    AND file_id = (SELECT file_id FROM item_photo WHERE item_id = ? ORDER BY sort, file_id LIMIT 1)`,
			itemID, itemID).Error; err != nil {
			return err
		}
	}
	// 没有任何物品再引用它 → 退回私有文件，上传者可以继续查看/删除/重挂（可追踪回收）。
	return tx.WithContext(ctx).Exec(
		`UPDATE file_object f SET space_id = NULL, attached = false
		  WHERE f.id = ? AND NOT EXISTS (SELECT 1 FROM item_photo ip WHERE ip.file_id = f.id)`, fileID).Error
}

// ReorderPhotos 全量提交排序：缺项/多项/不属于该物品 → 422，不部分应用。
func (s *itemService) ReorderPhotos(ctx context.Context, actor httpx.Actor, itemID string, in photoOrderInput) ([]photoView, error) {
	if _, err := s.itemSpaceForWrite(ctx, actor, itemID); err != nil {
		return nil, err
	}
	ids, err := normalizeFileIDs(in.FileIDs)
	if err != nil {
		return nil, err
	}
	existing, err := s.photoFileIDs(ctx, itemID)
	if err != nil {
		return nil, err
	}
	if err := validatePhotoOrder(existing, ids); err != nil {
		return nil, err
	}
	err = s.db.Gorm.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		for i, fid := range ids {
			if err := tx.WithContext(ctx).Exec(
				`UPDATE item_photo SET sort = ? WHERE item_id = ? AND file_id = ?`,
				i+1, itemID, fid).Error; err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return s.photoList(ctx, itemID)
}

// SetPrimaryPhoto 设主图：清空本条物品旧主图再设，保证 is_primary 单条。
func (s *itemService) SetPrimaryPhoto(ctx context.Context, actor httpx.Actor, itemID, fileID string) ([]photoView, error) {
	if _, err := s.itemSpaceForWrite(ctx, actor, itemID); err != nil {
		return nil, err
	}
	if _, err := uuid.Parse(fileID); err != nil {
		return nil, httpx.NotFound("照片未关联到该物品")
	}
	err := s.db.Gorm.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var exists bool
		if err := tx.WithContext(ctx).Raw(
			`SELECT true FROM item_photo WHERE item_id = ? AND file_id = ?`, itemID, fileID).
			Scan(&exists).Error; err != nil {
			return err
		}
		if !exists {
			return httpx.NotFound("照片未关联到该物品")
		}
		if err := tx.WithContext(ctx).Exec(
			`UPDATE item_photo SET is_primary = false WHERE item_id = ?`, itemID).Error; err != nil {
			return err
		}
		return tx.WithContext(ctx).Exec(
			`UPDATE item_photo SET is_primary = true WHERE item_id = ? AND file_id = ?`,
			itemID, fileID).Error
	})
	if err != nil {
		return nil, err
	}
	return s.photoList(ctx, itemID)
}

func (s *itemService) photoFileIDs(ctx context.Context, itemID string) ([]string, error) {
	var ids []string
	if err := s.db.Gorm.WithContext(ctx).Raw(
		`SELECT file_id::text FROM item_photo WHERE item_id = ? ORDER BY sort, file_id`, itemID).
		Scan(&ids).Error; err != nil {
		return nil, err
	}
	return ids, nil
}

func containsID(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}
