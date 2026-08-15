<!--
  第三步：选出图比例。

  比例列表从后端 `GET /ratios` 拉，**不写死在前端**——以后加比例是配置问题，
  不用改代码（CLAUDE.md B3-1）。第一次拿到列表时自动选中后端标了 is_default 的那个。

  ⚠ 单价这里必须诚实：`price_confirmed=false` 时显示「价格待确认」，
  **绝不能显示 $0.00**。价目表要真出一张图才知道落哪一档（设计定型 第十节 1），
  显示成 0 会让运营以为免费，一口气排几十张。
-->
<template>
  <section class="dk-panel">
    <header class="dk-panel-head">
      <div>
        <h2 class="dk-panel-title">{{ t('designkit.workbench.stepRatio') }}</h2>
        <p class="dk-panel-hint">{{ t('designkit.ratio.hint') }}</p>
      </div>
    </header>

    <p v-if="loading" class="dk-muted">{{ t('designkit.common.loading') }}</p>

    <div v-else-if="loadError" class="dk-alert dk-alert--danger">
      <p>{{ loadError }}</p>
      <button
        type="button"
        class="dk-button dk-button--secondary dk-button--sm dk-alert__action"
        @click="load"
      >
        {{ t('designkit.common.retry') }}
      </button>
    </div>

    <!--
      ⚠ 每个比例左上角那个小方块的形状由 CSS 的 `[data-ratio="…"]` 决定
      （见 components/designkit-ui.css 的 .dk-size-option__shape）。
      **`data-ratio` 不能去掉**：没有它，「竖图 3:4」和「方图 1:1」长得一模一样，
      运营选错了整批图都要重出，是真金白银。
    -->
    <ul v-else class="dk-size-options">
      <li v-for="option in ratios" :key="option.ratio">
        <button
          type="button"
          class="dk-size-option"
          :class="{ 'is-selected': option.ratio === modelValue }"
          :data-ratio="option.ratio"
          :aria-pressed="option.ratio === modelValue"
          :title="`${option.ratio} · ${t('designkit.gallery.imageSize', {
            width: option.target_width,
            height: option.target_height,
          })} · ${t('designkit.ratio.tier', { tier: option.pricing_tier })}`"
          @click="select(option.ratio)"
        >
          <span class="dk-size-option__shape" aria-hidden="true"></span>
          <span class="dk-size-option__ratio">{{ option.ratio }}</span>
          <span v-if="ratioLabel(option.ratio)" class="dk-size-option__label">
            {{ ratioLabel(option.ratio) }}
          </span>
          <span
            class="dk-size-option__price"
            :class="{ 'is-unconfirmed': !option.price_confirmed }"
          >
            {{ t('designkit.money.perImage') }} {{ formatPrice(option.unit_price) }}
          </span>
        </button>
      </li>
    </ul>

    <p v-if="!loading && !loadError" class="dk-note dk-mt-3">
      {{ t('designkit.ratio.tierHint') }}
    </p>
    <p v-if="hasUnconfirmedPrice" class="dk-note dk-note--warning dk-mt-1">
      {{ t('designkit.money.priceUnconfirmedHint') }}
    </p>
  </section>
</template>

<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted, ref } from 'vue'
import { useI18n } from 'vue-i18n'
import { errorText, formatPrice, isCanceledError, listRatios, ratioLabel } from '../../api'
import type { RatioOption } from '../../api'

defineProps<{
  /** 当前选中的比例，例如 "3:4"。 */
  modelValue: string
}>()

const emit = defineEmits<{
  (e: 'update:modelValue', value: string): void
}>()

const { t } = useI18n()

const ratios = ref<RatioOption[]>([])
const loading = ref(false)
const loadError = ref<string | null>(null)

let controller: AbortController | null = null

const hasUnconfirmedPrice = computed(() => ratios.value.some((r) => !r.price_confirmed))

function select(ratio: string): void {
  emit('update:modelValue', ratio)
}

async function load(): Promise<void> {
  controller?.abort()
  controller = new AbortController()
  loading.value = true
  loadError.value = null
  try {
    const list = await listRatios(controller.signal)
    ratios.value = list.ratios
    // 后端标了默认的就用它；没标就用第一个。列表顺序即界面顺序，不要自己排。
    const preferred = list.ratios.find((r) => r.is_default) ?? list.ratios[0]
    if (preferred) {
      emit('update:modelValue', preferred.ratio)
    }
  } catch (err) {
    if (isCanceledError(err)) {
      return
    }
    loadError.value = errorText(err)
  } finally {
    loading.value = false
  }
}

onMounted(load)

onBeforeUnmount(() => {
  controller?.abort()
})
</script>
