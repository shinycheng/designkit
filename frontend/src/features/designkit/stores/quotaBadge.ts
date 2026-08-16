/**
 * 侧边栏「额度申请」菜单项的红点：有没有待处理的申请。
 *
 * ── 为什么是模块级 ref 而不是 pinia ────────────────────────────────
 * 只有一个数字、两个使用方（AppSidebar 的红点、额度申请页顶部的角标），
 * 不需要持久化也不需要派生状态。promptHandoff 用 pinia 是因为要配合
 * sessionStorage 兜整页刷新，这里没有那个需求。
 *
 * ── 刷新时机（决策：简单轮询都不上，别过度设计）────────────────────
 *  1. 侧边栏挂载时（管理员登录后第一眼就该看到红点）；
 *  2. 额度申请页每次拉列表、每次处理完（页面自己调 setDesignkitQuotaPendingCount）。
 * 不做定时轮询：管理员切页面本来就频繁，红点晚几分钟不是事故。
 *
 * ── 失败静默 ──────────────────────────────────────────────────────
 * 红点拉不到**绝不弹错**：它是锦上添花，弹错会让管理员以为系统坏了。
 * 拉失败保持上一次的值。
 */

import { ref } from 'vue'
import { listQuotaRequests } from '../api/quotaAdmin'

/** 待处理的额度申请条数。侧边栏红点显示条件：> 0。 */
export const designkitQuotaPendingCount = ref(0)

/**
 * 重新拉一次待处理条数。**只在管理员会话里调**（后端对非管理员返回 403，
 * 虽然这里会静默吞掉，但白发一次 403 请求没有意义）。
 */
export async function refreshDesignkitQuotaPendingCount(): Promise<void> {
  try {
    // limit=1：只要 pending_count 这个数，不要整页数据。
    const list = await listQuotaRequests('pending', 1)
    designkitQuotaPendingCount.value = list.pending_count
  } catch {
    // 静默：红点是锦上添花，拉不到保持旧值，绝不弹错。
  }
}

/** 额度申请页拉过列表 / 处理完一条之后，把最新的数同步给红点。 */
export function setDesignkitQuotaPendingCount(count: number): void {
  designkitQuotaPendingCount.value = Math.max(0, count)
}
