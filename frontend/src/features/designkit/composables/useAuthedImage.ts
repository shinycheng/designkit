/**
 * 在页面上显示需要登录才能看的图片。
 *
 * **为什么不能直接 `<img :src="item.content_url">`：**
 * 后端返回的图片地址（`/api/v1/designkit/...`）要带登录凭证请求头才放行，
 * 而浏览器的 `<img>` 标签不会带任何请求头，结果是 401 + 一个碎图标，
 * 运营会以为「图没出来」，其实图好好地在服务器上。
 *
 * 所以只能先把图片取成一段字节（blob），再用 `URL.createObjectURL()` 变成
 * 一个本地地址给 `<img>` 用。这个 composable 负责取、缓存、限流、以及**回收**
 * ——objectURL 不主动回收就会一直占着内存，几十张几 MB 的图翻两页就上百 MB。
 *
 * 用法：
 *
 * ```ts
 * const images = useAuthedImages()
 * watch(() => items.value, (list) => images.ensure(list.map((it) => it.content_url)), { immediate: true })
 * ```
 * ```html
 * <img v-if="images.urls.value[item.content_url]" :src="images.urls.value[item.content_url]" />
 * <div v-else-if="images.failed.value[item.content_url]">图片加载失败，刷新试试</div>
 * <div v-else>加载中…</div>
 * ```
 *
 * ⚠ 不要在模板里调用会触发加载的函数（那样每次重新渲染都会再发一次请求）。
 * 统一在 `watch` 里调 `ensure()`。
 */

import { getCurrentInstance, onUnmounted, ref } from 'vue'
import { fetchContentBlob } from '../api'
import { isCanceledError, toFriendlyError } from '../api/errors'
import type { FriendlyError } from '../api/errors'

export interface UseAuthedImagesOptions {
  /**
   * 同时最多取几张。默认 4。
   *
   * 别调太大：浏览器对同一个站点本来就只开 6 条连接，排满了会把
   * 「查进度」这类请求挤到后面，进度条看起来就卡住了。
   */
  concurrency?: number
}

export function useAuthedImages(options: UseAuthedImagesOptions = {}) {
  const concurrency = Math.max(1, options.concurrency ?? 4)

  /** 图片地址 → 能给 `<img src>` 用的本地地址。 */
  const urls = ref<Record<string, string>>({})
  /** 图片地址 → 正在取。 */
  const loading = ref<Record<string, boolean>>({})
  /** 图片地址 → 取失败的原因（中文，可以直接显示）。 */
  const failed = ref<Record<string, FriendlyError>>({})

  const queue: string[] = []
  const queued = new Set<string>()
  let running = 0
  let disposed = false
  const controllers = new Map<string, AbortController>()

  function pump(): void {
    while (!disposed && running < concurrency && queue.length > 0) {
      const key = queue.shift()
      if (key === undefined) {
        return
      }
      queued.delete(key)
      if (urls.value[key]) {
        continue
      }
      running += 1
      void load(key).finally(() => {
        running -= 1
        pump()
      })
    }
  }

  async function load(contentUrl: string): Promise<void> {
    if (disposed || urls.value[contentUrl]) {
      return
    }
    loading.value = { ...loading.value, [contentUrl]: true }
    const controller = new AbortController()
    controllers.set(contentUrl, controller)
    try {
      const blob = await fetchContentBlob(contentUrl, controller.signal)
      if (disposed) {
        return
      }
      const objectUrl = URL.createObjectURL(blob)
      urls.value = { ...urls.value, [contentUrl]: objectUrl }
      if (failed.value[contentUrl]) {
        const next = { ...failed.value }
        delete next[contentUrl]
        failed.value = next
      }
    } catch (err) {
      if (disposed || isCanceledError(err)) {
        return
      }
      failed.value = { ...failed.value, [contentUrl]: toFriendlyError(err) }
    } finally {
      controllers.delete(contentUrl)
      if (!disposed) {
        const next = { ...loading.value }
        delete next[contentUrl]
        loading.value = next
      }
    }
  }

  /**
   * 确保这些图都取回来了。已经取过的、正在取的会自动跳过，可以放心重复调用。
   *
   * 传后端给的 `content_url` 原样即可（`/api/v1/designkit/...`），
   * 路径转换在 api 层做掉了。
   */
  function ensure(contentUrls: Array<string | null | undefined>): void {
    if (disposed) {
      return
    }
    for (const raw of contentUrls) {
      const key = (raw ?? '').trim()
      if (key === '' || urls.value[key] || loading.value[key] || queued.has(key)) {
        continue
      }
      queued.add(key)
      queue.push(key)
    }
    pump()
  }

  /** 重新取一张（「加载失败，点这里重试」）。 */
  function retry(contentUrl: string): void {
    release(contentUrl)
    ensure([contentUrl])
  }

  /** 放掉一张图占的内存。翻页、关灯箱时用。 */
  function release(contentUrl: string): void {
    const existing = urls.value[contentUrl]
    if (existing) {
      URL.revokeObjectURL(existing)
      const next = { ...urls.value }
      delete next[contentUrl]
      urls.value = next
    }
    const controller = controllers.get(contentUrl)
    if (controller) {
      controller.abort()
      controllers.delete(contentUrl)
    }
    if (failed.value[contentUrl]) {
      const next = { ...failed.value }
      delete next[contentUrl]
      failed.value = next
    }
  }

  /** 全部放掉。组件卸载时会自动调用，一般不用手动调。 */
  function releaseAll(): void {
    for (const objectUrl of Object.values(urls.value)) {
      URL.revokeObjectURL(objectUrl)
    }
    urls.value = {}
    failed.value = {}
    loading.value = {}
    for (const controller of controllers.values()) {
      controller.abort()
    }
    controllers.clear()
    queue.length = 0
    queued.clear()
  }

  function dispose(): void {
    disposed = true
    releaseAll()
  }

  // 不回收 objectURL 就是内存泄漏：一批 50 张图能占几百 MB，运营的电脑会越用越卡。
  if (getCurrentInstance()) {
    onUnmounted(dispose)
  }

  return {
    urls,
    loading,
    failed,
    ensure,
    retry,
    release,
    releaseAll,
    dispose,
  }
}

export type UseAuthedImagesReturn = ReturnType<typeof useAuthedImages>
