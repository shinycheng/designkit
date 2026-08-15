package repository

// 批次（designkit_jobs）、单张图（designkit_job_items）、结果图（designkit_images）的读写。
//
// 这个文件里有三处「错一次就是钱」的地方，改之前先读注释：
//  1. CreateJobWithItemsAndHold 里那条 `SELECT balance ... FOR UPDATE`
//     ——它是**全部**的并发防护（CLAUDE.md B4：上游准入不预留额度，
//     50 张并发会全部放行看到同一个未扣减余额，最后把余额打成负数）；
//  2. ClaimNextItem 的 `FOR UPDATE SKIP LOCKED` + 领取时 attempt_count 就 +1；
//  3. FinalizeJob 那条带守卫的 UPDATE ——只有 RowsAffected=1 的 worker 去结算
//     和发回调，否则 5 个 worker 都以为自己是最后一张，**回调发 5 次**。

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/lib/pq"

	dkdomain "github.com/Wei-Shaw/sub2api/internal/designkit/domain"
)

// idempotencyLookupWindow 幂等复用的时间窗，跟上游幂等协调器的 24 小时对齐。
//
// ⚠ 为什么必须有窗口：designkit_jobs 上**没有** (user_id, idempotency_key)
// 唯一索引（9001 迁移里写死的，写 ON CONFLICT 会报 42P10）。这里改用
// 「同一个用户行 FOR UPDATE 之后再查一次」来防重复下单。
// 但查询不加窗口就等于把那个被否掉的唯一索引又装回来了：
// 24 小时后 ERP 复用同一个 key 下新单，会被错认成老单，**直接不出图**。
const idempotencyLookupWindow = "24 hours"

// ============================================================================
// 提交一个批次
// ============================================================================

func (r *designkitRepository) CreateJobWithItemsAndHold(ctx context.Context, params dkdomain.CreateJobParams) (*dkdomain.CreateJobResult, error) {
	if err := validateCreateJobParams(params); err != nil {
		return nil, err
	}

	var result *dkdomain.CreateJobResult
	err := r.withTx(ctx, func(tx *sql.Tx) error {
		// 第 1 步：锁住用户那一行。**顺序不能反**，这条 FOR UPDATE 一旦拿到，
		// 同一个用户的其它提交就得排队，可用额不会被两笔并发同时算成够。
		balance, err := lockUserBalance(ctx, tx, params.UserID)
		if err != nil {
			return err
		}

		// 第 2 步：命中幂等就直接返已有批次，什么都不建、也不再冻结一次。
		// 放在拿到用户行锁之后，运营手滑双击的两笔请求才是串行的。
		if key := trimmedPtr(params.IdempotencyKey); key != "" {
			existing, err := findJobByIdempotencyKey(ctx, tx, params.UserID, key)
			if err != nil {
				return err
			}
			if existing != nil {
				items, err := listJobItems(ctx, tx, existing.ID)
				if err != nil {
					return err
				}
				result = &dkdomain.CreateJobResult{Job: existing, Items: items, Reused: true}
				return nil
			}
		}

		// 第 3 步：可用额 = 余额 - 已冻结（open）。不够就整个事务回滚，什么都不建。
		held, err := sumOpenHolds(ctx, tx, params.UserID)
		if err != nil {
			return err
		}
		estimated := dkdomain.QuantizeMoney(params.EstimatedCost)
		available := dkdomain.QuantizeMoney(balance.Sub(held))
		if available.LessThan(estimated) {
			return newInsufficientBalance(estimated, available)
		}

		// 第 4 步：建批次。status 用 created —— 状态机的入口，
		// 由 service 走 UpdateJobStatus(created→holding→running) 往下推。
		// heartbeat_at 在这里就先打一次，否则进程刚建完就被杀，
		// 僵尸巡检拿 NULL 心跳判不了死，这批的冻结额会永远占着。
		job, err := insertJob(ctx, tx, params, estimated)
		if err != nil {
			return err
		}

		// 第 5 步：建每一张。**按传进来的顺序插**（商品图外层、提示词内层）。
		if err := insertJobItems(ctx, tx, job.ID, params.Items); err != nil {
			return err
		}
		items, err := listJobItems(ctx, tx, job.ID)
		if err != nil {
			return err
		}

		// 第 6 步：记冻结。一个 job 一行（job_id 上有 UNIQUE）。
		if err := insertHold(ctx, tx, job.ID, params.UserID, estimated); err != nil {
			return err
		}

		result = &dkdomain.CreateJobResult{Job: job, Items: items, Reused: false}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

func validateCreateJobParams(params dkdomain.CreateJobParams) error {
	if params.UserID <= 0 {
		return fmt.Errorf("designkit: 提交批次缺少 user_id: %w", dkdomain.ErrNotFound)
	}
	if params.APIKeyID <= 0 {
		// api_key_id 是 NOT NULL：出图走上游 Images()，取不到 Key 直接 401。
		return dkdomain.NewError(dkdomain.ErrCodeAPIKeyMissing)
	}
	if len(params.Items) == 0 {
		return dkdomain.NewError(dkdomain.ErrCodeInvalidRequest).
			WithMessage("这一批一张图都没有，请先选商品图和提示词。")
	}
	for i, item := range params.Items {
		if item.Seq != i+1 {
			// seq 必须是从 1 开始的连续序号：对外契约「第 N 张」直接按它取，
			// 也是 request_id 的组成部分。
			return dkdomain.NewError(dkdomain.ErrCodeInvalidRequest).
				WithMessagef("批次内序号不连续（第 %d 个的 seq 是 %d），请重新提交。", i+1, item.Seq)
		}
	}
	return nil
}

// lockUserBalance 锁住用户行并取余额。用户不存在或已注销返回 ErrNotFound。
func lockUserBalance(ctx context.Context, tx dkExecutor, userID int64) (dkdomain.Money, error) {
	var balance dkdomain.Money
	err := tx.QueryRowContext(ctx,
		`SELECT balance FROM users WHERE id = $1 AND deleted_at IS NULL FOR UPDATE`,
		userID).Scan(&balance)
	if err != nil {
		return dkdomain.ZeroMoney, translate(err, "用户")
	}
	return balance, nil
}

// sumOpenHolds 算这个用户还冻着多少。
func sumOpenHolds(ctx context.Context, tx dkExecutor, userID int64) (dkdomain.Money, error) {
	var held dkdomain.Money
	err := tx.QueryRowContext(ctx,
		`SELECT COALESCE(SUM(amount), 0) FROM designkit_holds WHERE user_id = $1 AND status = 'open'`,
		userID).Scan(&held)
	if err != nil {
		return dkdomain.ZeroMoney, err
	}
	return held, nil
}

// newInsufficientBalance 拼「还差多少」。界面要把这个数显示出来（决策 19）。
func newInsufficientBalance(required, available dkdomain.Money) error {
	shortfall := required.Sub(available)
	if shortfall.IsNegative() {
		shortfall = dkdomain.ZeroMoney
	}
	return &dkdomain.InsufficientBalanceError{
		Required:  required,
		Available: available,
		Shortfall: dkdomain.QuantizeMoney(shortfall),
	}
}

func findJobByIdempotencyKey(ctx context.Context, tx dkExecutor, userID int64, key string) (*dkdomain.Job, error) {
	job, err := scanJob(tx.QueryRowContext(ctx,
		`SELECT `+jobColumns+` FROM designkit_jobs
		 WHERE user_id = $1 AND idempotency_key = $2
		   AND created_at > NOW() - INTERVAL '`+idempotencyLookupWindow+`'
		 ORDER BY id DESC LIMIT 1`, userID, key))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return job, nil
}

func insertJob(ctx context.Context, tx dkExecutor, params dkdomain.CreateJobParams, estimated dkdomain.Money) (*dkdomain.Job, error) {
	origin := params.Origin
	if origin == "" {
		origin = dkdomain.OriginWeb
	}
	job, err := scanJob(tx.QueryRowContext(ctx, `
INSERT INTO designkit_jobs (
  uid, user_id, api_key_id, origin, name, status, ratio, model, item_count,
  idempotency_key, callback_url, currency, estimated_cost,
  base_unit_price, group_rate_multiplier, account_rate_multiplier,
  billing_mode, pricing_tier, price_source, pricing_snapshot_version,
  heartbeat_at
) VALUES (
  $1, $2, $3, $4, $5, $6, $7, $8, $9,
  $10, $11, $12, $13,
  $14, $15, $16,
  $17, $18, $19, $20,
  NOW()
) RETURNING `+jobColumns,
		params.JobUID,
		params.UserID,
		params.APIKeyID,
		origin.String(),
		params.Name,
		dkdomain.JobStatusCreated.String(),
		params.Ratio.String(),
		params.Model,
		len(params.Items),
		nullableStringPtr(params.IdempotencyKey),
		nullableStringPtr(params.CallbackURL),
		dkdomain.CurrencyUSD,
		estimated,
		dkdomain.QuantizeMoney(params.Pricing.BaseUnitPrice),
		params.Pricing.GroupRateMultiplier,
		params.Pricing.AccountRateMultiplier,
		params.Pricing.BillingMode,
		params.Pricing.PricingTier,
		params.Pricing.PriceSource,
		params.Pricing.Version,
	))
	if err != nil {
		return nil, translate(err, "任务")
	}
	return job, nil
}

// buildInsertJobItemsSQL 拼多行 VALUES。$1 固定是 job_id，之后每行 6 个占位符。
// 单次批量上限 50（designkit_settings.max_batch_items），离 PostgreSQL 的
// 65535 个参数上限很远。
func buildInsertJobItemsSQL(rows int) string {
	var b strings.Builder
	_, _ = b.WriteString(`INSERT INTO designkit_job_items ` +
		`(job_id, seq, asset_id, variant_id, prompt_id, prompt_text, max_attempts) VALUES `)
	for i := 0; i < rows; i++ {
		if i > 0 {
			_, _ = b.WriteString(", ")
		}
		base := 2 + i*6
		fmt.Fprintf(&b, "($1, $%d, $%d, $%d, $%d, $%d, $%d)",
			base, base+1, base+2, base+3, base+4, base+5)
	}
	return b.String()
}

func insertJobItems(ctx context.Context, tx dkExecutor, jobID int64, items []dkdomain.CreateJobItemParams) error {
	args := make([]any, 0, 1+len(items)*6)
	args = append(args, jobID)
	for _, item := range items {
		maxAttempts := item.MaxAttempts
		if maxAttempts <= 0 {
			maxAttempts = dkdomain.DefaultMaxAttempts
		}
		args = append(args, item.Seq, item.AssetID, item.VariantID, item.PromptID, item.PromptText, maxAttempts)
	}
	if _, err := tx.ExecContext(ctx, buildInsertJobItemsSQL(len(items)), args...); err != nil {
		return translate(err, "任务明细")
	}
	return nil
}

func insertHold(ctx context.Context, tx dkExecutor, jobID, userID int64, amount dkdomain.Money) error {
	_, err := tx.ExecContext(ctx, `
INSERT INTO designkit_holds (job_id, user_id, amount, status)
VALUES ($1, $2, $3, 'open')
ON CONFLICT (job_id) DO UPDATE SET
  amount = CASE WHEN designkit_holds.status = 'open'
                THEN designkit_holds.amount + EXCLUDED.amount
                ELSE EXCLUDED.amount END,
  status = 'open',
  updated_at = NOW()`, jobID, userID, amount)
	if err != nil {
		return translate(err, "冻结记录")
	}
	return nil
}

// ============================================================================
// 读
// ============================================================================

func (r *designkitRepository) GetJobByUID(ctx context.Context, userID int64, uid string) (*dkdomain.Job, error) {
	// userID 不匹配一律 ErrNotFound，不要返回 403 —— 那会泄露「这个任务号存在」。
	job, err := scanJob(r.sql.QueryRowContext(ctx,
		`SELECT `+jobColumns+` FROM designkit_jobs
		 WHERE uid = $1 AND user_id = $2 AND user_deleted_at IS NULL`, uid, userID))
	if err != nil {
		return nil, translate(err, "任务")
	}
	return job, nil
}

func (r *designkitRepository) GetJobByID(ctx context.Context, id int64) (*dkdomain.Job, error) {
	// 给 worker 用：它没有 userID 上下文，也要看得到用户软删掉的任务
	// （运营删了任务，在途的图仍然要跑完并结算，钱已经花了）。
	job, err := scanJob(r.sql.QueryRowContext(ctx,
		`SELECT `+jobColumns+` FROM designkit_jobs WHERE id = $1`, id))
	if err != nil {
		return nil, translate(err, "任务")
	}
	return job, nil
}

// buildListJobsQuery 拼批次列表查询。**游标分页，不用 offset**
// （offset 在持续写入时会漏数据，设计定型 3.3）。
// 多取一条用来判断有没有下一页。
func buildListJobsQuery(query dkdomain.ListJobsQuery) (string, []any) {
	args := []any{query.UserID}
	var b strings.Builder
	_, _ = b.WriteString("SELECT " + jobColumns + " FROM designkit_jobs WHERE user_id = $1")
	if !query.IncludeDeleted {
		_, _ = b.WriteString(" AND user_deleted_at IS NULL")
	}
	if len(query.Statuses) > 0 {
		statuses := make([]string, 0, len(query.Statuses))
		for _, s := range query.Statuses {
			statuses = append(statuses, s.String())
		}
		args = append(args, pq.Array(statuses))
		fmt.Fprintf(&b, " AND status = ANY($%d)", len(args))
	}
	if !query.CursorCreatedAt.IsZero() && query.CursorID > 0 {
		// (created_at, id) 行比较：只按时间翻页会在同一微秒建的批次上漏行或重复行。
		args = append(args, query.CursorCreatedAt, query.CursorID)
		fmt.Fprintf(&b, " AND (created_at, id) < ($%d, $%d)", len(args)-1, len(args))
	}
	limit := clampLimit(query.Limit, DefaultPageLimit, MaxPageLimit)
	args = append(args, limit+1)
	fmt.Fprintf(&b, " ORDER BY created_at DESC, id DESC LIMIT $%d", len(args))
	return b.String(), args
}

func (r *designkitRepository) ListJobs(ctx context.Context, query dkdomain.ListJobsQuery) (*dkdomain.JobPage, error) {
	sqlText, args := buildListJobsQuery(query)
	rows, err := r.sql.QueryContext(ctx, sqlText, args...)
	if err != nil {
		return nil, err
	}
	defer closeRows(rows)

	jobs, err := scanJobs(rows)
	if err != nil {
		return nil, err
	}

	limit := clampLimit(query.Limit, DefaultPageLimit, MaxPageLimit)
	page := &dkdomain.JobPage{Jobs: jobs}
	if len(jobs) > limit {
		page.Jobs = jobs[:limit]
		page.HasMore = true
	}
	if n := len(page.Jobs); n > 0 && page.HasMore {
		last := page.Jobs[n-1]
		page.NextCursorCreatedAt = last.CreatedAt
		page.NextCursorID = last.ID
	}
	return page, nil
}

// ListJobItems **必须显式 ORDER BY seq ASC** —— 「按序号取第几张图」是发给 ERP 的
// 对外契约，ent 的 edge 和裸 SELECT 都不保证顺序。有专门单测守着这条 SQL。
func (r *designkitRepository) ListJobItems(ctx context.Context, jobID int64) ([]*dkdomain.JobItem, error) {
	return listJobItems(ctx, r.sql, jobID)
}

const listJobItemsSQL = `SELECT ` + jobItemColumns +
	` FROM designkit_job_items WHERE job_id = $1 ORDER BY seq ASC`

func listJobItems(ctx context.Context, exec dkExecutor, jobID int64) ([]*dkdomain.JobItem, error) {
	rows, err := exec.QueryContext(ctx, listJobItemsSQL, jobID)
	if err != nil {
		return nil, err
	}
	defer closeRows(rows)
	return scanJobItems(rows)
}

func (r *designkitRepository) GetJobItemBySeq(ctx context.Context, jobID int64, seq int) (*dkdomain.JobItem, error) {
	item, err := scanJobItem(r.sql.QueryRowContext(ctx,
		`SELECT `+jobItemColumns+` FROM designkit_job_items WHERE job_id = $1 AND seq = $2`,
		jobID, seq))
	if err != nil {
		return nil, translate(err, "任务里的这一张")
	}
	return item, nil
}

// buildListImagesByItemSQL 结果图查询。
// 软删的一律不给；currentOnly 时只给当前版本（重试出了新图，旧图 is_current=false）。
func buildListImagesByItemSQL(currentOnly bool) string {
	where := "WHERE item_id = $1 AND deleted_at IS NULL"
	if currentOnly {
		where += " AND is_current"
	}
	// image_index 升序是对外契约（「第 1 张」就是第一张）；
	// 后两个排序键只为让同一 index 的历史版本有稳定顺序。
	return "SELECT " + imageColumns + " FROM designkit_images " + where +
		" ORDER BY image_index ASC, attempt ASC, id ASC"
}

func (r *designkitRepository) ListImagesByItem(ctx context.Context, itemID int64, currentOnly bool) ([]*dkdomain.Image, error) {
	rows, err := r.sql.QueryContext(ctx, buildListImagesByItemSQL(currentOnly), itemID)
	if err != nil {
		return nil, err
	}
	defer closeRows(rows)
	return scanImages(rows)
}

func (r *designkitRepository) GetImageByUID(ctx context.Context, userID int64, uid string) (*dkdomain.Image, error) {
	img, err := scanImage(r.sql.QueryRowContext(ctx, `
SELECT `+prefixColumns("i", imageColumns)+`
FROM designkit_images i
JOIN designkit_jobs j ON j.id = i.job_id
WHERE i.uid = $1 AND j.user_id = $2 AND i.deleted_at IS NULL`, uid, userID))
	if err != nil {
		return nil, translate(err, "图片")
	}
	return img, nil
}

// ============================================================================
// 状态流转
// ============================================================================

func (r *designkitRepository) UpdateJobStatus(ctx context.Context, jobID int64, from, to dkdomain.JobStatus) error {
	if !dkdomain.CanTransition(from, to) {
		return illegalTransitionErr("任务", from.String(), to.String())
	}
	// ⚠ `$3` 两处**都必须显式 ::text**，少一个整条语句连 PREPARE 都过不了：
	//
	//	ERROR: inconsistent types deduced for parameter $3
	//	DETAIL: text versus character varying
	//
	// 原因：`status = $3` 会把 $3 按列类型推成 character varying，
	// 而 `$3 = 'running'` 跟无类型字面量比较会把它推成 text，两边打架。
	// 加了 ::text 之后 $3 恒为 text，`status = $3` 变成 varchar = text，
	// 这两个类型是二进制兼容的，隐式转换本来就有，比较照常走索引。
	//
	// 2026-08-14 这条 bug 让**每一次提交都 500**（DK_INTERNAL），
	// 而且现象极具迷惑性：批次行、item 行、冻结额全都建出来了，
	// 队列也真的开始出图了 —— 因为炸在最后那步 created→holding 的状态流转上，
	// 服务层如实回滚（退冻结、清幂等键、判 failed），运营看到的是
	// 「提交没有完成，冻结的额度已经退回」。
	//
	// 拦不住它的原因是 designkit 一条集成测试都没有（36 个测试文件全是 unit 标签），
	// 这条 SQL 从来没在真的 PostgreSQL 上跑过 —— 类型推断错误只在
	// PREPARE 阶段暴露，用假 repo 的单测永远碰不到。
	res, err := r.sql.ExecContext(ctx, `
UPDATE designkit_jobs
SET status = $3::text,
    started_at = CASE WHEN $3::text = 'running' AND started_at IS NULL THEN NOW() ELSE started_at END,
    heartbeat_at = NOW(),
    updated_at = NOW()
WHERE id = $1 AND status = $2`, jobID, from.String(), to.String())
	if err != nil {
		return err
	}
	affected, err := affectedRows(res)
	if err != nil {
		return err
	}
	if affected == 0 {
		// 守卫没命中：状态已经被别人改过了。
		return conflictErr(fmt.Sprintf("任务 %d 已经不在 %s 状态", jobID, from))
	}
	return nil
}

// ============================================================================
// 队列
// ============================================================================

// buildClaimItemSQL 领取一张待办。**一个字都不要改逻辑**（设计定型 第六节）：
//
//   - `FOR UPDATE SKIP LOCKED LIMIT 1`：多个 worker 抢同一批时互相跳过，不打架；
//   - 「僵尸回收」（status='running' 且租约过期）写进**同一个** WHERE，
//     不需要第二条巡检 SQL；
//   - `attempt_count` **在领取时就 +1**，不是成功后才加 ——
//     否则进程崩一次，这一张会被永远重试下去；
//   - `ORDER BY job_id, seq`：先来的批次先跑，批内按序号跑。
//
// **`attempt_count < max_attempts` 只加在 pending 那一支，不要加到僵尸那一支。**
// （2026-08-13 补的第二道防线，第一道在 worker 的 handleItem 里。）
//
//	pending 支加它：万一哪天有人写出「把试满的 item 退回 pending」的代码，
//	                这条 WHERE 会让它领不出来，也就花不掉钱。
//	僵尸支不能加：一张 attempt_count 已经等于 max_attempts、正在跑的图，
//	              进程被 SIGKILL 之后就永远停在 running。加了这个条件它
//	              **再也领不出来 = 再也判不了死**，批次的
//	              success+fail+cancelled 永远凑不满 item_count，
//	              收尾守卫命中不了 → 冻结额永远占着用户余额
//	              （表现为「明明没任务却提示余额不足」）。
//	              所以僵尸照领不误，由 worker 领到手第一件事判上限、
//	              **不调网关**直接判 failed。
//
// ⚠ pending 支那条守卫自己也会造一个死结：一行「pending 且已经试满」的 item
// 从此永远领不出来，而心跳按 `status IN ('pending','running')` 算，
// 会让它所在的批次永远续心跳、永远躲过僵尸巡检 —— 批次卡在 running、
// 冻结额永远占着。**拆这个死结的是 reapDeadPendingItems**（同一文件下方），
// 巡检每分钟把这类 item 直接判死。两段代码要一起看，改一边必须想到另一边。
//
// useDBNow=false 时用调用方传进来的时间（$3），给测试钉死时间用。
func buildClaimItemSQL(useDBNow bool) string {
	now := "NOW()"
	if !useDBNow {
		now = "$3::timestamptz"
	}
	return `
UPDATE designkit_job_items SET
  status = 'running',
  worker_id = $1,
  lease_expires_at = ` + now + ` + make_interval(secs => $2),
  attempt_count = attempt_count + 1,
  started_at = COALESCE(started_at, ` + now + `),
  updated_at = ` + now + `
WHERE id = (
  SELECT id FROM designkit_job_items
  WHERE (status = 'pending' AND available_at <= ` + now + ` AND attempt_count < max_attempts)
     OR (status = 'running' AND lease_expires_at < ` + now + `)
  ORDER BY job_id, seq
  FOR UPDATE SKIP LOCKED
  LIMIT 1
)
RETURNING ` + jobItemColumns
}

func (r *designkitRepository) ClaimNextItem(ctx context.Context, params dkdomain.ClaimItemParams) (*dkdomain.JobItem, error) {
	if strings.TrimSpace(params.WorkerID) == "" {
		return nil, errors.New("designkit: 领取任务必须带 worker_id（写回结果时要拿它做守卫）")
	}
	lease := seconds(params.LeaseFor, dkdomain.DefaultLeaseSeconds*time.Second)

	args := []any{params.WorkerID, lease}
	useDBNow := params.Now.IsZero()
	if !useDBNow {
		args = append(args, params.Now)
	}

	item, err := scanJobItem(r.sql.QueryRowContext(ctx, buildClaimItemSQL(useDBNow), args...))
	if err != nil {
		// 没有可领的 → ErrNotFound，调用方睡一会儿再来。
		return nil, translate(err, "可领取的待办")
	}
	return item, nil
}

func (r *designkitRepository) RenewItemLease(ctx context.Context, itemID int64, workerID string, leaseFor time.Duration) error {
	res, err := r.sql.ExecContext(ctx, `
UPDATE designkit_job_items
SET lease_expires_at = NOW() + make_interval(secs => $3), updated_at = NOW()
WHERE id = $1 AND worker_id = $2 AND status = 'running'`,
		itemID, workerID, seconds(leaseFor, dkdomain.DefaultLeaseSeconds*time.Second))
	if err != nil {
		return err
	}
	affected, err := affectedRows(res)
	if err != nil {
		return err
	}
	if affected != 1 {
		return leaseLostErr(itemID, workerID)
	}
	return nil
}

// completeItemGuardSuffix 是写回结果的守卫。
// **影响行数不等于 1 就丢弃结果**（说明租约已被别人抢走，那张图已经有人在负责了）。
const completeItemGuardSuffix = ` WHERE id = $1 AND worker_id = $2 AND status = 'running'`

func (r *designkitRepository) CompleteItemSuccess(ctx context.Context, params dkdomain.CompleteItemSuccessParams) error {
	if len(params.Images) == 0 {
		return dkdomain.NewError(dkdomain.ErrCodeInternal).
			WithMessage("出图成功却一张图都没有，已丢弃这次结果。")
	}
	finishedAt := params.FinishedAt
	if finishedAt.IsZero() {
		finishedAt = time.Now()
	}

	return r.withTx(ctx, func(tx *sql.Tx) error {
		var jobID int64
		err := tx.QueryRowContext(ctx, `
UPDATE designkit_job_items SET
  status = 'succeeded',
  billing_request_id = $3,
  last_error_code = NULL,
  last_error_message = NULL,
  lease_expires_at = NULL,
  finished_at = $4,
  updated_at = NOW()`+completeItemGuardSuffix+`
RETURNING job_id`, params.ItemID, params.WorkerID, nullableString(params.BillingRequestID), finishedAt).Scan(&jobID)
		if errors.Is(err, sql.ErrNoRows) {
			return leaseLostErr(params.ItemID, params.WorkerID)
		}
		if err != nil {
			return err
		}

		// 重试出了新图：把**其它 attempt** 的当前图降级，历史版本留着不删。
		if _, err := tx.ExecContext(ctx, `
UPDATE designkit_images SET is_current = FALSE
WHERE item_id = $1 AND attempt <> $2 AND is_current AND deleted_at IS NULL`,
			params.ItemID, params.Attempt); err != nil {
			return err
		}

		for _, img := range params.Images {
			if err := insertImage(ctx, tx, jobID, params, img); err != nil {
				return err
			}
		}

		// 计数列一律原子自增，绝不读回来再写。
		if _, err := tx.ExecContext(ctx, `
UPDATE designkit_jobs
SET success_count = success_count + 1,
    started_at = COALESCE(started_at, NOW()),
    heartbeat_at = NOW(),
    updated_at = NOW()
WHERE id = $1`, jobID); err != nil {
			return err
		}
		return nil
	})
}

// imageCurrentConflictTarget 是「每个 item 的每个 image_index 只能有一张当前图」
// 那条**部分**唯一索引的冲突目标。
//
// **WHERE 谓词必须跟 9001 迁移里的索引定义一字不差**，否则 PostgreSQL 推断不出
// 用哪个索引，直接报错。唯一键里带 image_index：万一网关一次返回两张图，
// 少了这一列第二张会插不进去 —— 钱花了、图丢了。
const imageCurrentConflictTarget = `ON CONFLICT (item_id, image_index) WHERE is_current AND deleted_at IS NULL`

// insertImage 写一张结果图。
func insertImage(ctx context.Context, tx dkExecutor, jobID int64, params dkdomain.CompleteItemSuccessParams, img dkdomain.NewImage) error {
	_, err := tx.ExecContext(ctx, `
INSERT INTO designkit_images (
  uid, job_id, item_id, attempt, image_index, is_current,
  object_key, content_type, byte_size, width, height, billing_tier
) VALUES ($1, $2, $3, $4, $5, TRUE, $6, $7, $8, $9, $10, $11)
`+imageCurrentConflictTarget+`
DO UPDATE SET
  uid = EXCLUDED.uid,
  attempt = EXCLUDED.attempt,
  object_key = EXCLUDED.object_key,
  content_type = EXCLUDED.content_type,
  byte_size = EXCLUDED.byte_size,
  width = EXCLUDED.width,
  height = EXCLUDED.height,
  billing_tier = EXCLUDED.billing_tier`,
		img.UID, jobID, params.ItemID, params.Attempt, img.ImageIndex,
		img.ObjectKey, img.ContentType, img.ByteSize, img.Width, img.Height, img.BillingTier)
	if err != nil {
		return translate(err, "结果图")
	}
	return nil
}

func (r *designkitRepository) CompleteItemFailure(ctx context.Context, params dkdomain.CompleteItemFailureParams) error {
	finishedAt := params.FinishedAt
	if finishedAt.IsZero() {
		finishedAt = time.Now()
	}

	if !params.Terminal {
		// 还能再试：退回 pending 并把 available_at 往后推做退避。
		// **不动 job 的任何计数**（这一张还没到终态）。
		res, err := r.sql.ExecContext(ctx, `
UPDATE designkit_job_items SET
  status = 'pending',
  last_error_code = $3,
  last_error_message = $4,
  available_at = NOW() + make_interval(secs => $5),
  worker_id = NULL,
  lease_expires_at = NULL,
  updated_at = NOW()`+completeItemGuardSuffix,
			params.ItemID, params.WorkerID,
			nullableString(params.ErrorCode), nullableString(params.ErrorMessage),
			seconds(params.RetryAfter, time.Second))
		if err != nil {
			return err
		}
		affected, err := affectedRows(res)
		if err != nil {
			return err
		}
		if affected != 1 {
			return leaseLostErr(params.ItemID, params.WorkerID)
		}
		return nil
	}

	return r.withTx(ctx, func(tx *sql.Tx) error {
		var jobID int64
		err := tx.QueryRowContext(ctx, `
UPDATE designkit_job_items SET
  status = 'failed',
  last_error_code = $3,
  last_error_message = $4,
  worker_id = NULL,
  lease_expires_at = NULL,
  finished_at = $5,
  updated_at = NOW()`+completeItemGuardSuffix+`
RETURNING job_id`,
			params.ItemID, params.WorkerID,
			nullableString(params.ErrorCode), nullableString(params.ErrorMessage), finishedAt).Scan(&jobID)
		if errors.Is(err, sql.ErrNoRows) {
			return leaseLostErr(params.ItemID, params.WorkerID)
		}
		if err != nil {
			return err
		}

		if _, err := tx.ExecContext(ctx, `
UPDATE designkit_jobs
SET fail_count = fail_count + 1, heartbeat_at = NOW(), updated_at = NOW()
WHERE id = $1`, jobID); err != nil {
			return err
		}
		return nil
	})
}

// ============================================================================
// 停止排队 / 重试 / 终态
// ============================================================================

// RequestJobStop 只做两件事：把 pending 的 item 转 cancelled、给 job 打
// cancel_requested_at。**已经 running 的一律不动**（决策 21）——
// 上游出图跟浏览器连接是故意脱钩的，在跑的那几张停不下来，
// 必须等它自然落地、正常扣费、图存进作品库。
func (r *designkitRepository) RequestJobStop(ctx context.Context, jobID int64) (int, error) {
	var cancelled int
	err := r.withTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `
UPDATE designkit_jobs
SET cancel_requested_at = COALESCE(cancel_requested_at, NOW()), updated_at = NOW()
WHERE id = $1`, jobID)
		if err != nil {
			return err
		}
		affected, err := affectedRows(res)
		if err != nil {
			return err
		}
		if affected == 0 {
			return notFoundErr("任务")
		}

		res, err = tx.ExecContext(ctx, `
UPDATE designkit_job_items
SET status = 'cancelled', finished_at = NOW(), updated_at = NOW()
WHERE job_id = $1 AND status = 'pending'`, jobID)
		if err != nil {
			return err
		}
		n, err := affectedRows(res)
		if err != nil {
			return err
		}
		cancelled = int(n)
		if cancelled == 0 {
			return nil
		}

		// cancelled_count 只统计「没开始就被砍掉的」。
		_, err = tx.ExecContext(ctx, `
UPDATE designkit_jobs
SET cancelled_count = cancelled_count + $2, updated_at = NOW()
WHERE id = $1`, jobID, cancelled)
		return err
	})
	if err != nil {
		return 0, err
	}
	return cancelled, nil
}

// retryCountAdjustSQL 返回重试时要回退的那个计数列。
// 一张 item 被拉回 pending，它之前记在哪个计数上就从哪个计数减回去，
// 否则 success+fail+cancelled 会永远大于 item_count，终态判定那条守卫再也命中不了。
func retryCountAdjustSQL(from dkdomain.ItemStatus) string {
	switch from {
	case dkdomain.ItemStatusSucceeded:
		return "success_count = GREATEST(success_count - 1, 0)"
	case dkdomain.ItemStatusFailed:
		return "fail_count = GREATEST(fail_count - 1, 0)"
	case dkdomain.ItemStatusCancelled:
		return "cancelled_count = GREATEST(cancelled_count - 1, 0)"
	default:
		return ""
	}
}

func (r *designkitRepository) RetryItem(ctx context.Context, params dkdomain.RetryItemParams) (*dkdomain.JobItem, error) {
	var item *dkdomain.JobItem
	err := r.withTx(ctx, func(tx *sql.Tx) error {
		// 锁顺序固定：users → designkit_jobs → designkit_job_items。
		// 跟提交那条路径同序，避免两条路径互相死锁。
		balance, err := lockUserBalance(ctx, tx, params.UserID)
		if err != nil {
			return err
		}

		job, err := scanJob(tx.QueryRowContext(ctx,
			`SELECT `+jobColumns+` FROM designkit_jobs WHERE id = $1 AND user_id = $2 FOR UPDATE`,
			params.JobID, params.UserID))
		if err != nil {
			return translate(err, "任务")
		}
		if job.IsSettled() {
			// 账已经算清的批次拒绝重试（决策：409 +「这批任务已结算，请重新提交」）。
			return dkdomain.NewError(dkdomain.ErrCodeJobAlreadySettled).WithCause(dkdomain.ErrConflict)
		}

		current, err := scanJobItem(tx.QueryRowContext(ctx,
			`SELECT `+jobItemColumns+` FROM designkit_job_items WHERE job_id = $1 AND seq = $2 FOR UPDATE`,
			params.JobID, params.Seq))
		if err != nil {
			return translate(err, "任务里的这一张")
		}
		if !dkdomain.CanTransitionItem(current.Status, dkdomain.ItemStatusPending) {
			return illegalTransitionErr("这一张", current.Status.String(), dkdomain.ItemStatusPending.String())
		}
		if !current.CanRetry() {
			// max_attempts 是**累计**上限，运营手动点的重试也算在内（决策 20）。
			return dkdomain.NewError(dkdomain.ErrCodeMaxAttemptsExceeded).WithCause(dkdomain.ErrConflict)
		}

		// 重试走与提交同一套准入：单张预估 → 可用额不够就 422，不允许透支。
		held, err := sumOpenHolds(ctx, tx, params.UserID)
		if err != nil {
			return err
		}
		estimated := dkdomain.QuantizeMoney(params.EstimatedCost)
		available := dkdomain.QuantizeMoney(balance.Sub(held))
		if available.LessThan(estimated) {
			return newInsufficientBalance(estimated, available)
		}

		if err := updateJobForRetry(ctx, tx, job, current.Status); err != nil {
			return err
		}

		item, err = scanJobItem(tx.QueryRowContext(ctx, `
UPDATE designkit_job_items SET
  status = 'pending',
  available_at = NOW(),
  worker_id = NULL,
  lease_expires_at = NULL,
  last_error_code = NULL,
  last_error_message = NULL,
  finished_at = NULL,
  updated_at = NOW()
WHERE id = $1 AND status = $2
RETURNING `+jobItemColumns, current.ID, current.Status.String()))
		if err != nil {
			return translate(err, "任务里的这一张")
		}

		// 补冻结：同一行加回去，**不要再插一行**（job_id 上有 UNIQUE）。
		return insertHold(ctx, tx, job.ID, params.UserID, estimated)
	})
	if err != nil {
		return nil, err
	}
	return item, nil
}

// updateJobForRetry 把批次从终态拉回 running（revision + 1、清 finished_at），
// 或者在批次还在跑时只回退计数。
func updateJobForRetry(ctx context.Context, tx dkExecutor, job *dkdomain.Job, itemFrom dkdomain.ItemStatus) error {
	sets := []string{"updated_at = NOW()", "heartbeat_at = NOW()"}
	if adjust := retryCountAdjustSQL(itemFrom); adjust != "" {
		sets = append(sets, adjust)
	}

	args := []any{job.ID}
	where := "WHERE id = $1 AND settled_at IS NULL"

	switch {
	case job.Status.IsTerminal():
		if !dkdomain.CanTransition(job.Status, dkdomain.JobStatusRunning) {
			// failed / cancelled 的批次不允许拉回 running（白名单只放 succeeded /
			// partially_failed）。让运营重新提交一批，别在这里开后门。
			return illegalTransitionErr("任务", job.Status.String(), dkdomain.JobStatusRunning.String())
		}
		sets = append(sets, "status = 'running'", "finished_at = NULL", "revision = revision + 1")
		args = append(args, job.Status.String())
		where += fmt.Sprintf(" AND status = $%d", len(args))
	case job.Status == dkdomain.JobStatusRunning || job.Status == dkdomain.JobStatusHolding:
		// 批次还在跑，不用改状态，回退计数就行。
	default:
		// created / settling：账正在算或还没起步，这时候重试会把账算乱。
		return illegalTransitionErr("任务", job.Status.String(), dkdomain.JobStatusRunning.String())
	}

	res, err := tx.ExecContext(ctx,
		"UPDATE designkit_jobs SET "+strings.Join(sets, ", ")+" "+where, args...)
	if err != nil {
		return err
	}
	affected, err := affectedRows(res)
	if err != nil {
		return err
	}
	if affected != 1 {
		return conflictErr(fmt.Sprintf("任务 %d 的状态已经被别人改过了", job.ID))
	}
	return nil
}

// finalizeSourceStatuses 是 FinalizeJob 允许的来源状态。
//
// 白名单里只有 running → settling。这里额外放行 created / holding：
// 整批还没开跑就被「停止排队」全砍掉时，job 停在 created 或 holding，
// 计数已经填满但永远不会走到 running —— 不放它过去，这批任务不会结算、
// 冻结额永远占着用户的可用余额（表现为「明明没任务却提示余额不足」）。
func finalizeSourceStatuses() []string {
	sources := allowedSourceStatuses(dkdomain.JobStatusSettling)
	for _, extra := range []dkdomain.JobStatus{dkdomain.JobStatusCreated, dkdomain.JobStatusHolding} {
		if !containsString(sources, extra.String()) {
			sources = append(sources, extra.String())
		}
	}
	return sources
}

// FinalizeJob 终态判定 + 回调触发合并成**一条带守卫的 UPDATE**。
//
// 只有 RowsAffected = 1 的那个 worker 拿到 won=true 去结算和发回调，
// 否则 5 个 worker 都以为自己是最后一张，回调会发 5 次。
//
// ⚠ 这里落的状态是 settling，不是 params.Status 里那个终态：
// 上游写 usage_logs 是**异步**的，最后一张图返回的那一刻立刻汇总很可能少算甚至全算 0。
// 真正的终态 + settled_at 由 SettleJob 写（设计定型 第五节第 6~8 步）。
func (r *designkitRepository) FinalizeJob(ctx context.Context, params dkdomain.FinalizeJobParams) (bool, error) {
	finishedAt := params.FinishedAt
	if finishedAt.IsZero() {
		finishedAt = time.Now()
	}
	if params.Status != "" && !params.Status.IsValid() {
		return false, illegalTransitionErr("任务", "?", string(params.Status))
	}

	res, err := r.sql.ExecContext(ctx, `
UPDATE designkit_jobs
SET status = $2, finished_at = $3, updated_at = NOW()
WHERE id = $1
  AND finished_at IS NULL
  AND success_count + fail_count + cancelled_count = item_count
  AND status = ANY($4)`,
		params.JobID, dkdomain.JobStatusSettling.String(), finishedAt, pq.Array(finalizeSourceStatuses()))
	if err != nil {
		return false, err
	}
	affected, err := affectedRows(res)
	if err != nil {
		return false, err
	}
	return affected == 1, nil
}

func (r *designkitRepository) TouchJobHeartbeat(ctx context.Context, jobIDs []int64) error {
	if len(jobIDs) == 0 {
		return nil
	}
	// 只动 heartbeat_at，不碰 updated_at —— 心跳不是业务变更，
	// 每 30 秒刷一次 updated_at 会让「最近更新」这类排序全乱。
	_, err := r.sql.ExecContext(ctx,
		`UPDATE designkit_jobs SET heartbeat_at = NOW() WHERE id = ANY($1)`, pq.Array(jobIDs))
	return err
}

// staleSettlingAfter 批次卡在 settling 多久之后由巡检兜底。
//
// 【为什么非有这一档不可】
// FinalizeJob 一律把批次写成 settling，而把它推到终态、释放冻结额的**只有结算 worker**。
// 结算 worker 只要缺对象存储 / 图像处理服务 / 出图网关任意一个就整个不启动
// （designkit/module.go 的 newWorker），而这恰恰是最需要兜底的场景：
// 上一次进程好好地把几个批次推到了 settling，重启时某个环境变量配坏了 →
// 队列整个不起 → 那几笔冻结额**永远**占着用户的可用额，没有任何报错，
// 运营看到的是「明明没任务却提示余额不足」。
// worker/settlement.go 里那个 30 分钟的强制结算兜底活在结算 goroutine 里，救不了这种情况。
//
// 【为什么是 60 分钟，比 settlement.go 的 30 分钟长一倍】
// 结算 worker 只要活着就一定先动手（30 分钟就强制结算，而且会按已经查到的账单
// 把 actual_cost 填准）。巡检这一档是「worker 根本没起来」时的最后一道，
// 抢在它前面只会把本来能对准的账变成粗算。
//
// 刻意不做成配置项：它是兜底不是调参，调错了（比如调成 1 分钟）会把还在正常
// 等账单的批次全部粗算结掉。
const staleSettlingAfter = 60 * time.Minute

// ReapStaleJobs 僵尸巡检。**三件事，都是「不做就有钱被永久冻住」**：
//
//	① pending 却已经没有尝试次数的 item —— 直接判死（见 reapDeadPendingItems）；
//	② 心跳过期的 created/holding/running 批次 —— 转 failed 并释放冻结；
//	③ 卡在 settling 超过 staleSettlingAfter 的批次 —— 判终态、封账、释放冻结。
//
// 返回值是 ②③ 命中的 job id（①不返回，它没把批次判死，只是把死结拆开，
// 让批次沿着正常的 settling → 结算路径往下走）。
//
// ⚠ 返回值里 ②③ 是混在一起的，调用方分不出是哪一种 —— ReapStaleJobs 的签名
// 定在 domain/port.go。worker/reaper.go 靠回读一次 status 来区分，见那边的注释。
func (r *designkitRepository) ReapStaleJobs(ctx context.Context, staleAfter time.Duration, limit int) ([]int64, error) {
	if limit <= 0 {
		limit = 50
	}
	stale := seconds(staleAfter, dkdomain.DefaultStaleJobMinutes*time.Minute)

	var ids []int64
	err := r.withTx(ctx, func(tx *sql.Tx) error {
		// ---- ① 拆「pending 但没次数了」的死结 ----
		if err := reapDeadPendingItems(ctx, tx, limit); err != nil {
			return err
		}

		// ---- ② 心跳过期的批次 ----
		// 单独一个函数把结果集读完并关掉：*sql.Tx 共用一条连接，
		// 结果集没关就接着发下一条语句，lib/pq 会报「连接忙」。
		staleIDs, err := reapStaleJobIDs(ctx, tx, stale, limit)
		if err != nil {
			return err
		}
		if len(staleIDs) > 0 {
			if err := stopUnfinishedItems(ctx, tx, staleIDs); err != nil {
				return err
			}
			if err := releaseHoldsForJobs(ctx, tx, staleIDs); err != nil {
				return err
			}
		}

		// ---- ③ 卡死在 settling 的批次 ----
		settlingIDs, err := reapStuckSettlingJobIDs(ctx, tx, staleSettlingAfter.Seconds(), limit)
		if err != nil {
			return err
		}
		if len(settlingIDs) > 0 {
			if err := releaseHoldsForJobs(ctx, tx, settlingIDs); err != nil {
				return err
			}
		}

		ids = append(staleIDs, settlingIDs...)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return ids, nil
}

// stopUnfinishedItemsSQL 把被判死的批次名下**还没跑完的 item 也停掉**。
//
// 不停的话，那些 pending 的 item 照样会被别的 worker 领走出图扣钱，
// 而任务已经是 failed —— 凭空多出无归属扣款。
//
// pending → cancelled（没开始，不花钱）；running → failed（租约早过期了，
// 那个进程已经死了）。item 侧没有「任意 → failed」的兜底，
// 所以这里的两个目标状态都严格落在 CanTransitionItem 白名单内。
const stopUnfinishedItemsSQL = `
WITH reaped AS (
  UPDATE designkit_job_items SET
    status = CASE WHEN status = 'pending' THEN 'cancelled' ELSE 'failed' END,
    last_error_code = CASE WHEN status = 'running' THEN $2 ELSE last_error_code END,
    worker_id = NULL,
    lease_expires_at = NULL,
    finished_at = COALESCE(finished_at, NOW()),
    updated_at = NOW()
  WHERE job_id = ANY($1) AND status IN ('pending', 'running')
  RETURNING job_id, status
)
UPDATE designkit_jobs j SET
  cancelled_count = j.cancelled_count + agg.cancelled,
  fail_count = j.fail_count + agg.failed,
  updated_at = NOW()
FROM (
  SELECT job_id,
         COUNT(*) FILTER (WHERE status = 'cancelled') AS cancelled,
         COUNT(*) FILTER (WHERE status = 'failed') AS failed
  FROM reaped GROUP BY job_id
) agg
WHERE j.id = agg.job_id`

func stopUnfinishedItems(ctx context.Context, tx dkExecutor, jobIDs []int64) error {
	_, err := tx.ExecContext(ctx, stopUnfinishedItemsSQL, pq.Array(jobIDs), dkdomain.ErrCodeStale)
	return err
}

// releaseHoldsForJobsSQL 释放冻结：可用额 = balance - SUM(open holds)，
// 把 status 翻掉就等于把钱还给用户。
const releaseHoldsForJobsSQL = `
UPDATE designkit_holds SET amount = 0, status = 'released', updated_at = NOW()
WHERE job_id = ANY($1) AND status = 'open'`

func releaseHoldsForJobs(ctx context.Context, tx dkExecutor, jobIDs []int64) error {
	_, err := tx.ExecContext(ctx, releaseHoldsForJobsSQL, pq.Array(jobIDs))
	return err
}

// reapStaleJobIDsSQL 把心跳过期的批次原子转 failed。
//
// `FOR UPDATE SKIP LOCKED` 让多个副本同时巡检也不会互相阻塞；
// `COALESCE(heartbeat_at, created_at)` 兜住「刚建完就被杀、心跳还没打过」的那一档。
//
// ⚠ **状态清单里刻意没有 settling**：进了 settling 就没有在途 item 了，心跳本来
// 就不会再续，拿 heartbeat_at 判它必然误杀（而且会把一批已经出好图的任务标成
// failed）。settling 走 reapStuckSettlingJobIDs，判据是 finished_at、阈值长得多。
const reapStaleJobIDsSQL = `
UPDATE designkit_jobs SET
  status = 'failed',
  last_error_code = $1,
  finished_at = COALESCE(finished_at, NOW()),
  updated_at = NOW()
WHERE id IN (
  SELECT id FROM designkit_jobs
  WHERE status IN ('created', 'holding', 'running')
    AND COALESCE(heartbeat_at, created_at) < NOW() - make_interval(secs => $2)
  ORDER BY id
  FOR UPDATE SKIP LOCKED
  LIMIT $3
)
RETURNING id`

func reapStaleJobIDs(ctx context.Context, tx dkExecutor, stale float64, limit int) ([]int64, error) {
	return queryJobIDs(ctx, tx, reapStaleJobIDsSQL, dkdomain.ErrCodeStale, stale, limit)
}

// reapStuckSettlingSQL 把卡死在 settling 的批次判到终态、封账、留给上层释放冻结。
//
// 三个 SET 各有各的必须：
//
//	status      —— 不改就永远留在 settling，冻结额永远占着用户可用额。
//	               **不能一律写 failed**：这批的图很可能全出好了、钱也扣了，
//	               标成 failed 会让运营照着 DK_STALE 的提示「重新提交」，
//	               为已经拿到手的图再花一遍钱。所以按四个计数算真实终态。
//	actual_cost —— 用已经回填的 billed_cost 合计。不填就是「usage_logs 里有扣款、
//	               我们这边 actual_cost 为空」= 凭空多出无归属扣款（设计定型 6.1）。
//	settled_at  —— 封账。填了之后 ListJobsPendingSettlement 不会再捞到它，
//	               重试也会被 DK_JOB_ALREADY_SETTLED 挡下（账已经粗算过，
//	               再往上加会算乱）。
//
// **刻意不动 last_error_code**：所有 item 都已经到终态、各自的失败原因都在
// item 那一行上；批次这一级再写一个「系统重启中断了」是假的 —— 真正出问题的
// 是结算，不是出图。
//
// ⚠ 那个 CASE 是 domain.TerminalJobStatus 的逐条镜像（顺序也一样）。
// 两边必须同步改，job_repo_test.go 里有一条测试把它们钉在一起。
const reapStuckSettlingSQL = `
UPDATE designkit_jobs SET
  status = CASE
    WHEN cancel_requested_at IS NOT NULL AND cancelled_count > 0 THEN 'cancelled'
    WHEN item_count > 0 AND success_count = item_count           THEN 'succeeded'
    WHEN success_count > 0                                       THEN 'partially_failed'
    WHEN cancelled_count > 0 AND fail_count = 0                  THEN 'cancelled'
    ELSE 'failed'
  END,
  actual_cost = COALESCE(actual_cost, (
    SELECT COALESCE(SUM(i.billed_cost), 0)
    FROM designkit_job_items i
    WHERE i.job_id = designkit_jobs.id
  )),
  settled_at = COALESCE(settled_at, NOW()),
  finished_at = COALESCE(finished_at, NOW()),
  updated_at = NOW()
WHERE id IN (
  SELECT id FROM designkit_jobs
  WHERE status = 'settling'
    AND settled_at IS NULL
    AND COALESCE(finished_at, updated_at, created_at) < NOW() - make_interval(secs => $1)
  ORDER BY id
  FOR UPDATE SKIP LOCKED
  LIMIT $2
)
RETURNING id`

func reapStuckSettlingJobIDs(ctx context.Context, tx dkExecutor, stale float64, limit int) ([]int64, error) {
	return queryJobIDs(ctx, tx, reapStuckSettlingSQL, stale, limit)
}

// deadPendingItemsSQL 把「status='pending' 但 attempt_count 已经用满」的 item 直接判死，
// 顺手把 fail_count 加上去，返回受影响的 job id。
//
// 【这是在拆一个死结，不是在防超支】
// 领取 SQL 的 pending 支带 `attempt_count < max_attempts`（设计定型 第六节，
// 那条守卫是对的，别去掉）。代价是：一行「pending 且已经试满」的 item
// **永远领不出来**，于是
//
//	→ 它既不会被出图、也不会到终态；
//	→ activeJobIDs（心跳）按 `status IN ('pending','running')` 给它所在的批次
//	  **永远续心跳**，reapStaleJobIDs 看的是心跳，于是永远命中不了；
//	→ 批次卡在 running，success+fail+cancelled 永远凑不满 item_count，
//	  收尾守卫命中不了，冻结额永远占着用户余额。
//
// 加这条守卫之前，这种 job 会因为心跳停摆被巡检回收；加了之后不能自愈了，
// 所以必须补这一条。
//
// 【为什么是「判死这一行」而不是「让心跳别算它」】
// 让心跳别算它，等于把整批交给僵尸巡检 —— 巡检会把**整个批次**转 failed、
// 把它名下其它正常排队的 item 全部 cancelled，为一行坏数据牵连一整批，
// 而且要等满 10 分钟。判死这一行则只影响这一行：批次接着按正常路径收尾、结算、
// 释放冻结，运营在界面上看到的是这一张「已经重试到上限了」——这本来就是实情。
//
// 【绝不能误伤没跑过的 item】
// `attempt_count > 0` 那一条是硬保险：max_attempts 要是被写成 0 或负数，
// 光看 `attempt_count >= max_attempts` 会把**一张都还没开始跑**的新任务全判死。
// CASE 里的兜底值跟 worker 的 normalizeMaxAttempts 一致（都是 DefaultMaxAttempts）。
var deadPendingItemsSQL = fmt.Sprintf(`
WITH dead AS (
  UPDATE designkit_job_items SET
    status = 'failed',
    last_error_code = $1,
    last_error_message = COALESCE(last_error_message, $2),
    worker_id = NULL,
    lease_expires_at = NULL,
    finished_at = COALESCE(finished_at, NOW()),
    updated_at = NOW()
  WHERE id IN (
    SELECT id FROM designkit_job_items
    WHERE status = 'pending'
      AND attempt_count > 0
      AND attempt_count >= CASE WHEN max_attempts > 0 THEN max_attempts ELSE %d END
    ORDER BY id
    FOR UPDATE SKIP LOCKED
    LIMIT $3
  )
  RETURNING job_id
)
UPDATE designkit_jobs j SET
  fail_count = j.fail_count + agg.dead,
  updated_at = NOW()
FROM (SELECT job_id, COUNT(*) AS dead FROM dead GROUP BY job_id) agg
WHERE j.id = agg.job_id
RETURNING j.id`, dkdomain.DefaultMaxAttempts)

// deadPendingItemUpstreamNote 落进 last_error_message（只给管理员看）。
// 只在原来没有上游原文时才写，不覆盖真正的失败原因。
const deadPendingItemUpstreamNote = "item was pending with no attempt budget left; swept by designkit reaper"

// finalizeAfterSweepSQL 判「这一批是不是刚好被扫完了最后一张」。
//
// 守卫跟 FinalizeJob 那条**一模一样**（finished_at IS NULL + 三个计数凑满 +
// 来源状态白名单），所以最多只有一条路径会把批次推进 settling，不会重复触发结算。
//
// ⚠ 必须是**独立的一条语句**，不能跟 deadPendingItemsSQL 揉进同一个 CTE：
// PostgreSQL 里数据修改型 CTE 全部看同一个快照，同一条语句里读到的 fail_count
// 还是加之前的值，`success+fail+cancelled = item_count` 永远凑不满。
const finalizeAfterSweepSQL = `
UPDATE designkit_jobs SET
  status = 'settling',
  finished_at = COALESCE(finished_at, NOW()),
  updated_at = NOW()
WHERE id = ANY($1)
  AND finished_at IS NULL
  AND success_count + fail_count + cancelled_count = item_count
  AND status = ANY($2)`

func reapDeadPendingItems(ctx context.Context, tx dkExecutor, limit int) error {
	jobIDs, err := queryJobIDs(ctx, tx, deadPendingItemsSQL,
		dkdomain.ErrCodeMaxAttemptsExceeded, deadPendingItemUpstreamNote, limit)
	if err != nil {
		return err
	}
	if len(jobIDs) == 0 {
		return nil
	}
	_, err = tx.ExecContext(ctx, finalizeAfterSweepSQL,
		pq.Array(jobIDs), pq.Array(finalizeSourceStatuses()))
	return err
}

// queryJobIDs 跑一条 RETURNING id 的语句并把 id 收干净。
// 抽出来是因为 *sql.Tx 共用一条连接：结果集没读完关掉就发下一条语句，
// lib/pq 会报「连接忙」，而这三条巡检语句都在同一个事务里排队跑。
func queryJobIDs(ctx context.Context, tx dkExecutor, query string, args ...any) ([]int64, error) {
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer closeRows(rows)

	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// ClearJobIdempotencyKey 把一个批次的 idempotency_key 置 NULL。
//
// 【为什么非有不可】
// 提交没走完时（建好了 job 但没能进队列），service 会把这一批判死。判死之后
// 那一行的 idempotency_key 还在，而 findJobByIdempotencyKey 的窗口是 24 小时、
// **不看状态**。运营见到「失败了」照原样再提交一次（浏览器端那把 key 是打开
// 确认弹窗时生成的，不刷新页面就不会变），第二次会命中幂等分支，
// 原样返回这个 failed 的批次 —— 界面上是「提交成功」，状态却是失败，
// 而且怎么点都一样。清掉 key 之后，重投就是干干净净的一次新提交。
//
// 影响 0 行不算错：这条语句本来就是「尽力而为的善后」，那一行可能已经被
// 别的路径改过了；报错只会让调用方多打一行没人看的日志。
func (r *designkitRepository) ClearJobIdempotencyKey(ctx context.Context, jobID int64) error {
	_, err := r.sql.ExecContext(ctx, `
UPDATE designkit_jobs SET idempotency_key = NULL, updated_at = NOW()
WHERE id = $1 AND idempotency_key IS NOT NULL`, jobID)
	return err
}

func (r *designkitRepository) SoftDeleteJob(ctx context.Context, userID int64, uid string) error {
	res, err := r.sql.ExecContext(ctx, `
UPDATE designkit_jobs SET user_deleted_at = NOW(), updated_at = NOW()
WHERE uid = $1 AND user_id = $2 AND user_deleted_at IS NULL`, uid, userID)
	if err != nil {
		return err
	}
	affected, err := affectedRows(res)
	if err != nil {
		return err
	}
	if affected == 0 {
		return notFoundErr("任务")
	}
	return nil
}

const listJobsPendingSettlementSQL = `SELECT ` + jobColumns + ` FROM designkit_jobs
WHERE status = 'settling' AND settled_at IS NULL
ORDER BY finished_at ASC NULLS FIRST, id ASC
LIMIT $1`

func (r *designkitRepository) ListJobsPendingSettlement(ctx context.Context, limit int) ([]*dkdomain.Job, error) {
	limit = clampLimit(limit, DefaultPageLimit, MaxPageLimit)
	rows, err := r.sql.QueryContext(ctx, listJobsPendingSettlementSQL, limit)
	if err != nil {
		return nil, err
	}
	defer closeRows(rows)
	return scanJobs(rows)
}

// ---- 小工具 ----

// prefixColumns 给列清单里的每一列加上表别名，用于 JOIN 查询。
func prefixColumns(alias, columns string) string {
	parts := strings.Split(columns, ",")
	for i, p := range parts {
		parts[i] = alias + "." + strings.TrimSpace(p)
	}
	return strings.Join(parts, ", ")
}

func containsString(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}

// trimmedPtr 取指针里的字符串并去空白；nil 或空白返回空串。
func trimmedPtr(s *string) string {
	if s == nil {
		return ""
	}
	return strings.TrimSpace(*s)
}

// nullableStringPtr 把 *string 转成 SQL 参数：nil 或空串一律写 NULL。
// 空串和 NULL 混着存会让「WHERE idempotency_key IS NOT NULL」这类条件失准。
func nullableStringPtr(s *string) any {
	v := trimmedPtr(s)
	if v == "" {
		return nil
	}
	return v
}
