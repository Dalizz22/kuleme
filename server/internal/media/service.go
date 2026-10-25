package media

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"io"
	"io/fs"
	"log/slog"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	xdraw "golang.org/x/image/draw"
	"golang.org/x/image/webp"

	"github.com/klm/server/internal/authz"
	"github.com/klm/server/internal/config"
	"github.com/klm/server/internal/httpx"
	"github.com/klm/server/internal/store"
)

// Service 承担本模块全部业务：上传落盘、元数据查询、授权下载。
type Service struct {
	db     *store.DB
	logger *slog.Logger
	config config.Config
	authz  *authz.Checker
}

// New 构造服务，依赖与 auth 模块同款；authz 用于下载授权。
func New(db *store.DB, logger *slog.Logger, cfg config.Config) *Service {
	return &Service{db: db, logger: logger, config: cfg, authz: authz.New(db)}
}

// fileView 是对外 JSON 视图，字段 snake_case，不暴露磁盘路径。
type fileView struct {
	ID          string    `json:"id"`
	FileName    string    `json:"file_name"`
	ContentType string    `json:"content_type"`
	ByteSize    int64     `json:"byte_size"`
	CreatedAt   time.Time `json:"created_at"`
}

// fileRow 是下载时要读的元数据子集。
type fileRow struct {
	ID          string
	ContentType string
	StoragePath string
	// SpaceID 是文件所属库（关联后回填）；为空表示尚未关联、仍在上传者私有区。
	SpaceID    string `gorm:"column:space_id"`
	UploadedBy string `gorm:"column:uploaded_by"`
}

// Upload 校验大小与类型白名单（以磁盘嗅探为准）、按 uuid 落盘、写 file_object 元数据。
// 单条 INSERT 无需事务：契约第 4 节明确「图片上传不放在持锁事务内」。
func (s *Service) Upload(ctx context.Context, actor httpx.Actor, fh *multipart.FileHeader) (*fileView, error) {
	if !s.db.Available() {
		return nil, store.ErrUnavailable
	}
	if err := validateUploadSize(fh.Size, s.config.MaxUploadBytes); err != nil {
		return nil, err
	}
	id := uuid.NewString()
	name := storedName(id, filepath.Base(fh.Filename))
	dest := filepath.Join(s.config.MediaDir(), name)

	sum, err := saveFile(fh, dest)
	if err != nil {
		s.logger.Error("文件写盘失败", "error", err, "dest", dest)
		return nil, httpx.Internal(err)
	}
	// 类型白名单只认磁盘嗅探结果，不采信客户端声明的 Content-Type（可伪造）。
	sniffed := sniffFromDisk(dest)
	if !allowedMediaType(sniffed) {
		_ = os.Remove(dest) // 拒收就别留孤儿文件
		return nil, httpx.Validation("不支持的文件类型，仅支持图片（JPEG/PNG/WebP/GIF）与常见文档（PDF/CSV/TXT）")
	}
	// 图片入库前先真实解码：拒绝损坏图与超像素图（图片处理约定），
	// 顺带产出方向校正后的源图，供变体生成复用，避免二次解码。
	var source image.Image
	if mt := baseMediaType(sniffed); variantEnabled(mt) {
		img, err := s.prepareImageSource(dest, mt)
		if err != nil {
			_ = os.Remove(dest)
			return nil, err
		}
		source = img
	}
	// kind 固定 photo：上传入口目前只有物品照片用途；RETURNING 拿数据库时间。
	view := fileView{ID: id, FileName: name, ContentType: sniffed, ByteSize: fh.Size}
	err = s.db.Gorm.WithContext(ctx).Raw(
		`INSERT INTO file_object (id, space_id, uploaded_by, kind, content_type, byte_size, storage_path, sha256, created_at)
		 VALUES (?, NULL, ?, 'photo', ?, ?, ?, ?, now())
		 RETURNING created_at`,
		id, actor.UserID, view.ContentType, fh.Size, name, sum).Scan(&view.CreatedAt).Error
	if err != nil {
		_ = os.Remove(dest) // 元数据没写进去就别留孤儿文件
		s.logger.Error("文件元数据写入失败", "error", err, "file_id", id)
		return nil, httpx.Internal(err)
	}
	// 变体生成尽力而为：失败只记日志，不阻断上传，读取端按约定降级原图。
	if source != nil {
		s.generateVariants(id, source)
	}
	return &view, nil
}

// Get 读取下载元数据。记录不存在或 ID 非 uuid 一律 404，不区分报错原因。
func (s *Service) Get(ctx context.Context, id string) (*fileRow, error) {
	if _, err := uuid.Parse(id); err != nil {
		return nil, httpx.NotFound("文件不存在")
	}
	if !s.db.Available() {
		return nil, store.ErrUnavailable
	}
	var row fileRow
	// uploaded_by 是 uuid 列：'' 必须显式转 text，否则 COALESCE 把 '' 当 uuid 解析直接报 22P02。
	// space_id 同样可能是 NULL，显式转 text，与 uploaded_by 一个道理。
	res := s.db.Gorm.WithContext(ctx).Raw(
		`SELECT id, content_type, storage_path, COALESCE(space_id::text, '') AS space_id,
		        COALESCE(uploaded_by::text, '') AS uploaded_by
		   FROM file_object WHERE id = ?`, id).Scan(&row)
	if res.Error != nil {
		return nil, res.Error
	}
	if res.RowsAffected == 0 {
		return nil, httpx.NotFound("文件不存在")
	}
	return &row, nil
}

// AuthorizeDownload 下载授权：经 item_photo 找到引用该文件的
// 全部物品，连同文件自身 space_id，调用者对任一所在库有读权（authz.Check）即放行；
// 上传者例外只覆盖「未关联的私有文件」。其余一律 404——与 Get 同语义，
// 不向无权者泄露文件是否存在、类型、大小与时间。
func (s *Service) AuthorizeDownload(ctx context.Context, actor httpx.Actor, row *fileRow) error {
	var spaces []string
	if err := s.db.Gorm.WithContext(ctx).Raw(
		`SELECT DISTINCT i.space_id::text FROM item_photo ip
		 JOIN item i ON i.id = ip.item_id
		 WHERE ip.file_id = ?`, row.ID).Scan(&spaces).Error; err != nil {
		return err
	}
	// 文件已回填 space_id 但引用行缺失时（关联后物品照片被删），仍按所属库授权。
	if row.SpaceID != "" && !containsSpace(spaces, row.SpaceID) {
		spaces = append(spaces, row.SpaceID)
	}
	for _, sp := range spaces {
		if err := s.authz.Check(ctx, actor, sp, authz.ActionRead); err == nil {
			return nil
		}
	}
	if uploaderMayRead(row.SpaceID, row.UploadedBy, actor.UserID, len(spaces) > 0) {
		return nil
	}
	return httpx.NotFound("文件不存在")
}

// uploaderMayRead 是上传者例外：只有「没有库归属、也没被任何物品引用」的私有文件
// 才回落到上传者本人。已关联的正式文件按所属库授权，不因当初谁上传而继续可见
// （接口约定：上传成功 ≠ 已关联）。
func uploaderMayRead(spaceID, uploadedBy, actorID string, linked bool) bool {
	return spaceID == "" && !linked && uploadedBy != "" && uploadedBy == actorID
}

func containsSpace(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}

// List 分页返回文件元数据，默认排序 created_at DESC, id DESC（契约公共分页规则）。
// 按 actor 可见空间过滤——本人上传且未关联的、归属可见库的、
// 被可见库物品引用的；其余不可见。count 与 items 共用同一 where，
// 所以 total 是过滤后的数量，不会因分页回退泄露他人文件数量。
func (s *Service) List(ctx context.Context, actor httpx.Actor, limit, offset int) ([]fileView, int, error) {
	if !s.db.Available() {
		return nil, 0, store.ErrUnavailable
	}
	visible, err := s.authz.VisibleSpaceIDs(ctx, actor)
	if err != nil {
		return nil, 0, err
	}
	// 上传者分支必须收紧成「上传者本人 + 未关联的私有文件」：
	// 已关联的正式文件只按所属库授权，否则原上传者失去库权限后仍能从列表里
	// 看到文件 ID/类型/大小/时间。
	const where = `
WHERE (
        f.uploaded_by = ?
    AND f.space_id IS NULL
    AND NOT EXISTS (SELECT 1 FROM item_photo ip WHERE ip.file_id = f.id)
      )
   OR f.space_id::text IN ?
   OR EXISTS (
       SELECT 1 FROM item_photo ip JOIN item i ON i.id = ip.item_id
        WHERE ip.file_id = f.id AND i.space_id::text IN ?
   )`
	var total int64
	if err := s.db.Gorm.WithContext(ctx).Raw(
		`SELECT count(*) FROM file_object f `+where, actor.UserID, visible, visible).Scan(&total).Error; err != nil {
		return nil, 0, err
	}
	items := []fileView{}
	// storage_path 即落盘文件名（uuid+扩展名），别名成 file_name 直接扫进视图。
	err = s.db.Gorm.WithContext(ctx).Raw(
		`SELECT id, storage_path AS file_name, content_type, byte_size, created_at
		 FROM file_object f `+where+`
		 ORDER BY f.created_at DESC, f.id DESC
		 LIMIT ? OFFSET ?`, actor.UserID, visible, visible, limit, offset).Scan(&items).Error
	if err != nil {
		return nil, 0, err
	}
	return items, int(total), nil
}

// Delete 删除「上传者本人 + 未关联」的私有文件。
//
// 上传成功不等于已关联：失败批次留下的私有文件必须有出口清理，否则只增不减。
// 已关联的文件是正式资料，走物品照片接口，不在这里删（409 FILE_IN_USE）；
// 他人文件与不存在的 ID 一律 404，不泄露存在性。
func (s *Service) Delete(ctx context.Context, actor httpx.Actor, id string) error {
	if _, err := uuid.Parse(id); err != nil {
		return httpx.NotFound("文件不存在")
	}
	if !s.db.Available() {
		return store.ErrUnavailable
	}
	// 条件删除：把「本人上传 + 未归属 + 未被任何物品引用」写进同一条语句，
	// 避免先查后删之间被并发关联。
	var storagePath string
	res := s.db.Gorm.WithContext(ctx).Raw(
		`DELETE FROM file_object f
		  WHERE f.id = ? AND f.uploaded_by = ? AND f.space_id IS NULL
		    AND NOT EXISTS (SELECT 1 FROM item_photo ip WHERE ip.file_id = f.id)
		  RETURNING f.storage_path`, id, actor.UserID).Scan(&storagePath)
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		row, err := s.Get(ctx, id)
		if err != nil {
			return err
		}
		if row.UploadedBy != actor.UserID {
			return httpx.NotFound("文件不存在")
		}
		return httpx.Conflict(httpx.CodeFileInUse, "文件已关联到物品，不能删除")
	}
	// 元数据删掉后回收磁盘对象：原图与变体尽力而为，失败只留日志，
	// 不让磁盘故障把已经生效的删除回滚成 500。
	s.removeStored(id, storagePath)
	return nil
}

// removeStored 删除原图与已生成的变体文件。
func (s *Service) removeStored(id, storagePath string) {
	paths := []string{filepath.Join(s.config.MediaDir(), storagePath)}
	for _, maxEdge := range variantSpecs {
		paths = append(paths, filepath.Join(s.config.MediaDir(), variantName(id, maxEdge)))
	}
	for _, p := range paths {
		if err := os.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) && s.logger != nil {
			s.logger.Warn("删除文件对象失败", "error", err, "path", p)
		}
	}
}

// diskPath 拼绝对路径并确认文件还在盘上。元数据在而文件丢是服务端故障，报 500。
func (s *Service) diskPath(name string) (string, error) {
	path := filepath.Join(s.config.MediaDir(), name)
	if _, err := os.Stat(path); err != nil {
		s.logger.Error("盘上文件缺失", "error", err, "path", path)
		return "", httpx.Internal(err)
	}
	return path, nil
}

// ---- 图片变体（图片变体）----
//
// 存储约定（图片处理约定，不新增列、不走迁移）：变体与原图同放 media/ 目录，
// 按 <文件id>_<最长边>.jpg 命名，即 <id>_320.jpg / <id>_1600.jpg / <id>_2048.jpg。
// 读取端按约定取文件，不存在则降级原图；不查 file_object 列。

const (
	// jpegQuality 变体 JPEG 输出质量。
	jpegQuality = 82
	// maxImagePixels 解码后像素上限：64 MP（图片处理约定 单图 64MP，≤ 允许）。
	maxImagePixels = 64_000_000
)

// variantSpecs 固定三种变体的最长边：缩略图 320、预览 1600、AI 副本 2048。
var variantSpecs = [...]int{320, 1600, 2048}

// imageGate 服务端图像解码/变体 CPU 并发闸（图片处理约定：解码 worker 初始 1）。
// ponytail: 全局串行满足初始限额；吞吐不够再换可配置 worker 池。
var imageGate = make(chan struct{}, 1)

// variantName 由文件 id 与最长边拼变体文件名（统一 JPEG 输出）。
func variantName(id string, maxEdge int) string {
	return id + "_" + strconv.Itoa(maxEdge) + ".jpg"
}

// variantEnabled 判定是否为需要生成变体的图片类型；GIF 与文档不做变体。
func variantEnabled(mt string) bool {
	return mt == "image/jpeg" || mt == "image/png" || mt == "image/webp"
}

// baseMediaType 截掉嗅探结果里的分号参数，得到纯 MIME。
func baseMediaType(sniffed string) string {
	return strings.TrimSpace(strings.SplitN(sniffed, ";", 2)[0])
}

// exceedsPixelLimit 判断解码后像素数是否超过 64MP 上限（恰好 64MP 放行）。
func exceedsPixelLimit(w, h int) bool {
	return w > 0 && h > 0 && int64(w)*int64(h) > maxImagePixels
}

// variantSize 计算等比缩放目标尺寸：最长边压到 maxEdge，只缩不放大
// （原图最长边不超过 maxEdge 时原尺寸返回）。短边不足 1 时钳到 1。
func variantSize(w, h, maxEdge int) (int, int) {
	if w <= 0 || h <= 0 || (w <= maxEdge && h <= maxEdge) {
		return w, h
	}
	if w >= h {
		return maxEdge, max(1, int(int64(h)*int64(maxEdge)/int64(w)))
	}
	return max(1, int(int64(w)*int64(maxEdge)/int64(h))), maxEdge
}

// prepareImageSource 图片入库前校验与解码：像素限额（400 VALIDATION_FAILED）、
// 损坏拒绝（400）、EXIF 方向校正。调用方在失败时负责清理盘上文件。
func (s *Service) prepareImageSource(path, mt string) (image.Image, error) {
	imageGate <- struct{}{}
	defer func() { <-imageGate }()

	cfg, err := decodeImageConfig(path, mt)
	if err != nil {
		return nil, httpx.Validation("图片损坏或不是受支持的图片格式")
	}
	if exceedsPixelLimit(cfg.Width, cfg.Height) {
		return nil, httpx.Validation("图片像素超过上限（64 MP），请缩小后重新上传")
	}
	img, err := decodeImageFile(path, mt)
	if err != nil {
		return nil, httpx.Validation("图片损坏或不是受支持的图片格式")
	}
	if mt == "image/jpeg" {
		// 副本需校正 EXIF 方向（图片处理约定）；PNG/WebP 无 EXIF 方向概念。
		img = orientImage(img, jpegOrientation(path))
	}
	return img, nil
}

// decodeImageConfig 按嗅探到的 MIME 只读图片头，拿尺寸做像素限额判断。
func decodeImageConfig(path, mt string) (image.Config, error) {
	f, err := os.Open(path)
	if err != nil {
		return image.Config{}, err
	}
	defer f.Close()
	switch mt {
	case "image/jpeg":
		return jpeg.DecodeConfig(f)
	case "image/png":
		return png.DecodeConfig(f)
	case "image/webp":
		return webp.DecodeConfig(f)
	}
	return image.Config{}, errors.New("非图片类型")
}

// decodeImageFile 全量解码图片。输出统一交给变体管线处理，原图文件不动。
func decodeImageFile(path, mt string) (image.Image, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	switch mt {
	case "image/jpeg":
		return jpeg.Decode(f)
	case "image/png":
		return png.Decode(f)
	case "image/webp":
		return webp.Decode(f)
	}
	return nil, errors.New("非图片类型")
}

// generateVariants 落盘 320/1600/2048 三个 JPEG 变体：只缩不放大、透明合成白底、
// 重编码即去 EXIF（含 GPS）。单个变体失败只记日志，不影响其余变体与上传结果。
func (s *Service) generateVariants(id string, src image.Image) {
	imageGate <- struct{}{}
	defer func() { <-imageGate }()

	dir := s.config.MediaDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		s.logger.Error("媒体目录创建失败", "error", err, "dir", dir)
		return
	}
	for _, maxEdge := range variantSpecs {
		w, h := variantSize(src.Bounds().Dx(), src.Bounds().Dy(), maxEdge)
		if w <= 0 || h <= 0 {
			continue // 防御：解码出异常尺寸时不产出废变体
		}
		canvas := image.NewRGBA(image.Rect(0, 0, w, h))
		// 白底打底，透明像素在 JPEG（无 alpha）里合成后不透出黑底。
		xdraw.Draw(canvas, canvas.Bounds(), image.NewUniform(color.White), image.Point{}, xdraw.Src)
		xdraw.CatmullRom.Scale(canvas, canvas.Bounds(), src, src.Bounds(), xdraw.Over, nil)
		var buf bytes.Buffer
		if err := jpeg.Encode(&buf, canvas, &jpeg.Options{Quality: jpegQuality}); err != nil {
			s.logger.Error("变体编码失败", "error", err, "file_id", id, "max_edge", maxEdge)
			continue
		}
		name := variantName(id, maxEdge)
		if err := os.WriteFile(filepath.Join(dir, name), buf.Bytes(), 0o640); err != nil {
			s.logger.Error("变体写盘失败", "error", err, "file_id", id, "name", name)
		}
	}
}

// variantFile 按存储约定查找变体文件，不存在返回 false（读取端降级原图）。
func (s *Service) variantFile(id string, maxEdge int) (string, bool) {
	path := filepath.Join(s.config.MediaDir(), variantName(id, maxEdge))
	if st, err := os.Stat(path); err == nil && st.Mode().IsRegular() {
		return path, true
	}
	return "", false
}

// orientImage 按 EXIF 方向值（1-8）重排像素得到显示方向图；其余值原样返回。
// 映射口径：display(x,y) 取自 src(mapXY(x,y))，坐标基于解码图的 Min 偏移。
func orientImage(src image.Image, orientation int) image.Image {
	if orientation < 2 || orientation > 8 {
		return src
	}
	b := src.Bounds()
	w, h := b.Dx(), b.Dy()
	dw, dh := w, h
	if orientation >= 5 { // 5-8 旋转 90°，宽高互换
		dw, dh = h, w
	}
	var mapXY func(x, y int) (int, int)
	switch orientation {
	case 2: // 水平镜像
		mapXY = func(x, y int) (int, int) { return w - 1 - x, y }
	case 3: // 旋转 180°
		mapXY = func(x, y int) (int, int) { return w - 1 - x, h - 1 - y }
	case 4: // 垂直镜像
		mapXY = func(x, y int) (int, int) { return x, h - 1 - y }
	case 5: // 主对角线转置
		mapXY = func(x, y int) (int, int) { return y, x }
	case 6: // 顺时针 90°
		mapXY = func(x, y int) (int, int) { return y, h - 1 - x }
	case 7: // 反对角线转置
		mapXY = func(x, y int) (int, int) { return w - 1 - y, h - 1 - x }
	case 8: // 逆时针 90°
		mapXY = func(x, y int) (int, int) { return w - 1 - y, x }
	}
	dst := image.NewRGBA(image.Rect(0, 0, dw, dh))
	for y := 0; y < dh; y++ {
		for x := 0; x < dw; x++ {
			sx, sy := mapXY(x, y)
			dst.Set(x, y, src.At(b.Min.X+sx, b.Min.Y+sy))
		}
	}
	return dst
}

// jpegOrientation 从 JPEG 头部 APP1 EXIF 段读取方向标签（tag 0x0112）。
// 没有该段或值非法一律返回 1（不转）。只扫文件头 64KiB，EXIF 恒在前部。
func jpegOrientation(path string) int {
	f, err := os.Open(path)
	if err != nil {
		return 1
	}
	defer f.Close()
	head := make([]byte, 64<<10)
	n, _ := io.ReadFull(f, head)
	if n < 4 {
		return 1
	}
	data := head[:n]
	if data[0] != 0xFF || data[1] != 0xD8 { // 不是 SOI
		return 1
	}
	for pos := 2; pos+4 <= len(data); {
		if data[pos] != 0xFF {
			return 1
		}
		marker := data[pos+1]
		if marker == 0xD8 || (marker >= 0xD0 && marker <= 0xD7) { // 无长度的独立标记
			pos += 2
			continue
		}
		segLen := int(data[pos+2])<<8 | int(data[pos+3])
		if segLen < 2 || pos+2+segLen > len(data) {
			return 1
		}
		payload := data[pos+4 : pos+2+segLen]
		if marker == 0xE1 && len(payload) > 6 && string(payload[:6]) == "Exif\x00\x00" {
			if o := exifOrientation(payload[6:]); o >= 1 && o <= 8 {
				return o
			}
			return 1
		}
		pos += 2 + segLen
	}
	return 1
}

// exifOrientation 在 TIFF 头 + IFD0 条目中找 orientation，找不到/结构非法返回 1。
func exifOrientation(tiff []byte) int {
	if len(tiff) < 8 {
		return 1
	}
	var bo binary.ByteOrder = binary.LittleEndian
	switch {
	case tiff[0] == 'I' && tiff[1] == 'I':
	case tiff[0] == 'M' && tiff[1] == 'M':
		bo = binary.BigEndian
	default:
		return 1
	}
	if bo.Uint16(tiff[2:4]) != 0x2A { // TIFF 魔数 42
		return 1
	}
	ifd0 := int(bo.Uint32(tiff[4:8]))
	if ifd0 <= 0 || ifd0+2 > len(tiff) {
		return 1
	}
	count := int(bo.Uint16(tiff[ifd0 : ifd0+2]))
	for i := 0; i < count; i++ {
		off := ifd0 + 2 + i*12
		if off+12 > len(tiff) {
			break
		}
		entry := tiff[off : off+12]
		if bo.Uint16(entry[0:2]) != 0x0112 {
			continue
		}
		// 方向是 SHORT(3) ×1，值内联在前两字节；结构不符按无方向处理。
		if bo.Uint16(entry[2:4]) != 3 || bo.Uint32(entry[4:8]) < 1 {
			return 1
		}
		return int(bo.Uint16(entry[8:10]))
	}
	return 1
}

// validateUploadSize 校验上传大小：空文件拒绝，超限返回契约 413 FILE_TOO_LARGE。
func validateUploadSize(size, maxBytes int64) error {
	switch {
	case size <= 0:
		return httpx.Validation("不能上传空文件")
	case size > maxBytes:
		return httpx.New(http.StatusRequestEntityTooLarge, httpx.CodeFileTooLarge, "文件超过大小限制，请更换或缩小文件")
	}
	return nil
}

// storedName 由存储 ID 加原始扩展名生成落盘文件名。
// 0003 迁移的 file_object 没有 original_name 列且禁止新增迁移，
// 原始文件名只保留扩展名（小写、限长、剔除路径分隔符）。
func storedName(id, original string) string {
	ext := strings.ToLower(filepath.Ext(original))
	if len(ext) > 16 || strings.ContainsAny(ext, `/\`) {
		ext = ""
	}
	return id + ext
}

// allowedMediaTypes 上传类型白名单：图片 + 常见文档。
var allowedMediaTypes = map[string]bool{
	"image/jpeg":      true,
	"image/png":       true,
	"image/webp":      true,
	"image/gif":       true,
	"application/pdf": true,
	"text/csv":        true,
	"text/plain":      true,
}

// allowedMediaType 判定嗅探结果是否在白名单内；嗅探值形如
// "image/jpeg" 或 "text/plain; charset=utf-8"，截掉参数后比对。
func allowedMediaType(sniffed string) bool {
	return allowedMediaTypes[baseMediaType(sniffed)]
}

// sniffFromDisk 读文件头 512 字节做类型嗅探，读不到回退通用二进制。
func sniffFromDisk(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return "application/octet-stream"
	}
	defer f.Close()
	head := make([]byte, 512)
	n, _ := io.ReadFull(f, head)
	if n == 0 {
		return "application/octet-stream"
	}
	return http.DetectContentType(head[:n])
}

// saveFile 把上传内容写到 dest，单趟完成写盘与 SHA-256。
// O_EXCL 确保 uuid 名字不覆盖既有文件；创建成功之后的失败由这里自删半成品，
// 创建之前（如 EEXIST）的失败不删——dest 可能是别的记录正在用的文件。
func saveFile(fh *multipart.FileHeader, dest string) (sumHex string, err error) {
	src, err := fh.Open()
	if err != nil {
		return "", err
	}
	defer src.Close()
	dst, err := os.OpenFile(dest, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o640)
	if err != nil {
		return "", err
	}
	sum := sha256.New()
	if _, err = io.Copy(io.MultiWriter(dst, sum), src); err != nil {
		dst.Close()
		_ = os.Remove(dest)
		return "", err
	}
	if err = dst.Close(); err != nil {
		_ = os.Remove(dest)
		return "", err
	}
	return hex.EncodeToString(sum.Sum(nil)), nil
}
