/**
 * 金额的显示与「价格待确认」的处理。
 *
 * 三条铁律：
 *  1. **金额一律美元，界面渲染 `$`**（决策 18，不做汇率换算）。
 *     任何文案里都不许出现「元」——底层余额和账单全是美元，
 *     换算会导致「工作台说还差 12.3 元、个人资料显示 $12.30」这种充错钱的事。
 *  2. 后端把金额传成**字符串**，别 `parseFloat` 之后拿去算钱再显示，
 *     0.1+0.2 会变成 0.30000000000000004。只在显示的最后一步转数字。
 *  3. **「价格待确认」不是 0。** 价目表要真出一张图才知道落哪一档
 *     （设计定型 第十节 1）。测出来之前一律显示「价格待确认」，
 *     显示成 $0.00 会让运营以为免费，一口气排几十张。
 */

import { i18n } from '@/i18n'
import type { Money, Price, PriceConfirmed, PriceUnconfirmed } from './types'

/** 价格待确认的单例，省得到处 new 对象。 */
export const PRICE_UNCONFIRMED: PriceUnconfirmed = Object.freeze({ confirmed: false } as const)

/** 金额缺失时的占位符（余额、实际花费这类「还没有值」的地方）。 */
export const MONEY_PLACEHOLDER = '—'

/**
 * 把后端的 `string | null` 金额转成 Price。
 *
 * null、空串、以及任何解析不出数字的值，一律当成「价格待确认」——
 * 宁可显示「待确认」也不要显示一个猜的数字。
 */
export function toPrice(amount: string | null | undefined): Price {
  if (typeof amount !== 'string') {
    return PRICE_UNCONFIRMED
  }
  const trimmed = amount.trim()
  if (trimmed === '' || !Number.isFinite(Number(trimmed))) {
    return PRICE_UNCONFIRMED
  }
  return { confirmed: true, amount: trimmed }
}

/** 造一个「已确认」的价格。主要给测试和界面上的小计用。 */
export function confirmedPrice(amount: Money): PriceConfirmed {
  return { confirmed: true, amount }
}

/** 类型守卫，方便在模板里 `v-if`。 */
export function isPriceConfirmed(price: Price): price is PriceConfirmed {
  return price.confirmed
}

/**
 * 金额 → 界面文字，例如 `$12.30`。
 *
 * 小于 1 分的单价会多显示两位（`$0.0125`），否则一张图的单价会被四舍五入成
 * `$0.01` 甚至 `$0.00`，运营会以为不要钱。
 *
 * @param amount   后端给的金额字符串；null / 空 / 非数字返回 fallback
 * @param fallback 没有金额时显示什么，默认 `—`
 */
export function formatMoney(amount: Money | null | undefined, fallback: string = MONEY_PLACEHOLDER): string {
  if (typeof amount !== 'string') {
    return fallback
  }
  const trimmed = amount.trim()
  // ⚠ Number('') 是 0 不是 NaN：空串不拦在这里就会显示成 $0.00（= 被读成免费）。
  if (trimmed === '' || !Number.isFinite(Number(trimmed))) {
    return fallback
  }
  const value = Number(trimmed)
  const twoDecimals = value.toFixed(2)
  const fourDecimals = value.toFixed(4)
  if (Number(twoDecimals) === Number(fourDecimals)) {
    return `$${twoDecimals}`
  }
  return `$${fourDecimals.replace(/0+$/, '')}`
}

/**
 * 价格 → 界面文字。待确认时返回「价格待确认」（可用 unconfirmedText 覆盖）。
 *
 * **这是显示单价 / 预估花费的唯一入口**，不要自己写 `price.confirmed ? ... : ...`。
 */
export function formatPrice(price: Price, unconfirmedText?: string): string {
  if (price.confirmed) {
    return formatMoney(price.amount)
  }
  return unconfirmedText ?? i18n.global.t('designkit.money.priceUnconfirmed')
}

/**
 * 单价 × 张数 = 小计。任何一头不确定就返回「待确认」。
 *
 * 用途：确认弹窗里的「N 张 × 单价 = 合计」。真正扣钱的数以后端报价为准，
 * 这里只是给人看的。
 */
export function multiplyPrice(unitPrice: Price, count: number): Price {
  if (!unitPrice.confirmed || !Number.isFinite(count) || count < 0) {
    return PRICE_UNCONFIRMED
  }
  const total = Number(unitPrice.amount) * count
  if (!Number.isFinite(total)) {
    return PRICE_UNCONFIRMED
  }
  // 保留 6 位再交给 formatMoney 收敛，避免二进制浮点尾巴（0.1 * 3 = 0.30000000000000004）。
  return { confirmed: true, amount: total.toFixed(6) }
}

/** 金额是不是大于 0（用来判断「要不要显示花费」）。解析不出来当成 0。 */
export function isPositiveMoney(amount: Money | null | undefined): boolean {
  if (typeof amount !== 'string') {
    return false
  }
  const value = Number(amount.trim())
  return Number.isFinite(value) && value > 0
}
