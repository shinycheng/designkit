/**
 * 「商品图设置」的接口封装。**只有管理员调得动**（后端挂着管理员判定）。
 *
 * 走上游 axios 单例（见 `./index.ts` 开头那三条坑），所以路径只写
 * `/designkit/admin/settings`，实际请求是 `/api/v1/designkit/admin/settings`。
 *
 *   GET  /designkit/admin/settings   读全部配置 + 允许范围 + 价格预览
 *   PUT  /designkit/admin/settings   保存（**只传改过的项**，没传的后端不动）
 *
 * 三件事，改这个文件前先读一遍：
 *
 *  1. **只传改过的项。** 后端是「传了才改」的语义，所以 `buildPatch()` 会把
 *     表单和服务端原值逐项比对，只把真正变了的字段放进请求体。
 *     整份提交的话，任何一次前端漏字段都会把那一项悄悄清空。
 *
 *  2. **单价的空值是空字符串，不是 0。** 没测出来的档在表单里就是空输入框，
 *     提交时原样传空串，后端会把它从配置里剔掉 → 工作台显示「价格待确认」。
 *     千万不要在这里 `Number(...) || 0` —— 那会把「待确认」变成「免费」。
 *
 *  3. **允许范围由后端下发**（`limits`），前端不要另写一份。
 *     两边对不上的话，会出现「界面让填、保存被拒」这种谁也说不清的情况。
 */

import { apiClient } from '@/api/client'
import { DESIGNKIT_API_BASE_PATH } from './paths'

// ============================================================================
// 类型（字段名与后端 handler/settings_handler.go 逐字一致，一律 snake_case）
// ============================================================================

/** 每一项的允许范围。输入框的 min/max 和提示语都用它，**不要在前端另写一份**。 */
export interface DesignkitSettingsLimits {
  max_batch_items_min: number
  max_batch_items_max: number
  item_timeout_seconds_min: number
  item_timeout_seconds_max: number
  max_dimension_min: number
  max_dimension_max: number
  worker_concurrency_min: number
  worker_concurrency_max: number
  /** 超过这个数就会跟自己在界面上的单张操作抢并发（后端不拦，只提醒）。 */
  worker_concurrency_safe: number
  ratios_max: number
  model_max_length: number
  admin_contact_max_length: number
  /** 单价表有哪几档，从便宜到贵，例如 ['1K','2K','4K']。 */
  tiers: string[]
  unit_price_max: string
  rate_multiplier_min: string
  rate_multiplier_max: string
}

/** 灵感库同步走哪个代理。**这一页只读**，改要去灵感库那一页。 */
export interface DesignkitSettingsPromptSync {
  proxy_id: number | null
  proxy_name: string
  /** 后端拼好的整句中文，里面写着「只影响同步，不影响出图」，必须原样显示。 */
  message: string
  /** 恒为 false。留着是为了以后真要在这一页开放编辑时不用改接口。 */
  editable: boolean
  /** 去哪儿改（`/inspiration`）。 */
  edit_path: string
}

/** 全部配置。 */
export interface DesignkitSettings {
  ratios: string[]
  model: string
  max_batch_items: number
  item_timeout_seconds: number
  max_dimension: number
  worker_concurrency: number
  admin_contact: string
  /**
   * 计费档 → 单价（美元，十进制字符串）。
   * **只包含已经填了的档**；没填的档根本不在这个对象里 = 价格待确认。
   */
  unit_prices: Record<string, string>
  /** 空串 = 没配过，按 1 算（不加价不打折）。 */
  rate_multiplier: string
  prompt_sync: DesignkitSettingsPromptSync
  limits: DesignkitSettingsLimits
  /** 保存成功、但有话要说（例如并发数超过建议值）。GET 时是空数组。 */
  warnings: string[]
  /** 改了「要重启才完全生效」的项时，后端给的整句中文；否则是空串。 */
  restart_note: string
}

/** 一次保存要提交的内容。**只放改过的项。** */
export interface DesignkitSettingsPatch {
  ratios?: string[]
  model?: string
  max_batch_items?: number
  item_timeout_seconds?: number
  max_dimension?: number
  worker_concurrency?: number
  admin_contact?: string
  /** 整份替换。空串的档表示「还没测出来」，后端会把它剔掉。 */
  unit_prices?: Record<string, string>
  rate_multiplier?: string
}

/** 设置页表单的形状。单价和倍率是**字符串**，空串 = 没填。 */
export interface DesignkitSettingsForm {
  ratios: string[]
  model: string
  max_batch_items: number
  item_timeout_seconds: number
  max_dimension: number
  worker_concurrency: number
  admin_contact: string
  /** 档位 → 输入框里的原文。空串 = 这一档还没填。 */
  unit_prices: Record<string, string>
  /** 输入框里的原文。空串 = 没填，按 1 算。 */
  rate_multiplier: string
}

// ============================================================================
// 请求
// ============================================================================

const SETTINGS_PATH = `${DESIGNKIT_API_BASE_PATH}/admin/settings`

/** 读全部配置。 */
export async function getDesignkitSettings(signal?: AbortSignal): Promise<DesignkitSettings> {
  const { data } = await apiClient.get<DesignkitSettings>(SETTINGS_PATH, { signal })
  return normalizeSettings(data)
}

/**
 * 保存。`patch` 里只放改过的项 —— 没放的项后端**不会动**。
 *
 * 返回的是保存之后的完整配置（含警告和「要不要重启」那句话），
 * 直接拿它覆盖页面状态即可，不用再 GET 一次。
 */
export async function updateDesignkitSettings(
  patch: DesignkitSettingsPatch,
): Promise<DesignkitSettings> {
  const { data } = await apiClient.put<DesignkitSettings>(SETTINGS_PATH, patch)
  return normalizeSettings(data)
}

/**
 * 后端整组端点没挂时是 404（不是错误信封）。
 *
 * 什么时候会这样：数据库没连上，配置服务整块没装配。
 * 这时要显示「这个功能还没准备好」，而不是「找不到这条记录」——
 * 后者会让管理员以为自己点错了地方。
 */
export function isSettingsUnavailableStatus(status: number | null): boolean {
  return status === 404 || status === 405
}

// ============================================================================
// 表单 ⇄ 接口
// ============================================================================

/** 把接口返回的配置转成表单的形状（每一档单价都要有一个输入框，包括没填的）。 */
export function toSettingsForm(settings: DesignkitSettings): DesignkitSettingsForm {
  const prices: Record<string, string> = {}
  for (const tier of settings.limits.tiers) {
    // 没填的档在 unit_prices 里根本不存在 → 输入框留空。
    // **不要写 0**：0 会被运营读成「免费」。
    prices[tier] = settings.unit_prices[tier] ?? ''
  }
  return {
    ratios: [...settings.ratios],
    model: settings.model,
    max_batch_items: settings.max_batch_items,
    item_timeout_seconds: settings.item_timeout_seconds,
    max_dimension: settings.max_dimension,
    worker_concurrency: settings.worker_concurrency,
    admin_contact: settings.admin_contact,
    unit_prices: prices,
    rate_multiplier: settings.rate_multiplier,
  }
}

/**
 * 逐项比对表单和服务端原值，只把**真正变了**的字段放进请求体。
 *
 * 一项没变都返回 `{}`，调用方据此提示「没有要保存的内容」而不是白发一次请求。
 */
export function buildSettingsPatch(
  form: DesignkitSettingsForm,
  server: DesignkitSettings,
): DesignkitSettingsPatch {
  const patch: DesignkitSettingsPatch = {}

  if (!sameStringList(form.ratios, server.ratios)) {
    patch.ratios = [...form.ratios]
  }
  if (form.model.trim() !== server.model.trim()) {
    patch.model = form.model.trim()
  }
  if (form.max_batch_items !== server.max_batch_items) {
    patch.max_batch_items = form.max_batch_items
  }
  if (form.item_timeout_seconds !== server.item_timeout_seconds) {
    patch.item_timeout_seconds = form.item_timeout_seconds
  }
  if (form.max_dimension !== server.max_dimension) {
    patch.max_dimension = form.max_dimension
  }
  if (form.worker_concurrency !== server.worker_concurrency) {
    patch.worker_concurrency = form.worker_concurrency
  }
  if (form.admin_contact.trim() !== server.admin_contact.trim()) {
    patch.admin_contact = form.admin_contact.trim()
  }
  if (!sameUnitPrices(form.unit_prices, server.unit_prices, server.limits.tiers)) {
    // 整份替换。空串原样传上去，后端会把那一档剔掉（= 价格待确认）。
    const prices: Record<string, string> = {}
    for (const tier of server.limits.tiers) {
      prices[tier] = (form.unit_prices[tier] ?? '').trim()
    }
    patch.unit_prices = prices
  }
  if (form.rate_multiplier.trim() !== server.rate_multiplier.trim()) {
    patch.rate_multiplier = form.rate_multiplier.trim()
  }

  return patch
}

/** 表单和服务端原值完全一样吗（有没有未保存的改动）。 */
export function hasSettingsChanges(
  form: DesignkitSettingsForm,
  server: DesignkitSettings,
): boolean {
  return Object.keys(buildSettingsPatch(form, server)).length > 0
}

function sameStringList(a: string[], b: string[]): boolean {
  return a.length === b.length && a.every((value, index) => value === b[index])
}

function sameUnitPrices(
  form: Record<string, string>,
  server: Record<string, string>,
  tiers: string[],
): boolean {
  return tiers.every((tier) => (form[tier] ?? '').trim() === (server[tier] ?? '').trim())
}

/**
 * 把后端返回的东西补齐成一个「字段都在」的对象。
 *
 * 后端已经保证 map / 数组不为 null，这里只是兜底：万一哪天接口先上、字段后加，
 * 页面不该整个白屏。
 */
function normalizeSettings(data: DesignkitSettings): DesignkitSettings {
  return {
    ...data,
    ratios: data.ratios ?? [],
    unit_prices: data.unit_prices ?? {},
    rate_multiplier: data.rate_multiplier ?? '',
    warnings: data.warnings ?? [],
    restart_note: data.restart_note ?? '',
    limits: {
      ...data.limits,
      tiers: data.limits?.tiers ?? [],
    },
  }
}
