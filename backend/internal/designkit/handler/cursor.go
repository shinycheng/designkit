package handler

import (
	"encoding/base64"
	"errors"
	"strconv"
	"strings"
	"time"
)

// ============================================================================
// 游标分页
// ============================================================================
//
// **一律游标分页，不用 offset。**
// offset 在持续写入时会漏数据：运营一边翻页一边有新任务插到最前面，
// 第二页会把第一页看过的行再给一遍，同时把真正的下一批挤掉。
// ERP 拉历史时这种漏数据是静默的，等发现就已经少同步了一批图。
//
// 游标 = (created_at, id) 复合值，Base64URL 编码后当成不透明字符串给客户端。
// **客户端不许解析它**，我们保留随时换编码的自由。

// ErrInvalidCursor 游标解析失败。
var ErrInvalidCursor = errors.New("designkit: invalid cursor")

const (
	// defaultPageLimit 每页默认条数。
	defaultPageLimit = 20
	// maxPageLimit 每页上限。再大就该用导出而不是翻页。
	maxPageLimit = 100
)

// encodeCursor 把 (created_at, id) 编成不透明游标。
func encodeCursor(createdAt time.Time, id int64) string {
	raw := strconv.FormatInt(createdAt.UTC().UnixNano(), 10) + ":" + strconv.FormatInt(id, 10)
	return base64.RawURLEncoding.EncodeToString([]byte(raw))
}

// decodeCursor 把游标解回 (created_at, id)。空串表示第一页，返回零值且不报错。
func decodeCursor(cursor string) (time.Time, int64, error) {
	cursor = strings.TrimSpace(cursor)
	if cursor == "" {
		return time.Time{}, 0, nil
	}
	decoded, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil {
		return time.Time{}, 0, ErrInvalidCursor
	}
	parts := strings.Split(string(decoded), ":")
	if len(parts) != 2 {
		return time.Time{}, 0, ErrInvalidCursor
	}
	nanos, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		return time.Time{}, 0, ErrInvalidCursor
	}
	id, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil || id < 0 {
		return time.Time{}, 0, ErrInvalidCursor
	}
	return time.Unix(0, nanos).UTC(), id, nil
}

// parseLimit 解析每页条数：非法值一律退回默认值，不报错
// （翻页参数写错不该让整个列表打不开）。
func parseLimit(raw string) int {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return defaultPageLimit
	}
	limit, err := strconv.Atoi(raw)
	if err != nil || limit <= 0 {
		return defaultPageLimit
	}
	if limit > maxPageLimit {
		return maxPageLimit
	}
	return limit
}
