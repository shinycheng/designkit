/**
 * 工作台**临时**用的几个请求。
 *
 * ⚠ 这个文件是权宜之计，不是长期方案。
 *
 * `features/designkit/api/` 是接口层的正式地盘，页面本来应该只从那里拿数据。
 * 但那一层是上一阶段写的，当时后端还没有下面这几个端点，所以没有对应的函数：
 *
 *   - `POST /designkit/jobs/:uid/stop`              停止排队（决策 21）
 *   - `POST /designkit/jobs/:uid/items/:seq/retry`  重试一张（决策 20）
 *   - `POST /designkit/me/quota-requests`           申请额度（决策 19）
 *   - `POST /designkit/jobs/estimate` 的 `price_note` 字段
 *
 * 后端**已经实现**了这四样（`backend/internal/designkit/handler/register_business.go`
 * 与 `dto.go`），而工作台没有它们就不成闭环：余额不足没有出路、失败的图没法重试、
 * 排队中的图停不下来。所以先在工作台自己这一层调，等接口层补齐之后，
 * **把这个文件删掉，改成从 `../../api` 引入同名函数**即可，页面代码不用动。
 *
 * 三条纪律跟接口层一致：
 *  1. 走上游的 axios 单例 `@/api/client`（自带登录凭证、语言、续期），不另起实例；
 *  2. 路径只写 `/designkit/...`——baseURL 已经是 `/api/v1`，写全会变成 `/api/v1/api/v1/...`；
 *  3. 错误一律交给 `toFriendlyError()` 翻译，绝不把 axios 的英文原文显示给运营。
 */

import { apiClient } from '@/api/client'
import { DESIGNKIT_API_BASE_PATH, toPrice } from '../../api'
import type { Job, JobEstimate, JobEstimateWire, JobItem, JobSpecInput } from '../../api'

// ============================================================================
// 报价（带「价格待确认」的中文说明）
// ============================================================================

/** 服务端原样返回的报价 + 那句中文说明。 */
interface EstimateWireWithNote extends JobEstimateWire {
  /** 价格待确认时的中文说明；单价确认后是空串。 */
  price_note?: string
}

/** 报价结果 + 后端给的中文说明。 */
export interface EstimateWithNote {
  estimate: JobEstimate
  /**
   * 价格待确认时后端给的整句中文，**必须原样显示**。
   *
   * 少了这句话，运营在确认弹窗上看到的是一个不带任何解释的空价格，
   * 分不清是「这一批不要钱」还是「系统坏了」。空串表示价格已确认，不用显示。
   */
  priceNote: string
}

/**
 * 算这一批要多少钱。**只算账，不建任何东西、不冻结额度、不花钱。**
 *
 * 跟接口层的 `estimateJob()` 打的是同一个端点，只是多带回了 `price_note`。
 */
export async function estimateJobWithNote(spec: JobSpecInput, signal?: AbortSignal): Promise<EstimateWithNote> {
  const { data } = await apiClient.post<EstimateWireWithNote>(
    `${DESIGNKIT_API_BASE_PATH}/jobs/estimate`,
    normalizeSpec(spec),
    { signal },
  )
  return {
    estimate: {
      item_count: data.item_count,
      asset_count: data.asset_count,
      prompt_count: data.prompt_count,
      max_batch_items: data.max_batch_items,
      pricing_tier: data.pricing_tier,
      price_confirmed: data.price_confirmed,
      unit_price: toPrice(data.unit_price),
      estimated_cost: toPrice(data.estimated_cost),
      currency: data.currency,
      balance: data.balance,
      available: data.available,
      sufficient: data.sufficient,
      shortfall: data.shortfall,
    },
    priceNote: typeof data.price_note === 'string' ? data.price_note : '',
  }
}

/**
 * 把选择归一化成后端要的请求体：去空白、丢空串、**保持原顺序**。
 *
 * 顺序就是出图顺序（商品图外层、提示词内层），**不要排序、不要去重**。
 * 跟接口层 `normalizeSpec()` 保持一致；那个函数没导出，只能重复一份。
 */
function normalizeSpec(spec: JobSpecInput): Record<string, unknown> {
  const clean = (values: string[] | undefined): string[] =>
    (values ?? []).map((v) => v.trim()).filter((v) => v !== '')

  const body: Record<string, unknown> = {
    ratio: spec.ratio,
    asset_uids: clean(spec.asset_uids),
    prompt_uids: clean(spec.prompt_uids),
    prompts: clean(spec.prompts),
    keep_transparency: spec.keep_transparency === true,
  }
  if (spec.model && spec.model.trim() !== '') {
    body.model = spec.model.trim()
  }
  return body
}

// ============================================================================
// 停止排队（决策 21）
// ============================================================================

/** `POST /designkit/jobs/:uid/stop` 的返回。 */
export interface StopJobResponse {
  /** 还没开始就被停掉的张数：**不生成、也不扣费**。 */
  stopped_count: number
  /** 正在生成的张数：**会跑完并照常扣费**，图会存进「我的图片」。 */
  still_running_count: number
  /** 后端拼好的整句中文，直接显示，不要自己再拼一遍。 */
  message: string
  /** 停完之后的批次，四个计数都是最新的。 */
  job: Job
}

/**
 * 停止排队。**已经在生成的那几张停不下来**（上游出图跟浏览器连接是故意脱钩的），
 * 会跑完并正常扣费——这一点必须在点之前就跟运营说清楚（决策 21）。
 *
 * 无请求体。批次已经结束时后端返回 409，翻译过来是一句
 * 「这批任务已经结束了，不用再停。」
 */
export async function stopJob(uid: string): Promise<StopJobResponse> {
  const { data } = await apiClient.post<StopJobResponse>(
    `${DESIGNKIT_API_BASE_PATH}/jobs/${encodeURIComponent(uid)}/stop`,
  )
  return data
}

// ============================================================================
// 重试一张（决策 20）
// ============================================================================

/** `POST /designkit/jobs/:uid/items/:seq/retry` 的返回。 */
export interface RetryItemResponse {
  /** 重试之后这一张的最新状态：已退回排队中，尝试次数已 +1，images 是空数组。 */
  item: JobItem
  /** 后端拼好的整句中文（含「会重新收一次费用」和「还能再试几次」）。 */
  message: string
  /** 还能再重试几次。 */
  remaining_attempts: number
}

/**
 * 重试一张。**重试 = 重新出一张图 = 重新收一次钱**，技术上改不了（决策 20）。
 * 所以调用之前一定要二次确认，并把单价显示出来。
 *
 * @param idempotencyKey 防重复标识。**打开确认弹窗时生成一个存住**，
 *   在弹窗里点几次、超时重发都用同一个，后端据此认出「这是同一次重试」，
 *   只会重出一张、只扣一份钱。用 `newIdempotencyKey()` 生成。
 */
export async function retryJobItem(uid: string, seq: number, idempotencyKey: string): Promise<RetryItemResponse> {
  const { data } = await apiClient.post<RetryItemResponse>(
    `${DESIGNKIT_API_BASE_PATH}/jobs/${encodeURIComponent(uid)}/items/${seq}/retry`,
    undefined,
    { headers: { 'Idempotency-Key': idempotencyKey } },
  )
  return data
}

// ============================================================================
// 申请额度（决策 19）
// ============================================================================

/** `POST /designkit/me/quota-requests` 的返回。**不含内部主键。** */
export interface QuotaRequestResponse {
  /** 刚提交时是 'pending'。 */
  status: string
  /** 运营自己填的说明，没填是 null。 */
  note: string | null
  created_at: string
  /** 管理员联系方式；没配置时是空串，界面就别显示那一行。 */
  admin_contact: string
  /** 后端拼好的整句中文，直接显示。 */
  message: string
}

/** 运营手填说明的字数上限（按字数不是字节，后端一致）。超了后端会拒绝。 */
export const QUOTA_NOTE_MAX_LENGTH = 200

/**
 * 通知管理员「我要加额度」。
 *
 * 为什么必须有这个：充值和兑换码菜单对运营是隐藏的（决策 10），
 * 看到「余额不足」对他来说本来是死路一条，而管理员这边没有任何机制会主动知道
 * 有人卡住了（决策 19）。
 *
 * 已经有一条还没处理的申请时后端返回 409，翻译过来是
 * 「你已经提交过一次申请了，管理员还没处理，不用重复提交。」——这不是故障。
 */
export async function createQuotaRequest(note?: string): Promise<QuotaRequestResponse> {
  const trimmed = (note ?? '').trim()
  const body = trimmed === '' ? {} : { note: trimmed }
  const { data } = await apiClient.post<QuotaRequestResponse>(
    `${DESIGNKIT_API_BASE_PATH}/me/quota-requests`,
    body,
  )
  return data
}
