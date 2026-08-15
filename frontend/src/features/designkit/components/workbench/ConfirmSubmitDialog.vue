<!--
  提交前的二次确认。**这是运营点下去就开始花钱的最后一道关。**

  必须写清楚三件事（决策 12 / 20 / 21）：
    1. 这一批会出几张；
    2. 预计花多少钱——**没有价格时写「价格待确认」，不许显示 $0**；
    3. 提交之后能做什么、不能做什么：还没开始的能停下来不扣费，
       已经在生成的停不住、照扣；失败了想重来要重新收一次钱。

  「取消」在这里指的是**关掉弹窗**，所以叫「再想想」；
  出图那个按钮叫「停止排队」（决策 21），两者不要混。
-->
<template>
  <BaseDialog :show="show" :title="t('designkit.confirm.title')" width="normal" @close="emit('cancel')">
    <div class="dk-stack dk-stack--sm">
      <p class="dk-text-strong">{{ mainText }}</p>

      <!-- 价格待确认时后端给的中文说明，原样显示 -->
      <p v-if="!cost.confirmed" class="dk-alert dk-alert--warning">
        {{ priceNote !== '' ? priceNote : t('designkit.estimate.unconfirmed') }}
      </p>

      <!-- 提交之后能不能反悔（决策 21） -->
      <p class="dk-note">{{ t('designkit.confirm.note') }}</p>

      <!-- 重试要重新收钱（决策 20）。现在说，比失败之后再说有用得多。 -->
      <p class="dk-note">{{ retryNote }}</p>

      <p class="dk-note dk-note--quiet">{{ t('designkit.money.currencyNote') }}</p>
    </div>

    <template #footer>
      <div class="dk-dialog-actions">
        <button
          type="button"
          class="dk-button dk-button--secondary"
          :disabled="submitting"
          @click="emit('cancel')"
        >
          {{ t('designkit.confirm.cancel') }}
        </button>
        <button
          type="button"
          class="dk-button"
          :disabled="submitting"
          @click="emit('confirm')"
        >
          {{ submitting ? t('designkit.workbench.submitting') : t('designkit.confirm.submit') }}
        </button>
      </div>
    </template>
  </BaseDialog>
</template>

<script setup lang="ts">
import { computed } from 'vue'
import { useI18n } from 'vue-i18n'
import BaseDialog from '@/components/common/BaseDialog.vue'
import { formatPrice } from '../../api'
import type { Price } from '../../api'

const props = defineProps<{
  show: boolean
  /** 这一批会出几张。 */
  count: number
  /** 预估总花费。**待确认时不许当成 0。** */
  cost: Price
  /** 单张预估价，用来说明「重试一次要再花多少」。 */
  unitPrice: Price
  /** 价格待确认时后端给的中文说明。 */
  priceNote: string
  /** 正在提交，按钮要挡住重复点击。 */
  submitting: boolean
}>()

const emit = defineEmits<{
  (e: 'confirm'): void
  (e: 'cancel'): void
}>()

const { t } = useI18n()

/** 价格确认过就说总价，没确认就说「算不出总价」——两句话都由 i18n 提供。 */
const mainText = computed(() => {
  if (props.cost.confirmed) {
    return t('designkit.confirm.body', { count: props.count, amount: formatPrice(props.cost) })
  }
  return t('designkit.confirm.bodyUnconfirmed', { count: props.count })
})

/** 重试要重新收钱这件事，有单价就把单价一起说出来。 */
const retryNote = computed(() => {
  if (props.unitPrice.confirmed) {
    return t('designkit.retry.body', { price: formatPrice(props.unitPrice) })
  }
  return t('designkit.retry.bodyUnconfirmed')
})
</script>
