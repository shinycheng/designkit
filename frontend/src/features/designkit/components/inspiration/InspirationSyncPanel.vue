<!--
  灵感库同步（**只有管理员看得到**）。

  页面那边是 `v-if="isAdmin"`，非管理员**整块不渲染**——不是 CSS 隐藏。
  理由有两条：一是后端那四个端点本来就只放管理员过（普通用户会拿到 403，
  渲染出来只会得到一片红色报错）；二是运营看到「立即同步」会去点，
  点不动之后来问「是不是我账号坏了」。

  这一块管三件事：

  1. **同步走哪个代理**。下拉框直接复用上游 `components/common/ProxySelector.vue`
     （跟「添加账号」里那个是同一个组件，monica 明确要求「和添加账号里一样」），
     所以后端那份代理列表的字段名跟上游 `/admin/proxies/all` 逐字对齐。
     选完**立刻保存**，不再有一个「保存」按钮——见 saveProxy() 上的注释。

     ⚠ 这个代理**只给灵感库同步用，不影响出图**。后端的 message 里写着这句话，
     这里必须原样显示出来：不写的话，管理员会以为选了代理之后出图也走代理，
     于是在出图变慢时来改这里——改了没用，还找不到原因。

  2. **立即同步**。点完立刻返回（后端 202），真正的进度靠每 2 秒问一次。
     跑的过程中显示「拉了多少 / 新增多少 / 更新多少 / 没变化多少」，
     不是一个转不完的圈——一万四千条要十几秒，没有数字的等待会让人以为卡死了。

  3. **上次同步的结果**。成功、失败、失败原因（中文），以及失败时该去哪儿看。
     拉不到外网多半是代理的问题，所以失败提示里直接给一个去「IP管理」的链接。
-->
<template>
  <section class="dk-panel">
    <div class="dk-panel-head">
      <div>
        <h2 class="dk-panel-title">{{ t('designkit.inspiration.sync.title') }}</h2>
        <p class="dk-panel-hint">{{ t('designkit.inspiration.sync.description') }}</p>
      </div>
      <div class="dk-panel-actions">
        <button
          type="button"
          class="dk-button dk-button--quiet dk-button--sm"
          :disabled="loading"
          @click="refreshStatus()"
        >
          {{ t('designkit.common.refresh') }}
        </button>
        <button
          type="button"
          class="dk-button dk-button--sm"
          :disabled="startDisabled"
          @click="start()"
        >
          {{ startLabel }}
        </button>
      </div>
    </div>

    <!-- 后端根本没装同步功能：说清楚「浏览不受影响」，不要显示成故障 -->
    <p v-if="unavailable" class="dk-alert dk-alert--warning">
      {{ t('designkit.inspiration.sync.unavailable') }}
    </p>

    <template v-else>
      <!-- 正在跑：显示实时的四个数 -->
      <div v-if="status?.running" class="dk-alert">
        <p>{{ statusMessage }}</p>
        <p v-if="progressText !== ''" class="dk-alert__sub">{{ progressText }}</p>
      </div>

      <!-- 没在跑：上次的结果 -->
      <div v-else-if="status" class="dk-stack dk-stack--sm">
        <p :class="lastRunClass">{{ statusMessage }}</p>

        <!-- 失败原因（后端给的中文）+ 该去哪儿检查 -->
        <template v-if="lastRunFailed">
          <p v-if="failureReason !== ''" class="dk-note dk-note--danger">{{ failureReason }}</p>
          <p class="dk-note">
            {{ t('designkit.inspiration.sync.failedHint') }}
            <RouterLink class="dk-insp-sync__link" to="/admin/proxies">
              {{ t('designkit.inspiration.sync.openProxies') }}
            </RouterLink>
          </p>
        </template>

        <p class="dk-note dk-note--quiet">{{ libraryCountText }}</p>
      </div>

      <p v-else-if="loading" class="dk-muted">{{ t('designkit.common.loading') }}</p>

      <!-- 读状态失败（不是 404，是真出错了） -->
      <p v-if="statusError" class="dk-note dk-note--danger">{{ statusError }}</p>

      <!-- ── 同步走的代理 ─────────────────────────────────── -->
      <div class="dk-insp-sync__proxy">
        <label class="dk-label">{{ t('designkit.inspiration.sync.proxyLabel') }}</label>

        <ProxySelector
          v-model="proxyID"
          :proxies="proxyOptions"
          :disabled="savingProxy || settings === null"
        />

        <!-- 后端拼好的整句中文：写着「只影响同步，不影响出图」，必须显示 -->
        <p class="dk-note">{{ proxyMessage }}</p>

        <p v-if="settings && settings.proxies.length === 0" class="dk-note dk-note--quiet">
          {{ t('designkit.inspiration.sync.proxyEmpty') }}
          <RouterLink class="dk-insp-sync__link" to="/admin/proxies">
            {{ t('designkit.inspiration.sync.openProxies') }}
          </RouterLink>
        </p>

        <p v-if="savingProxy" class="dk-note dk-note--quiet">
          {{ t('designkit.inspiration.sync.proxySaving') }}
        </p>
        <p v-else-if="proxySaved" class="dk-note dk-note--success">
          {{ t('designkit.inspiration.sync.proxySaved') }}
        </p>
        <p v-if="proxyError" class="dk-note dk-note--danger">{{ proxyError }}</p>
      </div>

      <p class="dk-note dk-note--quiet dk-mt-3">{{ t('designkit.inspiration.sync.autoNote') }}</p>
    </template>
  </section>
</template>

<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import { RouterLink } from 'vue-router'
import ProxySelector from '@/components/common/ProxySelector.vue'
import { useAppStore } from '@/stores'
import {
  errorText,
  getPromptSyncSettings,
  getPromptSyncStatus,
  isCanceledError,
  isSyncUnavailableError,
  startPromptSync,
  toUpstreamProxy,
  updatePromptSyncSettings,
} from '../../api'
import type { PromptSyncRun, PromptSyncSettings, PromptSyncStatus } from '../../api'

/** 轮询间隔。同步大约十几秒，2 秒一次既看得出在动，也不至于把后端问烦。 */
const POLL_INTERVAL_MS = 2000

/**
 * 最多轮询多久（10 分钟）。
 *
 * 正常十几秒就完了。设这个上限是防「服务重启把记录卡在『进行中』」——
 * 那种情况下 running 永远是 true，没有上限的话这个页面会一直问下去，
 * 开着不管就是一晚上几万次请求。
 */
const POLL_TIMEOUT_MS = 10 * 60 * 1000

const emit = defineEmits<{
  /** 一次同步跑完了（成功或失败都发）：页面据此重新拉分类和列表。 */
  (e: 'synced'): void
}>()

const { t } = useI18n()
const appStore = useAppStore()

const status = ref<PromptSyncStatus | null>(null)
const settings = ref<PromptSyncSettings | null>(null)
/** 后端整组端点没挂（404）。这不是故障，浏览照常。 */
const unavailable = ref(false)
const loading = ref(false)
const starting = ref(false)
const statusError = ref('')

/** 下拉框绑的值。null = 不走代理（直连）。 */
const proxyID = ref<number | null>(null)
const savingProxy = ref(false)
const proxySaved = ref(false)
const proxyError = ref('')

let pollTimer: number | null = null
let pollDeadline = 0
let controller: AbortController | null = null
/** 上一次看到的「在跑吗」，用来认出「刚刚跑完」这一刻。 */
let wasRunning = false
/** 组件已经卸载了，异步回调回来时不要再动响应式状态。 */
let disposed = false

const proxyOptions = computed(() => (settings.value?.proxies ?? []).map(toUpstreamProxy))

/** 代理说明：优先用后端拼好的整句（它带着代理名字）。 */
const proxyMessage = computed(
  () => settings.value?.message ?? t('designkit.inspiration.sync.proxyFallbackHint'),
)

const startDisabled = computed(
  () => starting.value || status.value?.running === true || unavailable.value,
)

const startLabel = computed(() => {
  if (starting.value) {
    return t('designkit.inspiration.sync.starting')
  }
  if (status.value?.running) {
    return t('designkit.inspiration.sync.running')
  }
  return t('designkit.inspiration.sync.start')
})

/** 上次同步那句话的颜色：失败标红，跳过标黄，成功就正常颜色。 */
const lastRunClass = computed(() => {
  switch (status.value?.sync?.status) {
    case 'failed':
      return 'dk-note dk-note--danger'
    case 'skipped':
    case 'running':
      return 'dk-note dk-note--warning'
    default:
      return 'dk-text'
  }
})

/** 后端拼好的那句整话（在跑 / 上次结果都用它）。 */
const statusMessage = computed(() => status.value?.message ?? '')

/**
 * 「已拉到 N 条 · 新增 N · 更新 N · 没变化 N」。
 *
 * 一万四千条要跑十几秒，只给一个转圈会让人以为卡死了；这四个数每 2 秒变一次，
 * 管理员一眼看得出它确实在动。
 */
const progressText = computed(() => {
  const run: PromptSyncRun | null | undefined = status.value?.sync
  if (!run) {
    return ''
  }
  return t('designkit.inspiration.sync.progress', {
    fetched: run.fetched,
    inserted: run.inserted,
    updated: run.updated,
    skipped: run.skipped,
  })
})

const lastRunFailed = computed(() => status.value?.sync?.status === 'failed')

/** 失败原因，后端给的中文原话。 */
const failureReason = computed(() => status.value?.sync?.error_message ?? '')

const libraryCountText = computed(() =>
  t('designkit.inspiration.sync.libraryCount', {
    prompts: status.value?.prompt_count ?? 0,
    categories: status.value?.category_count ?? 0,
  }),
)

// ---------------------------------------------------------------------------
// 读状态
// ---------------------------------------------------------------------------

/**
 * 读一次同步状态。
 *
 * @param silent 轮询时传 true：不把「刷新」按钮切成加载中——
 *               每 2 秒闪一下会让人以为页面在抽风。
 */
async function refreshStatus(silent = false): Promise<void> {
  controller?.abort()
  controller = new AbortController()
  if (!silent) {
    loading.value = true
  }
  statusError.value = ''
  try {
    const next = await getPromptSyncStatus(controller.signal)
    if (disposed) {
      return
    }
    status.value = next
    unavailable.value = false

    // 刚从「在跑」变成「不跑了」：这一次结束了，让页面把分类和列表重新拉一遍。
    if (wasRunning && !next.running) {
      stopPolling()
      emit('synced')
      if (next.sync?.status === 'succeeded') {
        appStore.showSuccess(next.message)
      } else if (next.sync?.status === 'failed') {
        appStore.showError(next.message)
      }
    }
    wasRunning = next.running

    if (next.running) {
      startPolling()
    } else {
      stopPolling()
    }
  } catch (error) {
    if (isCanceledError(error) || disposed) {
      return
    }
    if (isSyncUnavailableError(error)) {
      // 后端没装同步功能。这不是故障，也不该弹错误提示。
      unavailable.value = true
      stopPolling()
      return
    }
    statusError.value = errorText(error)
    stopPolling()
  } finally {
    if (!disposed) {
      loading.value = false
    }
  }
}

async function loadSettings(): Promise<void> {
  try {
    const next = await getPromptSyncSettings()
    if (disposed) {
      return
    }
    settings.value = next
    proxyID.value = next.proxy_id
  } catch (error) {
    if (isCanceledError(error) || disposed) {
      return
    }
    if (isSyncUnavailableError(error)) {
      unavailable.value = true
      return
    }
    proxyError.value = errorText(error)
  }
}

// ---------------------------------------------------------------------------
// 轮询
// ---------------------------------------------------------------------------

function startPolling(): void {
  if (pollTimer !== null) {
    return
  }
  pollDeadline = Date.now() + POLL_TIMEOUT_MS
  pollTimer = window.setInterval(() => {
    if (Date.now() > pollDeadline) {
      // 超时了：多半是服务重启把记录卡在「进行中」。停下来，
      // 后端那句话已经写着「可以再点一次」，管理员看得到出路。
      stopPolling()
      return
    }
    void refreshStatus(true)
  }, POLL_INTERVAL_MS)
}

function stopPolling(): void {
  if (pollTimer !== null) {
    window.clearInterval(pollTimer)
    pollTimer = null
  }
}

// ---------------------------------------------------------------------------
// 立即同步
// ---------------------------------------------------------------------------

async function start(): Promise<void> {
  if (startDisabled.value) {
    return
  }
  starting.value = true
  statusError.value = ''
  try {
    const started = await startPromptSync()
    if (disposed) {
      return
    }
    appStore.showInfo(started.message)
    // 先把界面切到「在跑」，别等第一次轮询回来才有反应。
    wasRunning = true
    if (status.value) {
      status.value = { ...status.value, running: true, sync: started.sync ?? status.value.sync }
    }
    startPolling()
    void refreshStatus(true)
  } catch (error) {
    if (disposed) {
      return
    }
    if (isSyncUnavailableError(error)) {
      unavailable.value = true
      return
    }
    // 409：已经有一次在跑（可能是定时任务刚好开始了）。后端的中文原样显示，
    // 然后接着轮询——那一次跑完了，这里照样能看到结果。
    statusError.value = errorText(error)
    appStore.showWarning(errorText(error))
    wasRunning = true
    startPolling()
    void refreshStatus(true)
  } finally {
    if (!disposed) {
      starting.value = false
    }
  }
}

// ---------------------------------------------------------------------------
// 换代理
// ---------------------------------------------------------------------------

/**
 * 选完立刻保存，**没有单独的「保存」按钮**。
 *
 * 理由：有保存按钮的话，管理员改完下拉框直接去点「立即同步」，
 * 跑的还是旧代理——而界面上显示的是新的那个，看不出任何异常，
 * 只会得到一次莫名其妙的失败。立刻保存就不存在这个中间状态。
 *
 * 保存失败时**把选择退回服务端那个值**：留着一个没生效的选择，
 * 就是上面说的那种「看着是新的、实际是旧的」。
 */
async function saveProxy(next: number | null): Promise<void> {
  if (settings.value === null || savingProxy.value) {
    return
  }
  savingProxy.value = true
  proxySaved.value = false
  proxyError.value = ''
  try {
    const saved = await updatePromptSyncSettings(next)
    if (disposed) {
      return
    }
    settings.value = saved
    proxyID.value = saved.proxy_id
    proxySaved.value = true
    window.setTimeout(() => {
      if (!disposed) {
        proxySaved.value = false
      }
    }, 3000)
  } catch (error) {
    if (disposed) {
      return
    }
    proxyError.value = errorText(error)
    appStore.showError(errorText(error))
    // 退回服务端的值，避免「界面上是新的、实际用的是旧的」。
    proxyID.value = settings.value?.proxy_id ?? null
  } finally {
    if (!disposed) {
      savingProxy.value = false
    }
  }
}

watch(proxyID, (next) => {
  // 首次加载和「保存失败退回」也会改这个值，那两种情况不要再发一次请求。
  if (settings.value === null || next === settings.value.proxy_id) {
    return
  }
  void saveProxy(next)
})

onMounted(() => {
  void loadSettings()
  void refreshStatus()
})

onBeforeUnmount(() => {
  disposed = true
  stopPolling()
  controller?.abort()
  controller = null
})

defineExpose({ refreshStatus })
</script>

<style scoped>
.dk-insp-sync__proxy {
  display: grid;
  gap: var(--dk-space-2);
  margin-top: var(--dk-space-4);
  padding-top: var(--dk-space-4);
  border-top: 1px solid var(--dk-border);
}

.dk-insp-sync__link {
  color: var(--dk-accent);
  text-decoration: underline;
}

.dk-insp-sync__link:hover {
  color: var(--dk-accent-strong);
}
</style>
