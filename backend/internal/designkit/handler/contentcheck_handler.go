package handler

import (
	"net/http"
	"strings"
	"unicode/utf8"

	dkdomain "github.com/Wei-Shaw/sub2api/internal/designkit/domain"
	"github.com/Wei-Shaw/sub2api/internal/designkit/wordcheck"

	"github.com/gin-gonic/gin"
)

// ============================================================================
// 文案检查（违禁词 + 标题字数）
// ============================================================================
//
// 一个端点：
//
//	POST /content/check   {text, platform?} → {hits, title_len, title_max, platform_name}
//
// 纯计算：不落库、不计费、不发网络请求。词库和平台规则表都在
// internal/designkit/wordcheck 包里（编译期 embed），所以这个 handler
// 没有任何服务依赖，永远可用——前端靠这一点做输入防抖实时检查。

// maxContentCheckRunes 一次最多检查多少字。
//
// 标题几十字、详情文案几千字都够用；不设上限的话这是个不鉴权成本的
// CPU 放大器（一次请求带几十 MB 文本，逐词扫描就是几十亿次比较）。
const maxContentCheckRunes = 10000

// ContentCheckHandler 「文案检查」页。
type ContentCheckHandler struct{}

// NewContentCheckHandler 建 handler。没有依赖，永不为 nil。
func NewContentCheckHandler() *ContentCheckHandler {
	return &ContentCheckHandler{}
}

// contentCheckBody 是 POST /content/check 的请求体。
type contentCheckBody struct {
	// Text 要检查的文字。允许为空（返回零命中、零字数），方便调用方边输边查。
	Text string `json:"text"`
	// Platform 平台标识（taobao/tmall/jd/pdd/douyin/amazon），选填。
	// 空 = 不校验标题字数（title_max 返回 0）。
	Platform string `json:"platform"`
}

// Check 处理 POST /content/check。
func (h *ContentCheckHandler) Check(c *gin.Context) {
	if _, ok := userIDOf(c); !ok {
		failCode(c, dkdomain.ErrCodeUnauthorized)
		return
	}
	var body contentCheckBody
	if err := c.ShouldBindJSON(&body); err != nil {
		failCodef(c, dkdomain.ErrCodeInvalidRequest, "请求体不是合法的 JSON：%v", err)
		return
	}
	if !utf8.ValidString(body.Text) {
		failCodef(c, dkdomain.ErrCodeInvalidRequest, "文字里有无法识别的字符，重新粘贴后再试。")
		return
	}
	if utf8.RuneCountInString(body.Text) > maxContentCheckRunes {
		failCodef(c, dkdomain.ErrCodeInvalidRequest, "一次最多检查 %d 个字，分段检查。", maxContentCheckRunes)
		return
	}

	var platform wordcheck.Platform
	if key := strings.TrimSpace(body.Platform); key != "" {
		found, ok := wordcheck.PlatformByKey(key)
		if !ok {
			failCodef(c, dkdomain.ErrCodeInvalidRequest,
				"没有这个平台标识：%s。可选：%s，留空则不校验字数。",
				key, strings.Join(wordcheck.PlatformKeys(), " / "))
			return
		}
		platform = found
	}

	// hits 永远给数组，不给 null——前端少一个判空分支就少一个崩法（同 chat 的规矩）。
	hits := wordcheck.Check(body.Text)
	out := make([]gin.H, 0, len(hits))
	for _, hit := range hits {
		out = append(out, gin.H{
			"word":  hit.Word,
			"start": hit.Start,
			"end":   hit.End,
		})
	}

	c.JSON(http.StatusOK, gin.H{
		"hits": out,
		// title_len 按字（rune）计，首尾空白不算；start/end 同一口径。
		"title_len": wordcheck.TitleLen(body.Text),
		// title_max 为 0 = 没选平台，不校验字数。
		"title_max":     platform.TitleMaxRunes,
		"platform_name": platform.Name,
	})
}
