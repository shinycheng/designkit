package handler

import (
	"net/http"
	"strings"

	dkdomain "github.com/Wei-Shaw/sub2api/internal/designkit/domain"

	"github.com/gin-gonic/gin"
)

// contentCacheControl 图片字节的缓存策略。
//
// private：这是经我方鉴权才能取的图，绝不能被共享缓存（反代/CDN）存下来发给别人。
// max-age 给 5 分钟：够挡住列表页反复渲染的重复请求，又不会让「重试出了新图」
// 之后运营刷半天还看到旧图。
const contentCacheControl = "private, max-age=300"

// writeContent 把一段二进制内容写进响应。
//
// 走我们自己的下载端点而不是给对象存储直链，有两个原因：
//   - 上游对象存储默认签发 24 小时失效的链接，存进库过期后拼不回来；
//   - 「必须经我方鉴权才能看图」只有自己写下载端点才做得到。
func writeContent(c *gin.Context, blob *ContentBlob, notFoundCode string) {
	if blob == nil || len(blob.Data) == 0 {
		failCode(c, notFoundCode)
		return
	}

	contentType := strings.TrimSpace(blob.ContentType)
	if contentType == "" {
		contentType = "application/octet-stream"
	}

	if name := safeAttachmentName(blob.Filename); name != "" {
		c.Header("Content-Disposition", `inline; filename="`+name+`"`)
	}
	c.Header("Cache-Control", contentCacheControl)
	c.Header("X-Content-Type-Options", "nosniff")
	c.Data(http.StatusOK, contentType, blob.Data)
}

// safeAttachmentName 只放行「肯定不会把响应头拆坏」的文件名。
//
// 文件名里带引号、换行、非 ASCII 都可能让某些客户端把 Content-Disposition 解析歪，
// 严重的会变成响应头注入。拿不准就直接不下发这个头 —— 少一个建议文件名不影响使用。
func safeAttachmentName(name string) string {
	name = strings.TrimSpace(name)
	if name == "" || len(name) > 100 {
		return ""
	}
	for i := 0; i < len(name); i++ {
		ch := name[i]
		if ch < 0x20 || ch > 0x7e {
			return ""
		}
		if ch == '"' || ch == '\\' || ch == ';' || ch == '/' {
			return ""
		}
	}
	return name
}

// parsePositiveInt 解析「从 1 开始」的路径 / 查询参数。
// fallback <= 0 表示这个参数是必填的，解析失败时返回 (0, false)。
func parsePositiveInt(raw string, fallback int) (int, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		if fallback > 0 {
			return fallback, true
		}
		return 0, false
	}
	value := 0
	for i := 0; i < len(raw); i++ {
		ch := raw[i]
		if ch < '0' || ch > '9' {
			return 0, false
		}
		value = value*10 + int(ch-'0')
		if value > 1_000_000 {
			return 0, false
		}
	}
	if value <= 0 {
		return 0, false
	}
	return value, true
}

// requireUID 取路径里的 uid 并做基本校验。
//
// 只校验长度和字符集，**不校验它存不存在** —— 存不存在由 service 查库决定，
// 而且归属不匹配也要走同一个「找不到」，不能让调用方通过错误信息区分出
// 「这个编号存在但不是你的」。
func requireUID(c *gin.Context, param, notFoundCode string) (string, bool) {
	uid := strings.TrimSpace(c.Param(param))
	if uid == "" || len(uid) > 32 || !dkdomain.IsValidULID(uid) {
		failCode(c, notFoundCode)
		return "", false
	}
	return uid, true
}
