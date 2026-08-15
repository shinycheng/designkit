//go:build unit

package service

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"io"
	"io/fs"
	"math/rand"
	"path/filepath"
	"testing"
	"time"

	dkdomain "github.com/Wei-Shaw/sub2api/internal/designkit/domain"
	dkstorage "github.com/Wei-Shaw/sub2api/internal/designkit/storage"

	"github.com/stretchr/testify/require"
)

// ============================================================================
// 假实现
// ============================================================================
//
// 对象存储用**真的**本地磁盘实现（跑在 t.TempDir() 里），
// 这样路径拼接、扩展名、覆盖写这些真问题在这一层就能暴露出来。
// 仓储和 Python 服务用假的：单测不碰数据库、不联网、不花钱。

type assetTestRepo struct {
	nextAssetID   int64
	nextVariantID int64
	assets        map[int64]*dkdomain.Asset
	variants      map[string]*dkdomain.AssetVariant
	createCalls   int
	upsertCalls   int
}

func newAssetTestRepo() *assetTestRepo {
	return &assetTestRepo{
		assets:   map[int64]*dkdomain.Asset{},
		variants: map[string]*dkdomain.AssetVariant{},
	}
}

// assetVariantKey 对应 designkit_asset_variants 的唯一索引
// (asset_id, ratio, keep_transparency, max_dimension)。
func assetVariantKey(assetID int64, ratio dkdomain.Ratio, keep bool, maxDim int) string {
	return fmt.Sprintf("%d|%s|%t|%d", assetID, ratio, keep, maxDim)
}

func (r *assetTestRepo) CreateAsset(_ context.Context, asset *dkdomain.Asset) error {
	r.createCalls++
	r.nextAssetID++
	asset.ID = r.nextAssetID
	clone := *asset
	r.assets[asset.ID] = &clone
	return nil
}

func (r *assetTestRepo) GetAssetByUID(_ context.Context, userID int64, uid string) (*dkdomain.Asset, error) {
	for _, a := range r.assets {
		if a.UID == uid && a.UserID == userID && a.DeletedAt == nil {
			clone := *a
			return &clone, nil
		}
	}
	return nil, dkdomain.ErrNotFound
}

func (r *assetTestRepo) GetAssetsByIDs(_ context.Context, userID int64, ids []int64) ([]*dkdomain.Asset, error) {
	out := make([]*dkdomain.Asset, 0, len(ids))
	for _, id := range ids {
		if a, ok := r.assets[id]; ok && a.UserID == userID {
			clone := *a
			out = append(out, &clone)
		}
	}
	return out, nil
}

func (r *assetTestRepo) FindAssetBySHA256(_ context.Context, userID int64, sum string) (*dkdomain.Asset, error) {
	for _, a := range r.assets {
		if a.UserID == userID && a.SHA256 != nil && *a.SHA256 == sum && a.DeletedAt == nil {
			clone := *a
			return &clone, nil
		}
	}
	return nil, dkdomain.ErrNotFound
}

func (r *assetTestRepo) ListAssets(_ context.Context, query dkdomain.ListAssetsQuery) ([]*dkdomain.Asset, error) {
	out := make([]*dkdomain.Asset, 0, len(r.assets))
	for _, a := range r.assets {
		if a.UserID == query.UserID && a.DeletedAt == nil {
			clone := *a
			out = append(out, &clone)
		}
	}
	return out, nil
}

func (r *assetTestRepo) SoftDeleteAsset(_ context.Context, userID int64, uid string) error {
	for _, a := range r.assets {
		if a.UID == uid && a.UserID == userID {
			now := time.Now()
			a.DeletedAt = &now
			return nil
		}
	}
	return dkdomain.ErrNotFound
}

func (r *assetTestRepo) GetVariant(_ context.Context, assetID int64, ratio dkdomain.Ratio, keep bool, maxDim int) (*dkdomain.AssetVariant, error) {
	if v, ok := r.variants[assetVariantKey(assetID, ratio, keep, maxDim)]; ok {
		clone := *v
		return &clone, nil
	}
	return nil, dkdomain.ErrNotFound
}

func (r *assetTestRepo) UpsertVariant(_ context.Context, params dkdomain.UpsertVariantParams) (*dkdomain.AssetVariant, error) {
	r.upsertCalls++
	r.nextVariantID++
	variant := &dkdomain.AssetVariant{
		ID:               r.nextVariantID,
		AssetID:          params.AssetID,
		Ratio:            params.Ratio,
		KeepTransparency: params.KeepTransparency,
		MaxDimension:     params.MaxDimension,
		ObjectKey:        params.ObjectKey,
		Width:            params.Width,
		Height:           params.Height,
		ContentType:      params.ContentType,
		CreatedAt:        time.Now(),
	}
	r.variants[assetVariantKey(params.AssetID, params.Ratio, params.KeepTransparency, params.MaxDimension)] = variant
	clone := *variant
	return &clone, nil
}

type assetTestSettings struct {
	values map[string]string
}

func newAssetTestSettings() *assetTestSettings {
	return &assetTestSettings{values: map[string]string{}}
}

func (s *assetTestSettings) set(key, rawJSON string) {
	s.values[key] = rawJSON
}

func (s *assetTestSettings) GetSetting(_ context.Context, key string) (*dkdomain.Setting, error) {
	raw, ok := s.values[key]
	if !ok {
		return nil, dkdomain.ErrNotFound
	}
	return &dkdomain.Setting{Key: key, Value: json.RawMessage(raw), UpdatedAt: time.Now()}, nil
}

func (s *assetTestSettings) ListSettings(_ context.Context) ([]*dkdomain.Setting, error) {
	out := make([]*dkdomain.Setting, 0, len(s.values))
	for key, raw := range s.values {
		out = append(out, &dkdomain.Setting{Key: key, Value: json.RawMessage(raw)})
	}
	return out, nil
}

func (s *assetTestSettings) PutSetting(_ context.Context, key string, value []byte) error {
	s.values[key] = string(value)
	return nil
}

type assetTestPreprocessor struct {
	calls       int
	lastRequest dkdomain.PreprocessRequest
	err         error
	out         []byte
	width       int
	height      int
	contentType string
}

func (p *assetTestPreprocessor) Preprocess(_ context.Context, req dkdomain.PreprocessRequest) (*dkdomain.PreprocessResult, error) {
	p.calls++
	p.lastRequest = req
	if p.err != nil {
		return nil, p.err
	}
	contentType := p.contentType
	if contentType == "" {
		contentType = "image/png"
	}
	return &dkdomain.PreprocessResult{
		Data:        p.out,
		ContentType: contentType,
		Width:       p.width,
		Height:      p.height,
		Changed:     true,
		Actions:     []string{"padded"},
	}, nil
}

func (p *assetTestPreprocessor) Health(_ context.Context) error { return nil }

// 编译期断言：假实现必须严格满足接口，签名对不上要在这里就炸。
var (
	_ dkdomain.AssetRepository   = (*assetTestRepo)(nil)
	_ dkdomain.SettingRepository = (*assetTestSettings)(nil)
	_ dkdomain.ImagePreprocessor = (*assetTestPreprocessor)(nil)
)

type assetTestFixture struct {
	svc      *AssetService
	repo     *assetTestRepo
	settings *assetTestSettings
	pre      *assetTestPreprocessor
	store    *dkstorage.LocalStore
	root     string
}

func newAssetTestFixture(t *testing.T) *assetTestFixture {
	t.Helper()
	root := t.TempDir()
	store, err := dkstorage.NewLocalStore(root)
	require.NoError(t, err)

	repo := newAssetTestRepo()
	settings := newAssetTestSettings()
	pre := &assetTestPreprocessor{out: testPNG(t, 12, 16), width: 12, height: 16}

	svc, err := NewAssetService(AssetServiceDeps{
		Assets:       repo,
		Settings:     settings,
		Store:        store,
		Preprocessor: pre,
		Now:          func() time.Time { return time.Date(2026, 8, 13, 10, 0, 0, 0, time.UTC) },
	})
	require.NoError(t, err)

	return &assetTestFixture{svc: svc, repo: repo, settings: settings, pre: pre, store: store, root: store.Root()}
}

func (f *assetTestFixture) countFiles(t *testing.T) int {
	t.Helper()
	count := 0
	require.NoError(t, filepath.WalkDir(f.root, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			count++
		}
		return nil
	}))
	return count
}

// randomPNG 生成一张噪声图。
// 纯色图 PNG 压出来只有几百字节，测不了大小限制，必须用压不掉的随机像素。
func randomPNG(t *testing.T, size int) []byte {
	t.Helper()
	src := rand.New(rand.NewSource(42))
	img := image.NewRGBA(image.Rect(0, 0, size, size))
	for x := 0; x < size; x++ {
		for y := 0; y < size; y++ {
			img.Set(x, y, color.RGBA{
				R: uint8(src.Intn(256)),
				G: uint8(src.Intn(256)),
				B: uint8(src.Intn(256)),
				A: 255,
			})
		}
	}
	var buf bytes.Buffer
	require.NoError(t, png.Encode(&buf, img))
	return buf.Bytes()
}

// bytesReader 把字节切片包成 io.Reader，模拟 multipart 上来的文件流。
func bytesReader(data []byte) io.Reader {
	return bytes.NewReader(data)
}

// ============================================================================
// 上传
// ============================================================================

func TestAssetService_UploadAsset_DetectsRealFormat(t *testing.T) {
	ctx := context.Background()
	f := newAssetTestFixture(t)

	data := testPNG(t, 8, 6)
	result, err := f.svc.UploadAsset(ctx, UploadAssetInput{
		UserID: 7,
		Origin: dkdomain.OriginWeb,
		// 客户端在撒谎：说是 JPEG、文件名也是 .jpg，实际是 PNG。
		// Mac 导出的照片天天这样。
		Filename:          "photo.jpg",
		ClientContentType: "image/jpeg",
		DeclaredSize:      int64(len(data)),
		Data:              bytesReader(data),
	})
	require.NoError(t, err)
	require.False(t, result.Reused)

	asset := result.Asset
	require.Equal(t, "image/png", asset.ContentType, "必须以真实字节为准，不能信客户端")
	require.True(t, filepath.Ext(asset.ObjectKey) == ".png")
	require.Equal(t, "designkit/assets/2026/08/13/"+asset.UID+".png", asset.ObjectKey)
	require.True(t, dkdomain.IsValidULID(asset.UID))
	require.NotNil(t, asset.Width)
	require.Equal(t, 8, *asset.Width)
	require.NotNil(t, asset.Height)
	require.Equal(t, 6, *asset.Height)
	require.NotNil(t, asset.SHA256)
	require.Len(t, *asset.SHA256, 64)
	require.Equal(t, int64(len(data)), asset.ByteSize)

	stored, contentType, err := f.svc.AssetContent(ctx, 7, asset.UID)
	require.NoError(t, err)
	require.Equal(t, data, stored)
	require.Equal(t, "image/png", contentType)
}

func TestAssetService_UploadAsset_DeduplicatesBySHA256(t *testing.T) {
	ctx := context.Background()
	f := newAssetTestFixture(t)
	data := testPNG(t, 8, 6)

	first, err := f.svc.UploadAsset(ctx, UploadAssetInput{UserID: 7, Data: bytesReader(data)})
	require.NoError(t, err)
	require.False(t, first.Reused)

	second, err := f.svc.UploadAsset(ctx, UploadAssetInput{UserID: 7, Data: bytesReader(data)})
	require.NoError(t, err)
	require.True(t, second.Reused, "同一个人传同一张图必须复用，不重复占盘也不重复入库")
	require.Equal(t, first.Asset.UID, second.Asset.UID)
	require.Equal(t, 1, f.repo.createCalls)
	require.Equal(t, 1, f.countFiles(t))

	// 换一个人传同一张图：互不影响，各存各的。
	other, err := f.svc.UploadAsset(ctx, UploadAssetInput{UserID: 8, Data: bytesReader(data)})
	require.NoError(t, err)
	require.False(t, other.Reused)
	require.NotEqual(t, first.Asset.UID, other.Asset.UID)
	require.Equal(t, 2, f.repo.createCalls)
}

func TestAssetService_UploadAsset_RejectsOversize(t *testing.T) {
	ctx := context.Background()
	f := newAssetTestFixture(t)
	f.settings.set("max_upload_bytes", "65536") // 64KB

	big := randomPNG(t, 300) // 噪声图，压不下去，肯定超过 64KB
	require.Greater(t, len(big), 65536)

	// ① 客户端声明的大小就超了：一个字节都不用读。
	_, err := f.svc.UploadAsset(ctx, UploadAssetInput{UserID: 7, DeclaredSize: int64(len(big)), Data: bytesReader(big)})
	requireDKCode(t, err, dkdomain.ErrCodeImageTooLarge)

	// ② 客户端撒谎说自己很小：读的时候仍然必须被拦下。
	_, err = f.svc.UploadAsset(ctx, UploadAssetInput{UserID: 7, DeclaredSize: 10, Data: bytesReader(big)})
	requireDKCode(t, err, dkdomain.ErrCodeImageTooLarge)

	require.Equal(t, 0, f.repo.createCalls)
	require.Equal(t, 0, f.countFiles(t))
}

func TestAssetService_UploadAsset_RejectsUnsupportedFormat(t *testing.T) {
	ctx := context.Background()
	f := newAssetTestFixture(t)

	cases := []struct {
		name string
		data []byte
	}{
		{name: "纯文本", data: []byte("这不是图片")},
		{name: "改了扩展名的压缩包", data: []byte{0x50, 0x4B, 0x03, 0x04, 0x00, 0x00}},
		{name: "空内容", data: []byte{}},
	}
	for _, tc := range cases {
		name, data := tc.name, tc.data
		t.Run(name, func(t *testing.T) {
			_, err := f.svc.UploadAsset(ctx, UploadAssetInput{
				UserID:            7,
				ClientContentType: "image/png",
				Filename:          "x.png",
				Data:              bytesReader(data),
			})
			require.Error(t, err)
			dkErr, ok := dkdomain.AsDesignkitError(err)
			require.True(t, ok)
			require.Contains(t,
				[]string{dkdomain.ErrCodeUnsupportedImageFormat, dkdomain.ErrCodeInvalidRequest},
				dkErr.Code)
		})
	}
	require.Equal(t, 0, f.countFiles(t))
}

// TestAssetService_UploadAsset_HEIC HEIC 是 iPhone 默认格式，必须收得下，
// 但 Go 读不出宽高——留空，交给 Python 处理完之后由 variant 记着。
func TestAssetService_UploadAsset_HEIC(t *testing.T) {
	ctx := context.Background()
	f := newAssetTestFixture(t)

	heic := make([]byte, 64)
	copy(heic[4:], []byte("ftypheic"))
	result, err := f.svc.UploadAsset(ctx, UploadAssetInput{
		UserID:            7,
		Filename:          "IMG_0001.JPG", // Mac 导出的 HEIC 常常顶着 .jpg 的名字
		ClientContentType: "image/jpeg",
		Data:              bytesReader(heic),
	})
	require.NoError(t, err)
	require.Equal(t, "image/heic", result.Asset.ContentType)
	require.Equal(t, ".heic", filepath.Ext(result.Asset.ObjectKey))
	require.Nil(t, result.Asset.Width)
	require.Nil(t, result.Asset.Height)
}

// ============================================================================
// 预处理 / 比例护栏
// ============================================================================

func TestAssetService_EnsureVariant_RejectsRatioNotInSettings(t *testing.T) {
	ctx := context.Background()
	f := newAssetTestFixture(t)
	f.settings.set(dkdomain.SettingKeyRatios, `["1:1","3:4"]`)
	asset := uploadTestAsset(t, f)

	// 7:13 格式合法但不在白名单里。Python 的 parse_aspect 会照收，
	// 所以这一关必须在 Go 侧拦住。
	for _, ratio := range []dkdomain.Ratio{"7:13", "16:9", "0:1", "abc", ""} {
		_, err := f.svc.EnsureVariant(ctx, EnsureVariantInput{
			UserID:   7,
			AssetUID: asset.UID,
			Ratio:    ratio,
		})
		requireDKCode(t, err, dkdomain.ErrCodeRatioNotAllowed)
	}
	require.Zero(t, f.pre.calls, "比例不合法时一次预处理都不该发出去")
	require.Zero(t, f.repo.upsertCalls)
}

func TestAssetService_EnsureVariant_UsesSettingsMaxDimension(t *testing.T) {
	ctx := context.Background()
	f := newAssetTestFixture(t)
	f.settings.set(dkdomain.SettingKeyMaxDimension, "4096")
	asset := uploadTestAsset(t, f)

	variant, err := f.svc.EnsureVariant(ctx, EnsureVariantInput{
		UserID:   7,
		AssetUID: asset.UID,
		Ratio:    dkdomain.Ratio3x4,
	})
	require.NoError(t, err)

	// max_dimension 必须来自 designkit_settings 并显式传给 Python，
	// 否则「界面说 4K、实际按 2K 出图」且不报错。
	require.Equal(t, 4096, f.pre.lastRequest.MaxDimension)
	require.Equal(t, dkdomain.Ratio3x4, f.pre.lastRequest.Ratio)
	require.Equal(t, 4096, variant.MaxDimension)
	require.Equal(t, "designkit/variants/2026/08/13/"+asset.UID+"-3x4-o-4096.png", variant.ObjectKey)
	require.NotNil(t, variant.Width)
	require.Equal(t, 12, *variant.Width)
	require.NotNil(t, variant.Height)
	require.Equal(t, 16, *variant.Height)

	data, contentType, err := f.svc.VariantContent(ctx, variant)
	require.NoError(t, err)
	require.Equal(t, f.pre.out, data)
	require.Equal(t, "image/png", contentType)
}

func TestAssetService_EnsureVariant_DefaultsToSettingFallback(t *testing.T) {
	ctx := context.Background()
	f := newAssetTestFixture(t)
	asset := uploadTestAsset(t, f)

	_, err := f.svc.EnsureVariant(ctx, EnsureVariantInput{UserID: 7, AssetUID: asset.UID, Ratio: dkdomain.Ratio1x1})
	require.NoError(t, err)
	require.Equal(t, dkdomain.DefaultMaxDimension, f.pre.lastRequest.MaxDimension)
}

func TestAssetService_EnsureVariant_ReusesExisting(t *testing.T) {
	ctx := context.Background()
	f := newAssetTestFixture(t)
	asset := uploadTestAsset(t, f)

	first, err := f.svc.EnsureVariant(ctx, EnsureVariantInput{UserID: 7, AssetUID: asset.UID, Ratio: dkdomain.Ratio3x4})
	require.NoError(t, err)
	second, err := f.svc.EnsureVariant(ctx, EnsureVariantInput{UserID: 7, AssetUID: asset.UID, Ratio: dkdomain.Ratio3x4})
	require.NoError(t, err)

	require.Equal(t, first.ID, second.ID)
	require.Equal(t, 1, f.pre.calls, "同样的四个参数命中已有产物，不该再调 Python")
	require.Equal(t, 1, f.repo.upsertCalls)

	// 换一个比例就是另一个产物（这正是 variants 单独一张表的理由）。
	_, err = f.svc.EnsureVariant(ctx, EnsureVariantInput{UserID: 7, AssetUID: asset.UID, Ratio: dkdomain.Ratio1x1})
	require.NoError(t, err)
	require.Equal(t, 2, f.pre.calls)
	require.Equal(t, 2, f.repo.upsertCalls)
}

// TestAssetService_EnsureVariant_FailClosed 这一条守的是最贵的坑：
// 预处理失败时**绝不能**把原图当成产物用下去。
// 回吐原图 = 运营选 3:4 拿回 4:3、钱照扣，而且只有一行 warning。
func TestAssetService_EnsureVariant_FailClosed(t *testing.T) {
	ctx := context.Background()
	f := newAssetTestFixture(t)
	asset := uploadTestAsset(t, f)
	filesBefore := f.countFiles(t)

	f.pre.err = dkdomain.NewError(dkdomain.ErrCodePreprocessFailed).
		WithUpstream("imgsvc 500 internal_error: 服务端处理图片时出错")

	variant, err := f.svc.EnsureVariant(ctx, EnsureVariantInput{UserID: 7, AssetUID: asset.UID, Ratio: dkdomain.Ratio3x4})
	require.Nil(t, variant)
	requireDKCode(t, err, dkdomain.ErrCodePreprocessFailed)

	require.Zero(t, f.repo.upsertCalls, "失败时不能落任何 variant 行")
	require.Equal(t, filesBefore, f.countFiles(t), "失败时不能往存储里写任何东西")

	dkErr, ok := dkdomain.AsDesignkitError(err)
	require.True(t, ok)
	require.NotEmpty(t, dkErr.Upstream, "上游原文只落 last_error_message 给管理员看")
	require.NotContains(t, dkErr.Message, "imgsvc", "界面文案里不该出现英文原文")
}

func TestAssetService_EnsureVariant_NoPreprocessorIsFailClosed(t *testing.T) {
	ctx := context.Background()
	f := newAssetTestFixture(t)
	asset := uploadTestAsset(t, f)

	svc, err := NewAssetService(AssetServiceDeps{
		Assets:   f.repo,
		Settings: f.settings,
		Store:    f.store,
		// Preprocessor 故意留空
	})
	require.NoError(t, err)

	_, err = svc.EnsureVariant(ctx, EnsureVariantInput{UserID: 7, AssetUID: asset.UID, Ratio: dkdomain.Ratio3x4})
	requireDKCode(t, err, dkdomain.ErrCodePreprocessFailed)
}

// ============================================================================
// 配置读取
// ============================================================================

func TestAssetService_SettingsFallbacks(t *testing.T) {
	ctx := context.Background()
	f := newAssetTestFixture(t)

	// 一条都没配：全用兜底值。
	require.Equal(t, dkdomain.DefaultRatios(), f.svc.AllowedRatios(ctx))
	require.Equal(t, dkdomain.DefaultMaxDimension, f.svc.MaxDimension(ctx))
	require.Equal(t, defaultAssetMaxUploadBytes, f.svc.MaxUploadBytes(ctx))

	// 配了垃圾值：也用兜底值，不能让上传和出图整个挂掉。
	f.settings.set(dkdomain.SettingKeyRatios, `"不是数组"`)
	f.settings.set(dkdomain.SettingKeyMaxDimension, `999999`)
	f.settings.set("max_upload_bytes", `-1`)
	require.Equal(t, dkdomain.DefaultRatios(), f.svc.AllowedRatios(ctx))
	require.Equal(t, dkdomain.DefaultMaxDimension, f.svc.MaxDimension(ctx))
	require.Equal(t, defaultAssetMaxUploadBytes, f.svc.MaxUploadBytes(ctx))

	// 数组里混了不合法的比例：跳过坏的，留下好的。
	f.settings.set(dkdomain.SettingKeyRatios, `["1:1","7/13","9:16"]`)
	require.Equal(t, []dkdomain.Ratio{dkdomain.Ratio1x1, dkdomain.Ratio9x16}, f.svc.AllowedRatios(ctx))
}

func TestAssetService_DeleteAssetKeepsFile(t *testing.T) {
	ctx := context.Background()
	f := newAssetTestFixture(t)
	asset := uploadTestAsset(t, f)

	require.NoError(t, f.svc.DeleteAsset(ctx, 7, asset.UID))
	// 软删：历史任务还要能看到当时用的是哪张图，文件不能删。
	require.Equal(t, 1, f.countFiles(t))

	_, err := f.svc.GetAsset(ctx, 7, asset.UID)
	requireDKCode(t, err, dkdomain.ErrCodeAssetNotFound)
}

func TestAssetService_GetAssetChecksOwnership(t *testing.T) {
	ctx := context.Background()
	f := newAssetTestFixture(t)
	asset := uploadTestAsset(t, f)

	// 别人的图一律报「找不到」，不报 403——403 会泄露「这个编号存在」。
	_, err := f.svc.GetAsset(ctx, 999, asset.UID)
	requireDKCode(t, err, dkdomain.ErrCodeAssetNotFound)
}

func TestNewAssetService_RequiresDeps(t *testing.T) {
	_, err := NewAssetService(AssetServiceDeps{})
	require.Error(t, err)
	_, err = NewAssetService(AssetServiceDeps{Assets: newAssetTestRepo()})
	require.Error(t, err)
	_, err = NewAssetService(AssetServiceDeps{Assets: newAssetTestRepo(), Settings: newAssetTestSettings()})
	require.Error(t, err)
}

// ============================================================================
// ULID
// ============================================================================

func TestNewAssetULID(t *testing.T) {
	seen := map[string]struct{}{}
	for i := 0; i < 2000; i++ {
		uid := newAssetULID()
		require.Len(t, uid, 26)
		require.True(t, dkdomain.IsValidULID(uid), "%q 必须是合法 ULID", uid)
		_, dup := seen[uid]
		require.False(t, dup, "uid 重复了：%s（uid 上有唯一索引，重复会让插入直接失败）", uid)
		seen[uid] = struct{}{}
	}
}

func TestDkDetectImageFormat(t *testing.T) {
	cases := []struct {
		name        string
		data        []byte
		wantType    string
		wantExt     string
		wantDecoded bool
	}{
		{name: "png", data: testPNG(t, 3, 5), wantType: "image/png", wantExt: ".png", wantDecoded: true},
		{name: "jpeg", data: append([]byte{0xFF, 0xD8, 0xFF}, make([]byte, 32)...), wantType: "image/jpeg", wantExt: ".jpg"},
		{name: "gif", data: append([]byte("GIF89a"), make([]byte, 32)...), wantType: "image/gif", wantExt: ".gif"},
		{name: "bmp", data: append([]byte("BM"), make([]byte, 32)...), wantType: "image/bmp", wantExt: ".bmp"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			info, ok := dkDetectImageInfo(tc.data)
			require.True(t, ok)
			require.Equal(t, tc.wantType, info.ContentType)
			require.Equal(t, tc.wantExt, info.Ext)
			if tc.wantDecoded {
				require.Equal(t, 3, info.Width)
				require.Equal(t, 5, info.Height)
			}
		})
	}

	_, ok := dkDetectImageInfo([]byte("hello world"))
	require.False(t, ok)
}

func TestDkExtForContentType(t *testing.T) {
	require.Equal(t, ".png", dkExtForContentType("image/png"))
	require.Equal(t, ".jpg", dkExtForContentType("image/jpeg; charset=binary"))
	require.Equal(t, ".webp", dkExtForContentType("IMAGE/WEBP"))
	require.Equal(t, ".bin", dkExtForContentType("application/json"))
}

// ============================================================================
// 小工具
// ============================================================================

func uploadTestAsset(t *testing.T, f *assetTestFixture) *dkdomain.Asset {
	t.Helper()
	result, err := f.svc.UploadAsset(context.Background(), UploadAssetInput{
		UserID: 7,
		Data:   bytesReader(testPNG(t, 8, 6)),
	})
	require.NoError(t, err)
	return result.Asset
}

func requireDKCode(t *testing.T, err error, wantCode string) {
	t.Helper()
	require.Error(t, err)
	dkErr, ok := dkdomain.AsDesignkitError(err)
	require.True(t, ok, "错误必须是 *DesignkitError：%v", err)
	require.Equal(t, wantCode, dkErr.Code)
	require.NotEmpty(t, dkErr.Message, "面向运营的中文文案不能为空")
}
