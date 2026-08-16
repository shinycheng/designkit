package repository

// 钱：冻结账（designkit_holds）、结算、超支告警、额度申请、消费汇总。
//
// 前提（9001 迁移里写死的，别再论证一遍）：
// **users.balance 全程只由上游扣一次，我们只在 designkit_holds 上记账。**
//
//	可用额 = users.balance - SUM(designkit_holds.amount WHERE user_id=? AND status='open')
//
// 上游自带的 Reserve/Capture/Release 四条全部验实、四条全部致命，一条都不能用
// （见 domain/entity.go 里 Hold 那段注释）。

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/lib/pq"
	"github.com/shopspring/decimal"

	dkdomain "github.com/Wei-Shaw/sub2api/internal/designkit/domain"
)

// GetAvailableBalance **只读，不加锁**。
// 真正要拦透支的地方必须走 CreateJobWithItemsAndHold / RetryItem 里那条
// `SELECT balance ... FOR UPDATE`，不能靠这个函数的返回值做判断 ——
// 读完到写之间的窗口就是上游 B4 那个「50 张并发全部放行」的窗口。
func (r *designkitRepository) GetAvailableBalance(ctx context.Context, userID int64) (dkdomain.Money, error) {
	var available dkdomain.Money
	err := r.sql.QueryRowContext(ctx, `
SELECT u.balance - COALESCE((
         SELECT SUM(h.amount) FROM designkit_holds h
         WHERE h.user_id = u.id AND h.status = 'open'
       ), 0)
FROM users u
WHERE u.id = $1 AND u.deleted_at IS NULL`, userID).Scan(&available)
	if err != nil {
		return dkdomain.ZeroMoney, translate(err, "用户")
	}
	return available, nil
}

// SumUsageLogCosts 按 billing_request_id 去上游 usage_logs 反查实际扣费。
//
// 传进来的必须是**带 client: 前缀的完整值**（domain.StoredBillingRequestID 的返回值），
// 少了前缀一条也查不到，结算会永远等不到账单、无限退避重试。
//
// ⚠ 调用方传的是**每一张图每一次 attempt** 的 id（枚举 attempt = 1..attempt_count
// 推导出来的），不是只传 job_items.billing_request_id 那一个值 ——
// 那一列只存最新一次 attempt，只查它等于把重试花掉的钱全丢了
// （设计定型 6.2：actual_cost = 所有 attempt 合计）。
//
// 查不到的 key 直接不出现在返回值里，调用方靠这一点区分「没扣过钱」和「扣了 0 元」：
// 上游真写过账单的那一行即使 actual_cost 是 0，也会带着 key 出现在返回值里。
func (r *designkitRepository) SumUsageLogCosts(ctx context.Context, billingRequestIDs []string) (map[string]dkdomain.Money, error) {
	out := make(map[string]dkdomain.Money, len(billingRequestIDs))
	if len(billingRequestIDs) == 0 {
		return out, nil
	}
	rows, err := r.sql.QueryContext(ctx, sumUsageLogCostsSQL, pq.Array(billingRequestIDs))
	if err != nil {
		return nil, err
	}
	defer closeRows(rows)

	for rows.Next() {
		var id string
		var cost dkdomain.Money
		if err := rows.Scan(&id, &cost); err != nil {
			return nil, err
		}
		out[id] = cost
	}
	return out, rows.Err()
}

// 这几条 SQL 抽成常量是为了能在单测里断言（本机没有 Go 编译器也没有数据库，
// 能不连库就验的东西必须验掉）。
const (
	// 一次把所有 attempt 的 id 都查掉：= ANY($1) 一条语句，不要在 Go 里循环发 N 条。
	sumUsageLogCostsSQL = `
SELECT request_id, COALESCE(SUM(actual_cost), 0)
FROM usage_logs
WHERE request_id = ANY($1)
GROUP BY request_id
ORDER BY request_id`

	selectItemBillingForUpdateSQL = `SELECT job_id, billed_cost FROM designkit_job_items WHERE id = $1 FOR UPDATE`

	updateItemBillingSQL = `
UPDATE designkit_job_items
SET billed_cost = $2, billing_mode = $3, billing_tier = $4, updated_at = NOW()
WHERE id = $1`

	// 冻结额只减不越界：实际花费高于预估是常态（计费档会向上漂，CLAUDE.md B3），
	// 减到 0 就停，绝不让 amount 变成负数去放大别的任务的可用额。
	decrementHoldSQL = `
UPDATE designkit_holds
SET amount = GREATEST(amount - $2, 0), updated_at = NOW()
WHERE job_id = $1 AND status = 'open'`
)

// ApplyItemBilling 把某一张的实际扣费回填进 job_items，并按张递减该 job 的 open 冻结额。
//
// ⚠ cost 的语义是「这一张**所有 attempt 的合计**」，不是「当前这次 attempt 花了多少」。
// 设计定型 6.2 写死 actual_cost = 所有 attempt 合计，重试一张 = 重新出一张图 =
// 重新收一次钱，前一次 attempt 花掉的钱一样要算进来。
// 调用方（结算 worker）枚举 attempt = 1..attempt_count 的 request_id 一次性查全，
// 加出来的那个数才是这里该传的值。
//
// **幂等 + 单调**，两条一起才安全：
//   - 幂等：重复回填同样的金额不会重复动冻结额（差额是 0，直接跳过）；
//   - 单调：billed_cost 只增不减。ApplyItemBilling 动冻结额用的是「新值 - 旧值」，
//     负差额会让 GREATEST(amount - 负数, 0) = amount + |差额|，
//     把冻结额**加**回去、凭空放大用户的可用额，接着就能透支。
//     账单只会越查越全、不会消失，所以「新值比旧值小」一定是别处出了问题，
//     这里保守地整条跳过（连 billing_mode / billing_tier 也不覆盖）。
//
// 第一次回填（billed_cost 还是 NULL）无论金额是不是 0 都要写下去 ——
// 否则上游按 0 元计费的那一张会永远停在 NULL，每一轮结算都白跑一次。
func (r *designkitRepository) ApplyItemBilling(ctx context.Context, itemID int64, cost dkdomain.Money, billingMode, billingTier string) error {
	newCost := dkdomain.QuantizeMoney(cost)

	return r.withTx(ctx, func(tx *sql.Tx) error {
		var jobID int64
		var old decimal.NullDecimal
		if err := tx.QueryRowContext(ctx, selectItemBillingForUpdateSQL, itemID).Scan(&jobID, &old); err != nil {
			return translate(err, "任务里的这一张")
		}

		oldCost := dkdomain.ZeroMoney
		if old.Valid {
			oldCost = old.Decimal
		}
		if old.Valid && !newCost.GreaterThan(oldCost) {
			return nil // 只增不减，见上面注释
		}

		if _, err := tx.ExecContext(ctx, updateItemBillingSQL,
			itemID, newCost, nullableString(billingMode), nullableString(billingTier)); err != nil {
			return err
		}

		delta := dkdomain.QuantizeMoney(newCost.Sub(oldCost))
		if delta.Sign() <= 0 {
			return nil
		}
		_, err := tx.ExecContext(ctx, decrementHoldSQL, jobID, delta)
		return err
	})
}

// settleSourceStatuses 结算允许的来源状态：白名单里能迁到目标终态的那些，
// 外加目标状态自己（结算 worker 重跑时不至于卡死）。
func settleSourceStatuses(to dkdomain.JobStatus) []string {
	sources := allowedSourceStatuses(to)
	if !containsString(sources, to.String()) {
		sources = append(sources, to.String())
	}
	return sources
}

// SettleJob 写 actual_cost + 终态 + settled_at，并把剩余 open 冻结额释放。
//
// **绝不能因为「实际 > 预估」就报错回滚** —— 钱已经被上游扣走了，
// 报错只会让任务卡死。超支走 InsertBillingAlert 分级记录。
func (r *designkitRepository) SettleJob(ctx context.Context, params dkdomain.SettleJobParams) error {
	if !params.Status.IsTerminal() {
		return illegalTransitionErr("任务", "settling", params.Status.String())
	}
	settledAt := params.SettledAt
	if settledAt.IsZero() {
		settledAt = time.Now()
	}

	return r.withTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `
UPDATE designkit_jobs SET
  actual_cost = $2,
  status = $3,
  settled_at = $4,
  finished_at = COALESCE(finished_at, $4),
  updated_at = NOW()
WHERE id = $1 AND settled_at IS NULL AND status = ANY($5)`,
			params.JobID,
			dkdomain.QuantizeMoney(params.ActualCost),
			params.Status.String(),
			settledAt,
			pq.Array(settleSourceStatuses(params.Status)))
		if err != nil {
			return err
		}
		affected, err := affectedRows(res)
		if err != nil {
			return err
		}
		if affected == 0 {
			return conflictErr(fmt.Sprintf("任务 %d 已经结算过、或状态不允许结算", params.JobID))
		}

		return releaseHold(ctx, tx, params.JobID)
	})
}

// ReleaseHold 把某个批次的冻结整笔释放（提交失败、僵尸回收时用）。
// 没有 open 的冻结行时**返回 nil**（幂等），不要报错。
func (r *designkitRepository) ReleaseHold(ctx context.Context, jobID int64) error {
	return releaseHold(ctx, r.sql, jobID)
}

func releaseHold(ctx context.Context, exec dkExecutor, jobID int64) error {
	_, err := exec.ExecContext(ctx, `
UPDATE designkit_holds SET amount = 0, status = 'released', updated_at = NOW()
WHERE job_id = $1 AND status = 'open'`, jobID)
	return err
}

func (r *designkitRepository) InsertBillingAlert(ctx context.Context, alert *dkdomain.BillingAlert) error {
	if alert == nil {
		return errors.New("designkit: 告警记录为空")
	}
	level := alert.Level
	if level == "" {
		level = dkdomain.BillingAlertLevelWarn
	}
	err := r.sql.QueryRowContext(ctx, `
INSERT INTO designkit_billing_alerts (job_id, estimated, actual, ratio_over, level)
VALUES ($1, $2, $3, $4, $5)
RETURNING id, created_at`,
		alert.JobID,
		dkdomain.QuantizeMoney(alert.Estimated),
		dkdomain.QuantizeMoney(alert.Actual),
		alert.RatioOver,
		level).Scan(&alert.ID, &alert.CreatedAt)
	if err != nil {
		return translate(err, "超支告警")
	}
	alert.Level = level
	return nil
}

// CreateQuotaRequest 运营点「申请额度」（决策 19）。
// 同一个人已有 pending 记录时部分唯一索引会挡下来 → ErrConflict，防止连点刷屏。
func (r *designkitRepository) CreateQuotaRequest(ctx context.Context, userID int64, note *string) (*dkdomain.QuotaRequest, error) {
	q, err := scanQuotaRequest(r.sql.QueryRowContext(ctx, `
INSERT INTO designkit_quota_requests (user_id, note, status)
VALUES ($1, $2, 'pending')
RETURNING `+quotaRequestColumns, userID, nullableStringPtr(note)))
	if err != nil {
		return nil, translate(err, "额度申请")
	}
	return q, nil
}

// buildListQuotaRequestsSQL 管理员后台的待办列表。status 为空表示不过滤。
func buildListQuotaRequestsSQL(filterByStatus bool) string {
	where := ""
	if filterByStatus {
		where = " WHERE status = $3"
	}
	return "SELECT " + quotaRequestColumns + " FROM designkit_quota_requests" + where +
		" ORDER BY created_at DESC, id DESC LIMIT $1 OFFSET $2"
}

func (r *designkitRepository) ListQuotaRequests(ctx context.Context, status dkdomain.QuotaRequestStatus, limit, offset int) ([]*dkdomain.QuotaRequest, error) {
	limit = clampLimit(limit, DefaultPageLimit, MaxPageLimit)
	if offset < 0 {
		offset = 0
	}
	args := []any{limit, offset}
	filterByStatus := status != ""
	if filterByStatus {
		args = append(args, status.String())
	}

	rows, err := r.sql.QueryContext(ctx, buildListQuotaRequestsSQL(filterByStatus), args...)
	if err != nil {
		return nil, err
	}
	defer closeRows(rows)
	return scanQuotaRequests(rows)
}

// quotaRequestDetailSQL 管理端列表：本体 + 申请人邮箱 + 处理详情。
//
// 前 7 列**必须**跟 quotaRequestColumns 同名同序（scanQuotaRequestDetail 先扫它们）。
// handle_note / approved_amount 是 9003 加的列，刻意不进那个常量
// （columns_parity_test.go 拿 9001 当唯一真相，见 9003 文件头）。
//
// JOIN users 只读邮箱，**绝不写 users**。LEFT JOIN + COALESCE：
// 申请人账号被删时行照样列出来（钱的事不能因为删号就凭空消失），邮箱给空串。
const quotaRequestDetailSQL = `
SELECT q.id, q.user_id, q.note, q.status, q.handled_by, q.handled_at, q.created_at,
       q.handle_note, q.approved_amount,
       COALESCE(u.email, ''),
       hu.email
FROM designkit_quota_requests q
LEFT JOIN users u ON u.id = q.user_id AND u.deleted_at IS NULL
LEFT JOIN users hu ON hu.id = q.handled_by AND hu.deleted_at IS NULL`

// buildListQuotaRequestDetailsSQL 两个 tab 各一条 WHERE：
// 待处理最老的排前面（先来先处理），处理过的最新的排前面（刚点完的在最上面）。
func buildListQuotaRequestDetailsSQL(pendingOnly bool) string {
	if pendingOnly {
		return quotaRequestDetailSQL + `
WHERE q.status = 'pending'
ORDER BY q.created_at ASC, q.id ASC
LIMIT $1 OFFSET $2`
	}
	return quotaRequestDetailSQL + `
WHERE q.status <> 'pending'
ORDER BY q.handled_at DESC, q.id DESC
LIMIT $1 OFFSET $2`
}

func scanQuotaRequestDetail(row rowScanner) (*dkdomain.QuotaRequestDetail, error) {
	var d dkdomain.QuotaRequestDetail
	var note sql.NullString
	var status string
	var handledBy sql.NullInt64
	var handledAt sql.NullTime
	var handleNote sql.NullString
	var approvedAmount decimal.NullDecimal
	var handledByEmail sql.NullString

	if err := row.Scan(
		&d.ID, &d.UserID, &note, &status, &handledBy, &handledAt, &d.CreatedAt,
		&handleNote, &approvedAmount, &d.RequesterEmail, &handledByEmail,
	); err != nil {
		return nil, err
	}

	d.Note = nullStringPtr(note)
	d.Status = dkdomain.QuotaRequestStatus(status)
	d.HandledBy = nullInt64Ptr(handledBy)
	d.HandledAt = nullTimePtr(handledAt)
	d.HandleNote = nullStringPtr(handleNote)
	if approvedAmount.Valid {
		amount := approvedAmount.Decimal
		d.ApprovedAmount = &amount
	}
	d.HandledByEmail = nullStringPtr(handledByEmail)
	return &d, nil
}

func (r *designkitRepository) ListQuotaRequestDetails(ctx context.Context, pendingOnly bool, limit, offset int) ([]*dkdomain.QuotaRequestDetail, error) {
	limit = clampLimit(limit, DefaultPageLimit, MaxPageLimit)
	if offset < 0 {
		offset = 0
	}
	rows, err := r.sql.QueryContext(ctx, buildListQuotaRequestDetailsSQL(pendingOnly), limit, offset)
	if err != nil {
		return nil, err
	}
	defer closeRows(rows)

	var out []*dkdomain.QuotaRequestDetail
	for rows.Next() {
		d, err := scanQuotaRequestDetail(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// CountPendingQuotaRequests 待处理条数（侧边栏红点用）。走 status 索引。
func (r *designkitRepository) CountPendingQuotaRequests(ctx context.Context) (int, error) {
	var count int
	err := r.sql.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM designkit_quota_requests WHERE status = 'pending'`).Scan(&count)
	if err != nil {
		return 0, err
	}
	return count, nil
}

func (r *designkitRepository) HandleQuotaRequest(ctx context.Context, id int64, handledBy int64, status dkdomain.QuotaRequestStatus, handleNote *string, approvedAmount *dkdomain.Money) (*dkdomain.QuotaRequest, error) {
	if status != dkdomain.QuotaRequestHandled && status != dkdomain.QuotaRequestRejected {
		return nil, fmt.Errorf("designkit: 处理额度申请只能置 handled 或 rejected，收到 %q: %w",
			status, dkdomain.ErrConflict)
	}
	// 金额存 NullDecimal：驳回的行是 NULL，不是 0 —— 0 会被读成「通过了但一分没加」。
	var amount decimal.NullDecimal
	if approvedAmount != nil {
		amount = decimal.NullDecimal{Decimal: dkdomain.QuantizeMoney(*approvedAmount), Valid: true}
	}
	// WHERE status='pending' 是「两个管理员同时点通过」的唯一防线：
	// 第二个人会打中 0 行 → ErrConflict，绝不会加第二次钱。
	q, err := scanQuotaRequest(r.sql.QueryRowContext(ctx, `
UPDATE designkit_quota_requests
SET status = $3, handled_by = $2, handled_at = NOW(), handle_note = $4, approved_amount = $5
WHERE id = $1 AND status = 'pending'
RETURNING `+quotaRequestColumns,
		id, handledBy, status.String(), nullableStringPtr(handleNote), amount))
	if err == nil {
		return q, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return nil, translate(err, "额度申请")
	}

	// 打中 0 行有两种可能，分开报错，管理员才知道是「没这条」还是「已经被处理过」。
	var exists bool
	if scanErr := r.sql.QueryRowContext(ctx,
		`SELECT EXISTS(SELECT 1 FROM designkit_quota_requests WHERE id = $1)`, id).Scan(&exists); scanErr != nil {
		return nil, scanErr
	}
	if !exists {
		return nil, notFoundErr("额度申请")
	}
	return nil, conflictErr(fmt.Sprintf("额度申请 %d 已经被处理过了", id))
}

// ReopenQuotaRequest 把一条刚置成 handled 的申请退回 pending（补偿用）。
//
// 只认 handled 的行：rejected 不该被退回（驳回没有后续动作会失败）。
// 退回可能撞「同人只能有一条 pending」的部分唯一索引（申请人已另提一条新申请），
// translate 会把 23505 翻成 ErrConflict，调用方据此换文案。
func (r *designkitRepository) ReopenQuotaRequest(ctx context.Context, id int64) error {
	res, err := r.sql.ExecContext(ctx, `
UPDATE designkit_quota_requests
SET status = 'pending', handled_by = NULL, handled_at = NULL, handle_note = NULL, approved_amount = NULL
WHERE id = $1 AND status = 'handled'`, id)
	if err != nil {
		return translate(err, "额度申请")
	}
	affected, err := affectedRows(res)
	if err != nil {
		return err
	}
	if affected != 1 {
		return notFoundErr("额度申请")
	}
	return nil
}

// designkitUsageLogRequestIDPattern 是「这条账单是 designkit 出图产生的」的判据：
// usage_logs.request_id 以 "client:dki:" 开头。
//
// 拼法从 domain 的两个常量来，不许在 SQL 里手写字面量 ——
// 前缀哪天改了，手写的那个不会报错，只会让工作台上的「本月花费」悄悄变成 0。
// 前缀里没有 % 和 _，不需要 ESCAPE。
var designkitUsageLogRequestIDPattern = dkdomain.UpstreamClientRequestIDPrefix +
	dkdomain.BillingRequestIDPrefix + ":%"

// usageSummarySQL 见 GetUsageSummary 的注释。
const usageSummarySQL = `
SELECT
  (SELECT COUNT(*)
     FROM designkit_images i
     JOIN designkit_jobs ij ON ij.id = i.job_id
    WHERE ij.user_id = $1 AND i.deleted_at IS NULL
      AND i.created_at >= $2 AND i.created_at < $3),
  (SELECT COALESCE(SUM(ul.actual_cost), 0)
     FROM usage_logs ul
    WHERE ul.user_id = $1
      AND ul.request_id LIKE $4
      AND ul.created_at >= $2 AND ul.created_at < $3),
  u.balance,
  u.balance - COALESCE((
    SELECT SUM(h.amount) FROM designkit_holds h
    WHERE h.user_id = u.id AND h.status = 'open'
  ), 0)
FROM users u
WHERE u.id = $1 AND u.deleted_at IS NULL`

// GetUsageSummary 工作台角落那三个数：本月出图 N 张 / 花费 $X / 余额 $Y（决策 16、18）。
//
// 张数按**结果图**算（重试出的每一张都真金白银花过钱，所以历史版本也算在内）。
// designkit_images 的行是一次 attempt 出一行、created_at 之后再也不会变，
// 所以张数天然落在正确的月份里。
//
// 【花费为什么不按 job_items 算】
// 旧写法是 SUM(job_items.billed_cost) 且窗口落在 job_items.finished_at 上，
// 有两个错：
//   - finished_at **会被重试刷新**。8 月出的图，9 月运营点一次重试，
//     那一整张（含 8 月那次 attempt 的钱）就整个跑到 9 月去了，月度对账对不上；
//   - billed_cost 是「所有 attempt 的合计」，本来就没法按月拆。
//
// 改成直接按 usage_logs 自己的 created_at 落窗口：**每一笔钱记在它真正被扣的那个月**，
// 跨月重试不会再挪动账目。顺带把「已经扣了钱但批次还没结算完」的部分也算进来了 ——
// 钱确实已经花了，工作台如实显示更好。
// 判据是 request_id 前缀（designkitUsageLogRequestIDPattern），
// 所以只统计 designkit 出的图，用户在别处的消费不会混进来。
// 走 idx_usage_logs_user_created (user_id, created_at)。
func (r *designkitRepository) GetUsageSummary(ctx context.Context, userID int64, from, to time.Time) (*dkdomain.UsageSummary, error) {
	summary := &dkdomain.UsageSummary{Currency: dkdomain.CurrencyUSD}
	err := r.sql.QueryRowContext(ctx, usageSummarySQL,
		userID, from, to, designkitUsageLogRequestIDPattern).
		Scan(&summary.ImageCount, &summary.Cost, &summary.Balance, &summary.Available)
	if err != nil {
		return nil, translate(err, "用户")
	}
	return summary, nil
}
