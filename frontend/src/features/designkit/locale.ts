/**
 * designkit：界面默认中文。
 *
 * 背景（CLAUDE.md 决策 2 / 第二·五节 D2）：
 * 上游 `i18n/index.ts` 的 `DEFAULT_LOCALE` 是 `'en'`，且该文件不在允许触碰的
 * 上游文件白名单里，不能改。所以由我们在启动早期把默认语言纠正成中文。
 *
 * 为什么不能只写 localStorage：
 * `@/i18n` 在模块加载时就调用 `createI18n({ locale: getDefaultLocale() })`，
 * 而 `router/index.ts` 的 `./title` 依赖链会先把 `@/i18n` 求值完。也就是说
 * 等到 router 的模块体执行时，locale 已经定下来了，只写 localStorage 只能
 * 影响「下一次刷新」——首屏仍然是英文。
 * 因此这里同时做两件事：写 localStorage（持久化）+ 直接改 i18n 当前语言。
 *
 * 本模块 `import { i18n }`，保证 `@/i18n` 一定先于本函数求值，与调用方的
 * import 顺序无关。调用点在 `router/index.ts` 的模块体，早于 `main.ts` 的
 * `bootstrap()`，所以 `initI18n()` 会直接去加载中文语言包，不会闪一屏英文。
 */

import { i18n } from '@/i18n'

/** 与 `@/i18n` 的 LOCALE_KEY 保持一致（该常量未导出，只能重复一份）。 */
export const DESIGNKIT_LOCALE_STORAGE_KEY = 'sub2api_locale'

/** 用户没有显式选过语言时使用的语言。 */
export const DESIGNKIT_DEFAULT_LOCALE = 'zh'

function readSavedLocale(): string | null {
  try {
    return localStorage.getItem(DESIGNKIT_LOCALE_STORAGE_KEY)
  } catch {
    // 隐私模式 / 禁用存储时读不到，按「没选过」处理。
    return null
  }
}

function writeSavedLocale(locale: string): void {
  try {
    localStorage.setItem(DESIGNKIT_LOCALE_STORAGE_KEY, locale)
  } catch {
    // 写不进去也不影响本次会话的显示语言。
  }
}

/**
 * 若用户从未显式选择过语言，就把界面语言设为中文。
 * 用户自己在界面上切过语言（localStorage 里有 'zh' / 'en'）时不覆盖。
 * 幂等，可重复调用。
 */
export function applyDesignkitDefaultLocale(): void {
  const saved = readSavedLocale()
  if (saved === 'zh' || saved === 'en') {
    return
  }

  writeSavedLocale(DESIGNKIT_DEFAULT_LOCALE)
  i18n.global.locale.value = DESIGNKIT_DEFAULT_LOCALE
}
