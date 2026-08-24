//go:build unit

package service

// ============================================================================
// 图片自动清理
// ============================================================================
//
// 这一组守四条底线：
//  1. 开关关着（或没配过）一个字节都不动；
//  2. 保留天数越界 fail-closed：整轮不跑，绝不收敛成更激进的口径去删文件；
//  3. 文件删失败的行**不软删**（留给下一轮重试），且整批全失败时不死循环；
//  4. 分批干活：一批最多 BatchLimit 条，批满了接着捞下一批，直到捞空。
//
// 「被未结束批次引用的素材不删」在 SQL 里实现，
// 由 repository/cleanup_repo_test.go 守着，这里不重复。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	dkdomain "github.com/Wei-Shaw/sub2api/internal/designkit/domain"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ----------------------------------------------------------------------------
// 假实现
// ----------------------------------------------------------------------------

type cleanupListCall struct {
	cutoff time.Time
	limit  int
}

// fakeCleanupStore 按预置批次逐次吐数据；软删只记录、按条数返回。
type fakeCleanupStore struct {
	imageBatches [][]dkdomain.CleanupCandidate
	assetBatches [][]dkdomain.CleanupCandidate
	variantKeys  map[int64][]string

	// staticImages 非空时每次都返回同一批（模拟「软删没发生、下一轮还筛得出来」）。
	staticImages []dkdomain.CleanupCandidate

	imageCalls        []cleanupListCall
	assetCalls        []cleanupListCall
	softDeletedImages [][]int64
	softDeletedAssets [][]int64
}

func (f *fakeCleanupStore) ListExpiredImages(_ context.Context, cutoff time.Time, limit int) ([]dkdomain.CleanupCandidate, error) {
	f.imageCalls = append(f.imageCalls, cleanupListCall{cutoff: cutoff, limit: limit})
	if f.staticImages != nil {
		if len(f.staticImages) > limit {
			return f.staticImages[:limit], nil
		}
		return f.staticImages, nil
	}
	if len(f.imageBatches) == 0 {
		return nil, nil
	}
	batch := f.imageBatches[0]
	f.imageBatches = f.imageBatches[1:]
	return batch, nil
}

func (f *fakeCleanupStore) ListExpiredAssets(_ context.Context, cutoff time.Time, limit int) ([]dkdomain.CleanupCandidate, error) {
	f.assetCalls = append(f.assetCalls, cleanupListCall{cutoff: cutoff, limit: limit})
	if len(f.assetBatches) == 0 {
		return nil, nil
	}
	batch := f.assetBatches[0]
	f.assetBatches = f.assetBatches[1:]
	return batch, nil
}

func (f *fakeCleanupStore) ListVariantKeysByAsset(_ context.Context, assetIDs []int64) (map[int64][]string, error) {
	out := make(map[int64][]string, len(assetIDs))
	for _, id := range assetIDs {
		if keys, ok := f.variantKeys[id]; ok {
			out[id] = keys
		}
	}
	return out, nil
}

func (f *fakeCleanupStore) SoftDeleteImages(_ context.Context, ids []int64) (int64, error) {
	f.softDeletedImages = append(f.softDeletedImages, ids)
	return int64(len(ids)), nil
}

func (f *fakeCleanupStore) SoftDeleteAssets(_ context.Context, ids []int64) (int64, error) {
	f.softDeletedAssets = append(f.softDeletedAssets, ids)
	return int64(len(ids)), nil
}

func (f *fakeCleanupStore) allSoftDeletedImageIDs() []int64 {
	var out []int64
	for _, batch := range f.softDeletedImages {
		out = append(out, batch...)
	}
	return out
}

// fakeCleanupObjects 记录删了哪些文件；failKeys 里的 key 删除报错。
type fakeCleanupObjects struct {
	deleted  []string
	failKeys map[string]bool
}

func (f *fakeCleanupObjects) Put(context.Context, string, string, []byte) error {
	return errors.New("清理不该写文件")
}

func (f *fakeCleanupObjects) Get(context.Context, string) ([]byte, string, error) {
	return nil, "", errors.New("清理不该读文件")
}

func (f *fakeCleanupObjects) Delete(_ context.Context, key string) error {
	if f.failKeys[key] {
		return fmt.Errorf("磁盘坏了：%s", key)
	}
	f.deleted = append(f.deleted, key)
	return nil
}

func (f *fakeCleanupObjects) Exists(context.Context, string) (bool, error) {
	return false, nil
}

// fakeCleanupSettings 只认给进来的键值；没有的键返回 ErrNotFound（跟真 repo 一致）。
type fakeCleanupSettings struct {
	values map[string]string
}

func (f *fakeCleanupSettings) GetSetting(_ context.Context, key string) (*dkdomain.Setting, error) {
	raw, ok := f.values[key]
	if !ok {
		return nil, dkdomain.ErrNotFound
	}
	return &dkdomain.Setting{Key: key, Value: json.RawMessage(raw)}, nil
}

// ----------------------------------------------------------------------------
// 组装
// ----------------------------------------------------------------------------

var cleanupTestNow = time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC)

func newCleanupForTest(t *testing.T, store *fakeCleanupStore, objects *fakeCleanupObjects, settings map[string]string, batchLimit int) *CleanupService {
	t.Helper()
	svc, err := NewCleanupService(CleanupServiceDeps{
		Store:      store,
		Objects:    objects,
		Settings:   &fakeCleanupSettings{values: settings},
		BatchLimit: batchLimit,
		Now:        func() time.Time { return cleanupTestNow },
	})
	require.NoError(t, err)
	return svc
}

func enabledSettings(days int) map[string]string {
	return map[string]string{
		dkdomain.SettingKeyCleanupEnabled:       "true",
		dkdomain.SettingKeyCleanupRetentionDays: fmt.Sprintf("%d", days),
	}
}

// ----------------------------------------------------------------------------
// 1. 开关
// ----------------------------------------------------------------------------

// 开关明确是 false，或者根本没配过：一个字节都不动。
func TestCleanupDoesNothingWhenDisabled(t *testing.T) {
	for name, settings := range map[string]map[string]string{
		"开关是 false": {
			dkdomain.SettingKeyCleanupEnabled:       "false",
			dkdomain.SettingKeyCleanupRetentionDays: "180",
		},
		"从没配过": {},
		"开关存的不是布尔（按关处理）": {
			dkdomain.SettingKeyCleanupEnabled: `"yes"`,
		},
	} {
		t.Run(name, func(t *testing.T) {
			store := &fakeCleanupStore{
				imageBatches: [][]dkdomain.CleanupCandidate{{{ID: 1, ObjectKey: "k1"}}},
			}
			objects := &fakeCleanupObjects{}
			svc := newCleanupForTest(t, store, objects, settings, 0)

			report, err := svc.RunOnce(context.Background())
			require.NoError(t, err)
			assert.False(t, report.Ran)
			assert.NotEmpty(t, report.SkipReason)
			// 关着时连「筛一下」都不做，更不许碰文件。
			assert.Empty(t, store.imageCalls)
			assert.Empty(t, store.assetCalls)
			assert.Empty(t, objects.deleted)
		})
	}
}

// ----------------------------------------------------------------------------
// 2. 保留天数边界
// ----------------------------------------------------------------------------

// 上下限之内正常跑，cutoff 必须恰好是 now − 天数×24h。
func TestCleanupRetentionDaysWithinRange(t *testing.T) {
	for _, days := range []int{dkdomain.MinCleanupRetentionDays, 180, dkdomain.MaxCleanupRetentionDays} {
		t.Run(fmt.Sprintf("%d天", days), func(t *testing.T) {
			store := &fakeCleanupStore{}
			svc := newCleanupForTest(t, store, &fakeCleanupObjects{}, enabledSettings(days), 0)

			report, err := svc.RunOnce(context.Background())
			require.NoError(t, err)
			assert.True(t, report.Ran)

			wantCutoff := cleanupTestNow.Add(-time.Duration(days) * 24 * time.Hour)
			require.Len(t, store.imageCalls, 1)
			require.Len(t, store.assetCalls, 1)
			assert.True(t, store.imageCalls[0].cutoff.Equal(wantCutoff),
				"结果图的 cutoff 该是 %v，实际 %v", wantCutoff, store.imageCalls[0].cutoff)
			assert.True(t, store.assetCalls[0].cutoff.Equal(wantCutoff),
				"素材的 cutoff 必须跟结果图同一条判据")
			// 没显式配 BatchLimit 时按默认 500 分批。
			assert.Equal(t, DefaultCleanupBatchLimit, store.imageCalls[0].limit)
		})
	}
}

// 越界 fail-closed：整轮不跑。收敛成下限 = 按比管理员填的更激进的口径删文件。
func TestCleanupRetentionDaysOutOfRangeSkipsTheRun(t *testing.T) {
	for name, raw := range map[string]string{
		"低于下限": fmt.Sprintf("%d", dkdomain.MinCleanupRetentionDays-1),
		"高于上限": fmt.Sprintf("%d", dkdomain.MaxCleanupRetentionDays+1),
		"不是整数": `"abc"`,
	} {
		t.Run(name, func(t *testing.T) {
			store := &fakeCleanupStore{
				imageBatches: [][]dkdomain.CleanupCandidate{{{ID: 1, ObjectKey: "k1"}}},
			}
			objects := &fakeCleanupObjects{}
			svc := newCleanupForTest(t, store, objects, map[string]string{
				dkdomain.SettingKeyCleanupEnabled:       "true",
				dkdomain.SettingKeyCleanupRetentionDays: raw,
			}, 0)

			report, err := svc.RunOnce(context.Background())
			require.NoError(t, err)
			assert.False(t, report.Ran)
			assert.Empty(t, store.imageCalls)
			assert.Empty(t, objects.deleted)
		})
	}
}

// 天数没配过（只开了开关）：用默认 180 天，照常跑。
func TestCleanupRetentionDaysDefaultsTo180(t *testing.T) {
	store := &fakeCleanupStore{}
	svc := newCleanupForTest(t, store, &fakeCleanupObjects{}, map[string]string{
		dkdomain.SettingKeyCleanupEnabled: "true",
	}, 0)

	report, err := svc.RunOnce(context.Background())
	require.NoError(t, err)
	assert.True(t, report.Ran)
	wantCutoff := cleanupTestNow.Add(-time.Duration(dkdomain.DefaultCleanupRetentionDays) * 24 * time.Hour)
	require.Len(t, store.imageCalls, 1)
	assert.True(t, store.imageCalls[0].cutoff.Equal(wantCutoff))
}

// ----------------------------------------------------------------------------
// 3. 分批上限
// ----------------------------------------------------------------------------

// 一批最多 BatchLimit 条；批满了接着捞，捞到不满一批为止。
func TestCleanupProcessesInBatches(t *testing.T) {
	store := &fakeCleanupStore{
		imageBatches: [][]dkdomain.CleanupCandidate{
			{{ID: 1, ObjectKey: "img/1.png", ByteSize: 10}, {ID: 2, ObjectKey: "img/2.png", ByteSize: 20}},
			{{ID: 3, ObjectKey: "img/3.png", ByteSize: 30}, {ID: 4, ObjectKey: "img/4.png", ByteSize: 40}},
			{{ID: 5, ObjectKey: "img/5.png", ByteSize: 50}},
		},
	}
	objects := &fakeCleanupObjects{}
	svc := newCleanupForTest(t, store, objects, enabledSettings(180), 2)

	report, err := svc.RunOnce(context.Background())
	require.NoError(t, err)

	// 2+2+1：第三批不满，停。每次都按上限 2 去要。
	require.Len(t, store.imageCalls, 3)
	for _, call := range store.imageCalls {
		assert.Equal(t, 2, call.limit)
	}
	assert.Equal(t, 5, report.ImagesDeleted)
	assert.Equal(t, int64(150), report.BytesFreed)
	assert.Len(t, objects.deleted, 5)
	// 软删也是一批一批做的（一批一个短事务，不锁表）。
	assert.Equal(t, [][]int64{{1, 2}, {3, 4}, {5}}, store.softDeletedImages)
}

// 整批文件全删失败时必须停手，不许死循环
// （失败的行没被软删，下一轮还会被筛出来 —— 不停就永远转下去）。
func TestCleanupStopsWhenAWholeBatchFailsToDelete(t *testing.T) {
	store := &fakeCleanupStore{
		staticImages: []dkdomain.CleanupCandidate{
			{ID: 1, ObjectKey: "img/broken.png", ByteSize: 10},
		},
	}
	objects := &fakeCleanupObjects{failKeys: map[string]bool{"img/broken.png": true}}
	svc := newCleanupForTest(t, store, objects, enabledSettings(180), 1)

	report, err := svc.RunOnce(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 0, report.ImagesDeleted)
	assert.Equal(t, 1, report.FileErrors)
	// 关键断言：只筛了一轮就停了（deleted==0 → break），没有第二次。
	assert.Len(t, store.imageCalls, 1)
}

// ----------------------------------------------------------------------------
// 4. 文件删失败的行不软删
// ----------------------------------------------------------------------------

// 顺序是「先删文件、后软删记录」：删失败的那行留在库里，下一轮重试。
func TestCleanupKeepsRecordWhenFileDeleteFails(t *testing.T) {
	store := &fakeCleanupStore{
		imageBatches: [][]dkdomain.CleanupCandidate{{
			{ID: 1, ObjectKey: "img/ok.png", ByteSize: 10},
			{ID: 2, ObjectKey: "img/broken.png", ByteSize: 20},
			{ID: 3, ObjectKey: "img/ok2.png", ByteSize: 30},
		}},
	}
	objects := &fakeCleanupObjects{failKeys: map[string]bool{"img/broken.png": true}}
	svc := newCleanupForTest(t, store, objects, enabledSettings(180), 0)

	report, err := svc.RunOnce(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 2, report.ImagesDeleted)
	assert.Equal(t, 1, report.FileErrors)
	// 失败那行（ID=2）绝不能进软删名单，字节数也不计。
	assert.Equal(t, []int64{1, 3}, store.allSoftDeletedImageIDs())
	assert.Equal(t, int64(40), report.BytesFreed)
}

// ----------------------------------------------------------------------------
// 5. 素材连着预处理产物一起删
// ----------------------------------------------------------------------------

func TestCleanupDeletesAssetVariantsWithTheAsset(t *testing.T) {
	store := &fakeCleanupStore{
		assetBatches: [][]dkdomain.CleanupCandidate{{
			{ID: 7, ObjectKey: "assets/a.png", ByteSize: 100},
		}},
		variantKeys: map[int64][]string{
			7: {"variants/a-1x1.png", "variants/a-3x4.png"},
		},
	}
	objects := &fakeCleanupObjects{}
	svc := newCleanupForTest(t, store, objects, enabledSettings(180), 0)

	report, err := svc.RunOnce(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 1, report.AssetsDeleted)
	// 产物在前、原图在后：产物删干净了才动原图。
	assert.Equal(t, []string{"variants/a-1x1.png", "variants/a-3x4.png", "assets/a.png"}, objects.deleted)
	assert.Equal(t, [][]int64{{7}}, store.softDeletedAssets)
}

// 产物文件删失败：整条素材跳过（原图不动、不软删），下一轮重来。
// 软删了却留着产物文件，就是没人管的孤儿文件。
func TestCleanupSkipsAssetWhenVariantDeleteFails(t *testing.T) {
	store := &fakeCleanupStore{
		assetBatches: [][]dkdomain.CleanupCandidate{{
			{ID: 7, ObjectKey: "assets/a.png", ByteSize: 100},
		}},
		variantKeys: map[int64][]string{7: {"variants/broken.png"}},
	}
	objects := &fakeCleanupObjects{failKeys: map[string]bool{"variants/broken.png": true}}
	svc := newCleanupForTest(t, store, objects, enabledSettings(180), 0)

	report, err := svc.RunOnce(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 0, report.AssetsDeleted)
	assert.Equal(t, 1, report.FileErrors)
	// 原图必须原封不动 —— 删了原图、软删没做，界面上这张图就成了打不开的死链。
	assert.Empty(t, objects.deleted)
	// 这一批的软删名单必须是空的（fake 会记下每次调用，包括空名单）。
	require.Len(t, store.softDeletedAssets, 1)
	assert.Empty(t, store.softDeletedAssets[0])
}
