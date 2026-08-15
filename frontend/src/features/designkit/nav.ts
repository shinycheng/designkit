/**
 * designkit：侧边栏相关的常量与图标。
 *
 * 放在这里而不是直接写进 `AppSidebar.vue`，是为了把上游文件的改动压到最小
 * （CLAUDE.md 第一节：AppSidebar.vue 是仅有的 5 个可触碰上游文件之一）。
 * 跟版时 AppSidebar.vue 只有三处几行的差异，冲突好解。
 */

import { h } from 'vue'

// ==================== 路由路径 ====================

// 路径跟上游其它用户页面一个风格（/dashboard、/keys、/usage、/subscriptions），
// 不再用 /designkit/* 这个独立命名空间——monica 2026-08-13：
// 「和其他侧栏一样，直接显示在当前页面的右侧」，网址也要一致。
// 老路径在 router/index.ts 里保留为跳转，书签不会失效。
export const DESIGNKIT_WORKBENCH_PATH = '/workbench'
export const DESIGNKIT_INSPIRATION_PATH = '/inspiration'
export const DESIGNKIT_GALLERY_PATH = '/gallery'

/**
 * 「商品图设置」（**仅管理员**）。
 *
 * 路径放在 `/admin/*` 下而不是跟上面三个一样的顶层单段：它是后台配置页，
 * 在侧边栏里也排在管理员那一区（跟上游「系统设置」并列），
 * 网址跟位置对得上，管理员不会以为自己进错了地方。
 */
export const DESIGNKIT_SETTINGS_PATH = '/admin/designkit-settings'

/**
 * 普通用户（非管理员）在侧边栏里要隐藏的**上游原生**菜单。
 *
 * 依据 CLAUDE.md 决策 10：使用者是电商运营，只需要我们自建的三个页面 + 个人资料；
 * 上游那套面向 API 用户的菜单对他们全是噪音。
 *
 * 实现方式刻意选择「在 AppSidebar.vue 里本地按 isAdmin 过滤」，
 * **不走 `utils/featureFlags.ts`**：那套机制新增一个开关要改 11 个文件，
 * 其中 10 个是上游文件（CLAUDE.md 第二·五节 D1），跟版成本无法承受。
 *
 * 注意：**只隐藏菜单入口，不动路由、不删上游代码**。
 * 直接输入 URL 仍然打得开——这是有意的：决策 9 说这些功能「页面关掉但代码不删」，
 * 哪天要对外收费，把这份名单清空即可恢复。
 */
export const DESIGNKIT_UPSTREAM_NAV_HIDDEN_FROM_USER: readonly string[] = [
  '/keys', // 我的密钥
  '/batch-image', // 批量生图（上游的 BatchImage，跟我们的批量出图不是一回事）
  '/usage', // 用量
  '/redeem', // 兑换码
  '/affiliate', // 推广返佣
  '/available-channels', // 可用渠道
  // 渠道状态：上游给「买 API 用量的客户」看的服务可用性页（7/15/30 天可用率、延迟）。
  //
  // 三个理由都指向隐藏：
  // ① 数据源是「渠道监控」（`channel_monitors` 表），管理员要手填 API 地址 + API Key +
  //    模型名才有内容。我们一条都没建（配置手册第 5 步就是「什么都不要做」），
  //    所以这一页对运营永远是空的，写着「管理员尚未配置可监控的渠道」。
  // ② 它跟我们的出图账号**没有任何关系**：它要 API Key，而我们的账号是
  //    ChatGPT OAuth 登录，根本没有 Key 可填（决策 22）。
  // ③ ⚠ 更要紧的是**不能**去配它：探针是真发请求的（`channel_monitor_checker.go:508`），
  //    间隔下限 15 秒（`125_add_channel_monitors.sql:32`）= 一天 5760 次。
  //    真填了出图模型就是在不停烧 Plus 额度，而这笔消耗在 designkit 的账里完全看不见。
  //
  // 它之所以一直露在外面：开关 `channel_monitor_enabled` 是 **opt-out（默认开）**
  //（`utils/featureFlags.ts:97`），而这份名单当初只收了路径相近的 `/available-channels`。
  '/monitor', // 渠道状态
  '/subscriptions', // 我的订阅
  '/purchase', // 购买订阅
  '/orders', // 我的订单
  // 模型广场：入口其实在 AppHeader.vue（顶栏），不在侧边栏，所以这一条在这里不生效。
  // AppHeader.vue 不在允许触碰的上游文件名单里，只能由管理员在
  //「系统设置」里关掉 model_plaza_enabled 来隐藏。列在这里是为了留个记录。
  '/model-plaza',
]

const hiddenPathSet = new Set(DESIGNKIT_UPSTREAM_NAV_HIDDEN_FROM_USER)

/** 该菜单路径是否应该对普通用户隐藏。管理员不走这个判断，永远看得到全部。 */
export function isUpstreamNavHiddenFromUser(path: string): boolean {
  return hiddenPathSet.has(path)
}

// ==================== 图标 ====================
// 与 AppSidebar.vue 里上游图标的写法一致：24x24 线性图标，描边用 currentColor。

function lineIcon(...paths: string[]) {
  return {
    render: () =>
      h(
        'svg',
        { fill: 'none', viewBox: '0 0 24 24', stroke: 'currentColor', 'stroke-width': '1.5' },
        paths.map((d) =>
          h('path', {
            'stroke-linecap': 'round',
            'stroke-linejoin': 'round',
            d,
          }),
        ),
      ),
  }
}

/** 生成工作台：魔法棒 + 星光 */
export const DesignkitWorkbenchIcon = lineIcon(
  'M9.813 15.904 9 18.75l-.813-2.846a4.5 4.5 0 0 0-3.09-3.09L2.25 12l2.846-.813a4.5 4.5 0 0 0 3.09-3.09L9 5.25l.813 2.846a4.5 4.5 0 0 0 3.09 3.09L15.75 12l-2.846.813a4.5 4.5 0 0 0-3.09 3.09Z',
  'M18.259 8.715 18 9.75l-.259-1.035a3.375 3.375 0 0 0-2.455-2.456L14.25 6l1.036-.259a3.375 3.375 0 0 0 2.455-2.456L18 2.25l.259 1.035a3.375 3.375 0 0 0 2.456 2.456L21.75 6l-1.035.259a3.375 3.375 0 0 0-2.456 2.456Z',
  'M16.894 20.567 16.5 21.75l-.394-1.183a2.25 2.25 0 0 0-1.423-1.423L13.5 18.75l1.183-.394a2.25 2.25 0 0 0 1.423-1.423l.394-1.183.394 1.183a2.25 2.25 0 0 0 1.423 1.423l1.183.394-1.183.394a2.25 2.25 0 0 0-1.423 1.423Z',
)

/** 灵感库：灯泡 */
export const DesignkitInspirationIcon = lineIcon(
  'M12 18v-5.25m0 0a6.01 6.01 0 0 0 1.5-.189m-1.5.189a6.01 6.01 0 0 1-1.5-.189m3.75 7.478a12.06 12.06 0 0 1-4.5 0m3.75 2.383a14.406 14.406 0 0 1-3 0M14.25 18v-.192c0-.983.658-1.823 1.508-2.316a7.5 7.5 0 1 0-7.517 0c.85.493 1.509 1.333 1.509 2.316V18',
)

/** 商品图设置：推子（跟上游「系统设置」的齿轮区分开，一眼看得出不是同一个页面） */
export const DesignkitSettingsIcon = lineIcon(
  'M10.5 6h9.75M10.5 6a1.5 1.5 0 1 1-3 0m3 0a1.5 1.5 0 1 0-3 0M3.75 6H7.5m3 12h9.75m-9.75 0a1.5 1.5 0 0 1-3 0m3 0a1.5 1.5 0 0 0-3 0m-3.75 0H7.5m9-6h3.75m-3.75 0a1.5 1.5 0 0 1-3 0m3 0a1.5 1.5 0 0 0-3 0m-9.75 0h9.75',
)

/** 我的图片：照片堆 */
export const DesignkitGalleryIcon = lineIcon(
  'M2.25 15.75l5.159-5.159a2.25 2.25 0 013.182 0l5.159 5.159m-1.5-1.5l1.409-1.409a2.25 2.25 0 013.182 0l2.909 2.909m-18 3.75h16.5a1.5 1.5 0 001.5-1.5V6a1.5 1.5 0 00-1.5-1.5H3.75A1.5 1.5 0 002.25 6v12a1.5 1.5 0 001.5 1.5zm10.5-11.25h.008v.008h-.008V8.25zm.375 0a.375.375 0 11-.75 0 .375.375 0 01.75 0z',
)
