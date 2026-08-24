<!--
  AI 对话：消息流。

  - user 右对齐、主紫底（--dk-action）；assistant 左对齐、面板底（--dk-surface-subtle）。
  - 正文 white-space: pre-wrap：AI 的回复常带分段和列表，压成一行没法读。
  - 带图消息在气泡里显示缩略图。⚠ 图片地址要带登录凭证，**必须走 useAuthedImages()**
    取成 blob 再显示，直接 <img src> 是 401 碎图。历史消息里只有 asset_uids，
    地址由 chatAssetContentUrl() 拼出（形状与 DesignkitAsset.content_url 一致）。
  - `replying` 为 true 时在 assistant 位置显示「AI 回复中…」占位——
    后端要问一趟对话模型，没有占位就是「点了没反应」。流式回复开始长字后
    （`streaming`）占位收起来，位置让给打字机气泡。
  - 发送失败的那条标红，下面给「重发」（emit 上去由页面层重发，
    组件自己不发请求）；流式中断留下的半截 assistant 回复也标红，
    但只挂「回复中断」说明，不给按钮——重发在对应的用户消息上。

  滚动容器就是本组件根节点；尺寸（flex:1 / min-height）由页面层通过 class 给。
-->
<template>
  <div ref="scroller" class="dk-chat-msgs">
    <p v-if="loading" class="dk-muted">{{ t('designkit.common.loading') }}</p>

    <p v-else-if="loadError !== ''" class="dk-note dk-note--danger">{{ loadError }}</p>

    <!-- 空态：居中一句。 -->
    <div v-else-if="messages.length === 0 && !replying" class="dk-chat-empty">
      <p>{{ t('designkit.chat.empty') }}</p>
    </div>

    <template v-else>
      <div
        v-for="msg in messages"
        :key="msg.key"
        class="dk-chat-row"
        :class="msg.role === 'user' ? 'is-user' : 'is-assistant'"
      >
        <div class="dk-chat-bubble" :class="{ 'is-failed': msg.failed || msg.interrupted }">
          <div v-if="msg.asset_uids.length > 0" class="dk-chat-thumbs">
            <template v-for="uid in msg.asset_uids" :key="uid">
              <img
                v-if="images.urls.value[urlOf(uid)]"
                :src="images.urls.value[urlOf(uid)]"
                alt=""
              />
              <!-- 取失败：点一下重取。 -->
              <button
                v-else-if="images.failed.value[urlOf(uid)]"
                type="button"
                class="dk-chat-thumb-fallback"
                @click="images.retry(urlOf(uid))"
              >
                {{ t('designkit.common.retry') }}
              </button>
              <span v-else class="dk-chat-thumb-fallback">
                {{ t('designkit.common.loading') }}
              </span>
            </template>
          </div>
          <p v-if="msg.content !== ''" class="dk-chat-text">{{ msg.content }}</p>
        </div>

        <p v-if="msg.failed" class="dk-chat-failed">
          <span>{{ t('designkit.chat.failed') }}</span>
          <button
            type="button"
            class="dk-button dk-button--danger dk-button--sm"
            :disabled="replying"
            @click="$emit('resend', msg.key)"
          >
            {{ t('designkit.chat.resend') }}
          </button>
        </p>
        <!-- 流式中断留下的半截回复：只说明，不给按钮（重发在用户消息那条上）。 -->
        <p v-else-if="msg.interrupted" class="dk-chat-failed">
          <span>{{ t('designkit.chat.interrupted') }}</span>
        </p>
      </div>

      <!-- AI 回复中的占位，永远排在最后、站 assistant 的位置。
           打字机开始长字之后（streaming）就收起来，位置让给真气泡。 -->
      <div v-if="replying && !streaming" class="dk-chat-row is-assistant">
        <div class="dk-chat-bubble dk-chat-bubble--pending">
          {{ t('designkit.chat.replying') }}
        </div>
      </div>
    </template>
  </div>
</template>

<script setup lang="ts">
import { nextTick, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import { chatAssetContentUrl } from '../../api'
import { useAuthedImages } from '../../composables/useAuthedImage'
import type { ChatMessageView } from './viewTypes'

const props = defineProps<{
  messages: ChatMessageView[]
  /** AI 正在回复（显示占位、禁掉「重发」）。 */
  replying: boolean
  /**
   * 流式回复已经开始往气泡里长字。此时「AI 回复中…」占位收起来——
   * 打字机本身就是「正在回复」的证据，两个同时出现像卡了两条消息。
   */
  streaming?: boolean
  /** 正在加载历史。 */
  loading: boolean
  /** 历史加载失败的文案；空串 = 没出错。 */
  loadError: string
}>()

defineEmits<{
  (e: 'resend', key: string): void
}>()

const { t } = useI18n()

/** 取图、缓存、限流、回收都在它里面（组件卸载自动回收）。 */
const images = useAuthedImages()

const scroller = ref<HTMLElement | null>(null)

function urlOf(assetUid: string): string {
  return chatAssetContentUrl(assetUid)
}

// 消息集合一变就把要显示的图都排进队列。⚠ 不在模板里调用会触发加载的函数
// （每次重渲染都会再发请求），统一在 watch 里 ensure（useAuthedImage 文件头的约定）。
// watch 的键是「全部图片地址拼一起」：push 进同一个数组不会换引用，
// 只看 props.messages 本身会漏掉新消息。
watch(
  () => props.messages.flatMap((msg) => msg.asset_uids).join('\n'),
  () => {
    images.ensure(props.messages.flatMap((msg) => msg.asset_uids.map(urlOf)))
  },
  { immediate: true },
)

// 新消息、占位出现、**最后一条在流式长长**时滚到底。
// 只看条数的话，打字机每来一段字都会把内容顶出视口，运营得一直手动拖。
watch(
  [
    () => props.messages.length,
    () => props.replying,
    () => (props.messages.length > 0 ? props.messages[props.messages.length - 1].content.length : 0),
  ],
  () => {
    void nextTick(() => {
      const el = scroller.value
      if (el) {
        el.scrollTop = el.scrollHeight
      }
    })
  },
  { immediate: true },
)
</script>

<style scoped>
.dk-chat-msgs {
  display: flex;
  min-width: 0;
  flex-direction: column;
  gap: var(--dk-space-3);
  overflow-y: auto;
  overscroll-behavior: contain;
  padding: var(--dk-space-1);
}

.dk-chat-empty {
  display: flex;
  flex: 1;
  align-items: center;
  justify-content: center;
  margin: auto;
  color: var(--dk-text-secondary);
  font-size: var(--dk-text-md);
  text-align: center;
}

.dk-chat-row {
  display: flex;
  min-width: 0;
  flex-direction: column;
  align-items: flex-start;
  gap: var(--dk-space-1);
}

.dk-chat-row.is-user {
  align-items: flex-end;
}

.dk-chat-bubble {
  max-width: min(680px, 86%);
  border-radius: var(--dk-radius-lg);
  padding: 9px 13px;
  font-size: var(--dk-text-md);
  line-height: 1.6;
}

.is-assistant .dk-chat-bubble {
  border: 1px solid var(--dk-border);
  border-bottom-left-radius: var(--dk-radius-sm);
  background: var(--dk-surface-subtle);
  color: var(--dk-text);
}

.is-user .dk-chat-bubble {
  border-bottom-right-radius: var(--dk-radius-sm);
  background: var(--dk-action);
  color: var(--dk-text-inverse);
}

/* 没发出去的那条：标红。用户气泡本来是紫底，整个换成红调才看得出不一样。 */
.dk-chat-bubble.is-failed {
  border: 1px solid rgba(248, 113, 113, 0.6);
  background: var(--dk-danger-soft);
  color: var(--dk-danger-strong);
}

.dk-chat-bubble--pending {
  color: var(--dk-text-secondary);
}

.dk-chat-text {
  margin: 0;
  overflow-wrap: anywhere;
  white-space: pre-wrap;
}

.dk-chat-thumbs {
  display: flex;
  flex-wrap: wrap;
  gap: var(--dk-space-1);
}

.dk-chat-thumbs + .dk-chat-text {
  margin-top: var(--dk-space-2);
}

.dk-chat-thumbs img {
  display: block;
  width: 96px;
  height: 96px;
  border-radius: var(--dk-radius-sm);
  background: var(--dk-image-canvas);
  object-fit: cover;
}

.dk-chat-thumb-fallback {
  display: inline-flex;
  width: 96px;
  height: 96px;
  align-items: center;
  justify-content: center;
  border: 1px dashed var(--dk-border-strong);
  border-radius: var(--dk-radius-sm);
  background: var(--dk-image-canvas);
  color: var(--dk-text-tertiary);
  padding: var(--dk-space-1);
  font-family: inherit;
  font-size: 11px;
  line-height: 1.4;
  text-align: center;
}

button.dk-chat-thumb-fallback {
  cursor: pointer;
}

.dk-chat-failed {
  display: flex;
  align-items: center;
  gap: var(--dk-space-2);
  margin: 0;
  color: var(--dk-danger);
  font-size: var(--dk-text-xs);
}
</style>
