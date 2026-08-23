/**
 * 「用户记录」管理端的接口封装。**只有管理员调得动**（后端挂着管理员判定）。
 *
 * 走上游 axios 单例（见 `./index.ts` 开头那三条坑），所以路径只写
 * `/designkit/admin/records/...`，实际请求是 `/api/v1/designkit/admin/records/...`。
 *
 *   GET /designkit/admin/records/users                        有记录的账户（筛选下拉用）
 *   GET /designkit/admin/records/chat/sessions                会话列表（updated_at 倒序）
 *   GET /designkit/admin/records/chat/sessions/:uid           一个会话 + 全部消息（id 升序）
 *   GET /designkit/admin/records/jobs                         批次列表（created_at 倒序）
 *   GET /designkit/admin/records/jobs/:uid                    一个批次 + 全部单张（seq 升序）
 *   GET /designkit/admin/records/jobs/:uid/items/:seq/content 单张图的字节（image/png）
 *
 * 两件事，改这个文件前先读一遍：
 *
 *  1. **列表顺序由后端定，前端不要再排。** 会话按 updated_at 倒序、批次按
 *     created_at 倒序、消息按 id 升序、单张按 seq 升序，后端都是显式 ORDER BY。
 *
 *  2. **这里只有用户自己没删过的记录。** 管理员视角跟用户视角一致，
 *     用户删掉的会话/批次不在这里出现——列表条数对不上「总出图数」是正常的。
 */

import { apiClient } from '@/api/client'
import { DESIGNKIT_API_BASE_PATH } from './paths'
import type { ItemStatus, JobStatus, Money } from './types'

// ============================================================================
// 常量
// ============================================================================

/** 一页拉多少条。后端 limit 默认 50、封顶 200；界面用「加载更多」翻页。 */
export const ADMIN_RECORDS_PAGE_SIZE = 50

const ADMIN_RECORDS_PATH = `${DESIGNKIT_API_BASE_PATH}/admin/records`

// ============================================================================
// 类型（字段名与后端逐字一致，一律 snake_case）
// ============================================================================

/** 有记录的账户（筛选下拉里的一行）。 */
export interface AdminRecordUser {
  id: number
  email: string
  /** 该账户的对话会话数。 */
  session_count: number
  /** 该账户的出图批次数。 */
  job_count: number
}

/** 对话记录列表里的一行（比用户态的 ChatSession 多了账户信息和消息条数）。 */
export interface AdminChatSessionRow {
  uid: string
  /** 后端起的标题；可能是空串，界面用「新对话」兜底。 */
  title: string
  user_id: number
  user_email: string
  message_count: number
  created_at: string
  updated_at: string
}

/** 会话详情里的一条消息。形状与用户态 ChatMessage 一致。 */
export interface AdminChatMessage {
  /** 会话内自增编号。详情按它升序返回。 */
  id: number
  role: 'user' | 'assistant' | string
  content: string
  /** 这条消息带的图（素材编号）；没有时是**空数组**，这里已兜底。 */
  asset_uids: string[]
  created_at: string
}

/** 出图记录列表里的一行。 */
export interface AdminJobRow {
  uid: string
  /** 批次名称；可能是空串，界面用「未命名批次」兜底。 */
  name: string
  status: JobStatus | string
  user_id: number
  user_email: string
  /** 这一批共几张。 */
  item_count: number
  success_count: number
  fail_count: number
  /** 实际花费（美元，十进制字符串）；还没结算是 null，显示走 formatMoney 兜底。 */
  actual_cost: Money | null
  currency: string
  ratio: string
  created_at: string
}

/** 批次详情里的一张（一个「商品图 × 提示词」）。 */
export interface AdminJobItemRow {
  /** 批次内序号，从 1 开始。详情按它升序返回。 */
  seq: number
  status: ItemStatus | string
  /** 提示词快照（生成当时的原文，灵感库后来改了也不变）。 */
  prompt: string
  /** 这一张实际扣的钱（美元，十进制字符串）；没扣是 null。 */
  billed_cost: Money | null
  /** true = 有图可看，缩略图地址用 adminJobItemContentUrl() 拼。 */
  has_image: boolean
}

/** 列表的翻页参数。userId 不传 = 全部账户。 */
export interface AdminRecordsListParams {
  userId?: number
  limit?: number
  offset?: number
}

// ============================================================================
// 归一
// ============================================================================

function normalizeMessage(raw: Partial<AdminChatMessage> | null | undefined): AdminChatMessage {
  return {
    id: typeof raw?.id === 'number' ? raw.id : 0,
    role: raw?.role ?? 'assistant',
    content: raw?.content ?? '',
    asset_uids: raw?.asset_uids ?? [],
    created_at: raw?.created_at ?? '',
  }
}

function normalizeItem(raw: Partial<AdminJobItemRow> | null | undefined): AdminJobItemRow {
  return {
    seq: typeof raw?.seq === 'number' ? raw.seq : 0,
    status: raw?.status ?? 'pending',
    prompt: raw?.prompt ?? '',
    billed_cost: raw?.billed_cost ?? null,
    has_image: raw?.has_image === true,
  }
}

/** 翻页参数 → 请求 query。三个都显式传，不做「有值才带上」（user_id 除外：省略=全部）。 */
function listQuery(params: AdminRecordsListParams): Record<string, number> {
  const query: Record<string, number> = {
    limit: params.limit ?? ADMIN_RECORDS_PAGE_SIZE,
    offset: params.offset ?? 0,
  }
  if (typeof params.userId === 'number') {
    query.user_id = params.userId
  }
  return query
}

// ============================================================================
// 请求
// ============================================================================

/** 有记录的账户（对话或出图至少一条），给筛选下拉用。 */
export async function listAdminRecordUsers(signal?: AbortSignal): Promise<AdminRecordUser[]> {
  const { data } = await apiClient.get<{ users?: AdminRecordUser[] }>(
    `${ADMIN_RECORDS_PATH}/users`,
    { signal },
  )
  return data.users ?? []
}

/** 会话列表（updated_at 倒序，后端显式排序）。 */
export async function listAdminChatSessions(
  params: AdminRecordsListParams,
  signal?: AbortSignal,
): Promise<AdminChatSessionRow[]> {
  const { data } = await apiClient.get<{ sessions?: AdminChatSessionRow[] }>(
    `${ADMIN_RECORDS_PATH}/chat/sessions`,
    { params: listQuery(params), signal },
  )
  return data.sessions ?? []
}

/** 一个会话的全部消息（id 升序，直接按数组顺序渲染）。 */
export async function getAdminChatSession(
  uid: string,
  signal?: AbortSignal,
): Promise<{ session: AdminChatSessionRow | null; messages: AdminChatMessage[] }> {
  const { data } = await apiClient.get<{
    session?: AdminChatSessionRow
    messages?: AdminChatMessage[]
  }>(`${ADMIN_RECORDS_PATH}/chat/sessions/${encodeURIComponent(uid)}`, { signal })
  return {
    session: data.session ?? null,
    messages: (data.messages ?? []).map(normalizeMessage),
  }
}

/** 批次列表（created_at 倒序，后端显式排序）。 */
export async function listAdminJobs(
  params: AdminRecordsListParams,
  signal?: AbortSignal,
): Promise<AdminJobRow[]> {
  const { data } = await apiClient.get<{ jobs?: AdminJobRow[] }>(`${ADMIN_RECORDS_PATH}/jobs`, {
    params: listQuery(params),
    signal,
  })
  return data.jobs ?? []
}

/** 一个批次的全部单张（seq 升序，直接按数组顺序渲染）。 */
export async function getAdminJob(
  uid: string,
  signal?: AbortSignal,
): Promise<{ job: AdminJobRow | null; items: AdminJobItemRow[] }> {
  const { data } = await apiClient.get<{ job?: AdminJobRow; items?: AdminJobItemRow[] }>(
    `${ADMIN_RECORDS_PATH}/jobs/${encodeURIComponent(uid)}`,
    { signal },
  )
  return {
    job: data.job ?? null,
    items: (data.items ?? []).map(normalizeItem),
  }
}

// ============================================================================
// 缩略图地址
// ============================================================================

/**
 * 由批次编号 + 序号拼出取图地址（管理员专用通道，跟用户态的
 * `/assets/:uid/content` 是两条路）。手法同 `chat.ts` 的 chatAssetContentUrl。
 *
 * ⚠ 拼出来的地址**不能直接塞 `<img src>`**（要带登录凭证，`<img>` 不带，
 * 结果是 401 碎图）——原样交给 `useAuthedImages()`，它取成 blob 再显示。
 */
export function adminJobItemContentUrl(jobUid: string, seq: number): string {
  return `/api/v1${ADMIN_RECORDS_PATH}/jobs/${encodeURIComponent(jobUid)}/items/${seq}/content`
}
