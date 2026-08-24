<!--
  第四步：核对花费并提交。

  这一块是整个工作台唯一会花钱的地方，所以它的职责只有一个：
  **让运营在按下按钮之前就知道要花多少钱、够不够、点下去会发生什么。**

  三种必须显示到位的情况：

  1. **价格待确认**（`price_confirmed=false`）——单价和总价都是 null。
     显示后端给的那句中文说明（`price_note`），**绝不显示 $0.00**。
  2. **余额不够**——显示后端算出来的「还差 $X」，并且必须给「申请额度」按钮。
     充值和兑换码菜单对运营是隐藏的（决策 10），只说「不够」等于把人堵死（决策 19）。
  3. **超过单次上限**——提前拦住，别让运营填完一屏再被后端拒绝。

  ⚠ `estimate.sufficient === true` **不等于**一定提交得上去：价格待确认时后端
  一律给 true（拦不住也不该假装拦得住）。真正拦透支的是提交时的数据库检查。
-->
<template>
  <section class="dk-panel">
    <header class="dk-panel-head dk-panel-head--tight">
      <!-- 编号圆点是视觉锚，不替代标题里的「第四步」，读屏直接读标题。
           第四步没有「完成」态——可提交时高亮（is-current），提交了批次就开跑。 -->
      <div class="dk-step" :class="{ 'is-current': stepState === 'current' }">
        <span class="dk-step__dot" aria-hidden="true">4</span>
        <h2 class="dk-panel-title dk-step__title">{{ t('designkit.workbench.stepSubmit') }}</h2>
      </div>
    </header>

    <!-- 这一批出多少张 -->
    <div class="dk-summary-box">
      <p class="dk-note">{{ t('designkit.workbench.summaryTitle') }}</p>
      <p class="dk-text-strong">
        {{ t('designkit.workbench.summaryFormula', { assets: assetCount, prompts: promptCount, total: localTotal }) }}
      </p>
      <p class="dk-note">{{ t('designkit.workbench.summaryOrder') }}</p>
    </div>

    <!-- 给这批起个名字 -->
    <div class="dk-mt-3">
      <label class="dk-label dk-label--quiet" for="designkit-job-name">
        {{ t('designkit.workbench.nameLabel') }}
      </label>
      <input
        id="designkit-job-name"
        :value="name"
        type="text"
        class="dk-input"
        maxlength="60"
        :placeholder="t('designkit.workbench.namePlaceholder')"
        @input="emit('update:name', ($event.target as HTMLInputElement).value)"
      />
    </div>

    <!-- 报价 -->
    <div class="dk-mt-4">
      <p class="dk-eyebrow">{{ t('designkit.estimate.title') }}</p>

      <p v-if="!canEstimate" class="dk-muted dk-mt-2">{{ needWhat }}</p>
      <p v-else-if="loading" class="dk-muted dk-mt-2">{{ t('designkit.estimate.calculating') }}</p>

      <!--
        ⚠ 这四行里的金额一律走 formatPrice / formatMoney：
        价格待确认时它们显示「价格待确认」，**绝不会退化成 $0.00**。
        不要在这里自己拼字符串或者填默认值——0 会被读成「免费」。
      -->
      <div v-else-if="estimate" class="dk-estimate-lines dk-mt-2">
        <p class="dk-text">{{ t('designkit.estimate.total', { count: estimate.item_count }) }}</p>
        <p class="dk-text">
          {{ t('designkit.estimate.unitPrice', { price: formatPrice(estimate.unit_price) }) }}
        </p>
        <p class="dk-estimate-total">
          {{ t('designkit.estimate.estimatedCost', { amount: formatPrice(estimate.estimated_cost) }) }}
        </p>
        <p class="dk-muted">
          {{ t('designkit.estimate.balance', { amount: formatMoney(estimate.balance) }) }}
        </p>
        <p class="dk-muted">
          {{ t('designkit.estimate.available', { amount: formatMoney(estimate.available) }) }}
        </p>
      </div>

      <p v-else-if="loadError" class="dk-note dk-note--danger dk-mt-2">{{ loadError }}</p>
    </div>

    <!-- 价格待确认：这句话必须显示出来 -->
    <p
      v-if="estimate && !estimate.price_confirmed"
      class="dk-alert dk-alert--warning dk-mt-3"
    >
      {{ priceNote !== '' ? priceNote : t('designkit.estimate.unconfirmed') }}
    </p>

    <!-- 超过单次上限 -->
    <p v-if="overLimit && estimate" class="dk-alert dk-alert--warning dk-mt-3">
      {{ t('designkit.estimate.overLimit', { max: estimate?.max_batch_items ?? 0, count: estimate?.item_count ?? 0 }) }}
    </p>

    <!-- 余额不够：必须给出路（决策 19） -->
    <div v-if="insufficient && estimate" class="dk-alert dk-alert--danger dk-mt-3">
      <p>{{ t('designkit.estimate.insufficient', { amount: formatMoney(estimate?.shortfall) }) }}</p>
      <p class="dk-alert__sub">{{ t('designkit.estimate.insufficientHint') }}</p>
      <button
        type="button"
        class="dk-button dk-button--secondary dk-button--sm dk-button--block dk-alert__action"
        @click="emit('requestQuota')"
      >
        {{ t('designkit.quota.request') }}
      </button>
    </div>

    <!-- 提交被后端挡下来时的原因（余额不足、格式不对…） -->
    <div v-if="submitError" class="dk-alert dk-alert--danger dk-mt-3">
      <p>{{ submitError.text }}</p>
      <p v-if="submitError.requestId" class="dk-alert__sub">
        {{ t('designkit.errors.requestId', { id: submitError.requestId }) }}
      </p>
      <button
        v-if="submitError.showQuota"
        type="button"
        class="dk-button dk-button--secondary dk-button--sm dk-button--block dk-alert__action"
        @click="emit('requestQuota')"
      >
        {{ t('designkit.quota.request') }}
      </button>
    </div>

    <button
      type="button"
      class="dk-button dk-button--primary-action dk-mt-4"
      :disabled="submitDisabled"
      @click="emit('submit')"
    >
      {{ submitting ? t('designkit.workbench.submitting') : t('designkit.workbench.submit') }}
    </button>

    <p class="dk-note dk-mt-2">{{ t('designkit.money.currencyNote') }}</p>
  </section>
</template>

<script setup lang="ts">
import { computed } from 'vue'
import { useI18n } from 'vue-i18n'
import { formatMoney, formatPrice } from '../../api'
import type { JobEstimate } from '../../api'
import type { SubmitErrorView, WorkbenchStepState } from './viewTypes'

const props = defineProps<{
  assetCount: number
  promptCount: number
  /** 标题前编号圆点的状态，由页面层推导（viewTypes.ts 的说明）。 */
  stepState?: WorkbenchStepState
  /** 给这批起的名字，可以不填。 */
  name: string
  /** 后端算的报价；还没算出来是 null。 */
  estimate: JobEstimate | null
  /** 价格待确认时后端给的中文说明；空串表示价格已确认。 */
  priceNote: string
  loading: boolean
  /** 报价请求本身失败的中文原因。 */
  loadError: string | null
  submitting: boolean
  submitError: SubmitErrorView | null
}>()

const emit = defineEmits<{
  (e: 'update:name', value: string): void
  (e: 'submit'): void
  (e: 'requestQuota'): void
}>()

const { t } = useI18n()

/** 本地先算一个张数，让「会出几张」在报价回来之前就有得看。 */
const localTotal = computed(() => props.assetCount * props.promptCount)

/** 两头都选了才有得算。 */
const canEstimate = computed(() => props.assetCount > 0 && props.promptCount > 0)

/** 还缺什么：一次只说一件事，别一口气报三条。 */
const needWhat = computed(() => {
  if (props.assetCount === 0) {
    return t('designkit.workbench.needAsset')
  }
  return t('designkit.workbench.needPrompt')
})

const overLimit = computed(
  () => props.estimate !== null && props.estimate.item_count > props.estimate.max_batch_items,
)

/**
 * 余额不够。
 *
 * ⚠ 只在**价格确认过**的时候才判：价格待确认时后端把 sufficient 一律给 true，
 * 那时说「够」或「不够」都是猜的，不如什么都不说，交给提交时的后端检查。
 */
const insufficient = computed(
  () => props.estimate !== null && props.estimate.price_confirmed && !props.estimate.sufficient,
)

const submitDisabled = computed(
  () => props.submitting || !canEstimate.value || props.loading || props.estimate === null || overLimit.value,
)
</script>
