<!--
  AI 对话：底部输入条（文字 + 附图）。

  - 回车发送、Shift+回车换行。⚠ 中文输入法候选态的回车（isComposing）不发送——
    不判这个，运营选字按回车就把半句话发出去了。
  - 「发图片」直接调 `uploadAsset()`（跟工作台上传商品图同一个接口，拿 asset_uid）。
    ⚠ **不用 AssetUploader 组件**：它是单向组件、渲染自己内部的列表，
    塞不进这里的 chip 形态（CLAUDE.md 决策 29 的坑）。
  - 附图上限 3 张（MAX_CHAT_ASSETS）、单张 2MB（MAX_CHAT_IMAGE_BYTES，
    比工作台的 20MB 严——图是给对话模型看的，不参与出图）。
  - 发送把「文字 + 已传好的 asset_uid」emit 上去就清空自己；
    失败与重发都由页面层管（消息已经显示在流里）。
  - 有附图还在传、或有附图传失败时不允许发送：静默丢掉一张传失败的图，
    运营会以为 AI「看过图了」，答非所问还找不到原因。
-->
<template>
  <div class="dk-chat-input">
    <!-- 附图 chip：传输中带百分比，失败标红，都能删。 -->
    <ul v-if="attachments.length > 0" class="dk-chat-chips">
      <li
        v-for="entry in attachments"
        :key="entry.key"
        class="dk-chat-chip"
        :class="{ 'is-failed': entry.status === 'failed' }"
      >
        <span class="dk-chat-chip__name dk-truncate">{{ entry.name }}</span>
        <span v-if="entry.status === 'uploading'" class="dk-chat-chip__state">
          {{ t('designkit.upload.uploading', { percent: entry.percent }) }}
        </span>
        <span v-else-if="entry.status === 'failed'" class="dk-chat-chip__state">
          {{ t('designkit.upload.failed') }}
        </span>
        <button
          type="button"
          class="dk-chat-chip__remove"
          :aria-label="t('designkit.common.remove')"
          @click="removeAttachment(entry.key)"
        >
          ×
        </button>
      </li>
    </ul>

    <textarea
      v-model="text"
      class="dk-textarea dk-chat-textarea"
      rows="2"
      :maxlength="MAX_CHAT_TEXT_LENGTH"
      :placeholder="t('designkit.chat.inputPlaceholder')"
      :disabled="disabled"
      @keydown="onKeydown"
    ></textarea>

    <div class="dk-chat-input__actions">
      <input
        ref="fileInput"
        type="file"
        class="dk-file-input"
        multiple
        :accept="ACCEPT_ATTRIBUTE"
        @change="onPickFiles"
      />
      <button
        type="button"
        class="dk-button dk-button--quiet dk-button--sm"
        :disabled="disabled || attachments.length >= MAX_CHAT_ASSETS"
        @click="fileInput?.click()"
      >
        {{ t('designkit.chat.attach') }}
      </button>
      <span v-if="attachments.length > 0" class="dk-note dk-note--quiet">
        {{ attachments.length }}/{{ MAX_CHAT_ASSETS }}
      </span>
      <button
        type="button"
        class="dk-button dk-button--sm dk-chat-send"
        :disabled="!canSend"
        @click="trySend"
      >
        {{ t('designkit.chat.send') }}
      </button>
    </div>
  </div>
</template>

<script setup lang="ts">
import { computed, onBeforeUnmount, ref } from 'vue'
import { useI18n } from 'vue-i18n'
import { useAppStore } from '@/stores'
import {
  ACCEPT_ATTRIBUTE,
  MAX_CHAT_ASSETS,
  MAX_CHAT_IMAGE_BYTES,
  MAX_CHAT_TEXT_LENGTH,
  checkUploadFile,
  errorText,
  isCanceledError,
  uploadAsset,
} from '../../api'
import type { DesignkitAsset } from '../../api'

const props = defineProps<{
  /** AI 回复中 / 历史加载中：整条输入禁用。 */
  disabled: boolean
}>()

const emit = defineEmits<{
  (e: 'send', payload: { text: string; assetUids: string[] }): void
}>()

const { t } = useI18n()
const appStore = useAppStore()

interface AttachmentEntry {
  key: string
  name: string
  status: 'uploading' | 'done' | 'failed'
  percent: number
  asset: DesignkitAsset | null
  controller: AbortController
}

const text = ref('')
const attachments = ref<AttachmentEntry[]>([])
const fileInput = ref<HTMLInputElement | null>(null)

let keyCounter = 0

/**
 * 能不能发：有字、没在等回复、附图全传完（没有传输中也没有失败的）。
 * 文字必填（后端契约），只发图不发字是不行的。
 */
const canSend = computed(
  () =>
    !props.disabled &&
    text.value.trim() !== '' &&
    attachments.value.every((entry) => entry.status === 'done'),
)

function onKeydown(event: KeyboardEvent): void {
  if (event.key !== 'Enter' || event.shiftKey) {
    return
  }
  // 中文输入法正在选字：这个回车是「上屏」，不是「发送」。
  if (event.isComposing) {
    return
  }
  event.preventDefault()
  trySend()
}

function trySend(): void {
  if (!canSend.value) {
    return
  }
  const assetUids = attachments.value
    .map((entry) => entry.asset?.uid ?? '')
    .filter((uid) => uid !== '')
  emit('send', { text: text.value.trim(), assetUids })
  // 消息已经进了消息流，这里立刻清空；失败重发用的是消息流里存的那份。
  text.value = ''
  attachments.value = []
}

function onPickFiles(event: Event): void {
  const input = event.target as HTMLInputElement
  const files = Array.from(input.files ?? [])
  // 允许再选同一个文件（删了重选）。
  input.value = ''

  for (const file of files) {
    if (attachments.value.length >= MAX_CHAT_ASSETS) {
      appStore.showWarning(t('designkit.chat.attachLimit', { max: MAX_CHAT_ASSETS }))
      break
    }
    if (file.size > MAX_CHAT_IMAGE_BYTES) {
      appStore.showWarning(t('designkit.chat.attachTooLarge'))
      continue
    }
    // 格式、空文件这些交给工作台同一套预检（拿不准的放行给后端）。
    const check = checkUploadFile(file)
    if (!check.ok) {
      appStore.showWarning(t(check.reasonKey ?? 'designkit.upload.unsupported'))
      continue
    }
    startUpload(file)
  }
}

function startUpload(file: File): void {
  keyCounter += 1
  const entry: AttachmentEntry = {
    key: `chip-${Date.now()}-${keyCounter}`,
    name: file.name,
    status: 'uploading',
    percent: 0,
    asset: null,
    controller: new AbortController(),
  }
  attachments.value = [...attachments.value, entry]

  void uploadAsset(file, {
    signal: entry.controller.signal,
    onProgress: (percent) => {
      const target = attachments.value.find((one) => one.key === entry.key)
      if (target) {
        target.percent = percent
      }
    },
  })
    .then((asset) => {
      const target = attachments.value.find((one) => one.key === entry.key)
      if (target) {
        target.status = 'done'
        target.asset = asset
      }
    })
    .catch((err) => {
      if (isCanceledError(err)) {
        return
      }
      const target = attachments.value.find((one) => one.key === entry.key)
      if (target) {
        target.status = 'failed'
      }
      appStore.showError(errorText(err))
    })
}

function removeAttachment(key: string): void {
  const target = attachments.value.find((one) => one.key === key)
  target?.controller.abort()
  attachments.value = attachments.value.filter((one) => one.key !== key)
}

onBeforeUnmount(() => {
  for (const entry of attachments.value) {
    entry.controller.abort()
  }
})
</script>

<style scoped>
.dk-chat-input {
  display: grid;
  min-width: 0;
  gap: var(--dk-space-2);
  border-top: 1px solid var(--dk-border);
  padding-top: var(--dk-space-3);
}

.dk-chat-chips {
  display: flex;
  min-width: 0;
  flex-wrap: wrap;
  gap: var(--dk-space-2);
  list-style: none;
  margin: 0;
  padding: 0;
}

.dk-chat-chip {
  display: inline-flex;
  max-width: 100%;
  min-height: 26px;
  align-items: center;
  gap: var(--dk-space-1);
  border: 1px solid var(--dk-border);
  border-radius: var(--dk-radius-pill);
  background: var(--dk-surface-subtle);
  color: var(--dk-text);
  padding: 2px 6px 2px 10px;
  font-size: var(--dk-text-xs);
}

.dk-chat-chip.is-failed {
  border-color: rgba(248, 113, 113, 0.6);
  background: var(--dk-danger-soft);
  color: var(--dk-danger-strong);
}

.dk-chat-chip__name {
  max-width: 180px;
}

.dk-chat-chip__state {
  color: var(--dk-text-tertiary);
  white-space: nowrap;
}

.dk-chat-chip.is-failed .dk-chat-chip__state {
  color: var(--dk-danger);
}

.dk-chat-chip__remove {
  display: inline-flex;
  width: 18px;
  height: 18px;
  align-items: center;
  justify-content: center;
  border: 0;
  border-radius: 50%;
  background: transparent;
  color: inherit;
  font-size: 13px;
  line-height: 1;
  cursor: pointer;
}

.dk-chat-chip__remove:hover {
  background: var(--dk-surface-hover);
}

.dk-chat-textarea {
  min-height: 64px;
  resize: none;
}

.dk-chat-input__actions {
  display: flex;
  align-items: center;
  gap: var(--dk-space-2);
}

.dk-chat-send {
  margin-left: auto;
  min-width: 88px;
}
</style>
