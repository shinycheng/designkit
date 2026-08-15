<!--
  「停止排队」的二次确认（决策 21）。

  ⚠ 按钮不叫「取消」，弹窗里也一个「取消出图」都不能写。
  上游出图跟浏览器连接是**故意脱钩**的，已经在跑的那几张停不下来、
  会跑完并照常扣费。按钮叫「取消」而实际照扣钱，会被投诉
  「我明明取消了还扣我钱」——所以这里必须把两笔账分开说清楚：

    还没开始的 N 张：不再生成，**也不扣费**
    正在生成的 M 张：跑完并**正常扣费**，图会保存在「我的图片」里

  关掉弹窗那个按钮写「继续出图」，跟「停止」是一对反义词，不会点错。
-->
<template>
  <BaseDialog :show="show" :title="t('designkit.stop.title')" width="normal" @close="emit('cancel')">
    <div class="dk-stack dk-stack--sm">
      <p class="dk-text">{{ t('designkit.stop.body', { pending: pendingCount, running: runningCount }) }}</p>
      <p class="dk-note">{{ t('designkit.job.cancelledCostNote') }}</p>
    </div>

    <template #footer>
      <div class="dk-dialog-actions">
        <button
          type="button"
          class="dk-button dk-button--secondary"
          :disabled="working"
          @click="emit('cancel')"
        >
          {{ t('designkit.stop.cancel') }}
        </button>
        <button
          type="button"
          class="dk-button dk-button--danger"
          :disabled="working"
          @click="emit('confirm')"
        >
          {{ working ? t('designkit.common.loading') : t('designkit.stop.confirm') }}
        </button>
      </div>
    </template>
  </BaseDialog>
</template>

<script setup lang="ts">
import { useI18n } from 'vue-i18n'
import BaseDialog from '@/components/common/BaseDialog.vue'

defineProps<{
  show: boolean
  /** 还没开始的张数：停了就不生成、不扣费。 */
  pendingCount: number
  /** 正在生成的张数：**停不住，会跑完并照常扣费。** */
  runningCount: number
  working: boolean
}>()

const emit = defineEmits<{
  (e: 'confirm'): void
  (e: 'cancel'): void
}>()

const { t } = useI18n()
</script>
