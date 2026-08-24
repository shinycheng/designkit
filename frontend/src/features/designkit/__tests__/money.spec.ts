/**
 * 金额显示的单测。这里守的都是「会让运营白花钱」的行为：
 *  - 金额一律渲染 `$`（决策 18）；
 *  - 「价格待确认」绝不能退化成 $0.00（会被读成「免费」）；
 *  - 单价 × 张数不能出现二进制浮点尾巴。
 */
import { describe, expect, it, vi } from 'vitest'

// money.ts 只在「价格待确认」时用 i18n 取占位文案；这里让它原样返回键名，
// 断言就能同时确认「走的是待确认分支」和「没有渲染成金额」。
vi.mock('@/i18n', () => ({
  i18n: { global: { t: (key: string) => key } },
  getLocale: () => 'zh',
}))

import {
  MONEY_PLACEHOLDER,
  PRICE_UNCONFIRMED,
  confirmedPrice,
  formatMoney,
  formatPrice,
  isPositiveMoney,
  multiplyPrice,
  toPrice,
} from '../api/money'

describe('formatMoney', () => {
  it('一律渲染 $ 前缀、默认两位小数', () => {
    expect(formatMoney('12.3')).toBe('$12.30')
    expect(formatMoney('1')).toBe('$1.00')
    expect(formatMoney('0')).toBe('$0.00')
  })

  it('小于 1 分的单价保留四位，不被抹成 $0.00', () => {
    expect(formatMoney('0.0125')).toBe('$0.0125')
    // 尾零裁掉，但不会裁回两位以内
    expect(formatMoney('0.015')).toBe('$0.015')
  })

  it('null / 空串 / 非数字显示占位符，不显示 $NaN', () => {
    expect(formatMoney(null)).toBe(MONEY_PLACEHOLDER)
    expect(formatMoney(undefined)).toBe(MONEY_PLACEHOLDER)
    expect(formatMoney('')).toBe(MONEY_PLACEHOLDER)
    expect(formatMoney('abc')).toBe(MONEY_PLACEHOLDER)
    expect(formatMoney('abc', '待定')).toBe('待定')
  })
})

describe('toPrice / formatPrice', () => {
  it('null、空串、解析不出的值一律归成「价格待确认」，不猜数字', () => {
    expect(toPrice(null).confirmed).toBe(false)
    expect(toPrice(undefined).confirmed).toBe(false)
    expect(toPrice('').confirmed).toBe(false)
    expect(toPrice('  ').confirmed).toBe(false)
    expect(toPrice('n/a').confirmed).toBe(false)
    expect(toPrice('1.5')).toEqual({ confirmed: true, amount: '1.5' })
  })

  it('待确认的价格显示占位文案，绝不显示 $0 或任何 $ 金额', () => {
    const text = formatPrice(PRICE_UNCONFIRMED)
    expect(text).toBe('designkit.money.priceUnconfirmed')
    expect(text).not.toContain('$')
    expect(formatPrice(PRICE_UNCONFIRMED, '待确认')).toBe('待确认')
    // 已确认的走正常金额
    expect(formatPrice(confirmedPrice('1'))).toBe('$1.00')
  })
})

describe('multiplyPrice', () => {
  it('单价 × 张数不出现浮点尾巴（0.1 × 3 = $0.30）', () => {
    const total = multiplyPrice(confirmedPrice('0.1'), 3)
    expect(total.confirmed).toBe(true)
    expect(formatPrice(total)).toBe('$0.30')
  })

  it('任何一头不确定（待确认 / 负数张数）结果就是待确认', () => {
    expect(multiplyPrice(PRICE_UNCONFIRMED, 3).confirmed).toBe(false)
    expect(multiplyPrice(confirmedPrice('1'), -1).confirmed).toBe(false)
    expect(multiplyPrice(confirmedPrice('1'), Number.NaN).confirmed).toBe(false)
  })
})

describe('isPositiveMoney', () => {
  it('只有解析得出且大于 0 才算有花费', () => {
    expect(isPositiveMoney('0.01')).toBe(true)
    expect(isPositiveMoney('0')).toBe(false)
    expect(isPositiveMoney('-1')).toBe(false)
    expect(isPositiveMoney(null)).toBe(false)
    expect(isPositiveMoney('abc')).toBe(false)
  })
})
