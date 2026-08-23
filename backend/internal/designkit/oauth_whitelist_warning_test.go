//go:build unit

package designkit

// warnEmailWhitelistOAuthBypass 的开关组合矩阵：
// 只有「白名单非空 && 任一第三方 OAuth 开启」这一格该出警告，其余格保持安静。
// 警告是给部署它的管理员看的最后一道保险，静默失灵比误报更糟，所以两个方向都测。

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"

	upstreamservice "github.com/Wei-Shaw/sub2api/internal/service"
)

// stubGateReader 用桩顶替真 SettingService（真的那个要数据库才建得起来）。
type stubGateReader struct {
	whitelist []string
	pub       *upstreamservice.PublicSettings
	err       error
}

func (s stubGateReader) GetRegistrationEmailSuffixWhitelist(context.Context) []string {
	return s.whitelist
}

func (s stubGateReader) GetPublicSettings(context.Context) (*upstreamservice.PublicSettings, error) {
	return s.pub, s.err
}

// captureSlog 把 slog 默认输出临时接到内存里，跑完恢复。
// 因为动的是进程级默认 logger，这个文件里的测试都不能 t.Parallel()。
func captureSlog(t *testing.T, fn func()) string {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	defer slog.SetDefault(prev)
	fn()
	return buf.String()
}

// conflictMarker 是警告文案的开头——测试认它，不逐字钉死整段话。
const conflictMarker = "配置冲突"

func TestWarnEmailWhitelistOAuthBypass(t *testing.T) {
	whitelist := []string{"example.com"}

	tests := []struct {
		name         string
		reader       registrationGateReader
		wantWarn     bool
		wantContains []string // 仅在 wantWarn=true 时检查
	}{
		{
			name: "白名单非空且钉钉开启_出警告并点名钉钉",
			reader: stubGateReader{
				whitelist: whitelist,
				pub:       &upstreamservice.PublicSettings{DingTalkOAuthEnabled: true},
			},
			wantWarn:     true,
			wantContains: []string{"钉钉", "邀请码"},
		},
		{
			name: "多个OAuth同时开启_逐个点名",
			reader: stubGateReader{
				whitelist: whitelist,
				pub: &upstreamservice.PublicSettings{
					LinuxDoOAuthEnabled: true,
					WeChatOAuthEnabled:  true,
					OIDCOAuthEnabled:    true,
					GitHubOAuthEnabled:  true,
					GoogleOAuthEnabled:  true,
				},
			},
			wantWarn:     true,
			wantContains: []string{"LinuxDo", "微信", "OIDC", "GitHub", "Google"},
		},
		{
			name: "白名单非空但所有OAuth关闭_安静",
			reader: stubGateReader{
				whitelist: whitelist,
				pub:       &upstreamservice.PublicSettings{},
			},
			wantWarn: false,
		},
		{
			name: "白名单为空即放行所有邮箱_即使OAuth开启也安静",
			reader: stubGateReader{
				whitelist: nil,
				pub:       &upstreamservice.PublicSettings{DingTalkOAuthEnabled: true},
			},
			wantWarn: false,
		},
		{
			name: "读设置失败_不硬猜不喊冲突",
			reader: stubGateReader{
				whitelist: whitelist,
				err:       errors.New("db down"),
			},
			wantWarn: false,
		},
		{
			name: "设置返回nil_安静",
			reader: stubGateReader{
				whitelist: whitelist,
				pub:       nil,
			},
			wantWarn: false,
		},
		{
			name:     "settings为nil_不panic且安静",
			reader:   nil,
			wantWarn: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out := captureSlog(t, func() {
				warnEmailWhitelistOAuthBypass(context.Background(), tt.reader)
			})
			if got := strings.Contains(out, conflictMarker); got != tt.wantWarn {
				t.Fatalf("警告出现=%v，期望=%v，日志输出：%q", got, tt.wantWarn, out)
			}
			for _, want := range tt.wantContains {
				if !strings.Contains(out, want) {
					t.Errorf("警告里少了 %q，日志输出：%q", want, out)
				}
			}
		})
	}
}
