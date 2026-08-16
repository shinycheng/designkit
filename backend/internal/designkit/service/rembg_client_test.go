//go:build unit

package service

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	dkdomain "github.com/Wei-Shaw/sub2api/internal/designkit/domain"

	"github.com/stretchr/testify/require"
)

// TestNewRembgClient_ModelWhitelist 白名单是许可证护栏：
// bria-rmbg 一系是 BRIA 的**非商用权重**，必须在构造时就被拒掉，
// 不能等到有人把它填进配置、跑了半年才被发现。
func TestNewRembgClient_ModelWhitelist(t *testing.T) {
	for _, model := range []string{"isnet-general-use", "u2net", "u2netp", "birefnet-general-lite"} {
		_, err := NewRembgClient(RembgClientConfig{BaseURL: "http://rembg.test", Model: model})
		require.NoError(t, err, "白名单里的模型 %s 应该被接受", model)
	}

	// ⛔ bria-rmbg 一系（含新版本号）是这份白名单存在的直接原因；
	// birefnet-massive 和 sam 是「不在名单里就一律拒」的抽查。
	for _, model := range []string{"bria-rmbg", "bria-rmbg-2.0", "birefnet-massive", "sam"} {
		_, err := NewRembgClient(RembgClientConfig{BaseURL: "http://rembg.test", Model: model})
		require.Error(t, err, "白名单之外的模型 %s 必须被拒绝", model)
		require.Contains(t, err.Error(), "白名单", "报错要说清是许可证白名单拦的")
	}
}

// TestNewRembgClient_DefaultModel 空模型退回默认值（isnet-general-use），不报错。
func TestNewRembgClient_DefaultModel(t *testing.T) {
	client, err := NewRembgClient(RembgClientConfig{BaseURL: "http://rembg.test"})
	require.NoError(t, err)
	require.Equal(t, DefaultRembgModel, client.model)
}

// TestNewRembgClient_RejectsBadURL 地址不合法当场报错，不延迟到第一次调用。
func TestNewRembgClient_RejectsBadURL(t *testing.T) {
	for _, base := range []string{"", "   ", "not-a-url", "ftp://x"} {
		_, err := NewRembgClient(RembgClientConfig{BaseURL: base})
		require.Error(t, err, "地址 %q 应该被拒绝", base)
	}
}

// TestRembgClient_RemoveBackground 走一遍真实的 HTTP：multipart 字段名必须是 file、
// **model 必须是表单字段**（query 会被 rembg 忽略并回落 u2net，2026-08-16 实测）、
// 响应体原样返回。
func TestRembgClient_RemoveBackground(t *testing.T) {
	want := testPNG(t, 4, 4)
	var gotModel, gotFilename string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, http.MethodPost, r.Method)
		require.Equal(t, rembgRemovePath, r.URL.Path)
		gotModel = r.FormValue("model")

		file, header, err := r.FormFile("file")
		require.NoError(t, err, "multipart 字段名必须是 file")
		defer func() { _ = file.Close() }()
		gotFilename = header.Filename
		data, err := io.ReadAll(file)
		require.NoError(t, err)
		require.NotEmpty(t, data)

		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write(want)
	}))
	defer server.Close()

	client, err := NewRembgClient(RembgClientConfig{BaseURL: server.URL})
	require.NoError(t, err)

	got, err := client.RemoveBackground(context.Background(), testPNG(t, 2, 2), "sample.png")
	require.NoError(t, err)
	require.Equal(t, want, got)
	require.Equal(t, DefaultRembgModel, gotModel)
	require.Equal(t, "sample.png", gotFilename)
}

// TestRembgClient_TranslatesServerError 服务端 5xx 要变成我方错误码 + 中文文案，
// 不能把 rembg 的英文原文直接甩给运营。
func TestRembgClient_TranslatesServerError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "internal boom", http.StatusInternalServerError)
	}))
	defer server.Close()

	client, err := NewRembgClient(RembgClientConfig{BaseURL: server.URL})
	require.NoError(t, err)

	_, err = client.RemoveBackground(context.Background(), testPNG(t, 2, 2), "a.png")
	require.Error(t, err)
	var dkErr *dkdomain.DesignkitError
	require.ErrorAs(t, err, &dkErr)
	require.Equal(t, dkdomain.ErrCodePreprocessFailed, dkErr.Code)
	require.Contains(t, dkErr.Message, "没抠出来")
	require.Contains(t, dkErr.Upstream, "500", "上游原文要落 Upstream 给管理员排障")
}

// TestRembgClient_EmptyResponse 空响应体是失败，不是一张空图。
func TestRembgClient_EmptyResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	client, err := NewRembgClient(RembgClientConfig{BaseURL: server.URL})
	require.NoError(t, err)

	_, err = client.RemoveBackground(context.Background(), testPNG(t, 2, 2), "a.png")
	require.Error(t, err)
	require.Contains(t, err.Error(), "空图片")
}

// TestRembgClient_NilClient nil 客户端返回「还没准备好」，不 panic。
func TestRembgClient_NilClient(t *testing.T) {
	var client *RembgClient
	_, err := client.RemoveBackground(context.Background(), []byte{1}, "a.png")
	require.Error(t, err)
	require.Contains(t, err.Error(), "还没准备好")
}
