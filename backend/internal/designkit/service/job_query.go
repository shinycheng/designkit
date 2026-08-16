package service

import (
	"context"
	"errors"
	"log/slog"
	"sort"

	dkdomain "github.com/Wei-Shaw/sub2api/internal/designkit/domain"
)

// ============================================================================
// 查询（列表 / 详情 / 每一张 / 取图片字节）
// ============================================================================
//
// 两条铁律：
//
//  1. **归属校验一律走「查不到」而不是 403。** 403 等于告诉对方
//     「这个任务号是存在的，只是不是你的」。仓储的 WHERE 里已经带了 user_id，
//     本层只负责把 ErrNotFound 翻成中文。
//
//  2. **item 一律按 seq 升序。** 仓储的 SQL 里已经写死 ORDER BY seq ASC，
//     这里再排一次是第二道保险 —— 这条契约破了不会报错，
//     只会让 ERP 把图配错商品。handler 还会再排第三次。
//
// 「管理员可以看全部」这件事**本层做不到**：JobService 的方法签名只收一个
// userID（handler/ports.go 定的），没有身份角色。要做得改接口，见交付说明。

// GetJob 按对外任务号取批次，校验归属。
func (s *JobService) GetJob(ctx context.Context, userID int64, jobUID string) (*dkdomain.Job, error) {
	job, err := s.mustGetJob(ctx, userID, jobUID)
	if err != nil {
		return nil, err
	}
	return job, nil
}

// mustGetJob 取批次并保证非 nil。
func (s *JobService) mustGetJob(ctx context.Context, userID int64, jobUID string) (*dkdomain.Job, error) {
	if userID <= 0 {
		return nil, dkdomain.NewError(dkdomain.ErrCodeUnauthorized)
	}
	job, err := s.repo.GetJobByUID(ctx, userID, jobUID)
	if err != nil {
		return nil, mapJobRepoError(err, dkdomain.ErrCodeJobNotFound)
	}
	if job == nil {
		return nil, dkdomain.NewError(dkdomain.ErrCodeJobNotFound)
	}
	return job, nil
}

// ListJobs 批次列表，**游标分页**（created_at + id 复合游标，不用 offset）。
//
// offset 在持续写入时会漏数据：运营一边翻页一边有新任务插到最前面，
// 第二页会把第一页看过的行再给一遍，同时把真正的下一批挤掉；
// ERP 拉历史时这种漏数据是静默的，等发现就已经少同步了一批图。
func (s *JobService) ListJobs(ctx context.Context, query dkdomain.ListJobsQuery) (*dkdomain.JobPage, error) {
	if query.UserID <= 0 {
		return nil, dkdomain.NewError(dkdomain.ErrCodeUnauthorized)
	}
	if query.Limit <= 0 {
		query.Limit = jobDefaultListLimit
	}
	if query.Limit > jobMaxListLimit {
		query.Limit = jobMaxListLimit
	}
	page, err := s.repo.ListJobs(ctx, query)
	if err != nil {
		return nil, mapJobRepoError(err, dkdomain.ErrCodeJobNotFound)
	}
	if page == nil {
		page = &dkdomain.JobPage{}
	}
	return page, nil
}

// ListJobItems 取一个批次的全部 item，**按 seq 升序**，每一张带上当前版本的结果图。
func (s *JobService) ListJobItems(ctx context.Context, userID int64, jobUID string) ([]*JobItemView, error) {
	job, err := s.mustGetJob(ctx, userID, jobUID)
	if err != nil {
		return nil, err
	}

	items, err := s.repo.ListJobItems(ctx, job.ID)
	if err != nil {
		return nil, mapJobRepoError(err, dkdomain.ErrCodeItemNotFound)
	}

	views := make([]*JobItemView, 0, len(items))
	for _, item := range items {
		if item == nil {
			continue
		}
		view, err := s.viewOf(ctx, item)
		if err != nil {
			return nil, err
		}
		views = append(views, view)
	}
	sortItemViews(views)
	return views, nil
}

// GetJobItem 按序号取某一张。seq 从 1 开始。
func (s *JobService) GetJobItem(ctx context.Context, userID int64, jobUID string, seq int) (*JobItemView, error) {
	job, err := s.mustGetJob(ctx, userID, jobUID)
	if err != nil {
		return nil, err
	}
	if seq <= 0 {
		// seq 从 1 开始是对外契约的一部分，0 和负数一律当「没有这一张」。
		return nil, dkdomain.NewError(dkdomain.ErrCodeItemNotFound)
	}
	item, err := s.repo.GetJobItemBySeq(ctx, job.ID, seq)
	if err != nil {
		return nil, mapJobRepoError(err, dkdomain.ErrCodeItemNotFound)
	}
	if item == nil {
		return nil, dkdomain.NewError(dkdomain.ErrCodeItemNotFound)
	}
	return s.viewOf(ctx, item)
}

// OpenJobItemContent 取某一张当前版本的结果图字节。imageIndex 从 1 开始。
func (s *JobService) OpenJobItemContent(
	ctx context.Context,
	userID int64,
	jobUID string,
	seq, imageIndex int,
) ([]byte, string, error) {
	view, err := s.GetJobItem(ctx, userID, jobUID, seq)
	if err != nil {
		return nil, "", err
	}
	if imageIndex <= 0 {
		imageIndex = 1
	}

	var target *dkdomain.Image
	for _, img := range view.Images {
		if img != nil && img.ImageIndex == imageIndex {
			target = img
			break
		}
	}
	if target == nil {
		// 分开说：还没出图 vs 出了但没有这一张。运营看到的提示不一样。
		if view.Item != nil && !view.Item.Status.IsTerminal() {
			return nil, "", dkdomain.NewError(dkdomain.ErrCodeImageNotFound).
				WithMessage("这一张还没出好，请等它跑完再看。")
		}
		return nil, "", dkdomain.NewError(dkdomain.ErrCodeImageNotFound)
	}
	if s.store == nil {
		return nil, "", dkdomain.NewError(dkdomain.ErrCodeStorageError).
			WithMessage("图片存储没有配置好，暂时取不到图，请联系管理员。")
	}

	data, contentType, err := s.store.Get(ctx, target.ObjectKey)
	if err != nil {
		// 库里有记录、存储里没文件**不是「找不到」**，是存储出了问题。
		// 报成 404 会让运营以为图被自己删了，实际要查的是磁盘。
		return nil, "", dkdomain.NewError(dkdomain.ErrCodeStorageError).WithCause(err)
	}
	if contentType == "" {
		contentType = target.ContentType
	}
	return data, contentType, nil
}

// viewOf 给一张 item 配上它当前版本的结果图（按 image_index 升序）。
func (s *JobService) viewOf(ctx context.Context, item *dkdomain.JobItem) (*JobItemView, error) {
	// currentOnly=true：重试出了新图之后旧图 is_current=false，界面默认只给当前这版。
	images, err := s.repo.ListImagesByItem(ctx, item.ID, true)
	if err != nil {
		return nil, mapJobRepoError(err, dkdomain.ErrCodeImageNotFound)
	}
	sortImagesByIndex(images)
	return &JobItemView{Item: item, Images: images}, nil
}

// sortItemViews 按 seq 升序（第二道保险，仓储的 SQL 已经 ORDER BY seq ASC）。
func sortItemViews(views []*JobItemView) {
	sort.SliceStable(views, func(i, j int) bool {
		if views[i] == nil || views[i].Item == nil {
			return false
		}
		if views[j] == nil || views[j].Item == nil {
			return true
		}
		return views[i].Item.Seq < views[j].Item.Seq
	})
}

// sortImagesByIndex 按 image_index 升序。
func sortImagesByIndex(images []*dkdomain.Image) {
	sort.SliceStable(images, func(i, j int) bool {
		if images[i] == nil {
			return false
		}
		if images[j] == nil {
			return true
		}
		return images[i].ImageIndex < images[j].ImageIndex
	})
}

// ============================================================================
// 停止排队（决策 21 / 设计定型 6.1）
// ============================================================================

// StopJobResult 是一次「停止排队」的结果。
type StopJobResult struct {
	// Job 停完之后的批次。
	Job *dkdomain.Job
	// Cancelled 这次砍掉了几张（只统计「还没开始就被停掉的」）。
	Cancelled int
	// StillRunning 还有几张在途，必须等它们自然落地。界面要把这个数说出来。
	StillRunning int
}

// StopJob 「停止排队」。**只做两件事**：把 pending 的 item 转 cancelled、
// 给 job 打 cancel_requested_at。
//
// ⚠ **已经 running 的那几张必须等自然落地**，job 停在 running 直到在途清空，
// 然后走跟正常完成**完全相同**的结算路径，终态才落 cancelled、actual_cost 照实填。
// 状态机里刻意没有 running → cancelled：直接跳过去就等于跳过结算 ——
// 在途的图出来了、上游扣了钱，而我们的 actual_cost 永远是空的，账对不上；
// ERP 看到 cancelled 还会以为什么都没发生，直接重新下单，**再花一遍钱**。
//
// 按钮上写「停止排队」不写「取消」，说的就是这件事。
func (s *JobService) StopJob(ctx context.Context, userID int64, jobUID string) (*StopJobResult, error) {
	job, err := s.mustGetJob(ctx, userID, jobUID)
	if err != nil {
		return nil, err
	}
	if job.Status.IsTerminal() || job.IsSettled() {
		return nil, dkdomain.NewError(dkdomain.ErrCodeIllegalStateTransition).
			WithMessage("这批任务已经结束了，不用再停。")
	}

	cancelled, err := s.repo.RequestJobStop(ctx, job.ID)
	if err != nil {
		return nil, mapJobRepoError(err, dkdomain.ErrCodeJobNotFound)
	}

	// 重新读一份：cancelled_count 和 cancel_requested_at 都变了。
	fresh, err := s.mustGetJob(ctx, userID, jobUID)
	if err != nil {
		return nil, err
	}

	stillRunning := fresh.ItemCount - fresh.SuccessCount - fresh.FailCount - fresh.CancelledCount
	if stillRunning < 0 {
		stillRunning = 0
	}

	// 一张在途的都没有了（全砍光、或者刚好都跑完了）：
	// 这时候**没有任何 worker 会再回来收尾** —— 收尾是「写回一张图的结果」之后
	// 才做的事。不在这里推一把，这批任务会一直挂在 running 上，
	// 冻结额一直占着用户的可用额，直到 10 分钟后被僵尸巡检当成崩溃残留判死
	// （运营看到的是「我点了停止，然后它变成失败了」）。
	//
	// 推的是 settling 不是终态：真正的终态和 actual_cost 由结算 worker 写
	// （上游写 usage_logs 是异步的，立刻汇总会少算甚至全算 0）。
	if stillRunning == 0 && fresh.ItemCount > 0 {
		s.finalizeAfterStop(ctx, fresh)
	}

	return &StopJobResult{Job: fresh, Cancelled: cancelled, StillRunning: stillRunning}, nil
}

// finalizeAfterStop 尽力而为地把「已经没有在途 item」的批次推进结算。
// 失败只记日志：僵尸巡检兜底，不该让运营的「停止」按钮报错。
func (s *JobService) finalizeAfterStop(ctx context.Context, job *dkdomain.Job) {
	status := dkdomain.TerminalJobStatus(
		job.ItemCount, job.SuccessCount, job.FailCount, job.CancelledCount, job.IsStopRequested())

	won, err := s.repo.FinalizeJob(ctx, dkdomain.FinalizeJobParams{
		JobID:      job.ID,
		Status:     status,
		FinishedAt: s.now().UTC(),
	})
	if err != nil {
		s.logger().Warn("designkit 停止排队之后没能把批次推进结算，等僵尸巡检兜底",
			slog.String("job_uid", job.UID), slog.Any("error", err))
		return
	}
	s.logger().Info("designkit 停止排队后批次已无在途任务",
		slog.String("job_uid", job.UID),
		slog.String("terminal_status", status.String()),
		slog.Bool("finalize_won", won))
}

// ============================================================================
// 删除一批记录（软删）
// ============================================================================

// DeleteJob 把一个批次从「我的图片」里删掉（软删：user_deleted_at 打上时间戳）。
//
// 删的只是**记录的可见性**：数据行、结果图文件、账单全都还在，
// 只是列表和详情不再返回它（仓储的查询都带 user_deleted_at IS NULL）。
// 已扣的费用不退 —— 界面确认弹窗必须把这句写出来。
//
// **没结束的批次不让删。** 不是技术做不到，而是刻意的：
// 出图跟浏览器连接是故意脱钩的（决策 21），删掉一个还在跑的批次，
// 图照样出、钱照样扣，而运营连「停止排队」的按钮都找不到了 ——
// 这就是决策 21 要防的「取消了还扣我钱」换了一件马甲。
// 先停、等它结束、再删，每一步都看得见。
func (s *JobService) DeleteJob(ctx context.Context, userID int64, jobUID string) error {
	job, err := s.mustGetJob(ctx, userID, jobUID)
	if err != nil {
		return err
	}
	if !job.Status.IsTerminal() && !job.IsSettled() {
		return dkdomain.NewError(dkdomain.ErrCodeIllegalStateTransition).
			WithMessage("这批任务还没结束，不能删除。先点「停止排队」，等它结束再删。")
	}
	if err := s.repo.SoftDeleteJob(ctx, userID, jobUID); err != nil {
		return mapJobRepoError(err, dkdomain.ErrCodeJobNotFound)
	}
	return nil
}

// ============================================================================
// 重试一张（决策 20 / 设计定型 6.2）
// ============================================================================

// RetryItemInput 重试一张要的参数。
type RetryItemInput struct {
	// UserID 谁点的重试（校验归属）。
	UserID int64
	// JobUID 哪个批次。
	JobUID string
	// Seq 第几张，从 1 开始。
	Seq int
	// IdempotencyKey 运营双击必然发生。
	//
	// ⚠ 目前仓储的 RetryItem **没有**用这个值做去重，真正挡住双击的是状态机：
	// 第一次重试之后这一张已经是 pending，第二次会撞上
	// CanTransitionItem(pending → pending) = false，直接 409。
	IdempotencyKey string
}

// RetryJobItem 重试一张。
//
// **重试 = 重新出一张图 = 重新收一次钱**，所以走跟提交完全同一套准入：
// 单张预估 → FOR UPDATE 查可用额 → 不够就拒绝（都在仓储那一个事务里）。
//
// 三条硬规矩（仓储负责执行，这里只负责把错误翻成中文）：
//   - max_attempts 是**累计**上限（默认 3），运营手动点的重试也算在内；
//   - settled_at 非空的批次拒绝重试 → DK_JOB_ALREADY_SETTLED
//     「这批任务已经结算，不能再重试了，请重新提交。」；
//   - 拉回 running 时 revision + 1、清 finished_at、把那张之前记的计数减回去。
func (s *JobService) RetryJobItem(ctx context.Context, in RetryItemInput) (*dkdomain.JobItem, error) {
	job, err := s.mustGetJob(ctx, in.UserID, in.JobUID)
	if err != nil {
		return nil, err
	}
	if in.Seq <= 0 {
		return nil, dkdomain.NewError(dkdomain.ErrCodeItemNotFound)
	}
	if job.IsSettled() {
		// 仓储里还会再判一次（那才是权威判据，它在事务里拿着行锁）。
		// 这里先判是为了少打一个事务，也让 409 来得干脆。
		return nil, dkdomain.NewError(dkdomain.ErrCodeJobAlreadySettled)
	}

	// 单张预估：跟提交用同一张价目表。没配价目表时是 0 —— 冻结额加不上，
	// 拦不住透支，理由和提交那条路一样（价目表是上线阻塞项）。
	pricing := s.resolvePricing(ctx, job.Ratio)
	estimated := pricing.costOf(1)

	item, err := s.repo.RetryItem(ctx, dkdomain.RetryItemParams{
		UserID:         in.UserID,
		JobID:          job.ID,
		Seq:            in.Seq,
		EstimatedCost:  estimated,
		IdempotencyKey: optionalString(in.IdempotencyKey),
	})
	if err != nil {
		var shortErr *dkdomain.InsufficientBalanceError
		if errors.As(err, &shortErr) {
			return nil, insufficientBalanceError(shortErr, s.adminContact(ctx))
		}
		return nil, mapJobRepoError(err, dkdomain.ErrCodeItemNotFound)
	}
	if item == nil {
		return nil, dkdomain.NewError(dkdomain.ErrCodeItemNotFound)
	}
	return item, nil
}
