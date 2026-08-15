<!--
  出图比例的编辑器。

  为什么不做成一个「用逗号隔开的文本框」：那种框最常见的事故是运营打了个中文逗号
  或者中文冒号，保存之后整行比例全变成非法值，工作台上一个比例都选不出来 ——
  而页面上看着是填了的。所以这里一行一个比例，格式当场校验，还能上下挪顺序
  （**顺序就是工作台上按钮的顺序，第一个是默认选中的那个**）。

  这里只做「格式对不对、有没有重复、有没有留空」这三件事的即时提示；
  真正的护栏在后端（handler/settings_handler.go），改网页绕不过去。
-->
<template>
  <div class="dk-ratio-editor">
    <ol class="dk-ratio-editor__list">
      <li v-for="(ratio, index) in modelValue" :key="index" class="dk-ratio-editor__row">
        <span class="dk-ratio-editor__index" aria-hidden="true">{{ index + 1 }}</span>

        <input
          :id="`dk-ratio-${index}`"
          class="dk-input dk-ratio-editor__input"
          :class="{ 'is-invalid': errorAt(index) !== '' }"
          type="text"
          inputmode="text"
          autocomplete="off"
          :value="ratio"
          :placeholder="t('designkit.settings.ratiosPlaceholder')"
          :aria-label="t('designkit.settings.ratiosLabel') + ' ' + (index + 1)"
          :aria-invalid="errorAt(index) !== ''"
          @input="update(index, ($event.target as HTMLInputElement).value)"
        />

        <!-- 第一个是默认选中的比例，标出来；不然管理员不知道顺序有什么用 -->
        <span v-if="index === 0" class="dk-badge dk-badge--info dk-ratio-editor__default">
          {{ t('designkit.settings.ratiosDefault') }}
        </span>

        <div class="dk-ratio-editor__actions">
          <button
            type="button"
            class="dk-button dk-button--quiet dk-button--sm"
            :disabled="index === 0"
            :title="t('designkit.settings.ratiosUp')"
            :aria-label="t('designkit.settings.ratiosUp')"
            @click="move(index, -1)"
          >
            ↑
          </button>
          <button
            type="button"
            class="dk-button dk-button--quiet dk-button--sm"
            :disabled="index === modelValue.length - 1"
            :title="t('designkit.settings.ratiosDown')"
            :aria-label="t('designkit.settings.ratiosDown')"
            @click="move(index, 1)"
          >
            ↓
          </button>
          <button
            type="button"
            class="dk-button dk-button--quiet dk-button--sm dk-ratio-editor__remove"
            :title="t('designkit.settings.ratiosRemove')"
            :aria-label="t('designkit.settings.ratiosRemove')"
            @click="remove(index)"
          >
            ✕
          </button>
        </div>

        <p v-if="errorAt(index) !== ''" class="dk-note dk-note--danger dk-ratio-editor__error">
          {{ errorAt(index) }}
        </p>
      </li>
    </ol>

    <button
      type="button"
      class="dk-button dk-button--secondary dk-button--sm"
      :disabled="modelValue.length >= max"
      @click="add()"
    >
      {{ t('designkit.settings.ratiosAdd') }}
    </button>
  </div>
</template>

<script setup lang="ts">
import { computed } from 'vue'
import { useI18n } from 'vue-i18n'

const props = defineProps<{
  modelValue: string[]
  /** 最多配几个（后端下发的 limits.ratios_max）。 */
  max: number
}>()

const emit = defineEmits<{
  (e: 'update:modelValue', value: string[]): void
}>()

const { t } = useI18n()

/** 「宽:高」，两边都是正整数，中间是**英文**冒号。 */
const RATIO_PATTERN = /^\d+:\d+$/

/**
 * 每一行的错误。空串表示这一行没问题。
 *
 * 留空的那一行不报「格式不对」——那是「还没填」，不是「填错了」。
 * 报错会让人以为自己打的东西有问题，其实只是刚点了「添加比例」。
 */
const errors = computed<string[]>(() => {
  const seen = new Map<string, number>()
  return props.modelValue.map((raw) => {
    const value = raw.trim()
    if (value === '') {
      return ''
    }
    if (!RATIO_PATTERN.test(value)) {
      return t('designkit.settings.ratiosInvalid', { ratio: value })
    }
    const first = seen.get(value)
    if (first !== undefined) {
      return t('designkit.settings.ratiosDuplicate', { ratio: value })
    }
    seen.set(value, 1)
    return ''
  })
})

function errorAt(index: number): string {
  return errors.value[index] ?? ''
}

function update(index: number, value: string): void {
  const next = [...props.modelValue]
  next[index] = value
  emit('update:modelValue', next)
}

function add(): void {
  if (props.modelValue.length >= props.max) {
    return
  }
  emit('update:modelValue', [...props.modelValue, ''])
}

function remove(index: number): void {
  const next = [...props.modelValue]
  next.splice(index, 1)
  emit('update:modelValue', next)
}

/** 往前/往后挪一格。顺序就是工作台上按钮的顺序。 */
function move(index: number, delta: number): void {
  const target = index + delta
  if (target < 0 || target >= props.modelValue.length) {
    return
  }
  const next = [...props.modelValue]
  const [moved] = next.splice(index, 1)
  next.splice(target, 0, moved)
  emit('update:modelValue', next)
}
</script>

<style scoped>
.dk-ratio-editor {
  display: grid;
  gap: var(--dk-space-3);
}

.dk-ratio-editor__list {
  display: grid;
  gap: var(--dk-space-2);
  margin: 0;
  padding: 0;
  list-style: none;
}

.dk-ratio-editor__row {
  display: grid;
  grid-template-columns: auto minmax(0, 10rem) 1fr auto;
  align-items: center;
  gap: var(--dk-space-2);
}

.dk-ratio-editor__index {
  min-width: 1.25rem;
  color: var(--dk-text-tertiary);
  font-size: 0.8125rem;
  text-align: right;
}

.dk-ratio-editor__input.is-invalid {
  border-color: var(--dk-danger);
}

.dk-ratio-editor__default {
  justify-self: start;
}

.dk-ratio-editor__actions {
  display: flex;
  gap: var(--dk-space-1);
}

.dk-ratio-editor__remove:hover {
  color: var(--dk-danger);
}

/* 错误信息占满整行，接在这一行下面 */
.dk-ratio-editor__error {
  grid-column: 1 / -1;
  margin: 0;
}

@media (max-width: 640px) {
  .dk-ratio-editor__row {
    grid-template-columns: auto minmax(0, 1fr) auto;
  }

  .dk-ratio-editor__default {
    grid-column: 2 / -1;
  }
}
</style>
