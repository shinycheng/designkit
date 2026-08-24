/**
 * 第二步「AI 配提示词」的单测。守的是文件头写明的对外契约：
 *  - v-model 吐出去的 string[] 永远 ≤1 条（张数按它算，多一条就是多一张图的钱）；
 *  - 带换行的提示词不被切碎成多条；
 *  - 第一次推荐不带 force（允许命中缓存），「重新推荐」带 force；
 *  - 命中缓存时显示来源说明（不说的话运营以为「怎么点都一样」是坏了）。
 */
import { beforeEach, describe, expect, it, vi } from 'vitest'
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

const api = vi.hoisted(() => ({
  suggestPrompt: vi.fn(),
  listPromptCategories: vi.fn(),
  createMyPrompt: vi.fn(),
}))

vi.mock('@/features/designkit/api', () => ({
  suggestPrompt: api.suggestPrompt,
  listPromptCategories: api.listPromptCategories,
  createMyPrompt: api.createMyPrompt,
  isEmptySuggestion: (result: { prompt?: string } | null | undefined) =>
    (result?.prompt ?? '').trim() === '',
  isSuggestUnavailableError: () => false,
  toFriendlyError: (error: unknown) => ({ message: String(error) }),
}))

vi.mock('@/stores', () => ({
  useAppStore: () => ({ showSuccess: vi.fn(), showError: vi.fn() }),
}))

vi.mock('@/utils/format', () => ({
  formatRelativeTime: () => '3分钟前',
}))

import PromptEditor from '../components/workbench/PromptEditor.vue'

function suggestResult(over: Record<string, unknown> = {}) {
  return {
    prompt: '简约场景\n柔和自然光\n无文字无水印',
    candidates: [
      { uid: 'p1', title: 'Minimal scene' },
      { uid: 'p2', title: '' },
    ],
    category: { slug: 'main', name: '电商主图' },
    note: '',
    cached_at: '',
    ...over,
  }
}

function mountEditor() {
  api.listPromptCategories.mockResolvedValue({
    categories: [{ slug: 'main', name: '电商主图' }],
  })
  return mount(PromptEditor, {
    props: {
      modelValue: [],
      assetCount: 2,
      firstAssetUid: 'asset-1',
      extraAssetUids: ['asset-2'],
    },
  })
}

/** 点「让 AI 推荐 / 重新推荐」。 */
async function clickSuggest(wrapper: ReturnType<typeof mountEditor>) {
  const button = wrapper
    .findAll('button')
    .find(
      (b) =>
        b.text().includes('designkit.suggest.button') ||
        b.text().includes('designkit.suggest.again'),
    )
  expect(button).toBeTruthy()
  await button!.trigger('click')
  await flushPromises()
}

describe('PromptEditor', () => {
  beforeEach(() => {
    api.suggestPrompt.mockReset()
    api.listPromptCategories.mockReset()
    api.createMyPrompt.mockReset()
  })

  it('推荐结果带换行也只吐 1 条，不被切碎成多条（多一条 = 多一张图的钱）', async () => {
    api.suggestPrompt.mockResolvedValue(suggestResult())
    const wrapper = mountEditor()
    await flushPromises()
    await clickSuggest(wrapper)

    const emitted = wrapper.emitted('update:modelValue')
    expect(emitted).toBeTruthy()
    const last = emitted!.at(-1)![0] as string[]
    expect(last).toHaveLength(1)
    expect(last[0]).toBe('简约场景\n柔和自然光\n无文字无水印')
  })

  it('运营在结果框里改成多行文字，吐出去的仍然是 1 条；清空则是 0 条', async () => {
    api.suggestPrompt.mockResolvedValue(suggestResult())
    const wrapper = mountEditor()
    await flushPromises()
    await clickSuggest(wrapper)

    const textarea = wrapper.get<HTMLTextAreaElement>('#designkit-suggest-prompt')
    await textarea.setValue('第一行\n\n第三行\n第四行')
    let last = wrapper.emitted('update:modelValue')!.at(-1)![0] as string[]
    expect(last).toHaveLength(1)
    expect(last[0]).toBe('第一行\n\n第三行\n第四行')

    await textarea.setValue('   ')
    last = wrapper.emitted('update:modelValue')!.at(-1)![0] as string[]
    expect(last).toHaveLength(0)
  })

  it('第一次推荐不带 force（允许命中缓存），「重新推荐」带 force', async () => {
    api.suggestPrompt.mockResolvedValue(suggestResult())
    const wrapper = mountEditor()
    await flushPromises()

    await clickSuggest(wrapper)
    expect(api.suggestPrompt).toHaveBeenCalledTimes(1)
    expect(api.suggestPrompt.mock.calls[0][0]).toMatchObject({
      asset_uid: 'asset-1',
      extra_asset_uids: ['asset-2'],
      force: false,
    })
    // 有结果之后按钮换成「重新推荐」
    expect(wrapper.text()).toContain('designkit.suggest.again')

    await clickSuggest(wrapper)
    expect(api.suggestPrompt).toHaveBeenCalledTimes(2)
    expect(api.suggestPrompt.mock.calls[1][0]).toMatchObject({ force: true })
  })

  it('命中后端缓存时显示「这是 X 的推荐结果」；新鲜结果不显示', async () => {
    api.suggestPrompt.mockResolvedValue(
      suggestResult({ cached_at: '2026-08-24T00:00:00Z' }),
    )
    const wrapper = mountEditor()
    await flushPromises()
    await clickSuggest(wrapper)
    expect(wrapper.text()).toContain('designkit.suggest.cachedNote(time=3分钟前)')

    api.suggestPrompt.mockResolvedValue(suggestResult({ cached_at: '' }))
    const fresh = mountEditor()
    await flushPromises()
    await clickSuggest(fresh)
    expect(fresh.text()).not.toContain('designkit.suggest.cachedNote')
  })

  it('外部把 modelValue 清空（提交成功后重置）时，结果区跟着清掉', async () => {
    api.suggestPrompt.mockResolvedValue(suggestResult())
    const wrapper = mountEditor()
    await flushPromises()
    await clickSuggest(wrapper)
    expect(wrapper.find('#designkit-suggest-prompt').exists()).toBe(true)

    await wrapper.setProps({ modelValue: [] })
    await flushPromises()
    expect(wrapper.find('#designkit-suggest-prompt').exists()).toBe(false)
  })

  it('没传商品图时推荐按钮禁用并说明原因', async () => {
    api.suggestPrompt.mockResolvedValue(suggestResult())
    api.listPromptCategories.mockResolvedValue({ categories: [] })
    const wrapper = mount(PromptEditor, {
      props: { modelValue: [], assetCount: 0, firstAssetUid: '', extraAssetUids: [] },
    })
    await flushPromises()
    const button = wrapper
      .findAll('button')
      .find((b) => b.text().includes('designkit.suggest.button'))
    expect(button!.attributes()).toHaveProperty('disabled')
    expect(wrapper.text()).toContain('designkit.suggest.needAsset')
  })
})
