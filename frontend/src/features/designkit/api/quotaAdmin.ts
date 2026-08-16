/**
 * 「额度申请」管理端的接口封装。**只有管理员调得动**（后端挂着管理员判定）。
 *
 * 走上游 axios 单例（见 `./index.ts` 开头那三条坑），所以路径只写
 * `/designkit/admin/quota-requests`，实际请求是 `/api/v1/designkit/admin/quota-requests`。
 *
 *   GET  /designkit/admin/quota-requests?status=pending|handled   两个 tab 的列表
 *   POST /designkit/admin/quota-requests/:id/handle               通过 / 驳回
 *
 * 两件事，改这个文件前先读一遍：
 *
 *  1. **金额是字符串，不是数字。** 「通过」会真的给申请人加余额，
 *     金额走 JSON 浮点数会带来精度噪音（后端也只收十进制字符串）。
 *     输入框里的原文 trim 之后原样传，校验交给页面和后端。
 *
 *  2. **pending_count 跟当前 tab 无关。** 看「已处理」时它照样是全部待处理条数，
 *     侧边栏红点（stores/quotaBadge.ts）和「待处理」tab 的角标都用它。
 */

import { apiClient } from '@/api/client'
import { DESIGNKIT_API_BASE_PATH } from './paths'

// ============================================================================
// 类型（字段名与后端 handler/quota_admin_handler.go 逐字一致，一律 snake_case）
// ============================================================================

/** 一条申请的处理状态。pending=待处理；handled=已通过并加额；rejected=已驳回。 */
export type QuotaRequestStatus = 'pending' | 'handled' | 'rejected'

/** 管理端列表里的一行。 */
export interface QuotaRequestAdminItem {
  /** 自增编号，点「通过/驳回」时用。 */
  id: number
  /** 申请人邮箱；账号已删时是空串（界面显示「账号已删除」）。 */
  requester_email: string
  /** 运营自己填的申请说明，没填是 null。 */
  note: string | null
  status: QuotaRequestStatus
  /** 管理员处理时写的备注，没写是 null。 */
  handle_note: string | null
  /** 通过时加的金额（美元，十进制字符串）；驳回或未处理是 null。 */
  approved_amount: string | null
  /** 处理人邮箱，未处理是 null。 */
  handled_by_email: string | null
  /** 处理时间，未处理是 null。 */
  handled_at: string | null
  /** 申请时间。 */
  created_at: string
}

/** GET 列表的响应。 */
export interface QuotaRequestAdminList {
  items: QuotaRequestAdminItem[]
  /** 全部待处理条数（跟当前 tab 无关）。 */
  pending_count: number
}

/** 处理成功的响应。提示完记得重新拉列表（行已经换 tab 了）。 */
export interface HandledQuotaRequest {
  id: number
  status: QuotaRequestStatus
  /** 通过时实际加的金额（美元，十进制字符串）；驳回是 null。 */
  approved_amount: string | null
  handled_at: string | null
}

// ============================================================================
// 请求
// ============================================================================

const QUOTA_ADMIN_PATH = `${DESIGNKIT_API_BASE_PATH}/admin/quota-requests`

/**
 * 拉一个 tab 的列表。
 * status=pending 看待处理（最老的排前面，先来先处理）；
 * status=handled 看处理过的（通过 + 驳回都算，最新处理的排前面）。
 */
export async function listQuotaRequests(
  status: 'pending' | 'handled',
  limit?: number,
  signal?: AbortSignal,
): Promise<QuotaRequestAdminList> {
  const { data } = await apiClient.get<QuotaRequestAdminList>(QUOTA_ADMIN_PATH, {
    params: limit === undefined ? { status } : { status, limit },
    signal,
  })
  return {
    items: data.items ?? [],
    pending_count: data.pending_count ?? 0,
  }
}

/**
 * 通过：真的给申请人加余额。amount 是十进制字符串（例如 '50'），
 * 页面先校验大于 0，后端还会再拦一遍。
 */
export async function approveQuotaRequest(
  id: number,
  amount: string,
  note: string,
): Promise<HandledQuotaRequest> {
  const { data } = await apiClient.post<HandledQuotaRequest>(
    `${QUOTA_ADMIN_PATH}/${id}/handle`,
    { action: 'approve', amount: amount.trim(), note: note.trim() },
  )
  return data
}

/** 驳回：只标记，不动钱。 */
export async function rejectQuotaRequest(id: number, note: string): Promise<HandledQuotaRequest> {
  const { data } = await apiClient.post<HandledQuotaRequest>(
    `${QUOTA_ADMIN_PATH}/${id}/handle`,
    { action: 'reject', note: note.trim() },
  )
  return data
}

/**
 * 后端整组端点没挂时是 404（不是错误信封）。
 * 这时要显示「这个功能还没准备好」，而不是「找不到这条记录」。
 */
export function isQuotaAdminUnavailableStatus(status: number | null): boolean {
  return status === 404 || status === 405
}
