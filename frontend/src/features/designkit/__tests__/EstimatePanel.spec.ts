/**
 * 第四步（核对花费并提交）的单测。守的是「按下按钮之前知道要花多少钱」：
 *  - 张数公式 = 商品图 × 提示词；
 *  - 金额一律带 $（决策 18）；
 *  - 价格待确认时绝不显示 $0（会被读成免费）；
 *  - 余额不够必须给「申请额度」出路（决策 19）；
 *  - 超过单次上限提前拦住提交。
 */
import { describe, expect, it, vi } from 'vitest'
import { mount } from '@vue/test-utils'

// 组件里的 t() 原样返回键名并把参数拼在后面，这样能同时断言「用了哪条文案」
// 和「数字代进去了没有」。
vi.mock('vue-i18n', async () => {
  const actual = await vi.importActual<typeof import('vue-i18n')>('vue-i18n')
  return {
    ...actual,
    useI18n: () => ({
      locale: { value: 'zh' },
      t: (key: string, params?: Record<string, unknown>) =>
        params
          ? `${key}(${Object.entries(params)
              .map(([k, v]) => `${k}=${v}`)
              .join(',')})`
          : key,
    }),
  }
})

// formatPrice 在「价格待确认」时经 @/i18n 取占位文案；原样返回键名，
// 断言「没有 $」就能确认待确认分支没退化成金额。
vi.mock('@/i18n', () => ({
  i18n: { global: { t: (key: string) => key } },
  getLocale: () => 'zh',
}))

import EstimatePanel from '../components/workbench/EstimatePanel.vue'
import { PRICE_UNCONFIRMED, confirmedPrice } from '../api/money'
import type { JobEstimate } from '../api/types'

function estimateOf(over: Partial<JobEstimate> = {}): JobEstimate {
  return {
    item_count: 6,
    asset_count: 3,
    prompt_count: 2,
    max_batch_items: 50,
    pricing_tier: '2K',
    price_confirmed: true,
    unit_price: confirmedPrice('1'),
    estimated_cost: confirmedPrice('6'),
    currency: 'USD',
    balance: '10',
    available: '8',
    sufficient: true,
    shortfall: '0',
    ...over,
  }
}

function mountPanel(over: Record<string, unknown> = {}) {
  return mount(EstimatePanel, {
    props: {
      assetCount: 3,
      promptCount: 2,
      name: '',
      estimate: estimateOf(),
      priceNote: '',
      loading: false,
      loadError: null,
      submitting: false,
      submitError: null,
      ...over,
    },
  })
}

describe('EstimatePanel', () => {
  it('张数公式 = 商品图 × 提示词，报价没回来之前本地就能算', () => {
    const wrapper = mountPanel({ estimate: null, loading: true })
    expect(wrapper.text()).toContain(
      'designkit.workbench.summaryFormula(assets=3,prompts=2,total=6)',
    )
  })

  it('缺一头时一次只提示一件事：先图后词', () => {
    const noAsset = mountPanel({ assetCount: 0, promptCount: 2, estimate: null })
    expect(noAsset.text()).toContain('designkit.workbench.needAsset')
    expect(noAsset.text()).not.toContain('designkit.workbench.needPrompt')

    const noPrompt = mountPanel({ assetCount: 3, promptCount: 0, estimate: null })
    expect(noPrompt.text()).toContain('designkit.workbench.needPrompt')
  })

  it('价格已确认时单价和总价都带 $，余额可用额也带 $', () => {
    const wrapper = mountPanel()
    const text = wrapper.text()
    expect(text).toContain('designkit.estimate.unitPrice(price=$1.00)')
    expect(text).toContain('designkit.estimate.estimatedCost(amount=$6.00)')
    expect(text).toContain('designkit.estimate.balance(amount=$10.00)')
    expect(text).toContain('designkit.estimate.available(amount=$8.00)')
  })

  it('价格待确认：显示说明文字，绝不显示 $0，也不判「余额不够」', () => {
    const wrapper = mountPanel({
      estimate: estimateOf({
        price_confirmed: false,
        unit_price: PRICE_UNCONFIRMED,
        estimated_cost: PRICE_UNCONFIRMED,
        // 后端此时一律给 true；就算给 false 也不许显示「不够」——那是猜的
        sufficient: false,
      }),
    })
    const text = wrapper.text()
    expect(text).toContain('designkit.money.priceUnconfirmed')
    expect(text).toContain('designkit.estimate.unconfirmed')
    expect(text).not.toContain('$0')
    expect(text).not.toContain('designkit.estimate.insufficient')

    // 后端给了整句说明时显示后端那句，不再用兜底键
    const withNote = mountPanel({
      estimate: estimateOf({ price_confirmed: false, unit_price: PRICE_UNCONFIRMED, estimated_cost: PRICE_UNCONFIRMED }),
      priceNote: '该比例还没实测',
    })
    expect(withNote.text()).toContain('该比例还没实测')
    expect(withNote.text()).not.toContain('designkit.estimate.unconfirmed')
  })

  it('余额不够：显示还差多少（带 $），「申请额度」按钮直达 requestQuota', async () => {
    const wrapper = mountPanel({
      estimate: estimateOf({ sufficient: false, shortfall: '2.5' }),
    })
    expect(wrapper.text()).toContain('designkit.estimate.insufficient(amount=$2.50)')
    const quota = wrapper
      .findAll('button')
      .find((b) => b.text().includes('designkit.quota.request'))
    expect(quota).toBeTruthy()
    await quota!.trigger('click')
    expect(wrapper.emitted('requestQuota')).toHaveLength(1)
  })

  it('超过单次上限：显示上限与当前张数，提交按钮禁用', () => {
    const wrapper = mountPanel({
      estimate: estimateOf({ item_count: 60, max_batch_items: 50 }),
    })
    expect(wrapper.text()).toContain('designkit.estimate.overLimit(max=50,count=60)')
    const submit = wrapper
      .findAll('button')
      .find((b) => b.text().includes('designkit.workbench.submit'))
    expect(submit!.attributes()).toHaveProperty('disabled')
  })

  it('正常情况下提交可点并发出 submit；报价还在算时不可点', async () => {
    const wrapper = mountPanel()
    const submit = wrapper
      .findAll('button')
      .find((b) => b.text().includes('designkit.workbench.submit'))
    expect(submit!.attributes()).not.toHaveProperty('disabled')
    await submit!.trigger('click')
    expect(wrapper.emitted('submit')).toHaveLength(1)

    const loading = mountPanel({ loading: true })
    const loadingSubmit = loading
      .findAll('button')
      .find((b) => b.text().includes('designkit.workbench.submit'))
    expect(loadingSubmit!.attributes()).toHaveProperty('disabled')
  })
})
