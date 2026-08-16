<!--
  灵感库列表里的一张卡片。

  一张卡片上只放运营真正要看的四样：示例图、标题、正文前几行、要填几处空。
  正文**故意只显示三行**——一万多条摆在一起，全文展开会让页面长到没法翻；
  想看全文点开详情，那里是完整的、可复制的。

  两个动作分得很开：
    · 点卡片主体 = 看详情（先看清楚再决定）
    · 点「用它生成」 = 直接带到工作台（不花钱，工作台还要再确认一次）

  「用它生成」按钮在卡片里也保留，是因为熟悉的运营认得出常用那几条，
  不想每次都点开看一遍。带过去之后仍然可以在工作台里改。
-->
<template>
  <article class="dk-insp-card">
    <!-- 示例图：外部网站的图，加载不出来就整块不占位（不要留一个碎图标） -->
    <div v-if="showPreview" class="dk-insp-card__media">
      <img
        :src="prompt.preview_url ?? ''"
        :alt="displayTitle"
        loading="lazy"
        referrerpolicy="no-referrer"
        @error="previewFailed = true"
      />
    </div>

    <button type="button" class="dk-insp-card__open" @click="emit('open')">
      <span class="dk-insp-card__title">{{ displayTitle }}</span>
      <span class="dk-insp-card__body">{{ prompt.body }}</span>
    </button>

    <div class="dk-insp-card__foot">
      <div class="dk-insp-card__tags">
        <span v-if="prompt.category_name !== ''" class="dk-insp-card__tag">
          {{ prompt.category_name }}
        </span>
        <span v-if="prompt.variables.length > 0" class="dk-insp-card__tag">
          {{ t('designkit.inspiration.variableCount', { count: prompt.variables.length }) }}
        </span>
      </div>

      <div class="dk-insp-card__actions">
        <!-- 「我的提示词」页签里多两个动作：编辑、删除（其余卡片没有）。 -->
        <template v-if="editable">
          <button
            type="button"
            class="dk-button dk-button--quiet dk-button--sm"
            @click="emit('edit')"
          >
            {{ t('designkit.myPrompts.edit') }}
          </button>
          <button
            type="button"
            class="dk-button dk-button--quiet dk-button--sm"
            @click="confirmingDelete = true"
          >
            {{ t('designkit.common.delete') }}
          </button>
        </template>
        <button type="button" class="dk-button dk-button--sm" @click="emit('use')">
          {{ t('designkit.inspiration.useThis') }}
        </button>
      </div>
    </div>

    <!-- 原地确认（照抄对话列表的删除确认）：删了找不回来，但历史出图不受影响。 -->
    <div v-if="editable && confirmingDelete" class="dk-insp-card__confirm">
      <span class="dk-note dk-note--danger">{{ t('designkit.myPrompts.deleteConfirm') }}</span>
      <span class="dk-inline-actions">
        <button
          type="button"
          class="dk-button dk-button--danger dk-button--sm"
          @click="confirmDelete()"
        >
          {{ t('designkit.common.delete') }}
        </button>
        <button
          type="button"
          class="dk-button dk-button--quiet dk-button--sm"
          @click="confirmingDelete = false"
        >
          {{ t('designkit.common.cancel') }}
        </button>
      </span>
    </div>
  </article>
</template>

<script setup lang="ts">
import { computed, ref } from 'vue'
import { useI18n } from 'vue-i18n'
import type { Prompt } from '../../api'

const props = defineProps<{
  prompt: Prompt
  /** 「我的提示词」页签里为 true：多出编辑 / 删除两个动作。 */
  editable?: boolean
}>()

const emit = defineEmits<{
  /** 点了卡片主体：打开详情（我的提示词页签里是打开编辑）。 */
  (e: 'open'): void
  /** 点了「用它生成」：直接带到工作台。 */
  (e: 'use'): void
  /** 点了「编辑」（仅 editable）。 */
  (e: 'edit'): void
  /** 原地确认之后才发出（仅 editable）——收到它就可以直接删，不用再问一遍。 */
  (e: 'delete'): void
}>()

const { t } = useI18n()

/** 删除的原地确认开关。确认条收在卡片底部，不用弹窗。 */
const confirmingDelete = ref(false)

function confirmDelete(): void {
  confirmingDelete.value = false
  emit('delete')
}

/** 示例图加载失败（外部网站，国内网络常常拉不到）。失败就整块不显示。 */
const previewFailed = ref(false)

const showPreview = computed(
  () => !previewFailed.value && (props.prompt.preview_url ?? '').trim() !== '',
)

/**
 * 标题。上游有不少条**没有标题**，这时候拿正文开头顶上——
 * 卡片上留一行空白，运营会以为这条坏了。
 */
const displayTitle = computed(() => {
  const title = props.prompt.title.trim()
  if (title !== '') {
    return title
  }
  const body = props.prompt.body.trim().replace(/\s+/g, ' ')
  return body.length > 32 ? `${body.slice(0, 32)}…` : body
})
</script>

<style scoped>
.dk-insp-card {
  display: flex;
  flex-direction: column;
  min-width: 0;
  border: 1px solid var(--dk-border);
  border-radius: var(--dk-radius-lg);
  background: var(--dk-surface);
  overflow: hidden;
  transition:
    border-color var(--dk-motion-fast) var(--dk-ease-out),
    box-shadow var(--dk-motion-fast) var(--dk-ease-out);
}

.dk-insp-card:hover {
  border-color: var(--dk-border-strong);
  box-shadow: var(--dk-shadow-1);
}

.dk-insp-card__media {
  width: 100%;
  aspect-ratio: 4 / 3;
  background: var(--dk-surface-subtle);
  overflow: hidden;
}

.dk-insp-card__media img {
  width: 100%;
  height: 100%;
  object-fit: cover;
  display: block;
}

.dk-insp-card__open {
  display: flex;
  flex-direction: column;
  gap: var(--dk-space-2);
  flex: 1;
  min-width: 0;
  padding: var(--dk-space-4);
  border: 0;
  background: transparent;
  text-align: left;
  cursor: pointer;
}

.dk-insp-card__open:focus-visible {
  outline: 2px solid var(--dk-focus);
  outline-offset: -2px;
}

.dk-insp-card__title {
  font-size: var(--dk-text-md);
  font-weight: 600;
  color: var(--dk-text);
  line-height: var(--dk-leading-tight);
  display: -webkit-box;
  -webkit-line-clamp: 2;
  -webkit-box-orient: vertical;
  overflow: hidden;
}

.dk-insp-card__body {
  font-size: var(--dk-text-sm);
  color: var(--dk-text-secondary);
  line-height: var(--dk-leading);
  display: -webkit-box;
  -webkit-line-clamp: 3;
  -webkit-box-orient: vertical;
  overflow: hidden;
  white-space: pre-wrap;
  word-break: break-word;
}

.dk-insp-card__foot {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: var(--dk-space-2);
  padding: 0 var(--dk-space-4) var(--dk-space-4);
}

.dk-insp-card__tags {
  display: flex;
  flex-wrap: wrap;
  gap: var(--dk-space-1);
  min-width: 0;
}

.dk-insp-card__tag {
  display: inline-flex;
  align-items: center;
  padding: 2px var(--dk-space-2);
  border-radius: var(--dk-radius-pill);
  background: var(--dk-surface-subtle);
  color: var(--dk-text-tertiary);
  font-size: var(--dk-text-xs);
  white-space: nowrap;
}

.dk-insp-card__actions {
  display: flex;
  align-items: center;
  gap: var(--dk-space-1);
  flex-shrink: 0;
}

.dk-insp-card__confirm {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: var(--dk-space-2);
  flex-wrap: wrap;
  padding: var(--dk-space-2) var(--dk-space-4) var(--dk-space-3);
  border-top: 1px solid var(--dk-border);
}
</style>
