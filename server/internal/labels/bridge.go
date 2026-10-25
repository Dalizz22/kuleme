// 标签打印桥：把 labels 模块的窄接口接到核心服务上。
//
// FileRecorder 把标签 PDF 落成 file_object（kind=label_pdf）；
// FileLocator 下载/补打时重新做库级权限检查（权限变化旧文件失效）；
// SpaceAuthorizer 复用 authz.Checker。生成→落盘→下载→补打全链路真实可用，
// 不再 503 降级。
package labels

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"

	"github.com/google/uuid"

	"github.com/klm/server/internal/authz"
	"github.com/klm/server/internal/httpx"
	"github.com/klm/server/internal/store"
)

// recorderBridge 把标签 PDF 落成 file_object（kind=label_pdf）。
type recorderBridge struct {
	db      *store.DB
	dataDir string
}

// NewBridges 构造三件套，供 bootstrap 接线。
func NewBridges(db *store.DB, dataDir string) (FileRecorder, FileLocator, SpaceAuthorizer) {
	az := authz.New(db)
	return &recorderBridge{db: db, dataDir: dataDir},
		&locatorBridge{db: db, authz: az, dataDir: dataDir},
		&authorizerBridge{authz: az}
}

// Create 存字节、算哈希、登记 file_object。写权限校验由 labels service 完成。
func (b *recorderBridge) Create(ctx context.Context, actorID, spaceID string, req FileCreateRequest) (string, error) {
	sum := sha256.Sum256(req.Bytes)
	// media 约定：storage_path 相对 dataDir/media/ 目录。
	id := uuid.NewString()
	rel := id + ".pdf"
	dest := filepath.Join(b.dataDir, "media", rel)
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return "", err
	}
	if err := os.WriteFile(dest, req.Bytes, 0o600); err != nil {
		return "", err
	}
	err := b.db.Gorm.WithContext(ctx).Exec(
		`INSERT INTO file_object (id, space_id, uploaded_by, kind, content_type, byte_size, storage_path, sha256, attached)
		 VALUES (?, NULLIF(?,'')::uuid, ?, 'label_pdf', 'application/pdf', ?, ?, ?, true)`,
		id, spaceID, actorID, len(req.Bytes), rel, hex.EncodeToString(sum[:])).Error
	if err != nil {
		_ = os.Remove(dest)
		return "", err
	}
	return id, nil
}

// locatorBridge 下载/补打时重新做权限检查（硬要求：权限变化旧文件失效）。
type locatorBridge struct {
	db      *store.DB
	authz   *authz.Checker
	dataDir string
}

func (b *locatorBridge) Locate(ctx context.Context, actorID, fileID string) (FileInfo, error) {
	var row struct {
		SpaceID     *string `gorm:"column:space_id"`
		ByteSize    int64   `gorm:"column:byte_size"`
		StoragePath string  `gorm:"column:storage_path"`
	}
	err := b.db.Gorm.WithContext(ctx).Raw(
		`SELECT space_id, byte_size, storage_path FROM file_object WHERE id = ? AND kind = 'label_pdf'`, fileID).
		Scan(&row).Error
	if err != nil {
		return FileInfo{}, err
	}
	if row.StoragePath == "" {
		return FileInfo{}, httpx.NotFound("文件不存在")
	}
	if row.SpaceID != nil && *row.SpaceID != "" {
		if err := b.authz.Check(ctx, httpx.Actor{UserID: actorID}, *row.SpaceID, authz.ActionRead); err != nil {
			return FileInfo{}, err
		}
	}
	if _, err := os.Stat(filepath.Join(b.dataDir, "media", row.StoragePath)); err != nil {
		return FileInfo{}, httpx.NotFound("文件不存在")
	}
	return FileInfo{ID: fileID, ByteSize: row.ByteSize,
		ContentType: "application/pdf",
		DownloadURL: "/api/v1/files/" + fileID + "/content"}, nil
}

// authorizerBridge 复用 authz.Checker。
type authorizerBridge struct {
	authz *authz.Checker
}

func (b *authorizerBridge) RequireRead(ctx context.Context, actorID, spaceID string) error {
	return b.authz.Check(ctx, httpx.Actor{UserID: actorID}, spaceID, authz.ActionRead)
}

var (
	_ FileRecorder    = (*recorderBridge)(nil)
	_ FileLocator     = (*locatorBridge)(nil)
	_ SpaceAuthorizer = (*authorizerBridge)(nil)
)
