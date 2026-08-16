/**
 * 灵感库（提示词库）的接口封装。
 *
 * 走的还是上游 axios 单例（见 `./index.ts` 开头那三条坑），所以这里的路径只写
 * `/designkit/...`，实际请求是 `/api/v1/designkit/...`。
 *
 * 端点分两组，**权限完全不同**：
 *
 *   所有登录用户（运营）        GET /prompt-categories、GET /prompts、GET /prompts/:uid、
 *                              POST /prompts/suggest
 *   仅管理员                   POST /prompts/sync、GET /prompts/sync/latest、
 *                              GET|PUT /prompts/sync/settings
 *
 * ⚠ 后端的同步服务没装上时，管理员那四个端点**整组不注册**（404），
 * 而不是返回一个错误信封。所以调用方必须用 `isSyncUnavailableError()` 认出这种情况，
 * 显示「同步功能还没准备好」，**不要**弹「找不到这条记录」——
 * 那会让管理员以为是自己点错了。
 * AI 推荐（`POST /prompts/suggest`）没上线时是同一回事，但**不能用同一个函数认**，
 * 理由见 `isSuggestUnavailableError()` 上面那段。
 *
 * ⚠ 字段名与后端 `handler/dto.go` 逐字一致，一律 snake_case，不改驼峰。
 */

import { apiClient } from '@/api/client'
import type { Proxy, ProxyProtocol } from '@/types'
import { DESIGNKIT_API_BASE_PATH } from './paths'
import { toFriendlyError } from './errors'

// ============================================================================
// 类型
// ============================================================================

/** 一个分类。**没有 id**：对外标识是 slug，筛选也用 slug。 */
export interface PromptCategory {
  /** 稳定的英文标识，例如 `ecommerce-main-image`。筛选参数用它。 */
  slug: string
  /** 界面显示的名字，后端保证不为空（中文名 → 英文名 → slug）。 */
  name: string
  /** 英文名，可能是空串。 */
  name_en: string
  /** 越小越靠前。后端返回的顺序已经排好，**前端不要再排一遍**。 */
  sort_order: number
  /** 这个分类下有多少条可用的提示词。 */
  prompt_count: number
}

/** `GET /designkit/prompt-categories` 的返回。 */
export interface PromptCategoryList {
  categories: PromptCategory[]
  /** 全部分类加起来一共多少条，界面上「全部（14700）」那个数。 */
  total_prompt_count: number
}

/**
 * 提示词正文里的一个占位变量。
 *
 * 正文里长这样：`{main_color}`。`name` 就是花括号里那个键，
 * `example` 是上游给的默认值（可能是空串），当输入框的提示文字用。
 */
export interface PromptVariable {
  name: string
  example: string
}

/** 一条提示词。**body 是完整正文，没有截断。** */
export interface Prompt {
  uid: string
  /** 标题，可能是空串（上游有不少条只有正文）。 */
  title: string
  /** 正文。「用它生成」带走的就是它（占位符替换之后）。 */
  body: string
  /** 占位变量；没有变量时是**空数组**，后端保证不会是 null。 */
  variables: PromptVariable[]
  /** 上游给的效果预览图（外链，仅展示）；没有时是 null。 */
  preview_url: string | null
  /** 所属分类；没有分类时都是空串。 */
  category_slug: string
  category_name: string
  /** youmind = 从开源库同步来的；user = 运营自己存的。 */
  source: 'youmind' | 'user' | string
  /** 还在不在架上。列表里只会出现 true；详情可能是 false（历史收藏点进来的）。 */
  is_enabled: boolean
  created_at: string
  updated_at: string
}

/** `GET /designkit/prompts` 的一页（游标分页，但**有总数**）。 */
export interface PromptPage {
  items: Prompt[]
  /** 符合当前筛选条件的总条数。灵感库一万多条，不显示总数运营不知道该搜还是该翻。 */
  total: number
  has_more: boolean
  /** 下一页的游标，**不透明字符串，不要解析它**。没有下一页时是 null。 */
  next_cursor: string | null
}

/** 一次同步的记录。 */
export interface PromptSyncRun {
  /** manual = 管理员点的；auto = 每 12 小时自动跑的。 */
  kind: 'manual' | 'auto' | string
  status: 'running' | 'succeeded' | 'failed' | 'skipped' | string
  started_at: string
  /** 还在跑时是 null。 */
  finished_at: string | null
  /** 从开源库拉回来多少条。 */
  fetched: number
  inserted: number
  updated: number
  /** 内容没变、跳过的条数。 */
  skipped: number
  /** 这次跑了多少秒；还没跑完时是 null。 */
  duration_seconds: number | null
  /**
   * 失败原因；成功时是 null。
   *
   * ⚠ 后端刻意**不叫 error**：那会跟错误响应体顶层的 error 对象撞名。
   */
  error_message: string | null
}

/** `GET /designkit/prompts/sync/latest` 的返回。 */
export interface PromptSyncStatus {
  /** 最近一次同步；一次都没跑过时是 null。 */
  sync: PromptSyncRun | null
  /** 现在是不是正有一次在跑。**前端靠它决定要不要继续轮询。** */
  running: boolean
  prompt_count: number
  category_count: number
  /** 后端拼好的整句中文，直接显示。 */
  message: string
}

/** `POST /designkit/prompts/sync` 的返回（HTTP 202，同步在后台继续跑）。 */
export interface PromptSyncStarted {
  accepted: boolean
  /** 刚建好的那条「进行中」记录。 */
  sync: PromptSyncRun | null
  /** 去哪儿看进度（我们不用它拼路径，用 `getPromptSyncStatus()` 就行）。 */
  poll_url: string
  message: string
}

/**
 * 「同步走哪个代理」下拉框里的一项。
 *
 * ⚠ 字段名跟上游 `GET /api/v1/admin/proxies/all` 逐字对齐，
 * 就是为了能直接喂给上游的 `components/common/ProxySelector.vue`。
 * **没有 password**（上游那边也不下发）。
 */
export interface SyncProxyOption {
  id: number
  name: string
  protocol: string
  host: string
  port: number
  username: string
  status: string
  expires_at: string | null
  fallback_mode: string
  backup_proxy_id: number | null
  expiry_warn_days: number
  created_at: string
  updated_at: string
}

/** `GET | PUT /designkit/prompts/sync/settings` 的返回。 */
export interface PromptSyncSettings {
  /** 现在选的代理；null = 不走代理（直连）。 */
  proxy_id: number | null
  /** 选中那个代理的名字；没选时是空串。 */
  proxy_name: string
  /** 可选的代理，顺序即下拉框顺序。没有代理时是空数组。 */
  proxies: SyncProxyOption[]
  /** 后端拼好的整句中文，**必须显示**（它写着「只影响同步、不影响出图」）。 */
  message: string
}

// ============================================================================
// 浏览（所有登录用户）
// ============================================================================

/**
 * 分类列表（带每类条数）。
 *
 * 顺序即界面显示顺序，**不要在前端再排一遍**——排序是管理员在数据里定的。
 */
export async function listPromptCategories(signal?: AbortSignal): Promise<PromptCategoryList> {
  const { data } = await apiClient.get<PromptCategoryList>(
    `${DESIGNKIT_API_BASE_PATH}/prompt-categories`,
    { signal },
  )
  return {
    categories: data.categories ?? [],
    total_prompt_count: data.total_prompt_count ?? 0,
  }
}

/** 关键词长度上限，跟后端 `maxPromptKeywordRunes` 一致。超了后端会 400，**不会静默截断**。 */
export const MAX_PROMPT_KEYWORD_LENGTH = 64

/** 提示词列表的查询条件。 */
export interface ListPromptsQuery {
  /** 分类的 slug；不填 = 全部分类。 */
  category?: string
  /**
   * 不填 = 共享目录（全站一样）；`'user'` = 只看自己存的（「我的提示词」）。
   * 取值必须逐字是 `user`（后端只认这一个，拼错直接 400，不静默当成不填）。
   */
  source?: 'user'
  /** 关键词，标题和正文都搜；不填 = 不过滤。 */
  keyword?: string
  /** 上一页返回的 `next_cursor`；不填 = 第一页。**不透明字符串，不要解析。** */
  cursor?: string | null
  /** 每页条数，默认 20，最大 100。 */
  limit?: number
  signal?: AbortSignal
}

/** 拉一页提示词。**只返回还在架上的**（下架的不会出现在列表里）。 */
export async function listPrompts(query: ListPromptsQuery = {}): Promise<PromptPage> {
  const params: Record<string, string | number> = {}
  const category = (query.category ?? '').trim()
  const keyword = (query.keyword ?? '').trim()
  if (category !== '') {
    params.category = category
  }
  if (query.source) {
    params.source = query.source
  }
  if (keyword !== '') {
    params.keyword = keyword
  }
  if (typeof query.limit === 'number' && query.limit > 0) {
    params.limit = query.limit
  }
  if (query.cursor) {
    params.cursor = query.cursor
  }

  const { data } = await apiClient.get<PromptPage>(`${DESIGNKIT_API_BASE_PATH}/prompts`, {
    params,
    signal: query.signal,
  })
  return {
    // variables 后端保证是数组，这里再兜一次：模板里直接 v-for，null 会整页白屏。
    items: (data.items ?? []).map((item) => ({ ...item, variables: item.variables ?? [] })),
    total: data.total ?? 0,
    has_more: data.has_more === true,
    next_cursor: data.next_cursor ?? null,
  }
}

/**
 * 看一条提示词。
 *
 * **不过滤「已下架」**：历史收藏点进来仍然看得到，此时 `is_enabled` 是 false，
 * 界面要提示一句「这条已经下架」。
 */
export async function getPrompt(uid: string, signal?: AbortSignal): Promise<Prompt> {
  const { data } = await apiClient.get<Prompt>(
    `${DESIGNKIT_API_BASE_PATH}/prompts/${encodeURIComponent(uid)}`,
    { signal },
  )
  return { ...data, variables: data.variables ?? [] }
}

// ============================================================================
// 我的提示词（运营自建，所有登录用户）
//
// 读取沿用上面的 `listPrompts({ source: 'user' })` / `getPrompt()`；
// 这里只有三条写接口。规则都在后端（这里不重复校验，只把上限拿来做输入框的
// maxlength）：每人 200 条、标题 100 字、正文 5000 字、别人的词一律 404、
// youmind 来源的词不可改删（返回带中文说明的 400）。
// ============================================================================

/**
 * 「我的提示词」在分类栏里那个页签的**前端内部标记**。
 *
 * ⚠ 只在前端流转（选中态、网址 query），**绝不发给后端**——
 * 后端认的是 `source=user` 参数，这个值只是用来跟真实分类 slug 区分开。
 * 带双下划线是为了永远不会撞上 YouMind 的分类 slug。
 */
export const MY_PROMPTS_FILTER = '__mine__'

/** 每人最多存多少条。跟后端 `MaxUserPromptsPerUser` 一致。 */
export const MY_PROMPT_MAX_COUNT = 200
/** 标题字数上限。跟后端 `MaxUserPromptTitleRunes` 一致。 */
export const MY_PROMPT_TITLE_MAX = 100
/** 正文字数上限。跟后端 `MaxUserPromptBodyRunes` 一致。 */
export const MY_PROMPT_BODY_MAX = 5000

/** 存 / 改一条自己的提示词的输入。标题可以留空（卡片会拿正文开头顶上）。 */
export interface MyPromptInput {
  title: string
  body: string
}

/** 存一条到「我的提示词」。超上限时后端返回带中文文案的 400，原样显示即可。 */
export async function createMyPrompt(input: MyPromptInput): Promise<Prompt> {
  const { data } = await apiClient.post<Prompt>(`${DESIGNKIT_API_BASE_PATH}/prompts`, {
    title: (input.title ?? '').trim(),
    body: (input.body ?? '').trim(),
  })
  return { ...data, variables: data.variables ?? [] }
}

/** 改一条自己的（只有标题和正文能改）。别人的 / youmind 的会被后端拒绝。 */
export async function updateMyPrompt(uid: string, input: MyPromptInput): Promise<Prompt> {
  const { data } = await apiClient.put<Prompt>(
    `${DESIGNKIT_API_BASE_PATH}/prompts/${encodeURIComponent(uid)}`,
    {
      title: (input.title ?? '').trim(),
      body: (input.body ?? '').trim(),
    },
  )
  return { ...data, variables: data.variables ?? [] }
}

/** 删一条自己的（软删）。已出过的图和历史记录不受影响。 */
export async function deleteMyPrompt(uid: string): Promise<void> {
  await apiClient.delete(`${DESIGNKIT_API_BASE_PATH}/prompts/${encodeURIComponent(uid)}`)
}

/**
 * 这个错误是不是「后端还没上线『我的提示词』」。
 * 判据跟 `isSuggestUnavailableError()` 完全一致：裸 404/405 = 端点没挂；
 * 带 DK_ 错误码的是正经业务错误（越权、上限、youmind 不可改），不算没上线。
 */
export function isMyPromptUnavailableError(error: unknown): boolean {
  return isSuggestUnavailableError(error)
}

// ============================================================================
// AI 推荐提示词（所有登录用户）
//
// 运营不再自己翻一万多条提示词，而是：选个大致分类 → 写一句商品特点 →
// 后端拿着他上传的**商品图**去问对话模型，从该分类里挑 5 条，再合成**一条**
// 最终提示词。挑中的 5 条会一并返回，只是给运营看「凭什么这么推荐」，
// **不是要分别出 5 张图**——这一点在文案里也反复写了，别在调用方改成 5 条各出一张。
// ============================================================================

/**
 * 一次推荐要等多久。
 *
 * 后端一次要连着问三趟对话模型（判分类 → 挑 5 条 → 合成一条），实测十几秒，
 * 赶上模型慢的时候更久。而 axios 单例的默认超时只有 30 秒（`@/api/client`），
 * 照默认值走的结果是**后端还在算、前端已经报超时**：运营看到「等了很久没有反应」，
 * 再点一次又是一轮，白等两遍还多花一次问模型的钱。所以这里单独放宽到 2 分钟。
 *
 * ⚠ 放宽的只有这一个请求，不要去动 axios 单例的全局超时——那会让所有卡住的请求
 * 都转两分钟菊花，运营分不清「在算」和「坏了」。
 */
const SUGGEST_TIMEOUT_MS = 240_000

/** `POST /designkit/prompts/suggest` 的请求体。 */
export interface SuggestPromptRequest {
  /** 其余商品图的编号。整批一起给 AI 看，写出来的提示词才适合整批（后端封顶 3 张）。 */
  extra_asset_uids?: string[]
  /** 读哪张商品图。必填——推荐是**看着图**做的，没有图这件事根本做不了。 */
  asset_uid: string
  /**
   * 在哪个分类里挑，传分类的 **slug**（例如 `ecommerce-main-image`）。
   * **空串 = 全部分类**，此时后端会先让模型自己判一个分类出来。
   *
   * 不填不等于「不限」还得后端猜——它就是明确的「你替我判断」。
   *
   * ⚠ 分类**没有 id / uid 这种东西**，对外的唯一标识就是 slug
   * （后端 `handler/dto.go` 里写死了这一点，前端的 `PromptCategory` 也只有 slug）。
   * 从分类下拉框里拿到的 `category.slug` 原样传过来即可。
   */
  category_slug?: string
  /**
   * 运营填的「商品特点」，可空。
   *
   * 这就是原来那个「自己写一条」的输入框改成的东西：现在写的不是提示词，
   * 是**关于商品的事实**（材质、颜色、卖点、想要的感觉），由模型去写提示词。
   */
  features?: string
}

/** 推荐时参考到的一条灵感库提示词。**只用来展示依据**，不参与出图。 */
export interface SuggestPromptCandidate {
  uid: string
  /** 灵感库里的标题。可能是空串（上游有不少条只有正文），界面要兜一句占位文字。 */
  title: string
}

/** `POST /designkit/prompts/suggest` 的返回。 */
export interface SuggestPromptResponse {
  /** 合成出来的最终提示词全文。**出图用的就是这一条**，运营可以直接改。 */
  prompt: string
  /**
   * 最后落在哪个分类。运营选了「全部分类」时，这里就是模型判出来的那个，
   * 界面要把它显示出来（「AI 判断这是『电商主图』」），否则运营不知道推荐是从哪来的。
   *
   * `slug` 是分类的唯一标识（分类没有 id），`name` 是给人看的中文名。
   */
  category: { slug: string; name: string }
  /** 模型参考的那 5 条，顺序即界面显示顺序。没有时是空数组。 */
  candidates: SuggestPromptCandidate[]
  /**
   * 后端给运营的一句中文说明，可空（例如「这个分类下的提示词不多，结果可能偏泛」）。
   *
   * 这里做了归一：拿到的**永远是字符串**，模板里 `v-if="note"` 就够了，不必再判 undefined。
   */
  note?: string
}

/**
 * 让 AI 推荐一条提示词。**要等十几秒**，调用方必须给运营一个「正在想」的状态，
 * 并且在这期间禁掉按钮——重复点等于重复问模型，既慢又多花钱。
 *
 * `signal` 用来在运营切走页面 / 改了商品图时把这一次丢掉。
 *
 * 拿到结果之后还有两件事要判，两件都别省（各自的注释里写了不判的后果）：
 *   - 成功但没内容 → `isEmptySuggestion()` → `designkit.suggest.empty`
 *   - 抛错且是「功能没上线」 → `isSuggestUnavailableError()` → `designkit.suggest.unavailable`
 * 其余的错都走 `errorText()`，别自己拼英文。
 */
export async function suggestPrompt(
  req: SuggestPromptRequest,
  signal?: AbortSignal,
): Promise<SuggestPromptResponse> {
  const body = {
    asset_uid: (req.asset_uid ?? '').trim(),
      extra_asset_uids: req.extra_asset_uids ?? [],
    // 这两项**始终显式传**，不做「有值才带上」。少传一个字段，后端拿到的就是它自己的
    // 默认值，而默认值是什么前端说了不算——表现是「我明明选了分类，推出来却是别的类」，
    // 且界面上完全看不出哪一步出的岔子。同 `updatePromptSyncSettings` 的理由。
    category_slug: (req.category_slug ?? '').trim(),
    features: (req.features ?? '').trim(),
  }

  const { data } = await apiClient.post<SuggestPromptResponse>(
    `${DESIGNKIT_API_BASE_PATH}/prompts/suggest`,
    body,
    { timeout: SUGGEST_TIMEOUT_MS, signal },
  )

  return {
    prompt: data.prompt ?? '',
    // category 整个缺失也不能让模板炸，取值一律走兜底。
    category: { slug: data.category?.slug ?? '', name: data.category?.name ?? '' },
    // 后端保证是数组，这里再兜一次：模板里直接 v-for，null 会整页白屏（同 listPrompts）。
    candidates: (data.candidates ?? []).map((one) => ({
      uid: one?.uid ?? '',
      title: one?.title ?? '',
    })),
    note: data.note ?? '',
  }
}

/**
 * 这次推荐是不是「返回成功但没给出东西」。
 *
 * ⚠ 必须查，别省。后端返回 **HTTP 200 + 空 prompt** 是可能的（模型抽风、
 * 该分类下压根没几条可挑）。不查的话，界面会把一段空白当成推荐成功带进工作台，
 * 运营一路点到「提交出图」才发现提示词是空的——那时候已经在花钱了。
 * 查出来就显示 `designkit.suggest.empty`，让他改一下商品特点再来一次。
 */
export function isEmptySuggestion(result: SuggestPromptResponse): boolean {
  return (result.prompt ?? '').trim() === ''
}

/**
 * 这个错误是不是「后端还没上线这个功能」。
 *
 * 跟同步那几个端点一样，推荐端点没注册时请求打过去是**裸 404**（没有错误信封）。
 * 不认出来的话，运营点「让 AI 推荐」会看到「找不到这条记录」——他会以为是自己
 * 选的商品图出了问题，反复换图重试，怎么试都一样。所以这种情况要显示
 * `designkit.suggest.unavailable`（「还没准备好，可以先自己去灵感库挑」）。
 *
 * ⚠ 这里比 `isSyncUnavailableError()` 多一道判断，别照着那个抄：
 * 这个请求带着 `asset_uid`，**商品图找不到时后端也返回 404**，但那是一条正经的
 * 业务错误（信封里有 `DK_ASSET_NOT_FOUND` 之类的 `DK_` 错误码，且 message 是中文）。
 * 只看状态码会把「这张图没了，重新传一张」误报成「功能没上线」，运营就永远等不到
 * 那句该看的话了。所以：**带 DK_ 错误码的一律不算「没上线」**——能给出错误码，
 * 就说明这个端点在，而且它是有意这么答的。
 *
 * 为什么不像 `getUsageSummary()` 那样直接吞掉返回 null：那是页面加载时自己去拿的
 * 数据，藏起来就行；这里是运营**亲手点了按钮**，什么都不发生比报错更糟。
 */
export function isSuggestUnavailableError(error: unknown): boolean {
  const friendly = toFriendlyError(error)
  if (friendly.code !== null && friendly.code.startsWith('DK_')) {
    return false
  }
  return friendly.status === 404 || friendly.status === 405
}

// ============================================================================
// 同步（仅管理员）
// ============================================================================

/**
 * 手动同步一次。**立刻返回**（202），同步在后台跑十几秒。
 *
 * 已经有一次在跑时后端返回 409 `DK_SYNC_IN_PROGRESS`，
 * 中文是「灵感库正在同步，请等这一次跑完再点」——原样显示即可。
 */
export async function startPromptSync(): Promise<PromptSyncStarted> {
  const { data } = await apiClient.post<PromptSyncStarted>(
    `${DESIGNKIT_API_BASE_PATH}/prompts/sync`,
    // 后端不读请求体，但 axios 的 post 第二个参数省略会发 undefined，
    // 有些反代会把「没有 body 的 POST」拦掉，给个空对象最稳。
    {},
  )
  return data
}

/** 同步状态。轮询它看进度：`running` 为 true 就接着轮，变 false 就停。 */
export async function getPromptSyncStatus(signal?: AbortSignal): Promise<PromptSyncStatus> {
  const { data } = await apiClient.get<PromptSyncStatus>(
    `${DESIGNKIT_API_BASE_PATH}/prompts/sync/latest`,
    { signal },
  )
  return data
}

/** 同步走哪个代理（同时带回「能选谁」，下拉框两样都要）。 */
export async function getPromptSyncSettings(signal?: AbortSignal): Promise<PromptSyncSettings> {
  const { data } = await apiClient.get<PromptSyncSettings>(
    `${DESIGNKIT_API_BASE_PATH}/prompts/sync/settings`,
    { signal },
  )
  return { ...data, proxies: data.proxies ?? [] }
}

/**
 * 换一个代理。`null` = 不走代理（直连）。
 *
 * ⚠ `proxy_id` **必须显式出现在请求体里**，后端缺字段会 400 而不是当成 null——
 * 这是故意的：少传一个字段就悄悄把代理清掉的话，表现是「昨天还好好的，今天拉不动了」。
 * 所以这里写死 `{ proxy_id: proxyID }`，不要改成「有值才带上」。
 */
export async function updatePromptSyncSettings(
  proxyID: number | null,
): Promise<PromptSyncSettings> {
  const { data } = await apiClient.put<PromptSyncSettings>(
    `${DESIGNKIT_API_BASE_PATH}/prompts/sync/settings`,
    { proxy_id: proxyID },
  )
  return { ...data, proxies: data.proxies ?? [] }
}

/**
 * 这个错误是不是「后端根本没装同步功能」。
 *
 * 后端的同步服务缺席时，管理员那四个端点**整组不注册**，请求打过去是 404
 * （不是错误信封）。这种情况要显示「同步功能还没准备好，请联系管理员」，
 * 而不是「找不到这条记录」——后者会让管理员以为是自己点错了地方。
 */
export function isSyncUnavailableError(error: unknown): boolean {
  const status = toFriendlyError(error).status
  return status === 404 || status === 405
}

// ============================================================================
// 占位符替换
// ============================================================================

/**
 * 把正文里的 `{占位符}` 换成运营填的值。
 *
 * 规则（跟后端 `service.ConvertPrompt` 生成的形态对应）：
 *   - 占位符就是 `{变量名}`，变量名来自 `prompt.variables[].name`；
 *   - **填了才换**：留空的原样保留 `{变量名}`，运营带到工作台还能改。
 *     不换成空串，是因为「柔和的{光线}」变成「柔和的」之后运营看不出少了东西，
 *     而出的图会莫名其妙。
 *   - 只替换认识的变量名，正文里别的花括号原样保留。
 */
export function applyPromptVariables(body: string, values: Record<string, string>): string {
  let out = body
  for (const [name, raw] of Object.entries(values)) {
    const value = (raw ?? '').trim()
    if (value === '') {
      continue
    }
    // 变量名由后端清洗过（只有小写字母、数字、下划线、汉字），不含正则元字符，
    // 但仍然用 split/join 而不是 RegExp，省掉转义这件事。
    out = out.split(`{${name}}`).join(value)
  }
  return out
}

/** 正文里还有几处没填。界面据此提醒一句，不阻止运营带走。 */
export function countUnfilledVariables(
  body: string,
  variables: PromptVariable[],
  values: Record<string, string>,
): number {
  return variables.filter((one) => {
    if ((values[one.name] ?? '').trim() !== '') {
      return false
    }
    return body.includes(`{${one.name}}`)
  }).length
}

// ============================================================================
// 喂给上游的代理下拉框
// ============================================================================

const PROXY_PROTOCOLS: readonly string[] = ['http', 'https', 'socks5', 'socks5h']
const PROXY_STATUSES: readonly string[] = ['active', 'inactive', 'expired']
const PROXY_FALLBACK_MODES: readonly string[] = ['none', 'proxy', 'direct']

/**
 * 把后端给的代理选项转成上游 `ProxySelector.vue` 要的 `Proxy`。
 *
 * 两边字段名已经逐字对齐，这里只做两件事：
 *   1. 把几个「后端是 string、上游类型是字面量联合」的字段收进合法取值
 *      （上游以后加了新协议、而我们这份名单没跟上时，不至于让整页编译不过）；
 *   2. 空串的 username 还原成 null（上游那边是 `string | null`）。
 *
 * **不要在这里补 password**：后端不下发，下拉框也用不到。
 */
export function toUpstreamProxy(option: SyncProxyOption): Proxy {
  const protocol = PROXY_PROTOCOLS.includes(option.protocol) ? option.protocol : 'http'
  const status = PROXY_STATUSES.includes(option.status) ? option.status : 'inactive'
  const fallback = PROXY_FALLBACK_MODES.includes(option.fallback_mode)
    ? option.fallback_mode
    : 'none'
  return {
    id: option.id,
    name: option.name,
    protocol: protocol as ProxyProtocol,
    host: option.host,
    port: option.port,
    username: option.username === '' ? null : option.username,
    status: status as Proxy['status'],
    expires_at: option.expires_at,
    fallback_mode: fallback as Proxy['fallback_mode'],
    backup_proxy_id: option.backup_proxy_id,
    expiry_warn_days: option.expiry_warn_days,
    created_at: option.created_at,
    updated_at: option.updated_at,
  }
}
