<!--
  我的图片：出过的图按「一次提交」（一批）归档。

  两件事要说清楚：

  1. **只有「加载更多」，没有页码。**
     后端用的是游标分页（拿上一页最后一条的位置往下翻），不支持「跳到第 5 页」，
     也给不出总条数。原因是出图是一直在写的，用页码翻到一半会漏数据。
     所以这里就老老实实一个「加载更多」按钮。

  2. **点开一批看详情**，详情里的图严格按序号升序（「第几张」是对外契约）。
     选中哪一批记在网址上（`?job=xxx`），刷新页面还能回到原来那一批。

  版式照老 designkit 的 `frontend/css/views/history.css`：批次卡片一行一条
  （名字 + 状态 / 时间 · 比例 · 张数 / 成功失败 + 花费），点开是详情。
  外壳用上游的 `AppLayout`，跟其它侧边栏页面一致（2026-08-13 改，原来是窄图标栏外壳）。
-->
<template>
  <AppLayout>
    <div class="dk-canvas">
      <header class="dk-canvas-header">
        <div class="dk-page-head">
          <p class="dk-eyebrow">{{ t('designkit.brand') }}</p>
          <h1 class="dk-page-title">{{ t('designkit.gallery.title') }}</h1>
          <p class="dk-page-desc">{{ t('designkit.gallery.description') }}</p>
        </div>
      </header>

      <div class="dk-canvas-body">
        <!-- 本月花了多少 / 还剩多少（决策 16） -->
        <UsageCard ref="usageCard" />

        <!-- 详情 -->
        <JobDetail
          v-if="selectedJobUid"
          :job-uid="selectedJobUid"
          @back="closeDetail()"
          @balance-changed="refreshUsage()"
        />

        <!-- 列表 -->
        <template v-else>
          <section class="dk-panel">
            <div class="dk-panel-head dk-panel-head--tight">
              <h2 class="dk-panel-title">{{ t('designkit.job.listTitle') }}</h2>
              <button
                type="button"
                class="dk-button dk-button--quiet dk-button--sm"
                :disabled="loading"
                @click="reload()"
              >
                {{ t('designkit.common.refresh') }}
              </button>
            </div>

            <!-- 第一次加载 -->
            <p v-if="loading && jobs.length === 0" class="dk-muted">
              {{ t('designkit.common.loading') }}
            </p>

            <!-- 读失败 -->
            <div v-else-if="loadError && jobs.length === 0" class="dk-alert dk-alert--danger">
              <p>{{ loadError?.message }}</p>
              <p v-if="loadError?.requestId" class="dk-alert__sub">
                {{ t('designkit.errors.requestId', { id: loadError?.requestId }) }}
              </p>
              <button
                type="button"
                class="dk-button dk-button--secondary dk-button--sm dk-alert__action"
                @click="reload()"
              >
                {{ t('designkit.common.retry') }}
              </button>
            </div>

            <!-- 一张图都没出过：直接把人送去工作台。
                 gallery.empty 和 job.listEmpty 是同一句话说两遍，只留后者
                 （设计稿 05-视觉精修 第①块点名删的重复）。 -->
            <div v-else-if="jobs.length === 0" class="dk-empty">
              <img class="dk-empty__illus" src="/designkit/illustrations/empty-gallery.svg" alt="" />
              <p class="dk-empty__description">{{ t('designkit.job.listEmpty') }}</p>
              <RouterLink class="dk-button" :to="DESIGNKIT_WORKBENCH_PATH">
                {{ t('designkit.inspiration.goWorkbench') }}
              </RouterLink>
            </div>

            <!-- 有记录 -->
            <div v-else class="dk-history-list">
              <JobCard v-for="job in jobs" :key="job.uid" :job="job" @open="openDetail" />
            </div>
          </section>

          <!-- 翻页：只有「加载更多」（后端是游标分页，给不出总页数） -->
          <div v-if="jobs.length > 0" class="dk-pager">
            <button
              v-if="hasMore"
              type="button"
              class="dk-button dk-button--secondary dk-button--sm"
              :disabled="loadingMore"
              @click="loadMore()"
            >
              {{ loadingMore ? t('designkit.common.loading') : t('designkit.common.loadMore') }}
            </button>
            <p v-else class="dk-note dk-note--quiet">{{ t('designkit.common.noMore') }}</p>
            <p v-if="loadError" class="dk-note dk-note--danger">{{ loadError?.message }}</p>
          </div>
        </template>
      </div>
    </div>
  </AppLayout>
</template>

<script setup lang="ts">
import { computed, onMounted, onUnmounted, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import { RouterLink, useRoute, useRouter } from 'vue-router'
import AppLayout from '@/components/layout/AppLayout.vue'
import { isCanceledError, listJobs, toFriendlyError } from '../api'
import type { FriendlyError, Job } from '../api'
import { DESIGNKIT_WORKBENCH_PATH } from '../nav'
import UsageCard from '../components/UsageCard.vue'
import JobCard from '../components/gallery/JobCard.vue'
import JobDetail from '../components/gallery/JobDetail.vue'
// 页面里用到的通用样式（按钮、卡片、批次列表……），全局引入，Vite 会去重。
import '../components/designkit-ui.css'

/** 一次拉多少批。20 条大约是一屏半，够翻又不至于一次拉太久。 */
const PAGE_SIZE = 20

const { t } = useI18n()
const route = useRoute()
const router = useRouter()

/** 顶部那张消费卡片，停止排队 / 重试之后要让它重新读一次余额。 */
const usageCard = ref<InstanceType<typeof UsageCard> | null>(null)

const jobs = ref<Job[]>([])
const cursor = ref<string | null>(null)
const hasMore = ref(false)
const loading = ref(false)
const loadingMore = ref(false)
const loadError = ref<FriendlyError | null>(null)

let controller: AbortController | null = null

/** 当前打开的是哪一批（记在网址上，刷新页面不会丢）。 */
const selectedJobUid = computed(() => {
  const raw = route.query.job
  const value = Array.isArray(raw) ? raw[0] : raw
  return typeof value === 'string' && value.trim() !== '' ? value.trim() : ''
})

function openDetail(uid: string): void {
  void router.push({ query: { ...route.query, job: uid } })
}

function closeDetail(): void {
  const query = { ...route.query }
  delete query.job
  void router.push({ query })
}

/**
 * 拉一页。
 *
 * @param append true = 接在现有列表后面（「加载更多」），false = 从头重来（刷新）
 */
async function fetchPage(append: boolean): Promise<void> {
  controller?.abort()
  controller = new AbortController()
  if (append) {
    loadingMore.value = true
  } else {
    loading.value = true
  }
  loadError.value = null
  try {
    const page = await listJobs({
      limit: PAGE_SIZE,
      cursor: append ? cursor.value : null,
      signal: controller.signal,
    })
    jobs.value = append ? [...jobs.value, ...page.items] : page.items
    cursor.value = page.next_cursor
    hasMore.value = page.has_more
  } catch (error) {
    if (isCanceledError(error)) {
      return
    }
    loadError.value = toFriendlyError(error)
  } finally {
    loading.value = false
    loadingMore.value = false
  }
}

function reload(): void {
  cursor.value = null
  hasMore.value = false
  void fetchPage(false)
}

/** 详情里动过钱（停止排队 / 重试）之后刷新余额。 */
function refreshUsage(): void {
  void usageCard.value?.refresh()
}

function loadMore(): void {
  if (!hasMore.value || loadingMore.value) {
    return
  }
  void fetchPage(true)
}

// 从详情退回列表时刷新一遍：刚才盯着的那一批多半已经跑完了，
// 计数和花费都变了，还显示旧数字会让人以为没生效。
watch(selectedJobUid, (uid, previous) => {
  if (uid === '' && previous !== '') {
    reload()
  }
})

onMounted(() => {
  reload()
})

onUnmounted(() => {
  controller?.abort()
  controller = null
})
</script>
