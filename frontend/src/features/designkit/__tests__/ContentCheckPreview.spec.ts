/**
 * 文案检查页标红预览的单测。预览块是 v-html，守的就是那个顺序：
 * **先转义、再包 <mark>**——反了就是给任何能改文案的人开 XSS 口子。
 * 另外守码点对齐（emoji 不错位）、重叠命中合并、平台字数条。
 */
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { defineComponent } from 'vue'
import { mount, flushPromises } from '@vue/test-utils'

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

// 上游整页布局跟本测试无关，换成透传插槽的壳
vi.mock('@/components/layout/AppLayout.vue', () => ({
  default: defineComponent({ name: 'AppLayout', template: '<div><slot /></div>' }),
}))

// 只替掉真发请求的 checkContent，平台表等常量用真的
vi.mock('@/features/designkit/api', async (importOriginal) => {
  const actual = await importOriginal<typeof import('../api')>()
  return { ...actual, checkContent: vi.fn() }
})

import DesignkitContentCheckView from '../views/DesignkitContentCheckView.vue'
import { checkContent } from '../api'
import type { ContentCheckResult } from '../api'

const checkContentMock = vi.mocked(checkContent)

function resultOf(over: Partial<ContentCheckResult> = {}): ContentCheckResult {
  return { hits: [], title_len: 0, title_max: 0, platform_name: '', ...over }
}

/** 输入文字并点「检查」按钮（绕开 500ms 防抖，直接出结果）。 */
async function typeAndCheck(wrapper: ReturnType<typeof mount>, text: string) {
  await wrapper.get('textarea').setValue(text)
  const check = wrapper
    .findAll('button')
    .find((b) => b.text().includes('designkit.contentCheck.check'))
  await check!.trigger('click')
  await flushPromises()
}

describe('DesignkitContentCheckView 标红预览', () => {
  beforeEach(() => {
    vi.useFakeTimers()
    checkContentMock.mockReset()
  })

  afterEach(() => {
    vi.clearAllTimers()
    vi.useRealTimers()
  })

  it('输入含 <script> 时预览是转义文本，不产生任何脚本节点（XSS）', async () => {
    const text = '全网最低<script>alert(1)</script>'
    checkContentMock.mockResolvedValue(
      resultOf({ hits: [{ word: '全网最低', start: 0, end: 4 }] }),
    )
    const wrapper = mount(DesignkitContentCheckView)
    await typeAndCheck(wrapper, text)

    const preview = wrapper.get('.dk-cc-preview')
    // 没有真实的 script 元素，标签以转义文本出现
    expect(preview.element.querySelector('script')).toBeNull()
    expect(preview.element.innerHTML).toContain('&lt;script&gt;alert(1)&lt;/script&gt;')
    // 命中的那段确实包了 mark
    const marks = preview.element.querySelectorAll('mark')
    expect(marks).toHaveLength(1)
    expect(marks[0].textContent).toBe('全网最低')
  })

  it('命中下标按码点对齐：前面有 emoji 也不错位', async () => {
    // '🚀🚀国家级产品'：码点下标 2..5 恰好是「国家级」；按 UTF-16 slice 会错两位
    checkContentMock.mockResolvedValue(
      resultOf({ hits: [{ word: '国家级', start: 2, end: 5 }] }),
    )
    const wrapper = mount(DesignkitContentCheckView)
    await typeAndCheck(wrapper, '🚀🚀国家级产品')

    const mark = wrapper.get('.dk-cc-preview').element.querySelector('mark')
    expect(mark?.textContent).toBe('国家级')
  })

  it('部分重叠的命中合并成一段标红，不产生嵌套或断裂的 mark', async () => {
    checkContentMock.mockResolvedValue(
      resultOf({
        hits: [
          { word: '全网最低', start: 0, end: 4 },
          { word: '最低价', start: 2, end: 5 },
        ],
      }),
    )
    const wrapper = mount(DesignkitContentCheckView)
    await typeAndCheck(wrapper, '全网最低价')

    const marks = wrapper.get('.dk-cc-preview').element.querySelectorAll('mark')
    expect(marks).toHaveLength(1)
    expect(marks[0].textContent).toBe('全网最低价')
    // 汇总区两个词都在
    expect(wrapper.text()).toContain('全网最低')
    expect(wrapper.text()).toContain('最低价')
  })

  it('选了平台后字数条实时显示超出量（本地算，不等接口）', async () => {
    const wrapper = mount(DesignkitContentCheckView)
    // 淘宝上限 30 字；输入 35 个字
    await wrapper.get('select').setValue('taobao')
    await wrapper.get('textarea').setValue('好'.repeat(35))

    expect(wrapper.text()).toContain('designkit.contentCheck.titleLen(len=35,max=30)')
    expect(wrapper.text()).toContain('designkit.contentCheck.titleOver(n=5)')
    expect(wrapper.find('.dk-cc-count--over').exists()).toBe(true)
  })
})
