/**
 * 强制深色模式（DesignKit Dark，全站唯一主题）。
 *
 * 为什么需要（monica 2026-08-15 拍板 D1/D2「全站单一深色」，
 * 推翻 2026-08-13 的「只留白天」）：
 * 上游 `main.ts` 的主题初始化是「没存过偏好就跟随系统外观」——
 * 一台浅色外观的电脑第一次打开就是浅色，而浅色皮已经删了，
 * 会变成「深色变量配浅色版式」的杂拌。
 *
 * 为什么不能只靠 CSS：
 * **有 17 个上游文件在 JS 里读 `classList.contains('dark')`** 来选颜色
 * （图表配色、Stripe 内嵌支付框主题、embedded-url 的 theme 参数）。
 * CSS 管不到它们——class 不在，那些地方会实实在在渲染成浅色。
 * 从根上把 class 钉死，一次全解决。
 *
 * 跟被它替代的 force-light.ts 是同一套机制反着用：
 * 写偏好 → 立刻加 class → MutationObserver 兜底。
 * 侧栏的主题切换按钮已随单主题决策从 AppSidebar 移除，
 * 不存在用户操作和这里打架的路径。
 *
 * 引入点在 `router/index.ts` 顶部（在 main.ts 的 initThemeClass()
 * 读 localStorage 之前求值，所以它读到 'dark'，没有闪烁）。
 */

const THEME_KEY = 'theme'
const DARK_CLASS = 'dark'

function ensureDark(): void {
  document.documentElement.classList.add(DARK_CLASS)
}

export function forceDarkTheme(): void {
  if (typeof window === 'undefined' || typeof document === 'undefined') return

  // 1) 先写偏好。上游 initThemeClass() 稍后会读它，读到 'dark' 就会加 class。
  //    用 try：Safari 无痕模式下 localStorage 会抛异常，不能因此让应用起不来。
  try {
    window.localStorage.setItem(THEME_KEY, 'dark')
  } catch {
    // 存不进去也无所谓，下面两道还在
  }

  // 2) 立刻加一次，覆盖「首帧还没跑到 initThemeClass」的窗口。
  ensureDark()

  // 3) 兜底：万一上游哪条路径把它摘了，这里再加回来。
  const observer = new MutationObserver(() => {
    if (!document.documentElement.classList.contains(DARK_CLASS)) ensureDark()
  })
  observer.observe(document.documentElement, {
    attributes: true,
    attributeFilter: ['class'],
  })
}

forceDarkTheme()
