package handler

import (
	"archive/zip"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"strings"

	dkdomain "github.com/Wei-Shaw/sub2api/internal/designkit/domain"

	"github.com/gin-gonic/gin"
)

// ============================================================================
// 打包下载：GET /jobs/:uid/images.zip
// ============================================================================
//
// 把一个批次里**出成功的每一张**打进一个 zip：archive/zip 直接往响应体里写，
// 边读边写，不落盘、不在内存里攒整包。包内文件名「第{seq}张.png」——
// 「第几张」必须跟接口里的 seq 严格对应（发给 ERP 的对外契约，别改）。
//
// 挂浏览器组 + ERP 读组（**不花钱**）：跟单张取图同一个道理，
// 已经付过钱的图在额度耗尽时照样要取得回来。
//
// ⚠ 一旦开始往外写 zip，HTTP 状态码就改不了了。所以顺序是刻意的：
// 「有没有图可打」和「第一张能不能取到」都在写响应头**之前**判完 ——
// 空批次和存储故障都还能返回正常的 JSON 错误；只有中途才失败的那几张
// 走「跳过 + 日志」，比掐断连接留半截打不开的包强。

// ImagesZip 处理 GET /jobs/:uid/images.zip。
func (h *JobHandler) ImagesZip(c *gin.Context) {
	userID, ok := userIDOf(c)
	if !ok {
		failCode(c, dkdomain.ErrCodeUnauthorized)
		return
	}
	uid, ok := requireUID(c, "uid", dkdomain.ErrCodeJobNotFound)
	if !ok {
		return
	}

	// 先取批次本体：归属校验发生在这里（不是他的批次 = 「找不到」，不泄露存在性），
	// 批次名用来拼下载的文件名。
	job, err := h.jobs.GetJob(c.Request.Context(), userID, uid)
	if err != nil {
		failService(c, err, dkdomain.ErrCodeJobNotFound)
		return
	}
	if job == nil {
		failCode(c, dkdomain.ErrCodeJobNotFound)
		return
	}

	views, err := h.jobs.ListJobItems(c.Request.Context(), userID, uid)
	if err != nil {
		failService(c, err, dkdomain.ErrCodeJobNotFound)
		return
	}

	// 只挑「出成功且有图」的，并强制按 seq 升序（跟 Items 同一道保险）。
	entries := make([]*JobItemView, 0, len(views))
	for _, v := range views {
		if v == nil || v.Item == nil || v.Item.Status != dkdomain.ItemStatusSucceeded || len(v.Images) == 0 {
			continue
		}
		entries = append(entries, v)
	}
	sort.SliceStable(entries, func(i, j int) bool {
		return entries[i].Item.Seq < entries[j].Item.Seq
	})

	// 一张成功的图都没有：404，绝不回一个空 zip ——
	// 空包打开是「里面什么都没有」，运营分不清是系统坏了还是真没图。
	if len(entries) == 0 {
		failCodef(c, dkdomain.ErrCodeImageNotFound, "这一批还没有出好的图，等出好之后再打包下载。")
		return
	}

	// 第一张先取到手再写响应头：第一张就取不到（多半是存储坏了）时，
	// 还能返回带中文的 JSON 错误，而不是一个损坏的 zip。
	firstBlob, err := h.jobs.OpenJobItemContent(c.Request.Context(), userID, uid, entries[0].Item.Seq, 1)
	if err != nil {
		failService(c, err, dkdomain.ErrCodeImageNotFound)
		return
	}
	if firstBlob == nil || len(firstBlob.Data) == 0 {
		failCode(c, dkdomain.ErrCodeImageNotFound)
		return
	}

	// 下载文件名 = 批次名.zip（没起名用任务号）。中文走 RFC 5987 的 filename*，
	// 再给一个纯 ASCII 的 filename 兜底。
	zipName := strings.TrimSpace(job.Name)
	if zipName == "" {
		zipName = job.UID
	}
	c.Header("Content-Type", "application/zip")
	c.Header("Content-Disposition", attachmentDisposition(zipName+".zip"))
	// 整包是动态拼的，别让任何一层缓存半截结果。
	c.Header("Cache-Control", "private, no-store")
	c.Header("X-Content-Type-Options", "nosniff")
	c.Status(http.StatusOK)

	zw := zip.NewWriter(c.Writer)
	if err := writeZipImage(zw, entries[0].Item.Seq, firstBlob.Data); err != nil {
		// 写不动基本 = 客户端断开。停止即可，响应已经开头了，不能再回 JSON。
		slog.Debug("designkit 打包下载中断",
			slog.String("job_uid", uid), slog.Any("error", err))
		return
	}
	for _, v := range entries[1:] {
		blob, err := h.jobs.OpenJobItemContent(c.Request.Context(), userID, uid, v.Item.Seq, 1)
		if err != nil || blob == nil || len(blob.Data) == 0 {
			// zip 已经开始往外流：跳过这一张（包里少一张），日志记清楚是哪张。
			slog.Warn("designkit 打包下载跳过了一张取不到的图",
				slog.String("job_uid", uid),
				slog.Int("seq", v.Item.Seq),
				slog.Any("error", err))
			continue
		}
		if err := writeZipImage(zw, v.Item.Seq, blob.Data); err != nil {
			slog.Debug("designkit 打包下载中断",
				slog.String("job_uid", uid), slog.Any("error", err))
			return
		}
	}
	if err := zw.Close(); err != nil {
		slog.Debug("designkit 打包下载收尾失败",
			slog.String("job_uid", uid), slog.Any("error", err))
	}
}

// writeZipImage 往 zip 里写一张图，文件名「第{seq}张.png」。
//
// Method 用 Store（只存不压）：PNG 本身已经压缩过，再过一遍 Deflate
// 体积几乎不变、CPU 白烧。archive/zip 对非 ASCII 文件名会自动打 UTF-8 标记。
func writeZipImage(zw *zip.Writer, seq int, data []byte) error {
	w, err := zw.CreateHeader(&zip.FileHeader{
		Name:   fmt.Sprintf("第%d张.png", seq),
		Method: zip.Store,
	})
	if err != nil {
		return err
	}
	_, err = w.Write(data)
	return err
}

// attachmentDisposition 拼下载用的 Content-Disposition。
//
// 中文文件名走 RFC 5987 的 filename*（UTF-8 + 百分号编码）；
// 老客户端不认 filename* 时退回纯 ASCII 的 filename ——
// 名字是 ASCII 时两边一致，否则兜底叫 images.zip（名字难看但下载不坏）。
func attachmentDisposition(filename string) string {
	fallback := safeAttachmentName(filename)
	if fallback == "" {
		fallback = "images.zip"
	}
	return `attachment; filename="` + fallback + `"; filename*=UTF-8''` + rfc5987Encode(filename)
}

// rfc5987Encode 按 RFC 5987 的 value-chars 做百分号编码：
// attr-char（字母、数字和少数几个符号）原样保留，其余字节一律 %XX。
// 引号、分号、换行都会被编掉，所以编码结果放进响应头是安全的。
func rfc5987Encode(value string) string {
	const hexDigits = "0123456789ABCDEF"
	out := make([]byte, 0, len(value)*3)
	for i := 0; i < len(value); i++ {
		ch := value[i]
		if isRFC5987AttrChar(ch) {
			out = append(out, ch)
			continue
		}
		out = append(out, '%', hexDigits[ch>>4], hexDigits[ch&0x0F])
	}
	return string(out)
}

// isRFC5987AttrChar RFC 5987 定义的 attr-char。
func isRFC5987AttrChar(ch byte) bool {
	switch {
	case ch >= 'a' && ch <= 'z', ch >= 'A' && ch <= 'Z', ch >= '0' && ch <= '9':
		return true
	}
	switch ch {
	case '!', '#', '$', '&', '+', '-', '.', '^', '_', '`', '|', '~':
		return true
	}
	return false
}
