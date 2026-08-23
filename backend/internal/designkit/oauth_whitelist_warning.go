// 启动期配置核对：邮箱后缀白名单 × 第三方 OAuth 登录（CLAUDE.md 第七节点名的缺口）。
//
// 事实（2026-08-23 逐个函数核对过）：
//
//	上游的邮箱后缀白名单（validateRegistrationEmailQuota）只挂在「邮箱注册」上：
//	auth_service.go 的三条邮箱注册路径，以及 OAuth 补绑邮箱的 pending 流程
//	（auth_oauth_email_flow.go）都会调它。
//	而第三方 OAuth 的**直接注册**从头到尾不调它：
//	  - LinuxDo / 钉钉 / 微信 / OIDC 走 loginOrRegisterOAuthWithTokenPair
//	    （auth_service.go），全程没有白名单校验；
//	  - GitHub / Google 的自动流程（auth_email_oauth_auto.go）直接
//	    userRepo.Create，同样绕过。
//
// 后果：白名单填了公司域名、以为「外人注册不进来」，但只要任何一个第三方登录
// 开着，外人换那条路照样能建号。邀请码那道门槛不受影响，OAuth 注册仍要邀请码
// （决策 9 两道门槛是 AND 的前提就此被削掉一半）。
//
// 我们**不改上游鉴权逻辑**——注册链路动一刀的风险远大于收益，而且新用户默认
// 余额是 0（决策 9），混进来的号也花不了钱。这里只做启动期断言：两个条件同时
// 成立时在启动日志里打一条显眼的中文 slog.Error，**不阻断启动**。
//
// （上面这段与 package 声明隔一行，是有意的：包注释在 module.go，别重复。）

package designkit

import (
	"context"
	"log/slog"
	"strings"

	upstreamservice "github.com/Wei-Shaw/sub2api/internal/service"
)

// registrationGateReader 是这项核对要读的两个设置口子。
// 单独抽成小接口是为了单测能用桩替换——真 *SettingService 要数据库才建得起来。
type registrationGateReader interface {
	GetRegistrationEmailSuffixWhitelist(ctx context.Context) []string
	GetPublicSettings(ctx context.Context) (*upstreamservice.PublicSettings, error)
}

// 编译期确认上游 SettingService 满足这个接口（签名变了在这里第一时间炸出来）。
var _ registrationGateReader = (*upstreamservice.SettingService)(nil)

// warnEmailWhitelistOAuthBypass 在「邮箱后缀白名单非空 && 任一第三方 OAuth
// 登录开启」时打一条 slog.Error 提醒，其余情况保持安静。永不 panic、永不阻断。
func warnEmailWhitelistOAuthBypass(ctx context.Context, settings registrationGateReader) {
	if settings == nil {
		return
	}

	whitelist := settings.GetRegistrationEmailSuffixWhitelist(ctx)
	if len(whitelist) == 0 {
		// 白名单留空 = 放行所有邮箱（决策 9 特别标注过的语义），
		// 本来就没有「白名单在拦人」的预期，无需提醒。
		return
	}

	pub, err := settings.GetPublicSettings(ctx)
	if err != nil {
		// 读不到设置就不硬猜。只说核对没做成，不喊「配置冲突」。
		slog.Warn("designkit 启动核对没能读到系统设置，"+
			"「邮箱白名单对第三方 OAuth 注册不生效」这一项这次没查",
			slog.Any("error", err))
		return
	}
	if pub == nil {
		return
	}

	// 名单顺序 = 上游设置页里的展示顺序，方便管理员逐个对着关。
	var enabled []string
	if pub.LinuxDoOAuthEnabled {
		enabled = append(enabled, "LinuxDo")
	}
	if pub.DingTalkOAuthEnabled {
		enabled = append(enabled, "钉钉")
	}
	if pub.WeChatOAuthEnabled {
		enabled = append(enabled, "微信")
	}
	if pub.OIDCOAuthEnabled {
		enabled = append(enabled, "OIDC")
	}
	if pub.GitHubOAuthEnabled {
		enabled = append(enabled, "GitHub")
	}
	if pub.GoogleOAuthEnabled {
		enabled = append(enabled, "Google")
	}
	if len(enabled) == 0 {
		return
	}

	slog.Error("配置冲突：邮箱后缀白名单已配置，但下列第三方登录同时开着："+
		strings.Join(enabled, "、")+
		"。邮箱后缀白名单只校验邮箱注册，对第三方 OAuth 注册不生效——"+
		"外人用上面这些登录方式可以绕过白名单直接建号；"+
		"邀请码注册开着的话仍然拦得住（OAuth 注册也要邀请码）。"+
		"放公网前请关掉这些第三方登录，或确认邀请码注册已开启。"+
		"系统照常启动，此处只提醒、不拦截。",
		slog.String("email_suffix_whitelist", strings.Join(whitelist, ",")),
		slog.String("enabled_oauth", strings.Join(enabled, ",")))
}
