/**
 * AI 对话的接口封装。
 *
 * 走上游 axios 单例（三条坑见 `./index.ts` 文件头），路径只写 `/designkit/...`，
 * 实际请求是 `/api/v1/designkit/...`。
 *
 * ⚠ 字段名与后端逐字一致，一律 snake_case，不改驼峰。
 *
 * 端点一共四个，全部只对登录用户开放（浏览器路径，跟 ERP 无关）：
 *
 *   POST   /designkit/chat/messages          发一条消息，等 AI 回复（**慢，见下**；
 *                                            带 "stream": true 时变成 SSE 打字机，
 *                                            见 sendChatMessageStream）
 *   GET    /designkit/chat/sessions          会话列表
 *   GET    /designkit/chat/sessions/:uid     一个会话的全部消息（按 id 升序）
 *   DELETE /designkit/chat/sessions/:uid     删除一个会话
 */

import { apiClient, buildApiUrl } from '@/api/client'
import { getLocale } from '@/i18n'
import { DESIGNKIT_API_BASE_PATH } from './paths'

// ============================================================================
// 常量
// ============================================================================

/** 一条消息的字数上限，跟后端一致。超了后端会 400，前端用 maxlength 提前拦。 */
export const MAX_CHAT_TEXT_LENGTH = 4000

/** 一条消息最多带几张图，跟后端一致。 */
export const MAX_CHAT_ASSETS = 3

/**
 * 对话附图的大小上限（2MB）。
 *
 * ⚠ 比工作台上传商品图的 20MB 严得多——对话的图是发给对话模型「看」的，
 * 不参与出图，压小了不影响效果，传大图白等半天。前端在选文件时就拦。
 */
export const MAX_CHAT_IMAGE_BYTES = 2 * 1024 * 1024

/**
 * 发消息要等多久。
 *
 * 后端收到消息要去问一趟对话模型（带图时还要先读图），实测几十秒，
 * 赶上模型慢的时候更久。axios 单例的默认超时只有 30 秒（`@/api/client`），
 * 照默认值走的结果是**后端还在答、前端已经报超时**——运营看到「没发出去」
 * 又点重发，同一句话问两遍、等两遍。所以这里单独放宽到 120 秒。
 *
 * ⚠ 放宽的只有发消息这一个请求；列表、历史、删除都是普通查询，走默认超时。
 * 也不要去动 axios 单例的全局超时（同 `inspiration.ts` 的 SUGGEST_TIMEOUT_MS）。
 */
const SEND_CHAT_TIMEOUT_MS = 120_000

// ============================================================================
// 类型
// ============================================================================

/** 一条消息（用户发的或 AI 答的）。 */
export interface ChatMessage {
  /** 会话内自增编号。历史记录按它升序返回。 */
  id: number
  role: 'user' | 'assistant' | string
  /** 正文。显示时保留换行（white-space: pre-wrap）。 */
  content: string
  /** 这条消息带的图（商品图素材编号）；没有时是**空数组**，这里已兜底。 */
  asset_uids: string[]
  /** RFC3339 UTC。 */
  created_at: string
}

/** 一个会话（左栏列表里的一行）。 */
export interface ChatSession {
  uid: string
  /** 后端起的标题；可能是空串，界面用「新对话」兜底。 */
  title: string
  created_at: string
  updated_at: string
}

/** `POST /designkit/chat/messages` 的请求体。 */
export interface SendChatMessageInput {
  /** 发到哪个会话；**空串 = 新建会话**（后端建好后在响应里给回编号）。 */
  session_uid?: string
  /** 必填，≤4000 字。 */
  text: string
  /** 随消息带的图，最多 3 张。 */
  asset_uids?: string[]
}

/** `POST /designkit/chat/messages` 的返回。 */
export interface SendChatMessageResult {
  /** 消息落在哪个会话。新建会话时靠它拿到编号。 */
  session_uid: string
  /** 会话的最新标题（新会话由后端起名）。 */
  title: string
  /** 刚发的那条（服务端定稿：带 id 和时间）。 */
  user_message: ChatMessage
  /** AI 的回复。 */
  assistant_message: ChatMessage
}

// ============================================================================
// 归一
// ============================================================================

/** 把服务端的消息兜成模板可直接用的形状：数组不为 null（v-for 遇 null 会整页白屏）。 */
function normalizeMessage(raw: Partial<ChatMessage> | null | undefined): ChatMessage {
  return {
    id: typeof raw?.id === 'number' ? raw.id : 0,
    role: raw?.role ?? 'assistant',
    content: raw?.content ?? '',
    asset_uids: raw?.asset_uids ?? [],
    created_at: raw?.created_at ?? '',
  }
}

// ============================================================================
// 接口
// ============================================================================

/**
 * 发一条消息，等 AI 回复。
 *
 * **这一个会等几十秒**（后端要问一趟对话模型），调用方必须给「AI 回复中…」的
 * 状态并禁掉输入——重复发等于重复问模型，既慢又多花钱。
 *
 * 三个字段**始终显式传**，不做「有值才带上」：少传一个字段，后端拿到的就是
 * 它自己的默认值，而默认值是什么前端说了不算（同 `suggestPrompt` 的理由）。
 */
export async function sendChatMessage(
  input: SendChatMessageInput,
  signal?: AbortSignal,
): Promise<SendChatMessageResult> {
  const body = {
    session_uid: (input.session_uid ?? '').trim(),
    text: input.text,
    asset_uids: input.asset_uids ?? [],
  }
  const { data } = await apiClient.post<SendChatMessageResult>(
    `${DESIGNKIT_API_BASE_PATH}/chat/messages`,
    body,
    { timeout: SEND_CHAT_TIMEOUT_MS, signal },
  )
  return {
    session_uid: data.session_uid ?? '',
    title: data.title ?? '',
    user_message: normalizeMessage(data.user_message),
    assistant_message: normalizeMessage(data.assistant_message),
  }
}

/** `sendChatMessageStream` 的三个回调。 */
export interface SendChatMessageStreamCallbacks {
  /** 每收到一段正文调一次（按顺序）。把它接到气泡上就是打字机。 */
  onDelta: (text: string) => void
  /**
   * 成功收尾：服务端定稿（与非流式响应同构）。调用方用它**替换**打字机攒出来的
   * 内容——两边理论上一样，但以服务端落库的为准。
   */
  onDone: (result: SendChatMessageResult) => void
  /**
   * 失败收尾：err 交给 `toFriendlyError()` 翻译。已收到的 delta 不撤回——
   * 界面把半截回复留着显示（标失败），是「AI 回复中断」该有的样子。
   */
  onError: (err: unknown) => void
}

/**
 * 发一条消息，AI 回复**边生成边**回调（SSE 打字机）。
 *
 * 与 `sendChatMessage` 打的是同一个端点，只是请求体多带 `"stream": true`，
 * 响应变成 text/event-stream 三种帧：
 *
 *   event: delta   data: {"text":"…"}            → onDelta
 *   event: done    data: {与非流式相同的响应 JSON} → onDone（此后流结束）
 *   event: error   data: {标准错误信封}           → onError（此后流结束）
 *
 * 为什么用 fetch 手工解析而不是 EventSource：EventSource 只能发 GET，
 * 而消息体（文字 + 图）必须走 POST。
 *
 * 兼容旧后端（还不认识 stream 字段的部署）：
 *   - 端点整个 404 → 自动改发一次非流式 `sendChatMessage`；
 *   - 200 但回的是 application/json（stream 字段被忽略）→ 直接当非流式结果用。
 * 两种情况 onDelta 一次都不来、onDone 直接到——调用方不需要写第二套处理。
 *
 * 超时 120 秒（同 SEND_CHAT_TIMEOUT_MS 的理由），计的是**整条流**：
 * 到点还没收尾就断开并报超时。外部取消传 `signal`（比如页面卸载）。
 *
 * 三个回调恰好触发一个收尾（onDone 或 onError），绝不都触发、绝不都不触发。
 */
export async function sendChatMessageStream(
  input: SendChatMessageInput,
  callbacks: SendChatMessageStreamCallbacks,
  signal?: AbortSignal,
): Promise<void> {
  const controller = new AbortController()
  let timedOut = false
  const timer = window.setTimeout(() => {
    timedOut = true
    controller.abort()
  }, SEND_CHAT_TIMEOUT_MS)
  const onOuterAbort = () => controller.abort()
  signal?.addEventListener('abort', onOuterAbort)

  let finished = false
  const finishDone = (result: SendChatMessageResult) => {
    if (finished) return
    finished = true
    callbacks.onDone(result)
  }
  const finishError = (err: unknown) => {
    if (finished) return
    finished = true
    // 超时导致的 abort 不能报成「已取消」——运营要知道是等太久了。
    if (timedOut) {
      callbacks.onError({ code: 'ECONNABORTED', message: 'designkit chat stream timeout' })
      return
    }
    callbacks.onError(err)
  }

  try {
    const token = localStorage.getItem('auth_token')
    const response = await fetch(buildApiUrl(`${DESIGNKIT_API_BASE_PATH}/chat/messages`), {
      method: 'POST',
      credentials: 'include',
      signal: controller.signal,
      headers: {
        'Content-Type': 'application/json',
        Accept: 'text/event-stream',
        'Accept-Language': getLocale(),
        ...(token ? { Authorization: `Bearer ${token}` } : {}),
      },
      body: JSON.stringify({
        session_uid: (input.session_uid ?? '').trim(),
        text: input.text,
        asset_uids: input.asset_uids ?? [],
        stream: true,
      }),
    })

    // 旧后端没有这个端点：整个 404 时回落非流式（一次），别让功能随后端版本消失。
    // 401 同样回落：fetch 不经过 axios 的令牌续期拦截器，登录态刚过期时
    // 让非流式那条路去续期重试；真的没登录时它也会给出正确的中文提示。
    if (response.status === 404 || response.status === 401) {
      finishDone(await sendChatMessage(input, signal))
      return
    }

    if (!response.ok) {
      finishError(await readStreamHTTPError(response))
      return
    }

    const contentType = response.headers.get('Content-Type') ?? ''
    if (!contentType.includes('text/event-stream')) {
      // 旧后端忽略了 stream 字段，回的就是非流式 JSON——直接当结果用。
      const data = (await response.json()) as Partial<SendChatMessageResult>
      finishDone(normalizeSendResult(data))
      return
    }

    if (!response.body) {
      finishError({ status: 0 })
      return
    }

    await parseChatSSE(response.body, {
      onDelta: (text) => {
        if (!finished) callbacks.onDelta(text)
      },
      onDone: (raw) => finishDone(normalizeSendResult(raw as Partial<SendChatMessageResult>)),
      onError: (raw) => finishError(raw),
    })
    // 流断了却没等到 done/error 帧：按网络断线报（半截回复由调用方留着显示）。
    finishError({ status: 0 })
  } catch (err) {
    finishError(err)
  } finally {
    window.clearTimeout(timer)
    signal?.removeEventListener('abort', onOuterAbort)
  }
}

/** 把非 2xx 的流式响应体读成 `toFriendlyError()` 认得的形状。 */
async function readStreamHTTPError(response: Response): Promise<unknown> {
  let parsed: unknown = null
  try {
    parsed = await response.json()
  } catch {
    parsed = null
  }
  const body = (parsed && typeof parsed === 'object' ? parsed : {}) as Record<string, unknown>
  const errorBody =
    body.error && typeof body.error === 'object' ? (body.error as Record<string, unknown>) : null
  return {
    status: response.status,
    error: errorBody ?? undefined,
    message: typeof errorBody?.message === 'string' ? errorBody.message : undefined,
  }
}

/** 归一化 done 帧 / 非流式响应，兜掉 null（形状同 `sendChatMessage` 的收尾）。 */
function normalizeSendResult(data: Partial<SendChatMessageResult>): SendChatMessageResult {
  return {
    session_uid: data.session_uid ?? '',
    title: data.title ?? '',
    user_message: normalizeMessage(data.user_message),
    assistant_message: normalizeMessage(data.assistant_message),
  }
}

/**
 * 手工解析 SSE 流。只认后端承诺的三种帧；认不出的行一律跳过
 * （后端以后加新帧型不该把旧前端弄坏）。
 *
 * onDone / onError 收到的是 data 解出来的对象；error 帧的对象就是标准错误信封
 * `{"error":{...}}`，恰好是 `toFriendlyError()` 认得的形状之一。
 */
async function parseChatSSE(
  body: ReadableStream<Uint8Array>,
  handlers: {
    onDelta: (text: string) => void
    onDone: (raw: unknown) => void
    onError: (raw: unknown) => void
  },
): Promise<void> {
  const reader = body.getReader()
  const decoder = new TextDecoder()
  let buffer = ''

  const handleFrame = (frame: string): boolean => {
    let event = ''
    const dataLines: string[] = []
    for (const rawLine of frame.split('\n')) {
      const line = rawLine.endsWith('\r') ? rawLine.slice(0, -1) : rawLine
      if (line.startsWith('event:')) {
        event = line.slice('event:'.length).trim()
      } else if (line.startsWith('data:')) {
        dataLines.push(line.slice('data:'.length).replace(/^ /, ''))
      }
    }
    if (event === '' || dataLines.length === 0) {
      return false
    }
    let parsed: unknown
    try {
      parsed = JSON.parse(dataLines.join('\n'))
    } catch {
      return false
    }
    switch (event) {
      case 'delta': {
        const text = (parsed as { text?: unknown }).text
        if (typeof text === 'string' && text !== '') {
          handlers.onDelta(text)
        }
        return false
      }
      case 'done':
        handlers.onDone(parsed)
        return true
      case 'error':
        handlers.onError(parsed)
        return true
      default:
        return false
    }
  }

  try {
    for (;;) {
      const { done, value } = await reader.read()
      if (value) {
        buffer += decoder.decode(value, { stream: true })
        // 帧以空行分隔。统一把 CRLF 归一成 LF 再切：\r\n\r\n 里没有连续的
        // 两个 \n，不归一的话整条流会攒到结束才一次吐出来。
        buffer = buffer.replace(/\r\n/g, '\n')
        for (;;) {
          const splitAt = buffer.indexOf('\n\n')
          if (splitAt < 0) break
          const frame = buffer.slice(0, splitAt)
          buffer = buffer.slice(splitAt + 2)
          if (handleFrame(frame)) {
            return
          }
        }
      }
      if (done) {
        // 冲掉解码器攒着的半个多字节字符，再处理没带空行的残帧。
        buffer += decoder.decode()
        if (buffer.trim() !== '') {
          handleFrame(buffer)
        }
        return
      }
    }
  } finally {
    reader.releaseLock()
  }
}

/** 会话列表。顺序由界面按 updated_at 倒序排（后端顺序不作数）。 */
export async function listChatSessions(signal?: AbortSignal): Promise<ChatSession[]> {
  const { data } = await apiClient.get<{ sessions?: ChatSession[] }>(
    `${DESIGNKIT_API_BASE_PATH}/chat/sessions`,
    { signal },
  )
  return data.sessions ?? []
}

/** 一个会话的全部消息。**后端按 id 升序返回**，直接按数组顺序渲染，不要再排。 */
export async function getChatSession(
  uid: string,
  signal?: AbortSignal,
): Promise<{ session: ChatSession; messages: ChatMessage[] }> {
  const { data } = await apiClient.get<{ session?: ChatSession; messages?: ChatMessage[] }>(
    `${DESIGNKIT_API_BASE_PATH}/chat/sessions/${encodeURIComponent(uid)}`,
    { signal },
  )
  return {
    session: {
      uid: data.session?.uid ?? uid,
      title: data.session?.title ?? '',
      created_at: data.session?.created_at ?? '',
      updated_at: data.session?.updated_at ?? '',
    },
    messages: (data.messages ?? []).map(normalizeMessage),
  }
}

/** 删除一个会话（连同它的消息）。删了找不回来，界面必须先确认。 */
export async function deleteChatSession(uid: string): Promise<void> {
  await apiClient.delete(`${DESIGNKIT_API_BASE_PATH}/chat/sessions/${encodeURIComponent(uid)}`)
}

// ============================================================================
// 附图地址
// ============================================================================

/**
 * 由素材编号拼出取图地址，形状与 `DesignkitAsset.content_url` 一致
 * （`/api/v1/designkit/assets/<uid>/content`）。
 *
 * 历史消息里只有 `asset_uids` 没有整条素材，所以显示缩略图要自己拼。
 * ⚠ 拼出来的地址**不能直接塞 `<img src>`**（要带登录凭证，`<img>` 不带，
 * 结果是 401 碎图）——原样交给 `useAuthedImages()`，它取成 blob 再显示。
 */
export function chatAssetContentUrl(assetUid: string): string {
  return `/api/v1${DESIGNKIT_API_BASE_PATH}/assets/${encodeURIComponent(assetUid)}/content`
}
