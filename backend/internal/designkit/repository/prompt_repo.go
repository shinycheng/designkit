package repository

// 灵感库：分类（designkit_prompt_categories）和提示词（designkit_prompts）。
//
// 两个坑，写 SQL 前先看：
//  1. **关键词搜索一律 ILIKE** —— PostgreSQL 的 LIKE 区分大小写，
//     用 LIKE 会出现「搜 dress 搜不到 Dress」这种运营根本想不到的结果；
//  2. source_ref 上是**部分**唯一索引，写 ON CONFLICT 必须把 WHERE 谓词一起带上，
//     否则 PostgreSQL 推断不出用哪个索引，直接报错。

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/lib/pq"

	dkdomain "github.com/Wei-Shaw/sub2api/internal/designkit/domain"
)

// promptSourceRefConflictTarget 是那条部分唯一索引的冲突目标。
// **WHERE 谓词必须跟 9001 迁移里的索引定义一字不差。**
const promptSourceRefConflictTarget = `ON CONFLICT (source_ref) WHERE source_ref IS NOT NULL AND source_ref <> ''`

// ---- 分类 ----

const listCategoriesSQL = `SELECT ` + promptCategoryColumns +
	` FROM designkit_prompt_categories ORDER BY sort_order ASC, id ASC`

func (r *designkitRepository) ListCategories(ctx context.Context) ([]*dkdomain.PromptCategory, error) {
	rows, err := r.sql.QueryContext(ctx, listCategoriesSQL)
	if err != nil {
		return nil, err
	}
	defer closeRows(rows)
	return scanPromptCategories(rows)
}

// UpsertCategory 按 slug 幂等写入。slug 是稳定的英文标识，同步时靠它认人，
// 中文名改了也不会变成一条新分类。
func (r *designkitRepository) UpsertCategory(ctx context.Context, category *dkdomain.PromptCategory) (*dkdomain.PromptCategory, error) {
	if category == nil || strings.TrimSpace(category.Slug) == "" {
		return nil, dkdomain.NewError(dkdomain.ErrCodeInvalidRequest).
			WithMessage("分类缺少标识（slug），无法保存。")
	}
	c, err := scanPromptCategory(r.sql.QueryRowContext(ctx, `
INSERT INTO designkit_prompt_categories (slug, name_zh, name_en, sort_order)
VALUES ($1, $2, $3, $4)
ON CONFLICT (slug) DO UPDATE SET
  name_zh = EXCLUDED.name_zh,
  name_en = EXCLUDED.name_en,
  sort_order = EXCLUDED.sort_order,
  updated_at = NOW()
RETURNING `+promptCategoryColumns,
		category.Slug, category.NameZH, category.NameEN, category.SortOrder))
	if err != nil {
		return nil, translate(err, "分类")
	}
	return c, nil
}

// ---- 提示词检索 ----

// buildPromptFilter 拼检索条件。withCursor=false 给 CountPrompts 用
// （总数不能受游标影响，否则界面上的「共 N 条」会随翻页变小）。
func buildPromptFilter(query dkdomain.ListPromptsQuery, withCursor bool) (string, []any) {
	args := make([]any, 0, 5)
	var b strings.Builder
	_, _ = b.WriteString(" WHERE deleted_at IS NULL")

	if query.CategoryID != nil {
		args = append(args, *query.CategoryID)
		fmt.Fprintf(&b, " AND category_id = $%d", len(args))
	}
	if kw := strings.TrimSpace(query.Keyword); kw != "" {
		// ILIKE：PostgreSQL 的 LIKE 区分大小写。
		// 通配符要转义，否则运营打一个 % 就等于全表匹配。
		args = append(args, likeContains(kw))
		fmt.Fprintf(&b, " AND (title ILIKE $%d OR body ILIKE $%d)", len(args), len(args))
	}
	if query.OwnerUserID != nil {
		args = append(args, *query.OwnerUserID)
		fmt.Fprintf(&b, " AND owner_user_id = $%d", len(args))
	}
	if query.OnlyEnabled {
		_, _ = b.WriteString(" AND is_enabled")
	}
	if withCursor && query.CursorID > 0 {
		args = append(args, query.CursorID)
		fmt.Fprintf(&b, " AND id > $%d", len(args))
	}
	return b.String(), args
}

// buildListPromptsQuery 提示词检索。游标按 id 升序推进（灵感库是稳定的目录型数据，
// 用 id 就够，不需要复合游标）。
func buildListPromptsQuery(query dkdomain.ListPromptsQuery) (string, []any) {
	where, args := buildPromptFilter(query, true)
	args = append(args, clampLimit(query.Limit, DefaultPageLimit, MaxPageLimit))
	return "SELECT " + promptColumns + " FROM designkit_prompts" + where +
		fmt.Sprintf(" ORDER BY id ASC LIMIT $%d", len(args)), args
}

func buildCountPromptsQuery(query dkdomain.ListPromptsQuery) (string, []any) {
	where, args := buildPromptFilter(query, false)
	return "SELECT COUNT(*) FROM designkit_prompts" + where, args
}

// buildSamplePromptsQuery 随机取若干条。
//
// ⚠ 为什么需要「随机」这件事本身（2026-08-14，monica 问出来的）：
//
//	「AI 挑提示词」要先取一批候选给模型挑。原来用的是 ListPrompts，
//	而它是 `ORDER BY id ASC LIMIT n` —— 取的永远是**同样那 n 条**（最早同步进来的）。
//	社交媒体帖子有 3978 条，取 100 条意味着其中 3878 条（97%）**永远不可能被选中**，
//	运营写什么商品特点都改变不了。而且这个「不公平」是完全静默的：
//	界面上看不出来，日志里也没有。
//
//	关键词预筛本来是想解决这件事的，但实测发现它在这个库上根本不生效：
//	15045 条标题里 15016 条是**纯英文**，运营打的中文词（汽水、开衫、饮料）
//	一条都匹配不上，于是每次都落到兜底分支，回到「永远是同样那 100 条」。
//
// ORDER BY random() 在这个量级上是安全的：单个分类最多 3978 行，
// 整库 15045 行，一次全扫也就几毫秒。真到几十万条再换 TABLESAMPLE。
func buildSamplePromptsQuery(query dkdomain.ListPromptsQuery) (string, []any) {
	// withCursor=false：随机取跟游标没有关系，带上只会把候选池又缩回「id 之后那一段」。
	where, args := buildPromptFilter(query, false)
	args = append(args, clampLimit(query.Limit, DefaultPageLimit, MaxPageLimit))
	return "SELECT " + promptColumns + " FROM designkit_prompts" + where +
		fmt.Sprintf(" ORDER BY random() LIMIT $%d", len(args)), args
}

// promptDigestBriefChars 正文开头截多少个字符当简介的原料。
//
// 200 足够覆盖 JSON 形态的 "type" 字段（它总在最前面）和散文形态的第一句。
// 再多只是把粗筛的提示词撑大 —— 3978 条 × 每多 100 字符就是 40 万字符。
const promptDigestBriefChars = 200

// ListPromptDigests 取一个分类下**全部**提示词的简介（不拉正文全文）。
//
// ⚠ 这是「AI 挑提示词」粗筛阶段的数据源，跟 ListPrompts 有三点不同：
//   - **不受 MaxPageLimit(100) 约束**：粗筛就是要看全量，卡在 100 条等于没筛；
//   - 只取 id / uid / title / 正文前 200 字符 —— 正文平均 1616 字，
//     3978 条拉全文是 640 万字符，既传不动也没必要；
//   - 按 id 升序，**顺序稳定**。粗筛要的是「全都看一遍」，不是取样，
//     所以这里不需要（也不应该）随机。
//
// limit 是防炸上限，不是业务规则：最大的分类 3978 条，给 5000 足够，
// 真有一天单个分类几万条，粗筛这条路本身就要重新设计了。
func (r *designkitRepository) ListPromptDigests(ctx context.Context, categoryID *int64, limit int) ([]*dkdomain.PromptDigest, error) {
	args := make([]any, 0, 3)
	var b strings.Builder
	_, _ = b.WriteString(" WHERE deleted_at IS NULL AND is_enabled")
	if categoryID != nil {
		args = append(args, *categoryID)
		fmt.Fprintf(&b, " AND category_id = $%d", len(args))
	}
	if limit <= 0 {
		limit = 5000
	}
	args = append(args, promptDigestBriefChars)
	briefArg := len(args)
	args = append(args, limit)

	sqlText := fmt.Sprintf(
		"SELECT id, uid, title, LEFT(body, $%d) FROM designkit_prompts%s ORDER BY id ASC LIMIT $%d",
		briefArg, b.String(), len(args))

	rows, err := r.sql.QueryContext(ctx, sqlText, args...)
	if err != nil {
		return nil, err
	}
	defer closeRows(rows)

	out := make([]*dkdomain.PromptDigest, 0, 256)
	for rows.Next() {
		var d dkdomain.PromptDigest
		if err := rows.Scan(&d.ID, &d.UID, &d.Title, &d.Brief); err != nil {
			return nil, err
		}
		out = append(out, &d)
	}
	return out, rows.Err()
}

// SamplePrompts 在筛选条件下随机取若干条。
//
// 跟 ListPrompts 的唯一区别是排序：那个按 id 升序（给分页用，顺序必须稳定），
// 这个按 random()（给「挑候选」用，公平比稳定重要）。
//
// **不要拿它做分页**：每次顺序都不一样，翻页会重复和遗漏。
func (r *designkitRepository) SamplePrompts(ctx context.Context, query dkdomain.ListPromptsQuery) ([]*dkdomain.Prompt, error) {
	sqlText, args := buildSamplePromptsQuery(query)
	rows, err := r.sql.QueryContext(ctx, sqlText, args...)
	if err != nil {
		return nil, err
	}
	defer closeRows(rows)
	return scanPrompts(rows)
}

func (r *designkitRepository) ListPrompts(ctx context.Context, query dkdomain.ListPromptsQuery) ([]*dkdomain.Prompt, error) {
	sqlText, args := buildListPromptsQuery(query)
	rows, err := r.sql.QueryContext(ctx, sqlText, args...)
	if err != nil {
		return nil, err
	}
	defer closeRows(rows)
	return scanPrompts(rows)
}

func (r *designkitRepository) CountPrompts(ctx context.Context, query dkdomain.ListPromptsQuery) (int, error) {
	sqlText, args := buildCountPromptsQuery(query)
	var total int
	if err := r.sql.QueryRowContext(ctx, sqlText, args...).Scan(&total); err != nil {
		return 0, err
	}
	return total, nil
}

// countPromptsByCategorySQL 分类页的条数统计。
//
// %s 处填 is_enabled 的过滤条件（拼的是常量，不接受外部输入，没有注入面）。
// 显式 ORDER BY 是本包的铁律，虽然这里返回的是 map、顺序无所谓，
// 但「所有返回列表的查询都必须显式 ORDER BY」这条不开口子。
const countPromptsByCategorySQL = `
SELECT category_id, COUNT(*) FROM designkit_prompts
WHERE deleted_at IS NULL%s
GROUP BY category_id
ORDER BY category_id ASC NULLS LAST`

// CountPromptsByCategory 每个分类下有多少条。category_id 为空的归到键 0。
func (r *designkitRepository) CountPromptsByCategory(ctx context.Context, onlyEnabled bool) (map[int64]int, error) {
	filter := ""
	if onlyEnabled {
		filter = " AND is_enabled"
	}
	rows, err := r.sql.QueryContext(ctx, fmt.Sprintf(countPromptsByCategorySQL, filter))
	if err != nil {
		return nil, err
	}
	defer closeRows(rows)

	// 12 = 灵感库固定的 11 个分类 + 一个「没有分类」。
	out := make(map[int64]int, 12)
	for rows.Next() {
		var categoryID sql.NullInt64
		var count int
		if err := rows.Scan(&categoryID, &count); err != nil {
			return nil, err
		}
		var key int64
		if categoryID.Valid {
			key = categoryID.Int64
		}
		out[key] += count
	}
	return out, rows.Err()
}

func (r *designkitRepository) GetPromptByUID(ctx context.Context, uid string) (*dkdomain.Prompt, error) {
	p, err := scanPrompt(r.sql.QueryRowContext(ctx,
		`SELECT `+promptColumns+` FROM designkit_prompts WHERE uid = $1 AND deleted_at IS NULL`, uid))
	if err != nil {
		return nil, translate(err, "提示词")
	}
	return p, nil
}

// GetPromptsByIDs 批量取，**返回顺序跟传进来的 ids 严格一致**。
// 展开顺序是对外契约的一部分（图1×词1、图1×词2…），少一条就整体错位，
// 所以取不到一律 ErrNotFound，不做静默跳过。
//
// 注意这里**不过滤 is_enabled**：历史任务用过的词后来被下架了，
// 详情页仍要显示当时那一条。
func (r *designkitRepository) GetPromptsByIDs(ctx context.Context, ids []int64) ([]*dkdomain.Prompt, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	rows, err := r.sql.QueryContext(ctx,
		`SELECT `+promptColumns+` FROM designkit_prompts
		 WHERE id = ANY($1) AND deleted_at IS NULL ORDER BY id ASC`, pq.Array(ids))
	if err != nil {
		return nil, err
	}
	defer closeRows(rows)

	prompts, err := scanPrompts(rows)
	if err != nil {
		return nil, err
	}
	byID := make(map[int64]*dkdomain.Prompt, len(prompts))
	for _, p := range prompts {
		byID[p.ID] = p
	}

	out := make([]*dkdomain.Prompt, 0, len(ids))
	for _, id := range ids {
		p, ok := byID[id]
		if !ok {
			return nil, fmt.Errorf("designkit: 提示词 %d 不存在或已删除: %w", id, dkdomain.ErrNotFound)
		}
		out = append(out, p)
	}
	return out, nil
}

// ---- 运营自己存的提示词 ----

func (r *designkitRepository) CreatePrompt(ctx context.Context, prompt *dkdomain.Prompt) error {
	if prompt == nil {
		return fmt.Errorf("designkit: 提示词为空: %w", dkdomain.ErrNotFound)
	}
	source := prompt.Source
	if source == "" {
		source = dkdomain.PromptSourceUser
	}
	if source == dkdomain.PromptSourceUser && prompt.OwnerUserID == nil {
		return dkdomain.NewError(dkdomain.ErrCodeInvalidRequest).
			WithMessage("自己存的提示词必须有归属人。")
	}
	vars, err := encodePromptVariables(prompt.Variables)
	if err != nil {
		return err
	}

	created, err := scanPrompt(r.sql.QueryRowContext(ctx, `
INSERT INTO designkit_prompts (
  uid, category_id, title, body, variables, preview_url, source, source_ref, owner_user_id, is_enabled
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
RETURNING `+promptColumns,
		prompt.UID, prompt.CategoryID, prompt.Title, prompt.Body, vars,
		prompt.PreviewURL, source.String(), nullableStringPtr(prompt.SourceRef),
		prompt.OwnerUserID, prompt.IsEnabled))
	if err != nil {
		return translate(err, "提示词")
	}
	*prompt = *created
	return nil
}

// UpdatePrompt 改一条。带 OwnerUserID 时会校验归属（运营只能改自己的）；
// 不带时是管理员路径，只按 uid 定位。
func (r *designkitRepository) UpdatePrompt(ctx context.Context, prompt *dkdomain.Prompt) error {
	if prompt == nil || strings.TrimSpace(prompt.UID) == "" {
		return fmt.Errorf("designkit: 提示词缺少编号: %w", dkdomain.ErrNotFound)
	}
	vars, err := encodePromptVariables(prompt.Variables)
	if err != nil {
		return err
	}

	sqlText := `
UPDATE designkit_prompts SET
  category_id = $2,
  title = $3,
  body = $4,
  variables = $5,
  preview_url = $6,
  is_enabled = $7,
  updated_at = NOW()
WHERE uid = $1 AND deleted_at IS NULL`
	args := []any{prompt.UID, prompt.CategoryID, prompt.Title, prompt.Body, vars,
		prompt.PreviewURL, prompt.IsEnabled}
	if prompt.OwnerUserID != nil {
		args = append(args, *prompt.OwnerUserID)
		sqlText += fmt.Sprintf(" AND owner_user_id = $%d", len(args))
	}
	sqlText += " RETURNING " + promptColumns

	updated, err := scanPrompt(r.sql.QueryRowContext(ctx, sqlText, args...))
	if err != nil {
		return translate(err, "提示词")
	}
	*prompt = *updated
	return nil
}

// DeletePrompt 软删一条自己的。
// 软删而不是真删：历史任务里存的是提示词原文快照，但详情页还想显示「来自哪条」。
func (r *designkitRepository) DeletePrompt(ctx context.Context, ownerUserID int64, uid string) error {
	res, err := r.sql.ExecContext(ctx, `
UPDATE designkit_prompts SET deleted_at = NOW(), updated_at = NOW()
WHERE uid = $1 AND owner_user_id = $2 AND deleted_at IS NULL`, uid, ownerUserID)
	if err != nil {
		return err
	}
	affected, err := affectedRows(res)
	if err != nil {
		return err
	}
	if affected == 0 {
		return notFoundErr("提示词")
	}
	return nil
}

// ---- 同步进来的提示词 ----

// upsertSyncedPromptSQL 一条一条写。
//
// 三个细节：
//   - ON CONFLICT 目标带上部分索引的 WHERE 谓词（见 promptSourceRefConflictTarget）；
//   - DO UPDATE 自己再带一个 WHERE：内容没变就不写，用「有没有返回行」区分
//     更新和跳过，1.4 万条里绝大多数是没变的，少写一次就少一次 WAL；
//   - `xmax = 0` 是 PostgreSQL 里区分「这行是刚插的还是被更新的」的标准写法。
//
// **is_enabled 不跟着同步覆盖**：管理员手动下架过的词，下次同步不能自己又打开。
// deleted_at 则会被清掉 —— 上游把删掉的词又加回来了，就当它回来了。
const upsertSyncedPromptSQL = `
INSERT INTO designkit_prompts (
  uid, category_id, title, body, variables, preview_url, source, source_ref, is_enabled
) VALUES ($1, $2, $3, $4, $5, $6, 'youmind', $7, TRUE)
` + promptSourceRefConflictTarget + `
DO UPDATE SET
  category_id = EXCLUDED.category_id,
  title = EXCLUDED.title,
  body = EXCLUDED.body,
  variables = EXCLUDED.variables,
  preview_url = EXCLUDED.preview_url,
  deleted_at = NULL,
  updated_at = NOW()
WHERE designkit_prompts.title IS DISTINCT FROM EXCLUDED.title
   OR designkit_prompts.body IS DISTINCT FROM EXCLUDED.body
   OR designkit_prompts.variables IS DISTINCT FROM EXCLUDED.variables
   OR designkit_prompts.preview_url IS DISTINCT FROM EXCLUDED.preview_url
   OR designkit_prompts.category_id IS DISTINCT FROM EXCLUDED.category_id
   OR designkit_prompts.deleted_at IS NOT NULL
RETURNING (xmax = 0) AS inserted`

func (r *designkitRepository) UpsertSyncedPrompts(ctx context.Context, rows []dkdomain.SyncPromptRow) (*dkdomain.SyncPromptsResult, error) {
	result := &dkdomain.SyncPromptsResult{}
	if len(rows) == 0 {
		return result, nil
	}

	err := r.withTx(ctx, func(tx *sql.Tx) error {
		categories, err := loadCategoryIDsBySlug(ctx, tx, rows)
		if err != nil {
			return err
		}

		for _, row := range rows {
			if strings.TrimSpace(row.SourceRef) == "" {
				// 没有 source_ref 就没有幂等键，下次同步会再插一条重复的。
				// 老系统就是这么把 1.4 万条写重复的，直接跳过。
				result.Skipped++
				continue
			}
			vars, err := encodePromptVariables(row.Variables)
			if err != nil {
				return err
			}
			var categoryID any
			if id, ok := categories[row.CategorySlug]; ok {
				categoryID = id
			}

			var inserted bool
			err = tx.QueryRowContext(ctx, upsertSyncedPromptSQL,
				row.UID, categoryID, row.Title, row.Body, vars,
				nullableStringPtr(row.PreviewURL), row.SourceRef).Scan(&inserted)
			switch {
			case errors.Is(err, sql.ErrNoRows):
				// DO UPDATE 的 WHERE 没命中 = 内容一个字都没变。
				result.Skipped++
			case err != nil:
				return translate(err, "提示词")
			case inserted:
				result.Inserted++
			default:
				result.Updated++
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

// loadCategoryIDsBySlug 一次把用到的分类 slug 全查出来，避免每条提示词查一次库。
// 查不到的 slug 不会自动建分类 —— 建分类走 UpsertCategory，同步器自己负责先建。
func loadCategoryIDsBySlug(ctx context.Context, exec dkExecutor, prompts []dkdomain.SyncPromptRow) (map[string]int64, error) {
	slugSet := make(map[string]struct{}, len(prompts))
	for _, p := range prompts {
		if s := strings.TrimSpace(p.CategorySlug); s != "" {
			slugSet[s] = struct{}{}
		}
	}
	out := make(map[string]int64, len(slugSet))
	if len(slugSet) == 0 {
		return out, nil
	}
	slugs := make([]string, 0, len(slugSet))
	for s := range slugSet {
		slugs = append(slugs, s)
	}

	rows, err := exec.QueryContext(ctx,
		`SELECT id, slug FROM designkit_prompt_categories WHERE slug = ANY($1) ORDER BY id ASC`,
		pq.Array(slugs))
	if err != nil {
		return nil, err
	}
	defer closeRows(rows)

	for rows.Next() {
		var id int64
		var slug string
		if err := rows.Scan(&id, &slug); err != nil {
			return nil, err
		}
		out[slug] = id
	}
	return out, rows.Err()
}
