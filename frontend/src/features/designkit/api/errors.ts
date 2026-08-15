/**
 * 把「任何一种失败」翻译成一句运营看得懂的中文。
 *
 * 为什么必须有这一层：同一个页面上会冒出至少四种形状的错误，
 * 其中三种的文字是**英文**，直接显示给电商运营等于没显示。
 *
 *  1. designkit 后端的错误信封（中文，可以直接用）：
 *     `{"error":{"type":"...","error_code":"DK_XXX","message":"中文","request_id":"..."}}`
 *     经过 `@/api/client` 的拦截器之后变成 `{ status, error: {...}, message: <axios 英文> }`。
 *  2. 上游中间件的错误（英文）：`{"code":"TOKEN_EXPIRED","message":"Token has expired"}`
 *     —— designkit 的错误改写中间件挂在业务子组上，比它晚执行，改不到它。
 *  3. 网络不通 / 超时：拦截器统一给 `{ status: 0, message: "Network error. Please check your connection." }`
 *     —— 英文，而且分不出是断网还是超时。
 *  4. 请求被主动取消（切页面、换查询条件）：axios 原样抛出，**不该弹任何错误提示**。
 *
 * 规矩：
 *   - 只有**确认是中文**的后端文案才原样显示；英文一律换成我们自己的中文；
 *   - 每句话都要说「怎么办」，不是「哪里错了」；
 *   - 错误码用小字附在后面（`errorText()` 会拼），方便运营截图给管理员；
 *   - 不许出现「API」「token」「幂等」「租约」这类词，金额一律 `$`。
 */

import axios from 'axios'
import { i18n } from '@/i18n'

/** 错误的大类，页面可以据此决定「要不要给重试按钮」「要不要跳登录」。 */
export type FriendlyErrorKind =
  | 'canceled'
  | 'network'
  | 'timeout'
  | 'auth'
  | 'permission'
  | 'balance'
  | 'rate_limit'
  | 'not_found'
  | 'conflict'
  | 'too_large'
  | 'unsupported'
  | 'upstream'
  | 'server'
  | 'client'
  | 'unknown'

/** 翻译之后的错误。页面只该显示 message，其余字段用来做分支和排障。 */
export interface FriendlyError {
  /** 给运营看的中文，已经包含「该怎么办」。**不含错误码**，要带码用 `errorText()`。 */
  message: string
  /** 错误码，例如 "DK_INSUFFICIENT_BALANCE" 或 "HTTP_502"；没有就是 null。 */
  code: string | null
  /** HTTP 状态码；网络不通时是 0；拿不到时是 null。 */
  status: number | null
  /** 后端的排障编号，运营截图时带上它管理员能直接查日志。 */
  requestId: string | null
  kind: FriendlyErrorKind
  /**
   * 这个错误「再试一次有可能好」吗。
   *
   * ⚠ 只表示**这次请求**能不能重发（查询、翻页、上传），
   * **不表示「出图可以重试」**——出图重试要重新花钱，那是另一码事，
   * 必须走带二次确认和单价的重试按钮。
   */
  retriable: boolean
  /** 原始异常，只写 console，不要显示。 */
  raw: unknown
}

/** 中日韩文字，用来判断后端给的是不是中文（用码点写，避免文件编码问题）。 */
const CJK = /[\u4e00-\u9fff]/

function t(key: string, params?: Record<string, unknown>): string {
  return params ? i18n.global.t(key, params) : i18n.global.t(key)
}

/** 请求是不是被主动取消的（切页面、换查询条件）。取消**不该弹错误提示**。 */
export function isCanceledError(error: unknown): boolean {
  if (axios.isCancel(error)) {
    return true
  }
  if (error && typeof error === 'object') {
    const e = error as Record<string, unknown>
    if (e.code === 'ERR_CANCELED' || e.name === 'CanceledError' || e.name === 'AbortError') {
      return true
    }
  }
  return false
}

/**
 * 上游中间件的字符串错误码 → 我们的中文。
 *
 * 这些错误是在 designkit 的错误改写中间件**之前**产生的（登录态校验、限流），
 * 改写不到，所以只能在前端认。上游改了字面量的话，这里会静默退化成按状态码兜底
 * ——不会报错，只会变笼统。跟版检查清单里有这一条。
 */
const UPSTREAM_CODE_KEYS: Record<string, { key: string; kind: FriendlyErrorKind }> = {
  UNAUTHORIZED: { key: 'designkit.errors.auth', kind: 'auth' },
  INVALID_AUTH_HEADER: { key: 'designkit.errors.auth', kind: 'auth' },
  EMPTY_TOKEN: { key: 'designkit.errors.auth', kind: 'auth' },
  INVALID_TOKEN: { key: 'designkit.errors.auth', kind: 'auth' },
  TOKEN_EXPIRED: { key: 'designkit.errors.auth', kind: 'auth' },
  TOKEN_REVOKED: { key: 'designkit.errors.authRevoked', kind: 'auth' },
  TOKEN_REFRESH_FAILED: { key: 'designkit.errors.auth', kind: 'auth' },
  AUTH_SESSION_CHANGED: { key: 'designkit.errors.authSessionChanged', kind: 'auth' },
  USER_NOT_FOUND: { key: 'designkit.errors.auth', kind: 'auth' },
  USER_INACTIVE: { key: 'designkit.errors.accountDisabled', kind: 'permission' },
  ACCESS_DENIED: { key: 'designkit.errors.permission', kind: 'permission' },
  RATE_LIMITED: { key: 'designkit.errors.rateLimit', kind: 'rate_limit' },
  INVALID_AUTH_RATE_LIMITED: { key: 'designkit.errors.rateLimit', kind: 'rate_limit' },
  INSUFFICIENT_BALANCE: { key: 'designkit.errors.balance', kind: 'balance' },
  API_KEY_QUOTA_EXHAUSTED: { key: 'designkit.errors.quota', kind: 'permission' },
  USAGE_LIMIT_EXCEEDED: { key: 'designkit.errors.quota', kind: 'permission' },
}

/** HTTP 状态码 → 中文 + 大类。认不出具体原因时的兜底。 */
function byStatus(status: number): { key: string; kind: FriendlyErrorKind } {
  switch (status) {
    case 0:
      return { key: 'designkit.errors.network', kind: 'network' }
    case 400:
      return { key: 'designkit.errors.badRequest', kind: 'client' }
    case 401:
      return { key: 'designkit.errors.auth', kind: 'auth' }
    case 402:
      return { key: 'designkit.errors.balance', kind: 'balance' }
    case 403:
      return { key: 'designkit.errors.permission', kind: 'permission' }
    case 404:
      return { key: 'designkit.errors.notFound', kind: 'not_found' }
    case 408:
      return { key: 'designkit.errors.timeout', kind: 'timeout' }
    case 409:
      return { key: 'designkit.errors.conflict', kind: 'conflict' }
    case 413:
      return { key: 'designkit.errors.tooLarge', kind: 'too_large' }
    case 415:
      return { key: 'designkit.errors.unsupported', kind: 'unsupported' }
    case 422:
      return { key: 'designkit.errors.badRequest', kind: 'client' }
    case 429:
      return { key: 'designkit.errors.rateLimit', kind: 'rate_limit' }
    case 502:
    case 503:
      return { key: 'designkit.errors.upstream', kind: 'upstream' }
    case 504:
      return { key: 'designkit.errors.timeout', kind: 'timeout' }
    default:
      break
  }
  if (status >= 500) {
    return { key: 'designkit.errors.server', kind: 'server' }
  }
  if (status >= 400) {
    return { key: 'designkit.errors.badRequest', kind: 'client' }
  }
  return { key: 'designkit.errors.unknown', kind: 'unknown' }
}

/** 哪些大类「再点一次有可能好」。 */
const RETRIABLE_KINDS: ReadonlySet<FriendlyErrorKind> = new Set<FriendlyErrorKind>([
  'network',
  'timeout',
  'rate_limit',
  'upstream',
  'server',
])

/** 后端错误信封的内层。 */
interface ErrorEnvelopeBody {
  type?: string
  error_code?: string
  message?: string
  request_id?: string
}

/** 从各种形状里把 designkit 的错误信封挖出来；不是我们的格式就返回 null。 */
function extractEnvelope(error: unknown): ErrorEnvelopeBody | null {
  if (!error || typeof error !== 'object') {
    return null
  }
  const e = error as Record<string, unknown>

  // 1) 经过 @/api/client 拦截器：{ status, error: {...} }
  // 2) 原始响应体：{ error: {...} }
  const nested = e.error
  if (nested && typeof nested === 'object' && !Array.isArray(nested)) {
    const body = nested as Record<string, unknown>
    if (typeof body.error_code === 'string' || typeof body.message === 'string') {
      return {
        type: typeof body.type === 'string' ? body.type : undefined,
        error_code: typeof body.error_code === 'string' ? body.error_code : undefined,
        message: typeof body.message === 'string' ? body.message : undefined,
        request_id: typeof body.request_id === 'string' ? body.request_id : undefined,
      }
    }
  }

  // 3) 有人直接把信封内层传进来了。
  if (typeof e.error_code === 'string') {
    return {
      error_code: e.error_code,
      message: typeof e.message === 'string' ? e.message : undefined,
      request_id: typeof e.request_id === 'string' ? e.request_id : undefined,
    }
  }
  return null
}

/**
 * 把任意异常翻译成一句给运营看的中文。
 *
 * **永远不会抛异常，永远返回一个能直接显示的 message。**
 */
export function toFriendlyError(error: unknown): FriendlyError {
  if (isCanceledError(error)) {
    return {
      message: t('designkit.errors.canceled'),
      code: null,
      status: null,
      requestId: null,
      kind: 'canceled',
      retriable: false,
      raw: error,
    }
  }

  const e = (error && typeof error === 'object' ? error : {}) as Record<string, unknown>
  const status = typeof e.status === 'number' ? e.status : null
  const envelope = extractEnvelope(error)
  const requestId = envelope?.request_id ?? null

  // ---- ① 我们后端的错误信封：message 保证是中文，原样用 ----
  if (envelope && typeof envelope.error_code === 'string' && envelope.error_code.startsWith('DK_')) {
    const message =
      typeof envelope.message === 'string' && envelope.message.trim() !== ''
        ? envelope.message.trim()
        : t(byStatus(status ?? 500).key)
    const kind = kindOfDesignkitCode(envelope.error_code, status)
    return {
      message,
      code: envelope.error_code,
      status,
      requestId,
      kind,
      retriable: RETRIABLE_KINDS.has(kind),
      raw: error,
    }
  }

  // ---- ② 上游中间件的字符串错误码 ----
  const rawCode = typeof e.code === 'string' ? e.code : null
  if (rawCode && UPSTREAM_CODE_KEYS[rawCode]) {
    const hit = UPSTREAM_CODE_KEYS[rawCode]
    return {
      message: t(hit.key),
      code: rawCode,
      status,
      requestId,
      kind: hit.kind,
      retriable: RETRIABLE_KINDS.has(hit.kind),
      raw: error,
    }
  }

  // ---- ③ 后端给了中文（上游少数中文文案、或以后新增的端点）----
  const rawMessage = typeof e.message === 'string' ? e.message.trim() : ''
  if (rawMessage !== '' && CJK.test(rawMessage)) {
    const fallbackKind = byStatus(status ?? 500).kind
    return {
      message: rawMessage,
      code: rawCode ?? (status !== null && status > 0 ? `HTTP_${status}` : null),
      status,
      requestId,
      kind: fallbackKind,
      retriable: RETRIABLE_KINDS.has(fallbackKind),
      raw: error,
    }
  }

  // ---- ④ 兜底：按状态码给我们自己的中文。英文原文一个字都不往外放 ----
  const isTimeout = e.code === 'ECONNABORTED' || e.code === 'ETIMEDOUT'
  const hit = isTimeout ? { key: 'designkit.errors.timeout', kind: 'timeout' as FriendlyErrorKind } : byStatus(status ?? -1)
  return {
    message: t(hit.key),
    code: status !== null && status > 0 ? `HTTP_${status}` : null,
    status,
    requestId,
    kind: hit.kind,
    retriable: RETRIABLE_KINDS.has(hit.kind),
    raw: error,
  }
}

/** designkit 错误码 → 大类。用来决定要不要给「再试一次」按钮。 */
function kindOfDesignkitCode(code: string, status: number | null): FriendlyErrorKind {
  switch (code) {
    case 'DK_INSUFFICIENT_BALANCE':
      return 'balance'
    case 'DK_QUOTA_EXHAUSTED':
      return 'permission'
    case 'DK_UPSTREAM_BUSY':
    case 'DK_RATE_LIMITED':
      return 'rate_limit'
    case 'DK_TIMEOUT':
      return 'timeout'
    case 'DK_CANCELED':
      return 'canceled'
    case 'DK_NO_AVAILABLE_ACCOUNT':
    case 'DK_UPSTREAM_ERROR':
    case 'DK_UPSTREAM_BASE_URL_INVALID':
    case 'DK_UPSTREAM_UNAUTHORIZED':
    case 'DK_PREPROCESS_FAILED':
      return 'upstream'
    case 'DK_UNAUTHORIZED':
      return 'auth'
    case 'DK_FORBIDDEN':
    case 'DK_API_KEY_MISSING':
    case 'DK_IMAGE_NOT_ENABLED':
    case 'DK_MODEL_NOT_ALLOWED':
      return 'permission'
    case 'DK_JOB_NOT_FOUND':
    case 'DK_ITEM_NOT_FOUND':
    case 'DK_ASSET_NOT_FOUND':
    case 'DK_IMAGE_NOT_FOUND':
    case 'DK_PROMPT_NOT_FOUND':
      return 'not_found'
    case 'DK_ILLEGAL_STATE_TRANSITION':
    case 'DK_JOB_ALREADY_SETTLED':
    case 'DK_MAX_ATTEMPTS_EXCEEDED':
    case 'DK_IDEMPOTENCY_KEY_CONFLICT':
    case 'DK_SYNC_IN_PROGRESS':
      return 'conflict'
    case 'DK_IMAGE_TOO_LARGE':
      return 'too_large'
    case 'DK_UNSUPPORTED_IMAGE_FORMAT':
      return 'unsupported'
    case 'DK_STORAGE_ERROR':
    case 'DK_INTERNAL':
    case 'DK_STALE':
      return 'server'
    default:
      return byStatus(status ?? 500).kind
  }
}

/**
 * 可以直接扔进提示框的一整句话：中文 +（小字）错误码。
 *
 * 例：`余额不足，这一批还差 $3.20…（错误码 DK_INSUFFICIENT_BALANCE）`
 *
 * 错误码是给运营截图用的，不是给他看懂的。没有错误码时就只有中文。
 */
export function errorText(error: unknown): string {
  const friendly = error && typeof error === 'object' && 'kind' in error && 'message' in error
    ? (error as FriendlyError)
    : toFriendlyError(error)
  if (!friendly.code) {
    return friendly.message
  }
  return t('designkit.errors.withCode', { message: friendly.message, code: friendly.code })
}
