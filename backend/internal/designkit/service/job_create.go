package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	dkdomain "github.com/Wei-Shaw/sub2api/internal/designkit/domain"
)

// ============================================================================
// 提交一个批次
// ============================================================================
//
// 顺序是设计定型 第五节写死的，**一步都不能挪**：
//
//	1. 校验（比例在白名单里、张数不超上限、素材是本人的、提示词还在）
//	2. 展开（商品图外层、提示词内层，seq 从 1 连续递增，提示词存当时的原文快照）
//	3. 一个事务里：SELECT balance FOR UPDATE → 算可用额 → 不够就整个回滚、什么都不建
//	                → INSERT job(created) → INSERT items → INSERT hold
//	4. 提交成功之后再把 job 推进 created → holding → running
//
// 把第 3 步拆开（先建 job 再冻结）就会造出孤儿冻结或者「有任务没冻结」的透支口子；
// 把第 4 步并进事务里，则会让「状态推进失败」把整笔提交回滚掉 —— 而那时候
// 运营已经看到「提交成功」了。

// jobPlan 是「这一批要出哪些图」展开之后的样子。
type jobPlan struct {
	// Items 展开好的每一张，顺序即 seq 顺序（seq 从 1 开始连续递增）。
	Items []dkdomain.CreateJobItemParams
	// AssetCount / PromptCount 用于报价和错误文案。
	AssetCount  int
	PromptCount int
	// Pricing 提交那一刻的价格参数。
	Pricing jobPricing
	// Model 这一批实际用的出图模型。
	Model string
}

// CreateJob 提交一个批次。
//
// 幂等由 handler（上游协调器）和仓储事务两层一起保证：
// 仓储在拿到用户行锁**之后**再按 (user_id, idempotency_key) 查一次 24 小时内的老任务，
// 命中就原样返回 Reused=true，**什么都不建、也不再冻结一次**。
func (s *JobService) CreateJob(ctx context.Context, in CreateJobInput) (*dkdomain.Job, error) {
	if in.UserID <= 0 {
		return nil, dkdomain.NewError(dkdomain.ErrCodeUnauthorized)
	}

	plan, err := s.buildPlan(ctx, in.JobSpec)
	if err != nil {
		return nil, err
	}

	apiKeyID, err := s.resolveAPIKeyID(ctx, in)
	if err != nil {
		return nil, err
	}

	estimated := plan.Pricing.costOf(len(plan.Items))
	if plan.Pricing.UnitPrice == nil {
		// 价目表还没实测出来（设计定型 第十节 1）。冻结额只能记 0，
		// 也就是说**这一批不占用可用额、拦不住透支**。
		// 唯一还在守着的是上游每张图出图前那条 `balance <= 0` 的准入。
		// 这行日志是管理员发现「该配价目表了」的唯一线索，不要删。
		s.logger().Warn("designkit 出图单价还没配，本批次冻结额记 0（不占用可用额，也拦不住透支）",
			slog.Int64("user_id", in.UserID),
			slog.String("ratio", in.Ratio.String()),
			slog.String("pricing_tier", plan.Pricing.Tier),
			slog.Int("item_count", len(plan.Items)),
			slog.String("how_to_fix", "实测各比例的出图像素后，把单价写进 designkit_settings."+SettingKeyUnitPrices))
	}

	origin := in.Origin
	if !origin.IsValid() {
		origin = dkdomain.OriginWeb
	}

	result, err := s.repo.CreateJobWithItemsAndHold(ctx, dkdomain.CreateJobParams{
		UserID:         in.UserID,
		APIKeyID:       apiKeyID,
		Origin:         origin,
		JobUID:         s.newUID(),
		Name:           strings.TrimSpace(in.Name),
		Ratio:          in.Ratio,
		Model:          plan.Model,
		IdempotencyKey: optionalString(in.IdempotencyKey),
		CallbackURL:    optionalString(in.CallbackURL),
		EstimatedCost:  estimated,
		Pricing: dkdomain.PricingSnapshot{
			BaseUnitPrice:       dkdomain.MoneyOrZero(plan.Pricing.UnitPrice),
			GroupRateMultiplier: plan.Pricing.Multiplier,
			// 上游账号倍率要等真正选中账号才知道（那是出图那一刻的事），
			// 快照里先记 1，结算按 usage_logs 的实际扣费走，不靠这个数算钱。
			AccountRateMultiplier: dkdomain.MultiplierFromFloat(1),
			BillingMode:           plan.Pricing.BillingMode,
			PricingTier:           plan.Pricing.Tier,
			PriceSource:           plan.Pricing.Source,
			Version:               jobPricingSnapshotVersion,
		},
		Items: plan.Items,
	})
	if err != nil {
		return nil, s.mapCreateError(ctx, err)
	}
	if result == nil || result.Job == nil {
		return nil, dkdomain.NewError(dkdomain.ErrCodeInternal).
			WithCause(errors.New("designkit: 仓储没有返回建好的批次"))
	}

	if result.Reused {
		// 命中幂等：这次没有新建任务、没有新冻结额度，返回的是上一次的结果。
		// **绝不能再推一次状态** —— 那个任务可能已经跑完了。
		s.logger().Info("designkit 命中幂等，返回已有批次",
			slog.String("job_uid", result.Job.UID), slog.Int64("user_id", in.UserID))
		return result.Job, nil
	}

	return s.startJob(ctx, result.Job)
}

// startJob 把刚建好的批次推进 created → holding → running。
//
// 为什么建完还要推：仓储建出来的是 created（状态机的入口，那一刻冻结额还没落地）。
// 停在 created 的任务**永远不会被出图队列碰到**，而且僵尸巡检只扫 holding/running，
// 也不会去回收它 —— 冻结额会永久占着用户的可用额，运营看到的是
// 「明明没任务却提示余额不足」。
//
// 推不动的时候（数据库抖了）必须善后：把冻结整笔释放、任务判死，
// 让运营重新提交。宁可让他重来一次，也不能留一笔占着钱的僵尸。
func (s *JobService) startJob(ctx context.Context, job *dkdomain.Job) (*dkdomain.Job, error) {
	steps := []struct {
		from dkdomain.JobStatus
		to   dkdomain.JobStatus
	}{
		{dkdomain.JobStatusCreated, dkdomain.JobStatusHolding},
		{dkdomain.JobStatusHolding, dkdomain.JobStatusRunning},
	}

	for _, step := range steps {
		if job.Status != step.from {
			continue
		}
		// 白名单是唯一判据，改状态之前先问一句（设计定型 2.5）。
		if !dkdomain.CanTransition(step.from, step.to) {
			return nil, s.abandonJob(ctx, job, fmt.Errorf(
				"designkit: 状态机不允许 %s → %s", step.from, step.to))
		}
		err := s.repo.UpdateJobStatus(ctx, job.ID, step.from, step.to)
		switch {
		case err == nil:
			job.Status = step.to
		case errors.Is(err, dkdomain.ErrConflict), errors.Is(err, dkdomain.ErrIllegalTransition):
			// 守卫没命中 = 别人已经推过了。最常见的是出图 worker 抢先把
			// holding 推成了 running（它领到第一张时就会推）。这不是错误，
			// 但我们手上这份 job 的状态已经过期了，回库里取一份真的。
			s.logger().Debug("designkit 批次状态已被别处推进，重新读取",
				slog.String("job_uid", job.UID),
				slog.String("from", step.from.String()),
				slog.String("to", step.to.String()))
			if fresh, freshErr := s.repo.GetJobByUID(ctx, job.UserID, job.UID); freshErr == nil && fresh != nil {
				return fresh, nil
			}
			return job, nil
		default:
			return nil, s.abandonJob(ctx, job, err)
		}
	}
	return job, nil
}

// abandonJob 提交没能走完时的善后：释放冻结、把任务判死，两步都尽力而为。
//
// 「任意状态 → failed」是状态机允许的兜底（domain.CanTransition 里那条口子），
// 就是给这种场景留的。
// abandonJobTimeout 是提交失败后善后动作（释放冻结 + 判死）的时间预算。
// 短一点没关系：真失败了还有僵尸巡检兜底，但不能没有上限，
// 否则数据库卡住时这个 goroutine 会一直挂着。
const abandonJobTimeout = 5 * time.Second

func (s *JobService) abandonJob(ctx context.Context, job *dkdomain.Job, cause error) error {
	log := s.logger().With(slog.String("job_uid", job.UID), slog.Int64("job_id", job.ID))

	// ⚠ 必须脱开调用方的 ctx。走到这里最常见的原因就是「客户端断开 / 网关超时
	// 导致 ctx 被取消」——如果善后动作也用这个 ctx，ReleaseHold 和判死会
	// 一起失败（context canceled），冻结额就留在库里，运营会看到
	// 「明明没任务却提示余额不足」。虽然 10 分钟后僵尸巡检能自愈，
	// 但那 10 分钟是白白让人困惑的。worker 侧为同样的理由做了 writeBackContext。
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), abandonJobTimeout)
	defer cancel()

	if err := s.repo.ReleaseHold(ctx, job.ID); err != nil {
		// 释放失败是真的要人管：这笔冻结会一直占着运营的可用额。
		log.Error("designkit 批次没能进入队列，冻结额释放也失败了，需要管理员手工处理",
			slog.Any("error", err))
	}

	// ⚠ **必须在判死之前把 idempotency_key 清掉。**
	// 不清的话，运营看到「失败了」直接再点一次提交（浏览器端那把 key 是打开确认
	// 弹窗时生成的，不刷新页面就还是同一把），24 小时内会命中提交事务里那次
	// 「按 key 查已有批次」，原样返回**这个 failed 的批次** ——
	// 界面显示「提交成功」，状态却是失败，而且换几个名字重试都一样，
	// 运营完全无法自救。清掉之后，重投就是干干净净的一次新提交。
	if err := s.repo.ClearJobIdempotencyKey(ctx, job.ID); err != nil {
		log.Warn("designkit 批次判死时没能清掉防重复标识，运营用同一个页面重投可能会拿回这个失败的批次",
			slog.Any("error", err))
	}

	if err := s.repo.UpdateJobStatus(ctx, job.ID, job.Status, dkdomain.JobStatusFailed); err != nil {
		log.Warn("designkit 批次判死失败", slog.Any("error", err))
	}

	log.Error("designkit 批次建好了但没能进入队列，已判死并释放冻结", slog.Any("error", cause))
	return dkdomain.NewError(dkdomain.ErrCodeInternal).
		WithMessage("提交没有完成，这一批没有开始出图，冻结的额度已经退回，请重新提交。").
		WithCause(cause)
}

// mapCreateError 翻译提交事务返回的错误。余额不足要带上「还差多少」和管理员联系方式。
func (s *JobService) mapCreateError(ctx context.Context, err error) error {
	var shortErr *dkdomain.InsufficientBalanceError
	if errors.As(err, &shortErr) {
		return insufficientBalanceError(shortErr, s.adminContact(ctx))
	}
	// 提交路径上的「查不到」只可能是用户行没了（FOR UPDATE 那条 SELECT）。
	return mapJobRepoError(err, dkdomain.ErrCodeUnauthorized)
}

// ----------------------------------------------------------------------------
// 展开
// ----------------------------------------------------------------------------

// buildPlan 校验并展开一批。
//
// **展开顺序固定「商品图外层、提示词内层」**，seq 从 1 连续递增：
//
//	图1×词1 → seq 1
//	图1×词2 → seq 2
//	图2×词1 → seq 3
//	…
//
// 两个理由：运营是按商品思考的（一个商品的几种风格排在一起）；
// 「按序号取第几张图」是发给 ERP 的对外契约。**这个顺序写死，有单测守着。**
func (s *JobService) buildPlan(ctx context.Context, spec JobSpec) (*jobPlan, error) {
	if err := s.checkRatio(ctx, spec.Ratio); err != nil {
		return nil, err
	}

	assetUIDs := trimNonEmpty(spec.AssetUIDs)
	if len(assetUIDs) == 0 {
		return nil, dkdomain.NewError(dkdomain.ErrCodeInvalidRequest).
			WithMessage("请至少选择一张商品图。")
	}
	promptUIDs := trimNonEmpty(spec.PromptUIDs)
	promptTexts := trimNonEmpty(spec.PromptTexts)
	if len(promptUIDs)+len(promptTexts) == 0 {
		return nil, dkdomain.NewError(dkdomain.ErrCodeInvalidRequest).
			WithMessage("请至少选择一条提示词，或者自己写一条。")
	}

	// 先算张数再去查库：超上限的请求不该先打几十条 SELECT 出去。
	maxBatch := s.maxBatchItems(ctx)
	itemCount := len(assetUIDs) * (len(promptUIDs) + len(promptTexts))
	if itemCount > maxBatch {
		return nil, dkdomain.NewError(dkdomain.ErrCodeBatchTooLarge).
			WithMessagef("这一批要出 %d 张（%d 张商品图 × %d 条提示词），超过单次上限 %d 张，请少选几张再提交。",
				itemCount, len(assetUIDs), len(promptUIDs)+len(promptTexts), maxBatch)
	}

	// —— 商品图：uid → 内部主键，顺带校验归属（不是本人的一律「找不到」，
	//    不返回 403 —— 那等于告诉对方「这个编号是存在的，只是不是你的」）——
	assetIDs := make([]int64, 0, len(assetUIDs))
	for _, uid := range assetUIDs {
		asset, err := s.repo.GetAssetByUID(ctx, spec.UserID, uid)
		if err != nil {
			return nil, mapJobRepoError(err, dkdomain.ErrCodeAssetNotFound)
		}
		if asset == nil {
			return nil, dkdomain.NewError(dkdomain.ErrCodeAssetNotFound)
		}
		assetIDs = append(assetIDs, asset.ID)
	}

	// —— 提示词：uid → (内部主键, 正文快照) ——
	type plannedPrompt struct {
		id   *int64
		text string
	}
	prompts := make([]plannedPrompt, 0, len(promptUIDs)+len(promptTexts))
	for _, uid := range promptUIDs {
		prompt, err := s.repo.GetPromptByUID(ctx, uid)
		if err != nil {
			return nil, mapJobRepoError(err, dkdomain.ErrCodePromptNotFound)
		}
		if prompt == nil {
			return nil, dkdomain.NewError(dkdomain.ErrCodePromptNotFound)
		}
		// 别人的自建词（source=user 且不是本人的）按「找不到」拒绝 ——
		// 自建词只有本人可见（「我的提示词」），列表带不出来，
		// 拿编号硬提交也不能放行，否则可见性就是摆设。
		if prompt.Source == dkdomain.PromptSourceUser &&
			(prompt.OwnerUserID == nil || *prompt.OwnerUserID != spec.UserID) {
			return nil, dkdomain.NewError(dkdomain.ErrCodePromptNotFound)
		}
		body := strings.TrimSpace(prompt.Body)
		if body == "" {
			return nil, dkdomain.NewError(dkdomain.ErrCodeInvalidRequest).
				WithMessagef("灵感库里这条提示词（%s）的正文是空的，出不了图，请换一条。", prompt.UID)
		}
		id := prompt.ID
		prompts = append(prompts, plannedPrompt{id: &id, text: body})
	}
	// 手打的接在灵感库那几条后面，prompt_id 为空。
	for _, text := range promptTexts {
		prompts = append(prompts, plannedPrompt{text: text})
	}

	// —— 展开。**外层商品图、内层提示词，seq 从 1 开始** ——
	items := make([]dkdomain.CreateJobItemParams, 0, itemCount)
	seq := 1
	for _, assetID := range assetIDs {
		for _, prompt := range prompts {
			items = append(items, dkdomain.CreateJobItemParams{
				Seq:     seq,
				AssetID: &assetID,
				// VariantID 提交时留空：预处理产物由出图 worker 现做（或复用），
				// 它的唯一键是 (asset_id, ratio, keep_transparency, max_dimension)，
				// 而 max_dimension 是运行时配置，提交这一刻定死没有意义。
				VariantID:   nil,
				PromptID:    prompt.id,
				PromptText:  prompt.text,
				MaxAttempts: dkdomain.DefaultMaxAttempts,
			})
			seq++
		}
	}

	model := strings.TrimSpace(spec.Model)
	if model == "" {
		model = s.defaultModel(ctx)
	}

	return &jobPlan{
		Items:       items,
		AssetCount:  len(assetIDs),
		PromptCount: len(prompts),
		Pricing:     s.resolvePricing(ctx, spec.Ratio),
		Model:       model,
	}, nil
}

// resolveAPIKeyID 决定这一批记在哪把 Key 上。
//
// ERP 走上游 apiKeyAuth，上下文里有真实的 Key（费用记在 ERP 自己名下）；
// 浏览器端走 jwtAuth，没有 Key，用该用户的内部专用 Key 兜底。
// designkit_jobs.api_key_id 是 NOT NULL —— 出图时上游 Images() 取不到 Key 直接 401。
func (s *JobService) resolveAPIKeyID(ctx context.Context, in CreateJobInput) (int64, error) {
	if in.APIKeyID > 0 {
		return in.APIKeyID, nil
	}
	if s.keys == nil {
		return 0, dkdomain.NewError(dkdomain.ErrCodeAPIKeyMissing).
			WithCause(errors.New("designkit: 内部专用 API Key 服务没有装配"))
	}
	key, err := s.keys.EnsureInternalKey(ctx, in.UserID)
	if err != nil {
		// EnsureInternalKey 返回的已经是带中文文案的我方错误。
		return 0, err
	}
	if key == nil || key.ID <= 0 {
		return 0, dkdomain.NewError(dkdomain.ErrCodeAPIKeyMissing)
	}
	return key.ID, nil
}

// ----------------------------------------------------------------------------
// 小工具
// ----------------------------------------------------------------------------

// optionalString 空串 → nil（对应可空列）。
func optionalString(v string) *string {
	trimmed := strings.TrimSpace(v)
	if trimmed == "" {
		return nil
	}
	return &trimmed
}
