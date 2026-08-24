package repository

// 列清单 + 扫描函数。
//
// **每个 xxxColumns 常量的列名和顺序必须跟 backend/migrations/9001_designkit_init.sql
// 一模一样**（那个文件已经跑过、永不可改，列名以它为准）。
// columns_parity_test.go 会去解析那份迁移 SQL 逐列比对，改错了单测当场红。
//
// 可空列一律扫进 sql.NullXxx / decimal.NullDecimal，再转成指针填进实体
// （domain/doc.go 的全模块约定：可空列用指针，不用 sql.NullXxx，
// 因为后者 JSON 序列化出来是 {"String":"","Valid":false} 这种发给 ERP 会出事的形状）。

import (
	"database/sql"
	"encoding/json"
	"time"

	"github.com/shopspring/decimal"

	dkdomain "github.com/Wei-Shaw/sub2api/internal/designkit/domain"
)

// ---- 列清单 ----

// jobColumns 末尾的 keep_transparency 来自 9004 的 ALTER（物理列序就是追加在
// 最后），parity 测试按「9001 建表列 + 9004 追加列」比对。
const jobColumns = `id, uid, user_id, api_key_id, origin, name, status, ratio, model, ` +
	`item_count, success_count, fail_count, cancelled_count, ` +
	`idempotency_key, callback_url, currency, estimated_cost, actual_cost, ` +
	`base_unit_price, group_rate_multiplier, account_rate_multiplier, ` +
	`billing_mode, pricing_tier, price_source, pricing_snapshot_version, ` +
	`revision, last_error_code, last_error_message, ` +
	`heartbeat_at, cancel_requested_at, expires_at, ` +
	`created_at, updated_at, started_at, finished_at, settled_at, user_deleted_at, ` +
	`keep_transparency`

const jobItemColumns = `id, job_id, seq, asset_id, variant_id, prompt_id, prompt_text, status, ` +
	`attempt_count, max_attempts, last_error_code, last_error_message, ` +
	`billed_cost, billing_request_id, billing_mode, billing_tier, ` +
	`worker_id, lease_expires_at, available_at, ` +
	`created_at, updated_at, started_at, finished_at`

const imageColumns = `id, uid, job_id, item_id, attempt, image_index, is_current, ` +
	`object_key, content_type, byte_size, width, height, billing_tier, ` +
	`created_at, deleted_at, expires_at`

const assetColumns = `id, uid, user_id, object_key, content_type, byte_size, ` +
	`width, height, sha256, origin, created_at, deleted_at`

const assetVariantColumns = `id, asset_id, ratio, keep_transparency, max_dimension, ` +
	`object_key, width, height, content_type, created_at`

const promptCategoryColumns = `id, slug, name_zh, name_en, sort_order, created_at, updated_at`

const promptColumns = `id, uid, category_id, title, body, variables, preview_url, ` +
	`source, source_ref, owner_user_id, is_enabled, created_at, updated_at, deleted_at`

const quotaRequestColumns = `id, user_id, note, status, handled_by, handled_at, created_at`

const syncRunColumns = `id, kind, status, started_at, finished_at, ` +
	`fetched, inserted, updated, skipped, error`

const settingColumns = `key, value, updated_at`

// ---- 扫描 ----

func scanJob(row rowScanner) (*dkdomain.Job, error) {
	var job dkdomain.Job
	var origin, status, ratio string
	var idempotencyKey, callbackURL sql.NullString
	var actualCost decimal.NullDecimal
	var lastErrorCode, lastErrorMessage sql.NullString
	var heartbeatAt, cancelRequestedAt, expiresAt sql.NullTime
	var startedAt, finishedAt, settledAt, userDeletedAt sql.NullTime

	if err := row.Scan(
		&job.ID, &job.UID, &job.UserID, &job.APIKeyID, &origin, &job.Name, &status, &ratio, &job.Model,
		&job.ItemCount, &job.SuccessCount, &job.FailCount, &job.CancelledCount,
		&idempotencyKey, &callbackURL, &job.Currency, &job.EstimatedCost, &actualCost,
		&job.BaseUnitPrice, &job.GroupRateMultiplier, &job.AccountRateMultiplier,
		&job.BillingMode, &job.PricingTier, &job.PriceSource, &job.PricingSnapshotVersion,
		&job.Revision, &lastErrorCode, &lastErrorMessage,
		&heartbeatAt, &cancelRequestedAt, &expiresAt,
		&job.CreatedAt, &job.UpdatedAt, &startedAt, &finishedAt, &settledAt, &userDeletedAt,
		&job.KeepTransparency,
	); err != nil {
		return nil, err
	}

	job.Origin = dkdomain.Origin(origin)
	job.Status = dkdomain.JobStatus(status)
	job.Ratio = dkdomain.Ratio(ratio)
	job.IdempotencyKey = nullStringPtr(idempotencyKey)
	job.CallbackURL = nullStringPtr(callbackURL)
	job.ActualCost = nullDecimalPtr(actualCost)
	job.LastErrorCode = nullStringPtr(lastErrorCode)
	job.LastErrorMessage = nullStringPtr(lastErrorMessage)
	job.HeartbeatAt = nullTimePtr(heartbeatAt)
	job.CancelRequestedAt = nullTimePtr(cancelRequestedAt)
	job.ExpiresAt = nullTimePtr(expiresAt)
	job.StartedAt = nullTimePtr(startedAt)
	job.FinishedAt = nullTimePtr(finishedAt)
	job.SettledAt = nullTimePtr(settledAt)
	job.UserDeletedAt = nullTimePtr(userDeletedAt)
	return &job, nil
}

func scanJobs(rows *sql.Rows) ([]*dkdomain.Job, error) {
	var out []*dkdomain.Job
	for rows.Next() {
		job, err := scanJob(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, job)
	}
	return out, rows.Err()
}

func scanJobItem(row rowScanner) (*dkdomain.JobItem, error) {
	var item dkdomain.JobItem
	var status string
	var assetID, variantID, promptID sql.NullInt64
	var lastErrorCode, lastErrorMessage sql.NullString
	var billedCost decimal.NullDecimal
	var billingRequestID, billingMode, billingTier, workerID sql.NullString
	var leaseExpiresAt, startedAt, finishedAt sql.NullTime

	if err := row.Scan(
		&item.ID, &item.JobID, &item.Seq, &assetID, &variantID, &promptID, &item.PromptText, &status,
		&item.AttemptCount, &item.MaxAttempts, &lastErrorCode, &lastErrorMessage,
		&billedCost, &billingRequestID, &billingMode, &billingTier,
		&workerID, &leaseExpiresAt, &item.AvailableAt,
		&item.CreatedAt, &item.UpdatedAt, &startedAt, &finishedAt,
	); err != nil {
		return nil, err
	}

	item.Status = dkdomain.ItemStatus(status)
	item.AssetID = nullInt64Ptr(assetID)
	item.VariantID = nullInt64Ptr(variantID)
	item.PromptID = nullInt64Ptr(promptID)
	item.LastErrorCode = nullStringPtr(lastErrorCode)
	item.LastErrorMessage = nullStringPtr(lastErrorMessage)
	item.BilledCost = nullDecimalPtr(billedCost)
	item.BillingRequestID = nullStringPtr(billingRequestID)
	item.BillingMode = nullStringPtr(billingMode)
	item.BillingTier = nullStringPtr(billingTier)
	item.WorkerID = nullStringPtr(workerID)
	item.LeaseExpiresAt = nullTimePtr(leaseExpiresAt)
	item.StartedAt = nullTimePtr(startedAt)
	item.FinishedAt = nullTimePtr(finishedAt)
	return &item, nil
}

func scanJobItems(rows *sql.Rows) ([]*dkdomain.JobItem, error) {
	var out []*dkdomain.JobItem
	for rows.Next() {
		item, err := scanJobItem(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

func scanImage(row rowScanner) (*dkdomain.Image, error) {
	var img dkdomain.Image
	var width, height sql.NullInt64
	var billingTier sql.NullString
	var deletedAt, expiresAt sql.NullTime

	if err := row.Scan(
		&img.ID, &img.UID, &img.JobID, &img.ItemID, &img.Attempt, &img.ImageIndex, &img.IsCurrent,
		&img.ObjectKey, &img.ContentType, &img.ByteSize, &width, &height, &billingTier,
		&img.CreatedAt, &deletedAt, &expiresAt,
	); err != nil {
		return nil, err
	}

	img.Width = nullIntPtr(width)
	img.Height = nullIntPtr(height)
	img.BillingTier = nullStringPtr(billingTier)
	img.DeletedAt = nullTimePtr(deletedAt)
	img.ExpiresAt = nullTimePtr(expiresAt)
	return &img, nil
}

func scanImages(rows *sql.Rows) ([]*dkdomain.Image, error) {
	var out []*dkdomain.Image
	for rows.Next() {
		img, err := scanImage(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, img)
	}
	return out, rows.Err()
}

func scanAsset(row rowScanner) (*dkdomain.Asset, error) {
	var asset dkdomain.Asset
	var origin string
	var width, height sql.NullInt64
	var sha256 sql.NullString
	var deletedAt sql.NullTime

	if err := row.Scan(
		&asset.ID, &asset.UID, &asset.UserID, &asset.ObjectKey, &asset.ContentType, &asset.ByteSize,
		&width, &height, &sha256, &origin, &asset.CreatedAt, &deletedAt,
	); err != nil {
		return nil, err
	}

	asset.Width = nullIntPtr(width)
	asset.Height = nullIntPtr(height)
	asset.SHA256 = nullStringPtr(sha256)
	asset.Origin = dkdomain.Origin(origin)
	asset.DeletedAt = nullTimePtr(deletedAt)
	return &asset, nil
}

func scanAssets(rows *sql.Rows) ([]*dkdomain.Asset, error) {
	var out []*dkdomain.Asset
	for rows.Next() {
		asset, err := scanAsset(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, asset)
	}
	return out, rows.Err()
}

func scanAssetVariant(row rowScanner) (*dkdomain.AssetVariant, error) {
	var v dkdomain.AssetVariant
	var ratio string
	var width, height sql.NullInt64

	if err := row.Scan(
		&v.ID, &v.AssetID, &ratio, &v.KeepTransparency, &v.MaxDimension,
		&v.ObjectKey, &width, &height, &v.ContentType, &v.CreatedAt,
	); err != nil {
		return nil, err
	}

	v.Ratio = dkdomain.Ratio(ratio)
	v.Width = nullIntPtr(width)
	v.Height = nullIntPtr(height)
	return &v, nil
}

func scanPromptCategory(row rowScanner) (*dkdomain.PromptCategory, error) {
	var c dkdomain.PromptCategory
	if err := row.Scan(
		&c.ID, &c.Slug, &c.NameZH, &c.NameEN, &c.SortOrder, &c.CreatedAt, &c.UpdatedAt,
	); err != nil {
		return nil, err
	}
	return &c, nil
}

func scanPromptCategories(rows *sql.Rows) ([]*dkdomain.PromptCategory, error) {
	var out []*dkdomain.PromptCategory
	for rows.Next() {
		c, err := scanPromptCategory(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func scanPrompt(row rowScanner) (*dkdomain.Prompt, error) {
	var p dkdomain.Prompt
	var categoryID, ownerUserID sql.NullInt64
	var variables []byte
	var previewURL, sourceRef sql.NullString
	var source string
	var deletedAt sql.NullTime

	if err := row.Scan(
		&p.ID, &p.UID, &categoryID, &p.Title, &p.Body, &variables, &previewURL,
		&source, &sourceRef, &ownerUserID, &p.IsEnabled, &p.CreatedAt, &p.UpdatedAt, &deletedAt,
	); err != nil {
		return nil, err
	}

	p.CategoryID = nullInt64Ptr(categoryID)
	p.PreviewURL = nullStringPtr(previewURL)
	p.Source = dkdomain.PromptSource(source)
	p.SourceRef = nullStringPtr(sourceRef)
	p.OwnerUserID = nullInt64Ptr(ownerUserID)
	p.DeletedAt = nullTimePtr(deletedAt)

	vars, err := decodePromptVariables(variables)
	if err != nil {
		return nil, err
	}
	p.Variables = vars
	return &p, nil
}

func scanPrompts(rows *sql.Rows) ([]*dkdomain.Prompt, error) {
	var out []*dkdomain.Prompt
	for rows.Next() {
		p, err := scanPrompt(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func scanQuotaRequest(row rowScanner) (*dkdomain.QuotaRequest, error) {
	var q dkdomain.QuotaRequest
	var note sql.NullString
	var status string
	var handledBy sql.NullInt64
	var handledAt sql.NullTime

	if err := row.Scan(
		&q.ID, &q.UserID, &note, &status, &handledBy, &handledAt, &q.CreatedAt,
	); err != nil {
		return nil, err
	}

	q.Note = nullStringPtr(note)
	q.Status = dkdomain.QuotaRequestStatus(status)
	q.HandledBy = nullInt64Ptr(handledBy)
	q.HandledAt = nullTimePtr(handledAt)
	return &q, nil
}

func scanQuotaRequests(rows *sql.Rows) ([]*dkdomain.QuotaRequest, error) {
	var out []*dkdomain.QuotaRequest
	for rows.Next() {
		q, err := scanQuotaRequest(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, q)
	}
	return out, rows.Err()
}

func scanSyncRun(row rowScanner) (*dkdomain.SyncRun, error) {
	var run dkdomain.SyncRun
	var kind, status string
	var finishedAt sql.NullTime
	var errMsg sql.NullString

	if err := row.Scan(
		&run.ID, &kind, &status, &run.StartedAt, &finishedAt,
		&run.Fetched, &run.Inserted, &run.Updated, &run.Skipped, &errMsg,
	); err != nil {
		return nil, err
	}

	run.Kind = dkdomain.SyncKind(kind)
	run.Status = dkdomain.SyncStatus(status)
	run.FinishedAt = nullTimePtr(finishedAt)
	run.Error = nullStringPtr(errMsg)
	return &run, nil
}

func scanSetting(row rowScanner) (*dkdomain.Setting, error) {
	var s dkdomain.Setting
	var value []byte
	if err := row.Scan(&s.Key, &value, &s.UpdatedAt); err != nil {
		return nil, err
	}
	// 复制一份：驱动给的字节切片在下一次 Scan 后可能被复用。
	s.Value = append(json.RawMessage(nil), value...)
	return &s, nil
}

func scanSettings(rows *sql.Rows) ([]*dkdomain.Setting, error) {
	var out []*dkdomain.Setting
	for rows.Next() {
		s, err := scanSetting(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// ---- 可空列的转换 ----

func nullStringPtr(v sql.NullString) *string {
	if !v.Valid {
		return nil
	}
	s := v.String
	return &s
}

func nullInt64Ptr(v sql.NullInt64) *int64 {
	if !v.Valid {
		return nil
	}
	i := v.Int64
	return &i
}

func nullIntPtr(v sql.NullInt64) *int {
	if !v.Valid {
		return nil
	}
	i := int(v.Int64)
	return &i
}

func nullTimePtr(v sql.NullTime) *time.Time {
	if !v.Valid {
		return nil
	}
	t := v.Time
	return &t
}

// nullDecimalPtr 把可空金额列转成 *Money。
// Money 是 decimal.Decimal 的类型别名，所以 *decimal.Decimal 就是 *Money。
func nullDecimalPtr(v decimal.NullDecimal) *dkdomain.Money {
	if !v.Valid {
		return nil
	}
	m := v.Decimal
	return &m
}

// ---- JSONB ----

// decodePromptVariables 把 variables 这一列解出来。
// 列上有 NOT NULL DEFAULT '[]'，但历史数据/手工改库都可能给空，兜住不炸。
func decodePromptVariables(raw []byte) ([]dkdomain.PromptVariable, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var vars []dkdomain.PromptVariable
	if err := json.Unmarshal(raw, &vars); err != nil {
		return nil, err
	}
	return vars, nil
}

// encodePromptVariables 把变量定义编码成 JSONB 参数。
// nil 一律写成 []，不写 null —— 列是 NOT NULL。
func encodePromptVariables(vars []dkdomain.PromptVariable) ([]byte, error) {
	if len(vars) == 0 {
		return []byte("[]"), nil
	}
	return json.Marshal(vars)
}

// nullableString 把空串转成 NULL 参数。
// 用在 billing_mode / billing_tier 这类「有值才写」的可空列上。
func nullableString(s string) any {
	if s == "" {
		return nil
	}
	return s
}
