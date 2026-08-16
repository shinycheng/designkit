package service

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"image/draw"
	"image/png"

	dkdomain "github.com/Wei-Shaw/sub2api/internal/designkit/domain"
)

// ============================================================================
// 一键白底图：抠掉背景 + 合成白底 → 一条新的商品图
// ============================================================================
//
// 链路：读原图字节 → rembg 抠背景（返回透明 PNG）→ Go 标准库把透明 PNG
// 合成到白底上 → 走 UploadAsset 的 sha256 去重路径入库。
//
// 为什么产出是**一条新的 asset** 而不是覆盖原图：运营经常两个都要
// （原图出场景图、白底图出主图），而且历史任务的 item 还引用着原图。
// 这跟「用这张继续生成」是同一个形状：产出物是商品图编号，
// 拿到就能塞进下一批的 asset_uids。
//
// 为什么合白底在 Go 侧而不是让 Python imgsvc 做：rembg 返回的就是标准
// 透明 PNG，image/draw 一次 Over 就够，为它跨一次容器不值得。
// （imgsvc 的「透明合白」是出图预处理链路的事，跟这里互不相干。）

// RemoveBackground 给一张已有的商品图抠背景、合白底，存成一条新的商品图。
//
// origin 跟上传一样由挂载前缀决定（web / erp），记在新 asset 上。
// 抠图服务没配置（rembg 为 nil）时返回中文的「还没准备好」。
//
// # 会不会存重复的图
//
// 不会。走 UploadAsset 的 sha256 去重：同一张原图重复点「生成白底图」，
// rembg 模型是确定性的，产物字节一样，拿到的是同一条 asset 记录。
func (s *AssetService) RemoveBackground(ctx context.Context, userID int64, assetUID string, origin dkdomain.Origin) (*dkdomain.Asset, error) {
	if userID <= 0 {
		return nil, dkdomain.NewError(dkdomain.ErrCodeUnauthorized)
	}
	if s.rembg == nil {
		return nil, dkdomain.NewError(dkdomain.ErrCodePreprocessFailed).
			WithMessage("白底图功能还没准备好，请联系管理员。")
	}

	// AssetContent 已校验归属；记录在、文件不在时报 DK_STORAGE_ERROR。
	data, contentType, err := s.AssetContent(ctx, userID, assetUID)
	if err != nil {
		return nil, err
	}

	cut, err := s.rembg.RemoveBackground(ctx, data, assetUID+dkExtForContentType(contentType))
	if err != nil {
		return nil, err
	}

	white, err := composeOnWhite(cut)
	if err != nil {
		return nil, err
	}

	// 后缀 -whitebg 只是让文件名认得出来；真实格式仍由 UploadAsset 按字节头判。
	result, err := s.UploadAsset(ctx, UploadAssetInput{
		UserID:            userID,
		Origin:            origin,
		Filename:          assetUID + "-whitebg.png",
		ClientContentType: "image/png",
		DeclaredSize:      int64(len(white)),
		Data:              bytes.NewReader(white),
	})
	if err != nil {
		return nil, err
	}
	if result == nil || result.Asset == nil {
		return nil, dkdomain.NewError(dkdomain.ErrCodeInternal)
	}
	return result.Asset, nil
}

// composeOnWhite 把一张（可能带透明的）图合成到纯白底上，输出 PNG。
//
// 用 draw.Over 而不是逐像素判 alpha：半透明边缘（抠图产物的常态）
// 要按 alpha 混进白色，硬切会留一圈黑边。
func composeOnWhite(transparentPNG []byte) ([]byte, error) {
	img, _, err := image.Decode(bytes.NewReader(transparentPNG))
	if err != nil {
		return nil, dkdomain.NewError(dkdomain.ErrCodePreprocessFailed).
			WithMessage("抠图服务返回的图片读不出来，请再试一次。").
			WithCause(err)
	}
	bounds := img.Bounds()
	if bounds.Dx() <= 0 || bounds.Dy() <= 0 {
		return nil, dkdomain.NewError(dkdomain.ErrCodePreprocessFailed).
			WithMessage("抠图服务返回了一张空图片，请再试一次。")
	}

	out := image.NewRGBA(image.Rect(0, 0, bounds.Dx(), bounds.Dy()))
	draw.Draw(out, out.Bounds(), image.NewUniform(color.White), image.Point{}, draw.Src)
	draw.Draw(out, out.Bounds(), img, bounds.Min, draw.Over)

	var buf bytes.Buffer
	if err := png.Encode(&buf, out); err != nil {
		return nil, dkdomain.NewError(dkdomain.ErrCodeInternal).WithCause(err)
	}
	return buf.Bytes(), nil
}
