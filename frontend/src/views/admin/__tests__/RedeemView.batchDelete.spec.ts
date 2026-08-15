import { beforeEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'

import RedeemView from '../RedeemView.vue'

/**
 * 兑换码「批量删除」。
 *
 * 后端 POST /admin/redeem-codes/batch-delete 一直就有（「删除全部未使用」用的
 * 也是它），缺的只是界面上的入口 —— monica 2026-08-14：「现在只能一个个删除」。
 *
 * 这里钉三件坏了不会当场报错、只会静默出事的：
 *   1. 真的把**选中的那些 id** 发出去（发错等于删错东西，而且找不回来）；
 *   2. 提示语报的是后端**真实删掉的条数**，不是选中数 ——
 *      后端可能少删（有的已被别人删了、有的已使用不让删），
 *      报选中数会让管理员以为都删干净了；
 *   3. 删完**清空选中集合** —— 那个集合是跨页保留的，不清的话已经删掉的 id
 *      还留在里面，接着点「批量修改」会对着一批不存在的 id 发请求。
 */

const {
  listRedeemCodes,
  batchDeleteRedeemCodes,
  batchUpdateRedeemCodes,
  getAllGroups,
  showSuccess,
  showError,
  showInfo,
} = vi.hoisted(() => ({
  listRedeemCodes: vi.fn(),
  batchDeleteRedeemCodes: vi.fn(),
  batchUpdateRedeemCodes: vi.fn(),
  getAllGroups: vi.fn(),
  showSuccess: vi.fn(),
  showError: vi.fn(),
  showInfo: vi.fn(),
}))

vi.mock('@/api/admin', () => ({
  adminAPI: {
    redeem: {
      list: listRedeemCodes,
      generate: vi.fn(),
      delete: vi.fn(),
      batchDelete: batchDeleteRedeemCodes,
      batchUpdate: batchUpdateRedeemCodes,
      exportCodes: vi.fn(),
    },
    groups: {
      getAll: getAllGroups,
    },
  },
}))

vi.mock('@/stores/app', () => ({
  useAppStore: () => ({ showSuccess, showError, showInfo }),
}))

vi.mock('@/composables/useClipboard', () => ({
  useClipboard: () => ({ copyToClipboard: vi.fn() }),
}))

vi.mock('vue-i18n', async () => {
  const actual = await vi.importActual<typeof import('vue-i18n')>('vue-i18n')
  return {
    ...actual,
    useI18n: () => ({ t: (key: string) => key }),
  }
})

const DataTableStub = {
  props: ['columns', 'data'],
  template: `
    <table>
      <thead>
        <tr>
          <th v-for="column in columns" :key="column.key">
            <slot :name="'header-' + column.key" :column="column">{{ column.label }}</slot>
          </th>
        </tr>
      </thead>
      <tbody>
        <tr v-for="row in data" :key="row.id">
          <td v-for="column in columns" :key="column.key">
            <slot :name="'cell-' + column.key" :row="row" :value="row[column.key]">
              {{ row[column.key] }}
            </slot>
          </td>
        </tr>
      </tbody>
    </table>
  `,
}

/**
 * 确认弹窗的替身：直接把 confirm 事件暴露成一个按钮。
 *
 * 真的 ConfirmDialog 用 Teleport 渲染到 body 上，在测试里不好点。
 * 这里只关心「点了确认之后发生什么」，弹窗本身的行为上游已经有测试。
 */
const ConfirmDialogStub = {
  props: ['show'],
  emits: ['confirm', 'cancel'],
  template: `<button v-if="show" data-test="stub-confirm" @click="$emit('confirm')">confirm</button>`,
}

function mountView() {
  return mount(RedeemView, {
    attachTo: document.body,
    global: {
      stubs: {
        AppLayout: { template: '<div><slot /></div>' },
        TablePageLayout: {
          template:
            '<div><slot name="filters" /><slot name="table" /><slot name="pagination" /></div>',
        },
        DataTable: DataTableStub,
        Pagination: true,
        ConfirmDialog: ConfirmDialogStub,
        Select: true,
        GroupBadge: true,
        GroupOptionItem: true,
        Icon: true,
        Teleport: true,
      },
    },
  })
}

describe('admin RedeemView batch delete', () => {
  beforeEach(() => {
    localStorage.clear()
    document.body.innerHTML = ''

    listRedeemCodes.mockReset()
    batchDeleteRedeemCodes.mockReset()
    batchUpdateRedeemCodes.mockReset()
    getAllGroups.mockReset()
    showSuccess.mockReset()
    showError.mockReset()
    showInfo.mockReset()

    listRedeemCodes.mockResolvedValue({
      items: [
        {
          id: 1,
          code: 'CODE-1',
          type: 'balance',
          value: 10,
          status: 'unused',
          used_by: null,
          used_at: null,
          created_at: '2026-01-01T00:00:00Z',
          expires_at: null,
        },
        {
          id: 2,
          code: 'CODE-2',
          type: 'balance',
          value: 20,
          status: 'unused',
          used_by: null,
          used_at: null,
          created_at: '2026-01-01T00:00:00Z',
          expires_at: null,
        },
      ],
      total: 2,
      page: 1,
      page_size: 20,
      pages: 1,
    })
    getAllGroups.mockResolvedValue([])
  })

  it('把选中的那些 id 发给 batchDelete，并按真实删掉的条数提示', async () => {
    // 选了 2 个，后端只删掉 1 个（另一个已经被别人删了）。
    batchDeleteRedeemCodes.mockResolvedValue({ deleted: 1, message: 'ok' })

    const wrapper = mountView()
    await flushPromises()

    const checkboxes = wrapper.findAll('[data-test="select-code"]')
    await checkboxes[0].setValue(true)
    await checkboxes[1].setValue(true)

    await wrapper.get('[data-test="batch-delete-open"]').trigger('click')
    await flushPromises()
    await wrapper.get('[data-test="stub-confirm"]').trigger('click')
    await flushPromises()

    expect(batchDeleteRedeemCodes).toHaveBeenCalledWith([1, 2])
    // ⚠ 报的是后端返回的 deleted(1)，不是选中数(2)。
    expect(showSuccess).toHaveBeenCalledWith('admin.redeem.batchDeleted')
    // 删完要重新拉列表（第一次是挂载时拉的）。
    expect(listRedeemCodes.mock.calls.length).toBeGreaterThan(1)
  })

  it('删完清空选中集合，否则已删的 id 还会被下一次批量操作带上', async () => {
    batchDeleteRedeemCodes.mockResolvedValue({ deleted: 1, message: 'ok' })

    const wrapper = mountView()
    await flushPromises()

    await wrapper.findAll('[data-test="select-code"]')[0].setValue(true)
    await wrapper.get('[data-test="batch-delete-open"]').trigger('click')
    await flushPromises()
    await wrapper.get('[data-test="stub-confirm"]').trigger('click')
    await flushPromises()

    // 选中数归零之后，那条「已选择 N 个」的浮动栏就不该再出现了。
    expect(wrapper.find('[data-test="batch-delete"]').exists()).toBe(false)
  })

  it('一个都没选时点不动（按钮禁用）', async () => {
    const wrapper = mountView()
    await flushPromises()

    const button = wrapper.get('[data-test="batch-delete-open"]')
    expect(button.attributes('disabled')).toBeDefined()

    await button.trigger('click')
    await flushPromises()
    expect(batchDeleteRedeemCodes).not.toHaveBeenCalled()
  })

  it('后端报错时提示失败，且不清空选中（让管理员能重试）', async () => {
    batchDeleteRedeemCodes.mockRejectedValue(new Error('boom'))

    const wrapper = mountView()
    await flushPromises()

    await wrapper.findAll('[data-test="select-code"]')[0].setValue(true)
    await wrapper.get('[data-test="batch-delete-open"]').trigger('click')
    await flushPromises()
    await wrapper.get('[data-test="stub-confirm"]').trigger('click')
    await flushPromises()

    expect(showError).toHaveBeenCalled()
    expect(showSuccess).not.toHaveBeenCalled()
    // 选中还在 —— 失败之后清空的话，管理员得重新勾一遍。
    expect(wrapper.find('[data-test="batch-delete"]').exists()).toBe(true)
  })
})
