//go:build unit

package service

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	dkdomain "github.com/Wei-Shaw/sub2api/internal/designkit/domain"
	upstreamservice "github.com/Wei-Shaw/sub2api/internal/service"
)

// ============================================================================
// 批次服务的单测
// ============================================================================
//
// 守的是三条「错一次就是钱」的线：
//
//	1. 展开顺序「商品图外层、提示词内层」、seq 从 1 连续递增（ERP 契约）
//	2. 价目表没实测出来时**一个数字都不给**（不给 0，也不给猜的数）
//	3. 「停止排队」只砍 pending，绝不碰在跑的那几张
//
// 全部不连数据库、不连对象存储、不调网关 —— 假实现就在本文件里。

// ---- 假的持久化层 ----

type jobStatusCall struct {
	jobID int64
	from  dkdomain.JobStatus
	to    dkdomain.JobStatus
}

type jobFakeRepo struct {
	settings map[string]string
	assets   map[string]*dkdomain.Asset
	prompts  map[string]*dkdomain.Prompt

	summary    *dkdomain.UsageSummary
	summaryErr error

	createParams *dkdomain.CreateJobParams
	createResult *dkdomain.CreateJobResult
	createErr    error

	job    *dkdomain.Job
	page   *dkdomain.JobPage
	items  []*dkdomain.JobItem
	images map[int64][]*dkdomain.Image

	statusCalls []jobStatusCall
	statusErrs  map[string]error

	stopCalls     int
	stopCancelled int
	stopErr       error

	finalizeParams *dkdomain.FinalizeJobParams
	finalizeWon    bool

	retryParams *dkdomain.RetryItemParams
	retryItem   *dkdomain.JobItem
	retryErr    error

	softDeleteCalls int
	softDeleteErr   error

	releasedHolds []int64
	listQuery     *dkdomain.ListJobsQuery

	// clearedIdempotencyKeys 记下 abandonJob 清过哪几个批次的防重复标识。
	clearedIdempotencyKeys []int64
}

func newJobFakeRepo() *jobFakeRepo {
	return &jobFakeRepo{
		settings: map[string]string{},
		assets:   map[string]*dkdomain.Asset{},
		prompts:  map[string]*dkdomain.Prompt{},
		images:   map[int64][]*dkdomain.Image{},
		summary: &dkdomain.UsageSummary{
			Balance:   dkdomain.MoneyFromFloat(10),
			Available: dkdomain.MoneyFromFloat(10),
			Currency:  dkdomain.CurrencyUSD,
		},
		statusErrs: map[string]error{},
	}
}

func (r *jobFakeRepo) CreateJobWithItemsAndHold(_ context.Context, params dkdomain.CreateJobParams) (*dkdomain.CreateJobResult, error) {
	copied := params
	r.createParams = &copied
	if r.createErr != nil {
		return nil, r.createErr
	}
	if r.createResult != nil {
		return r.createResult, nil
	}
	job := &dkdomain.Job{
		ID:            1,
		UID:           params.JobUID,
		UserID:        params.UserID,
		APIKeyID:      params.APIKeyID,
		Origin:        params.Origin,
		Name:          params.Name,
		Status:        dkdomain.JobStatusCreated,
		Ratio:         params.Ratio,
		Model:         params.Model,
		ItemCount:     len(params.Items),
		Currency:      dkdomain.CurrencyUSD,
		EstimatedCost: params.EstimatedCost,
	}
	r.job = job
	return &dkdomain.CreateJobResult{Job: job}, nil
}

func (r *jobFakeRepo) GetJobByUID(_ context.Context, userID int64, uid string) (*dkdomain.Job, error) {
	if r.job == nil || r.job.UID != uid || r.job.UserID != userID {
		return nil, dkdomain.ErrNotFound
	}
	return r.job, nil
}

func (r *jobFakeRepo) ListJobs(_ context.Context, query dkdomain.ListJobsQuery) (*dkdomain.JobPage, error) {
	copied := query
	r.listQuery = &copied
	if r.page != nil {
		return r.page, nil
	}
	return &dkdomain.JobPage{}, nil
}

func (r *jobFakeRepo) ListJobItems(_ context.Context, _ int64) ([]*dkdomain.JobItem, error) {
	return r.items, nil
}

func (r *jobFakeRepo) GetJobItemBySeq(_ context.Context, _ int64, seq int) (*dkdomain.JobItem, error) {
	for _, item := range r.items {
		if item != nil && item.Seq == seq {
			return item, nil
		}
	}
	return nil, dkdomain.ErrNotFound
}

func (r *jobFakeRepo) ListImagesByItem(_ context.Context, itemID int64, _ bool) ([]*dkdomain.Image, error) {
	return r.images[itemID], nil
}

func (r *jobFakeRepo) UpdateJobStatus(_ context.Context, jobID int64, from, to dkdomain.JobStatus) error {
	r.statusCalls = append(r.statusCalls, jobStatusCall{jobID: jobID, from: from, to: to})
	if err, ok := r.statusErrs[from.String()+"->"+to.String()]; ok {
		return err
	}
	if r.job != nil && r.job.ID == jobID {
		r.job.Status = to
	}
	return nil
}

func (r *jobFakeRepo) ClearJobIdempotencyKey(_ context.Context, jobID int64) error {
	r.clearedIdempotencyKeys = append(r.clearedIdempotencyKeys, jobID)
	return nil
}

func (r *jobFakeRepo) RequestJobStop(_ context.Context, _ int64) (int, error) {
	r.stopCalls++
	if r.stopErr != nil {
		return 0, r.stopErr
	}
	if r.job != nil {
		r.job.CancelledCount += r.stopCancelled
		now := time.Now()
		r.job.CancelRequestedAt = &now
	}
	return r.stopCancelled, nil
}

func (r *jobFakeRepo) RetryItem(_ context.Context, params dkdomain.RetryItemParams) (*dkdomain.JobItem, error) {
	copied := params
	r.retryParams = &copied
	if r.retryErr != nil {
		return nil, r.retryErr
	}
	return r.retryItem, nil
}

func (r *jobFakeRepo) FinalizeJob(_ context.Context, params dkdomain.FinalizeJobParams) (bool, error) {
	copied := params
	r.finalizeParams = &copied
	return r.finalizeWon, nil
}

func (r *jobFakeRepo) SoftDeleteJob(_ context.Context, userID int64, uid string) error {
	r.softDeleteCalls++
	if r.softDeleteErr != nil {
		return r.softDeleteErr
	}
	if r.job == nil || r.job.UID != uid || r.job.UserID != userID || r.job.UserDeletedAt != nil {
		return dkdomain.ErrNotFound
	}
	now := time.Now()
	r.job.UserDeletedAt = &now
	return nil
}

func (r *jobFakeRepo) GetAssetByUID(_ context.Context, userID int64, uid string) (*dkdomain.Asset, error) {
	asset, ok := r.assets[uid]
	if !ok || asset.UserID != userID {
		return nil, dkdomain.ErrNotFound
	}
	return asset, nil
}

func (r *jobFakeRepo) GetPromptByUID(_ context.Context, uid string) (*dkdomain.Prompt, error) {
	prompt, ok := r.prompts[uid]
	if !ok {
		return nil, dkdomain.ErrNotFound
	}
	return prompt, nil
}

func (r *jobFakeRepo) GetUsageSummary(_ context.Context, _ int64, _, _ time.Time) (*dkdomain.UsageSummary, error) {
	if r.summaryErr != nil {
		return nil, r.summaryErr
	}
	return r.summary, nil
}

func (r *jobFakeRepo) ReleaseHold(_ context.Context, jobID int64) error {
	r.releasedHolds = append(r.releasedHolds, jobID)
	return nil
}

func (r *jobFakeRepo) GetSetting(_ context.Context, key string) (*dkdomain.Setting, error) {
	raw, ok := r.settings[key]
	if !ok {
		return nil, dkdomain.ErrNotFound
	}
	return &dkdomain.Setting{Key: key, Value: []byte(raw)}, nil
}

// 编译期断言：假实现跟真接口一致。
var _ JobStore = (*jobFakeRepo)(nil)

// ---- 假的内部 Key 服务 ----

type jobFakeKeys struct {
	keyID int64
	err   error
	calls int
}

func (k *jobFakeKeys) EnsureInternalKey(_ context.Context, userID int64) (*upstreamservice.APIKey, error) {
	k.calls++
	if k.err != nil {
		return nil, k.err
	}
	return &upstreamservice.APIKey{ID: k.keyID, UserID: userID}, nil
}

// ---- 假的对象存储 ----

type jobFakeStore struct {
	objects map[string][]byte
}

func (s *jobFakeStore) Put(_ context.Context, key string, _ string, data []byte) error {
	s.objects[key] = data
	return nil
}

func (s *jobFakeStore) Get(_ context.Context, key string) ([]byte, string, error) {
	data, ok := s.objects[key]
	if !ok {
		return nil, "", dkdomain.ErrObjectNotFound
	}
	return data, "image/png", nil
}

func (s *jobFakeStore) Delete(_ context.Context, key string) error {
	delete(s.objects, key)
	return nil
}

func (s *jobFakeStore) Exists(_ context.Context, key string) (bool, error) {
	_, ok := s.objects[key]
	return ok, nil
}

var _ dkdomain.ObjectStore = (*jobFakeStore)(nil)

// ---- 夹具 ----

const jobTestUID = "01J8ZK7Q9X2M4N6P8R0T2V4W6Y"

func jobTestTime() time.Time {
	return time.Date(2026, 8, 13, 1, 2, 3, 0, time.UTC)
}

func newJobFixture(t *testing.T) (*JobService, *jobFakeRepo, *jobFakeKeys) {
	t.Helper()
	repo := newJobFakeRepo()
	keys := &jobFakeKeys{keyID: 42}
	svc, err := NewJobService(JobServiceDeps{
		Repo:   repo,
		Store:  &jobFakeStore{objects: map[string][]byte{}},
		Keys:   keys,
		Now:    jobTestTime,
		NewUID: func() string { return jobTestUID },
	})
	if err != nil {
		t.Fatalf("建 JobService 失败：%v", err)
	}
	return svc, repo, keys
}

// jobSeedAsset 造一张属于 userID 的商品图。
func jobSeedAsset(repo *jobFakeRepo, uid string, id, userID int64) {
	repo.assets[uid] = &dkdomain.Asset{ID: id, UID: uid, UserID: userID}
}

// jobSeedPrompt 造一条灵感库提示词。
func jobSeedPrompt(repo *jobFakeRepo, uid string, id int64, body string) {
	repo.prompts[uid] = &dkdomain.Prompt{ID: id, UID: uid, Body: body}
}

func jobRequireCode(t *testing.T, err error, wantCode string) *dkdomain.DesignkitError {
	t.Helper()
	if err == nil {
		t.Fatalf("期望错误码 %s，实际没有报错", wantCode)
	}
	dkErr, ok := dkdomain.AsDesignkitError(err)
	if !ok {
		t.Fatalf("期望 *DesignkitError（%s），实际：%v", wantCode, err)
	}
	if dkErr.Code != wantCode {
		t.Fatalf("期望错误码 %s，实际 %s（%s）", wantCode, dkErr.Code, dkErr.Message)
	}
	return dkErr
}

func jobBaseInput(userID int64) CreateJobInput {
	return CreateJobInput{
		JobSpec: JobSpec{
			UserID:      userID,
			Origin:      dkdomain.OriginWeb,
			Ratio:       dkdomain.Ratio3x4,
			AssetUIDs:   []string{"asset-1", "asset-2"},
			PromptUIDs:  []string{"prompt-1"},
			PromptTexts: []string{"纯白背景棚拍"},
		},
		Name:           "夏季连衣裙",
		IdempotencyKey: "browser-uuid-1",
		APIKeyID:       7,
	}
}

// ============================================================================
// 展开顺序 —— ERP 契约，这条破了不会报错，只会让 ERP 把图配错商品
// ============================================================================

func TestJobService_CreateJob_ExpandsAssetOuterPromptInner(t *testing.T) {
	svc, repo, _ := newJobFixture(t)
	jobSeedAsset(repo, "asset-1", 11, 7)
	jobSeedAsset(repo, "asset-2", 22, 7)
	jobSeedPrompt(repo, "prompt-1", 101, "灵感库里的词")

	job, err := svc.CreateJob(context.Background(), jobBaseInput(7))
	if err != nil {
		t.Fatalf("提交不该失败：%v", err)
	}
	if job == nil {
		t.Fatal("提交成功必须返回批次")
	}

	items := repo.createParams.Items
	if len(items) != 4 {
		t.Fatalf("2 张图 × 2 条词应该展开成 4 张，实际 %d", len(items))
	}

	// 图1×词1 → 1，图1×词2 → 2，图2×词1 → 3，图2×词2 → 4
	wantAsset := []int64{11, 11, 22, 22}
	wantText := []string{"灵感库里的词", "纯白背景棚拍", "灵感库里的词", "纯白背景棚拍"}
	for i, item := range items {
		if item.Seq != i+1 {
			t.Fatalf("第 %d 个 item 的 seq 应该是 %d，实际 %d（seq 必须从 1 连续递增）", i+1, i+1, item.Seq)
		}
		if item.AssetID == nil || *item.AssetID != wantAsset[i] {
			t.Fatalf("seq %d 应该用商品图 %d，实际 %v（展开顺序必须是商品图外层）", item.Seq, wantAsset[i], item.AssetID)
		}
		if item.PromptText != wantText[i] {
			t.Fatalf("seq %d 的提示词应该是 %q，实际 %q（展开顺序必须是提示词内层）", item.Seq, wantText[i], item.PromptText)
		}
		if item.MaxAttempts != dkdomain.DefaultMaxAttempts {
			t.Fatalf("seq %d 的 max_attempts 应该是默认值 %d，实际 %d", item.Seq, dkdomain.DefaultMaxAttempts, item.MaxAttempts)
		}
	}

	// 灵感库来的那两条要记住 prompt_id，手打的必须是 nil。
	if items[0].PromptID == nil || *items[0].PromptID != 101 {
		t.Fatalf("灵感库提示词要记 prompt_id，实际 %v", items[0].PromptID)
	}
	if items[1].PromptID != nil {
		t.Fatalf("手打的提示词 prompt_id 必须为空，实际 %v", *items[1].PromptID)
	}
}

// 提示词存的是**当时的原文快照**：灵感库以后改了措辞，历史记录不跟着变。
func TestJobService_CreateJob_SnapshotsPromptText(t *testing.T) {
	svc, repo, _ := newJobFixture(t)
	jobSeedAsset(repo, "asset-1", 11, 7)
	jobSeedPrompt(repo, "prompt-1", 101, "  提交那一刻的原文  ")

	in := jobBaseInput(7)
	in.AssetUIDs = []string{"asset-1"}
	in.PromptTexts = nil

	if _, err := svc.CreateJob(context.Background(), in); err != nil {
		t.Fatalf("提交不该失败：%v", err)
	}
	if got := repo.createParams.Items[0].PromptText; got != "提交那一刻的原文" {
		t.Fatalf("提示词快照应该是去掉首尾空白的原文，实际 %q", got)
	}

	// 灵感库改了措辞，已经落库的快照不受影响（这里直接改假数据来模拟）。
	repo.prompts["prompt-1"].Body = "后来被改掉的措辞"
	if got := repo.createParams.Items[0].PromptText; got != "提交那一刻的原文" {
		t.Fatalf("灵感库改词之后历史快照不该变，实际 %q", got)
	}
}

// ============================================================================
// 校验
// ============================================================================

func TestJobService_CreateJob_RejectsRatioOutsideSettings(t *testing.T) {
	svc, repo, _ := newJobFixture(t)
	// 白名单以数据库为准：这里只放 1:1。
	repo.settings[dkdomain.SettingKeyRatios] = `["1:1"]`
	jobSeedAsset(repo, "asset-1", 11, 7)
	jobSeedPrompt(repo, "prompt-1", 101, "词")

	_, err := svc.CreateJob(context.Background(), jobBaseInput(7))
	dkErr := jobRequireCode(t, err, dkdomain.ErrCodeRatioNotAllowed)
	if !strings.Contains(dkErr.Message, "1:1") {
		t.Fatalf("文案里要告诉运营能选哪些比例，实际：%s", dkErr.Message)
	}
	if repo.createParams != nil {
		t.Fatal("比例不合法时绝不能走到建任务那一步")
	}
}

func TestJobService_CreateJob_RejectsBatchTooLarge(t *testing.T) {
	svc, repo, _ := newJobFixture(t)
	repo.settings[dkdomain.SettingKeyMaxBatchItems] = `3`
	jobSeedAsset(repo, "asset-1", 11, 7)
	jobSeedAsset(repo, "asset-2", 22, 7)
	jobSeedPrompt(repo, "prompt-1", 101, "词")

	_, err := svc.CreateJob(context.Background(), jobBaseInput(7)) // 2 × 2 = 4 > 3
	dkErr := jobRequireCode(t, err, dkdomain.ErrCodeBatchTooLarge)
	if !strings.Contains(dkErr.Message, "3") || !strings.Contains(dkErr.Message, "4") {
		t.Fatalf("文案要说清「要出几张、上限几张」，实际：%s", dkErr.Message)
	}
	if repo.createParams != nil {
		t.Fatal("超上限时绝不能走到建任务那一步")
	}
}

// 别人的商品图一律「找不到」，**不返回 403**（403 等于告诉对方这个编号存在）。
func TestJobService_CreateJob_RejectsAssetOfAnotherUser(t *testing.T) {
	svc, repo, _ := newJobFixture(t)
	jobSeedAsset(repo, "asset-1", 11, 7)
	jobSeedAsset(repo, "asset-2", 22, 999) // 别人的
	jobSeedPrompt(repo, "prompt-1", 101, "词")

	_, err := svc.CreateJob(context.Background(), jobBaseInput(7))
	dkErr := jobRequireCode(t, err, dkdomain.ErrCodeAssetNotFound)
	if dkErr.HTTPStatus == 403 {
		t.Fatal("归属不符必须报「找不到」，不能报 403")
	}
}

func TestJobService_CreateJob_RejectsMissingPrompt(t *testing.T) {
	svc, repo, _ := newJobFixture(t)
	jobSeedAsset(repo, "asset-1", 11, 7)
	jobSeedAsset(repo, "asset-2", 22, 7)

	_, err := svc.CreateJob(context.Background(), jobBaseInput(7))
	jobRequireCode(t, err, dkdomain.ErrCodePromptNotFound)
}

// ============================================================================
// 钱
// ============================================================================

// 余额不足要说清「还差多少」，并且**一张都不建**（事务在仓储里回滚）。
func TestJobService_CreateJob_InsufficientBalanceSaysShortfall(t *testing.T) {
	svc, repo, _ := newJobFixture(t)
	jobSeedAsset(repo, "asset-1", 11, 7)
	jobSeedAsset(repo, "asset-2", 22, 7)
	jobSeedPrompt(repo, "prompt-1", 101, "词")
	repo.settings[dkdomain.SettingKeyAdminContact] = `"运营群 @小王"`
	repo.createErr = &dkdomain.InsufficientBalanceError{
		Required:  dkdomain.MoneyFromFloat(0.5),
		Available: dkdomain.MoneyFromFloat(0.1),
		Shortfall: dkdomain.MoneyFromFloat(0.4),
	}

	_, err := svc.CreateJob(context.Background(), jobBaseInput(7))
	dkErr := jobRequireCode(t, err, dkdomain.ErrCodeInsufficientBalance)
	if !strings.Contains(dkErr.Message, "$0.4000") {
		t.Fatalf("必须告诉运营还差多少（美元），实际：%s", dkErr.Message)
	}
	if strings.Contains(dkErr.Message, "元") {
		t.Fatalf("金额一律美元，文案里不许出现「元」，实际：%s", dkErr.Message)
	}
	if !strings.Contains(dkErr.Message, "运营群 @小王") {
		t.Fatalf("要把管理员联系方式拼进去（决策 19），实际：%s", dkErr.Message)
	}
}

// 价目表没实测出来时：单价和预估金额都必须是 nil，**绝不能是 0**（0 会被读成「免费」）。
func TestJobService_EstimateJob_PricePendingGivesNoNumber(t *testing.T) {
	svc, _, _ := newJobFixture(t)

	est, err := svc.EstimateJob(context.Background(), jobBaseInput(7).JobSpec)
	if err != nil {
		t.Fatalf("报价不该失败：%v", err)
	}
	if est.UnitPrice != nil {
		t.Fatalf("没配价目表时单价必须是 nil，实际 %s", dkdomain.MoneyString(*est.UnitPrice))
	}
	if est.EstimatedCost != nil {
		t.Fatalf("没配价目表时预估金额必须是 nil（不是 0），实际 %s", dkdomain.MoneyString(*est.EstimatedCost))
	}
	if est.PriceNote == "" {
		t.Fatal("价格待确认时必须给一句中文说明")
	}
	if !est.Sufficient || !est.Shortfall.IsZero() {
		t.Fatal("算不出金额时不该假装拦得住，Sufficient 要为 true、差额为 0")
	}
	if est.ItemCount != 4 || est.AssetCount != 2 || est.PromptCount != 2 {
		t.Fatalf("张数算错了：item=%d asset=%d prompt=%d", est.ItemCount, est.AssetCount, est.PromptCount)
	}
	if est.MaxBatchItems != dkdomain.DefaultMaxBatchItems {
		t.Fatalf("单次上限应该是默认的 %d，实际 %d", dkdomain.DefaultMaxBatchItems, est.MaxBatchItems)
	}
}

// 配好价目表之后：预估 = 单价 × 张数 × 倍率。
func TestJobService_EstimateJob_UsesPriceTable(t *testing.T) {
	svc, repo, _ := newJobFixture(t)
	repo.settings[SettingKeyUnitPrices] = `{"2K":"0.042"}`
	repo.settings[SettingKeyRateMultiplier] = `"2"`
	repo.summary = &dkdomain.UsageSummary{
		Balance:   dkdomain.MoneyFromFloat(1),
		Available: dkdomain.MoneyFromFloat(0.1),
	}

	est, err := svc.EstimateJob(context.Background(), jobBaseInput(7).JobSpec)
	if err != nil {
		t.Fatalf("报价不该失败：%v", err)
	}
	if est.PricingTier != dkdomain.BillingTier2K {
		t.Fatalf("默认最长边 2048 应该落 2K 档，实际 %s", est.PricingTier)
	}
	if est.UnitPrice == nil || dkdomain.MoneyString(*est.UnitPrice) != "0.042" {
		t.Fatalf("单价应该是 0.042，实际 %v", est.UnitPrice)
	}
	// 0.042 × 4 张 × 2 倍 = 0.336
	if est.EstimatedCost == nil || !est.EstimatedCost.Equal(dkdomain.MoneyFromFloat(0.336)) {
		t.Fatalf("预估应该是 0.336，实际 %v", est.EstimatedCost)
	}
	if est.Sufficient {
		t.Fatal("可用额 0.1 < 预估 0.336，应该判不够")
	}
	// 差额 0.336 - 0.1 = 0.236
	if !est.Shortfall.Equal(dkdomain.MoneyFromFloat(0.236)) {
		t.Fatalf("差额应该是 0.236，实际 %s", dkdomain.MoneyString(est.Shortfall))
	}
}

// 价目表里的价格写坏了 → 退回「价格待确认」，绝不当成 0 继续算。
func TestJobService_EstimateJob_BadPriceTableFallsBackToPending(t *testing.T) {
	svc, repo, _ := newJobFixture(t)
	repo.settings[SettingKeyUnitPrices] = `{"2K":"随便写的"}`

	est, err := svc.EstimateJob(context.Background(), jobBaseInput(7).JobSpec)
	if err != nil {
		t.Fatalf("报价不该失败：%v", err)
	}
	if est.UnitPrice != nil || est.EstimatedCost != nil {
		t.Fatal("价格写坏了必须退回「价格待确认」，不能算出一个数")
	}
}

// 没配价目表时冻结额记 0：这是已知的取舍（拦不住透支），但**必须真的是 0**，
// 不能悄悄冒出一个猜的数字去冻结用户的钱。
func TestJobService_CreateJob_HoldsZeroWhenPriceUnknown(t *testing.T) {
	svc, repo, _ := newJobFixture(t)
	jobSeedAsset(repo, "asset-1", 11, 7)
	jobSeedAsset(repo, "asset-2", 22, 7)
	jobSeedPrompt(repo, "prompt-1", 101, "词")

	if _, err := svc.CreateJob(context.Background(), jobBaseInput(7)); err != nil {
		t.Fatalf("提交不该失败：%v", err)
	}
	if !repo.createParams.EstimatedCost.IsZero() {
		t.Fatalf("没配价目表时预估花费必须是 0，实际 %s", dkdomain.MoneyString(repo.createParams.EstimatedCost))
	}
	if repo.createParams.Pricing.BillingMode == "token" {
		t.Fatal("计价方式绝不能落成 token（那是后台配错了的标志值）")
	}
}

// ============================================================================
// 状态推进
// ============================================================================

func TestJobService_CreateJob_AdvancesCreatedHoldingRunning(t *testing.T) {
	svc, repo, _ := newJobFixture(t)
	jobSeedAsset(repo, "asset-1", 11, 7)
	jobSeedAsset(repo, "asset-2", 22, 7)
	jobSeedPrompt(repo, "prompt-1", 101, "词")

	job, err := svc.CreateJob(context.Background(), jobBaseInput(7))
	if err != nil {
		t.Fatalf("提交不该失败：%v", err)
	}
	if len(repo.statusCalls) != 2 {
		t.Fatalf("应该推两步（created→holding→running），实际 %d 步：%+v", len(repo.statusCalls), repo.statusCalls)
	}
	if repo.statusCalls[0].from != dkdomain.JobStatusCreated || repo.statusCalls[0].to != dkdomain.JobStatusHolding {
		t.Fatalf("第一步必须是 created→holding，实际 %+v", repo.statusCalls[0])
	}
	if repo.statusCalls[1].from != dkdomain.JobStatusHolding || repo.statusCalls[1].to != dkdomain.JobStatusRunning {
		t.Fatalf("第二步必须是 holding→running，实际 %+v", repo.statusCalls[1])
	}
	if job.Status != dkdomain.JobStatusRunning {
		t.Fatalf("提交完返回的批次应该已经是 running，实际 %s", job.Status)
	}
}

// 命中幂等时**什么都不能再推**：那个任务可能早就跑完了。
func TestJobService_CreateJob_ReusedDoesNotTouchStatus(t *testing.T) {
	svc, repo, _ := newJobFixture(t)
	jobSeedAsset(repo, "asset-1", 11, 7)
	jobSeedAsset(repo, "asset-2", 22, 7)
	jobSeedPrompt(repo, "prompt-1", 101, "词")

	existing := &dkdomain.Job{ID: 9, UID: jobTestUID, UserID: 7, Status: dkdomain.JobStatusSucceeded}
	repo.createResult = &dkdomain.CreateJobResult{Job: existing, Reused: true}

	job, err := svc.CreateJob(context.Background(), jobBaseInput(7))
	if err != nil {
		t.Fatalf("幂等重放不该报错：%v", err)
	}
	if job.Status != dkdomain.JobStatusSucceeded {
		t.Fatalf("幂等重放应该原样返回已有批次，实际状态 %s", job.Status)
	}
	if len(repo.statusCalls) != 0 {
		t.Fatalf("幂等重放不该改任何状态，实际推了 %d 次", len(repo.statusCalls))
	}
}

// 推不动的时候必须善后：释放冻结 + 判死。留一笔占着钱的僵尸最糟。
func TestJobService_CreateJob_ReleasesHoldWhenStatusPushFails(t *testing.T) {
	svc, repo, _ := newJobFixture(t)
	jobSeedAsset(repo, "asset-1", 11, 7)
	jobSeedAsset(repo, "asset-2", 22, 7)
	jobSeedPrompt(repo, "prompt-1", 101, "词")
	repo.statusErrs["created->holding"] = errors.New("数据库抖了")

	_, err := svc.CreateJob(context.Background(), jobBaseInput(7))
	jobRequireCode(t, err, dkdomain.ErrCodeInternal)

	if len(repo.releasedHolds) != 1 || repo.releasedHolds[0] != 1 {
		t.Fatalf("推不动时必须把冻结释放掉，实际 %v", repo.releasedHolds)
	}
	var judged bool
	for _, call := range repo.statusCalls {
		if call.to == dkdomain.JobStatusFailed {
			judged = true
		}
	}
	if !judged {
		t.Fatalf("推不动时必须把任务判死，实际的状态调用：%+v", repo.statusCalls)
	}

	// ⚠ 判死的同时必须把 idempotency_key 清掉。
	// 不清的话，运营看到「失败了」直接在同一个页面再点一次提交（那把 key 没变），
	// 24 小时内会命中提交事务里那次「按 key 查已有批次」，原样拿回这个 failed 的
	// 批次 —— 界面显示「提交成功」、状态却是失败，怎么点都一样，完全无法自救。
	if len(repo.clearedIdempotencyKeys) != 1 || repo.clearedIdempotencyKeys[0] != 1 {
		t.Fatalf("判死时必须清掉防重复标识，否则同一把 key 重投会拿回这个失败的批次，实际 %v",
			repo.clearedIdempotencyKeys)
	}
}

// worker 抢先把 holding 推成了 running（很常见）：这不是错误。
func TestJobService_CreateJob_ToleratesConcurrentAdvance(t *testing.T) {
	svc, repo, _ := newJobFixture(t)
	jobSeedAsset(repo, "asset-1", 11, 7)
	jobSeedAsset(repo, "asset-2", 22, 7)
	jobSeedPrompt(repo, "prompt-1", 101, "词")
	repo.statusErrs["holding->running"] = dkdomain.ErrConflict

	job, err := svc.CreateJob(context.Background(), jobBaseInput(7))
	if err != nil {
		t.Fatalf("守卫没命中说明别人推过了，不该报错：%v", err)
	}
	if job == nil {
		t.Fatal("仍然要返回批次")
	}
	if len(repo.releasedHolds) != 0 {
		t.Fatal("这不是失败，绝不能把冻结释放掉")
	}
}

// ============================================================================
// API Key
// ============================================================================

func TestJobService_CreateJob_FallsBackToInternalKey(t *testing.T) {
	svc, repo, keys := newJobFixture(t)
	jobSeedAsset(repo, "asset-1", 11, 7)
	jobSeedAsset(repo, "asset-2", 22, 7)
	jobSeedPrompt(repo, "prompt-1", 101, "词")

	in := jobBaseInput(7)
	in.APIKeyID = 0 // 浏览器端没有 Key

	if _, err := svc.CreateJob(context.Background(), in); err != nil {
		t.Fatalf("提交不该失败：%v", err)
	}
	if keys.calls != 1 {
		t.Fatalf("浏览器端提交应该去要一把内部专用 Key，实际调了 %d 次", keys.calls)
	}
	if repo.createParams.APIKeyID != 42 {
		t.Fatalf("api_key_id 应该是内部 Key 的 42，实际 %d", repo.createParams.APIKeyID)
	}
}

func TestJobService_CreateJob_WithoutKeyServiceSaysSo(t *testing.T) {
	repo := newJobFakeRepo()
	jobSeedAsset(repo, "asset-1", 11, 7)
	jobSeedAsset(repo, "asset-2", 22, 7)
	jobSeedPrompt(repo, "prompt-1", 101, "词")
	svc, err := NewJobService(JobServiceDeps{Repo: repo, Now: jobTestTime, NewUID: func() string { return jobTestUID }})
	if err != nil {
		t.Fatalf("建服务失败：%v", err)
	}

	in := jobBaseInput(7)
	in.APIKeyID = 0
	_, err = svc.CreateJob(context.Background(), in)
	jobRequireCode(t, err, dkdomain.ErrCodeAPIKeyMissing)
	if repo.createParams != nil {
		t.Fatal("拿不到 Key 时不能建任务：出图时上游取不到 Key 会直接 401")
	}
}

// ============================================================================
// 查询
// ============================================================================

func TestJobService_ListJobItems_OrdersBySeq(t *testing.T) {
	svc, repo, _ := newJobFixture(t)
	repo.job = &dkdomain.Job{ID: 1, UID: jobTestUID, UserID: 7, ItemCount: 3}
	// 故意乱序，模拟「有人把 ORDER BY 删了」。
	repo.items = []*dkdomain.JobItem{
		{ID: 3, JobID: 1, Seq: 3},
		{ID: 1, JobID: 1, Seq: 1},
		{ID: 2, JobID: 1, Seq: 2},
	}
	repo.images[1] = []*dkdomain.Image{{ID: 12, ImageIndex: 2}, {ID: 11, ImageIndex: 1}}

	views, err := svc.ListJobItems(context.Background(), 7, jobTestUID)
	if err != nil {
		t.Fatalf("查询不该失败：%v", err)
	}
	for i, v := range views {
		if v.Item.Seq != i+1 {
			t.Fatalf("第 %d 个应该是 seq %d，实际 %d（ERP 按序号取图，顺序错了会配错商品）", i+1, i+1, v.Item.Seq)
		}
	}
	if views[0].Images[0].ImageIndex != 1 || views[0].Images[1].ImageIndex != 2 {
		t.Fatal("结果图要按 image_index 升序")
	}
}

func TestJobService_GetJob_OtherUserGetsNotFound(t *testing.T) {
	svc, repo, _ := newJobFixture(t)
	repo.job = &dkdomain.Job{ID: 1, UID: jobTestUID, UserID: 7}

	_, err := svc.GetJob(context.Background(), 999, jobTestUID)
	dkErr := jobRequireCode(t, err, dkdomain.ErrCodeJobNotFound)
	if dkErr.HTTPStatus != 404 {
		t.Fatalf("别人的任务必须是 404，不能是 403（403 会泄露「这个任务号存在」），实际 %d", dkErr.HTTPStatus)
	}
}

// ============================================================================
// 删除一批记录（软删）
// ============================================================================

func TestJobService_DeleteJob_TerminalJobIsDeleted(t *testing.T) {
	svc, repo, _ := newJobFixture(t)
	repo.job = &dkdomain.Job{ID: 1, UID: jobTestUID, UserID: 7, Status: dkdomain.JobStatusSucceeded}

	if err := svc.DeleteJob(context.Background(), 7, jobTestUID); err != nil {
		t.Fatalf("已结束的批次删除不该失败：%v", err)
	}
	if repo.softDeleteCalls != 1 {
		t.Fatalf("该调一次 SoftDeleteJob，实际 %d 次", repo.softDeleteCalls)
	}
	if repo.job.UserDeletedAt == nil {
		t.Fatal("软删之后 user_deleted_at 该有时间戳")
	}
}

func TestJobService_DeleteJob_RunningJobIsRefused(t *testing.T) {
	svc, repo, _ := newJobFixture(t)
	repo.job = &dkdomain.Job{ID: 1, UID: jobTestUID, UserID: 7, Status: dkdomain.JobStatusRunning}

	err := svc.DeleteJob(context.Background(), 7, jobTestUID)
	jobRequireCode(t, err, dkdomain.ErrCodeIllegalStateTransition)
	if repo.softDeleteCalls != 0 {
		t.Fatal("没结束的批次绝不能碰 SoftDeleteJob：删掉一个在跑的批次，图照样出、钱照样扣，而「停止排队」的按钮没了")
	}
}

func TestJobService_DeleteJob_OtherUserGetsNotFound(t *testing.T) {
	svc, repo, _ := newJobFixture(t)
	repo.job = &dkdomain.Job{ID: 1, UID: jobTestUID, UserID: 7, Status: dkdomain.JobStatusSucceeded}

	err := svc.DeleteJob(context.Background(), 999, jobTestUID)
	dkErr := jobRequireCode(t, err, dkdomain.ErrCodeJobNotFound)
	if dkErr.HTTPStatus != 404 {
		t.Fatalf("别人的任务必须是 404，不能是 403（403 会泄露「这个任务号存在」），实际 %d", dkErr.HTTPStatus)
	}
	if repo.softDeleteCalls != 0 {
		t.Fatal("归属对不上时不该碰 SoftDeleteJob")
	}
}

func TestJobService_ListJobs_ClampsLimit(t *testing.T) {
	svc, repo, _ := newJobFixture(t)

	if _, err := svc.ListJobs(context.Background(), dkdomain.ListJobsQuery{UserID: 7, Limit: 0}); err != nil {
		t.Fatalf("查询不该失败：%v", err)
	}
	if repo.listQuery.Limit != jobDefaultListLimit {
		t.Fatalf("limit 为 0 时该退回默认值 %d，实际 %d", jobDefaultListLimit, repo.listQuery.Limit)
	}
	if _, err := svc.ListJobs(context.Background(), dkdomain.ListJobsQuery{UserID: 7, Limit: 100000}); err != nil {
		t.Fatalf("查询不该失败：%v", err)
	}
	if repo.listQuery.Limit != jobMaxListLimit {
		t.Fatalf("limit 超上限时该压到 %d，实际 %d", jobMaxListLimit, repo.listQuery.Limit)
	}
}

func TestJobService_OpenJobItemContent_NotFinishedYet(t *testing.T) {
	svc, repo, _ := newJobFixture(t)
	repo.job = &dkdomain.Job{ID: 1, UID: jobTestUID, UserID: 7, ItemCount: 1}
	repo.items = []*dkdomain.JobItem{{ID: 1, JobID: 1, Seq: 1, Status: dkdomain.ItemStatusRunning}}

	_, _, err := svc.OpenJobItemContent(context.Background(), 7, jobTestUID, 1, 1)
	dkErr := jobRequireCode(t, err, dkdomain.ErrCodeImageNotFound)
	if !strings.Contains(dkErr.Message, "还没出好") {
		t.Fatalf("还在跑的时候要说「还没出好」而不是「找不到」，实际：%s", dkErr.Message)
	}
}

// ============================================================================
// 停止排队（决策 21）
// ============================================================================

// 只砍 pending，**在跑的那几张一张都不许动**。
func TestJobService_StopJob_LeavesRunningItemsAlone(t *testing.T) {
	svc, repo, _ := newJobFixture(t)
	repo.job = &dkdomain.Job{
		ID:           1,
		UID:          jobTestUID,
		UserID:       7,
		Status:       dkdomain.JobStatusRunning,
		ItemCount:    5,
		SuccessCount: 1,
	}
	repo.stopCancelled = 2

	result, err := svc.StopJob(context.Background(), 7, jobTestUID)
	if err != nil {
		t.Fatalf("停止排队不该失败：%v", err)
	}
	if repo.stopCalls != 1 {
		t.Fatalf("应该调一次 RequestJobStop，实际 %d 次", repo.stopCalls)
	}
	if result.Cancelled != 2 {
		t.Fatalf("应该砍掉 2 张，实际 %d", result.Cancelled)
	}
	// 5 张 - 1 成功 - 2 取消 = 2 张还在途
	if result.StillRunning != 2 {
		t.Fatalf("还有 2 张在途要等它自然落地，实际 %d", result.StillRunning)
	}
	for _, call := range repo.statusCalls {
		if call.to == dkdomain.JobStatusCancelled {
			t.Fatal("在途没清空之前，批次绝不能直接落 cancelled（那会跳过结算，钱对不上账）")
		}
	}
	if repo.finalizeParams != nil {
		t.Fatal("还有在途任务时不该收尾")
	}
}

// 一张在途的都没有了：必须推一把收尾，否则冻结额要占满 10 分钟等巡检。
func TestJobService_StopJob_FinalizesWhenNothingInFlight(t *testing.T) {
	svc, repo, _ := newJobFixture(t)
	repo.job = &dkdomain.Job{
		ID:           1,
		UID:          jobTestUID,
		UserID:       7,
		Status:       dkdomain.JobStatusRunning,
		ItemCount:    3,
		SuccessCount: 1,
	}
	repo.stopCancelled = 2

	result, err := svc.StopJob(context.Background(), 7, jobTestUID)
	if err != nil {
		t.Fatalf("停止排队不该失败：%v", err)
	}
	if result.StillRunning != 0 {
		t.Fatalf("应该已经没有在途任务，实际 %d", result.StillRunning)
	}
	if repo.finalizeParams == nil {
		t.Fatal("没有在途任务时必须推进收尾，否则冻结额会一直占着")
	}
	// 运营点过「停止排队」且真的砍掉了张数 → 终态记 cancelled。
	if repo.finalizeParams.Status != dkdomain.JobStatusCancelled {
		t.Fatalf("终态应该是 cancelled，实际 %s", repo.finalizeParams.Status)
	}
}

func TestJobService_StopJob_RejectsFinishedJob(t *testing.T) {
	svc, repo, _ := newJobFixture(t)
	repo.job = &dkdomain.Job{ID: 1, UID: jobTestUID, UserID: 7, Status: dkdomain.JobStatusSucceeded}

	_, err := svc.StopJob(context.Background(), 7, jobTestUID)
	jobRequireCode(t, err, dkdomain.ErrCodeIllegalStateTransition)
	if repo.stopCalls != 0 {
		t.Fatal("已经结束的任务不该再去写库")
	}
}

// ============================================================================
// 重试（决策 20）
// ============================================================================

func TestJobService_RetryJobItem_RejectsSettledJob(t *testing.T) {
	svc, repo, _ := newJobFixture(t)
	settled := jobTestTime()
	repo.job = &dkdomain.Job{
		ID:        1,
		UID:       jobTestUID,
		UserID:    7,
		Status:    dkdomain.JobStatusSucceeded,
		SettledAt: &settled,
	}

	_, err := svc.RetryJobItem(context.Background(), RetryItemInput{UserID: 7, JobUID: jobTestUID, Seq: 1})
	dkErr := jobRequireCode(t, err, dkdomain.ErrCodeJobAlreadySettled)
	if dkErr.HTTPStatus != 409 {
		t.Fatalf("已结算的批次拒绝重试要回 409，实际 %d", dkErr.HTTPStatus)
	}
	if repo.retryParams != nil {
		t.Fatal("已结算的批次不该走到仓储")
	}
}

// 重试走跟提交同一套准入：带上**单张**预估花费。
func TestJobService_RetryJobItem_PassesSingleItemEstimate(t *testing.T) {
	svc, repo, _ := newJobFixture(t)
	repo.settings[SettingKeyUnitPrices] = `{"2K":"0.042"}`
	repo.job = &dkdomain.Job{
		ID:        1,
		UID:       jobTestUID,
		UserID:    7,
		Status:    dkdomain.JobStatusPartiallyFailed,
		Ratio:     dkdomain.Ratio3x4,
		ItemCount: 4,
	}
	repo.retryItem = &dkdomain.JobItem{ID: 2, JobID: 1, Seq: 2, Status: dkdomain.ItemStatusPending}

	item, err := svc.RetryJobItem(context.Background(), RetryItemInput{
		UserID: 7, JobUID: jobTestUID, Seq: 2, IdempotencyKey: "retry-1",
	})
	if err != nil {
		t.Fatalf("重试不该失败：%v", err)
	}
	if item.Seq != 2 {
		t.Fatalf("返回的应该是第 2 张，实际第 %d 张", item.Seq)
	}
	if !repo.retryParams.EstimatedCost.Equal(dkdomain.MoneyFromFloat(0.042)) {
		t.Fatalf("重试只该预估一张的钱，实际 %s", dkdomain.MoneyString(repo.retryParams.EstimatedCost))
	}
	if repo.retryParams.IdempotencyKey == nil || *repo.retryParams.IdempotencyKey != "retry-1" {
		t.Fatalf("防重复标识要透传下去，实际 %v", repo.retryParams.IdempotencyKey)
	}
}

func TestJobService_RetryJobItem_InsufficientBalanceIsChinese(t *testing.T) {
	svc, repo, _ := newJobFixture(t)
	repo.job = &dkdomain.Job{ID: 1, UID: jobTestUID, UserID: 7, Status: dkdomain.JobStatusPartiallyFailed}
	repo.retryErr = &dkdomain.InsufficientBalanceError{
		Required:  dkdomain.MoneyFromFloat(0.042),
		Available: dkdomain.MoneyFromFloat(0.01),
		Shortfall: dkdomain.MoneyFromFloat(0.032),
	}

	_, err := svc.RetryJobItem(context.Background(), RetryItemInput{UserID: 7, JobUID: jobTestUID, Seq: 1})
	dkErr := jobRequireCode(t, err, dkdomain.ErrCodeInsufficientBalance)
	if !strings.Contains(dkErr.Message, "$0.0320") {
		t.Fatalf("要说清还差多少，实际：%s", dkErr.Message)
	}
}

// 仓储造好的中文错误（比如「重试到上限」）必须原样透传，不能被包成 DK_INTERNAL。
func TestJobService_RetryJobItem_KeepsRepoErrorCode(t *testing.T) {
	svc, repo, _ := newJobFixture(t)
	repo.job = &dkdomain.Job{ID: 1, UID: jobTestUID, UserID: 7, Status: dkdomain.JobStatusPartiallyFailed}
	repo.retryErr = dkdomain.NewError(dkdomain.ErrCodeMaxAttemptsExceeded).WithCause(dkdomain.ErrConflict)

	_, err := svc.RetryJobItem(context.Background(), RetryItemInput{UserID: 7, JobUID: jobTestUID, Seq: 1})
	jobRequireCode(t, err, dkdomain.ErrCodeMaxAttemptsExceeded)
}

// ============================================================================
// 装配
// ============================================================================

func TestNewJobService_RequiresRepo(t *testing.T) {
	if _, err := NewJobService(JobServiceDeps{}); err == nil {
		t.Fatal("没有持久化层时必须报错，不能留到运行时 nil panic")
	}
}
