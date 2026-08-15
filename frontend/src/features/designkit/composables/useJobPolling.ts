/**
 * 盯着一个批次出图，直到跑完。
 *
 * 出一张图大约 20 秒，一批可能几十张，所以运营会盯着这个页面等好几分钟。
 * 这个 composable 负责那段时间里的全部脏活：
 *
 *   - 每 2 秒问一次后端「跑到哪了」；
 *   - **页面被切到后台就停下来**，切回来立刻问一次。
 *     不这么做的话，运营开着标签页去干别的，几十个标签页一起轮询，
 *     后端会被自己人打到限流（每人每分钟 240 次），别的页面跟着一起卡；
 *   - 跑完（成功 / 部分失败 / 失败 / 已停止）就自动停，不再空转；
 *   - 组件卸载、切到别的批次时把定时器和在途请求都清掉，**不留残留**；
 *   - 网络抖一下不当回事（继续重试，只在角落提示），连着失败才认定为出错。
 *
 * 用法：
 *
 * ```ts
 * const polling = useJobPolling()
 * polling.start(job.uid)              // 提交成功拿到任务号之后
 * // 模板里用 polling.job / polling.items / polling.progress / polling.error
 * ```
 *
 * ⚠ 这里只**看**进度，不做任何会花钱的事。重试、停止排队要另外调接口，
 * 而且都要二次确认。
 */

import { computed, getCurrentInstance, onUnmounted, ref, shallowRef } from 'vue'
import { getJob, getJobItems, isTerminalJobStatus } from '../api'
import { isCanceledError, toFriendlyError } from '../api/errors'
import type { FriendlyError } from '../api/errors'
import type { Job, JobItem } from '../api/types'

/** 进度条要显示的几个数。 */
export interface JobProgress {
  /** 这一批一共几张。 */
  total: number
  /** 出好了几张。**判断「有没有图」看这个，不要看批次状态。** */
  success: number
  /** 失败几张。 */
  fail: number
  /** 还没开始就被「停止排队」砍掉几张。 */
  cancelled: number
  /** 正在生成几张。 */
  running: number
  /** 还在排队几张。 */
  pending: number
  /** 已经有结果的张数 = 成功 + 失败 + 已取消。 */
  done: number
  /** 0~100 的整数，直接给进度条。 */
  percent: number
  /** 整批跑完了没有。 */
  finished: boolean
}

export interface UseJobPollingOptions {
  /** 问一次的间隔，默认 2000 毫秒。 */
  intervalMs?: number
  /** 连着失败几次就放弃并报错，默认 5 次（≈10 秒都没连上）。 */
  maxConsecutiveFailures?: number
  /** 要不要顺便把每一张的状态也拉回来，默认 true。列表页可以关掉省流量。 */
  withItems?: boolean
  /** 整批跑完时回调一次（用来弹「出好了」的提示、刷新作品库）。 */
  onFinished?: (job: Job) => void
}

const EMPTY_PROGRESS: JobProgress = Object.freeze({
  total: 0,
  success: 0,
  fail: 0,
  cancelled: 0,
  running: 0,
  pending: 0,
  done: 0,
  percent: 0,
  finished: false,
})

export function useJobPolling(options: UseJobPollingOptions = {}) {
  const intervalMs = options.intervalMs ?? 2000
  const maxFailures = options.maxConsecutiveFailures ?? 5
  const withItems = options.withItems !== false

  /** 当前盯着哪个批次；null = 没在盯。 */
  const jobUid = ref<string | null>(null)
  /** 批次概况。 */
  const job = shallowRef<Job | null>(null)
  /** 每一张的状态，**已按序号升序**，直接渲染即可。 */
  const items = shallowRef<JobItem[]>([])
  /** 第一次加载中（用来显示骨架屏；后续每次轮询不该让页面闪）。 */
  const initialLoading = ref(false)
  /** 还在盯着吗。跑完 / 出错 / 手动停止之后是 false。 */
  const polling = ref(false)
  /** 因为页面被切到后台而暂停了（回到页面会自动继续，不用提示运营）。 */
  const paused = ref(false)
  /** 致命错误：已经不再重试了，页面要显示出来并给一个「重新加载」按钮。 */
  const error = ref<FriendlyError | null>(null)
  /** 暂时性问题：还在自动重试，角落小字提示即可，别弹窗。 */
  const warning = ref<FriendlyError | null>(null)

  let timer: number | null = null
  let controller: AbortController | null = null
  let inflight = false
  let failures = 0
  let disposed = false
  let visibilityBound = false
  /**
   * 「第几轮」。start / stop / dispose 都会 +1。
   *
   * 被丢弃的那一次请求的 catch/finally 是**异步**跑的，很可能在新一轮已经开跑之后
   * 才轮到它。没有这个计数的话，它会把新一轮的 inflight 标记和 AbortController 清掉，
   * 于是同一时刻冒出两条轮询链，运营的浏览器和后端一起翻倍。
   */
  let generation = 0

  const progress = computed<JobProgress>(() => {
    const current = job.value
    if (!current) {
      return EMPTY_PROGRESS
    }
    const total = current.item_count
    const success = current.success_count
    const fail = current.fail_count
    const cancelled = current.cancelled_count
    const done = success + fail + cancelled
    // running / pending 后端没有单独的计数，只能从每一张的状态里数；
    // 没拉 items 的时候退化成「剩下的都算排队中」。
    const running = items.value.filter((it) => it.status === 'running').length
    const pending = Math.max(0, total - done - running)
    return {
      total,
      success,
      fail,
      cancelled,
      running,
      pending,
      done,
      percent: total > 0 ? Math.min(100, Math.round((done / total) * 100)) : 0,
      finished: isTerminalJobStatus(current.status),
    }
  })

  /** 整批跑完了没有。 */
  const finished = computed(() => progress.value.finished)

  function clearTimer(): void {
    if (timer !== null) {
      window.clearTimeout(timer)
      timer = null
    }
  }

  function abortInflight(): void {
    if (controller) {
      controller.abort()
      controller = null
    }
    inflight = false
  }

  function scheduleNext(): void {
    clearTimer()
    if (disposed || !polling.value) {
      return
    }
    // 页面在后台就不排下一次了；切回来时 onVisibilityChange 会立刻补一次。
    if (typeof document !== 'undefined' && document.visibilityState === 'hidden') {
      paused.value = true
      return
    }
    paused.value = false
    timer = window.setTimeout(() => {
      void tick()
    }, intervalMs)
  }

  async function tick(): Promise<void> {
    if (disposed || !polling.value || inflight) {
      return
    }
    const uid = jobUid.value
    if (!uid) {
      return
    }

    const myGeneration = generation
    const stale = (): boolean => disposed || generation !== myGeneration || jobUid.value !== uid

    inflight = true
    controller = new AbortController()
    const signal = controller.signal

    try {
      const nextJob = await getJob(uid, signal)
      // 期间换了批次 / 已经停了，这一份结果作废，不要污染页面。
      if (stale()) {
        return
      }
      job.value = nextJob

      if (withItems) {
        const nextItems = await getJobItems(uid, signal)
        if (stale()) {
          return
        }
        items.value = nextItems
      }

      failures = 0
      warning.value = null
      error.value = null
      initialLoading.value = false

      if (isTerminalJobStatus(nextJob.status)) {
        polling.value = false
        clearTimer()
        options.onFinished?.(nextJob)
      }
    } catch (err) {
      if (isCanceledError(err) || stale()) {
        return
      }
      const friendly = toFriendlyError(err)
      initialLoading.value = false

      if (!friendly.retriable) {
        // 找不到、没权限、登录过期这类，重试一万次也一样。
        error.value = friendly
        warning.value = null
        polling.value = false
        clearTimer()
        return
      }

      failures += 1
      warning.value = friendly
      if (failures >= maxFailures) {
        error.value = friendly
        warning.value = null
        polling.value = false
        clearTimer()
      }
    } finally {
      // 已经是上一轮的残响就什么都别碰，否则会把新一轮的状态清掉。
      if (generation === myGeneration) {
        inflight = false
        controller = null
        // polling 已经为 false 时 scheduleNext 会直接返回，不会多排一次。
        scheduleNext()
      }
    }
  }

  function onVisibilityChange(): void {
    if (disposed || !polling.value) {
      return
    }
    if (document.visibilityState === 'visible') {
      paused.value = false
      clearTimer()
      void tick()
    } else {
      paused.value = true
      clearTimer()
    }
  }

  function bindVisibility(): void {
    if (visibilityBound || typeof document === 'undefined') {
      return
    }
    document.addEventListener('visibilitychange', onVisibilityChange)
    visibilityBound = true
  }

  function unbindVisibility(): void {
    if (!visibilityBound || typeof document === 'undefined') {
      return
    }
    document.removeEventListener('visibilitychange', onVisibilityChange)
    visibilityBound = false
  }

  /**
   * 开始盯一个批次。
   *
   * 重复对同一个批次调用是安全的（会重置状态重新开始）；
   * 换成别的批次时上一批的在途请求会被丢弃。
   */
  function start(uid: string): void {
    if (disposed || !uid) {
      return
    }
    generation += 1
    abortInflight()
    clearTimer()

    if (jobUid.value !== uid) {
      job.value = null
      items.value = []
    }
    jobUid.value = uid
    failures = 0
    error.value = null
    warning.value = null
    initialLoading.value = job.value === null
    polling.value = true
    paused.value = false
    bindVisibility()
    void tick()
  }

  /** 不再盯了（离开页面、运营点了「不看了」）。可以再 `start()` 回来。 */
  function stop(): void {
    generation += 1
    polling.value = false
    paused.value = false
    clearTimer()
    abortInflight()
    unbindVisibility()
  }

  /** 立刻问一次（下拉刷新、点了「重新加载」）。不影响原有节奏。 */
  async function refresh(): Promise<void> {
    if (disposed || !jobUid.value) {
      return
    }
    if (!polling.value) {
      // 已经停了（跑完或出错），补一次最新数据，但不重新开始轮询。
      const uid = jobUid.value
      try {
        error.value = null
        const nextJob = await getJob(uid)
        if (disposed || jobUid.value !== uid) {
          return
        }
        job.value = nextJob
        if (withItems) {
          const nextItems = await getJobItems(uid)
          if (disposed || jobUid.value !== uid) {
            return
          }
          items.value = nextItems
        }
      } catch (err) {
        if (!isCanceledError(err)) {
          error.value = toFriendlyError(err)
        }
      }
      return
    }
    clearTimer()
    await tick()
  }

  function dispose(): void {
    disposed = true
    stop()
  }

  // 组件卸载时一定要清干净：定时器还在跑 = 内存泄漏 + 白白打后端。
  if (getCurrentInstance()) {
    onUnmounted(dispose)
  }

  return {
    jobUid,
    job,
    items,
    progress,
    finished,
    initialLoading,
    polling,
    paused,
    error,
    warning,
    start,
    stop,
    refresh,
    dispose,
  }
}

export type UseJobPollingReturn = ReturnType<typeof useJobPolling>
