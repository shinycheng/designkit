<!--
  额度申请（**仅管理员**，决策 19 的闭环）。

  运营在工作台点「申请额度」之后，记录落在这里等着处理：
  「通过」填一个金额，**真的会给申请人加余额**；「驳回」只标记，不动钱。

  三件事，改这一页前先读一遍：

  1. **金额是字符串，不走浮点。** 输入框原文 trim 之后原样发给后端
     （后端只收十进制字符串），前端只拦「不是数字 / 不大于 0」，
     真正的护栏在后端，改网页绕不过去。

  2. **同一条申请可能被另一位管理员先处理。** 后端那条 WHERE status='pending'
     守卫会返回冲突，这里把它当普通错误弹出来（文案后端给的是中文），
     然后照常刷新列表——行会自己消失。

  3. **处理完要同步侧边栏红点**（stores/quotaBadge.ts）：列表接口每次都带
     pending_count，拿它调 setDesignkitQuotaPendingCount，不另发请求。
-->
<template>
  <AppLayout>
    <div class="dk-quota-page">
      <header class="dk-canvas-header">
        <div class="dk-page-head">
          <p class="dk-eyebrow">{{ t('designkit.brand') }}</p>
          <h1 class="dk-page-title">{{ t('designkit.quotaAdmin.title') }}</h1>
          <p class="dk-page-desc">{{ t('designkit.quotaAdmin.description') }}</p>
        </div>
      </header>

      <!-- 后端整组端点没挂（404）。申请记录都在表里，不丢。 -->
      <div v-if="unavailable" class="dk-alert dk-alert--warning">
        <p class="dk-text-strong">{{ t('designkit.quotaAdmin.unavailable') }}</p>
        <p class="dk-alert__sub">{{ t('designkit.quotaAdmin.unavailableHint') }}</p>
      </div>

      <template v-else>
        <!-- 两个 tab：待处理（带条数）/ 已处理 -->
        <div class="dk-quota-tabs" role="tablist">
          <button
            type="button"
            role="tab"
            class="dk-button dk-button--sm"
            :class="{ 'dk-button--secondary': activeTab !== 'pending' }"
            :aria-selected="activeTab === 'pending'"
            @click="switchTab('pending')"
          >
            {{ t('designkit.quotaAdmin.pending') }}
            <span v-if="pendingCount > 0" class="dk-count-pill">{{ pendingCount }}</span>
          </button>
          <button
            type="button"
            role="tab"
            class="dk-button dk-button--sm"
            :class="{ 'dk-button--secondary': activeTab !== 'handled' }"
            :aria-selected="activeTab === 'handled'"
            @click="switchTab('handled')"
          >
            {{ t('designkit.quotaAdmin.handled') }}
          </button>
        </div>

        <!-- 第一次加载 -->
        <p v-if="loading && items === null" class="dk-muted">
          {{ t('designkit.common.loading') }}
        </p>

        <!-- 读失败 -->
        <div v-else-if="loadError && items === null" class="dk-alert dk-alert--danger">
          <p>{{ t('designkit.quotaAdmin.loadFailed') }}</p>
          <p class="dk-alert__sub">{{ loadError.message }}</p>
          <p v-if="loadError.requestId" class="dk-alert__sub">
            {{ t('designkit.errors.requestId', { id: loadError.requestId }) }}
          </p>
          <button
            type="button"
            class="dk-button dk-button--secondary dk-button--sm dk-alert__action"
            @click="reload()"
          >
            {{ t('designkit.common.retry') }}
          </button>
        </div>

        <!-- 空列表 -->
        <div v-else-if="items !== null && items.length === 0" class="dk-empty dk-empty--boxed">
          <p class="dk-empty__title">
            {{ activeTab === 'pending' ? t('designkit.quotaAdmin.empty') : t('designkit.common.empty') }}
          </p>
        </div>

        <!-- 列表 -->
        <div v-else-if="items !== null" class="dk-stack">
          <section v-for="item in items" :key="item.id" class="dk-panel dk-quota-row">
            <!-- 申请本身 -->
            <div class="dk-quota-row__info">
              <p class="dk-text-strong">
                {{ item.requester_email || t('designkit.quotaAdmin.requesterDeleted') }}
              </p>
              <p v-if="item.note" class="dk-text dk-quota-row__note">
                {{ t('designkit.quotaAdmin.requestNote') }}：{{ item.note }}
              </p>
              <p class="dk-muted">
                {{ t('designkit.quotaAdmin.requestedAt') }}
                {{ formatDateTimeToMinute(item.created_at) }}
              </p>
            </div>

            <!-- 待处理：通过（金额）/ 驳回（备注） -->
            <div v-if="item.status === 'pending'" class="dk-quota-row__actions">
              <div class="dk-quota-row__fields">
                <div class="dk-quota-row__field">
                  <label class="dk-label" :for="`dk-quota-amount-${item.id}`">
                    {{ t('designkit.quotaAdmin.amountLabel') }}
                  </label>
                  <input
                    :id="`dk-quota-amount-${item.id}`"
                    v-model="draftOf(item.id).amount"
                    type="text"
                    inputmode="decimal"
                    class="dk-input"
                    autocomplete="off"
                    :placeholder="t('designkit.quotaAdmin.amountPlaceholder')"
                    :disabled="draftOf(item.id).submitting"
                  />
                  <p v-if="draftOf(item.id).amountError" class="dk-note dk-note--danger dk-mt-1">
                    {{ draftOf(item.id).amountError }}
                  </p>
                </div>
                <div class="dk-quota-row__field">
                  <label class="dk-label" :for="`dk-quota-note-${item.id}`">
                    {{ t('designkit.quotaAdmin.noteLabel') }}
                  </label>
                  <input
                    :id="`dk-quota-note-${item.id}`"
                    v-model="draftOf(item.id).note"
                    type="text"
                    class="dk-input"
                    autocomplete="off"
                    maxlength="200"
                    :disabled="draftOf(item.id).submitting"
                  />
                </div>
              </div>
              <div class="dk-quota-row__buttons">
                <button
                  type="button"
                  class="dk-button dk-button--sm"
                  :disabled="draftOf(item.id).submitting"
                  @click="approve(item)"
                >
                  {{ t('designkit.quotaAdmin.approve') }}
                </button>
                <button
                  type="button"
                  class="dk-button dk-button--danger dk-button--sm"
                  :disabled="draftOf(item.id).submitting"
                  @click="reject(item)"
                >
                  {{ t('designkit.quotaAdmin.reject') }}
                </button>
              </div>
            </div>

            <!-- 已处理：结果 + 处理详情 -->
            <div v-else class="dk-quota-row__result">
              <p>
                <span
                  class="dk-badge"
                  :class="item.status === 'handled' ? 'dk-badge--success' : 'dk-badge--warning'"
                >
                  {{
                    item.status === 'handled'
                      ? t('designkit.quotaAdmin.approved')
                      : t('designkit.quotaAdmin.rejected')
                  }}
                </span>
                <span v-if="item.approved_amount" class="dk-text-strong dk-quota-row__amount">
                  +{{ formatMoney(item.approved_amount) }}
                </span>
              </p>
              <p v-if="item.handle_note" class="dk-text dk-quota-row__note">
                {{ t('designkit.quotaAdmin.noteLabel') }}：{{ item.handle_note }}
              </p>
              <p class="dk-muted">
                <template v-if="item.handled_by_email">
                  {{ t('designkit.quotaAdmin.handledBy') }} {{ item.handled_by_email }} ·
                </template>
                {{ t('designkit.quotaAdmin.handledAt') }}
                {{ formatDateTimeToMinute(item.handled_at ?? undefined) }}
              </p>
            </div>
          </section>
        </div>
      </template>
    </div>
  </AppLayout>
</template>

<script setup lang="ts">
import { onMounted, onUnmounted, reactive, ref } from 'vue'
import { useI18n } from 'vue-i18n'
import AppLayout from '@/components/layout/AppLayout.vue'
import { useAppStore } from '@/stores'
import { formatDateTimeToMinute } from '@/utils/format'
import {
  approveQuotaRequest,
  errorText,
  formatMoney,
  isCanceledError,
  isQuotaAdminUnavailableStatus,
  listQuotaRequests,
  rejectQuotaRequest,
  toFriendlyError,
} from '../api'
import type { FriendlyError, QuotaRequestAdminItem } from '../api'
import { setDesignkitQuotaPendingCount } from '../stores/quotaBadge'
// 页面用到的通用样式（卡片、按钮、输入框…），全局引入，Vite 会去重。
import '../components/designkit-ui.css'

const { t } = useI18n()
const appStore = useAppStore()

type Tab = 'pending' | 'handled'

const activeTab = ref<Tab>('pending')
/** 当前 tab 的行；null = 还没读回来（跟空列表区分开）。 */
const items = ref<QuotaRequestAdminItem[] | null>(null)
/** 全部待处理条数（列表接口每次都带，跟当前 tab 无关）。 */
const pendingCount = ref(0)
const loading = ref(false)
const loadError = ref<FriendlyError | null>(null)
/** 后端整组端点没挂（404）。不是故障，申请记录都在表里。 */
const unavailable = ref(false)

let controller: AbortController | null = null
let disposed = false

/** 每一行「通过/驳回」的输入。行处理完从列表消失，残留的草稿无害。 */
interface RowDraft {
  amount: string
  note: string
  amountError: string
  submitting: boolean
}
const drafts = reactive<Record<number, RowDraft>>({})

function draftOf(id: number): RowDraft {
  if (!drafts[id]) {
    drafts[id] = { amount: '', note: '', amountError: '', submitting: false }
  }
  return drafts[id]
}

// ---------------------------------------------------------------------------
// 读
// ---------------------------------------------------------------------------

async function reload(): Promise<void> {
  controller?.abort()
  controller = new AbortController()
  loading.value = true
  loadError.value = null
  try {
    const list = await listQuotaRequests(activeTab.value, undefined, controller.signal)
    if (disposed) {
      return
    }
    items.value = list.items
    pendingCount.value = list.pending_count
    // 顺手同步侧边栏红点，不另发请求。
    setDesignkitQuotaPendingCount(list.pending_count)
    unavailable.value = false
  } catch (error) {
    if (isCanceledError(error) || disposed) {
      return
    }
    const friendly = toFriendlyError(error)
    if (isQuotaAdminUnavailableStatus(friendly.status)) {
      unavailable.value = true
      return
    }
    loadError.value = friendly
  } finally {
    if (!disposed) {
      loading.value = false
    }
  }
}

function switchTab(tab: Tab): void {
  if (activeTab.value === tab) {
    return
  }
  activeTab.value = tab
  // 换 tab 先清掉旧行，避免闪一下上一个 tab 的内容。
  items.value = null
  void reload()
}

// ---------------------------------------------------------------------------
// 处理
// ---------------------------------------------------------------------------

/** 金额校验：非数字、0、负数都拦下。真正的护栏在后端。 */
function validAmount(raw: string): boolean {
  const value = Number(raw.trim())
  return raw.trim() !== '' && Number.isFinite(value) && value > 0
}

async function approve(item: QuotaRequestAdminItem): Promise<void> {
  const draft = draftOf(item.id)
  draft.amountError = ''
  if (!validAmount(draft.amount)) {
    draft.amountError = t('designkit.quotaAdmin.amountInvalid')
    return
  }
  draft.submitting = true
  try {
    await approveQuotaRequest(item.id, draft.amount, draft.note)
    appStore.showSuccess(t('designkit.quotaAdmin.approved'))
    await reload()
  } catch (error) {
    // 后端的中文原样显示（含「已被另一位管理员处理」那类冲突）；
    // 英文形状的错误统一换成「没处理成功」。冲突时行已经不是 pending 了，
    // 刷新一次让它自己消失。
    appStore.showError(friendlyHandleError(error))
    await reload()
  } finally {
    draft.submitting = false
  }
}

async function reject(item: QuotaRequestAdminItem): Promise<void> {
  const draft = draftOf(item.id)
  draft.submitting = true
  try {
    await rejectQuotaRequest(item.id, draft.note)
    appStore.showSuccess(t('designkit.quotaAdmin.rejected'))
    await reload()
  } catch (error) {
    appStore.showError(friendlyHandleError(error))
    await reload()
  } finally {
    draft.submitting = false
  }
}

function friendlyHandleError(error: unknown): string {
  const friendly = toFriendlyError(error)
  if (friendly.kind === 'unknown' || friendly.kind === 'network') {
    return t('designkit.quotaAdmin.handleFailed')
  }
  return errorText(error)
}

onMounted(() => {
  void reload()
})

onUnmounted(() => {
  disposed = true
  controller?.abort()
})
</script>

<style scoped>
.dk-quota-page {
  display: flex;
  flex-direction: column;
  gap: var(--dk-space-4);
}

.dk-quota-tabs {
  display: flex;
  gap: var(--dk-space-2);
}

.dk-quota-row {
  display: flex;
  flex-wrap: wrap;
  gap: var(--dk-space-4);
  align-items: flex-start;
  justify-content: space-between;
}

.dk-quota-row__info {
  flex: 1 1 16rem;
  min-width: 0;
  display: flex;
  flex-direction: column;
  gap: var(--dk-space-1);
}

.dk-quota-row__note {
  overflow-wrap: anywhere;
}

.dk-quota-row__actions {
  flex: 1 1 20rem;
  display: flex;
  flex-direction: column;
  gap: var(--dk-space-2);
}

.dk-quota-row__fields {
  display: flex;
  flex-wrap: wrap;
  gap: var(--dk-space-2);
}

.dk-quota-row__field {
  flex: 1 1 9rem;
  min-width: 0;
}

.dk-quota-row__buttons {
  display: flex;
  gap: var(--dk-space-2);
  justify-content: flex-end;
}

.dk-quota-row__result {
  flex: 1 1 16rem;
  display: flex;
  flex-direction: column;
  gap: var(--dk-space-1);
}

.dk-quota-row__amount {
  margin-left: var(--dk-space-2);
}
</style>
