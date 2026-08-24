/**
 * 「停止排队」二次确认的单测（决策 21）。
 *
 * 守的是两段语义一个都不能少：还没开始的 N 张不再生成、不扣费；
 * 正在生成的 M 张会跑完并照常扣费。两个数字必须真的代进文案。
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

import StopJobDialog from '../components/workbench/StopJobDialog.vue'

const DialogStub = defineComponent({
  props: ['show', 'title', 'width'],
  emits: ['close'],
  template: '<div v-if="show" data-test="dialog"><header>{{ title }}</header><slot /><slot name="footer" /></div>',
})

function mountDialog(over: Record<string, unknown> = {}) {
  return mount(StopJobDialog, {
    props: { show: true, pendingCount: 7, runningCount: 2, working: false, ...over },
    global: { stubs: { BaseDialog: DialogStub } },
  })
}

describe('StopJobDialog', () => {
  it('两段语义的数字代入正文：未开始 7 张 / 正在生成 2 张，并带扣费说明', () => {
    const wrapper = mountDialog()
    expect(wrapper.text()).toContain('designkit.stop.body(pending=7,running=2)')
    // 「已开始的会跑完并正常扣费」那句不能省
    expect(wrapper.text()).toContain('designkit.job.cancelledCostNote')
  })

  it('确认走 confirm，关掉弹窗的按钮是「继续出图」走 cancel——不叫「取消」', async () => {
    const wrapper = mountDialog()
    const confirm = wrapper
      .findAll('button')
      .find((b) => b.text().includes('designkit.stop.confirm'))
    const keep = wrapper
      .findAll('button')
      .find((b) => b.text().includes('designkit.stop.cancel'))
    expect(confirm).toBeTruthy()
    expect(keep).toBeTruthy()
    await confirm!.trigger('click')
    expect(wrapper.emitted('confirm')).toHaveLength(1)
    await keep!.trigger('click')
    expect(wrapper.emitted('cancel')).toHaveLength(1)
  })

  it('working 时两个按钮都禁用，确认键显示加载中', () => {
    const wrapper = mountDialog({ working: true })
    const buttons = wrapper.findAll('button')
    expect(buttons.length).toBeGreaterThan(0)
    for (const b of buttons) {
      expect(b.attributes()).toHaveProperty('disabled')
    }
    expect(wrapper.text()).toContain('designkit.common.loading')
  })
})
