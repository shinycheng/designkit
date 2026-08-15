<!--
  重试一张的二次确认（决策 20）。

  **重试 = 重新出一张图 = 重新收一次钱**，技术上改不了。运营默认
  「失败了当然不该收钱」，不把这句写在按钮上面，就会一顿猛点然后来问
  「为什么失败了还扣我钱」。所以：

    - 有单价就把「这次要花多少」直接写出来；
    - 没有单价（价格待确认）就明说「扣多少以实际出图为准」；
    - **上一次是超时的那一张要额外警告**：那张图很可能已经出来、已经扣过钱了，
      只是结果没回到我们这里（设计定型 8.1）。要不要再花一次，交给人决定。
-->
<template>
  <BaseDialog :show="show" :title="t('designkit.retry.title')" width="normal" @close="emit('cancel')">
    <div class="dk-stack dk-stack--sm">
      <p v-if="seq > 0" class="dk-note">{{ t('designkit.job.seq', { seq }) }}</p>

      <p class="dk-text-strong">{{ bodyText }}</p>

      <!-- 超时那一张：钱多半已经花了 -->
      <p v-if="wasTimeout" class="dk-alert dk-alert--warning">
        {{ t('designkit.retry.timeoutWarning') }}
      </p>

      <p class="dk-note">
        {{ t('designkit.job.attempt', { n: attemptCount }) }} ·
        {{ t('designkit.job.attemptLimit', { max: maxAttempts }) }}
      </p>

      <p class="dk-note dk-note--quiet">{{ t('designkit.money.currencyNote') }}</p>
    </div>

    <template #footer>
      <div class="dk-dialog-actions">
        <button
          type="button"
          class="dk-button dk-button--secondary"
          :disabled="working"
          @click="emit('cancel')"
        >
          {{ t('designkit.retry.cancel') }}
        </button>
        <button type="button" class="dk-button" :disabled="working" @click="emit('confirm')">
          {{ working ? t('designkit.common.loading') : t('designkit.retry.confirm') }}
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
  /** 第几张（从 1 开始）；0 表示不显示。 */
  seq: number
  /** 单张预估价。**待确认时不许当成 0。** */
  unitPrice: Price
  attemptCount: number
  maxAttempts: number
  /** 上一次失败是不是超时（错误码 DK_TIMEOUT）。 */
  wasTimeout: boolean
  working: boolean
}>()

const emit = defineEmits<{
  (e: 'confirm'): void
  (e: 'cancel'): void
}>()

const { t } = useI18n()

const bodyText = computed(() => {
  if (props.unitPrice.confirmed) {
    return t('designkit.retry.body', { price: formatPrice(props.unitPrice) })
  }
  return t('designkit.retry.bodyUnconfirmed')
})
</script>
