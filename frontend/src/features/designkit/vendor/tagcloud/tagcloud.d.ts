/**
 * TagCloud 2.5.0 的类型声明。
 *
 * 库本身是 jsDelivr 打好的 ESM 包（tagcloud.js，MIT，见同目录 LICENSE），
 * 原样保存在仓库里、没做改动，所以它不带 .d.ts —— TypeScript 会把它推成 any 然后报错。
 *
 * 这里**只声明我们真正用到的那部分**，不求覆盖库的全部能力：
 * 声明多了以后升级库时要跟着核对，而我们只用了创建 + 销毁 + 暂停三件事。
 *
 * 用在哪：登录页左侧那 15 个电商平台标识绕成的自转球（DesignkitLoginView.vue）。
 */
export interface TagCloudOptions {
  /** 球半径（像素）。我们按容器宽度算，见 cloudRadius。 */
  radius?: number
  /** 最大转速。'slow' | 'normal' | 'fast'。 */
  maxSpeed?: 'slow' | 'normal' | 'fast'
  /** 初始转速。 */
  initSpeed?: 'slow' | 'normal' | 'fast'
  /** 鼠标移开后是否继续转。true = 继续（我们要的是一直缓慢自转）。 */
  keep?: boolean
  /** 是否让库往容器上写行内样式。**给 false**：版式由我们自己的 CSS 管。 */
  useContainerInlineStyles?: boolean
  /** 同上，作用在每一项上。 */
  useItemInlineStyles?: boolean
  /** 其余选项按库文档，用到再补。 */
  [key: string]: unknown
}

export interface TagCloudInstance {
  /** 停止转动（不拆 DOM）。 */
  pause?: () => void
  /** 恢复转动。 */
  resume?: () => void
  /** 拆掉：移除 DOM 和内部定时器。**离开页面前必须调**，否则定时器一直跑。 */
  destroy?: () => void
}

/**
 * @param container 容器元素
 * @param texts 每一项的内容。**可以是 HTML 字符串**（我们传的就是拼好的
 *              `<span class="dk-auth-logo-token">…</span>`）
 */
const TagCloud: (
  container: HTMLElement,
  texts: string[],
  options?: TagCloudOptions,
) => TagCloudInstance

export default TagCloud
