/**
 * designkit 接口的数据类型。
 *
 * ⚠ 字段名与后端 `backend/internal/designkit/handler/dto.go` **逐字一致**，
 * 一律 snake_case，不做驼峰改名。这样出问题时浏览器网络面板里看到的字段
 * 和这里的类型名对得上，也方便跟 ERP 对接文档核对。
 *
 * 只有两个字段被「升级」了类型（见下面的 Price）：
 *   - `RatioOption.unit_price`
 *   - `JobEstimate.unit_price` / `JobEstimate.estimated_cost`
 *
 * 后端在这三处用 `null` 表示**价格待确认**（价目表还没实测出来）。
 * 如果照原样声明成 `string | null`，页面上写 `Number(unit_price)` 会得到 **0**，
 * 于是「不知道多少钱」被显示成「免费」——这是会让人白花钱的错。
 * 所以这三处在本文件里是 `Price` 对象，`Number(...)` 得到 NaN，
 * 想读金额必须先判断 `confirmed`，编译器会逼你处理「待确认」这一支。
 */

// ============================================================================
// 金额
// ============================================================================

/**
 * 金额。后端一律用**字符串**传（decimal 序列化成带引号的字符串），
 * 这样不会出现 0.1+0.2=0.30000000000000004 这种给运营看的怪数。
 *
 * 单位固定美元（决策 18，不做汇率换算）。
 * **界面上一律渲染 `$`，任何文案里都不许出现「元」。**
 * 要显示请用 `formatMoney()`，不要自己拼字符串。
 */
export type Money = string

/** 价格已经实测确认过。 */
export interface PriceConfirmed {
  readonly confirmed: true
  readonly amount: Money
}

/**
 * 价格待确认：价目表还没实测出来（设计定型 第十节 1，上线阻塞项）。
 *
 * 刻意**没有** amount 字段：想读金额必须先 `if (price.confirmed)`，
 * 否则编译不过。这就是它存在的全部意义。
 */
export interface PriceUnconfirmed {
  readonly confirmed: false
}

/** 单价 / 预估花费。用 `formatPrice()` 显示，用 `price.confirmed` 分支取值。 */
export type Price = PriceConfirmed | PriceUnconfirmed

// ============================================================================
// 枚举
// ============================================================================

/**
 * 一个批次的状态。
 *
 * ⚠ `cancelled` **不代表没花钱**：已经在跑的那几张照样出图照样扣费。
 * 判断「有没有出图」一律看 `success_count`，不要看 status。
 */
export type JobStatus =
  | 'created'
  | 'holding'
  | 'running'
  | 'settling'
  | 'succeeded'
  | 'partially_failed'
  | 'failed'
  | 'cancelled'

/** 单张图（一个「商品图 × 提示词」）的状态。 */
export type ItemStatus = 'pending' | 'running' | 'succeeded' | 'failed' | 'cancelled'

/** 记录是谁产生的：运营在网页上点的，还是外部系统调接口来的。 */
export type Origin = 'web' | 'erp'

// ============================================================================
// 素材（上传的商品图）
// ============================================================================

/** `POST /designkit/assets`、`GET /designkit/assets/:uid` 的返回。 */
export interface DesignkitAsset {
  /** 对外编号。提交批次时用它。 */
  uid: string
  content_type: string
  byte_size: number
  /** 后端还没读出宽高时是 null。 */
  width: number | null
  height: number | null
  origin: Origin
  /** RFC3339 UTC，例如 "2026-08-13T02:03:04Z"。 */
  created_at: string
  /**
   * 取原图字节的地址，形如 `/api/v1/designkit/assets/<uid>/content`。
   *
   * ⚠ **不能直接塞进 `<img src>`**：这个地址要带 JWT 请求头才放行，
   * 而 `<img>` 不会带请求头，结果是 401 加一个碎图标。
   * 用 `useAuthedImages()`（composables/useAuthedImage.ts）把它取成 blob 再显示。
   */
  content_url: string
}

// ============================================================================
// 出图比例
// ============================================================================

/** `GET /designkit/ratios` 里的一个比例（已把 unit_price 升级成 Price）。 */
export interface RatioOption {
  /** 比例串，例如 "3:4"。提交批次时原样回传。 */
  ratio: string
  /** 补白边之后的目标宽高，只用于展示，不是计费依据。 */
  target_width: number
  target_height: number
  /** 预估档位 "1K" / "2K" / "4K"。真正计费的档由上游按实际输出像素定，所以只是预估。 */
  pricing_tier: string
  /** false = 价格待确认，界面必须显示「价格待确认」，不许显示任何猜的单价。 */
  price_confirmed: boolean
  /** 单张预估价。等价于 price_confirmed，但类型上挡住「当成 0 用」。 */
  unit_price: Price
  /** 界面默认选中这一个。 */
  is_default: boolean
}

/** `GET /designkit/ratios` 的返回。 */
export interface RatioList {
  ratios: RatioOption[]
  /** 固定 "USD"。 */
  currency: string
}

// ============================================================================
// 报价
// ============================================================================

/** `POST /designkit/jobs/estimate` 的请求体。 */
export interface JobSpecInput {
  /** 出图比例，形如 "3:4"，从 `GET /ratios` 里选。 */
  ratio: string
  /** 选中的商品图 uid，顺序即展开时的**外层**顺序。至少一个。 */
  asset_uids: string[]
  /** 选中的灵感库提示词 uid，顺序即展开时的**内层**顺序。 */
  prompt_uids?: string[]
  /** 运营手打的提示词，接在 prompt_uids 后面。 */
  prompts?: string[]
  /** 出图模型；不填用系统默认。 */
  model?: string
  /** 预处理时保留透明底；不填 = 合成白底。 */
  keep_transparency?: boolean
}

/** `POST /designkit/jobs` 的请求体。 */
export interface CreateJobInput extends JobSpecInput {
  /** 给这批起的名字，可以不填。 */
  name?: string
  /** 出完回调这个地址；主要给 ERP 用，界面上不填。 */
  callback_url?: string
}

/** `POST /designkit/jobs/estimate` 的返回（已把两个金额升级成 Price）。 */
export interface JobEstimate {
  /** 展开后的总张数 = 商品图数 × 提示词数。 */
  item_count: number
  asset_count: number
  prompt_count: number
  /** 单次批量上限。超了后端会拒绝，界面应该提前拦。 */
  max_batch_items: number
  pricing_tier: string
  /** false 时 unit_price / estimated_cost 都是「待确认」。 */
  price_confirmed: boolean
  /** 单张预估价。 */
  unit_price: Price
  /** 这一批的预估花费。 */
  estimated_cost: Price
  /** 固定 "USD"。 */
  currency: string
  /** 当前余额。 */
  balance: Money
  /** 可用额 = 余额 − 已冻结。真正拦透支的是提交时的数据库检查，这里只是参考值。 */
  available: Money
  /**
   * 可用额够不够这一单。
   *
   * ⚠ **价格待确认时后端一律给 true**（拦不住也不该假装拦得住）。
   * 所以 `sufficient === true` 不等于「一定提交得上去」，提交仍可能被后端挡下来。
   */
  sufficient: boolean
  /** 还差多少（够的时候是 "0"）。不够时界面要显示它 +「申请额度」按钮。 */
  shortfall: Money
}

// ============================================================================
// 批次
// ============================================================================

/**
 * `POST /designkit/jobs` 的返回。
 *
 * 刻意只有小对象、**不含 items 数组**：这个响应体会被上游幂等协调器整个存进
 * 数据库、重放时原样吐回来。要看进度去 `GET /jobs/:uid` 和 `/items`。
 */
export interface JobCreated {
  /** 任务号。后面所有查询、轮询都用它。 */
  uid: string
  status: JobStatus
  item_count: number
  currency: string
  estimated_cost: Money
  revision: number
  name: string
  ratio: string
  model: string
  created_at: string
  callback_url: string | null
}

/** `GET /designkit/jobs/:uid`、以及列表里的一行。 */
export interface Job {
  uid: string
  name: string
  status: JobStatus
  origin: Origin
  ratio: string
  model: string

  /**
   * 四个计数任何终态都会一起返回。
   * **判断有没有出图一律看 success_count，不看 status。**
   */
  item_count: number
  success_count: number
  fail_count: number
  /** 只统计「还没开始就被『停止排队』砍掉的」，不含正在跑的。 */
  cancelled_count: number

  currency: string
  /** 提交时算的预估花费，一定有值。 */
  estimated_cost: Money
  /** 结算后的实际花费；还没结算时是 null（**不是 0**，别显示成 $0.00）。 */
  actual_cost: Money | null

  revision: number
  /** 整批失败的原因码，例如 "DK_STALE"。 */
  last_error_code: string | null
  /** 上面那个码对应的**中文**，可以直接显示给运营。 */
  last_error_message: string | null

  created_at: string
  updated_at: string
  started_at: string | null
  finished_at: string | null
  /** 有值 = 账已经算清，不能再重试了。 */
  settled_at: string | null
  /** 有值 = 运营点过「停止排队」。 */
  cancel_requested_at: string | null

  /** 取这一批每张图的地址，形如 `/api/v1/designkit/jobs/<uid>/items`。 */
  items_url: string
}

/** `GET /designkit/jobs` 的一页（游标分页，不用页码）。 */
export interface JobPage {
  items: Job[]
  has_more: boolean
  /** 下一页的游标，**不透明字符串，不要解析它**。has_more 为 false 时是 null。 */
  next_cursor: string | null
}

// ============================================================================
// 单张图
// ============================================================================

/** `GET /designkit/jobs/:uid/items` 里的一行。**返回顺序固定按 seq 升序。** */
export interface JobItem {
  /** 批次内序号，**从 1 开始**。展开顺序：图1×词1、图1×词2、图2×词1… */
  seq: number
  status: ItemStatus
  /** 生成时用的提示词原文（快照，灵感库后来改了也不跟着变）。 */
  prompt_text: string
  /** 用了哪张商品图 / 哪条灵感库提示词；手打提示词时 prompt_uid 为 null。 */
  asset_uid: string | null
  prompt_uid: string | null

  /** 已经试了几次 / 最多能试几次（默认 3）。重试一次 = 重新收一次钱。 */
  attempt_count: number
  max_attempts: number

  /** 我方错误码，界面显示中文时把它用小字附在后面，方便运营截图报障。 */
  error_code: string | null
  /** 中文错误文案，可以直接显示。**后端保证不会是英文原文。** */
  error_message: string | null

  currency: string
  /** 这一张实际扣了多少；没成功 / 还没结算时是 null。 */
  billed_cost: Money | null
  billing_tier: string | null

  created_at: string
  started_at: string | null
  finished_at: string | null

  /** 当前版本的结果图，按 image_index 升序。还没出图时是空数组。 */
  images: DesignkitImage[]
  /** 直接取这一张第一张图的地址。同样要走 blob，不能塞 `<img src>`。 */
  content_url: string
}

// ============================================================================
// 结果图
// ============================================================================

/** `GET /designkit/images/:uid`，以及 JobItem.images 里的一张。 */
export interface DesignkitImage {
  uid: string
  /** 所属批次的任务号。 */
  job_uid: string
  /** 所属那一张的批次内序号，从 1 开始。 */
  seq: number
  /** 第几次尝试出的图。 */
  attempt: number
  /** 同一次尝试返回多张时的序号，从 1 开始。 */
  image_index: number
  /** 是不是当前版本（重试之后旧图会变成 false）。 */
  is_current: boolean
  content_type: string
  byte_size: number
  width: number | null
  height: number | null
  billing_tier: string | null
  created_at: string
  /** 取图片字节的地址。要走 blob，不能塞 `<img src>`。 */
  content_url: string
}

// ============================================================================
// 用量汇总（工作台角落那三个数）
// ============================================================================

/**
 * `GET /designkit/me/usage/summary` 的返回。
 *
 * ⚠ **后端还没实现这个端点**（register_business.go 里是 TODO）。
 * `getUsageSummary()` 遇到 404 会返回 null，界面据此把这块整个藏起来即可，
 * 不要显示成 0 —— 「本月花费 $0」和「还不知道花了多少」是两回事。
 *
 * ⚠ 张数和花费是**两套口径，刻意不统一**（设计定型 5.2）：
 *   张数来自图片表（删掉的图不算），花费来自账单（出图失败但已计费的也算）。
 * 所以会出现「张数是 0，花费不是 0」。界面必须把 `note` 显示出来，
 * 这句话是文案的一部分，不是可选提示。
 */
export interface UsageSummary {
  /** 统计月份，形如 "2026-08"。 */
  period: string
  /** 本月出图张数（只算还留在作品库里的图）。 */
  image_count: number
  /** 本月花费（按实际扣费统计）。 */
  cost: Money
  /** 当前余额。 */
  balance: Money
  /** 可用额 = 余额 − 已冻结。 */
  available: Money
  currency: string
  /** 后端下发的中文口径说明；没有时用 i18n 的 `designkit.usage.note` 兜底。 */
  note?: string
}

// ============================================================================
// 健康检查
// ============================================================================

/**
 * `GET /designkit/health` 的返回。
 *
 * 所有字段都是可选的：后端还在改，多一个少一个字段都不该让页面白屏。
 */
export interface DesignkitHealth {
  /** 'ok' = 全通；'degraded' = 有东西不对。**HTTP 一定是 200，必须看这个字段。** */
  status?: 'ok' | 'degraded' | string
  /** 固定 'designkit'，用来确认打到的确实是我们的接口。 */
  module?: string
  /** 数据库通不通。 */
  db?: 'ok' | 'error' | string
  /**
   * 模块功能齐不齐。跟 db 是两件事：出图网关没配、图像处理服务地址填错时，
   * 数据库是好的（db='ok'）但一张图也出不来。
   */
  module_status?: 'ok' | 'degraded' | 'down' | string
  /** 中文降级原因，只在降级时出现。直接显示给运营，让他截图给管理员。 */
  degraded_reasons?: string[]
}

// ============================================================================
// 服务端原样形状（内部用）
// ============================================================================
//
// 下面两个是「金额还没升级成 Price」的原始形状，只给 api/index.ts 里的转换函数用。
// 页面不要用它们。

/** @internal 服务端原样返回的比例。 */
export interface RatioOptionWire {
  ratio: string
  target_width: number
  target_height: number
  pricing_tier: string
  unit_price: string | null
  price_confirmed: boolean
  is_default: boolean
}

/** @internal 服务端原样返回的比例列表。 */
export interface RatioListWire {
  ratios: RatioOptionWire[]
  currency: string
}

/** @internal 服务端原样返回的报价。 */
export interface JobEstimateWire {
  item_count: number
  asset_count: number
  prompt_count: number
  max_batch_items: number
  pricing_tier: string
  unit_price: string | null
  estimated_cost: string | null
  price_confirmed: boolean
  currency: string
  balance: string
  available: string
  sufficient: boolean
  shortfall: string
}

// ============================================================================
// 兼容旧代码
// ============================================================================

/**
 * 请求失败时 `@/api/client` 拒绝出来的原始形状。
 *
 * ⚠ 页面**不要**直接显示它的 message —— 那多半是 axios 的英文原文
 * （"Request failed with status code 402"）。一律先过 `toFriendlyError()`。
 */
export interface DesignkitApiError {
  status?: number
  code?: string | number
  message?: string
}
