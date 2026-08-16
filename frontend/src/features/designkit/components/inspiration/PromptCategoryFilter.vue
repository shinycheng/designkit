<!--
  灵感库的分类筛选。

  做成一排「标签」（chips）而不是下拉框，理由：一共只有 11 个分类，全摆出来
  运营一眼能看见有哪些；下拉框要点开才知道，还多一次点击。
  屏幕窄的时候自动换行，不做横向滚动——横滚条在触屏上很容易被滑过头，
  而且会把后面几个分类藏起来，运营根本不知道它们存在。

  每个标签后面带条数：**这个数是「同步进来多少条」的直接证据**。
  分类是空的（0 条）时仍然显示，不隐藏——隐藏了运营会以为是自己看错了。
-->
<template>
  <div class="dk-insp-cats" role="group" :aria-label="t('designkit.inspiration.categoryTitle')">
    <button
      type="button"
      class="dk-insp-cat"
      :class="{ 'is-active': modelValue === '' }"
      :disabled="disabled"
      @click="select('')"
    >
      <span class="dk-insp-cat__name">{{ t('designkit.inspiration.allCategories') }}</span>
      <span class="dk-insp-cat__count">{{ totalCount }}</span>
    </button>

    <!--
      「我的提示词」：紧跟「全部」的一个页签（决策：分类栏顶部一项，不另开页面）。
      选中值是前端内部标记 MY_PROMPTS_FILTER，不是真实分类 slug。
      条数拿不到时（接口失败）不显示数字，页签本身照常——入口不能跟着接口一起消失。
    -->
    <button
      v-if="showMine"
      type="button"
      class="dk-insp-cat"
      :class="{ 'is-active': modelValue === MY_PROMPTS_FILTER }"
      :disabled="disabled"
      @click="select(MY_PROMPTS_FILTER)"
    >
      <span class="dk-insp-cat__name">{{ t('designkit.myPrompts.title') }}</span>
      <span v-if="mineCount !== null && mineCount !== undefined" class="dk-insp-cat__count">
        {{ mineCount }}
      </span>
    </button>

    <button
      v-for="category in categories"
      :key="category.slug"
      type="button"
      class="dk-insp-cat"
      :class="{ 'is-active': modelValue === category.slug }"
      :disabled="disabled"
      @click="select(category.slug)"
    >
      <span class="dk-insp-cat__name">{{ category.name }}</span>
      <span class="dk-insp-cat__count">{{ category.prompt_count }}</span>
    </button>
  </div>
</template>

<script setup lang="ts">
import { useI18n } from 'vue-i18n'
import { MY_PROMPTS_FILTER } from '../../api'
import type { PromptCategory } from '../../api'

defineProps<{
  /** 分类列表，**顺序即显示顺序**（后端已经排好，不要再排一遍）。 */
  categories: PromptCategory[]
  /** 当前选中的分类 slug；空串 = 全部分类；MY_PROMPTS_FILTER = 我的提示词。 */
  modelValue: string
  /** 「全部分类」后面那个总数。 */
  totalCount: number
  /** 要不要显示「我的提示词」页签。灵感库页传 true。 */
  showMine?: boolean
  /** 「我的提示词」的条数；null = 没拿到（页签照常显示，只是不带数字）。 */
  mineCount?: number | null
  /** 正在加载时禁用，避免连点几下把请求排成一串。 */
  disabled?: boolean
}>()

const emit = defineEmits<{
  (e: 'update:modelValue', value: string): void
}>()

const { t } = useI18n()

function select(slug: string): void {
  emit('update:modelValue', slug)
}
</script>

<style scoped>
.dk-insp-cats {
  display: flex;
  flex-wrap: wrap;
  gap: var(--dk-space-2);
}

.dk-insp-cat {
  display: inline-flex;
  align-items: center;
  gap: var(--dk-space-2);
  min-height: 32px;
  padding: 0 var(--dk-space-3);
  border: 1px solid var(--dk-border);
  border-radius: var(--dk-radius-pill);
  background: var(--dk-surface);
  color: var(--dk-text-secondary);
  font-size: var(--dk-text-sm);
  line-height: 1;
  cursor: pointer;
  transition:
    background var(--dk-motion-fast) var(--dk-ease-out),
    border-color var(--dk-motion-fast) var(--dk-ease-out),
    color var(--dk-motion-fast) var(--dk-ease-out);
}

.dk-insp-cat:hover:not(:disabled) {
  background: var(--dk-surface-hover);
  color: var(--dk-text);
}

.dk-insp-cat:focus-visible {
  outline: 2px solid var(--dk-focus);
  outline-offset: 2px;
}

.dk-insp-cat:disabled {
  cursor: not-allowed;
  opacity: 0.6;
}

.dk-insp-cat.is-active {
  background: var(--dk-action);
  border-color: var(--dk-action);
  color: var(--dk-text-inverse);
}

.dk-insp-cat__name {
  white-space: nowrap;
}

.dk-insp-cat__count {
  font-size: var(--dk-text-xs);
  color: var(--dk-text-tertiary);
  font-variant-numeric: tabular-nums;
}

.dk-insp-cat.is-active .dk-insp-cat__count {
  color: var(--dk-text-inverse);
  opacity: 0.8;
}
</style>
