/**
 * 文案检查的接口封装。
 *
 * 走上游 axios 单例（三条坑见 `./index.ts` 文件头），路径只写 `/designkit/...`，
 * 实际请求是 `/api/v1/designkit/...`。
 *
 * ⚠ 字段名与后端逐字一致，一律 snake_case，不改驼峰。
 *
 * 端点一个：
 *
 *   POST /designkit/content/check   {text, platform?} → 命中列表 + 标题字数
 *
 * 纯计算、不扣费、毫秒级返回，所以页面敢做「边输边查」（500ms 防抖）。
 */

import { apiClient } from '@/api/client'
import { DESIGNKIT_API_BASE_PATH } from './paths'

// ============================================================================
// 常量
// ============================================================================

/** 一次最多检查多少字，跟后端一致（超了后端 400，前端用 maxlength 提前拦）。 */
export const MAX_CONTENT_CHECK_LENGTH = 10000

/**
 * 平台选项。**镜像自后端 `wordcheck/platform.go` 的规则表**，顺序也一致——
 * 后端那份是权威（接口的 title_max / platform_name 以响应为准），
 * 这份只给「边输入边显示 N/上限」的实时字数条用。改平台规则两处一起改。
 *
 * max 按「字」计（一个汉字 / 字母 / 数字都算 1 个字），跟后端 rune 口径一致。
 */
export interface ContentCheckPlatform {
  key: string
  name: string
  max: number
}

export const CONTENT_CHECK_PLATFORMS: readonly ContentCheckPlatform[] = [
  { key: 'taobao', name: '淘宝', max: 30 },
  { key: 'tmall', name: '天猫', max: 30 },
  { key: 'jd', name: '京东', max: 45 },
  { key: 'pdd', name: '拼多多', max: 30 },
  { key: 'douyin', name: '抖音', max: 30 },
  { key: 'amazon', name: 'Amazon', max: 200 },
]

// ============================================================================
// 类型
// ============================================================================

/**
 * 一次违禁词命中。
 *
 * ⚠ start / end 是**按码点（Unicode code point）**的下标，end 不含——
 * 跟后端的 rune 口径一致。JS 里要先 `Array.from(text)` 切成码点数组再按下标取，
 * 直接 `text[i]` 是 UTF-16 单元，文案里有 emoji 时会错位。
 */
export interface ContentCheckHit {
  /** 词表里的词（英文词是小写形式，如 no.1）。 */
  word: string
  start: number
  end: number
}

/** `POST /designkit/content/check` 的返回。 */
export interface ContentCheckResult {
  /** 命中列表，按出现位置排序；没有命中时是**空数组**（后端保证不给 null）。 */
  hits: ContentCheckHit[]
  /** 标题字数（按字计，首尾空白不算）。 */
  title_len: number
  /** 所选平台的上限；**0 = 没选平台**，不校验字数。 */
  title_max: number
  /** 所选平台的显示名；没选平台时是空串。 */
  platform_name: string
}

// ============================================================================
// 接口
// ============================================================================

/**
 * 检查一段文案。text 可以为空（返回零命中、零字数），platform 空串 = 不校验字数。
 *
 * 两个字段**始终显式传**，不做「有值才带上」：少传一个字段，后端拿到的就是
 * 它自己的默认值，而默认值是什么前端说了不算（同 `sendChatMessage` 的理由）。
 */
export async function checkContent(
  input: { text: string; platform?: string },
  signal?: AbortSignal,
): Promise<ContentCheckResult> {
  const { data } = await apiClient.post<ContentCheckResult>(
    `${DESIGNKIT_API_BASE_PATH}/content/check`,
    {
      text: input.text,
      platform: (input.platform ?? '').trim(),
    },
    { signal },
  )
  return {
    hits: data.hits ?? [],
    title_len: data.title_len ?? 0,
    title_max: data.title_max ?? 0,
    platform_name: data.platform_name ?? '',
  }
}
