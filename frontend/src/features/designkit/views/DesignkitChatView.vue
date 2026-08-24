<!--
  AI 对话：跟对话模型聊商品图的事，可以随消息带图（最多 3 张）。

  版式：**左会话列表 260px + 右消息流/输入条**，外壳用上游 AppLayout
  （跟 /workbench、/inspiration 一致）。窄于 1100px 时会话列表收成
  从底部拉起的抽屉——跟工作台配置区抽屉同一套手法（fixed + translate +
  scrim，z-index 必须小于 50，理由见 designkit-ui.css 那段注释：
  上游 BaseDialog 的遮罩是 z-50，压过它会点不到弹窗）。

  几件只能在这一层做的事：

  1. **发送中锁整页**：AI 回复要几十秒，这期间输入禁用、会话列表禁用——
     回复是发到「当前会话」的，切走会话它就落进错的对话里。
  2. **失败的消息留在流里**，标红 + 「重发」。重发用的就是那条消息本身
     （文字 + asset_uids 都在上面），不需要运营重打一遍。
  3. **新会话的编号以响应为准**：session_uid 传空串 = 新建，后端在响应里
     给回编号，从那一刻起这个页面就「在」那个会话里了。
  4. 发完消息把会话顶到列表最上面（updated_at 就地改，排序是 computed）。
  5. **发送默认走流式**（sendChatMessageStream）：assistant 气泡边收边长
     （打字机），done 后用服务端定稿替换；中途出错时已收的文字**保留展示**
     （标红 + 「回复中断」），重发挂在那条用户消息上，重发前把半截移掉。
     旧后端不支持流式时 api 层自动回落非流式，这一层不感知。
-->
<template>
  <AppLayout>
    <div class="dk-chat-page" :class="{ 'is-list-open': listOpen }">
      <header class="dk-canvas-header">
        <div class="dk-page-head">
          <p class="dk-eyebrow">{{ t('designkit.brand') }}</p>
          <h1 class="dk-page-title">{{ t('designkit.chat.title') }}</h1>
        </div>
        <!-- 窄屏才显示：打开会话列表抽屉。 -->
        <button
          type="button"
          class="dk-button dk-button--secondary dk-button--sm dk-chat-toggle"
          @click="listOpen = true"
        >
          {{ t('designkit.chat.sessions') }}
        </button>
      </header>

      <div class="dk-chat-layout">
        <aside class="dk-chat-side">
          <ChatSessionList
            :sessions="sortedSessions"
            :current-uid="currentUid"
            :busy="replying"
            :loading="sessionsLoading"
            :error="sessionsError"
            @select="selectSession"
            @create="createSession"
            @remove="removeSession"
          />
        </aside>

        <!-- 抽屉拉起时点空白处收起来（窄屏专用，宽屏 display:none）。 -->
        <button
          type="button"
          class="dk-chat-scrim"
          :aria-label="t('designkit.common.close')"
          @click="listOpen = false"
        ></button>

        <section class="dk-chat-main">
          <ChatMessageList
            class="dk-chat-stream"
            :messages="messages"
            :replying="replying"
            :streaming="streamKey !== null"
            :loading="historyLoading"
            :load-error="historyError"
            @resend="resend"
          />
          <ChatInput :disabled="replying || historyLoading" @send="handleSend" />
        </section>
      </div>
    </div>
  </AppLayout>
</template>

<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted, ref } from 'vue'
import { useI18n } from 'vue-i18n'
import AppLayout from '@/components/layout/AppLayout.vue'
import { useAppStore } from '@/stores'
import {
  deleteChatSession,
  getChatSession,
  isCanceledError,
  listChatSessions,
  sendChatMessageStream,
  toFriendlyError,
} from '../api'
import type { ChatMessage, ChatSession } from '../api'
import ChatSessionList from '../components/chat/ChatSessionList.vue'
import ChatMessageList from '../components/chat/ChatMessageList.vue'
import ChatInput from '../components/chat/ChatInput.vue'
import type { ChatMessageView } from '../components/chat/viewTypes'
// 页面里用到的通用样式（按钮、页头、备注……），全局引入，Vite 会去重。
import '../components/designkit-ui.css'

const { t } = useI18n()
const appStore = useAppStore()

// ---- 版式 ----
/** 窄屏时会话列表抽屉开没开。宽屏时 CSS 不读它。 */
const listOpen = ref(false)

// ---- 会话列表 ----
const sessions = ref<ChatSession[]>([])
const sessionsLoading = ref(false)
const sessionsError = ref('')

// ---- 当前会话 ----
/** 空串 = 新对话（第一条消息发出去后端才建会话）。 */
const currentUid = ref('')
const messages = ref<ChatMessageView[]>([])
const historyLoading = ref(false)
const historyError = ref('')

/** AI 正在回复：输入禁用、会话列表禁用。 */
const replying = ref(false)

/**
 * 正在流式接收的那条 assistant 消息的本地 key；null = 还没收到第一段。
 * 它是「打字机占位切换」的开关：第一段到达前消息流里显示「AI 回复中…」，
 * 到达后换成边收边长的气泡。
 */
const streamKey = ref<string | null>(null)

let sessionsController: AbortController | null = null
let historyController: AbortController | null = null
let deliverController: AbortController | null = null
let keyCounter = 0

function newLocalKey(): string {
  keyCounter += 1
  return `msg-${Date.now()}-${keyCounter}`
}

/** 列表按最近说过话的排前面。就地改 updated_at 即可让它自动上浮。 */
const sortedSessions = computed(() =>
  [...sessions.value].sort((a, b) => (a.updated_at < b.updated_at ? 1 : -1)),
)

function toView(msg: ChatMessage, key: string): ChatMessageView {
  return {
    key,
    id: typeof msg.id === 'number' && msg.id > 0 ? msg.id : null,
    role: msg.role === 'user' ? 'user' : 'assistant',
    content: msg.content,
    asset_uids: msg.asset_uids,
    created_at: msg.created_at,
    failed: false,
  }
}

// ---------------------------------------------------------------------------
// 会话列表
// ---------------------------------------------------------------------------

async function loadSessions(): Promise<void> {
  sessionsController?.abort()
  sessionsController = new AbortController()
  const signal = sessionsController.signal
  sessionsLoading.value = true
  sessionsError.value = ''
  try {
    sessions.value = await listChatSessions(signal)
  } catch (err) {
    if (isCanceledError(err)) {
      return
    }
    sessionsError.value = t('designkit.chat.loadFailed')
  } finally {
    if (!signal.aborted) {
      sessionsLoading.value = false
    }
  }
}

/** 发完消息后把会话信息落到列表里（没有就插一条新的）。 */
function touchSession(uid: string, title: string): void {
  const now = new Date().toISOString()
  const existing = sessions.value.find((one) => one.uid === uid)
  if (existing) {
    existing.title = title
    existing.updated_at = now
  } else {
    sessions.value = [{ uid, title, created_at: now, updated_at: now }, ...sessions.value]
  }
}

function selectSession(uid: string): void {
  // replying 期间列表本身已禁用，这里再挡一次（键盘焦点还在按钮上时回车也会触发）。
  if (replying.value) {
    return
  }
  listOpen.value = false
  if (uid === currentUid.value) {
    return
  }
  currentUid.value = uid
  void loadSession(uid)
}

function createSession(): void {
  if (replying.value) {
    return
  }
  historyController?.abort()
  listOpen.value = false
  currentUid.value = ''
  messages.value = []
  historyLoading.value = false
  historyError.value = ''
}

async function removeSession(uid: string): Promise<void> {
  try {
    await deleteChatSession(uid)
    sessions.value = sessions.value.filter((one) => one.uid !== uid)
    // 删的是正开着的：回到「新对话」，不能留着一屏幕已经不存在的消息。
    if (currentUid.value === uid) {
      createSession()
    }
  } catch (err) {
    appStore.showError(toFriendlyError(err).message)
  }
}

// ---------------------------------------------------------------------------
// 历史
// ---------------------------------------------------------------------------

async function loadSession(uid: string): Promise<void> {
  historyController?.abort()
  historyController = new AbortController()
  const signal = historyController.signal
  historyLoading.value = true
  historyError.value = ''
  messages.value = []
  try {
    const data = await getChatSession(uid, signal)
    if (signal.aborted) {
      return
    }
    // 后端已按 id 升序，照原顺序进流。
    messages.value = data.messages.map((msg) => toView(msg, newLocalKey()))
  } catch (err) {
    if (isCanceledError(err)) {
      return
    }
    historyError.value = t('designkit.chat.loadFailed')
  } finally {
    if (!signal.aborted) {
      historyLoading.value = false
    }
  }
}

// ---------------------------------------------------------------------------
// 发消息
// ---------------------------------------------------------------------------

function handleSend(payload: { text: string; assetUids: string[] }): void {
  if (replying.value) {
    return
  }
  // 先把这条摆进流里（乐观显示），发送结果回来再替换成服务端定稿。
  const view: ChatMessageView = {
    key: newLocalKey(),
    id: null,
    role: 'user',
    content: payload.text,
    asset_uids: payload.assetUids,
    created_at: new Date().toISOString(),
    failed: false,
  }
  messages.value = [...messages.value, view]
  void deliver(view.key)
}

/** 失败那条上的「重发」。消息内容都在那条上，原样再发一次。 */
function resend(key: string): void {
  if (replying.value) {
    return
  }
  const target = messages.value.find((one) => one.key === key)
  // 只有用户消息能重发：中断的半截 assistant 也标红，但它上面没有重发按钮，
  // 这里再守一道，免得哪天模板改了把半截当消息发出去。
  if (!target || !target.failed || target.role !== 'user') {
    return
  }
  // 上次中断留下的半截回复在重发前移掉，免得新旧两截回答并排。
  messages.value = messages.value.filter((one) => !one.interrupted)
  target.failed = false
  void deliver(key)
}

/**
 * 真正发出去（默认流式，打字机）。按 key 从流里取（拿到的是响应式代理，
 * 直接改属性才会刷新界面；存局部变量改原对象是不会刷新的）。
 *
 * replying 期间会话列表整栏禁用，所以在途中 currentUid 不会变，
 * 响应落回来一定还是这个会话。
 *
 * 三个回调的分工：
 *   - onDelta：第一段到达时把 assistant 气泡摆进流里，之后逐段接长；
 *   - onDone：用服务端定稿替换打字机内容（key 不变，v-for 不闪）——
 *     以落库的为准，也顺带接过新会话的编号；
 *   - onError：**已收到的文字保留**（标「回复中断」），用户消息标失败给重发。
 *     后端流完才落库，中断的这半截在服务端不存在，重发就是干净的一问。
 */
async function deliver(key: string): Promise<void> {
  const view = messages.value.find((one) => one.key === key)
  if (!view) {
    return
  }
  replying.value = true
  streamKey.value = null
  deliverController?.abort()
  deliverController = new AbortController()
  try {
    await sendChatMessageStream(
      {
        session_uid: currentUid.value,
        text: view.content,
        asset_uids: view.asset_uids,
      },
      {
        onDelta(piece) {
          const sk = streamKey.value
          if (sk === null) {
            const assistant: ChatMessageView = {
              key: newLocalKey(),
              id: null,
              role: 'assistant',
              content: piece,
              asset_uids: [],
              created_at: new Date().toISOString(),
              failed: false,
            }
            streamKey.value = assistant.key
            messages.value = [...messages.value, assistant]
          } else {
            const target = messages.value.find((one) => one.key === sk)
            if (target) {
              target.content += piece
            }
          }
        },
        onDone(result) {
          // 新对话：从响应里接过后端建好的会话编号。
          if (currentUid.value === '') {
            currentUid.value = result.session_uid
          }
          // 用服务端定稿替换乐观显示的那条（key 不变，v-for 不闪）。
          const index = messages.value.findIndex((one) => one.key === key)
          if (index >= 0) {
            messages.value.splice(index, 1, toView(result.user_message, key))
          }
          const sk = streamKey.value
          const streamIndex = sk === null ? -1 : messages.value.findIndex((one) => one.key === sk)
          if (streamIndex >= 0 && sk !== null) {
            // 打字机攒的内容换成服务端定稿（理论上一样，以落库的为准）。
            messages.value.splice(streamIndex, 1, toView(result.assistant_message, sk))
          } else {
            // 没收到过 delta（api 层回落非流式时就是这样）：直接摆进来。
            messages.value = [...messages.value, toView(result.assistant_message, newLocalKey())]
          }
          touchSession(result.session_uid, result.title)
        },
        onError(err) {
          // 页面卸载等主动取消：组件都不在了，什么都不该弹。
          if (isCanceledError(err)) {
            return
          }
          const target = messages.value.find((one) => one.key === key)
          if (target) {
            target.failed = true
          }
          // 已收到的文字保留展示，标「回复中断」；重发按钮在用户消息那条上。
          const sk = streamKey.value
          if (sk !== null) {
            const partial = messages.value.find((one) => one.key === sk)
            if (partial) {
              partial.interrupted = true
            }
          }
          // 后端的中文原话（超长、图没了这类都写着原因），失败行上只有「重发」两个字。
          appStore.showError(toFriendlyError(err).message)
        },
      },
      deliverController.signal,
    )
  } finally {
    replying.value = false
    streamKey.value = null
  }
}

onMounted(() => {
  void loadSessions()
})

onBeforeUnmount(() => {
  sessionsController?.abort()
  historyController?.abort()
  deliverController?.abort()
})
</script>

<style scoped>
/*
 * 聊天页要「占满一屏、消息区自己滚」，跟其它页面的「正文多长页面多长」不同。
 * 高度算法照抄 designkit-ui.css 里配置区 sticky 的那条（100dvh − 顶栏 − 余量）。
 */
.dk-chat-page {
  display: flex;
  min-width: 0;
  flex-direction: column;
  gap: var(--dk-space-4);
  height: calc(100dvh - var(--dk-topbar-h) - var(--dk-space-12));
  min-height: 420px;
  /* 窄屏时抽屉离底部多远：外壳在 ≤959px 有一条底部导航，要让开。 */
  --dk-chat-bottom: 0px;
}

.dk-chat-layout {
  display: grid;
  min-width: 0;
  flex: 1;
  min-height: 0;
  grid-template-columns: minmax(0, 1fr);
  gap: var(--dk-space-4);
}

.dk-chat-side {
  min-width: 0;
  border: 1px solid var(--dk-border);
  border-radius: var(--dk-radius-lg);
  background: var(--dk-surface);
  padding: var(--dk-space-3);
  overflow-y: auto;
  overscroll-behavior: contain;
}

.dk-chat-main {
  display: flex;
  min-width: 0;
  min-height: 0;
  flex-direction: column;
  gap: var(--dk-space-3);
  border: 1px solid var(--dk-border);
  border-radius: var(--dk-radius-lg);
  background: var(--dk-surface);
  padding: var(--dk-space-4);
}

/* 消息流占满剩余高度并自己滚（组件根节点已是滚动容器）。 */
.dk-chat-stream {
  flex: 1;
  min-height: 0;
}

.dk-chat-scrim {
  display: none;
}

/* ── 宽屏：左列表 260px 贴住，抽屉开关不存在 ─────────────── */
@media (min-width: 1100px) {
  .dk-chat-layout {
    grid-template-columns: 260px minmax(0, 1fr);
  }

  .dk-chat-toggle {
    display: none;
  }
}

/*
 * ── 窄屏：会话列表收成从底部拉起的抽屉 ──────────────────────
 * 跟工作台配置区抽屉同一套手法。
 * ⚠ 层级必须小于 50：上游 BaseDialog 的遮罩是 z-50（designkit-ui.css 有整段注释）。
 */
@media (max-width: 1099px) {
  .dk-chat-side {
    position: fixed;
    z-index: 40;
    right: 0;
    bottom: var(--dk-chat-bottom);
    left: 0;
    max-height: min(70dvh, 560px);
    border-top: 1px solid var(--dk-border);
    border-radius: var(--dk-radius-lg) var(--dk-radius-lg) 0 0;
    background: var(--dk-canvas);
    box-shadow: var(--dk-shadow-overlay);
    padding: var(--dk-space-4);
    translate: 0 100%;
    visibility: hidden;
    transition:
      translate var(--dk-motion-normal) var(--dk-ease-out),
      visibility var(--dk-motion-normal) var(--dk-ease-out);
  }

  .dk-chat-page.is-list-open .dk-chat-side {
    translate: 0 0;
    visibility: visible;
  }

  .dk-chat-page.is-list-open .dk-chat-scrim {
    position: fixed;
    z-index: 38;
    inset: 0;
    display: block;
    border: 0;
    background: var(--dk-overlay);
    padding: 0;
    cursor: pointer;
  }
}

@media (max-width: 959px) {
  .dk-chat-page {
    --dk-chat-bottom: calc(var(--dk-bottom-nav-h) + var(--dk-safe-bottom));
    /* 底部导航占掉一条，可用高度跟着减，否则输入条被压在导航下面。 */
    height: calc(
      100dvh - var(--dk-topbar-h) - var(--dk-bottom-nav-h) - var(--dk-safe-bottom) -
        var(--dk-space-10)
    );
  }
}

@media (max-width: 639px) {
  .dk-chat-main {
    border-radius: var(--dk-radius-md);
    padding: var(--dk-space-3);
  }
}
</style>
