/**
 * 「重试这一张」二次确认的单测（决策 20）。
 *
 * 守的是「重试 = 重新收一次钱」这句必须带着价钱说出来：
 *  - 有单价就把本次要花的 $ 金额写进正文；
 *  - 价格待确认时明说按实际扣费，绝不显示 $0；
 *  - 上次是超时的那张要多一条警告（钱可能已经花了）。
 */
import { describe, expect, it, vi } from 'vitest'
import { defineComponent } from 'vue'
import { mount } from '@vue/test-utils'

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

// formatPrice 的「待确认」占位文案经 @/i18n 取；原样返回键名即可。
vi.mock('@/i18n', () => ({
  i18n: { global: { t: (key: string) => key } },
  getLocale: () => 'zh',
}))

import RetryItemDialog from '../components/workbench/RetryItemDialog.vue'
import { PRICE_UNCONFIRMED, confirmedPrice } from '../api/money'

const DialogStub = defineComponent({
  props: ['show', 'title', 'width'],
  emits: ['close'],
  template: '<div v-if="show" data-test="dialog"><header>{{ title }}</header><slot /><slot name="footer" /></div>',
})

function mountDialog(over: Record<string, unknown> = {}) {
  return mount(RetryItemDialog, {
    props: {
      show: true,
      seq: 3,
      unitPrice: confirmedPrice('1'),
      attemptCount: 1,
      maxAttempts: 3,
      wasTimeout: false,
      working: false,
      ...over,
    },
    global: { stubs: { BaseDialog: DialogStub } },
  })
}

describe('RetryItemDialog', () => {
  it('有单价时正文带着 $ 标价，并显示第几张、第几次、上限', () => {
    const wrapper = mountDialog()
    const text = wrapper.text()
    expect(text).toContain('designkit.retry.body(price=$1.00)')
    expect(text).toContain('designkit.job.seq(seq=3)')
    expect(text).toContain('designkit.job.attempt(n=1)')
    expect(text).toContain('designkit.job.attemptLimit(max=3)')
    // seq=0 表示不显示序号
    const noSeq = mountDialog({ seq: 0 })
    expect(noSeq.text()).not.toContain('designkit.job.seq')
  })

  it('价格待确认时换成「按实际扣费」的那句，绝不显示 $0', () => {
    const wrapper = mountDialog({ unitPrice: PRICE_UNCONFIRMED })
    expect(wrapper.text()).toContain('designkit.retry.bodyUnconfirmed')
    expect(wrapper.text()).not.toContain('designkit.retry.body(')
    expect(wrapper.text()).not.toContain('$0')
  })

  it('上次是超时的那张要多一条「可能已扣费」警告，其余情况没有', () => {
    const timeout = mountDialog({ wasTimeout: true })
    expect(timeout.text()).toContain('designkit.retry.timeoutWarning')
    const normal = mountDialog({ wasTimeout: false })
    expect(normal.text()).not.toContain('designkit.retry.timeoutWarning')
  })

  it('确认 / 先不试各走各的事件；working 时都禁用', async () => {
    const wrapper = mountDialog()
    const confirm = wrapper
      .findAll('button')
      .find((b) => b.text().includes('designkit.retry.confirm'))
    const cancel = wrapper
      .findAll('button')
      .find((b) => b.text().includes('designkit.retry.cancel'))
    await confirm!.trigger('click')
    expect(wrapper.emitted('confirm')).toHaveLength(1)
    await cancel!.trigger('click')
    expect(wrapper.emitted('cancel')).toHaveLength(1)

    const busy = mountDialog({ working: true })
    for (const b of busy.findAll('button')) {
      expect(b.attributes()).toHaveProperty('disabled')
    }
  })
})
