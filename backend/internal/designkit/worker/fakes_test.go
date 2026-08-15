//go:build unit

package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"sort"
	"sync"
	"time"

	dkdomain "github.com/Wei-Shaw/sub2api/internal/designkit/domain"
)

// ============================================================================
// 测试替身
// ============================================================================
//
// fakeRepo **刻意照着真实 SQL 的语义写**，不是随便糊一个能过测试的东西：
//
//	ClaimNextItem        → FOR UPDATE SKIP LOCKED + 僵尸回收写在同一个 WHERE，
//	                       attempt_count 在领取那一刻就 +1，
//	                       **pending 支带 attempt_count < max_attempts，僵尸支不带**
//	CompleteItem*        → WHERE id=? AND worker_id=? AND status='running'，
//	                       影响行数不等于 1 就返回 ErrLeaseLost
//	FinalizeJob          → WHERE finished_at IS NULL
//	                       AND success+fail+cancelled = item_count
//	ApplyItemBilling     → billed_cost **只增不减 + 幂等**，冻结额按「新值 - 旧值」
//	                       递减且 GREATEST(..., 0) 兜底
//
// 这样这几个测试守的就不只是「worker 代码没崩」，还有「repository 必须实现成这样」。
// repository 的 SQL 写歪了，同名的集成测试会在真数据库上炸。

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newFakeClock(t time.Time) *fakeClock { return &fakeClock{t: t} }

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// ----------------------------------------------------------------------------

type fakeRepo struct {
	mu  sync.Mutex
	now func() time.Time

	jobs     map[int64]*dkdomain.Job
	items    map[int64]*dkdomain.JobItem
	images   map[int64][]*dkdomain.Image
	assets   map[int64]*dkdomain.Asset
	variants map[string]*dkdomain.AssetVariant
	settings map[string]json.RawMessage

	// holds 模拟 designkit_holds：job_id → 还冻着多少（只记 status='open' 那些）。
	// 可用额 = users.balance - SUM(open holds)，所以这个数被放大就等于凭空放大可用额。
	holds map[int64]dkdomain.Money

	// usageLogs 模拟上游 usage_logs：key 是带 client: 前缀的 request_id。
	usageLogs map[string]dkdomain.Money

	// 注入的错误。
	claimErr      error
	successErr    error
	failureErr    error
	renewErr      error
	usageLogsErr  error
	settleJobErr  error
	staleJobIDs   []int64
	staleJobsErr  error
	settlingQueue []*dkdomain.Job

	// 记录下来给断言用。
	heartbeats      [][]int64
	billed          []billingApplication
	settled         []dkdomain.SettleJobParams
	alerts          []*dkdomain.BillingAlert
	finalizeWon     int
	finalizeCalls   int
	statusUpdates   []statusUpdate
	reapCalls       int
	usageQueries    int
	settleListCalls int
}

type billingApplication struct {
	itemID int64
	cost   dkdomain.Money
	mode   string
	tier   string
}

type statusUpdate struct {
	jobID    int64
	from, to dkdomain.JobStatus
}

func newFakeRepo(now func() time.Time) *fakeRepo {
	return &fakeRepo{
		now:       now,
		jobs:      map[int64]*dkdomain.Job{},
		items:     map[int64]*dkdomain.JobItem{},
		images:    map[int64][]*dkdomain.Image{},
		assets:    map[int64]*dkdomain.Asset{},
		variants:  map[string]*dkdomain.AssetVariant{},
		settings:  map[string]json.RawMessage{},
		usageLogs: map[string]dkdomain.Money{},
		holds:     map[int64]dkdomain.Money{},
	}
}

// 编译期断言：假实现必须满足 worker 要的全部接口。
var _ Repository = (*fakeRepo)(nil)

func variantKey(assetID int64, ratio dkdomain.Ratio, keep bool, maxDim int) string {
	return fmt.Sprintf("%d|%s|%t|%d", assetID, ratio, keep, maxDim)
}

func (r *fakeRepo) GetJobByID(_ context.Context, id int64) (*dkdomain.Job, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	job, ok := r.jobs[id]
	if !ok {
		return nil, dkdomain.ErrNotFound
	}
	clone := *job
	return &clone, nil
}

func (r *fakeRepo) UpdateJobStatus(_ context.Context, jobID int64, from, to dkdomain.JobStatus) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.statusUpdates = append(r.statusUpdates, statusUpdate{jobID: jobID, from: from, to: to})
	job, ok := r.jobs[jobID]
	if !ok {
		return dkdomain.ErrNotFound
	}
	if job.Status != from { // WHERE status = from 没命中
		return dkdomain.ErrConflict
	}
	job.Status = to
	return nil
}

// ClaimNextItem 模拟设计定型 第六节那条 SQL。
func (r *fakeRepo) ClaimNextItem(_ context.Context, params dkdomain.ClaimItemParams) (*dkdomain.JobItem, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.claimErr != nil {
		return nil, r.claimErr
	}
	now := r.now()

	for _, item := range r.sortedItemsLocked() {
		// pending 支带 `attempt_count < max_attempts`（真 SQL 里有，别删）：
		// 万一哪天有人写出「把试满的 item 退回 pending」的代码，这条守卫让它
		// 领不出来，也就花不掉钱。**用裸比较，不做 normalize —— 真 SQL 就是裸的。**
		pending := item.Status == dkdomain.ItemStatusPending &&
			!item.AvailableAt.After(now) &&
			item.AttemptCount < item.MaxAttempts
		// 僵尸支**故意不判 attempt**：不判才能把崩溃残留的 running 行捞回来判死。
		// 拦截点在 worker（领到手第一件事判上限、不调网关）。
		zombie := item.Status == dkdomain.ItemStatusRunning &&
			item.LeaseExpiresAt != nil && item.LeaseExpiresAt.Before(now)
		if !pending && !zombie {
			continue
		}

		lease := params.LeaseFor
		if lease <= 0 {
			lease = time.Duration(dkdomain.DefaultLeaseSeconds) * time.Second
		}
		workerID := params.WorkerID
		expires := now.Add(lease)

		item.Status = dkdomain.ItemStatusRunning
		item.WorkerID = &workerID
		item.LeaseExpiresAt = &expires
		item.AttemptCount++ // **领取时就 +1**，不是成功后才加
		if item.StartedAt == nil {
			started := now
			item.StartedAt = &started
		}
		clone := *item
		return &clone, nil
	}
	return nil, dkdomain.ErrNotFound
}

func (r *fakeRepo) sortedItemsLocked() []*dkdomain.JobItem {
	out := make([]*dkdomain.JobItem, 0, len(r.items))
	for _, item := range r.items {
		out = append(out, item)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].JobID != out[j].JobID {
			return out[i].JobID < out[j].JobID
		}
		return out[i].Seq < out[j].Seq
	})
	return out
}

func (r *fakeRepo) RenewItemLease(_ context.Context, itemID int64, workerID string, leaseFor time.Duration) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.renewErr != nil {
		return r.renewErr
	}
	item, ok := r.items[itemID]
	if !ok || item.Status != dkdomain.ItemStatusRunning || item.WorkerID == nil || *item.WorkerID != workerID {
		return dkdomain.ErrLeaseLost
	}
	expires := r.now().Add(leaseFor)
	item.LeaseExpiresAt = &expires
	return nil
}

func (r *fakeRepo) CompleteItemSuccess(_ context.Context, params dkdomain.CompleteItemSuccessParams) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.successErr != nil {
		return r.successErr
	}
	item, ok := r.items[params.ItemID]
	if !ok || item.Status != dkdomain.ItemStatusRunning || item.WorkerID == nil || *item.WorkerID != params.WorkerID {
		return dkdomain.ErrLeaseLost
	}
	item.Status = dkdomain.ItemStatusSucceeded
	finished := params.FinishedAt
	item.FinishedAt = &finished
	requestID := params.BillingRequestID
	item.BillingRequestID = &requestID
	item.WorkerID = nil
	item.LeaseExpiresAt = nil

	for _, img := range params.Images {
		r.images[item.ID] = append(r.images[item.ID], &dkdomain.Image{
			UID:         img.UID,
			JobID:       item.JobID,
			ItemID:      item.ID,
			Attempt:     params.Attempt,
			ImageIndex:  img.ImageIndex,
			IsCurrent:   true,
			ObjectKey:   img.ObjectKey,
			ContentType: img.ContentType,
			ByteSize:    img.ByteSize,
			Width:       img.Width,
			Height:      img.Height,
			BillingTier: img.BillingTier,
			CreatedAt:   params.FinishedAt,
		})
	}
	if job, ok := r.jobs[item.JobID]; ok {
		job.SuccessCount++ // SET x = x + 1
	}
	return nil
}

func (r *fakeRepo) CompleteItemFailure(_ context.Context, params dkdomain.CompleteItemFailureParams) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.failureErr != nil {
		return r.failureErr
	}
	item, ok := r.items[params.ItemID]
	if !ok || item.Status != dkdomain.ItemStatusRunning || item.WorkerID == nil || *item.WorkerID != params.WorkerID {
		return dkdomain.ErrLeaseLost
	}
	code := params.ErrorCode
	msg := params.ErrorMessage
	item.LastErrorCode = &code
	item.LastErrorMessage = &msg
	item.WorkerID = nil
	item.LeaseExpiresAt = nil

	if params.Terminal {
		item.Status = dkdomain.ItemStatusFailed
		finished := params.FinishedAt
		item.FinishedAt = &finished
		if job, ok := r.jobs[item.JobID]; ok {
			job.FailCount++
		}
		return nil
	}
	item.Status = dkdomain.ItemStatusPending
	item.AvailableAt = r.now().Add(params.RetryAfter)
	return nil
}

func (r *fakeRepo) FinalizeJob(_ context.Context, params dkdomain.FinalizeJobParams) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.finalizeCalls++
	job, ok := r.jobs[params.JobID]
	if !ok {
		return false, dkdomain.ErrNotFound
	}
	if job.FinishedAt != nil {
		return false, nil
	}
	if job.SuccessCount+job.FailCount+job.CancelledCount != job.ItemCount {
		return false, nil
	}
	finished := params.FinishedAt
	job.FinishedAt = &finished
	job.Status = params.Status
	r.finalizeWon++
	return true, nil
}

func (r *fakeRepo) TouchJobHeartbeat(_ context.Context, jobIDs []int64) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	cp := append([]int64(nil), jobIDs...)
	r.heartbeats = append(r.heartbeats, cp)
	now := r.now()
	for _, id := range jobIDs {
		if job, ok := r.jobs[id]; ok {
			t := now
			job.HeartbeatAt = &t
		}
	}
	return nil
}

func (r *fakeRepo) GetAssetsByIDs(_ context.Context, userID int64, ids []int64) ([]*dkdomain.Asset, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]*dkdomain.Asset, 0, len(ids))
	for _, id := range ids { // 顺序必须跟入参一致
		asset, ok := r.assets[id]
		if !ok || asset.UserID != userID {
			continue
		}
		clone := *asset
		out = append(out, &clone)
	}
	return out, nil
}

func (r *fakeRepo) GetVariant(_ context.Context, assetID int64, ratio dkdomain.Ratio, keep bool, maxDim int) (*dkdomain.AssetVariant, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	v, ok := r.variants[variantKey(assetID, ratio, keep, maxDim)]
	if !ok {
		return nil, dkdomain.ErrNotFound
	}
	clone := *v
	return &clone, nil
}

func (r *fakeRepo) UpsertVariant(_ context.Context, params dkdomain.UpsertVariantParams) (*dkdomain.AssetVariant, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	key := variantKey(params.AssetID, params.Ratio, params.KeepTransparency, params.MaxDimension)
	v := &dkdomain.AssetVariant{
		ID:               int64(len(r.variants) + 1),
		AssetID:          params.AssetID,
		Ratio:            params.Ratio,
		KeepTransparency: params.KeepTransparency,
		MaxDimension:     params.MaxDimension,
		ObjectKey:        params.ObjectKey,
		Width:            params.Width,
		Height:           params.Height,
		ContentType:      params.ContentType,
		CreatedAt:        r.now(),
	}
	r.variants[key] = v
	clone := *v
	return &clone, nil
}

func (r *fakeRepo) GetSetting(_ context.Context, key string) (*dkdomain.Setting, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	raw, ok := r.settings[key]
	if !ok {
		return nil, dkdomain.ErrNotFound
	}
	return &dkdomain.Setting{Key: key, Value: raw, UpdatedAt: r.now()}, nil
}

func (r *fakeRepo) ReapStaleJobs(_ context.Context, _ time.Duration, _ int) ([]int64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.reapCalls++
	if r.staleJobsErr != nil {
		return nil, r.staleJobsErr
	}
	// 真 SQL 在同一个事务里把这些 job 的 open 冻结额置 released（amount = 0）。
	for _, id := range r.staleJobIDs {
		if _, ok := r.holds[id]; ok {
			r.holds[id] = dkdomain.ZeroMoney
		}
	}
	return append([]int64(nil), r.staleJobIDs...), nil
}

func (r *fakeRepo) ListJobsPendingSettlement(_ context.Context, limit int) ([]*dkdomain.Job, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.settleListCalls++
	out := make([]*dkdomain.Job, 0, len(r.settlingQueue))
	for i, job := range r.settlingQueue {
		if i >= limit {
			break
		}
		clone := *job
		out = append(out, &clone)
	}
	return out, nil
}

func (r *fakeRepo) ListJobItems(_ context.Context, jobID int64) ([]*dkdomain.JobItem, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]*dkdomain.JobItem, 0)
	for _, item := range r.sortedItemsLocked() { // ORDER BY seq ASC
		if item.JobID != jobID {
			continue
		}
		clone := *item
		out = append(out, &clone)
	}
	return out, nil
}

func (r *fakeRepo) ListImagesByItem(_ context.Context, itemID int64, _ bool) ([]*dkdomain.Image, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]*dkdomain.Image(nil), r.images[itemID]...), nil
}

func (r *fakeRepo) SumUsageLogCosts(_ context.Context, ids []string) (map[string]dkdomain.Money, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.usageQueries++
	if r.usageLogsErr != nil {
		return nil, r.usageLogsErr
	}
	out := map[string]dkdomain.Money{}
	for _, id := range ids {
		if cost, ok := r.usageLogs[id]; ok {
			out[id] = cost // 查不到的 key 直接不出现
		}
	}
	return out, nil
}

// ApplyItemBilling 照抄 repository/billing_repo.go 的两条守卫。
//
// 真 SQL 是「读旧值 FOR UPDATE → 只增不减 → 按（新值 − 旧值）减冻结额
// → GREATEST(amount − 差额, 0)」。原来这个假实现直接覆写 billed_cost、
// 也不碰冻结额，于是「金额变小」这条最贵的路径在测试里根本走不到：
// 负差额会让 GREATEST(amount − 负数, 0) = amount + |差额|，**把冻结额加回去**，
// 凭空放大用户可用额，接着就能透支。
func (r *fakeRepo) ApplyItemBilling(_ context.Context, itemID int64, cost dkdomain.Money, mode, tier string) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	item, ok := r.items[itemID]
	if !ok {
		return dkdomain.ErrNotFound
	}
	newCost := dkdomain.QuantizeMoney(cost)
	oldCost := dkdomain.MoneyOrZero(item.BilledCost)

	// 只增不减 + 幂等：已经回填过、而且新值没有更大 → 整条跳过，
	// billing_mode / billing_tier 也不覆盖（真 SQL 就是整条 return nil）。
	if item.BilledCost != nil && !newCost.GreaterThan(oldCost) {
		return nil
	}

	r.billed = append(r.billed, billingApplication{itemID: itemID, cost: newCost, mode: mode, tier: tier})
	c := newCost
	item.BilledCost = &c
	item.BillingMode = &mode
	item.BillingTier = &tier

	// 按差额递减冻结额。差额可能是 0（第一次回填、而上游就是按 0 元计的费），
	// 那种情况不动冻结额；负数走不到这里，但照真 SQL 再判一次 ——
	// 这一行是「凭空放大可用额」的最后一道闸。
	delta := dkdomain.QuantizeMoney(newCost.Sub(oldCost))
	if delta.Sign() <= 0 {
		return nil
	}
	if held, ok := r.holds[item.JobID]; ok {
		next := dkdomain.QuantizeMoney(held.Sub(delta))
		if next.IsNegative() { // GREATEST(amount - $2, 0)
			next = dkdomain.ZeroMoney
		}
		r.holds[item.JobID] = next
	}
	return nil
}

func (r *fakeRepo) SettleJob(_ context.Context, params dkdomain.SettleJobParams) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.settleJobErr != nil {
		return r.settleJobErr
	}
	r.settled = append(r.settled, params)
	if job, ok := r.jobs[params.JobID]; ok {
		job.Status = params.Status
		cost := params.ActualCost
		job.ActualCost = &cost
		settled := params.SettledAt
		job.SettledAt = &settled
	}
	// 真 SQL 在同一个事务里 releaseHold：剩余的 open 冻结额置 released。
	if _, ok := r.holds[params.JobID]; ok {
		r.holds[params.JobID] = dkdomain.ZeroMoney
	}
	// 结算完成后就不该再出现在待结算队列里。
	remaining := r.settlingQueue[:0]
	for _, job := range r.settlingQueue {
		if job.ID != params.JobID {
			remaining = append(remaining, job)
		}
	}
	r.settlingQueue = remaining
	return nil
}

func (r *fakeRepo) InsertBillingAlert(_ context.Context, alert *dkdomain.BillingAlert) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.alerts = append(r.alerts, alert)
	return nil
}

// ---- 断言用的读方法 ----

func (r *fakeRepo) item(id int64) dkdomain.JobItem {
	r.mu.Lock()
	defer r.mu.Unlock()
	return *r.items[id]
}

func (r *fakeRepo) job(id int64) dkdomain.Job {
	r.mu.Lock()
	defer r.mu.Unlock()
	return *r.jobs[id]
}

// hold 返回这个批次还冻着多少（没有冻结行时返回 0）。
func (r *fakeRepo) hold(jobID int64) dkdomain.Money {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.holds[jobID]
}

func (r *fakeRepo) imageCount(itemID int64) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.images[itemID])
}

func (r *fakeRepo) allItemsTerminal() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, item := range r.items {
		if !item.Status.IsTerminal() {
			return false
		}
	}
	return true
}

// ----------------------------------------------------------------------------

type storedObject struct {
	data        []byte
	contentType string
}

type fakeStore struct {
	mu      sync.Mutex
	objects map[string]storedObject
	putErr  error
	puts    int
}

func newFakeStore() *fakeStore {
	return &fakeStore{objects: map[string]storedObject{}}
}

var _ dkdomain.ObjectStore = (*fakeStore)(nil)

func (s *fakeStore) Put(_ context.Context, key, contentType string, data []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.puts++
	if s.putErr != nil {
		return s.putErr
	}
	s.objects[key] = storedObject{data: append([]byte(nil), data...), contentType: contentType}
	return nil
}

func (s *fakeStore) Get(_ context.Context, key string) ([]byte, string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	obj, ok := s.objects[key]
	if !ok {
		return nil, "", dkdomain.ErrObjectNotFound
	}
	return append([]byte(nil), obj.data...), obj.contentType, nil
}

func (s *fakeStore) Delete(_ context.Context, key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.objects, key)
	return nil
}

func (s *fakeStore) Exists(_ context.Context, key string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.objects[key]
	return ok, nil
}

func (s *fakeStore) has(key string) bool {
	ok, _ := s.Exists(context.Background(), key)
	return ok
}

// ----------------------------------------------------------------------------

type fakePreprocessor struct {
	mu    sync.Mutex
	calls int
	err   error
}

var _ dkdomain.ImagePreprocessor = (*fakePreprocessor)(nil)

func (p *fakePreprocessor) Preprocess(_ context.Context, req dkdomain.PreprocessRequest) (*dkdomain.PreprocessResult, error) {
	p.mu.Lock()
	p.calls++
	err := p.err
	p.mu.Unlock()
	if err != nil {
		return nil, err
	}
	w, h := targetSize(req.Ratio, req.MaxDimension)
	return &dkdomain.PreprocessResult{
		Data:        append([]byte("padded:"), req.Data...),
		ContentType: "image/png",
		Width:       w,
		Height:      h,
		Changed:     true,
		Actions:     []string{"pad"},
	}, nil
}

func (p *fakePreprocessor) Health(context.Context) error { return nil }

func (p *fakePreprocessor) callCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls
}

// ----------------------------------------------------------------------------

type fakeGateway struct {
	mu       sync.Mutex
	requests []dkdomain.GenerateRequest
	// handler 可换，默认返回一张 8x8 的假 PNG。
	handler func(ctx context.Context, req dkdomain.GenerateRequest) (dkdomain.GenerateResult, error)
}

var _ dkdomain.GatewayInvoker = (*fakeGateway)(nil)

func (g *fakeGateway) Generate(ctx context.Context, req dkdomain.GenerateRequest) (dkdomain.GenerateResult, error) {
	g.mu.Lock()
	g.requests = append(g.requests, req)
	handler := g.handler
	g.mu.Unlock()

	if handler != nil {
		return handler(ctx, req)
	}
	return dkdomain.GenerateResult{
		Images: []dkdomain.GeneratedImage{{
			Data:        []byte("PNG-BYTES"),
			ContentType: "image/png",
			Width:       1254,
			Height:      1254,
		}},
		UpstreamStatus:         200,
		StoredBillingRequestID: dkdomain.UpstreamClientRequestIDPrefix + req.BillingRequestID,
	}, nil
}

func (g *fakeGateway) callCount() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.requests)
}

func (g *fakeGateway) billingRequestIDs() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	out := make([]string, 0, len(g.requests))
	for _, req := range g.requests {
		out = append(out, req.BillingRequestID)
	}
	return out
}

// ----------------------------------------------------------------------------

type fakeLocker struct {
	mu       sync.Mutex
	granted  int
	refused  int
	allow    bool
	err      error
	unlocked int
}

var _ AdvisoryLocker = (*fakeLocker)(nil)

func (l *fakeLocker) TryAdvisoryLock(_ context.Context, _ int64) (bool, func(), error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.err != nil {
		return false, nil, l.err
	}
	if !l.allow {
		l.refused++
		return false, nil, nil
	}
	l.granted++
	return true, func() {
		l.mu.Lock()
		l.unlocked++
		l.mu.Unlock()
	}, nil
}

// ----------------------------------------------------------------------------

// discardLogger 测试里不要往终端刷日志。
func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError + 1}))
}

// ----------------------------------------------------------------------------
// 造数据
// ----------------------------------------------------------------------------

const testJobUID = "01J8ZK7Q9X2M4N6P8R0T2V4W6Y"

type fixture struct {
	repo  *fakeRepo
	store *fakeStore
	pre   *fakePreprocessor
	gw    *fakeGateway
	clock *fakeClock
	pool  *Pool
}

// newFixture 造一个「1 个批次 + n 张图 + 每张一张原图」的场景。
func newFixture(t testingT, itemCount int, mutate func(cfg *Config, deps *Deps)) *fixture {
	clock := newFakeClock(time.Date(2026, 8, 13, 10, 0, 0, 0, time.UTC))
	repo := newFakeRepo(clock.Now)
	store := newFakeStore()
	pre := &fakePreprocessor{}
	gw := &fakeGateway{}

	job := &dkdomain.Job{
		ID:            1,
		UID:           testJobUID,
		UserID:        7,
		APIKeyID:      99,
		Origin:        dkdomain.OriginWeb,
		Status:        dkdomain.JobStatusHolding,
		Ratio:         dkdomain.Ratio3x4,
		Model:         dkdomain.DefaultModel,
		ItemCount:     itemCount,
		Currency:      dkdomain.CurrencyUSD,
		EstimatedCost: dkdomain.MoneyFromFloat(0.5),
		CreatedAt:     clock.Now(),
	}
	repo.jobs[job.ID] = job
	// 提交时冻结的就是预估额。冻结额是「可用额 = balance - SUM(open holds)」里
	// 唯一会动的那一项，所以它被放大 = 用户凭空多出可用额 = 能透支。
	repo.holds[job.ID] = job.EstimatedCost

	for i := 1; i <= itemCount; i++ {
		assetID := int64(100 + i)
		repo.assets[assetID] = &dkdomain.Asset{
			ID:          assetID,
			UID:         fmt.Sprintf("01J8ZK7Q9X2M4N6P8R0T2V4W%02d", i),
			UserID:      job.UserID,
			ObjectKey:   fmt.Sprintf("designkit/assets/2026/08/13/asset-%d.jpg", i),
			ContentType: "image/jpeg",
			ByteSize:    12,
			Origin:      dkdomain.OriginWeb,
			CreatedAt:   clock.Now(),
		}
		store.objects[repo.assets[assetID].ObjectKey] = storedObject{
			data:        []byte("RAW-JPEG"),
			contentType: "image/jpeg",
		}
		aid := assetID
		repo.items[int64(i)] = &dkdomain.JobItem{
			ID:          int64(i),
			JobID:       job.ID,
			Seq:         i,
			AssetID:     &aid,
			PromptText:  fmt.Sprintf("白底商品图 %d", i),
			Status:      dkdomain.ItemStatusPending,
			MaxAttempts: dkdomain.DefaultMaxAttempts,
			AvailableAt: clock.Now(),
			CreatedAt:   clock.Now(),
		}
	}

	cfg := Config{
		Concurrency:        2,
		ItemTimeout:        2 * time.Second,
		MaxDimension:       dkdomain.DefaultMaxDimension,
		LeaseFor:           3 * time.Second,
		LeaseRenewInterval: time.Second,
		HeartbeatInterval:  20 * time.Millisecond,
		IdleDelay:          5 * time.Millisecond,
		ErrorDelay:         5 * time.Millisecond,
		WriteBackTimeout:   time.Second,
		ShutdownWait:       2 * time.Second,
		RetryBackoffBase:   10 * time.Millisecond,
		SettleInterval:     10 * time.Millisecond,
		SettleBackoffBase:  time.Millisecond,
		DisableReaper:      true,
		DisableSettlement:  true,
	}
	deps := Deps{
		Repo:         repo,
		Store:        store,
		Preprocessor: pre,
		Gateway:      gw,
		Now:          clock.Now,
		Logger:       discardLogger(),
		NewUID:       NewULID,
	}
	if mutate != nil {
		mutate(&cfg, &deps)
	}

	pool, err := New(cfg, deps)
	if err != nil {
		t.Fatalf("New 失败: %v", err)
	}
	// 直接调 handleItem / settler 的测试不会走 Start，这里先把配置快照填好。
	pool.settings = Settings{
		Concurrency:  cfg.Concurrency,
		ItemTimeout:  cfg.ItemTimeout,
		MaxDimension: cfg.MaxDimension,
	}

	return &fixture{repo: repo, store: store, pre: pre, gw: gw, clock: clock, pool: pool}
}

// testingT 只用到 Fatalf，单独抽出来免得 fixture 依赖 *testing.T 的全部方法。
type testingT interface {
	Fatalf(format string, args ...any)
}
