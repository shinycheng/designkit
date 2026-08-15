package repository

// 纯逻辑小工具：游标编解码、分页条数收敛、ILIKE 转义、状态来源白名单。
//
// 这里的每个函数都**不碰数据库**，所以全部有单测（cursor_test.go / query_build_test.go）。
// 本机没有 Go 编译器也没有 Docker，能靠纯逻辑测到的就必须测到。

import (
	"encoding/base64"
	"fmt"
	"strconv"
	"strings"
	"time"

	dkdomain "github.com/Wei-Shaw/sub2api/internal/designkit/domain"
)

const (
	// DefaultPageLimit 列表默认每页条数。
	DefaultPageLimit = 20
	// MaxPageLimit 每页条数上限。ERP 一次拉太多会把内存打满。
	MaxPageLimit = 100
)

// clampLimit 把调用方给的每页条数收敛到 [1, max]。
// 0 或负数用默认值；超过上限压到上限。**绝不返回 0**，否则查询会一条都不返。
func clampLimit(limit, def, max int) int {
	if limit <= 0 {
		limit = def
	}
	if limit > max {
		limit = max
	}
	if limit < 1 {
		limit = 1
	}
	return limit
}

// 游标格式：base64url( RFC3339Nano时间 + "|" + 自增主键 )。
//
// 为什么不用 offset：offset 在持续写入时会漏数据（设计定型 3.3）。
// 为什么把 id 也编进去：created_at 会撞（同一批任务可能同一微秒建的），
// 只按时间翻页会漏行或重复行，(created_at, id) 才是全序。
//
// 编码只是为了让 ERP 把它当**不透明字符串**原样回传，不要自己拼时间。
const cursorSeparator = "|"

// EncodeCursor 把 (created_at, id) 编成一个不透明游标串。id <= 0 时返回空串（没有下一页）。
func EncodeCursor(createdAt time.Time, id int64) string {
	if id <= 0 {
		return ""
	}
	raw := createdAt.UTC().Format(time.RFC3339Nano) + cursorSeparator + strconv.FormatInt(id, 10)
	return base64.RawURLEncoding.EncodeToString([]byte(raw))
}

// DecodeCursor 把游标串解回 (created_at, id)。空串表示第一页，返回零值且不报错。
func DecodeCursor(cursor string) (time.Time, int64, error) {
	cursor = strings.TrimSpace(cursor)
	if cursor == "" {
		return time.Time{}, 0, nil
	}
	decoded, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil {
		return time.Time{}, 0, fmt.Errorf("designkit: 游标不是合法的 base64: %w", err)
	}
	parts := strings.Split(string(decoded), cursorSeparator)
	if len(parts) != 2 {
		return time.Time{}, 0, fmt.Errorf("designkit: 游标格式不对: %q", cursor)
	}
	createdAt, err := time.Parse(time.RFC3339Nano, parts[0])
	if err != nil {
		return time.Time{}, 0, fmt.Errorf("designkit: 游标里的时间解析失败: %w", err)
	}
	id, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil || id <= 0 {
		return time.Time{}, 0, fmt.Errorf("designkit: 游标里的 id 解析失败: %q", cursor)
	}
	return createdAt, id, nil
}

// escapeLikePattern 转义 ILIKE 模式里的通配符。
//
// 不转义的后果：运营在灵感库搜索框里打一个 `%` 就等于「全表匹配」，
// 打 `_` 会匹配任意单字符，搜出来的结果莫名其妙。
// PostgreSQL 的 LIKE/ILIKE 默认转义符就是反斜杠，所以反斜杠自己也要转。
func escapeLikePattern(s string) string {
	replacer := strings.NewReplacer(
		`\`, `\\`,
		`%`, `\%`,
		`_`, `\_`,
	)
	return replacer.Replace(s)
}

// likeContains 拼出 ILIKE 用的 %关键词% 模式。
func likeContains(keyword string) string {
	return "%" + escapeLikePattern(keyword) + "%"
}

// allowedSourceStatuses 返回「能合法迁到 to 的全部来源状态」。
//
// 用在带守卫的 UPDATE 上：`WHERE ... AND status = ANY($n)`。
// 这样状态迁移白名单（domain.CanTransition）就是唯一判据，
// SQL 里不会出现第二份手写的状态清单——两份清单迟早会不一致，
// 而状态写错的代价是「钱扣了、任务显示已取消」。
func allowedSourceStatuses(to dkdomain.JobStatus) []string {
	var out []string
	for _, from := range dkdomain.AllJobStatuses() {
		if dkdomain.CanTransition(from, to) {
			out = append(out, from.String())
		}
	}
	return out
}

// seconds 把 Duration 转成 make_interval(secs => ?) 要的秒数。
// 非正数一律用兜底值，避免算出「租约立刻过期」这种自伤。
func seconds(d time.Duration, fallback time.Duration) float64 {
	if d <= 0 {
		d = fallback
	}
	return d.Seconds()
}
