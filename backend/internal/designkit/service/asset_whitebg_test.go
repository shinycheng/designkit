//go:build unit

package service

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"image/png"
	"testing"

	dkdomain "github.com/Wei-Shaw/sub2api/internal/designkit/domain"

	"github.com/stretchr/testify/require"
)

// ============================================================================
// 一键白底图（asset_whitebg.go）
// ============================================================================
//
// 复用 asset_test.go 的 fixture（真本地磁盘 + 假仓储），rembg 用假实现 ——
// 单测不起容器、不联网。

// fakeRembg 是 BackgroundRemover 的假实现：原样返回预置的透明 PNG。
type fakeRembg struct {
	out   []byte
	err   error
	calls int
}

func (f *fakeRembg) RemoveBackground(_ context.Context, _ []byte, _ string) ([]byte, error) {
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	return f.out, nil
}

// transparentTestPNG 2×2：三个像素完全透明，(1,1) 是不透明的红色。
// 透明的三个角必须被合成成白色，红色那个必须保持原色 ——
// 两头都断言，才能证明是「合白底」而不是「整张刷白」。
func transparentTestPNG(t *testing.T) []byte {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, 2, 2))
	img.SetNRGBA(1, 1, color.NRGBA{R: 200, G: 10, B: 10, A: 255})
	var buf bytes.Buffer
	require.NoError(t, png.Encode(&buf, img))
	return buf.Bytes()
}

// requirePixel 断言某个像素的不透明 RGB 值。
func requirePixel(t *testing.T, img image.Image, x, y int, r, g, b uint8) {
	t.Helper()
	got := color.RGBAModel.Convert(img.At(x, y)).(color.RGBA)
	require.Equal(t, color.RGBA{R: r, G: g, B: b, A: 255}, got, "像素 (%d,%d) 颜色不对", x, y)
}

func TestRemoveBackground_ComposesWhite(t *testing.T) {
	fx := newAssetTestFixture(t)
	rembg := &fakeRembg{out: transparentTestPNG(t)}
	fx.svc.rembg = rembg
	ctx := context.Background()

	uploaded, err := fx.svc.UploadAsset(ctx, UploadAssetInput{
		UserID:   7,
		Filename: "source.png",
		Data:     bytes.NewReader(testPNG(t, 2, 2)),
	})
	require.NoError(t, err)

	asset, err := fx.svc.RemoveBackground(ctx, 7, uploaded.Asset.UID, dkdomain.OriginWeb)
	require.NoError(t, err)
	require.NotNil(t, asset)
	require.NotEqual(t, uploaded.Asset.UID, asset.UID, "白底图必须是一条新的商品图")
	require.Equal(t, "image/png", asset.ContentType)
	require.Equal(t, 1, rembg.calls)

	data, _, err := fx.store.Get(ctx, asset.ObjectKey)
	require.NoError(t, err)
	img, err := png.Decode(bytes.NewReader(data))
	require.NoError(t, err)
	require.Equal(t, 2, img.Bounds().Dx())
	require.Equal(t, 2, img.Bounds().Dy())

	// 透明的三个角合成后必须是纯白。
	requirePixel(t, img, 0, 0, 255, 255, 255)
	requirePixel(t, img, 1, 0, 255, 255, 255)
	requirePixel(t, img, 0, 1, 255, 255, 255)
	// 不透明的商品像素保持原色：合白底不能把商品也刷白。
	requirePixel(t, img, 1, 1, 200, 10, 10)
}

// TestRemoveBackground_DedupesBySHA256 重复点「生成白底图」：
// 产物字节一样 → sha256 命中 → 同一条 asset，不新插行、不新写文件。
func TestRemoveBackground_DedupesBySHA256(t *testing.T) {
	fx := newAssetTestFixture(t)
	fx.svc.rembg = &fakeRembg{out: transparentTestPNG(t)}
	ctx := context.Background()

	uploaded, err := fx.svc.UploadAsset(ctx, UploadAssetInput{
		UserID:   7,
		Filename: "source.png",
		Data:     bytes.NewReader(testPNG(t, 2, 2)),
	})
	require.NoError(t, err)

	first, err := fx.svc.RemoveBackground(ctx, 7, uploaded.Asset.UID, dkdomain.OriginWeb)
	require.NoError(t, err)
	createsAfterFirst := fx.repo.createCalls

	second, err := fx.svc.RemoveBackground(ctx, 7, uploaded.Asset.UID, dkdomain.OriginWeb)
	require.NoError(t, err)
	require.Equal(t, first.UID, second.UID, "同一张原图的白底图必须复用同一条记录")
	require.Equal(t, createsAfterFirst, fx.repo.createCalls, "第二次不许再插一行")
}

// TestRemoveBackground_NotConfigured rembg 没配置：中文「还没准备好」，
// 不 panic、不去读图。
func TestRemoveBackground_NotConfigured(t *testing.T) {
	fx := newAssetTestFixture(t)
	ctx := context.Background()

	uploaded, err := fx.svc.UploadAsset(ctx, UploadAssetInput{
		UserID:   7,
		Filename: "source.png",
		Data:     bytes.NewReader(testPNG(t, 2, 2)),
	})
	require.NoError(t, err)

	_, err = fx.svc.RemoveBackground(ctx, 7, uploaded.Asset.UID, dkdomain.OriginWeb)
	require.Error(t, err)
	require.Contains(t, err.Error(), "还没准备好")
}

// TestRemoveBackground_ChecksOwnership 别人的图抠不了：报「找不到」而不是 403，
// 不泄露「这个编号存在」。
func TestRemoveBackground_ChecksOwnership(t *testing.T) {
	fx := newAssetTestFixture(t)
	fx.svc.rembg = &fakeRembg{out: transparentTestPNG(t)}
	ctx := context.Background()

	uploaded, err := fx.svc.UploadAsset(ctx, UploadAssetInput{
		UserID:   7,
		Filename: "source.png",
		Data:     bytes.NewReader(testPNG(t, 2, 2)),
	})
	require.NoError(t, err)

	_, err = fx.svc.RemoveBackground(ctx, 8, uploaded.Asset.UID, dkdomain.OriginWeb)
	require.Error(t, err)
	var dkErr *dkdomain.DesignkitError
	require.ErrorAs(t, err, &dkErr)
	require.Equal(t, dkdomain.ErrCodeAssetNotFound, dkErr.Code)
}

// TestComposeOnWhite_RejectsGarbage 抠图服务返回的不是图片时报中文错误，不 panic。
func TestComposeOnWhite_RejectsGarbage(t *testing.T) {
	_, err := composeOnWhite([]byte("not a png"))
	require.Error(t, err)
	var dkErr *dkdomain.DesignkitError
	require.ErrorAs(t, err, &dkErr)
	require.Equal(t, dkdomain.ErrCodePreprocessFailed, dkErr.Code)
}
