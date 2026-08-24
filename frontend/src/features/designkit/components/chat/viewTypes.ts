/**
 * AI 对话页面内部的视图类型（页面与 chat/ 子组件共用）。
 *
 * 跟 `api/chat.ts` 的 `ChatMessage` 分开：视图层多两样东西——
 *   - `key`：本地唯一键。刚发出去、后端还没给 id 的消息也要能被 v-for 稳定追踪，
 *     失败重发时也靠它找回那一条；
 *   - `failed`：这条没发出去（该标红、给「重发」按钮）。
 */
export interface ChatMessageView {
  /** 本地唯一键，v-for 和重发都用它。**不是**服务端 id。 */
  key: string
  /** 服务端编号；刚发出去还没落库时是 null。 */
  id: number | null
  role: 'user' | 'assistant'
  content: string
  asset_uids: string[]
  created_at: string
  /** true = 没发出去，气泡标红并显示「重发」。 */
  failed: boolean
  /**
   * true = 这条 assistant 回复是流式中断留下的半截（已收到的文字保留展示，
   * 气泡标红 + 一行「回复中断」说明，但**没有**重发按钮——重发挂在对应的
   * 用户消息上，重发时这条半截会被移除）。
   */
  interrupted?: boolean
}
