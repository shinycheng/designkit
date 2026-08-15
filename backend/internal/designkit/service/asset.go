package service

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"image"
	"io"
	"log/slog"
	"strings"
	"time"

	// 注册解码器，让 image.DecodeConfig 能读出宽高。
	// 只读文件头，不解整张图，所以对内存没有压力。
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"

	_ "golang.org/x/image/bmp"
	_ "golang.org/x/image/webp"

	dkdomain "github.com/Wei-Shaw/sub2api/internal/designkit/domain"
)

// ============================================================================
// 商品图（素材）
// ============================================================================
//
// 三件事：
//
//	1. 上传：限大小、算 sha256 去重、**探测真实格式**（不信客户端的 Content-Type）、
//	   落对象存储、写 designkit_assets。
//	2. 预处理：按 (ratio, keep_transparency, max_dimension) 找 variant，
//	   命中就复用，没命中才调 Python 服务，产物写 designkit_asset_variants。
//	3. 出图比例白名单校验：读 designkit_settings.ratios。
//	   **不要指望 Python** —— imgsvc 的 parse_aspect 接受任意 W:H（"7:13" 也照收），
//	   里面的 SUPPORTED_RATIOS 只是错误提示文案用的常量，不是护栏。

const (
	// assetMaxUploadBytesSettingKey 单张上传上限（字节）。
	//
	// ⚠ 9001 迁移里**没有**预置这一项（那个文件跑过就永不可改），
	// 所以读不到时用 defaultAssetMaxUploadBytes 兜底；要改就新写一条
	// 9xxx 迁移 INSERT 进 designkit_settings，或在管理端 PutSetting。
	assetMaxUploadBytesSettingKey = "max_upload_bytes"

	// defaultAssetMaxUploadBytes 20MB。
	//
	// 这个数跟三处对齐，改一处就要三处一起改：
	//   - 界面提示文案「20MB 以内」（CLAUDE.md 第二·五节 E）
	//   - Python 侧 DESIGNKIT_IMGSVC_MAX_UPLOAD_BYTES（默认也是 20MB）
	//   - 我方错误文案 DK_IMAGE_TOO_LARGE「请压到 20MB 以内」
	// 把它调到比 Python 侧大，后果是「这里放行、预处理阶段才 413」，
	// 运营已经等了一轮上传才看到失败。
	defaultAssetMaxUploadBytes int64 = 20 << 20

	// assetMinUploadBytesLimit / assetMaxUploadBytesLimit 配置值的合理区间。
	// 管理员把它填成 0 或者 10GB 都不接受，直接退回默认值。
	assetMinUploadBytesLimit int64 = 64 << 10
	assetMaxUploadBytesLimit int64 = 100 << 20

	// assetMinDimensionLimit / assetMaxDimensionLimit max_dimension 的合理区间，
	// 跟 Python 侧 DESIGNKIT_IMGSVC_MIN/MAX_MAX_DIMENSION 的默认值一致。
	// 超出区间 Python 会返 400，不如在这里就退回默认值。
	assetMinDimensionLimit = 256
	assetMaxDimensionLimit = 8192

	// assetDefaultListLimit / assetMaxListLimit 列表分页。
	assetDefaultListLimit = 20
	assetMaxListLimit     = 100
)

// AssetServiceDeps 建 AssetService 要的依赖。
type AssetServiceDeps struct {
	// Assets 商品图仓储，必填。
	Assets dkdomain.AssetRepository
	// Settings 配置仓储，必填（比例白名单、尺寸上限都从这里读）。
	Settings dkdomain.SettingRepository
	// Store 对象存储，必填。群晖上是本地磁盘实现。
	Store dkdomain.ObjectStore
	// Preprocessor Python 预处理服务客户端。
	// 允许为 nil：没配置时 EnsureVariant 直接失败（fail-closed），
	// 而不是把没补边的原图发出去。
	Preprocessor dkdomain.ImagePreprocessor
	// Now 取当前时间，测试可以塞固定值。为 nil 用 time.Now。
	Now func() time.Time
	// NewUID 生成 26 位 ULID。为 nil 用内置实现。
	NewUID func() string
}

// AssetService 商品图上传与预处理。
type AssetService struct {
	assets       dkdomain.AssetRepository
	settings     dkdomain.SettingRepository
	store        dkdomain.ObjectStore
	preprocessor dkdomain.ImagePreprocessor
	now          func() time.Time
	newUID       func() string
}

// NewAssetService 建服务。缺必填依赖直接报错，不延迟到运行时才 nil panic。
func NewAssetService(deps AssetServiceDeps) (*AssetService, error) {
	if deps.Assets == nil {
		return nil, errors.New("designkit: AssetService 缺少 Assets 仓储")
	}
	if deps.Settings == nil {
		return nil, errors.New("designkit: AssetService 缺少 Settings 仓储")
	}
	if deps.Store == nil {
		return nil, errors.New("designkit: AssetService 缺少对象存储")
	}
	now := deps.Now
	if now == nil {
		now = time.Now
	}
	newUID := deps.NewUID
	if newUID == nil {
		newUID = newAssetULID
	}
	return &AssetService{
		assets:       deps.Assets,
		settings:     deps.Settings,
		store:        deps.Store,
		preprocessor: deps.Preprocessor,
		now:          now,
		newUID:       newUID,
	}, nil
}

// ----------------------------------------------------------------------------
// 上传
// ----------------------------------------------------------------------------

// UploadAssetInput 一次上传。
type UploadAssetInput struct {
	// UserID 谁传的。
	UserID int64
	// Origin web（界面）/ erp（接口）。留空按 web 处理。
	Origin dkdomain.Origin
	// Filename 客户端给的文件名，只用来取扩展名做参考，不用来定格式。
	Filename string
	// ClientContentType 客户端声明的 MIME。
	//
	// ⚠ **只记进日志，绝不作为判断依据。** 浏览器对 HEIC 常常报
	// image/jpeg，Mac 导出的 HEIC 还经常顶着 .jpg 扩展名。
	// 真实格式一律靠字节头判断。
	ClientContentType string
	// DeclaredSize 客户端声明的字节数（multipart 的 Size），<=0 表示未知。
	// 只用来「早点拒绝」，真正的限额靠读取时的 LimitReader 兜住。
	DeclaredSize int64
	// Data 图片字节流。
	Data io.Reader
}

// UploadAssetResult 上传结果。
type UploadAssetResult struct {
	// Asset 落库的（或复用到的）商品图。
	Asset *dkdomain.Asset
	// Reused true = 同一个人之前传过一模一样的图，直接复用了旧记录，
	// 没有新写对象存储、也没有新插一行。界面可以照常显示，不用提示。
	Reused bool
}

// UploadAsset 接收一张商品图。
//
// 顺序是刻意的：**先限大小 → 再探真实格式 → 再算 sha256 去重 → 最后才落盘**。
// 去重放在落盘前，同一个人重复上传同一张图（运营从素材库里反复拖同一张，
// 很常见）就不会在磁盘上留几十份副本。
func (s *AssetService) UploadAsset(ctx context.Context, in UploadAssetInput) (*UploadAssetResult, error) {
	if in.UserID <= 0 {
		return nil, dkdomain.NewError(dkdomain.ErrCodeUnauthorized)
	}
	if in.Data == nil {
		return nil, dkdomain.NewError(dkdomain.ErrCodeInvalidRequest).
			WithMessage("没有收到图片内容，请重新选择文件。")
	}
	origin := in.Origin
	if origin == "" {
		origin = dkdomain.OriginWeb
	}
	if !origin.IsValid() {
		return nil, dkdomain.NewError(dkdomain.ErrCodeInvalidRequest)
	}

	limit := s.MaxUploadBytes(ctx)
	if in.DeclaredSize > limit {
		return nil, tooLargeError(limit)
	}

	// 多读 1 个字节：读满了就说明超限。
	// 只信 LimitReader 不信 DeclaredSize —— 后者是客户端说了算的。
	data, err := io.ReadAll(io.LimitReader(in.Data, limit+1))
	if err != nil {
		return nil, dkdomain.NewError(dkdomain.ErrCodeInvalidRequest).
			WithMessage("读取上传的图片失败，请重试。").WithCause(err)
	}
	if int64(len(data)) > limit {
		return nil, tooLargeError(limit)
	}
	if len(data) == 0 {
		return nil, dkdomain.NewError(dkdomain.ErrCodeInvalidRequest).
			WithMessage("上传的文件是空的，请重新选择。")
	}

	info, ok := dkDetectImageInfo(data)
	if !ok {
		slog.Info("designkit 拒绝了一个认不出格式的上传",
			slog.Int64("user_id", in.UserID),
			slog.String("filename", in.Filename),
			slog.String("client_content_type", in.ClientContentType),
			slog.Int("bytes", len(data)),
		)
		return nil, dkdomain.NewError(dkdomain.ErrCodeUnsupportedImageFormat)
	}
	if in.ClientContentType != "" && !strings.EqualFold(in.ClientContentType, info.ContentType) {
		// 不报错，只记一笔：这几乎总是浏览器/系统的锅，不是运营的错。
		slog.Debug("designkit 上传的 Content-Type 与真实格式不符，以真实格式为准",
			slog.String("client", in.ClientContentType),
			slog.String("detected", info.ContentType),
			slog.String("format", info.Format),
		)
	}

	sum := sha256.Sum256(data)
	digest := hex.EncodeToString(sum[:])

	if existing, err := s.assets.FindAssetBySHA256(ctx, in.UserID, digest); err == nil && existing != nil {
		// 复用前确认文件还在：记录在、文件没了的话复用回去就是一张打不开的图。
		if existing.DeletedAt == nil {
			exists, existsErr := s.store.Exists(ctx, existing.ObjectKey)
			if existsErr == nil && exists {
				return &UploadAssetResult{Asset: existing, Reused: true}, nil
			}
			if existsErr != nil {
				slog.Warn("designkit 检查已有素材文件失败，按新图处理",
					slog.String("object_key", existing.ObjectKey), slog.Any("error", existsErr))
			}
		}
	} else if err != nil && !errors.Is(err, dkdomain.ErrNotFound) {
		return nil, dkdomain.NewError(dkdomain.ErrCodeInternal).WithCause(err)
	}

	createdAt := s.now().UTC()
	uid := s.newUID()
	objectKey := dkdomain.AssetObjectKey(createdAt, uid, info.Ext)

	if err := s.store.Put(ctx, objectKey, info.ContentType, data); err != nil {
		return nil, dkdomain.NewError(dkdomain.ErrCodeStorageError).WithCause(err)
	}

	asset := &dkdomain.Asset{
		UID:         uid,
		UserID:      in.UserID,
		ObjectKey:   objectKey,
		ContentType: info.ContentType,
		ByteSize:    int64(len(data)),
		SHA256:      &digest,
		Origin:      origin,
		CreatedAt:   createdAt,
	}
	// HEIC 这类 Go 标准库读不出来的，宽高留空（列本来就允许 NULL）。
	// 真实尺寸在预处理之后由 designkit_asset_variants 记着。
	if info.Width > 0 && info.Height > 0 {
		w, h := info.Width, info.Height
		asset.Width = &w
		asset.Height = &h
	}

	if err := s.assets.CreateAsset(ctx, asset); err != nil {
		// 入库失败就把刚写进去的文件删掉，别留孤儿文件占盘。
		if delErr := s.store.Delete(ctx, objectKey); delErr != nil {
			slog.Warn("designkit 回滚素材文件失败，留下了一个孤儿文件",
				slog.String("object_key", objectKey), slog.Any("error", delErr))
		}
		return nil, dkdomain.NewError(dkdomain.ErrCodeInternal).WithCause(err)
	}
	return &UploadAssetResult{Asset: asset}, nil
}

// tooLargeError 带上实际上限的中文提示。
func tooLargeError(limit int64) *dkdomain.DesignkitError {
	return dkdomain.NewError(dkdomain.ErrCodeImageTooLarge).
		WithMessagef("图片太大了，请压到 %d MB 以内再上传。", limit>>20)
}

// ----------------------------------------------------------------------------
// 读取
// ----------------------------------------------------------------------------

// GetAsset 按对外编号取一张商品图（校验归属）。
func (s *AssetService) GetAsset(ctx context.Context, userID int64, uid string) (*dkdomain.Asset, error) {
	asset, err := s.assets.GetAssetByUID(ctx, userID, uid)
	if err != nil {
		return nil, mapAssetRepoError(err)
	}
	return asset, nil
}

// AssetContent 取原图字节，给 GET /assets/:uid/content 用。
//
// 记录在、文件不在时返回 DK_STORAGE_ERROR 而不是「找不到」——
// 那是一个需要管理员知道的故障，报成「找不到」会被当成正常的删除。
func (s *AssetService) AssetContent(ctx context.Context, userID int64, uid string) ([]byte, string, error) {
	asset, err := s.GetAsset(ctx, userID, uid)
	if err != nil {
		return nil, "", err
	}
	data, contentType, err := s.store.Get(ctx, asset.ObjectKey)
	if err != nil {
		return nil, "", dkdomain.NewError(dkdomain.ErrCodeStorageError).WithCause(err)
	}
	if contentType == "" {
		contentType = asset.ContentType
	}
	return data, contentType, nil
}

// ListAssets 商品图列表（游标分页）。Limit 会被收敛到 [1, 100]。
func (s *AssetService) ListAssets(ctx context.Context, query dkdomain.ListAssetsQuery) ([]*dkdomain.Asset, error) {
	if query.Limit <= 0 {
		query.Limit = assetDefaultListLimit
	}
	if query.Limit > assetMaxListLimit {
		query.Limit = assetMaxListLimit
	}
	assets, err := s.assets.ListAssets(ctx, query)
	if err != nil {
		return nil, dkdomain.NewError(dkdomain.ErrCodeInternal).WithCause(err)
	}
	return assets, nil
}

// DeleteAsset 软删一张商品图。
//
// **刻意不删对象存储里的文件**：历史任务的 item 还引用着它，
// 详情页要能看到「当时用的是哪张图」。图片永久保留是拍板过的（决策 17）。
func (s *AssetService) DeleteAsset(ctx context.Context, userID int64, uid string) error {
	if err := s.assets.SoftDeleteAsset(ctx, userID, uid); err != nil {
		return mapAssetRepoError(err)
	}
	return nil
}

// ----------------------------------------------------------------------------
// 预处理（补白边到目标比例）
// ----------------------------------------------------------------------------

// EnsureVariantInput 预处理一张商品图。
type EnsureVariantInput struct {
	// UserID 谁的图（校验归属）。
	UserID int64
	// AssetUID 商品图对外编号。
	AssetUID string
	// Ratio 目标比例，必须在 designkit_settings.ratios 白名单里。
	Ratio dkdomain.Ratio
	// KeepTransparency true = 保留透明；false = 合成白底。
	KeepTransparency bool
	// MaxDimension 最长边上限。<=0 表示按 designkit_settings.max_dimension 取。
	MaxDimension int
}

// EnsureVariant 拿到「这张图按这个比例补好边」的产物，没有就现做一个。
//
// 比例白名单在这里校验（读 designkit_settings.ratios）。
func (s *AssetService) EnsureVariant(ctx context.Context, in EnsureVariantInput) (*dkdomain.AssetVariant, error) {
	asset, err := s.GetAsset(ctx, in.UserID, in.AssetUID)
	if err != nil {
		return nil, err
	}
	return s.EnsureVariantForAsset(ctx, asset, in.Ratio, in.KeepTransparency, in.MaxDimension)
}

// EnsureVariantForAsset 同 EnsureVariant，但直接吃一个已经取好的 Asset。
// 出图 worker 手上有的是 asset 行不是 uid，走这个入口省一次查询。
func (s *AssetService) EnsureVariantForAsset(
	ctx context.Context,
	asset *dkdomain.Asset,
	ratio dkdomain.Ratio,
	keepTransparency bool,
	maxDimension int,
) (*dkdomain.AssetVariant, error) {
	if asset == nil || asset.ID <= 0 {
		return nil, dkdomain.NewError(dkdomain.ErrCodeAssetNotFound)
	}

	// —— 比例护栏。Python 侧不做这件事，这里是唯一的关口 ——
	allowed := s.AllowedRatios(ctx)
	if err := dkdomain.ValidateRatio(ratio, allowed); err != nil {
		return nil, dkdomain.NewError(dkdomain.ErrCodeRatioNotAllowed).
			WithMessagef("不支持 %q 这个出图比例，请从 %s 里选一个。", ratio.String(), joinRatios(allowed)).
			WithCause(err)
	}

	if maxDimension <= 0 {
		maxDimension = s.MaxDimension(ctx)
	}
	if maxDimension < assetMinDimensionLimit || maxDimension > assetMaxDimensionLimit {
		return nil, dkdomain.NewError(dkdomain.ErrCodePreprocessFailed).
			WithMessagef("图片尺寸上限配置成了 %d，不在 %d~%d 之间，请联系管理员改设置。",
				maxDimension, assetMinDimensionLimit, assetMaxDimensionLimit)
	}

	// —— 命中就复用，不重复调 Python（省时间也省内存）——
	existing, err := s.assets.GetVariant(ctx, asset.ID, ratio, keepTransparency, maxDimension)
	if err != nil && !errors.Is(err, dkdomain.ErrNotFound) {
		return nil, dkdomain.NewError(dkdomain.ErrCodeInternal).WithCause(err)
	}
	if existing != nil {
		ok, existsErr := s.store.Exists(ctx, existing.ObjectKey)
		if existsErr != nil {
			slog.Warn("designkit 检查预处理产物失败，重新生成一份",
				slog.String("object_key", existing.ObjectKey), slog.Any("error", existsErr))
		} else if ok {
			return existing, nil
		}
	}

	if s.preprocessor == nil {
		// fail-closed：没有预处理服务就不出图，绝不拿原图硬上。
		// 拿原图硬上 = 运营选 3:4 拿回 4:3、钱照扣，而且不报错。
		return nil, dkdomain.NewError(dkdomain.ErrCodePreprocessFailed).
			WithMessage("图像处理服务没有配置，暂时无法出图，请联系管理员。")
	}

	source, sourceContentType, err := s.store.Get(ctx, asset.ObjectKey)
	if err != nil {
		return nil, dkdomain.NewError(dkdomain.ErrCodeStorageError).WithCause(err)
	}
	if sourceContentType == "" {
		sourceContentType = asset.ContentType
	}

	result, err := s.preprocessor.Preprocess(ctx, dkdomain.PreprocessRequest{
		Data:             source,
		Filename:         asset.UID + dkExtForContentType(asset.ContentType),
		ContentType:      sourceContentType,
		Ratio:            ratio,
		KeepTransparency: keepTransparency,
		// ⚠ 显式带上，值来自 designkit_settings。省了它就会
		// 「界面说 4K、实际按 2K 出图」且不报错。
		MaxDimension: maxDimension,
	})
	if err != nil {
		// 这里**直接把错误抛上去**，调用方（出图 worker）据此把该 item 置 failed。
		// 绝不 fallback 到原图。
		return nil, err
	}
	if result == nil || len(result.Data) == 0 {
		return nil, dkdomain.NewError(dkdomain.ErrCodePreprocessFailed)
	}

	createdAt := s.now().UTC()
	variantKey := dkdomain.VariantObjectKey(
		createdAt, asset.UID, ratio, keepTransparency, maxDimension,
		dkExtForContentType(result.ContentType),
	)
	if err := s.store.Put(ctx, variantKey, result.ContentType, result.Data); err != nil {
		return nil, dkdomain.NewError(dkdomain.ErrCodeStorageError).WithCause(err)
	}

	params := dkdomain.UpsertVariantParams{
		AssetID:          asset.ID,
		Ratio:            ratio,
		KeepTransparency: keepTransparency,
		MaxDimension:     maxDimension,
		ObjectKey:        variantKey,
		ContentType:      result.ContentType,
	}
	if result.Width > 0 && result.Height > 0 {
		w, h := result.Width, result.Height
		params.Width = &w
		params.Height = &h
	}

	variant, err := s.assets.UpsertVariant(ctx, params)
	if err != nil {
		if delErr := s.store.Delete(ctx, variantKey); delErr != nil {
			slog.Warn("designkit 回滚预处理产物失败，留下了一个孤儿文件",
				slog.String("object_key", variantKey), slog.Any("error", delErr))
		}
		return nil, dkdomain.NewError(dkdomain.ErrCodeInternal).WithCause(err)
	}
	return variant, nil
}

// VariantContent 取预处理产物的字节（出图前要把它塞进 multipart）。
func (s *AssetService) VariantContent(ctx context.Context, variant *dkdomain.AssetVariant) ([]byte, string, error) {
	if variant == nil {
		return nil, "", dkdomain.NewError(dkdomain.ErrCodeAssetNotFound)
	}
	data, contentType, err := s.store.Get(ctx, variant.ObjectKey)
	if err != nil {
		return nil, "", dkdomain.NewError(dkdomain.ErrCodeStorageError).WithCause(err)
	}
	if contentType == "" {
		contentType = variant.ContentType
	}
	return data, contentType, nil
}

// ----------------------------------------------------------------------------
// 配置读取
// ----------------------------------------------------------------------------
//
// 三个都**不返回 error**：配置读不到时退回默认值并记一条日志，
// 不能因为设置表抖了一下就把上传和出图全挡掉。
// 唯一真相仍是数据库里的值，默认值只是兜底。

// AllowedRatios 读 designkit_settings.ratios。
func (s *AssetService) AllowedRatios(ctx context.Context) []dkdomain.Ratio {
	raw, ok := s.settingRaw(ctx, dkdomain.SettingKeyRatios)
	if !ok {
		return dkdomain.DefaultRatios()
	}
	var values []string
	if err := json.Unmarshal(raw, &values); err != nil {
		slog.Warn("designkit 配置 ratios 解析失败，退回默认比例",
			slog.String("value", string(raw)), slog.Any("error", err))
		return dkdomain.DefaultRatios()
	}
	out := make([]dkdomain.Ratio, 0, len(values))
	for _, v := range values {
		parsed, err := dkdomain.ParseRatio(v)
		if err != nil {
			slog.Warn("designkit 配置 ratios 里有不合法的比例，已跳过", slog.String("ratio", v))
			continue
		}
		out = append(out, parsed)
	}
	if len(out) == 0 {
		return dkdomain.DefaultRatios()
	}
	return out
}

// MaxDimension 读 designkit_settings.max_dimension。
//
// ⚠ 这个数直接决定出图落 2K 还是 4K 计费档，所以取到之后必须**显式传给 Python**。
func (s *AssetService) MaxDimension(ctx context.Context) int {
	raw, ok := s.settingRaw(ctx, dkdomain.SettingKeyMaxDimension)
	if !ok {
		return dkdomain.DefaultMaxDimension
	}
	var value int
	if err := json.Unmarshal(raw, &value); err != nil || value < assetMinDimensionLimit || value > assetMaxDimensionLimit {
		slog.Warn("designkit 配置 max_dimension 不可用，退回默认值",
			slog.String("value", string(raw)), slog.Int("default", dkdomain.DefaultMaxDimension))
		return dkdomain.DefaultMaxDimension
	}
	return value
}

// MaxUploadBytes 读 designkit_settings.max_upload_bytes（9001 迁移里没有预置，
// 读不到就是 20MB）。handler 拿它做 multipart 的上限，界面拿它写提示文案。
func (s *AssetService) MaxUploadBytes(ctx context.Context) int64 {
	raw, ok := s.settingRaw(ctx, assetMaxUploadBytesSettingKey)
	if !ok {
		return defaultAssetMaxUploadBytes
	}
	var value int64
	if err := json.Unmarshal(raw, &value); err != nil || value < assetMinUploadBytesLimit || value > assetMaxUploadBytesLimit {
		slog.Warn("designkit 配置 max_upload_bytes 不可用，退回默认值",
			slog.String("value", string(raw)), slog.Int64("default", defaultAssetMaxUploadBytes))
		return defaultAssetMaxUploadBytes
	}
	return value
}

// settingRaw 取一个配置项的原始 JSON；不存在或出错返回 ok=false。
func (s *AssetService) settingRaw(ctx context.Context, key string) (json.RawMessage, bool) {
	setting, err := s.settings.GetSetting(ctx, key)
	if err != nil {
		if !errors.Is(err, dkdomain.ErrNotFound) {
			slog.Warn("designkit 读取配置失败，使用默认值",
				slog.String("key", key), slog.Any("error", err))
		}
		return nil, false
	}
	if setting == nil || len(setting.Value) == 0 {
		return nil, false
	}
	return setting.Value, true
}

// joinRatios 把比例列表拼成给运营看的文案，例如 `1:1、3:4、4:3`。
func joinRatios(ratios []dkdomain.Ratio) string {
	parts := make([]string, 0, len(ratios))
	for _, r := range ratios {
		parts = append(parts, r.String())
	}
	return strings.Join(parts, "、")
}

// mapAssetRepoError 把仓储的哨兵错误翻译成带中文文案的我方错误。
func mapAssetRepoError(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, dkdomain.ErrNotFound):
		return dkdomain.NewError(dkdomain.ErrCodeAssetNotFound).WithCause(err)
	default:
		return dkdomain.NewError(dkdomain.ErrCodeInternal).WithCause(err)
	}
}

// ----------------------------------------------------------------------------
// 图片格式探测
// ----------------------------------------------------------------------------

// dkImageInfo 是「这堆字节到底是什么图」的判断结果。
type dkImageInfo struct {
	// Format 小写格式名：png / jpeg / webp / gif / bmp / heic / heif / avif。
	Format string
	// ContentType 真实 MIME。
	ContentType string
	// Ext 带点的扩展名，用来拼 object_key。
	Ext string
	// Width / Height 像素。Go 读不出来（HEIC 等）时为 0。
	Width  int
	Height int
}

// dkDetectImageInfo 按**字节头**判断真实格式，顺带尽量读出宽高。
//
// 为什么不信客户端的 Content-Type：
//   - Mac 导出的照片常常是 HEIC 却顶着 .jpg 扩展名，浏览器照着扩展名报 image/jpeg；
//   - 一个改了扩展名的 zip 也能声明成 image/png。
//
// 认不出来就返回 ok=false，上层报 DK_UNSUPPORTED_IMAGE_FORMAT。
func dkDetectImageInfo(data []byte) (dkImageInfo, bool) {
	info, ok := dkDetectImageFormat(data)
	if !ok {
		return dkImageInfo{}, false
	}
	// DecodeConfig 只读文件头。HEIC/HEIF/AVIF 没有注册解码器，会直接失败，
	// 宽高留 0 —— 列允许为空，真实尺寸由 Python 处理完之后记在 variant 上。
	if cfg, _, err := image.DecodeConfig(bytes.NewReader(data)); err == nil {
		info.Width = cfg.Width
		info.Height = cfg.Height
	}
	return info, true
}

// dkDetectImageFormat 只看字节头，不解码。
func dkDetectImageFormat(data []byte) (dkImageInfo, bool) {
	switch {
	case len(data) >= 8 && bytes.HasPrefix(data, []byte{0x89, 'P', 'N', 'G', 0x0D, 0x0A, 0x1A, 0x0A}):
		return dkImageInfo{Format: "png", ContentType: "image/png", Ext: ".png"}, true
	case len(data) >= 3 && bytes.HasPrefix(data, []byte{0xFF, 0xD8, 0xFF}):
		return dkImageInfo{Format: "jpeg", ContentType: "image/jpeg", Ext: ".jpg"}, true
	case len(data) >= 6 && (bytes.HasPrefix(data, []byte("GIF87a")) || bytes.HasPrefix(data, []byte("GIF89a"))):
		return dkImageInfo{Format: "gif", ContentType: "image/gif", Ext: ".gif"}, true
	case len(data) >= 2 && bytes.HasPrefix(data, []byte("BM")):
		return dkImageInfo{Format: "bmp", ContentType: "image/bmp", Ext: ".bmp"}, true
	case len(data) >= 12 && bytes.HasPrefix(data, []byte("RIFF")) && bytes.Equal(data[8:12], []byte("WEBP")):
		return dkImageInfo{Format: "webp", ContentType: "image/webp", Ext: ".webp"}, true
	}
	// ISO BMFF：offset 4 是 "ftyp"，offset 8 起是品牌标识。
	// HEIC 是运营最常上传的格式（iPhone 默认就是它），必须认得出来。
	if len(data) >= 12 && bytes.Equal(data[4:8], []byte("ftyp")) {
		switch strings.ToLower(string(data[8:12])) {
		case "heic", "heix", "heim", "heis", "hevc", "hevx", "hevm", "hevs":
			return dkImageInfo{Format: "heic", ContentType: "image/heic", Ext: ".heic"}, true
		case "mif1", "msf1", "miaf":
			return dkImageInfo{Format: "heif", ContentType: "image/heif", Ext: ".heif"}, true
		case "avif", "avis":
			return dkImageInfo{Format: "avif", ContentType: "image/avif", Ext: ".avif"}, true
		}
	}
	return dkImageInfo{}, false
}

// dkExtForContentType 由 MIME 反推扩展名，认不出来给 .bin。
// 只用于拼 object_key —— key 里的扩展名是我们自己读回内容时判断 MIME 的依据。
func dkExtForContentType(contentType string) string {
	switch strings.ToLower(strings.TrimSpace(strings.Split(contentType, ";")[0])) {
	case "image/png":
		return ".png"
	case "image/jpeg", "image/jpg":
		return ".jpg"
	case "image/webp":
		return ".webp"
	case "image/gif":
		return ".gif"
	case "image/bmp", "image/x-ms-bmp":
		return ".bmp"
	case "image/heic":
		return ".heic"
	case "image/heif":
		return ".heif"
	case "image/avif":
		return ".avif"
	default:
		return ".bin"
	}
}

// ----------------------------------------------------------------------------
// ULID
// ----------------------------------------------------------------------------

// dkULIDAlphabet Crockford Base32（去掉了容易看混的 I、L、O、U）。
// 跟 domain.IsValidULID 用的是同一张表。
const dkULIDAlphabet = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

// newAssetULID 生成一个 26 位 ULID：前 48 位是毫秒时间戳，后 80 位是随机数。
//
// 上游 go.mod 里没有 ULID 库，为一个 30 行的函数引新依赖不划算
// （引新依赖每次跟版都要处理 go.mod 冲突）。
// 时间戳在前保证同一天生成的 uid 按时间有序，列表页按 uid 排也是对的。
func newAssetULID() string {
	var raw [16]byte
	ms := uint64(time.Now().UTC().UnixMilli())
	raw[0] = byte(ms >> 40)
	raw[1] = byte(ms >> 32)
	raw[2] = byte(ms >> 24)
	raw[3] = byte(ms >> 16)
	raw[4] = byte(ms >> 8)
	raw[5] = byte(ms)
	if _, err := rand.Read(raw[6:]); err != nil {
		// crypto/rand 读失败在 Linux 上基本等于系统坏了。
		// 这里不能返回一个可能重复的 uid（uid 上有唯一索引，重复会让插入失败），
		// 所以退化成「时间戳 + 纳秒」，重复概率极低且仍然是合法 ULID 字符集。
		nano := uint64(time.Now().UnixNano())
		for i := 6; i < 16; i++ {
			raw[i] = byte(nano >> (uint(i-6) * 6))
		}
	}
	return dkCrockfordEncode128(raw)
}

// dkCrockfordEncode128 把 16 字节编码成 26 个 Crockford Base32 字符。
//
// 26 × 5 = 130 位，比 128 位多 2 位，所以最前面补两个 0 位——
// 这正是 ULID 规范的做法，编码结果跟 oklog/ulid 一致。
func dkCrockfordEncode128(raw [16]byte) string {
	bitAt := func(pos int) int {
		if pos < 2 { // 前两位是补的 0
			return 0
		}
		idx := pos - 2
		return int((raw[idx/8] >> (7 - uint(idx%8))) & 1)
	}
	out := make([]byte, 26)
	for i := 0; i < 26; i++ {
		value := 0
		for k := 0; k < 5; k++ {
			value = value<<1 | bitAt(i*5+k)
		}
		out[i] = dkULIDAlphabet[value]
	}
	return string(out)
}
