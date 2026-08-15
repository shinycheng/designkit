<!--
  出图进度 + 结果图。

  设计要点（都是「不这么做就会被投诉」的那种）：

  1. **出一张显示一张**，不等整批跑完。一批几十张要好几分钟，
     盯着空白页等最容易让人以为「卡死了」。
  2. **关掉网页不影响出图**。上游出图跟浏览器连接是故意脱钩的，
     所以要明说「可以先去忙别的」，不然运营会一直不敢关页面。
  3. **停止排队 ≠ 取消**（决策 21）。已经在跑的停不住、照扣费，
     这句话必须在点之前说清楚。
  4. **重试要重新收钱**（决策 20）。按钮给，但一定过二次确认并显示单价。
  5. 图片地址要带登录凭证，不能塞 `<img src>`——统一走 useAuthedImages()。
-->
<template>
  <section class="dk-panel">
    <header class="dk-panel-head">
      <div>
        <h2 class="dk-panel-title">{{ t('designkit.job.title') }}</h2>
        <p class="dk-panel-hint dk-truncate">{{ jobLabel }}</p>
        <p v-if="job" class="dk-panel-hint">
          {{ t('designkit.job.ratio') }} {{ job.ratio }} ·
          {{ t('designkit.job.total', { count: job.item_count }) }} ·
          {{ jobStatusLabel(job.status) }}
        </p>
      </div>

      <div class="dk-panel-actions">
        <button type="button" class="dk-button dk-button--quiet dk-button--sm" @click="reload">
          {{ t('designkit.common.refresh') }}
        </button>
        <button
          v-if="canStop"
          type="button"
          class="dk-button dk-button--secondary dk-button--sm"
          @click="openStopDialog"
        >
          {{ t('designkit.stop.button') }}
        </button>
      </div>
    </header>

    <!-- 进度条 -->
    <div v-if="job">
      <div class="dk-progress-track">
        <div class="dk-progress-bar" :style="{ width: `${progress.percent}%` }"></div>
      </div>
      <div class="dk-meta-row dk-mt-2">
        <span>{{ t('designkit.job.progress', { done: progress.done, total: progress.total }) }}</span>
        <span>
          {{
            t('designkit.job.counts', {
              success: progress.success,
              fail: progress.fail,
              cancelled: progress.cancelled,
            })
          }}
        </span>
        <span v-if="progress.running > 0">{{ t('designkit.job.running', { count: progress.running }) }}</span>
        <span v-if="progress.pending > 0">{{ t('designkit.job.pending', { count: progress.pending }) }}</span>
      </div>
    </div>

    <!-- 还在跑：告诉运营可以走开 -->
    <p v-if="!finished" class="dk-note dk-mt-3">{{ t('designkit.job.waitHint') }}</p>

    <!-- 跑完了：一句话说清结果 -->
    <div v-else-if="job" class="dk-stack dk-stack--sm dk-mt-3">
      <p class="dk-text-strong">{{ finishedText }}</p>
      <!--
        ⚠「已停止」不等于没花钱：已经开始生成的那几张会跑完并照常扣费（决策 21）。
        这一句不能删，删了就会被投诉「我明明停了还扣我钱」。
      -->
      <p v-if="job.status === 'cancelled'" class="dk-note">
        {{ t('designkit.job.cancelledCostNote') }}
      </p>
      <p v-if="job.actual_cost" class="dk-note">
        {{ t('designkit.money.actual') }} {{ formatMoney(job.actual_cost) }}
      </p>
      <p v-if="job.last_error_message" class="dk-note dk-note--danger">
        {{ job.last_error_message }}
      </p>
    </div>

    <!-- 页面切走 / 网络抖动：小字提示，别弹窗 -->
    <p v-if="paused" class="dk-note dk-note--quiet dk-mt-2">{{ t('designkit.job.paused') }}</p>
    <p v-else-if="warning" class="dk-note dk-note--warning dk-mt-2">
      {{ t('designkit.job.reconnecting') }}
    </p>

    <!-- 真出错了：说清楚 + 给一个重新加载 -->
    <div v-if="pollingError" class="dk-alert dk-alert--danger dk-mt-3">
      <p>{{ pollingError.message }}</p>
      <p v-if="pollingError.requestId" class="dk-alert__sub">
        {{ t('designkit.errors.requestId', { id: pollingError.requestId }) }}
      </p>
      <button
        type="button"
        class="dk-button dk-button--secondary dk-button--sm dk-alert__action"
        @click="reload"
      >
        {{ t('designkit.common.reload') }}
      </button>
    </div>

    <!-- 每一张 -->
    <p v-if="initialLoading" class="dk-muted dk-mt-4">{{ t('designkit.common.loading') }}</p>
    <ul v-else-if="items.length > 0" class="dk-result-grid dk-mt-4">
      <JobItemCard
        v-for="item in items"
        :key="item.seq"
        :item="item"
        :thumbnail-url="thumbUrlOf(item)"
        :image-failed-text="thumbFailedTextOf(item)"
        :continuing="continuingSeq === item.seq"
        @open="openLightbox(item)"
        @download="download(item)"
        @retry="openRetryDialog(item)"
        @continue="continueFrom(item)"
      />
    </ul>

    <!-- 停止排队 -->
    <StopJobDialog
      :show="stopDialogOpen"
      :pending-count="progress.pending"
      :running-count="progress.running"
      :working="stopping"
      @confirm="confirmStop"
      @cancel="stopDialogOpen = false"
    />

    <!-- 重试一张 -->
    <RetryItemDialog
      :show="retryTarget !== null"
      :seq="retryTarget?.seq ?? 0"
      :unit-price="unitPrice"
      :attempt-count="retryTarget?.attempt_count ?? 0"
      :max-attempts="retryTarget?.max_attempts ?? 0"
      :was-timeout="retryTarget?.error_code === 'DK_TIMEOUT'"
      :working="retrying"
      @confirm="confirmRetry"
      @cancel="closeRetryDialog"
    />

    <!-- 看大图 -->
    <ImageLightbox
      :show="lightboxItem !== null"
      :src="lightboxItem ? thumbUrlOf(lightboxItem) : ''"
      :title="lightboxItem ? t('designkit.job.seq', { seq: lightboxItem.seq }) : ''"
      :caption="lightboxItem?.prompt_text ?? ''"
      :failed-text="lightboxItem ? thumbFailedTextOf(lightboxItem) : ''"
      :downloading="downloadingSeq !== null"
      @close="lightboxItem = null"
      @download="downloadFromLightbox"
    />
  </section>
</template>

<script setup lang="ts">
import { computed, onBeforeUnmount, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import { useAppStore } from '@/stores/app'
import {
  downloadContent,
  errorText,
  formatMoney,
  isCanceledError,
  jobStatusLabel,
  newIdempotencyKey,
} from '../../api'
import type { DesignkitAsset, JobItem, Price } from '../../api'
import { useAuthedImages, useJobPolling } from '../../composables'
import { retryJobItem, stopJob } from './jobApi'
import { continueFromImage } from '../../api'
import JobItemCard from './JobItemCard.vue'
import StopJobDialog from './StopJobDialog.vue'
import RetryItemDialog from './RetryItemDialog.vue'
import ImageLightbox from './ImageLightbox.vue'

const props = defineProps<{
  /** 要盯的批次任务号。换一个值就会切过去盯新的。 */
  jobUid: string
  /** 单张预估价，重试确认弹窗要显示它。 */
  unitPrice: Price
}>()

const emit = defineEmits<{
  /** 整批跑完了（父组件据此刷新余额）。 */
  (e: 'finished'): void
  /** 钱动过了（停止排队 / 重试），父组件刷新余额。 */
  (e: 'balanceChanged'): void
  /** 「用这张继续生成」：把出好的图变成的新商品图交给工作台，塞进第一步。 */
  (e: 'continue-with-asset', asset: DesignkitAsset): void
}>()

const { t } = useI18n()

/**
 * 「用这张继续生成」正在准备的是哪一张（按 seq）。0 = 没有在准备。
 *
 * 按 seq 而不是布尔：一次只允许准备一张，但要让**那一张**的按钮转圈，
 * 而不是所有按钮一起变灰 —— 后者会让运营以为整个页面卡住了。
 */
const continuingSeq = ref(0)

/**
 * 把这一张出好的图变成新的商品图，交给上层塞进第一步。
 *
 * ⚠ 图片字节**不经过浏览器**：服务端直接从存储里复制（见 api 的 continueFromImage）。
 */
async function continueFrom(item: JobItem) {
  const images = item.images ?? []
  const current = images.find((one) => one.is_current) ?? images[0]
  if (!current?.uid || continuingSeq.value !== 0) {
    return
  }
  continuingSeq.value = item.seq
  try {
    const asset = await continueFromImage(current.uid)
    // ⚠ 提示语由**上层**给：只有它知道这张图到底加进去没有
    //（可能重复、可能到张数上限）。这里一律报「已加好」会骗人 ——
    // monica 2026-08-14 看到的就是「说加好了但第一步还是空的」。
    emit('continue-with-asset', asset)
  } catch {
    appStore.showToast('error', t('designkit.gallery.continueFailed'))
  } finally {
    continuingSeq.value = 0
  }
}
const appStore = useAppStore()

const polling = useJobPolling({
  onFinished: () => {
    emit('finished')
  },
})
const images = useAuthedImages()

// 模板里要用的都从 polling 上取出来（它返回的是普通对象，模板不会自动解 ref）。
const job = polling.job
const items = polling.items
const progress = polling.progress
const finished = polling.finished
const initialLoading = polling.initialLoading
const paused = polling.paused
const warning = polling.warning
const pollingError = polling.error

const stopDialogOpen = ref(false)
const stopping = ref(false)
const retryTarget = ref<JobItem | null>(null)
const retrying = ref(false)
const lightboxItem = ref<JobItem | null>(null)
const downloadingSeq = ref<number | null>(null)

/**
 * 重试的防重复标识。
 *
 * **打开确认弹窗时生成一个存住**，弹窗里点几次、超时重发都用同一个——
 * 后端据此认出「这是同一次重试」，只会重出一张、只扣一份钱。
 */
let retryIdempotencyKey = ''

const jobLabel = computed(() => {
  const name = job.value?.name ?? ''
  return name.trim() !== '' ? name : t('designkit.job.untitled')
})

/** 还能停：批次没结束。已经在跑的停不住，那句话由弹窗负责说。 */
const canStop = computed(() => job.value !== null && !finished.value)

/** 跑完之后那一句话。**判断有没有出图一律看 success_count，不看状态。** */
const finishedText = computed(() => {
  const current = job.value
  if (!current) {
    return ''
  }
  if (current.status === 'cancelled') {
    return t('designkit.job.finishedCancelled')
  }
  if (current.fail_count > 0 && current.success_count > 0) {
    return t('designkit.job.finishedPartial', {
      success: current.success_count,
      fail: current.fail_count,
    })
  }
  if (current.success_count === 0) {
    return t('designkit.job.finishedFailed')
  }
  return t('designkit.job.finishedAll')
})

/** 这一张的结果图地址（后端给的原始地址，要经 useAuthedImages 换成本地地址）。 */
function contentUrlOf(item: JobItem): string {
  const first = item.images.length > 0 ? item.images[0] : null
  return first ? first.content_url : ''
}

/** 已经取好的本地缩略图地址；还没取到就是空串。 */
function thumbUrlOf(item: JobItem): string {
  const url = contentUrlOf(item)
  if (url === '') {
    return ''
  }
  return images.urls.value[url] ?? ''
}

/** 取图失败的中文原因；没失败是空串。 */
function thumbFailedTextOf(item: JobItem): string {
  const url = contentUrlOf(item)
  if (url === '') {
    return ''
  }
  const failure = images.failed.value[url]
  return failure ? failure.message : ''
}

// 出一张取一张。已经取过的会自动跳过，可以放心重复调。
watch(
  items,
  (list) => {
    images.ensure(list.map((item) => contentUrlOf(item)).filter((url) => url !== ''))
  },
  { immediate: true },
)

// 换批次就重新开始盯。
watch(
  () => props.jobUid,
  (uid) => {
    lightboxItem.value = null
    retryTarget.value = null
    stopDialogOpen.value = false
    images.releaseAll()
    if (uid) {
      polling.start(uid)
    } else {
      polling.stop()
    }
  },
  { immediate: true },
)

async function reload(): Promise<void> {
  await polling.refresh()
}

// ---------------------------------------------------------------------------
// 停止排队（决策 21）
// ---------------------------------------------------------------------------

function openStopDialog(): void {
  stopDialogOpen.value = true
}

async function confirmStop(): Promise<void> {
  if (stopping.value) {
    return
  }
  stopping.value = true
  try {
    const result = await stopJob(props.jobUid)
    // 后端那句话已经把「哪些不扣费、哪些照扣费」说清楚了，直接显示，不要自己拼。
    appStore.showSuccess(result.message !== '' ? result.message : t('designkit.stop.done'))
    stopDialogOpen.value = false
    emit('balanceChanged')
    await polling.refresh()
  } catch (err) {
    if (!isCanceledError(err)) {
      // 「这批任务已经结束了，不用再停。」也走这里——不是故障，是一句人话。
      appStore.showError(errorText(err))
      stopDialogOpen.value = false
      await polling.refresh()
    }
  } finally {
    stopping.value = false
  }
}

// ---------------------------------------------------------------------------
// 重试一张（决策 20：重试 = 重新收一次钱）
// ---------------------------------------------------------------------------

function openRetryDialog(item: JobItem): void {
  retryTarget.value = item
  // 一次确认流程一个标识：点几次都只重出一张、只扣一份钱。
  retryIdempotencyKey = newIdempotencyKey()
}

function closeRetryDialog(): void {
  retryTarget.value = null
  retryIdempotencyKey = ''
}

async function confirmRetry(): Promise<void> {
  const target = retryTarget.value
  if (!target || retrying.value) {
    return
  }
  retrying.value = true
  try {
    const result = await retryJobItem(props.jobUid, target.seq, retryIdempotencyKey)
    // 后端这句话里已经写明「会重新收一次费用」和「还能再试几次」。
    appStore.showSuccess(result.message !== '' ? result.message : t('designkit.retry.done'))
    closeRetryDialog()
    emit('balanceChanged')
    // 这一张退回了排队中，整批又开始动了，重新盯起来。
    polling.start(props.jobUid)
  } catch (err) {
    if (!isCanceledError(err)) {
      appStore.showError(errorText(err))
      closeRetryDialog()
      await polling.refresh()
    }
  } finally {
    retrying.value = false
  }
}

// ---------------------------------------------------------------------------
// 看大图 / 下载
// ---------------------------------------------------------------------------

function openLightbox(item: JobItem): void {
  lightboxItem.value = item
}

/** Windows 上文件名里这几个字符会被拒绝，换成下划线。 */
function safeFileName(raw: string): string {
  return raw.replace(/[\\/:*?"<>|]/g, '_').trim()
}

function fileNameOf(item: JobItem): string {
  const first = item.images.length > 0 ? item.images[0] : null
  const contentType = first?.content_type ?? 'image/png'
  const extension = contentType.split('/')[1]?.split(';')[0] || 'png'
  return `${safeFileName(jobLabel.value)}-${item.seq}.${extension}`
}

async function download(item: JobItem): Promise<void> {
  const url = contentUrlOf(item)
  if (url === '' || downloadingSeq.value !== null) {
    return
  }
  downloadingSeq.value = item.seq
  try {
    await downloadContent(url, fileNameOf(item))
  } catch (err) {
    if (!isCanceledError(err)) {
      appStore.showError(errorText(err))
    }
  } finally {
    downloadingSeq.value = null
  }
}

/** 大图弹窗里的「下载」。 */
async function downloadFromLightbox(): Promise<void> {
  const target = lightboxItem.value
  if (target) {
    await download(target)
  }
}

onBeforeUnmount(() => {
  polling.dispose()
  images.dispose()
})
</script>
