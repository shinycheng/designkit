<!--
  「我的提示词」的新建 / 编辑弹窗：一个标题（可空）+ 一段正文（必填）。

  规则跟后端一致：标题 100 字、正文 5000 字（输入框 maxlength 先拦一道，
  真正的校验在后端，超上限 / 越权 / youmind 不可改都会回中文文案，原样显示）。

  弹窗自己不发请求：点「保存」把内容交给父组件（emit save），保存中 / 失败
  由父组件用 saving / error 两个 prop 回灌进来 —— 失败时弹窗**保持打开**，
  运营改一下能直接重试，内容不丢。
-->
<template>
  <BaseDialog
    :show="show"
    :title="mode === 'create' ? t('designkit.myPrompts.create') : t('designkit.myPrompts.edit')"
    @close="emit('close')"
  >
    <div class="dk-stack dk-stack--sm">
      <label class="dk-label" :for="titleId">{{ t('designkit.myPrompts.titleLabel') }}</label>
      <input
        :id="titleId"
        v-model="title"
        type="text"
        class="dk-input"
        :maxlength="MY_PROMPT_TITLE_MAX"
        :disabled="saving"
      />

      <label class="dk-label dk-mt-2" :for="bodyId">{{ t('designkit.myPrompts.bodyLabel') }}</label>
      <textarea
        :id="bodyId"
        v-model="body"
        rows="8"
        class="dk-textarea"
        :maxlength="MY_PROMPT_BODY_MAX"
        :disabled="saving"
      ></textarea>

      <p v-if="error !== ''" class="dk-note dk-note--danger">{{ error }}</p>
    </div>

    <template #footer>
      <div class="dk-dialog-actions">
        <button
          type="button"
          class="dk-button dk-button--secondary"
          :disabled="saving"
          @click="emit('close')"
        >
          {{ t('designkit.common.cancel') }}
        </button>
        <button
          type="button"
          class="dk-button"
          :disabled="saving || body.trim() === ''"
          @click="emit('save', { title: title.trim(), body: body.trim() })"
        >
          {{ saving ? t('designkit.common.loading') : t('designkit.myPrompts.save') }}
        </button>
      </div>
    </template>
  </BaseDialog>
</template>

<script setup lang="ts">
import { ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import BaseDialog from '@/components/common/BaseDialog.vue'
import { MY_PROMPT_BODY_MAX, MY_PROMPT_TITLE_MAX } from '../../api'
import type { MyPromptInput } from '../../api'

const props = defineProps<{
  show: boolean
  /** create = 新建；edit = 改一条已有的。 */
  mode: 'create' | 'edit'
  /** 编辑时的初始内容；新建传空串。 */
  initialTitle: string
  initialBody: string
  /** 保存请求进行中（父组件在发请求）。 */
  saving: boolean
  /** 保存失败的中文文案；空串 = 没出错。失败时弹窗保持打开，内容不丢。 */
  error: string
}>()

const emit = defineEmits<{
  (e: 'close'): void
  (e: 'save', input: MyPromptInput): void
}>()

const { t } = useI18n()

const titleId = 'dk-my-prompt-title'
const bodyId = 'dk-my-prompt-body'

const title = ref(props.initialTitle)
const body = ref(props.initialBody)

// 每次打开时按 initial* 重置：上一次编辑的残留不能带进下一条。
watch(
  () => props.show,
  (show) => {
    if (show) {
      title.value = props.initialTitle
      body.value = props.initialBody
    }
  },
)
</script>
