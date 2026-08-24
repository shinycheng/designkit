<!--
  用户记录（**仅管理员**）：所有账户的对话记录和出图记录，只读。

  两个 tab（对话记录 / 出图记录）+ 一个账户筛选下拉，列表 50 条一页
  「加载更多」翻页，点「查看」进详情、「返回列表」回来。不做复杂分页。

  三件事，改这一页前先读一遍：

  1. **这里只有用户自己没删过的记录。** 管理员视角跟用户视角一致，
     后端已把软删的过滤掉——列表条数对不上总量是正常的，不是丢数据。

  2. **对话里带图的消息显示真缩略图**，走管理员专用的
     /admin/records/assets/:uid/content（跨用户读，门是 RequireAdmin）——
     用户态的 /designkit/assets/:uid/content 只放素材主人过，管理员的登录态
     去取别人的图会 404，别换回去。取失败（素材被主人删了、存储故障）
     回落「[附图]」占位文字，不给重试按钮——回放场景里图取不到就是取不到。
     出图记录的缩略图走 /admin/records/jobs/:uid/items/:seq/content。

  3. **气泡样式是从 ChatMessageList.vue 抄来的**（类名保持一致）。
     那边的样式是 scoped 的，跨组件引用不到，只能复制一份；
     改气泡观感时两处都要改。这里是只读回放，没有重发/占位/滚动跟底那些逻辑。
-->
<template>
  <AppLayout>
    <div class="dk-records-page">
      <header class="dk-canvas-header">
        <div class="dk-page-head">
          <p class="dk-eyebrow">{{ t('designkit.brand') }}</p>
          <h1 class="dk-page-title">{{ t('designkit.adminRecords.title') }}</h1>
          <p class="dk-page-desc">{{ t('designkit.adminRecords.description') }}</p>
        </div>
      </header>

      <!-- 两个 tab + 账户筛选。筛选一变，两个 tab 的列表都重新拉。 -->
      <div class="dk-records-toolbar">
        <div class="dk-records-tabs" role="tablist">
          <button
            type="button"
            role="tab"
            class="dk-button dk-button--sm"
            :class="{ 'dk-button--secondary': activeTab !== 'chat' }"
            :aria-selected="activeTab === 'chat'"
            @click="switchTab('chat')"
          >
            {{ t('designkit.adminRecords.tabChat') }}
          </button>
          <button
            type="button"
            role="tab"
            class="dk-button dk-button--sm"
            :class="{ 'dk-button--secondary': activeTab !== 'jobs' }"
            :aria-selected="activeTab === 'jobs'"
            @click="switchTab('jobs')"
          >
            {{ t('designkit.adminRecords.tabJobs') }}
          </button>
        </div>

        <label class="dk-records-filter">
          <span class="dk-label dk-label--quiet">{{ t('designkit.adminRecords.userFilter') }}</span>
          <select v-model="userFilter" class="dk-input">
            <option value="">{{ t('designkit.adminRecords.allUsers') }}</option>
            <option v-for="u in users" :key="u.id" :value="u.id">
              {{
                t('designkit.adminRecords.userOption', {
                  email: u.email,
                  sessions: u.session_count,
                  jobs: u.job_count,
                })
              }}
            </option>
          </select>
        </label>
      </div>

      <!-- ==================== 对话记录 ==================== -->
      <template v-if="activeTab === 'chat'">
        <!-- 详情：只读消息流 -->
        <template v-if="sessionDetail">
          <div>
            <button
              type="button"
              class="dk-button dk-button--secondary dk-button--sm"
              @click="closeSession()"
            >
              {{ t('designkit.adminRecords.back') }}
            </button>
          </div>

          <section class="dk-panel dk-records-head">
            <p class="dk-text-strong dk-records-break">{{ sessionTitle(sessionDetail.row) }}</p>
            <p class="dk-muted">
              {{ t('designkit.adminRecords.user') }}：{{ sessionDetail.row.user_email }}
              · {{ t('designkit.adminRecords.messages', { count: sessionDetail.row.message_count }) }}
              · {{ formatDateTimeToMinute(sessionDetail.row.updated_at) }}
            </p>
          </section>

          <p v-if="sessionDetail.messages === null && !sessionDetail.error" class="dk-muted">
            {{ t('designkit.common.loading') }}
          </p>

          <div v-else-if="sessionDetail.error" class="dk-alert dk-alert--danger">
            <p>{{ t('designkit.adminRecords.loadFailed') }}</p>
            <p class="dk-alert__sub">{{ sessionDetail.error.message }}</p>
            <p v-if="sessionDetail.error.requestId" class="dk-alert__sub">
              {{ t('designkit.errors.requestId', { id: sessionDetail.error.requestId }) }}
            </p>
            <button
              type="button"
              class="dk-button dk-button--secondary dk-button--sm dk-alert__action"
              @click="openSession(sessionDetail.row)"
            >
              {{ t('designkit.common.retry') }}
            </button>
          </div>

          <div v-else class="dk-records-msgs">
            <div
              v-for="msg in sessionDetail.messages ?? []"
              :key="msg.id"
              class="dk-chat-row"
              :class="msg.role === 'user' ? 'is-user' : 'is-assistant'"
            >
              <div class="dk-chat-bubble">
                <!-- 带图消息：缩略图走管理员附图通道（文件头第 2 条）；
                     取失败回落「[附图]」占位文字。 -->
                <div v-if="msg.asset_uids.length > 0" class="dk-chat-thumbs">
                  <template v-for="uid in msg.asset_uids" :key="uid">
                    <img
                      v-if="chatImages.urls.value[attachmentUrl(uid)]"
                      :src="chatImages.urls.value[attachmentUrl(uid)]"
                      alt=""
                    />
                    <span
                      v-else-if="chatImages.failed.value[attachmentUrl(uid)]"
                      class="dk-records-attach"
                    >
                      {{ t('designkit.adminRecords.attachment') }}
                    </span>
                    <span v-else class="dk-chat-thumb-fallback">
                      {{ t('designkit.common.loading') }}
                    </span>
                  </template>
                </div>
                <p v-if="msg.content !== ''" class="dk-chat-text">{{ msg.content }}</p>
              </div>
            </div>
          </div>
        </template>

        <!-- 列表：会话 -->
        <template v-else>
          <p v-if="sessionsLoading && sessions === null" class="dk-muted">
            {{ t('designkit.common.loading') }}
          </p>

          <div v-else-if="sessionsError && sessions === null" class="dk-alert dk-alert--danger">
            <p>{{ t('designkit.adminRecords.loadFailed') }}</p>
            <p class="dk-alert__sub">{{ sessionsError.message }}</p>
            <p v-if="sessionsError.requestId" class="dk-alert__sub">
              {{ t('designkit.errors.requestId', { id: sessionsError.requestId }) }}
            </p>
            <button
              type="button"
              class="dk-button dk-button--secondary dk-button--sm dk-alert__action"
              @click="loadSessions(true)"
            >
              {{ t('designkit.common.retry') }}
            </button>
          </div>

          <div v-else-if="sessions !== null && sessions.length === 0" class="dk-empty dk-empty--boxed">
            <p class="dk-empty__title">{{ t('designkit.adminRecords.empty') }}</p>
          </div>

          <div v-else-if="sessions !== null" class="dk-stack">
            <section v-for="row in sessions" :key="row.uid" class="dk-panel dk-records-row">
              <div class="dk-records-row__info">
                <p class="dk-text-strong dk-records-break">{{ sessionTitle(row) }}</p>
                <p class="dk-muted">
                  {{ row.user_email }}
                  · {{ t('designkit.adminRecords.messages', { count: row.message_count }) }}
                  · {{ formatDateTimeToMinute(row.updated_at) }}
                </p>
              </div>
              <button
                type="button"
                class="dk-button dk-button--secondary dk-button--sm"
                @click="openSession(row)"
              >
                {{ t('designkit.adminRecords.open') }}
              </button>
            </section>

            <div v-if="sessionsHasMore" class="dk-records-more">
              <button
                type="button"
                class="dk-button dk-button--secondary dk-button--sm"
                :disabled="sessionsLoading"
                @click="loadSessions(false)"
              >
                {{ sessionsLoading ? t('designkit.common.loading') : t('designkit.common.loadMore') }}
              </button>
            </div>
          </div>
        </template>
      </template>

      <!-- ==================== 出图记录 ==================== -->
      <template v-else>
        <!-- 详情：每张的提示词快照 + 缩略图 -->
        <template v-if="jobDetail">
          <div>
            <button
              type="button"
              class="dk-button dk-button--secondary dk-button--sm"
              @click="closeJob()"
            >
              {{ t('designkit.adminRecords.back') }}
            </button>
          </div>

          <section class="dk-panel dk-records-head">
            <p class="dk-records-head__title">
              <GalleryStatusBadge :status="jobDetail.row.status" kind="job" />
              <span class="dk-text-strong dk-records-break">{{ jobName(jobDetail.row) }}</span>
            </p>
            <p class="dk-muted">
              {{ t('designkit.adminRecords.user') }}：{{ jobDetail.row.user_email }}
              · {{
                t('designkit.adminRecords.imagesCount', {
                  success: jobDetail.row.success_count,
                  total: jobDetail.row.item_count,
                })
              }}
              · {{ t('designkit.adminRecords.cost', { amount: formatMoney(jobDetail.row.actual_cost) }) }}
              · {{ jobDetail.row.ratio }}
              · {{ formatDateTimeToMinute(jobDetail.row.created_at) }}
            </p>
          </section>

          <p v-if="jobDetail.items === null && !jobDetail.error" class="dk-muted">
            {{ t('designkit.common.loading') }}
          </p>

          <div v-else-if="jobDetail.error" class="dk-alert dk-alert--danger">
            <p>{{ t('designkit.adminRecords.loadFailed') }}</p>
            <p class="dk-alert__sub">{{ jobDetail.error.message }}</p>
            <p v-if="jobDetail.error.requestId" class="dk-alert__sub">
              {{ t('designkit.errors.requestId', { id: jobDetail.error.requestId }) }}
            </p>
            <button
              type="button"
              class="dk-button dk-button--secondary dk-button--sm dk-alert__action"
              @click="openJob(jobDetail.row)"
            >
              {{ t('designkit.common.retry') }}
            </button>
          </div>

          <div v-else class="dk-stack">
            <section v-for="item in jobDetail.items ?? []" :key="item.seq" class="dk-panel dk-records-item">
              <!-- 缩略图走管理员专用通道 + useAuthedImages（直接 <img src> 是 401 碎图）。 -->
              <template v-if="item.has_image">
                <img
                  v-if="images.urls.value[itemUrl(item)]"
                  class="dk-records-thumb"
                  :src="images.urls.value[itemUrl(item)]"
                  alt=""
                />
                <button
                  v-else-if="images.failed.value[itemUrl(item)]"
                  type="button"
                  class="dk-records-thumb-fallback"
                  @click="images.retry(itemUrl(item))"
                >
                  {{ t('designkit.common.retry') }}
                </button>
                <span v-else class="dk-records-thumb-fallback">
                  {{ t('designkit.common.loading') }}
                </span>
              </template>

              <div class="dk-records-item__body">
                <p class="dk-records-item__head">
                  <span class="dk-text-strong">{{ t('designkit.job.seq', { seq: item.seq }) }}</span>
                  <GalleryStatusBadge :status="item.status" kind="item" />
                </p>
                <p class="dk-label dk-label--quiet dk-records-item__label">
                  {{ t('designkit.adminRecords.promptOfThis') }}
                </p>
                <p class="dk-records-prompt">{{ item.prompt }}</p>
                <p v-if="item.billed_cost" class="dk-muted">
                  {{ t('designkit.job.billedCost', { amount: formatMoney(item.billed_cost) }) }}
                </p>
              </div>
            </section>
          </div>
        </template>

        <!-- 列表：批次 -->
        <template v-else>
          <p v-if="jobsLoading && jobs === null" class="dk-muted">
            {{ t('designkit.common.loading') }}
          </p>

          <div v-else-if="jobsError && jobs === null" class="dk-alert dk-alert--danger">
            <p>{{ t('designkit.adminRecords.loadFailed') }}</p>
            <p class="dk-alert__sub">{{ jobsError.message }}</p>
            <p v-if="jobsError.requestId" class="dk-alert__sub">
              {{ t('designkit.errors.requestId', { id: jobsError.requestId }) }}
            </p>
            <button
              type="button"
              class="dk-button dk-button--secondary dk-button--sm dk-alert__action"
              @click="loadJobs(true)"
            >
              {{ t('designkit.common.retry') }}
            </button>
          </div>

          <div v-else-if="jobs !== null && jobs.length === 0" class="dk-empty dk-empty--boxed">
            <p class="dk-empty__title">{{ t('designkit.adminRecords.empty') }}</p>
          </div>

          <div v-else-if="jobs !== null" class="dk-stack">
            <section v-for="row in jobs" :key="row.uid" class="dk-panel dk-records-row">
              <div class="dk-records-row__info">
                <p class="dk-records-head__title">
                  <GalleryStatusBadge :status="row.status" kind="job" />
                  <span class="dk-text-strong dk-records-break">{{ jobName(row) }}</span>
                </p>
                <p class="dk-muted">
                  {{ row.user_email }}
                  · {{
                    t('designkit.adminRecords.imagesCount', {
                      success: row.success_count,
                      total: row.item_count,
                    })
                  }}
                  · {{ t('designkit.adminRecords.cost', { amount: formatMoney(row.actual_cost) }) }}
                  · {{ formatDateTimeToMinute(row.created_at) }}
                </p>
              </div>
              <button
                type="button"
                class="dk-button dk-button--secondary dk-button--sm"
                @click="openJob(row)"
              >
                {{ t('designkit.adminRecords.open') }}
              </button>
            </section>

            <div v-if="jobsHasMore" class="dk-records-more">
              <button
                type="button"
                class="dk-button dk-button--secondary dk-button--sm"
                :disabled="jobsLoading"
                @click="loadJobs(false)"
              >
                {{ jobsLoading ? t('designkit.common.loading') : t('designkit.common.loadMore') }}
              </button>
            </div>
          </div>
        </template>
      </template>
    </div>
  </AppLayout>
</template>

<script setup lang="ts">
import { onMounted, onUnmounted, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import AppLayout from '@/components/layout/AppLayout.vue'
import { useAppStore } from '@/stores'
import { formatDateTimeToMinute } from '@/utils/format'
import {
  ADMIN_RECORDS_PAGE_SIZE,
  adminAssetContentUrl,
  adminJobItemContentUrl,
  formatMoney,
  getAdminChatSession,
  getAdminJob,
  isCanceledError,
  listAdminChatSessions,
  listAdminJobs,
  listAdminRecordUsers,
  toFriendlyError,
} from '../api'
import type {
  AdminChatMessage,
  AdminChatSessionRow,
  AdminJobItemRow,
  AdminJobRow,
  AdminRecordUser,
  FriendlyError,
} from '../api'
import GalleryStatusBadge from '../components/gallery/GalleryStatusBadge.vue'
import { useAuthedImages } from '../composables/useAuthedImage'
// 页面用到的通用样式（卡片、按钮、输入框…），全局引入，Vite 会去重。
import '../components/designkit-ui.css'

const { t } = useI18n()
const appStore = useAppStore()

type Tab = 'chat' | 'jobs'

const activeTab = ref<Tab>('chat')
/** 账户筛选：'' = 全部账户。option 的 :value 是数字，v-model 会保持数字。 */
const userFilter = ref<number | ''>('')
/** 筛选下拉的账户列表。拉不回来就只剩「全部账户」，不拦着看记录。 */
const users = ref<AdminRecordUser[]>([])

// ---------------------------------------------------------------------------
// 对话记录
// ---------------------------------------------------------------------------

/** 会话列表；null = 还没读回来（跟空列表区分开）。 */
const sessions = ref<AdminChatSessionRow[] | null>(null)
/** 上一页整整 50 条 → 可能还有下一页。 */
const sessionsHasMore = ref(false)
const sessionsLoading = ref(false)
const sessionsError = ref<FriendlyError | null>(null)

/** 打开的会话。messages === null 且没 error = 详情在加载。 */
interface SessionDetail {
  row: AdminChatSessionRow
  messages: AdminChatMessage[] | null
  error: FriendlyError | null
}
const sessionDetail = ref<SessionDetail | null>(null)

// ---------------------------------------------------------------------------
// 出图记录
// ---------------------------------------------------------------------------

const jobs = ref<AdminJobRow[] | null>(null)
const jobsHasMore = ref(false)
const jobsLoading = ref(false)
const jobsError = ref<FriendlyError | null>(null)

interface JobDetail {
  row: AdminJobRow
  items: AdminJobItemRow[] | null
  error: FriendlyError | null
}
const jobDetail = ref<JobDetail | null>(null)

/** 批次缩略图。取、缓存、限流、回收都在它里面（组件卸载自动回收）。 */
const images = useAuthedImages()

/**
 * 对话附图的缩略图。跟批次缩略图**分开一个实例**：关批次详情会 releaseAll，
 * 合用一个会把还开着的对话详情里的图一起放掉。
 */
const chatImages = useAuthedImages()

let sessionsController: AbortController | null = null
let jobsController: AbortController | null = null
// 两个详情各一个 controller：合用一个的话，chat 详情还在加载时打开一个批次
// 会把它悄悄打断，切回对话 tab 就是永远的「加载中…」。
let sessionDetailController: AbortController | null = null
let jobDetailController: AbortController | null = null
let disposed = false

// ---------------------------------------------------------------------------
// 小工具
// ---------------------------------------------------------------------------

function filterUserId(): number | undefined {
  return userFilter.value === '' ? undefined : userFilter.value
}

/** 后端还没起标题的会话显示「新对话」（跟用户自己的对话页同一个兜底）。 */
function sessionTitle(row: AdminChatSessionRow): string {
  return row.title.trim() !== '' ? row.title : t('designkit.chat.untitled')
}

/** 没起名的批次显示「未命名批次」（跟「我的图片」同一个兜底）。 */
function jobName(row: AdminJobRow): string {
  return row.name.trim() !== '' ? row.name : t('designkit.job.untitled')
}

function itemUrl(item: AdminJobItemRow): string {
  return adminJobItemContentUrl(jobDetail.value?.row.uid ?? '', item.seq)
}

function attachmentUrl(assetUid: string): string {
  return adminAssetContentUrl(assetUid)
}

// ---------------------------------------------------------------------------
// 读列表
// ---------------------------------------------------------------------------

/** 筛选下拉的账户。失败静默：下拉少了选项，记录本身照常能看。 */
async function loadUsers(): Promise<void> {
  try {
    const list = await listAdminRecordUsers()
    if (!disposed) {
      users.value = list
    }
  } catch {
    // 静默。
  }
}

async function loadSessions(reset: boolean): Promise<void> {
  sessionsController?.abort()
  sessionsController = new AbortController()
  sessionsLoading.value = true
  sessionsError.value = null
  const offset = reset ? 0 : (sessions.value?.length ?? 0)
  try {
    const page = await listAdminChatSessions(
      { userId: filterUserId(), limit: ADMIN_RECORDS_PAGE_SIZE, offset },
      sessionsController.signal,
    )
    if (disposed) {
      return
    }
    sessions.value = reset ? page : [...(sessions.value ?? []), ...page]
    sessionsHasMore.value = page.length === ADMIN_RECORDS_PAGE_SIZE
  } catch (error) {
    if (isCanceledError(error) || disposed) {
      return
    }
    const friendly = toFriendlyError(error)
    if (sessions.value === null) {
      sessionsError.value = friendly
    } else {
      // 「加载更多」失败：列表还在，弹个提示就够了，别把整页换成报错。
      appStore.showError(t('designkit.adminRecords.loadFailed'))
    }
  } finally {
    if (!disposed) {
      sessionsLoading.value = false
    }
  }
}

async function loadJobs(reset: boolean): Promise<void> {
  jobsController?.abort()
  jobsController = new AbortController()
  jobsLoading.value = true
  jobsError.value = null
  const offset = reset ? 0 : (jobs.value?.length ?? 0)
  try {
    const page = await listAdminJobs(
      { userId: filterUserId(), limit: ADMIN_RECORDS_PAGE_SIZE, offset },
      jobsController.signal,
    )
    if (disposed) {
      return
    }
    jobs.value = reset ? page : [...(jobs.value ?? []), ...page]
    jobsHasMore.value = page.length === ADMIN_RECORDS_PAGE_SIZE
  } catch (error) {
    if (isCanceledError(error) || disposed) {
      return
    }
    const friendly = toFriendlyError(error)
    if (jobs.value === null) {
      jobsError.value = friendly
    } else {
      appStore.showError(t('designkit.adminRecords.loadFailed'))
    }
  } finally {
    if (!disposed) {
      jobsLoading.value = false
    }
  }
}

// ---------------------------------------------------------------------------
// 详情
// ---------------------------------------------------------------------------

async function openSession(row: AdminChatSessionRow): Promise<void> {
  sessionDetailController?.abort()
  sessionDetailController = new AbortController()
  sessionDetail.value = { row, messages: null, error: null }
  try {
    const detail = await getAdminChatSession(row.uid, sessionDetailController.signal)
    if (disposed || sessionDetail.value?.row.uid !== row.uid) {
      return
    }
    // 详情返回的会话行更新头部（标题、消息数可能比列表里的新）。
    sessionDetail.value = { row: detail.session ?? row, messages: detail.messages, error: null }
  } catch (error) {
    if (isCanceledError(error) || disposed || sessionDetail.value?.row.uid !== row.uid) {
      return
    }
    sessionDetail.value = { row, messages: null, error: toFriendlyError(error) }
  }
}

function closeSession(): void {
  sessionDetailController?.abort()
  sessionDetail.value = null
  // 附图缩略图占的内存，关详情就放掉（同 closeJob 的做法）。
  chatImages.releaseAll()
}

async function openJob(row: AdminJobRow): Promise<void> {
  jobDetailController?.abort()
  jobDetailController = new AbortController()
  jobDetail.value = { row, items: null, error: null }
  try {
    const detail = await getAdminJob(row.uid, jobDetailController.signal)
    if (disposed || jobDetail.value?.row.uid !== row.uid) {
      return
    }
    jobDetail.value = { row: detail.job ?? row, items: detail.items, error: null }
  } catch (error) {
    if (isCanceledError(error) || disposed || jobDetail.value?.row.uid !== row.uid) {
      return
    }
    jobDetail.value = { row, items: null, error: toFriendlyError(error) }
  }
}

function closeJob(): void {
  jobDetailController?.abort()
  jobDetail.value = null
  // 一批几十张缩略图占的内存，关详情就放掉。
  images.releaseAll()
}

// 详情一到就把缩略图排进队列。⚠ 不在模板里调用会触发加载的函数
// （每次重渲染都会再发请求），统一在 watch 里 ensure（useAuthedImage 文件头的约定）。
watch(
  () =>
    jobDetail.value?.items
      ?.filter((item) => item.has_image)
      .map((item) => itemUrl(item))
      .join('\n') ?? '',
  () => {
    const detail = jobDetail.value
    if (detail?.items) {
      images.ensure(detail.items.filter((item) => item.has_image).map((item) => itemUrl(item)))
    }
  },
  { immediate: true },
)

// 会话详情一到就把附图排进队列（同上：只在 watch 里 ensure）。
watch(
  () => sessionDetail.value?.messages?.flatMap((msg) => msg.asset_uids).join('\n') ?? '',
  () => {
    const messages = sessionDetail.value?.messages
    if (messages) {
      chatImages.ensure(messages.flatMap((msg) => msg.asset_uids.map(attachmentUrl)))
    }
  },
  { immediate: true },
)

// ---------------------------------------------------------------------------
// tab 与筛选
// ---------------------------------------------------------------------------

function switchTab(tab: Tab): void {
  if (activeTab.value === tab) {
    return
  }
  activeTab.value = tab
  // 每个 tab 的列表和打开的详情各自保留；第一次进来才拉。
  if (tab === 'chat' && sessions.value === null && !sessionsLoading.value) {
    void loadSessions(true)
  }
  if (tab === 'jobs' && jobs.value === null && !jobsLoading.value) {
    void loadJobs(true)
  }
}

// 筛选一变：详情关掉、两个 tab 的列表都作废，当前 tab 立即重拉，
// 另一个 tab 等切过去再拉（sessions/jobs 为 null 时 switchTab 会拉）。
watch(userFilter, () => {
  closeSession()
  closeJob()
  sessions.value = null
  sessionsHasMore.value = false
  jobs.value = null
  jobsHasMore.value = false
  if (activeTab.value === 'chat') {
    void loadSessions(true)
  } else {
    void loadJobs(true)
  }
})

onMounted(() => {
  void loadUsers()
  void loadSessions(true)
})

onUnmounted(() => {
  disposed = true
  sessionsController?.abort()
  jobsController?.abort()
  sessionDetailController?.abort()
  jobDetailController?.abort()
})
</script>

<style scoped>
.dk-records-page {
  display: flex;
  flex-direction: column;
  gap: var(--dk-space-4);
}

.dk-records-toolbar {
  display: flex;
  flex-wrap: wrap;
  gap: var(--dk-space-3);
  align-items: flex-end;
  justify-content: space-between;
}

.dk-records-tabs {
  display: flex;
  gap: var(--dk-space-2);
}

.dk-records-filter {
  display: flex;
  flex: 0 1 24rem;
  min-width: 14rem;
  flex-direction: column;
  gap: var(--dk-space-1);
}

/* 列表里的一行：左边信息、右边「查看」。 */
.dk-records-row {
  display: flex;
  flex-wrap: wrap;
  gap: var(--dk-space-3);
  align-items: center;
  justify-content: space-between;
}

.dk-records-row__info {
  flex: 1 1 18rem;
  min-width: 0;
  display: flex;
  flex-direction: column;
  gap: var(--dk-space-1);
}

.dk-records-break {
  overflow-wrap: anywhere;
}

.dk-records-head {
  display: flex;
  flex-direction: column;
  gap: var(--dk-space-1);
}

.dk-records-head__title {
  display: flex;
  flex-wrap: wrap;
  gap: var(--dk-space-2);
  align-items: center;
  margin: 0;
}

.dk-records-more {
  display: flex;
  justify-content: center;
}

/* ---------- 对话详情：气泡样式抄自 ChatMessageList.vue（那边是 scoped，
   跨组件引用不到，类名保持一致，改观感时两处都要改）。 ---------- */

.dk-records-msgs {
  display: flex;
  min-width: 0;
  flex-direction: column;
  gap: var(--dk-space-3);
}

.dk-chat-row {
  display: flex;
  min-width: 0;
  flex-direction: column;
  align-items: flex-start;
}

.dk-chat-row.is-user {
  align-items: flex-end;
}

.dk-chat-bubble {
  max-width: min(680px, 86%);
  border-radius: var(--dk-radius-lg);
  padding: 9px 13px;
  font-size: var(--dk-text-md);
  line-height: 1.6;
}

.is-assistant .dk-chat-bubble {
  border: 1px solid var(--dk-border);
  border-bottom-left-radius: var(--dk-radius-sm);
  background: var(--dk-surface-subtle);
  color: var(--dk-text);
}

.is-user .dk-chat-bubble {
  border-bottom-right-radius: var(--dk-radius-sm);
  background: var(--dk-action);
  color: var(--dk-text-inverse);
}

.dk-chat-text {
  margin: 0;
  overflow-wrap: anywhere;
  white-space: pre-wrap;
}

/* 带图消息的缩略图（样式抄自 ChatMessageList.vue 的 dk-chat-thumbs，
   那边是 scoped 跨组件引用不到；改观感时两处都要改）。 */
.dk-chat-thumbs {
  display: flex;
  flex-wrap: wrap;
  gap: var(--dk-space-1);
}

.dk-chat-thumbs + .dk-chat-text {
  margin-top: var(--dk-space-2);
}

.dk-chat-thumbs img {
  display: block;
  width: 96px;
  height: 96px;
  border-radius: var(--dk-radius-sm);
  background: var(--dk-image-canvas);
  object-fit: cover;
}

.dk-chat-thumb-fallback {
  display: inline-flex;
  width: 96px;
  height: 96px;
  align-items: center;
  justify-content: center;
  border: 1px dashed var(--dk-border-strong);
  border-radius: var(--dk-radius-sm);
  background: var(--dk-image-canvas);
  color: var(--dk-text-tertiary);
  padding: var(--dk-space-1);
  font-size: 11px;
  line-height: 1.4;
  text-align: center;
}

/* 取失败的回落：「[附图]」占位文字（文件头第 2 条）。 */
.dk-records-attach {
  margin: 0;
  align-self: center;
  font-size: var(--dk-text-xs);
  opacity: 0.85;
}

/* ---------- 出图详情：单张 = 缩略图 + 提示词快照。 ---------- */

.dk-records-item {
  display: flex;
  gap: var(--dk-space-3);
  align-items: flex-start;
}

.dk-records-thumb {
  display: block;
  width: 96px;
  height: 96px;
  flex: none;
  border-radius: var(--dk-radius-sm);
  background: var(--dk-image-canvas);
  object-fit: cover;
}

.dk-records-thumb-fallback {
  display: inline-flex;
  width: 96px;
  height: 96px;
  flex: none;
  align-items: center;
  justify-content: center;
  border: 1px dashed var(--dk-border-strong);
  border-radius: var(--dk-radius-sm);
  background: var(--dk-image-canvas);
  color: var(--dk-text-tertiary);
  padding: var(--dk-space-1);
  font-family: inherit;
  font-size: 11px;
  line-height: 1.4;
  text-align: center;
}

button.dk-records-thumb-fallback {
  cursor: pointer;
}

.dk-records-item__body {
  flex: 1 1 0;
  min-width: 0;
  display: flex;
  flex-direction: column;
  gap: var(--dk-space-1);
}

.dk-records-item__head {
  display: flex;
  flex-wrap: wrap;
  gap: var(--dk-space-2);
  align-items: center;
  margin: 0;
}

.dk-records-item__label {
  margin-bottom: 0;
}

.dk-records-prompt {
  margin: 0;
  overflow-wrap: anywhere;
  white-space: pre-wrap;
}
</style>
