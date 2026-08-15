/**
 * 「用它生成」：把灵感库挑中的提示词交给生成工作台。
 *
 * ── 为什么用 pinia 而不是路由 query ────────────────────────────────
 *
 * 提示词正文动辄几百上千字（上游那批最长的超过 3000 字），塞进网址会出三个问题：
 *
 *   1. **会被截断。** 浏览器本身能扛很长的地址，但中间的反向代理（群晖自带的
 *      那层、Nginx 默认 8KB 请求行）不一定扛得住；一旦被截，运营带过去的是
 *      半句提示词，而他看不出来——出的图不对，钱已经花了。
 *   2. **中文要 URL 编码。** 一个汉字变 9 个字符，一条 800 字的中文提示词
 *      编码完就是 7000 多字符，上面那条更容易踩到。
 *   3. **网址会被记进访问日志、也会被运营复制给别人**，一条长得看不懂的地址
 *      对非技术使用者是纯粹的噪音。
 *
 * pinia 没有这些问题：内存里传，多长都行，网址还是干干净净的 `/workbench`。
 *
 * ── 为什么还要写一份到 sessionStorage ──────────────────────────────
 *
 * 从灵感库跳工作台是前端路由切换，store 本来就活着。但**整页刷新**
 * （运营手滑按 F5、或者从收藏夹直接打开工作台）会把内存清空。
 * 写一份到 sessionStorage，刷新后还能接上；标签页一关就没了，不会留到下次。
 *
 * ⚠ 只放提示词文本，**不放任何跟钱有关的东西**。工作台拿到之后仍然要
 * 重新报价、重新走确认弹窗——这里传的只是「要写什么」，不是「要花多少」。
 */

import { computed, ref } from 'vue'
import { defineStore } from 'pinia'

/** 从灵感库带过去的一条提示词。 */
export interface HandoffPrompt {
  /** 灵感库里的编号；运营自己写的没有，是 null。留着方便以后做「来源」标注。 */
  uid: string | null
  /** 标题，可能是空串。工作台上用它做一行小标签，让运营认得出是哪一条。 */
  title: string
  /** 提示词正文（占位符已经按运营填的值替换过）。**这才是真正拿去出图的内容。** */
  text: string
}

const STORAGE_KEY = 'designkit.promptHandoff'

/** 最多存几条。运营连点几十下也不至于把工作台撑爆。 */
const MAX_PENDING = 50

/** 从 sessionStorage 读回来。任何异常都当成「没有」，绝不让它把页面搞崩。 */
function readSession(): HandoffPrompt[] {
  try {
    const raw = window.sessionStorage.getItem(STORAGE_KEY)
    if (!raw) {
      return []
    }
    const parsed: unknown = JSON.parse(raw)
    if (!Array.isArray(parsed)) {
      return []
    }
    return parsed
      .filter((one): one is HandoffPrompt => {
        return !!one && typeof one === 'object' && typeof (one as HandoffPrompt).text === 'string'
      })
      .map((one) => ({
        uid: typeof one.uid === 'string' ? one.uid : null,
        title: typeof one.title === 'string' ? one.title : '',
        text: one.text,
      }))
      .slice(0, MAX_PENDING)
  } catch {
    return []
  }
}

/** 写回 sessionStorage。写不进去（隐私模式、配额满）就算了，内存里那份照样能用。 */
function writeSession(list: HandoffPrompt[]): void {
  try {
    if (list.length === 0) {
      window.sessionStorage.removeItem(STORAGE_KEY)
      return
    }
    window.sessionStorage.setItem(STORAGE_KEY, JSON.stringify(list))
  } catch {
    // 忽略：这只是刷新页面时的兜底，丢了不影响这一次跳转。
  }
}

export const usePromptHandoffStore = defineStore('designkitPromptHandoff', () => {
  /** 已经点了「用它生成」、但工作台还没接走的那些。 */
  const pending = ref<HandoffPrompt[]>(readSession())

  const count = computed(() => pending.value.length)

  /**
   * 灵感库那边调用：把一条提示词排进去，然后跳 `/workbench`。
   *
   * 允许重复：运营故意挑两条一样的也照排（顺序就是出图顺序，那是他的钱）。
   */
  function send(one: HandoffPrompt): void {
    const text = (one.text ?? '').trim()
    if (text === '') {
      return
    }
    const next = [...pending.value, { uid: one.uid ?? null, title: one.title ?? '', text }]
    pending.value = next.slice(-MAX_PENDING)
    writeSession(pending.value)
  }

  /**
   * 工作台那边调用：**取走并清空**。
   *
   * 取走之后就归工作台管了，这里不再留副本——否则运营在工作台删掉一条、
   * 一刷新它又冒出来，怎么删都删不掉。
   */
  function takeAll(): HandoffPrompt[] {
    const taken = pending.value
    pending.value = []
    writeSession(pending.value)
    return taken
  }

  /** 丢掉还没被接走的（一般用不到，留给「取消」类操作）。 */
  function clear(): void {
    pending.value = []
    writeSession(pending.value)
  }

  return { pending, count, send, takeAll, clear }
})

/**
 * 「用这张继续生成」的交接键（sessionStorage）。
 *
 * 只放**商品图编号**，不放图片本身——工作台拿到编号自己去查。
 * 跟提示词那份分开一个键：两者可以同时发生（从灵感库带了词、又从我的图片
 * 带了图），揉进一个键会互相覆盖。
 */
export const CONTINUE_ASSET_KEY = 'designkit_continue_asset_uid'
