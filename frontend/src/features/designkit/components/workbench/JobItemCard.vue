<!--
  一张图（一个「商品图 × 提示词」）。

  出一张显示一张——**不等整批跑完**。一批可能几十张、要好几分钟，
  让运营盯着一个空白页等是最容易被骂的设计。

  失败的那一张要给三样东西：
    - **中文原因**（后端保证 error_message 是中文，可以直接显示）；
    - 小字错误码，方便截图报障；
    - 「重试这一张」按钮——但重试要重新收一次钱（决策 20），
      所以这里只发事件，真正的二次确认在上层弹窗里。
-->
<template>
  <li class="dk-result-card">
    <!-- 图 / 占位 -->
    <div class="dk-result-card__media">
      <button
        v-if="thumbnailUrl"
        type="button"
        class="dk-result-card__open"
        :title="t('designkit.gallery.openLarge')"
        @click="emit('open')"
      >
        <img :src="thumbnailUrl" :alt="seqLabel" />
      </button>

      <div v-else class="dk-thumb__fallback">
        <p v-if="item.status === 'succeeded' && imageFailedText" class="dk-note dk-note--danger">
          {{ imageFailedText }}
        </p>
        <p v-else-if="item.status === 'succeeded'" class="dk-note">
          {{ t('designkit.gallery.imageLoading') }}
        </p>
        <p v-else class="dk-note">{{ itemStatusLabel(item.status) }}</p>
      </div>

      <span class="dk-thumb__tag">{{ seqLabel }}</span>
      <span class="dk-thumb__tag dk-thumb__tag--state" :class="statusToneClass">
        {{ itemStatusLabel(item.status) }}
      </span>
    </div>

    <div class="dk-result-card__body">
      <!-- 用的提示词（快照，灵感库以后改了也不跟着变） -->
      <p class="dk-note dk-clamp-2" :title="item.prompt_text">{{ item.prompt_text }}</p>

      <!-- 失败原因：中文 + 小字错误码 -->
      <template v-if="item.status === 'failed'">
        <p class="dk-note dk-note--danger">
          {{ item.error_message || t('designkit.errors.unknown') }}
        </p>
        <p v-if="item.error_code" class="dk-mono-note">{{ item.error_code }}</p>
      </template>

      <p v-if="item.billed_cost" class="dk-note dk-note--quiet">
        {{ t('designkit.job.billedCost', { amount: formatMoney(item.billed_cost) }) }}
      </p>

      <p v-if="item.attempt_count > 1" class="dk-note dk-note--quiet">
        {{ t('designkit.job.attempt', { n: item.attempt_count }) }}
      </p>

      <div class="dk-inline-actions dk-mt-auto">
        <button
          v-if="thumbnailUrl"
          type="button"
          class="dk-button dk-button--secondary dk-button--sm dk-button--block"
          @click="emit('download')"
        >
          {{ t('designkit.common.download') }}
        </button>
        <!--
          「用这张继续生成」：把这张出好的图当成新的商品图，接着出下一轮。
          只在真的有图的时候出现（失败的那几张没有图可以继续）。
        -->
        <button
          v-if="thumbnailUrl && currentImageUID !== ''"
          type="button"
          class="dk-button dk-button--secondary dk-button--sm dk-button--block"
          :disabled="continuing"
          :title="t('designkit.gallery.continueHint')"
          @click="emit('continue')"
        >
          {{ continuing
            ? t('designkit.gallery.continueRunning')
            : t('designkit.gallery.continueFromImage') }}
        </button>
        <!--
          「高清放大」：把这张出好的图放大 4 倍，存成一条新的商品图。
          异步排队（一张约 1~2 分钟），这里只发事件、显示上层给的进度文案，
          轮询在上层的 JobProgressPanel 里做。
        -->
        <button
          v-if="thumbnailUrl && currentImageUID !== ''"
          type="button"
          class="dk-button dk-button--secondary dk-button--sm dk-button--block"
          :disabled="upscaleState !== undefined && upscaleState !== ''"
          @click="emit('upscale')"
        >
          {{ upscaleButtonText }}
        </button>
        <!--
          ⚠ 这个按钮点下去要重新收一次钱（决策 20）。这里只发事件，
          真正的二次确认（会写明「这次要花多少」）在上层的 RetryItemDialog 里。
        -->
        <button
          v-if="canRetry"
          type="button"
          class="dk-button dk-button--secondary dk-button--sm dk-button--block"
          @click="emit('retry')"
        >
          {{ t('designkit.retry.button') }}
        </button>
        <p v-else-if="item.status === 'failed'" class="dk-note dk-note--quiet">
          {{ t('designkit.retry.limit', { max: item.max_attempts }) }}
        </p>
      </div>
    </div>
  </li>
</template>

<script setup lang="ts">
import { computed } from 'vue'
import { useI18n } from 'vue-i18n'
import { formatMoney, itemStatusLabel } from '../../api'
import type { JobItem } from '../../api'

const props = defineProps<{
  item: JobItem
  /** 已经取好的本地缩略图地址；还没取到 / 没有图时是空串。 */
  thumbnailUrl: string
  /** 取图失败的中文原因；空串表示没失败。 */
  imageFailedText: string
  /** 「继续生成」正在准备中（上层在调接口）。 */
  continuing?: boolean
  /**
   * 「高清放大」这一张的进度：'' = 没在放，'queued' / 'running' = 在排 / 在放。
   * 轮询在上层（JobProgressPanel），这里只把进度显示成按钮文案。
   */
  upscaleState?: '' | 'queued' | 'running'
}>()

const emit = defineEmits<{
  (e: 'open'): void
  (e: 'download'): void
  (e: 'retry'): void
  (e: 'continue'): void
  (e: 'upscale'): void
}>()

const { t } = useI18n()

/** 放大按钮的文案就是进度：高清放大 → 排队中… → 放大中… */
const upscaleButtonText = computed(() => {
  if (props.upscaleState === 'queued') {
    return t('designkit.upscale.queued')
  }
  if (props.upscaleState === 'running') {
    return t('designkit.upscale.running')
  }
  return t('designkit.upscale.button')
})

const seqLabel = computed(() => t('designkit.job.seq', { seq: props.item.seq }))

/**
 * 「继续生成」要用的那张图的编号。
 *
 * ⚠ 取 `is_current` 的那张，不是数组第一张：重试过的 item 里会同时留着
 * 旧版本和新版本，数组顺序不保证新的在前。拿错了运营会拿上一次失败前的图去继续。
 * 一张都没有（还没出完 / 失败了）时返回空串，按钮不显示。
 */
const currentImageUID = computed(() => {
  const images = props.item.images ?? []
  const current = images.find((one) => one.is_current) ?? images[0]
  return current?.uid ?? ''
})

/** 重试次数没用完的失败图才给按钮；用完了改成一句「试到上限了」。 */
const canRetry = computed(
  () => props.item.status === 'failed' && props.item.attempt_count < props.item.max_attempts,
)

/**
 * 右上角那个小状态块的配色。
 *
 * 类名定义在 components/designkit-ui.css（`.dk-thumb__tag--*`），
 * 颜色全部来自 --dk-* 变量，只有深色一套（2026-08-15 起）。
 * ⚠ 颜色只是辅助：块里同时写着中文状态，**不要用颜色代替文字**。
 */
const statusToneClass = computed(() => {
  switch (props.item.status) {
    case 'succeeded':
      return 'dk-thumb__tag--ok'
    case 'failed':
      return 'dk-thumb__tag--fail'
    case 'running':
      return ''
    case 'cancelled':
    default:
      return 'dk-thumb__tag--idle'
  }
})
</script>
