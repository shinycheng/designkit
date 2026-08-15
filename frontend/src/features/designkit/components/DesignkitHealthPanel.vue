<!--
  后端连通性面板。

  为什么要有它：出图功能还没做，页面上没有任何东西能证明「前端 ↔ 后端 ↔ 数据库」
  这条链路是通的。有了它，部署完打开工作台一眼就能看出地基有没有问题，
  不用去翻容器日志——使用者是非技术背景的运营。
-->
<template>
  <div
    class="mt-6 rounded-xl border p-4"
    :class="panelClass"
    role="status"
    data-test="designkit-health-panel"
  >
    <div class="flex flex-wrap items-center justify-between gap-3">
      <div class="min-w-0">
        <p class="text-sm font-semibold" :class="titleClass">
          {{ t('designkit.health.title') }}
        </p>
        <p class="mt-1 text-sm" :class="messageClass" data-test="designkit-health-message">
          {{ stateMessage }}
        </p>
      </div>
      <button
        type="button"
        class="btn btn-secondary btn-sm"
        :disabled="state === 'loading'"
        data-test="designkit-health-recheck"
        @click="check"
      >
        {{ t('designkit.health.recheck') }}
      </button>
    </div>

    <dl v-if="state === 'ok'" class="mt-3 grid gap-1 text-xs text-gray-600 dark:text-dark-300">
      <div class="flex gap-2">
        <dt class="shrink-0">{{ t('designkit.health.endpointLabel') }}</dt>
        <dd class="min-w-0 break-all font-mono">{{ endpoint }}</dd>
      </div>
      <div class="flex gap-2">
        <dt class="shrink-0">{{ t('designkit.health.statusLabel') }}</dt>
        <dd class="min-w-0 break-all font-mono">{{ health?.status || t('designkit.health.unknown') }}</dd>
      </div>
      <div class="flex gap-2">
        <dt class="shrink-0">{{ t('designkit.health.dbLabel') }}</dt>
        <dd class="min-w-0 break-all font-mono">{{ health?.db || t('designkit.health.unknown') }}</dd>
      </div>
    </dl>

    <!--
      后端活着但数据库不通。这一态必须单独存在：
      后端在这种情况下仍然返回 HTTP 200，前端如果只看「请求有没有报错」，
      会把它显示成绿色的「一切正常」——而这个面板存在的全部意义就是看出问题。
    -->
    <dl
      v-else-if="state === 'degraded'"
      class="mt-3 grid gap-1 text-xs text-amber-800 dark:text-amber-200"
    >
      <div class="flex gap-2">
        <dt class="shrink-0">{{ t('designkit.health.endpointLabel') }}</dt>
        <dd class="min-w-0 break-all font-mono">{{ endpoint }}</dd>
      </div>
      <div class="flex gap-2">
        <dt class="shrink-0">{{ t('designkit.health.dbLabel') }}</dt>
        <dd class="min-w-0 break-all font-mono" data-test="designkit-health-db">
          {{ health?.db || t('designkit.health.unknown') }}
        </dd>
      </div>
      <div class="flex gap-2">
        <dt class="shrink-0">{{ t('designkit.health.moduleLabel') }}</dt>
        <dd class="min-w-0 break-all font-mono">
          {{ health?.module_status || t('designkit.health.unknown') }}
        </dd>
      </div>
      <!-- 后端给的中文原因，直接显示，运营截图给管理员就够了 -->
      <ul
        v-if="degradedReasons.length"
        class="mt-1 list-disc space-y-1 pl-5"
        data-test="designkit-health-reasons"
      >
        <li v-for="(reason, i) in degradedReasons" :key="i">{{ reason }}</li>
      </ul>
      <p class="mt-1">{{ degradedHint }}</p>
    </dl>

    <div v-else-if="state === 'error'" class="mt-3 space-y-1 text-xs">
      <p class="text-gray-600 dark:text-dark-300">
        {{ t('designkit.health.endpointLabel') }}
        <span class="break-all font-mono">{{ endpoint }}</span>
      </p>
      <p v-if="httpStatusText" class="text-gray-600 dark:text-dark-300">{{ httpStatusText }}</p>
      <p class="break-all text-red-700 dark:text-red-300" data-test="designkit-health-error">
        {{ t('designkit.health.errorLabel') }}{{ errorDetail }}
      </p>
      <p class="text-gray-600 dark:text-dark-300">{{ t('designkit.health.failedHint') }}</p>
    </div>
  </div>
</template>

<script setup lang="ts">
import { computed, onMounted, ref } from 'vue'
import { useI18n } from 'vue-i18n'
import { DESIGNKIT_API_BASE_PATH, getHealth, toDesignkitApiError } from '../api'
import type { DesignkitHealth } from '../api/types'

type HealthState = 'loading' | 'ok' | 'degraded' | 'error'

const { t } = useI18n()

const state = ref<HealthState>('loading')
const health = ref<DesignkitHealth | null>(null)
const errorMessage = ref('')
const errorStatus = ref<number | null>(null)

// 只给人看，方便运营截图给管理员；真实 baseURL 由 @/api/client 决定。
const endpoint = `/api/v1${DESIGNKIT_API_BASE_PATH}/health`

async function check(): Promise<void> {
  state.value = 'loading'
  errorMessage.value = ''
  errorStatus.value = null
  try {
    health.value = await getHealth()
    // 后端在数据库不通、或模块降级时都返 200，所以这里必须看 body 里的字段，
    // 不能只看请求成没成功。module_status 是 undefined 时按 'ok' 处理
    // （旧版本后端没有这个字段，不要因此误报）。
    const dbOK = health.value?.db === 'ok'
    const moduleOK = (health.value?.module_status ?? 'ok') === 'ok'
    state.value = dbOK && moduleOK ? 'ok' : 'degraded'
  } catch (error) {
    const apiError = toDesignkitApiError(error)
    health.value = null
    errorMessage.value = apiError.message || ''
    errorStatus.value = typeof apiError.status === 'number' ? apiError.status : null
    state.value = 'error'
  }
}

const stateMessage = computed(() => {
  if (state.value === 'loading') return t('designkit.health.checking')
  if (state.value === 'ok') return t('designkit.health.ok')
  if (state.value === 'degraded') {
    return health.value?.db === 'ok'
      ? t('designkit.health.moduleDegraded')
      : t('designkit.health.dbFailed')
  }
  return t('designkit.health.failed')
})

const degradedReasons = computed(() => health.value?.degraded_reasons ?? [])

// 数据库不通和模块降级是两种完全不同的故障，提示语不能混：
// 前者是「数据库容器没起来」，后者是「有东西没配好，出不了图」。
const degradedHint = computed(() =>
  health.value?.db === 'ok'
    ? t('designkit.health.moduleDegradedHint')
    : t('designkit.health.dbFailedHint'),
)

const errorDetail = computed(() => errorMessage.value || t('designkit.health.unknown'))

const httpStatusText = computed(() =>
  errorStatus.value === null || errorStatus.value === 0
    ? ''
    : t('designkit.health.httpStatus', { status: errorStatus.value }),
)

const panelClass = computed(() => {
  if (state.value === 'ok') return 'border-emerald-200 bg-emerald-50 dark:border-emerald-900/70 dark:bg-emerald-950/30'
  if (state.value === 'degraded') return 'border-amber-200 bg-amber-50 dark:border-amber-900/70 dark:bg-amber-950/30'
  if (state.value === 'error') return 'border-red-200 bg-red-50 dark:border-red-900/70 dark:bg-red-950/30'
  return 'border-gray-200 bg-gray-50 dark:border-dark-700 dark:bg-dark-900/40'
})

const titleClass = computed(() => {
  if (state.value === 'ok') return 'text-emerald-800 dark:text-emerald-200'
  if (state.value === 'degraded') return 'text-amber-800 dark:text-amber-200'
  if (state.value === 'error') return 'text-red-800 dark:text-red-200'
  return 'text-gray-800 dark:text-dark-100'
})

const messageClass = computed(() => {
  if (state.value === 'ok') return 'text-emerald-700 dark:text-emerald-300'
  if (state.value === 'degraded') return 'text-amber-700 dark:text-amber-300'
  if (state.value === 'error') return 'text-red-700 dark:text-red-300'
  return 'text-gray-600 dark:text-dark-300'
})

onMounted(() => {
  void check()
})
</script>
