/**
 * 关掉上游那个「👋 欢迎使用 Sub2API」新手引导。
 *
 * monica 2026-08-13：不要这个弹窗。
 *
 * 为什么会弹给她一个人：
 * `composables/useOnboardingTour.ts` 里非管理员直接 return，普通运营永远不会自动播；
 * 而她是管理员，`components/layout/AppLayout.vue` 用 `autoStart: true` 起它，
 * 所以每次进后台都弹一次。
 *
 * 做法：提前把「已经看过」的标记写进 localStorage。
 * 上游 `hasSeen()` 读的是
 *   `${storageKey}_${userId}_${role}_${storageVersion}` === 'true'
 * 其中 storageKey 由 AppLayout 传入（管理员 `admin_guide`、其他人 `user_guide`），
 * storageVersion 是 useOnboardingTour 里的常量。
 *
 * 为什么不改上游文件：`AppLayout.vue` 和 `useOnboardingTour.ts` 都不在
 * CLAUDE.md 的白名单里，改了每次跟版都要重解冲突。写 localStorage 是从外部
 * 达到同样效果，零冲突。
 *
 * ⚠ 跟版注意：`storageVersion` 是上游硬编码的常量（当前 `v4_interactive`）。
 * 上游哪天 bump 它，这里的键就对不上，引导会重新开始弹。
 * 所以下面把已知版本都写一遍，并且**另有一层 CSS 兜底**
 * （见 features/designkit/theme/bridge.css 末尾的 driver.js 隐藏规则）。
 * 跟版清单里请加一条：grep 一下 useOnboardingTour.ts 的 storageVersion。
 */

/** 上游 AppLayout 会用到的两个 storageKey。 */
const BASE_KEYS = ['admin_guide', 'user_guide', 'onboarding_tour'] as const

/**
 * 上游用过的 storageVersion。新版本往数组里加，不要替换——
 * 老用户浏览器里可能还留着旧键，全写一遍最省事。
 */
const STORAGE_VERSIONS = ['v4_interactive'] as const

interface MinimalUser {
  id?: number | string
  role?: string
}

/**
 * 把当前用户的「新手引导已看过」标记写上。
 *
 * 每次路由切换都会调一次（很便宜：最多写几个 localStorage 键）。
 * 之所以不只在启动时调一次：登录前拿不到 userId，键名里带 userId，
 * 得等登录之后才知道要写哪个。
 */
export function suppressUpstreamOnboardingTour(user?: MinimalUser | null): void {
  if (typeof window === 'undefined') return

  // 跟上游 getStorageKey() 的兜底值保持一致，否则未登录状态下写的键对不上。
  const userId = user?.id ?? 'guest'
  const role = user?.role ?? 'user'

  try {
    for (const base of BASE_KEYS) {
      for (const version of STORAGE_VERSIONS) {
        const key = `${base}_${userId}_${role}_${version}`
        if (window.localStorage.getItem(key) !== 'true') {
          window.localStorage.setItem(key, 'true')
        }
      }
    }
  } catch {
    // Safari 无痕模式下 localStorage 会抛异常。写不进去还有 CSS 兜底，
    // 不能因为这个让路由跳转挂掉。
  }
}
