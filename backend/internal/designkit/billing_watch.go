package designkit

// billing_watch.go —— B2 巡检：出图模型绝不许被配成按 token 计价。
//
// 背景（CLAUDE.md 二·五 B2）：只要某分组对出图模型存在一条渠道定价
// （channel_model_pricing），且它的 billing_mode 是 "token" **或留空**
// （空串在上游 model_pricing_resolver.go:71-73 被默认成 token），整单就按
// input/output token 计价——按次价完全不参与，极端情况一张图算成 0 或算成文本价。
// 跟上游返不返 token 毫无关系，纯粹取决于后台那一栏有没有选对。
//
// 之前靠配置手册一句「渠道定价一条都不要建」的人工约定顶着（2026-08-23 实查
// 该表为 0 行）。本巡检把约定变成机器盯：启动时查一次，之后每 6 小时一次；
// 命中就 slog.Error 打显眼中文告警（dk-healthwatch.sh 巡检的是容器和磁盘，
// 这类配置错误只有日志能带出来；告警行带「配置冲突」标记，方便 grep）。
//
// 刻意**只读不改**：自动改上游配置等于替管理员做决定，错了没人知道。
// 也刻意不阻断启动——出图还能跑，只是计价可能错，告警比停机对运营友好。

import (
	"context"
	"database/sql"
	"log/slog"
	"strings"
	"time"
)

// billingWatchInterval 巡检间隔。渠道定价是管理员手工操作，6 小时足够。
const billingWatchInterval = 6 * time.Hour

// billingWatchQuery 找出「会把出图模型拖进 token 计价」的渠道定价行。
//
// models 是 JSONB 数组；用 models::text ILIKE 做宽松匹配——宁可多报
// （比如模型名是别的字符串的子串）也不漏报：告警的代价是管理员看一眼，
// 漏报的代价是每一张图都计错价。
const billingWatchQuery = `
SELECT COUNT(*)
FROM channel_model_pricing
WHERE models::text ILIKE '%' || $1 || '%'
  AND (billing_mode IS NULL OR billing_mode = '' OR billing_mode = 'token')`

// watchBillingMode 启动巡检 goroutine。db 为 nil 时静默不启（测试/降级场景）。
func watchBillingMode(ctx context.Context, db *sql.DB, imageModel string) {
	if db == nil || strings.TrimSpace(imageModel) == "" {
		return
	}
	check := func() {
		cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		var n int
		if err := db.QueryRowContext(cctx, billingWatchQuery, imageModel).Scan(&n); err != nil {
			// 查不了只降噪提示：数据库不可用时别的告警早就响了。
			slog.Warn("designkit 计费巡检没查成，下轮再试", slog.Any("err", err))
			return
		}
		if n > 0 {
			slog.Error("designkit 配置冲突：出图模型存在 token 计价的渠道定价，"+
				"每张图会按文本 token 而不是按次计价（极端情况算成 0）。"+
				"去后台把该渠道定价的 billing_mode 改成按次/按图，或直接删掉那条定价。",
				slog.String("model", imageModel),
				slog.Int("bad_rows", n))
		}
	}
	check()
	go func() {
		ticker := time.NewTicker(billingWatchInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				check()
			}
		}
	}()
}
