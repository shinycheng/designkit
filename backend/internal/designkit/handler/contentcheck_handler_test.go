//go:build unit

package handler

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// contentCheckResp 是 POST /content/check 的响应形状（对外契约，字段名别改）。
type contentCheckResp struct {
	Hits []struct {
		Word  string `json:"word"`
		Start int    `json:"start"`
		End   int    `json:"end"`
	} `json:"hits"`
	TitleLen     int    `json:"title_len"`
	TitleMax     int    `json:"title_max"`
	PlatformName string `json:"platform_name"`
}

// TestContentCheck 全程用空 Services 建引擎——文案检查不依赖任何服务，
// 「别的服务全缺席时这个端点也得活着」本身就是要测的性质。
func TestContentCheck(t *testing.T) {
	engine := newTestEngine(t, Services{}, 7)

	t.Run("命中加字数", func(t *testing.T) {
		rec := doRequest(t, engine, http.MethodPost, "/api/v1/designkit/content/check",
			`{"text":"全网第一连衣裙","platform":"taobao"}`, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("状态码 %d，响应：%s", rec.Code, rec.Body.String())
		}
		var resp contentCheckResp
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("响应不是合法 JSON：%v", err)
		}
		if len(resp.Hits) != 1 || resp.Hits[0].Word != "全网第一" || resp.Hits[0].Start != 0 || resp.Hits[0].End != 4 {
			t.Fatalf("命中不对：%+v", resp.Hits)
		}
		if resp.TitleLen != 7 {
			t.Fatalf("title_len = %d，期望 7", resp.TitleLen)
		}
		if resp.TitleMax != 30 || resp.PlatformName != "淘宝" {
			t.Fatalf("平台规则不对：max=%d name=%q", resp.TitleMax, resp.PlatformName)
		}
	})

	t.Run("不选平台时hits给空数组不给null", func(t *testing.T) {
		rec := doRequest(t, engine, http.MethodPost, "/api/v1/designkit/content/check",
			`{"text":"纯棉白色圆领短袖"}`, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("状态码 %d，响应：%s", rec.Code, rec.Body.String())
		}
		body := rec.Body.String()
		if !strings.Contains(body, `"hits":[]`) {
			t.Fatalf("hits 必须是空数组不能是 null：%s", body)
		}
		var resp contentCheckResp
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("响应不是合法 JSON：%v", err)
		}
		if resp.TitleMax != 0 || resp.PlatformName != "" {
			t.Fatalf("没选平台时 title_max 应为 0、platform_name 应为空：%+v", resp)
		}
		if resp.TitleLen != 8 {
			t.Fatalf("title_len = %d，期望 8", resp.TitleLen)
		}
	})

	t.Run("ERP前缀同样能用", func(t *testing.T) {
		rec := doRequest(t, engine, http.MethodPost, "/v1/designkit/content/check",
			`{"text":"顶级品质","platform":"jd"}`, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("状态码 %d，响应：%s", rec.Code, rec.Body.String())
		}
		var resp contentCheckResp
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("响应不是合法 JSON：%v", err)
		}
		if len(resp.Hits) != 1 || resp.Hits[0].Word != "顶级" {
			t.Fatalf("命中不对：%+v", resp.Hits)
		}
		if resp.TitleMax != 45 {
			t.Fatalf("京东上限应为 45，得到 %d", resp.TitleMax)
		}
	})

	t.Run("不认识的平台报DK_INVALID_REQUEST", func(t *testing.T) {
		rec := doRequest(t, engine, http.MethodPost, "/api/v1/designkit/content/check",
			`{"text":"随便","platform":"ebay"}`, nil)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("状态码 %d，期望 400。响应：%s", rec.Code, rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), "DK_INVALID_REQUEST") {
			t.Fatalf("错误码不对：%s", rec.Body.String())
		}
	})

	t.Run("没登录报401", func(t *testing.T) {
		anon := newTestEngine(t, Services{}, 0)
		rec := doRequest(t, anon, http.MethodPost, "/api/v1/designkit/content/check",
			`{"text":"随便"}`, nil)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("状态码 %d，期望 401。响应：%s", rec.Code, rec.Body.String())
		}
	})
}
