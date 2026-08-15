<!--
  AI 对话：左栏的会话列表。

  只管展示和转发事件，数据和排序都在页面层（DesignkitChatView）：
  列表要按 updated_at 倒序、发完消息要把会话顶到最上面，这些状态只有页面知道。

  删除带确认，但**不开弹窗**：点「删除」后原地换成一行「删除后找不回来。＋确定/取消」。
  上游 BaseDialog 是给花钱的二次确认用的重型件，删一组对话用不着；
  原地确认还顺带避免「弹窗问的是哪一条」的歧义——确认行就长在那一条下面。

  `busy` 为 true（AI 正在回复）时整栏禁用：回复是发到「当前会话」的，
  这时候切走会话，回复就会落进错的对话里。
-->
<template>
  <div class="dk-chat-sessions">
    <button
      type="button"
      class="dk-button dk-button--secondary dk-button--sm dk-button--block"
      :disabled="busy"
      @click="$emit('create')"
    >
      {{ t('designkit.chat.newSession') }}
    </button>

    <p class="dk-chat-sessions__title">{{ t('designkit.chat.sessions') }}</p>

    <p v-if="loading && sessions.length === 0" class="dk-muted">
      {{ t('designkit.common.loading') }}
    </p>
    <p v-else-if="error !== ''" class="dk-note dk-note--danger">{{ error }}</p>
    <p v-else-if="sessions.length === 0" class="dk-note dk-note--quiet">
      {{ t('designkit.chat.emptySessions') }}
    </p>

    <ul v-else class="dk-chat-session-list">
      <li
        v-for="session in sessions"
        :key="session.uid"
        class="dk-chat-session"
        :class="{ 'is-active': session.uid === currentUid }"
      >
        <div class="dk-chat-session__row">
          <button
            type="button"
            class="dk-chat-session__open"
            :disabled="busy"
            @click="$emit('select', session.uid)"
          >
            <span class="dk-chat-session__name dk-truncate">
              {{ session.title !== '' ? session.title : t('designkit.chat.untitled') }}
            </span>
            <span class="dk-chat-session__time">{{ formatTime(session.updated_at) }}</span>
          </button>
          <button
            type="button"
            class="dk-button dk-button--quiet dk-button--sm"
            :disabled="busy"
            :title="t('designkit.chat.deleteSession')"
            @click="confirmUid = session.uid"
          >
            {{ t('designkit.common.delete') }}
          </button>
        </div>

        <!-- 原地确认。删了找不回来，所以「确定」用 danger 样式。 -->
        <div v-if="confirmUid === session.uid" class="dk-chat-session__confirm">
          <span class="dk-note dk-note--danger">{{ t('designkit.chat.deleteConfirm') }}</span>
          <span class="dk-inline-actions">
            <button
              type="button"
              class="dk-button dk-button--danger dk-button--sm"
              :disabled="busy"
              @click="confirmRemove(session.uid)"
            >
              {{ t('designkit.common.delete') }}
            </button>
            <button
              type="button"
              class="dk-button dk-button--quiet dk-button--sm"
              @click="confirmUid = ''"
            >
              {{ t('designkit.common.cancel') }}
            </button>
          </span>
        </div>
      </li>
    </ul>
  </div>
</template>

<script setup lang="ts">
import { ref } from 'vue'
import { useI18n } from 'vue-i18n'
import type { ChatSession } from '../../api'

defineProps<{
  /** 已按 updated_at 倒序排好（页面层排，这里不再排）。 */
  sessions: ChatSession[]
  /** 当前打开的会话；空串 = 新对话（还没发过消息）。 */
  currentUid: string
  /** AI 正在回复：整栏禁用，防止回复落进切走后的会话。 */
  busy: boolean
  loading: boolean
  /** 列表加载失败的文案；空串 = 没出错。 */
  error: string
}>()

const emit = defineEmits<{
  (e: 'select', uid: string): void
  (e: 'create'): void
  (e: 'remove', uid: string): void
}>()

const { t } = useI18n()

/** 正在等确认删除的那一条；空串 = 没有。同一时间只确认一条。 */
const confirmUid = ref('')

function confirmRemove(uid: string): void {
  confirmUid.value = ''
  emit('remove', uid)
}

/**
 * 列表里的时间：当年只显示「月-日 时:分」，跨年才带年份。
 * 完整时间没人看，占位还把标题挤成一个字一行。
 */
function formatTime(iso: string): string {
  const date = new Date(iso)
  if (Number.isNaN(date.getTime())) {
    return ''
  }
  const two = (n: number): string => String(n).padStart(2, '0')
  const base = `${two(date.getMonth() + 1)}-${two(date.getDate())} ${two(date.getHours())}:${two(date.getMinutes())}`
  return date.getFullYear() === new Date().getFullYear()
    ? base
    : `${date.getFullYear()}-${base}`
}
</script>

<style scoped>
.dk-chat-sessions {
  display: grid;
  min-width: 0;
  align-content: start;
  gap: var(--dk-space-3);
}

.dk-chat-sessions__title {
  color: var(--dk-text-tertiary);
  font-size: var(--dk-text-xs);
  font-weight: 650;
}

.dk-chat-session-list {
  display: grid;
  min-width: 0;
  gap: var(--dk-space-1);
  list-style: none;
  margin: 0;
  padding: 0;
}

.dk-chat-session {
  min-width: 0;
  border-radius: var(--dk-radius-md);
}

.dk-chat-session__row {
  display: flex;
  min-width: 0;
  align-items: center;
  gap: var(--dk-space-1);
}

.dk-chat-session__open {
  display: grid;
  min-width: 0;
  flex: 1;
  gap: 2px;
  border: 0;
  border-radius: var(--dk-radius-md);
  background: transparent;
  color: var(--dk-text);
  padding: 7px 9px;
  font-family: inherit;
  text-align: left;
  cursor: pointer;
  transition: background-color var(--dk-motion-fast) var(--dk-ease-out);
}

.dk-chat-session__open:hover:not(:disabled) {
  background: var(--dk-surface-hover);
}

.dk-chat-session__open:disabled {
  color: var(--dk-text-disabled);
  cursor: default;
}

.dk-chat-session.is-active .dk-chat-session__open {
  background: var(--dk-accent-soft);
}

.dk-chat-session.is-active .dk-chat-session__name {
  color: var(--dk-accent-strong);
}

.dk-chat-session__name {
  font-size: var(--dk-text-sm);
  font-weight: 600;
  line-height: 1.4;
}

.dk-chat-session__time {
  color: var(--dk-text-tertiary);
  font-size: 11px;
  line-height: 1.3;
}

.dk-chat-session__confirm {
  display: grid;
  gap: var(--dk-space-2);
  border: 1px solid rgba(248, 113, 113, 0.45);
  border-radius: var(--dk-radius-md);
  background: var(--dk-danger-soft);
  margin-top: var(--dk-space-1);
  padding: var(--dk-space-2);
}
</style>
