/**
 * 「高清放大」（Real-ESRGAN ×4）的接口封装。
 *
 * 放大是**异步**的：`startUpscale()` 立刻返回 queued，真正的活在后端排队做
 * （一张要一两分钟），前端拿 `getUpscaleStatus()` 每 5 秒问一次。
 * 三件事必须知道：
 *
 *   1. **不花钱**：放大跑在本地图像服务里，不经过出图网关。
 *   2. **重复点安全**：同一张图在排队/在放/放完时再点，拿到的是同一个任务；
 *      放大结果按 sha256 去重入库，磁盘上永远只有一份。
 *   3. **后端重启任务会丢**（内存队列，接受过的代价）：轮询会拿到 404
 *      `DK_UPSCALE_NOT_FOUND`，这时按「失败」处理，让运营重新点一次。
 */

import { apiClient } from '@/api/client'
import { DESIGNKIT_API_BASE_PATH } from './paths'
import { toFriendlyError } from './errors'
import type { DesignkitAsset } from './types'

/** 任务的四个状态，跟后端字面量一致。 */
export type UpscaleTaskStatus = 'queued' | 'running' | 'done' | 'failed'

/** 一次放大任务的状态快照。 */
export interface UpscaleTask {
  /** 被放大的那张商品图。 */
  asset_uid: string
  status: UpscaleTaskStatus
  /** failed 时的中文原因，可直接显示。 */
  error_message?: string
  /** failed 时的错误码（DK_ 前缀），小字显示方便截图报障。 */
  error_code?: string
  /** done 时的产物：一条新的商品图，可直接塞进下一批的 asset_uids。 */
  result?: DesignkitAsset
  created_at: string
  updated_at: string
}

/** 后端统一包了一层 {task:{...}}。 */
interface UpscaleTaskEnvelope {
  task: UpscaleTask
}

/** 前端轮询间隔：5 秒一次（设计冻结值；240 次/分的面板限流下绰绰有余）。 */
export const UPSCALE_POLL_INTERVAL_MS = 5_000

/**
 * 把一张商品图排进放大队列。返回当前任务状态（多半是 queued；
 * 这张之前放过的话直接就是 done，result 里带着结果）。
 *
 * 可能的拒绝：
 *   - 429 `DK_UPSCALE_QUEUE_FULL`：排队满了（中文原样显示）；
 *   - 404 `DK_ASSET_NOT_FOUND`：图没了；
 *   - 裸 404（无 DK_ 码）：功能没上线，用 `isUpscaleUnavailableError()` 认。
 */
export async function startUpscale(assetUID: string): Promise<UpscaleTask> {
  const { data } = await apiClient.post<UpscaleTaskEnvelope>(
    `${DESIGNKIT_API_BASE_PATH}/assets/${encodeURIComponent(assetUID)}/upscale`,
    // 后端不读请求体；空对象最稳（有些反代拦「没有 body 的 POST」）。
    {},
  )
  return data.task
}

/** 查一张图的放大状态。轮询用它，直到 status 变成 done / failed。 */
export async function getUpscaleStatus(assetUID: string, signal?: AbortSignal): Promise<UpscaleTask> {
  const { data } = await apiClient.get<UpscaleTaskEnvelope>(
    `${DESIGNKIT_API_BASE_PATH}/assets/${encodeURIComponent(assetUID)}/upscale`,
    { signal },
  )
  return data.task
}

/** 任务到头了吗（到头就停止轮询）。 */
export function isTerminalUpscaleStatus(status: string): boolean {
  return status === 'done' || status === 'failed'
}

/** `waitForUpscale` 的可选项。 */
export interface WaitForUpscaleOptions {
  /** 组件卸载时取消轮询（不取消的话页面关了还在打后端）。 */
  signal?: AbortSignal
  /** 轮询间隔，默认 5 秒。测试才需要改。 */
  intervalMs?: number
  /** 每次拿到新状态都回调一次，界面据此切「排队中… / 放大中…」。 */
  onStatus?: (task: UpscaleTask) => void
}

/**
 * 一直轮询到任务结束（done / failed），返回最终状态。
 *
 * 不设总时长上限：后端单张有 5 分钟超时兜底，任务**一定**会走到终态；
 * 排在后面的任务等得再久也只是 queued，砍掉轮询只会让运营以为丢了。
 * 取消一律走 `signal`（抛 AbortError，`isCanceledError()` 认得）。
 */
export async function waitForUpscale(assetUID: string, options: WaitForUpscaleOptions = {}): Promise<UpscaleTask> {
  const interval = options.intervalMs ?? UPSCALE_POLL_INTERVAL_MS
  for (;;) {
    const task = await getUpscaleStatus(assetUID, options.signal)
    options.onStatus?.(task)
    if (isTerminalUpscaleStatus(task.status)) {
      return task
    }
    await upscaleSleep(interval, options.signal)
  }
}

/** 能被 AbortSignal 打断的 sleep。打断时抛 AbortError（isCanceledError 认得）。 */
function upscaleSleep(ms: number, signal?: AbortSignal): Promise<void> {
  return new Promise((resolve, reject) => {
    if (signal?.aborted) {
      reject(new DOMException('Aborted', 'AbortError'))
      return
    }
    const onAbort = () => {
      window.clearTimeout(timer)
      reject(new DOMException('Aborted', 'AbortError'))
    }
    const timer = window.setTimeout(() => {
      signal?.removeEventListener('abort', onAbort)
      resolve()
    }, ms)
    signal?.addEventListener('abort', onAbort, { once: true })
  })
}

/**
 * 这个错误是不是「后端还没上线放大功能」。
 *
 * 跟「AI 推荐」同一套约定：路由没挂时请求打过去是**裸 404**（没有错误信封）。
 * 带 `DK_` 错误码的一律不算「没上线」——`DK_ASSET_NOT_FOUND`（图没了）和
 * `DK_UPSCALE_NOT_FOUND`（任务丢了，多半是服务重启）都是正经业务错误，
 * 误判成「没上线」运营就永远看不到该看的那句话了。
 */
export function isUpscaleUnavailableError(error: unknown): boolean {
  const friendly = toFriendlyError(error)
  if (friendly.code !== null && friendly.code.startsWith('DK_')) {
    return false
  }
  return friendly.status === 404 || friendly.status === 405
}

/**
 * 这个错误是不是「任务丢了」（后端重启把内存队列清空了）。
 * 认出来就按失败处理、让运营重新点一次——这是内存队列约定好的代价。
 */
export function isUpscaleTaskLostError(error: unknown): boolean {
  return toFriendlyError(error).code === 'DK_UPSCALE_NOT_FOUND'
}

/**
 * 这个错误是不是「排队满了」。显示 `designkit.upscale.queueFull`。
 */
export function isUpscaleQueueFullError(error: unknown): boolean {
  return toFriendlyError(error).code === 'DK_UPSCALE_QUEUE_FULL'
}
