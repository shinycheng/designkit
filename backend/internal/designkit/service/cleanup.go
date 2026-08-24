package service

// ============================================================================
// 图片自动清理（决策 17：「过期字段先建好，哪天硬盘紧了开开关就能清」——这就是那个开关）
// ============================================================================
//
// 做的事：开关开着时，每天一次（外加启动后补跑一次）把「超过保留天数」的
// 结果图和素材删掉 —— 对象存储里的文件真删，数据库记录标记删除（软删）。
// 账目一个都不动：批次、扣费、消费统计全部保留，删的只是图片本身。
//
// 四条必须守住的规矩，改这个文件之前先看一遍：
//
// 【1】**开关关着就什么都不做。** 开关和保留天数每一轮都从 designkit_settings
//     现读（不是启动时读一次），管理员改完设置不用重启。
//
// 【2】**先删文件、后软删记录，顺序不能反。** 反过来的话，文件删失败的那些
//     行已经被标记删除，下一轮再也筛不出来，文件永远躺在磁盘上没人管。
//     现在的顺序下最坏情况是「文件删了、软删没写上」——下一轮重筛出来，
//     Delete 对不存在的 key 返回 nil（幂等），补上软删就完了。
//
// 【3】**被未结束批次引用的素材不删**（repository 的 NOT EXISTS 负责）。
//     批次在跑时 worker 随时会按 object_key 读原图做预处理，删了它读空。
//
// 【4】**分批干，每批默认 500 条**，一批一个短事务，绝不把上万行的 UPDATE
//     压成一条长语句锁表。批与批之间检查 ctx，进程要退出时立刻停手。
//
// 【保留天数超范围就整轮不跑（fail-closed）】设置里存了 30 以下或 3650 以上的
// 值时，不收敛、不猜，直接记一行警告并跳过 —— 收敛成 30 意味着「按比管理员
// 填的更激进的口径删文件」，删错的图找不回来，宁可不删。
//
// 【单实例约束】跟出图队列（module.go 的 Locker=nil）同一口径：没有跨实例锁，
// 整套服务只能起一个副本。两个副本同时清理不会删错东西（文件删除幂等、
// 软删带 deleted_at IS NULL 守卫），但哪天要上多副本，该跟队列一起补
// advisory lock，别单独指望这里的幂等性。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	dkdomain "github.com/Wei-Shaw/sub2api/internal/designkit/domain"
)

const (
	// DefaultCleanupInterval 清理间隔：每天跑一次。
	// 保留天数以「天」为粒度，跑得更勤没有收益。
	DefaultCleanupInterval = 24 * time.Hour

	// cleanupStartupDelay 进程起来后隔多久补跑第一次。
	// 跟灵感库同步一样不立刻跑：启动那几十秒机器最忙。
	cleanupStartupDelay = 90 * time.Second

	// cleanupRunTimeout 单轮清理的总超时。正常几秒；给到 30 分钟是兜住
	// 「第一次开启时积了几万张」的情况。
	cleanupRunTimeout = 30 * time.Minute

	// DefaultCleanupBatchLimit 一批最多处理多少条（结果图和素材各自按批）。
	// 500 跟灵感库同步的入库分批同一个量级：一批一个短事务，不锁表。
	DefaultCleanupBatchLimit = 500

	// cleanupCloseBudget 进程退出时等在途那一轮收尾的时间。
	cleanupCloseBudget = 3 * time.Second
)

// cleanupStore 清理要用到的持久化能力。repository.CleanupRepo 实现了它。
// 声明成窄接口：单测塞假实现就行，不用拖真数据库。
type cleanupStore interface {
	ListExpiredImages(ctx context.Context, cutoff time.Time, limit int) ([]dkdomain.CleanupCandidate, error)
	ListExpiredAssets(ctx context.Context, cutoff time.Time, limit int) ([]dkdomain.CleanupCandidate, error)
	ListVariantKeysByAsset(ctx context.Context, assetIDs []int64) (map[int64][]string, error)
	SoftDeleteImages(ctx context.Context, ids []int64) (int64, error)
	SoftDeleteAssets(ctx context.Context, ids []int64) (int64, error)
}

// cleanupSettingStore 读 designkit_settings。**只读**：开关和天数由设置页写。
type cleanupSettingStore interface {
	GetSetting(ctx context.Context, key string) (*dkdomain.Setting, error)
}

// CleanupServiceDeps 建清理服务要的依赖。
type CleanupServiceDeps struct {
	// Store 持久化（筛过期行 + 软删），必填。
	Store cleanupStore
	// Objects 对象存储（删文件），必填。
	Objects dkdomain.ObjectStore
	// Settings 配置仓储（读开关和保留天数），必填。
	Settings cleanupSettingStore

	// Interval 清理间隔。<=0 用 DefaultCleanupInterval。
	Interval time.Duration
	// StartupDelay 启动后多久补跑第一次。<=0 用 cleanupStartupDelay。
	StartupDelay time.Duration
	// BatchLimit 一批最多处理多少条。<=0 用 DefaultCleanupBatchLimit。
	BatchLimit int
	// Now 取当前时间，测试塞固定值。为 nil 用 time.Now。
	Now func() time.Time
}

// CleanupReport 一轮清理的结果，日志和测试都看它。
type CleanupReport struct {
	// Ran 这一轮真的动手了吗（开关关着 / 天数不合法时为 false）。
	Ran bool
	// SkipReason 没动手的原因（中文），Ran=true 时为空。
	SkipReason string
	// ImagesDeleted 删掉的结果图条数。
	ImagesDeleted int
	// AssetsDeleted 删掉的素材条数。
	AssetsDeleted int
	// BytesFreed 释放的字节数（按记录里的 byte_size 累计；
	// 预处理产物没有字节数列，不计入，所以对外说「约」）。
	BytesFreed int64
	// FileErrors 文件删除失败的条数。这些行**没有**被软删，下一轮重试。
	FileErrors int
}

// CleanupService 图片自动清理。生命周期跟 InspirationSyncService 同一套：
// Start 起定时器、Close 停掉，定时器活到 Close 为止。
type CleanupService struct {
	store    cleanupStore
	objects  dkdomain.ObjectStore
	settings cleanupSettingStore

	interval     time.Duration
	startupDelay time.Duration
	batchLimit   int
	now          func() time.Time

	mu      sync.Mutex
	baseCtx context.Context
	cancel  context.CancelFunc
	started bool
	closed  bool
	wg      sync.WaitGroup
}

// NewCleanupService 建清理服务。缺必填依赖直接报错，不留到运行时 nil panic。
func NewCleanupService(deps CleanupServiceDeps) (*CleanupService, error) {
	if deps.Store == nil {
		return nil, errors.New("designkit: 图片自动清理缺少持久化层")
	}
	if deps.Objects == nil {
		return nil, errors.New("designkit: 图片自动清理缺少对象存储")
	}
	if deps.Settings == nil {
		return nil, errors.New("designkit: 图片自动清理缺少配置仓储")
	}

	interval := deps.Interval
	if interval <= 0 {
		interval = DefaultCleanupInterval
	}
	startupDelay := deps.StartupDelay
	if startupDelay <= 0 {
		startupDelay = cleanupStartupDelay
	}
	batchLimit := deps.BatchLimit
	if batchLimit <= 0 {
		batchLimit = DefaultCleanupBatchLimit
	}
	now := deps.Now
	if now == nil {
		now = time.Now
	}

	return &CleanupService{
		store:        deps.Store,
		objects:      deps.Objects,
		settings:     deps.Settings,
		interval:     interval,
		startupDelay: startupDelay,
		batchLimit:   batchLimit,
		now:          now,
	}, nil
}

// ----------------------------------------------------------------------------
// 一轮清理
// ----------------------------------------------------------------------------

// RunOnce 跑一轮清理。开关关着 / 天数不合法时什么都不动（Ran=false）。
// 导出是给测试和以后可能出现的「立即清理」入口用的；定时器内部也走它。
func (s *CleanupService) RunOnce(ctx context.Context) (*CleanupReport, error) {
	report := &CleanupReport{}

	enabled, err := s.readEnabled(ctx)
	if err != nil {
		return nil, err
	}
	if !enabled {
		report.SkipReason = "开关关着"
		return report, nil
	}

	days, ok, err := s.readRetentionDays(ctx)
	if err != nil {
		return nil, err
	}
	if !ok {
		// fail-closed：天数不合法宁可不删（见文件头）。原因已在 read 里记过日志。
		report.SkipReason = "保留天数不在允许范围内，这一轮没有清理"
		return report, nil
	}

	report.Ran = true
	cutoff := s.now().Add(-time.Duration(days) * 24 * time.Hour)

	if err := s.sweepImages(ctx, cutoff, report); err != nil {
		return report, err
	}
	if err := s.sweepAssets(ctx, cutoff, report); err != nil {
		return report, err
	}
	return report, nil
}

// sweepImages 分批清结果图：删文件 → 软删记录，直到一批不满或没有进展。
func (s *CleanupService) sweepImages(ctx context.Context, cutoff time.Time, report *CleanupReport) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		batch, err := s.store.ListExpiredImages(ctx, cutoff, s.batchLimit)
		if err != nil {
			return err
		}
		if len(batch) == 0 {
			return nil
		}

		ids := make([]int64, 0, len(batch))
		var bytes int64
		for _, c := range batch {
			// 先删文件。删失败的**不软删**：下一轮还能筛出来重试；
			// Delete 对不存在的 key 返回 nil，重复处理无害。
			if err := s.objects.Delete(ctx, c.ObjectKey); err != nil {
				report.FileErrors++
				slog.Warn("designkit 图片自动清理：结果图文件删除失败，这一条留到下一轮",
					slog.String("object_key", c.ObjectKey), slog.Any("error", err))
				continue
			}
			ids = append(ids, c.ID)
			bytes += c.ByteSize
		}

		deleted, err := s.store.SoftDeleteImages(ctx, ids)
		if err != nil {
			return err
		}
		report.ImagesDeleted += int(deleted)
		report.BytesFreed += bytes

		// 一批不满 = 筛完了。整批文件都删失败（deleted=0）也停：
		// 失败的行还留在筛选结果里，不停就是死循环。
		if len(batch) < s.batchLimit || deleted == 0 {
			return nil
		}
	}
}

// sweepAssets 分批清素材。素材连着它的预处理产物文件一起删
// （产物行保留，理由见 repository/cleanup_repo.go 的 listVariantKeysSQL 注释）。
func (s *CleanupService) sweepAssets(ctx context.Context, cutoff time.Time, report *CleanupReport) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		batch, err := s.store.ListExpiredAssets(ctx, cutoff, s.batchLimit)
		if err != nil {
			return err
		}
		if len(batch) == 0 {
			return nil
		}

		assetIDs := make([]int64, 0, len(batch))
		for _, c := range batch {
			assetIDs = append(assetIDs, c.ID)
		}
		variantKeys, err := s.store.ListVariantKeysByAsset(ctx, assetIDs)
		if err != nil {
			return err
		}

		ids := make([]int64, 0, len(batch))
		var bytes int64
		for _, c := range batch {
			// 原图 + 名下全部产物文件都删干净才软删。任何一个删失败就整条跳过，
			// 让下一轮重来 —— 软删了却留着产物文件，就成了没人管的孤儿文件。
			if !s.deleteAssetFiles(ctx, c, variantKeys[c.ID], report) {
				continue
			}
			ids = append(ids, c.ID)
			bytes += c.ByteSize
		}

		deleted, err := s.store.SoftDeleteAssets(ctx, ids)
		if err != nil {
			return err
		}
		report.AssetsDeleted += int(deleted)
		report.BytesFreed += bytes

		if len(batch) < s.batchLimit || deleted == 0 {
			return nil
		}
	}
}

// deleteAssetFiles 删一个素材的原图和全部产物文件。全删成功才返回 true。
func (s *CleanupService) deleteAssetFiles(ctx context.Context, c dkdomain.CleanupCandidate, variantKeys []string, report *CleanupReport) bool {
	keys := make([]string, 0, len(variantKeys)+1)
	keys = append(keys, variantKeys...)
	keys = append(keys, c.ObjectKey) // 原图放最后：产物删干净了才动原图
	for _, key := range keys {
		if err := s.objects.Delete(ctx, key); err != nil {
			report.FileErrors++
			slog.Warn("designkit 图片自动清理：素材文件删除失败，这一条留到下一轮",
				slog.String("object_key", key), slog.Any("error", err))
			return false
		}
	}
	return true
}

// ----------------------------------------------------------------------------
// 读配置（每一轮现读，改完设置不用重启）
// ----------------------------------------------------------------------------

// readEnabled 读开关。没配过 / 读不出来一律当**关**——清理是删东西的动作，
// 拿不准就不动。
func (s *CleanupService) readEnabled(ctx context.Context) (bool, error) {
	setting, err := s.settings.GetSetting(ctx, dkdomain.SettingKeyCleanupEnabled)
	if err != nil {
		if errors.Is(err, dkdomain.ErrNotFound) {
			return false, nil
		}
		return false, err
	}
	if setting == nil || len(setting.Value) == 0 {
		return false, nil
	}
	var enabled bool
	if err := json.Unmarshal(setting.Value, &enabled); err != nil {
		slog.Warn("designkit 图片自动清理：开关的值不是布尔，按关闭处理",
			slog.String("value", string(setting.Value)))
		return false, nil
	}
	return enabled, nil
}

// readRetentionDays 读保留天数。没配过用默认 180；
// 存了范围外或不是整数的值返回 ok=false（fail-closed，整轮不跑）。
func (s *CleanupService) readRetentionDays(ctx context.Context) (days int, ok bool, err error) {
	setting, err := s.settings.GetSetting(ctx, dkdomain.SettingKeyCleanupRetentionDays)
	if err != nil {
		if errors.Is(err, dkdomain.ErrNotFound) {
			return dkdomain.DefaultCleanupRetentionDays, true, nil
		}
		return 0, false, err
	}
	if setting == nil || len(setting.Value) == 0 {
		return dkdomain.DefaultCleanupRetentionDays, true, nil
	}
	var v int
	if err := json.Unmarshal(setting.Value, &v); err != nil {
		slog.Warn("designkit 图片自动清理：保留天数不是整数，这一轮不清理",
			slog.String("value", string(setting.Value)))
		return 0, false, nil
	}
	if v < dkdomain.MinCleanupRetentionDays || v > dkdomain.MaxCleanupRetentionDays {
		// 不收敛：收敛成下限等于按更激进的口径删文件，删错找不回来。
		slog.Warn("designkit 图片自动清理：保留天数超出允许范围，这一轮不清理",
			slog.Int("configured", v),
			slog.Int("min", dkdomain.MinCleanupRetentionDays),
			slog.Int("max", dkdomain.MaxCleanupRetentionDays))
		return 0, false, nil
	}
	return v, true, nil
}

// ----------------------------------------------------------------------------
// 定时器（形态照 InspirationSyncService：Start 起、Close 停）
// ----------------------------------------------------------------------------

// Start 启动清理定时器：启动后补跑一次 + 每天一次。
//
// 传进来的 ctx 只用于启动阶段，**不决定定时器的生命周期**（装配时手上的 ctx
// 往往是请求级的，拿它当生命周期，定时器会在第一次请求结束时就停）。
// 可以重复调用，第二次起是空操作。
func (s *CleanupService) Start(_ context.Context) error {
	s.mu.Lock()
	if s.started || s.closed {
		s.mu.Unlock()
		return nil
	}
	s.started = true
	ctx, cancel := context.WithCancel(context.Background())
	s.baseCtx = ctx
	s.cancel = cancel
	s.mu.Unlock()

	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		s.loop(ctx)
	}()
	slog.Info("designkit 图片自动清理定时器已启动（开关关着时每轮空跑，不动任何数据）",
		slog.Duration("interval", s.interval))
	return nil
}

func (s *CleanupService) loop(ctx context.Context) {
	// 启动跑一次：管理员开着开关重启服务，不用等 24 小时才见效。
	first := time.NewTimer(s.startupDelay)
	select {
	case <-ctx.Done():
		first.Stop()
		return
	case <-first.C:
		s.runScheduled(ctx)
	}

	ticker := time.NewTicker(s.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.runScheduled(ctx)
		}
	}
}

// runScheduled 跑一轮并把结果写进日志。错误只记日志不上抛（调用方是定时器）。
func (s *CleanupService) runScheduled(parent context.Context) {
	ctx, cancel := context.WithTimeout(parent, cleanupRunTimeout)
	defer cancel()

	report, err := s.RunOnce(ctx)
	if err != nil {
		slog.Error("designkit 图片自动清理这一轮没跑完，已删掉的不会复活，剩下的下一轮接着清",
			slog.Any("error", err))
		return
	}
	if !report.Ran {
		// 开关关着是常态（默认就是关的），不刷日志。
		return
	}
	// 完成日志：一行中文说清删了多少、释放约多少（决策 17 的验收点）。
	slog.Info(fmt.Sprintf("designkit 图片自动清理完成：删除结果图 %d 张、素材 %d 个，释放约 %s",
		report.ImagesDeleted, report.AssetsDeleted, formatBytes(report.BytesFreed)),
		slog.Int("file_errors", report.FileErrors))
}

// formatBytes 把字节数写成人看的（1.2 GB / 34.5 MB / 210 KB / 512 B）。
func formatBytes(n int64) string {
	const unit = 1024
	switch {
	case n >= unit*unit*unit:
		return fmt.Sprintf("%.1f GB", float64(n)/(unit*unit*unit))
	case n >= unit*unit:
		return fmt.Sprintf("%.1f MB", float64(n)/(unit*unit))
	case n >= unit:
		return fmt.Sprintf("%.1f KB", float64(n)/unit)
	default:
		return fmt.Sprintf("%d B", n)
	}
}

// Close 停定时器，给在途那一轮一点时间收尾。可以重复调用。
func (s *CleanupService) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	cancel := s.cancel
	s.mu.Unlock()

	if cancel != nil {
		cancel()
	}

	done := make(chan struct{})
	go func() {
		s.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(cleanupCloseBudget):
		slog.Warn("designkit 图片自动清理没能在窗口内收尾，剩下的下一次启动接着清")
	}
	return nil
}
