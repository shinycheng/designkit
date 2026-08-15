<!--
  状态标签（中文 + 颜色）。批次和单张图共用一个组件。

  文案一律走 `jobStatusLabel()` / `itemStatusLabel()`，不要在这里写死中文——
  那两个函数认不出的状态会原样显示英文原词，也比显示一片空白强。

  ⚠ 颜色只是辅助，**不要用颜色代替文字**：色弱的人分不出绿和琥珀。
-->
<template>
  <span class="dk-badge" :class="toneClass">
    <span class="dk-badge__dot" aria-hidden="true"></span>
    {{ label }}
  </span>
</template>

<script setup lang="ts">
import { computed } from 'vue'
import { itemStatusLabel, jobStatusLabel } from '../../api'

const props = withDefaults(
  defineProps<{
    /** 状态原值，例如 'running' / 'partially_failed'。 */
    status: string
    /** 这是一批的状态还是一张图的状态（两套取值不一样）。 */
    kind?: 'job' | 'item'
  }>(),
  { kind: 'job' },
)

const label = computed(() =>
  props.kind === 'item' ? itemStatusLabel(props.status) : jobStatusLabel(props.status),
)

/**
 * 状态 → 配色。
 *
 * 「已停止」用灰色而不是红色：停止排队是运营自己点的，不是出错。
 * 「部分成功」用琥珀色：它既不是成功也不是失败，得让人一眼看出「要去看看」。
 */
const toneClass = computed(() => {
  switch (props.status) {
    case 'succeeded':
      return 'dk-badge--success'
    case 'partially_failed':
      return 'dk-badge--warning'
    case 'failed':
      return 'dk-badge--danger'
    case 'running':
    case 'settling':
      return 'dk-badge--info'
    case 'created':
    case 'holding':
    case 'pending':
    case 'cancelled':
    default:
      // 默认就是 .dk-badge 自带的中性灰，不用再加类。
      return ''
  }
})
</script>
