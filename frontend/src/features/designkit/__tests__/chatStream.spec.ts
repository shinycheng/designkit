/**
 * AI 对话 SSE 流式解析的单测（api/chat.ts 的 sendChatMessageStream）。
 *
 * 通过公开入口测（喂一个假的 fetch Response），不导出内部的 parseChatSSE。
 * 守的行为：
 *  - delta 帧按顺序回调，done 帧收尾且归一化（null 数组兜成 []）；
 *  - 帧被网络切成半截（甚至切在一个汉字的字节中间）也能拼回来；
 *  - error 帧走 onError，之后的帧不再处理；
 *  - 流断了没等到收尾帧 → 按断网报错；
 *  - 旧后端（回 JSON / 整个 404）自动回落非流式，onDelta 一次都不来。
 */
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'

const client = vi.hoisted(() => ({
  post: vi.fn(),
}))

vi.mock('@/api/client', () => ({
  apiClient: client,
  buildApiUrl: (path: string) => path,
}))

vi.mock('@/i18n', () => ({
  getLocale: () => 'zh',
}))

import { sendChatMessageStream } from '../api/chat'
import type { SendChatMessageResult } from '../api/chat'

/** 把若干块（字符串或字节）拼成一个 SSE Response。字节块用来模拟半截帧。 */
function sseResponse(chunks: Array<string | Uint8Array>): Response {
  const encoder = new TextEncoder()
  const stream = new ReadableStream<Uint8Array>({
    start(controller) {
      for (const chunk of chunks) {
        controller.enqueue(typeof chunk === 'string' ? encoder.encode(chunk) : chunk)
      }
      controller.close()
    },
  })
  return new Response(stream, {
    status: 200,
    headers: { 'Content-Type': 'text/event-stream' },
  })
}

const doneFrame =
  'event: done\n' +
  'data: {"session_uid":"s1","title":"新对话",' +
  '"user_message":{"id":1,"role":"user","content":"你好","asset_uids":null,"created_at":""},' +
  '"assistant_message":{"id":2,"role":"assistant","content":"你好，请上传商品图","asset_uids":["u1"],"created_at":""}}' +
  '\n\n'

/** 跑一次流式发送，收集三个回调的实况。 */
async function run(response: Response | Promise<Response>) {
  vi.stubGlobal('fetch', vi.fn().mockReturnValue(Promise.resolve(response)))
  const deltas: string[] = []
  let done: SendChatMessageResult | null = null
  let error: unknown = null
  let doneCalls = 0
  let errorCalls = 0
  await sendChatMessageStream(
    { session_uid: '', text: '你好', asset_uids: [] },
    {
      onDelta: (text) => deltas.push(text),
      onDone: (result) => {
        doneCalls += 1
        done = result
      },
      onError: (err) => {
        errorCalls += 1
        error = err
      },
    },
  )
  return { deltas, done: done as SendChatMessageResult | null, error, doneCalls, errorCalls }
}

describe('sendChatMessageStream', () => {
  beforeEach(() => {
    client.post.mockReset()
  })

  afterEach(() => {
    vi.unstubAllGlobals()
  })

  it('delta 按顺序回调，done 收尾且 null 数组被兜成 []', async () => {
    const out = await run(
      sseResponse([
        'event: delta\ndata: {"text":"你"}\n\n',
        'event: delta\ndata: {"text":"好"}\n\n',
        doneFrame,
      ]),
    )
    expect(out.deltas).toEqual(['你', '好'])
    expect(out.doneCalls).toBe(1)
    expect(out.errorCalls).toBe(0)
    expect(out.done!.session_uid).toBe('s1')
    // user_message.asset_uids 是 null，必须兜成 []（v-for 遇 null 会白屏）
    expect(out.done!.user_message.asset_uids).toEqual([])
    expect(out.done!.assistant_message.asset_uids).toEqual(['u1'])
  })

  it('帧切成半截、甚至切在一个汉字的字节中间，也能拼回完整内容', async () => {
    const bytes = new TextEncoder().encode('event: delta\ndata: {"text":"商品图"}\n\n' + doneFrame)
    // 「商」的三个 UTF-8 字节大约从第 28 字节开始；切在 30 保证劈开一个多字节字符
    const cut1 = 30
    const cut2 = 45
    const out = await run(
      sseResponse([bytes.slice(0, cut1), bytes.slice(cut1, cut2), bytes.slice(cut2)]),
    )
    expect(out.deltas).toEqual(['商品图'])
    expect(out.doneCalls).toBe(1)
    expect(out.errorCalls).toBe(0)
  })

  it('CRLF 分隔的帧照样能切开，不会攒到流结束才一次吐出', async () => {
    const out = await run(
      sseResponse([
        'event: delta\r\ndata: {"text":"a"}\r\n\r\nevent: delta\r\ndata: {"text":"b"}\r\n\r\n',
        doneFrame,
      ]),
    )
    expect(out.deltas).toEqual(['a', 'b'])
    expect(out.doneCalls).toBe(1)
  })

  it('error 帧走 onError（错误信封原样带出），done 不再触发', async () => {
    const out = await run(
      sseResponse([
        'event: delta\ndata: {"text":"半截"}\n\n',
        'event: error\ndata: {"error":{"error_code":"DK_UPSTREAM","message":"上游忙"}}\n\n',
        // error 之后的帧必须被忽略
        doneFrame,
      ]),
    )
    expect(out.deltas).toEqual(['半截'])
    expect(out.errorCalls).toBe(1)
    expect(out.doneCalls).toBe(0)
    expect((out.error as { error?: { error_code?: string } }).error?.error_code).toBe('DK_UPSTREAM')
  })

  it('流断了却没等到 done/error 帧：按断网报错（status 0），delta 保留不撤回', async () => {
    const out = await run(sseResponse(['event: delta\ndata: {"text":"半截回复"}\n\n']))
    expect(out.deltas).toEqual(['半截回复'])
    expect(out.doneCalls).toBe(0)
    expect(out.errorCalls).toBe(1)
    expect((out.error as { status?: number }).status).toBe(0)
  })

  it('旧后端忽略 stream 字段回 JSON：直接当非流式结果收尾，onDelta 一次不来', async () => {
    const body = {
      session_uid: 's9',
      title: 't',
      user_message: { id: 1, role: 'user', content: 'hi', asset_uids: null, created_at: '' },
      assistant_message: { id: 2, role: 'assistant', content: 'ok', asset_uids: [], created_at: '' },
    }
    const response = new Response(JSON.stringify(body), {
      status: 200,
      headers: { 'Content-Type': 'application/json' },
    })
    const out = await run(response)
    expect(out.deltas).toEqual([])
    expect(out.doneCalls).toBe(1)
    expect(out.done!.session_uid).toBe('s9')
    expect(out.done!.user_message.asset_uids).toEqual([])
  })

  it('端点整个 404：自动回落非流式 sendChatMessage，一样只收尾一次', async () => {
    client.post.mockResolvedValue({
      data: {
        session_uid: 's2',
        title: 't',
        user_message: { id: 1, role: 'user', content: '你好', asset_uids: [], created_at: '' },
        assistant_message: { id: 2, role: 'assistant', content: '回复', asset_uids: [], created_at: '' },
      },
    })
    const out = await run(new Response('not found', { status: 404 }))
    expect(client.post).toHaveBeenCalledTimes(1)
    // 回落时发的还是同一份消息体
    expect(client.post.mock.calls[0][1]).toMatchObject({ text: '你好' })
    expect(out.deltas).toEqual([])
    expect(out.doneCalls).toBe(1)
    expect(out.errorCalls).toBe(0)
    expect(out.done!.session_uid).toBe('s2')
  })
})
