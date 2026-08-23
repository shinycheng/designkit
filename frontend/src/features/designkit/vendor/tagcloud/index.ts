/**
 * TagCloud 的 TypeScript 包装层。
 *
 * ── 为什么要有这一层 ────────────────────────────────────────────
 *
 * `tagcloud.js` 是 jsDelivr 打好的 ESM 包（MIT，见同目录 LICENSE），原样保存在
 * 仓库里、没做任何改动，所以它不带类型。
 *
 * 最初的做法是在它旁边放一个同名的 `tagcloud.d.ts`。**那个做法在这个项目里
 * 两套 TS 配置下表现不一致**：`pnpm build`（`vue-tsc -b`）认，
 * 而开发服务器里 `vite-plugin-checker` 跑的那次不认，报
 * 「Could not find a declaration file for module '../vendor/tagcloud/tagcloud.js'」。
 * 同一份代码，构建绿、开发红 —— 这种不一致比报错本身更难查。
 *
 * 所以改成这一层：调用方 import 的是一个**真正的 .ts 文件**，
 * 两套配置都没有歧义。对无类型第三方包的引用被收在下面那一行里，
 * 用 `@ts-expect-error` 明确标注，而不是散落在业务代码里。
 *
 * ── 只声明用得上的那部分 ────────────────────────────────────────
 *
 * 库本身能力比这多，但我们只用「创建 / 暂停 / 销毁」三件事。
 * 声明多了以后升级库要跟着核对，收益为零。
 */

/** 造球时能调的选项（只列我们用到的）。 */
export interface TagCloudOptions {
  /** 球半径（像素）。按容器宽度算，见调用方的 cloudRadius。 */
  radius?: number
  /** 最大转速。 */
  maxSpeed?: 'slow' | 'normal' | 'fast'
  /** 初始转速。 */
  initSpeed?: 'slow' | 'normal' | 'fast'
  /** 鼠标移开后是否继续转。true = 继续（我们要的是一直缓慢自转）。 */
  keep?: boolean
  /** 转动方向（角度）。老 designkit 用的是 135。 */
  direction?: number
  /**
   * 每一项的内容是否按 HTML 解析。
   *
   * ⚠ **我们必须给 true**：库默认走 innerText，而我们传的是拼好的
   * `<span class="dk-auth-logo-token">…</span>` —— 默认值下会被当成纯文本
   * 原样显示，页面上出现一大串 `<span class=...` 的源码。2026-08-14 踩过。
   */
  useHTML?: boolean
  /**
   * 是否让库往**容器**上写行内样式。
   *
   * **给 false**：容器的 position/尺寸由 .dk-auth-logo-cloud 管
   * （absolute + inset:0，才能压到卡片后面）；让库写会把它变成 relative。
   */
  useContainerInlineStyles?: boolean
  /**
   * 是否让库往**每一项**上写行内样式。
   *
   * ⚠ **必须给 true**：每个标的定位（transform / position）就是靠它写的。
   * 给 false 等于说「样式我自己管」，而我们并没有那套 CSS，
   * 结果是 15 个标平铺在正文里、完全不转。2026-08-14 踩过。
   */
  useItemInlineStyles?: boolean
}

/** 造出来的那个球。三个方法都标成可选：库的老版本不一定都有。 */
export interface TagCloudInstance {
  /** 停止转动，不拆 DOM。 */
  pause?: () => void
  /** 恢复转动。 */
  resume?: () => void
  /** 拆掉：移除 DOM 和内部定时器。**离开页面前必须调**，否则定时器一直跑。 */
  destroy?: () => void
}

type TagCloudFactory = (
  container: HTMLElement,
  /** 每一项的内容。**可以是 HTML 字符串**，我们传的就是拼好的 `<span>…</span>`。 */
  texts: string[],
  options?: TagCloudOptions,
) => TagCloudInstance

/**
 * 动态加载 TagCloud。
 *
 * **动态而不是静态 import**：登录页之外的任何页面都不该为这 8KB 买单。
 * Vite 会把它单独打成一个块（实测产物 `assets/tagcloud-*.js`，7.2KB）。
 *
 * 加载失败时**返回 null 而不是抛异常**：调用方（登录页）拿到 null 会退回
 * 静态排列。为一个装饰性动画把登录页搞崩是完全不成比例的。
 */
export async function loadTagCloud(): Promise<TagCloudFactory | null> {
  try {
    // ⚠ 这里的 `as unknown` 是**有意的**：tagcloud.js 是无类型的第三方打包产物，
    // 类型由本文件上面那几个 interface 提供。不要试图给它加 @ts-expect-error ——
    // 这个项目的 TS 配置下那条指令是多余的（会报 TS2578），而两套配置
    // （build 的 vue-tsc -b 和 dev 的 vite-plugin-checker）对它的判定还不一致。
    // 走 `as unknown as` 是唯一在两边都稳定的写法。
    const module = (await import('./tagcloud.js')) as unknown as {
      default?: TagCloudFactory
    }
    const factory = module?.default
    return typeof factory === 'function' ? factory : null
  } catch {
    return null
  }
}
