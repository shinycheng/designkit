//go:build unit

package handler

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCursorRoundTrip(t *testing.T) {
	createdAt := time.Date(2026, 8, 13, 1, 2, 3, 456789000, time.UTC)

	cursor := encodeCursor(createdAt, 12345)
	assert.NotContains(t, cursor, ":", "游标要是不透明的，别让人一眼看出结构")
	assert.NotContains(t, cursor, "=", "用 RawURL 编码，免得游标被 URL 转义弄坏")

	gotTime, gotID, err := decodeCursor(cursor)
	require.NoError(t, err)
	assert.True(t, gotTime.Equal(createdAt), "时间要能原样还原：%v != %v", gotTime, createdAt)
	assert.Equal(t, int64(12345), gotID)
}

func TestDecodeCursorEmptyMeansFirstPage(t *testing.T) {
	gotTime, gotID, err := decodeCursor("")
	require.NoError(t, err)
	assert.True(t, gotTime.IsZero())
	assert.Zero(t, gotID)
}

func TestDecodeCursorRejectsGarbage(t *testing.T) {
	for _, bad := range []string{"not-base64!!", "YWJj", "MTIzOmFiYw"} {
		_, _, err := decodeCursor(bad)
		assert.ErrorIs(t, err, ErrInvalidCursor, "输入 %q", bad)
	}
}

func TestParseLimit(t *testing.T) {
	assert.Equal(t, defaultPageLimit, parseLimit(""))
	assert.Equal(t, defaultPageLimit, parseLimit("0"))
	assert.Equal(t, defaultPageLimit, parseLimit("-5"))
	assert.Equal(t, defaultPageLimit, parseLimit("abc"))
	assert.Equal(t, 7, parseLimit("7"))
	assert.Equal(t, maxPageLimit, parseLimit("100000"))
}

func TestParsePositiveInt(t *testing.T) {
	value, ok := parsePositiveInt("3", 0)
	assert.True(t, ok)
	assert.Equal(t, 3, value)

	value, ok = parsePositiveInt("", 1)
	assert.True(t, ok)
	assert.Equal(t, 1, value, "空值走默认")

	_, ok = parsePositiveInt("", 0)
	assert.False(t, ok, "没有默认值时空值不合法")

	for _, bad := range []string{"0", "-1", "1.5", "abc", "99999999"} {
		_, ok = parsePositiveInt(bad, 0)
		assert.False(t, ok, "输入 %q", bad)
	}
}

func TestSafeAttachmentName(t *testing.T) {
	assert.Equal(t, "job-1-1.png", safeAttachmentName("job-1-1.png"))
	assert.Empty(t, safeAttachmentName(""))
	assert.Empty(t, safeAttachmentName(`bad".png`), "带引号会把响应头拆坏")
	assert.Empty(t, safeAttachmentName("裙子.png"), "非 ASCII 一律不下发")
	assert.Empty(t, safeAttachmentName("a/b.png"), "路径分隔符一律不下发")
}
