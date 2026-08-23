/**
 * designkit 前端 → 后端的接口封装。**页面只从这里拿数据，不要自己写请求。**
 *
 * 走上游的 axios 单例 `@/api/client`：它已经带好了 baseURL（默认 `/api/v1`）、
 * 登录凭证请求头、界面语言、登录过期自动续期。不要另起一个 axios 实例。
 *
 * 因此这里的路径只写 `/designkit/...`，实际请求是 `/api/v1/designkit/...`。
 * **写成 `/api/v1/designkit/...` 就会变成 `/api/v1/api/v1/designkit/...`，直接 404。**
 * ERP 用的 `/v1/designkit/*` 跟前端无关，不在这里出现。
 *
 * 三个坑，改这个文件前先读一遍：
 *
 *  1. **上传必须显式写 `Content-Type: multipart/form-data`。**
 *     axios 单例的默认头是 `application/json`，而 axios 1.x 在
 *     「默认头是 json + 数据是 FormData」时会把表单**转成 JSON 再发**，
 *     后端于是收不到文件，报「没有收到图片文件」，而且前端毫无察觉。
 *  2. **图片地址不能直接塞进 `<img src>`。** 后端返回的 `content_url` 要带登录凭证
 *     请求头才放行，而 `<img>` 不会带，结果是碎图标。用 `useAuthedImages()`。
 *  3. **提交批次必须带防重复标识**（`newIdempotencyKey()`），否则运营手滑双击
 *     = 两个批次 = 两份钱。打开确认弹窗时生成一个存住，重试提交要用**同一个**。
 */

import type { AxiosProgressEvent } from 'axios'
import { apiClient } from '@/api/client'
import { i18n } from '@/i18n'
import { DESIGNKIT_API_BASE_PATH } from './paths'
import { toPrice } from './money'
import {
  getPrompt,
  getPromptSyncSettings,
  getPromptSyncStatus,
  listPromptCategories,
  listPrompts,
  startPromptSync,
  suggestPrompt,
  updatePromptSyncSettings,
} from './inspiration'
import {
  deleteChatSession,
  getChatSession,
  listChatSessions,
  sendChatMessage,
} from './chat'
import { checkContent } from './contentcheck'
import { getUpscaleStatus, startUpscale } from './upscale'
import type {
  CreateJobInput,
  DesignkitApiError,
  DesignkitAsset,
  DesignkitHealth,
  DesignkitImage,
  ItemStatus,
  Job,
  JobCreated,
  JobEstimate,
  JobEstimateWire,
  JobItem,
  JobPage,
  JobSpecInput,
  JobStatus,
  RatioList,
  RatioListWire,
  UsageSummary,
} from './types'

/**
 * 所有 designkit 端点的公共前缀（相对 `/api/v1`）。
 *
 * 常量本体在 `./paths`：灵感库那个文件也要用它，而它又被本文件 `export *` 出去，
 * 放在这里会变成循环引用。从这里继续导出，调用方不必知道这件事。
 */
export { DESIGNKIT_API_BASE_PATH } from './paths'

/** 上传单张商品图的大小上限，跟后端 `maxUploadBytes` 一致（20MB）。 */
export const MAX_UPLOAD_BYTES = 20 * 1024 * 1024

/** 允许上传的图片格式，跟后端白名单一致。用于 `<input accept>` 和前端预检。 */
export const ACCEPTED_IMAGE_TYPES = [
  'image/jpeg',
  'image/png',
  'image/webp',
  'image/heic',
  'image/heif',
] as const

/** `<input type="file" accept>` 用的字符串。HEIC 有些浏览器只认扩展名。 */
export const ACCEPT_ATTRIBUTE = '.jpg,.jpeg,.png,.webp,.heic,.heif,image/jpeg,image/png,image/webp,image/heic,image/heif'

/** 上传一张 20MB 的图在内网也可能要十几秒，用比默认 30 秒宽的超时。 */
const UPLOAD_TIMEOUT_MS = 180_000

/** 「生成白底图」抠一张大图要几十秒（首次还可能在下模型），超时放宽到 90 秒。 */
const REMOVE_BACKGROUND_TIMEOUT_MS = 90_000

// ============================================================================
// 健康检查
// ============================================================================

/**
 * 后端连通性。
 *
 * ⚠ 数据库不通时后端**仍然返回 HTTP 200**，必须看返回体里的 `db` / `module_status`，
 * 「请求没报错」不等于「能出图」。
 */
export async function getHealth(): Promise<DesignkitHealth> {
  const { data } = await apiClient.get<DesignkitHealth>(`${DESIGNKIT_API_BASE_PATH}/health`)
  return data
}

// ============================================================================
// 素材（商品图）
// ============================================================================

/** 上传商品图时的可选项。 */
export interface UploadAssetOptions {
  /** 上传进度，0~100 的整数。浏览器拿不到总大小时不会被调用。 */
  onProgress?: (percent: number) => void
  /** 用来取消上传（运营删掉了这一张、或离开了页面）。 */
  signal?: AbortSignal
}

/**
 * 上传一张商品图。**一次一张**，多选由页面自己循环调用（可以并发 2~3 个）。
 *
 * 上传前请先用 `checkUploadFile()` 挡掉太大 / 格式不对的文件，
 * 别让运营等传完 20MB 才被拒绝。
 */
/**
 * 「继续生成」：把一张**已经出好的结果图**变成一张新的商品图。
 *
 * 运营拿它接着出下一轮——换个提示词、换个比例、在上一版基础上再加工。
 *
 * ⚠ **图片字节不经过浏览器**：服务端直接从存储里复制。
 * 前端本来也能做（取图 → 构造 File → 调 uploadAsset），但那是下行 2MB + 上行 2MB，
 * 远程用的时候点一次要等好几秒还吃上行带宽。
 *
 * ⚠ 重复点不会存重复的图：后端按 sha256 去重，同一张图拿到的是同一条素材。
 */
export async function continueFromImage(imageUID: string): Promise<DesignkitAsset> {
  const { data } = await apiClient.post<DesignkitAsset>(
    `${DESIGNKIT_API_BASE_PATH}/assets/from-image/${encodeURIComponent(imageUID)}`,
  )
  return data
}

/**
 * 「生成白底图」：给一张已有的商品图抠掉背景、合成白底，存成一条**新的**商品图。
 *
 * ⚠ 图片字节不经过浏览器：抠图和合白底都在服务端做，返回的只是新素材的信息。
 * ⚠ 重复点不会存重复的图：后端按 sha256 去重，同一张原图拿到同一条素材。
 * ⚠ 这一步**不花钱**（走本地抠图服务，不经过出图网关），但要等几十秒，
 *   调用方要给「抠图中…」的状态并禁掉按钮。
 */
export async function removeBackground(assetUID: string): Promise<DesignkitAsset> {
  const { data } = await apiClient.post<DesignkitAsset>(
    `${DESIGNKIT_API_BASE_PATH}/assets/${encodeURIComponent(assetUID)}/remove-background`,
    undefined,
    { timeout: REMOVE_BACKGROUND_TIMEOUT_MS },
  )
  return data
}

export async function uploadAsset(file: File, options: UploadAssetOptions = {}): Promise<DesignkitAsset> {
  const form = new FormData()
  // 字段名必须是 file，后端写死的（asset_handler.go 的 uploadFormField）。
  form.append('file', file, file.name)

  const { data } = await apiClient.post<DesignkitAsset>(`${DESIGNKIT_API_BASE_PATH}/assets`, form, {
    // 见文件头第 1 条：不写这一行，axios 会把表单转成 JSON，文件就没了。
    headers: { 'Content-Type': 'multipart/form-data' },
    timeout: UPLOAD_TIMEOUT_MS,
    signal: options.signal,
    onUploadProgress: (event: AxiosProgressEvent) => {
      if (!options.onProgress) {
        return
      }
      const ratio =
        typeof event.progress === 'number'
          ? event.progress
          : event.total && event.total > 0
            ? event.loaded / event.total
            : null
      if (ratio === null) {
        return
      }
      options.onProgress(Math.max(0, Math.min(100, Math.round(ratio * 100))))
    },
  })
  return data
}

/** 上传前的本地预检结果。`ok` 为 false 时 `reasonKey` 是要显示的 i18n 键。 */
export interface UploadCheckResult {
  ok: boolean
  /** i18n 键，例如 'designkit.upload.tooLarge'。 */
  reasonKey?: string
}

/**
 * 上传前先在本地挡一道：太大、格式不对的文件不要浪费运营的时间和带宽。
 *
 * 只做「肯定不行」的判断；拿不准一律放行交给后端——后端的判定更全
 * （它还会嗅探文件字节）。
 */
export function checkUploadFile(file: File): UploadCheckResult {
  if (file.size <= 0) {
    return { ok: false, reasonKey: 'designkit.upload.empty' }
  }
  if (file.size > MAX_UPLOAD_BYTES) {
    return { ok: false, reasonKey: 'designkit.upload.tooLarge' }
  }
  const type = file.type.toLowerCase().split(';')[0].trim()
  const name = file.name.toLowerCase()
  const extensionOk = /\.(jpe?g|png|webp|heic|heif)$/.test(name)
  const typeOk = type === '' || (ACCEPTED_IMAGE_TYPES as readonly string[]).includes(type) || type === 'image/jpg' || type === 'image/pjpeg'
  if (!typeOk && !extensionOk) {
    return { ok: false, reasonKey: 'designkit.upload.unsupported' }
  }
  return { ok: true }
}

/** 按编号取一张商品图的信息。 */
export async function getAsset(uid: string): Promise<DesignkitAsset> {
  const { data } = await apiClient.get<DesignkitAsset>(`${DESIGNKIT_API_BASE_PATH}/assets/${encodeURIComponent(uid)}`)
  return data
}

/**
 * 删除一张商品图。
 *
 * 只从商品图列表里消失：历史批次的记录和结果图**不受影响**
 * （批次里存的是提示词快照和结果图，不跟着商品图走），文件本身也不删。
 */
export async function deleteAsset(uid: string): Promise<void> {
  await apiClient.delete(`${DESIGNKIT_API_BASE_PATH}/assets/${encodeURIComponent(uid)}`)
}

// ============================================================================
// 出图比例
// ============================================================================

/**
 * 允许的出图比例，顺序即界面显示顺序。
 *
 * ⚠ `unit_price.confirmed === false` 表示**价格待确认**，界面必须原样显示
 * 「价格待确认」，不许显示任何猜的单价（用 `formatPrice()`）。
 */
export async function listRatios(signal?: AbortSignal): Promise<RatioList> {
  const { data } = await apiClient.get<RatioListWire>(`${DESIGNKIT_API_BASE_PATH}/ratios`, { signal })
  return {
    currency: data.currency,
    ratios: (data.ratios ?? []).map((r) => ({
      ratio: r.ratio,
      target_width: r.target_width,
      target_height: r.target_height,
      pricing_tier: r.pricing_tier,
      price_confirmed: r.price_confirmed,
      unit_price: toPrice(r.unit_price),
      is_default: r.is_default,
    })),
  }
}

// ============================================================================
// 报价
// ============================================================================

/**
 * 算这一批要多少钱。**只算账，不建任何东西、不冻结额度、不花钱。**
 *
 * 每次改动选中的商品图 / 提示词 / 比例都可以重算（记得防抖，别每敲一个字就发一次）。
 */
export async function estimateJob(spec: JobSpecInput, signal?: AbortSignal): Promise<JobEstimate> {
  const { data } = await apiClient.post<JobEstimateWire>(`${DESIGNKIT_API_BASE_PATH}/jobs/estimate`, normalizeSpec(spec), {
    signal,
  })
  return {
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
  }
}

// ============================================================================
// 提交批次
// ============================================================================

/**
 * 生成一个防重复标识。
 *
 * **打开确认弹窗的时候生成一个存住**，用户在弹窗里犹豫、点了又点、
 * 网络超时后重发，都用**同一个**——后端据此认出「这是同一次提交」，
 * 只会建一个批次、只扣一份钱。提交成功后丢掉，下次提交重新生成。
 *
 * 实现说明：群晖内网是 http 不是 https，浏览器在非安全环境里**没有**
 * `crypto.randomUUID()`，所以必须有降级路径，否则工作台在 NAS 上会直接报错。
 */
export function newIdempotencyKey(): string {
  const cryptoObj = typeof globalThis !== 'undefined' ? globalThis.crypto : undefined
  if (cryptoObj && typeof cryptoObj.randomUUID === 'function') {
    return cryptoObj.randomUUID()
  }
  if (cryptoObj && typeof cryptoObj.getRandomValues === 'function') {
    const bytes = new Uint8Array(16)
    cryptoObj.getRandomValues(bytes)
    let hex = ''
    for (let i = 0; i < bytes.length; i += 1) {
      hex += bytes[i].toString(16).padStart(2, '0')
    }
    return `dk-${hex}`
  }
  return `dk-${Date.now().toString(36)}-${Math.random().toString(36).slice(2, 12)}`
}

/**
 * 提交一个批次，开始出图。**这一步开始花钱。**
 *
 * @param input          要出哪些图
 * @param idempotencyKey 防重复标识，必传，见 `newIdempotencyKey()`
 *
 * 返回的是小对象（任务号 + 张数 + 预估花费），**不含每张图的进度**。
 * 拿到 `uid` 之后交给 `useJobPolling()` 轮询进度。
 */
export async function createJob(input: CreateJobInput, idempotencyKey: string): Promise<JobCreated> {
  const body: Record<string, unknown> = normalizeSpec(input)
  if (input.name && input.name.trim() !== '') {
    body.name = input.name.trim()
  }
  if (input.callback_url && input.callback_url.trim() !== '') {
    body.callback_url = input.callback_url.trim()
  }
  const { data } = await apiClient.post<JobCreated>(`${DESIGNKIT_API_BASE_PATH}/jobs`, body, {
    headers: { 'Idempotency-Key': idempotencyKey },
  })
  return data
}

/** 把选择归一化成后端要的请求体：去空白、丢空串、**保持原顺序**（顺序就是出图顺序）。 */
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
// 查询批次
// ============================================================================

/** 批次列表的查询条件。 */
export interface ListJobsQuery {
  /** 只看这些状态；不填 = 全部。 */
  statuses?: JobStatus[]
  /** 每页条数，默认 20，最大 100。 */
  limit?: number
  /** 上一页返回的 `next_cursor`；不填 = 第一页。**不透明字符串，不要解析。** */
  cursor?: string | null
  signal?: AbortSignal
}

/** 批次列表（游标分页：只有「下一页」，没有页码，也没有总数）。 */
export async function listJobs(query: ListJobsQuery = {}): Promise<JobPage> {
  const params: Record<string, string | number> = {}
  if (query.statuses && query.statuses.length > 0) {
    params.status = query.statuses.join(',')
  }
  if (typeof query.limit === 'number' && query.limit > 0) {
    params.limit = query.limit
  }
  if (query.cursor) {
    params.cursor = query.cursor
  }
  const { data } = await apiClient.get<JobPage>(`${DESIGNKIT_API_BASE_PATH}/jobs`, {
    params,
    signal: query.signal,
  })
  return {
    items: data.items ?? [],
    has_more: data.has_more === true,
    next_cursor: data.next_cursor ?? null,
  }
}

/** 一个批次的概况（状态 + 四个计数 + 花费）。轮询进度用它。 */
export async function getJob(uid: string, signal?: AbortSignal): Promise<Job> {
  const { data } = await apiClient.get<Job>(`${DESIGNKIT_API_BASE_PATH}/jobs/${encodeURIComponent(uid)}`, { signal })
  return data
}

/**
 * 删除一批记录（「我的图片」里的一条）。
 *
 * ⚠ 删的是**记录的可见性**：这一批和它的结果图从「我的图片」消失，
 * 已扣的费用不退。没结束的批次后端会拒绝（先「停止排队」等它结束）。
 */
export async function deleteJob(uid: string): Promise<void> {
  await apiClient.delete(`${DESIGNKIT_API_BASE_PATH}/jobs/${encodeURIComponent(uid)}`)
}

/**
 * 一个批次里的每一张图。**返回顺序固定按 seq 升序**（图1×词1、图1×词2、图2×词1…），
 * 直接按数组顺序渲染即可，不要自己排序。
 */
export async function getJobItems(uid: string, signal?: AbortSignal): Promise<JobItem[]> {
  const { data } = await apiClient.get<{ items?: JobItem[] }>(
    `${DESIGNKIT_API_BASE_PATH}/jobs/${encodeURIComponent(uid)}/items`,
    { signal },
  )
  return data.items ?? []
}

/** 按序号取某一张（序号从 1 开始）。 */
export async function getJobItem(uid: string, seq: number, signal?: AbortSignal): Promise<JobItem> {
  const { data } = await apiClient.get<JobItem>(
    `${DESIGNKIT_API_BASE_PATH}/jobs/${encodeURIComponent(uid)}/items/${seq}`,
    { signal },
  )
  return data
}

/** 按编号取一张结果图的信息（不含图片字节）。 */
export async function getImage(uid: string, signal?: AbortSignal): Promise<DesignkitImage> {
  const { data } = await apiClient.get<DesignkitImage>(`${DESIGNKIT_API_BASE_PATH}/images/${encodeURIComponent(uid)}`, {
    signal,
  })
  return data
}

// ============================================================================
// 图片字节
// ============================================================================

/**
 * 把后端给的 `content_url`（形如 `/api/v1/designkit/images/xxx/content`）
 * 转成能直接交给 axios 单例的路径（`/designkit/images/xxx/content`）。
 *
 * 为什么要转：axios 的 baseURL 已经是 `/api/v1`，原样拼会变成
 * `/api/v1/api/v1/...`。而且部署时接口地址可能被配到别的域名上，
 * 只有走 axios 才会跟着走。
 */
export function contentPathOf(contentUrl: string): string {
  const raw = (contentUrl ?? '').trim()
  if (raw === '') {
    return raw
  }
  if (raw.startsWith('/api/v1/')) {
    return raw.slice('/api/v1'.length)
  }
  return raw
}

/**
 * 取一张图的字节。
 *
 * 这些地址要带登录凭证请求头才放行，`<img src>` 不会带，所以只能先取成 blob
 * 再用 `URL.createObjectURL()` 显示。页面一般不直接调这个，用 `useAuthedImages()`。
 */
export async function fetchContentBlob(contentUrl: string, signal?: AbortSignal): Promise<Blob> {
  const { data } = await apiClient.get<Blob>(contentPathOf(contentUrl), {
    responseType: 'blob',
    signal,
  })
  return data
}

/**
 * 下载一张图到本地（点「下载」按钮用）。
 *
 * @param filename 建议的文件名，例如 `主图-第3张.png`
 */
export async function downloadContent(contentUrl: string, filename: string): Promise<void> {
  const blob = await fetchContentBlob(contentUrl)
  const objectUrl = URL.createObjectURL(blob)
  try {
    const anchor = document.createElement('a')
    anchor.href = objectUrl
    anchor.download = filename
    document.body.appendChild(anchor)
    anchor.click()
    document.body.removeChild(anchor)
  } finally {
    // 立刻回收会让某些浏览器来不及开始下载，给一点缓冲。
    window.setTimeout(() => URL.revokeObjectURL(objectUrl), 60_000)
  }
}

// ============================================================================
// 用量汇总
// ============================================================================

/**
 * 工作台角落那三个数：本月出图张数 / 花费 / 余额。
 *
 * ⚠ **后端还没实现这个端点**，现在一定返回 null。
 * 界面拿到 null 时把这一块**整个藏起来**，不要显示 0 ——
 * 「本月花费 $0」和「还不知道花了多少」是两回事，前者会让人误判。
 *
 * 另外：张数和花费是两套口径，会出现「张数 0、花费不为 0」，
 * 这不是 bug（设计定型 5.2）。界面必须把口径说明显示出来
 * （后端的 `note`，没有就用 i18n 的 `designkit.usage.note`）。
 */
export async function getUsageSummary(signal?: AbortSignal): Promise<UsageSummary | null> {
  try {
    const { data } = await apiClient.get<UsageSummary>(`${DESIGNKIT_API_BASE_PATH}/me/usage/summary`, { signal })
    return data
  } catch (error) {
    const status = error && typeof error === 'object' ? (error as { status?: number }).status : undefined
    if (status === 404 || status === 405) {
      // 端点还没上线。这不是故障，静默返回 null，不要弹错误提示。
      return null
    }
    throw error
  }
}

// ============================================================================
// 状态
// ============================================================================

/** 批次的终态。到了这里就不会再自己变了（除非运营点重试）。 */
export const TERMINAL_JOB_STATUSES: readonly JobStatus[] = ['succeeded', 'partially_failed', 'failed', 'cancelled']

/** 单张图的终态。 */
export const TERMINAL_ITEM_STATUSES: readonly ItemStatus[] = ['succeeded', 'failed', 'cancelled']

/** 这个批次跑完了吗（跑完就该停止轮询）。 */
export function isTerminalJobStatus(status: string): boolean {
  return (TERMINAL_JOB_STATUSES as readonly string[]).includes(status)
}

/** 这一张跑完了吗。 */
export function isTerminalItemStatus(status: string): boolean {
  return (TERMINAL_ITEM_STATUSES as readonly string[]).includes(status)
}

/** 批次状态 → 中文。认不出的状态原样显示，不要显示成空白。 */
export function jobStatusLabel(status: string): string {
  const key = `designkit.jobStatus.${status}`
  const text = i18nText(key)
  return text === key ? status : text
}

/** 单张图状态 → 中文。 */
export function itemStatusLabel(status: string): string {
  const key = `designkit.itemStatus.${status}`
  const text = i18nText(key)
  return text === key ? status : text
}

/**
 * 出图比例 → 中文用途说明，例如 `"3:4"` → `详情竖版`。
 *
 * 比例是从数据库配置里来的，以后加比例不用改代码；文案没配的比例
 * 返回空串，界面直接只显示比例本身（`3:4`）即可。
 *
 * （i18n 的键名里不能带冒号，所以 `3:4` 对应的键是 `r3x4`。）
 */
export function ratioLabel(ratio: string): string {
  const key = `designkit.ratio.labels.r${ratio.replace(':', 'x')}`
  const text = i18nText(key)
  return text === key ? '' : text
}

/** 取一条文案；没有这个键时 vue-i18n 会原样返回键名，调用方据此兜底。 */
function i18nText(key: string): string {
  return i18n.global.t(key)
}

// ============================================================================
// 兼容旧代码
// ============================================================================

/**
 * @deprecated 用 `toFriendlyError()`（api/errors.ts）。
 * 这个函数只做形状归一，**不翻译**，直接显示它的 message 会给运营看到英文。
 */
export function toDesignkitApiError(error: unknown): DesignkitApiError {
  if (error && typeof error === 'object') {
    const e = error as Record<string, unknown>
    return {
      status: typeof e.status === 'number' ? e.status : undefined,
      code: typeof e.code === 'string' || typeof e.code === 'number' ? e.code : undefined,
      message: typeof e.message === 'string' ? e.message : undefined,
    }
  }
  return { message: String(error) }
}

// ============================================================================
// 统一出口
// ============================================================================

export * from './types'
export * from './money'
export * from './errors'
export * from './inspiration'
// AI 对话。
export * from './chat'
// 文案检查（违禁词 + 标题字数）。
export * from './contentcheck'
// 高清放大（异步排队 + 轮询）。
export * from './upscale'
// 「商品图设置」（仅管理员）。放在这里一起导出，页面只 import 一处。
export * from './settings'
// 「额度申请」管理端（仅管理员）。
export * from './quotaAdmin'
// 「用户记录」管理端（仅管理员：所有账户的对话与出图记录）。
export * from './adminRecords'

export const designkitAPI = {
  getHealth,
  uploadAsset,
  getAsset,
  // 删除商品图（软删）。历史批次的记录和结果图不受影响。
  deleteAsset,
  // 一键白底图。**会等几十秒**，调用方要给「抠图中…」的状态并禁掉按钮。
  removeBackground,
  listRatios,
  estimateJob,
  createJob,
  listJobs,
  getJob,
  // 删除一批记录（软删）。结果图一起从「我的图片」消失，已扣费用不退。
  deleteJob,
  getJobItems,
  getJobItem,
  getImage,
  fetchContentBlob,
  downloadContent,
  getUsageSummary,
  // 灵感库
  listPromptCategories,
  listPrompts,
  getPrompt,
  // AI 推荐提示词。**这一个会跑十几秒**（后端连问三趟对话模型），
  // 调用方别当成普通查询，要给「正在想」的状态并禁掉按钮。
  suggestPrompt,
  startPromptSync,
  getPromptSyncStatus,
  getPromptSyncSettings,
  updatePromptSyncSettings,
  // AI 对话。**发消息会等几十秒**（后端要问一趟对话模型），
  // 调用方要给「AI 回复中…」的状态并禁掉输入。
  sendChatMessage,
  listChatSessions,
  getChatSession,
  deleteChatSession,
  // 文案检查。毫秒级纯计算，页面可以边输边查。
  checkContent,
  // 高清放大。**异步**：start 立刻返回，之后每 5 秒轮询一次状态。
  startUpscale,
  getUpscaleStatus,
}

export default designkitAPI
