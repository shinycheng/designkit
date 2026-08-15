<!--
  「我的图片」列表里的一行：一次提交（一批）。

  显示的东西按运营找图的顺序排：名字 → 什么时候出的 → 出了多少张 → 花了多少钱。

  ⚠ 花费那一格有个坑：`actual_cost` 在结算完之前是 null，
  **不能显示成 $0.00** ——那会让人以为这一批不要钱。没结算就显示「预计花费」。
-->
<template>
  <button type="button" class="dk-history-card" @click="emit('open', job.uid)">
    <!-- 第一行：名字 + 状态 -->
    <div class="dk-history-card__title">
      <span>{{ displayName }}</span>
      <GalleryStatusBadge :status="job.status" kind="job" />
    </div>

    <!-- 第二行：时间 / 比例 / 张数 -->
    <div class="dk-meta-row">
      <span>{{ t('designkit.job.createdAt') }} {{ formatDateTimeToMinute(job.created_at) }}</span>
      <span>{{ t('designkit.job.ratio') }} {{ ratioText }}</span>
      <span>{{ t('designkit.job.total', { count: job.item_count }) }}</span>
    </div>

    <!-- 第三行：成功 / 失败 / 已停止 + 花费 -->
    <div class="dk-history-card__foot">
      <span class="dk-note">
        {{
          t('designkit.job.counts', {
            success: job.success_count,
            fail: job.fail_count,
            cancelled: job.cancelled_count,
          })
        }}
      </span>
      <!-- ⚠ 没结算完时显示的是「预计花费」，不是 $0.00（见下面的 costLabel）。 -->
      <span class="dk-history-card__cost">{{ costLabel }} {{ costAmount }}</span>
    </div>

    <!-- 整批失败时把原因直接摆出来，不要让人点进去才知道 -->
    <p v-if="job.last_error_message" class="dk-note dk-note--danger">
      {{ job.last_error_message }}
    </p>
  </button>
</template>

<script setup lang="ts">
import { computed } from 'vue'
import { useI18n } from 'vue-i18n'
import { formatDateTimeToMinute } from '@/utils/format'
import { formatMoney, ratioLabel } from '../../api'
import type { Job } from '../../api'
import GalleryStatusBadge from './GalleryStatusBadge.vue'

const props = defineProps<{ job: Job }>()

const emit = defineEmits<{ (e: 'open', uid: string): void }>()

const { t } = useI18n()

const displayName = computed(() => {
  const name = (props.job.name ?? '').trim()
  return name === '' ? t('designkit.job.untitled') : name
})

/** 例：`3:4 · 详情竖版`。没配中文说明的比例就只显示比例本身。 */
const ratioText = computed(() => {
  const label = ratioLabel(props.job.ratio)
  return label === '' ? props.job.ratio : `${props.job.ratio} · ${label}`
})

const settled = computed(() => typeof props.job.actual_cost === 'string')

const costLabel = computed(() =>
  settled.value ? t('designkit.money.actual') : t('designkit.money.estimated'),
)

const costAmount = computed(() =>
  settled.value ? formatMoney(props.job.actual_cost) : formatMoney(props.job.estimated_cost),
)
</script>
