<!--
  商品图设置（**仅管理员**）。

  这一页管的是「出图这件事按什么规矩跑」：能选哪些比例、用哪个模型、
  一次最多排多少张、同时出几张、图片压到多大、一张多少钱、出问题找谁。

  版式和保存交互照上游「系统设置」（views/admin/SettingsView.vue）来，不自创一套：
  分区卡片 → 表单 → 最下面一个「保存设置」按钮 → 保存成功弹一条提示。
  卡片壳用 designkit 的 dk-panel（全站换肤之后它就是上游 `card` 在这儿的样子），
  跟工作台、灵感库那几页保持同一个观感。

  四件事值得先说清楚：

  1. **每一项都写了「这是干什么的」和（危险项才有的）「填错会怎样」。**
     使用者是电商运营不是运维。只写一个「同时出几张」的标签，他只会照直觉往大了填，
     而填大了不报错，只是所有人一起变慢，谁也查不出原因。

  2. **只提交改过的项。** 后端是「传了才改」的语义，`buildSettingsPatch()` 负责比对。
     整份提交的话，任何一次漏字段都会把那一项悄悄清空。

  3. **没填的单价留空，绝不写 0。** 价目表要真出一张图才知道。
     留空时工作台显示「价格待确认」；填 0 运营会以为免费，一口气排几十张。

  4. **灵感库同步的代理在这里只能看。** 它在灵感库那一页改。
     两个地方都能改、改了不同步的话，就会出现「这边显示走代理、那边实际直连」，
     而且怎么查都查不出来。
-->
<template>
  <AppLayout>
    <div class="dk-settings-page">
      <header class="dk-canvas-header">
        <div class="dk-page-head">
          <p class="dk-eyebrow">{{ t('designkit.brand') }}</p>
          <h1 class="dk-page-title">{{ t('designkit.settings.title') }}</h1>
          <p class="dk-page-desc">{{ t('designkit.settings.description') }}</p>
        </div>
      </header>

      <!-- 第一次加载 -->
      <p v-if="loading && server === null" class="dk-muted">
        {{ t('designkit.settings.loading') }}
      </p>

      <!-- 后端根本没装这个功能（404）。不是故障，出图照常按现有配置跑。 -->
      <div v-else-if="unavailable" class="dk-alert dk-alert--warning">
        <p class="dk-text-strong">{{ t('designkit.settings.unavailable') }}</p>
        <p class="dk-alert__sub">{{ t('designkit.settings.unavailableHint') }}</p>
      </div>

      <!-- 读失败 -->
      <div v-else-if="loadError && server === null" class="dk-alert dk-alert--danger">
        <p>{{ t('designkit.settings.loadFailed') }}</p>
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

      <!-- 表单 -->
      <form v-else-if="server !== null" class="dk-settings-form" novalidate @submit.prevent="save()">
        <!-- 上一次保存之后要说的话（并发数超过建议值、联系方式没填…） -->
        <div v-if="warnings.length > 0" class="dk-alert dk-alert--warning">
          <p class="dk-text-strong">{{ t('designkit.settings.warningTitle') }}</p>
          <p v-for="(line, index) in warnings" :key="index" class="dk-alert__sub">{{ line }}</p>
        </div>

        <!-- 改了「要重启才完全生效」的项 -->
        <div v-if="restartNote !== ''" class="dk-alert">
          <p class="dk-text-strong">{{ t('designkit.settings.restartTitle') }}</p>
          <p class="dk-alert__sub">{{ restartNote }}</p>
        </div>

        <!-- ── 出图 ─────────────────────────────────────── -->
        <SettingsSection
          :title="t('designkit.settings.sectionOutput')"
          :hint="t('designkit.settings.sectionOutputHint')"
          :disabled="saving"
        >
          <SettingsField
            :label="t('designkit.settings.ratiosLabel')"
            :hint="t('designkit.settings.ratiosHint')"
            :danger="t('designkit.settings.ratiosDanger', { max: limits.ratios_max })"
            :error="fieldErrors.ratios"
          >
            <RatioListEditor v-model="form.ratios" :max="limits.ratios_max" />
          </SettingsField>

          <SettingsField
            :label="t('designkit.settings.modelLabel')"
            :hint="t('designkit.settings.modelHint')"
            :danger="t('designkit.settings.modelDanger')"
            :error="fieldErrors.model"
            v-slot="{ fieldId }"
          >
            <input
              :id="fieldId"
              v-model="form.model"
              type="text"
              class="dk-input"
              autocomplete="off"
              spellcheck="false"
              :maxlength="limits.model_max_length"
              :placeholder="t('designkit.settings.modelPlaceholder')"
            />
          </SettingsField>
        </SettingsSection>

        <!-- ── 批量与速度 ───────────────────────────────── -->
        <SettingsSection
          :title="t('designkit.settings.sectionThroughput')"
          :hint="t('designkit.settings.sectionThroughputHint')"
          :disabled="saving"
        >
          <SettingsField
            :label="t('designkit.settings.maxBatchLabel')"
            :hint="t('designkit.settings.maxBatchHint')"
            :danger="t('designkit.settings.maxBatchDanger')"
            :error="fieldErrors.max_batch_items"
            v-slot="{ fieldId }"
          >
            <input
              :id="fieldId"
              v-model.number="form.max_batch_items"
              type="number"
              class="dk-input dk-settings-page__number"
              :min="limits.max_batch_items_min"
              :max="limits.max_batch_items_max"
              step="1"
            />
          </SettingsField>

          <SettingsField
            :label="t('designkit.settings.timeoutLabel')"
            :hint="t('designkit.settings.timeoutHint')"
            :danger="t('designkit.settings.timeoutDanger')"
            :error="fieldErrors.item_timeout_seconds"
            v-slot="{ fieldId }"
          >
            <input
              :id="fieldId"
              v-model.number="form.item_timeout_seconds"
              type="number"
              class="dk-input dk-settings-page__number"
              :min="limits.item_timeout_seconds_min"
              :max="limits.item_timeout_seconds_max"
              step="1"
            />
          </SettingsField>

          <SettingsField
            :label="t('designkit.settings.concurrencyLabel')"
            :hint="t('designkit.settings.concurrencyHint')"
            :danger="t('designkit.settings.concurrencyDanger', { safe: limits.worker_concurrency_safe })"
            :error="fieldErrors.worker_concurrency"
            v-slot="{ fieldId }"
          >
            <input
              :id="fieldId"
              v-model.number="form.worker_concurrency"
              type="number"
              class="dk-input dk-settings-page__number"
              :min="limits.worker_concurrency_min"
              :max="limits.worker_concurrency_max"
              step="1"
            />
            <!--
              超过建议值时立刻提醒，不等保存完才说 —— 等保存完再说的话，
              管理员已经离开这一页了。
            -->
            <p v-if="overSafeConcurrency" class="dk-note dk-note--warning dk-mt-2">
              {{ t('designkit.settings.concurrencyOverSafe', { safe: limits.worker_concurrency_safe }) }}
            </p>
          </SettingsField>
        </SettingsSection>

        <!-- ── 图片尺寸与计费档 ─────────────────────────── -->
        <SettingsSection
          :title="t('designkit.settings.sectionDimension')"
          :hint="t('designkit.settings.sectionDimensionHint')"
          :disabled="saving"
        >
          <SettingsField
            :label="t('designkit.settings.maxDimensionLabel')"
            :hint="t('designkit.settings.maxDimensionHint')"
            :danger="t('designkit.settings.maxDimensionDanger')"
            :error="fieldErrors.max_dimension"
            v-slot="{ fieldId }"
          >
            <input
              :id="fieldId"
              v-model.number="form.max_dimension"
              type="number"
              class="dk-input dk-settings-page__number"
              :min="limits.max_dimension_min"
              :max="limits.max_dimension_max"
              step="1"
            />
          </SettingsField>

          <div>
            <h3 class="dk-label">{{ t('designkit.settings.previewTitle') }}</h3>
            <PricingPreviewTable
              :ratios="form.ratios"
              :max-dimension="form.max_dimension"
              :unit-prices="form.unit_prices"
              :rate-multiplier="form.rate_multiplier"
              :dirty="dirty"
            />
          </div>
        </SettingsSection>

        <!-- ── 价格 ─────────────────────────────────────── -->
        <SettingsSection
          :title="t('designkit.settings.sectionPricing')"
          :hint="t('designkit.settings.sectionPricingHint')"
          :disabled="saving"
        >
          <div class="dk-alert">
            <p class="dk-text-strong">{{ t('designkit.settings.pricingUnconfirmedTitle') }}</p>
            <p class="dk-alert__sub">{{ t('designkit.settings.pricingUnconfirmedHint') }}</p>
          </div>

          <SettingsField
            v-for="tier in limits.tiers"
            :key="tier"
            :label="t('designkit.settings.unitPriceLabel', { tier })"
            :hint="tierHint(tier)"
            :error="fieldErrors.unit_prices[tier] ?? ''"
            v-slot="{ fieldId }"
          >
            <div class="dk-settings-page__price">
              <span class="dk-settings-page__currency" aria-hidden="true">$</span>
              <input
                :id="fieldId"
                v-model="form.unit_prices[tier]"
                type="text"
                inputmode="decimal"
                autocomplete="off"
                class="dk-input dk-settings-page__number"
                :placeholder="t('designkit.settings.unitPricePlaceholder')"
              />
              <!-- 留空 = 价格待确认。把这句话贴在输入框边上，别让人猜。 -->
              <span v-if="(form.unit_prices[tier] ?? '').trim() === ''" class="dk-note dk-note--warning">
                {{ t('designkit.settings.unitPriceUnconfirmed') }}
              </span>
            </div>
          </SettingsField>

          <SettingsField
            :label="t('designkit.settings.rateMultiplierLabel')"
            :hint="t('designkit.settings.rateMultiplierHint')"
            :danger="t('designkit.settings.rateMultiplierDanger')"
            :error="fieldErrors.rate_multiplier"
            v-slot="{ fieldId }"
          >
            <input
              :id="fieldId"
              v-model="form.rate_multiplier"
              type="text"
              inputmode="decimal"
              autocomplete="off"
              class="dk-input dk-settings-page__number"
              :placeholder="t('designkit.settings.rateMultiplierPlaceholder')"
            />
          </SettingsField>
        </SettingsSection>

        <!-- ── 出问题时找谁 ─────────────────────────────── -->
        <SettingsSection
          :title="t('designkit.settings.sectionContact')"
          :hint="t('designkit.settings.sectionContactHint')"
          :disabled="saving"
        >
          <SettingsField
            :label="t('designkit.settings.adminContactLabel')"
            :hint="t('designkit.settings.adminContactHint')"
            :danger="t('designkit.settings.adminContactDanger')"
            :error="fieldErrors.admin_contact"
            v-slot="{ fieldId }"
          >
            <input
              :id="fieldId"
              v-model="form.admin_contact"
              type="text"
              class="dk-input"
              autocomplete="off"
              :maxlength="limits.admin_contact_max_length"
              :placeholder="t('designkit.settings.adminContactPlaceholder')"
            />
            <p v-if="form.admin_contact.trim() === ''" class="dk-note dk-note--warning dk-mt-2">
              {{ t('designkit.settings.adminContactEmptyWarn') }}
            </p>
          </SettingsField>
        </SettingsSection>

        <!-- ── 图片自动清理（决策 17）───────────────────── -->
        <SettingsSection
          :title="t('designkit.settings.sectionCleanup')"
          :hint="t('designkit.settings.sectionCleanupHint')"
          :disabled="saving"
        >
          <SettingsField
            :label="t('designkit.settings.cleanupEnabledLabel')"
            :hint="t('designkit.settings.cleanupEnabledHint')"
            v-slot="{ fieldId }"
          >
            <label class="dk-settings-page__toggle">
              <input
                :id="fieldId"
                v-model="form.cleanup_enabled"
                type="checkbox"
                class="dk-settings-page__checkbox"
              />
              <span>
                {{
                  form.cleanup_enabled
                    ? t('designkit.settings.cleanupEnabledOn')
                    : t('designkit.settings.cleanupEnabledOff')
                }}
              </span>
            </label>
          </SettingsField>

          <!-- 勾上的那一刻就把后果亮出来，不等保存：删的是文件，找不回来。 -->
          <div v-if="form.cleanup_enabled" class="dk-alert dk-alert--danger">
            <p class="dk-text-strong">{{ t('designkit.settings.cleanupDangerTitle') }}</p>
            <p class="dk-alert__sub">{{ t('designkit.settings.cleanupDangerScope') }}</p>
            <p class="dk-alert__sub">{{ t('designkit.settings.cleanupDangerMoney') }}</p>
            <p class="dk-alert__sub">{{ t('designkit.settings.cleanupDangerBackup') }}</p>
          </div>

          <SettingsField
            :label="t('designkit.settings.cleanupDaysLabel')"
            :hint="t('designkit.settings.cleanupDaysHint')"
            :danger="t('designkit.settings.cleanupDaysDanger', { min: limits.cleanup_retention_days_min })"
            :error="fieldErrors.cleanup_retention_days"
            v-slot="{ fieldId }"
          >
            <input
              :id="fieldId"
              v-model.number="form.cleanup_retention_days"
              type="number"
              class="dk-input dk-settings-page__number"
              :min="limits.cleanup_retention_days_min"
              :max="limits.cleanup_retention_days_max"
              step="1"
            />
          </SettingsField>
        </SettingsSection>

        <!-- ── 灵感库同步（只读）───────────────────────── -->
        <SettingsSection
          :title="t('designkit.settings.sectionSync')"
          :hint="t('designkit.settings.sectionSyncHint')"
        >
          <div>
            <p class="dk-label">{{ t('designkit.settings.syncProxyLabel') }}</p>
            <p class="dk-text-strong">
              {{ promptSync.proxy_name || t('designkit.settings.syncProxyDirect') }}
            </p>
            <!-- 后端拼好的整句：写着「只影响同步，不影响出图」，必须原样显示 -->
            <p v-if="promptSync.message !== ''" class="dk-note dk-mt-2">{{ promptSync.message }}</p>
            <p class="dk-note dk-note--quiet dk-mt-2">{{ t('designkit.settings.syncWhyReadonly') }}</p>
            <p class="dk-mt-3">
              <RouterLink class="dk-settings-page__link" :to="promptSync.edit_path">
                {{ t('designkit.settings.syncOpen') }}
              </RouterLink>
            </p>
          </div>
        </SettingsSection>

        <!-- ── 保存 ─────────────────────────────────────── -->
        <div class="dk-settings-page__footer">
          <p v-if="dirty" class="dk-note dk-note--warning">
            {{ t('designkit.settings.unsavedHint') }}
          </p>
          <p v-else-if="savedAt !== ''" class="dk-note dk-note--success">
            {{ t('designkit.settings.savedAt', { time: savedAt }) }}
          </p>
          <span class="dk-settings-page__spacer" />
          <button
            type="button"
            class="dk-button dk-button--secondary"
            :disabled="!dirty || saving"
            @click="discard()"
          >
            {{ t('designkit.settings.discard') }}
          </button>
          <button type="submit" class="dk-button" :disabled="saving || !dirty">
            {{ saving ? t('designkit.settings.saving') : t('designkit.settings.save') }}
          </button>
        </div>
      </form>
    </div>
  </AppLayout>
</template>

<script setup lang="ts">
import { computed, onMounted, onUnmounted, reactive, ref } from 'vue'
import { useI18n } from 'vue-i18n'
import { RouterLink } from 'vue-router'
import AppLayout from '@/components/layout/AppLayout.vue'
import { useAppStore } from '@/stores'
import {
  buildSettingsPatch,
  errorText,
  getDesignkitSettings,
  hasSettingsChanges,
  isCanceledError,
  isSettingsUnavailableStatus,
  toFriendlyError,
  toSettingsForm,
  updateDesignkitSettings,
} from '../api'
import type {
  DesignkitSettings,
  DesignkitSettingsForm,
  DesignkitSettingsLimits,
  FriendlyError,
} from '../api'
// 灵感库页面的路径。后端也会在 prompt_sync.edit_path 里下发同一个值，
// 这里只是「后端还没读回来」时的兜底，两边写的是同一个地方。
import { DESIGNKIT_INSPIRATION_PATH } from '../nav'
import SettingsSection from '../components/settings/SettingsSection.vue'
import SettingsField from '../components/settings/SettingsField.vue'
import RatioListEditor from '../components/settings/RatioListEditor.vue'
import PricingPreviewTable from '../components/settings/PricingPreviewTable.vue'
// 页面用到的通用样式（卡片、按钮、输入框…），全局引入，Vite 会去重。
import '../components/designkit-ui.css'

const { t } = useI18n()
const appStore = useAppStore()

/** 服务端那一份（保存成功后会被替换）。用来比对「有没有改动」。 */
const server = ref<DesignkitSettings | null>(null)

/**
 * 表单那一份。
 *
 * 刻意做成「永远存在的 reactive 对象」而不是 `ref<Form | null>`：
 * 后者在模板里每处都要判空，漏一处就是运行时报错，而且类型检查也过不去。
 * 表单区整块套在 `v-if="server !== null"` 里，所以读到数据之前它根本不渲染。
 */
const form = reactive<DesignkitSettingsForm>({
  ratios: [],
  model: '',
  max_batch_items: 0,
  item_timeout_seconds: 0,
  max_dimension: 0,
  worker_concurrency: 0,
  admin_contact: '',
  cleanup_enabled: false,
  cleanup_retention_days: 0,
  unit_prices: {},
  rate_multiplier: '',
})

const loading = ref(false)
const saving = ref(false)
const loadError = ref<FriendlyError | null>(null)
/** 后端整组端点没挂（404）。不是故障，出图照常按现有配置跑。 */
const unavailable = ref(false)
/** 上一次保存之后后端说的话。 */
const warnings = ref<string[]>([])
const restartNote = ref('')
/** 上一次保存成功的时刻（本地时间，只显示时分秒）。 */
const savedAt = ref('')

let controller: AbortController | null = null
let disposed = false

/**
 * 每个字段的错误。**保存前在前端先查一遍**，这样能直接标在出错的那一格上；
 * 真正拦下来的护栏在后端，改网页绕不过去。
 */
const fieldErrors = reactive<{
  ratios: string
  model: string
  max_batch_items: string
  item_timeout_seconds: string
  max_dimension: string
  worker_concurrency: string
  admin_contact: string
  cleanup_retention_days: string
  rate_multiplier: string
  unit_prices: Record<string, string>
}>({
  ratios: '',
  model: '',
  max_batch_items: '',
  item_timeout_seconds: '',
  max_dimension: '',
  worker_concurrency: '',
  admin_contact: '',
  cleanup_retention_days: '',
  rate_multiplier: '',
  unit_prices: {},
})

/**
 * 允许范围。**由后端下发**，前端不另写一份 ——
 * 两边对不上就会出现「界面让填、保存被拒」这种谁也说不清的情况。
 *
 * server 还没读回来时给一份保守的兜底，只为了让模板不炸。
 */
const limits = computed<DesignkitSettingsLimits>(
  () =>
    server.value?.limits ?? {
      max_batch_items_min: 1,
      max_batch_items_max: 500,
      item_timeout_seconds_min: 10,
      item_timeout_seconds_max: 3600,
      max_dimension_min: 256,
      max_dimension_max: 8192,
      worker_concurrency_min: 1,
      worker_concurrency_max: 8,
      worker_concurrency_safe: 4,
      ratios_max: 12,
      model_max_length: 64,
      admin_contact_max_length: 200,
      cleanup_retention_days_min: 30,
      cleanup_retention_days_max: 3650,
      tiers: [],
      unit_price_max: '10',
      rate_multiplier_min: '0.01',
      rate_multiplier_max: '10',
    },
)

const dirty = computed(() => server.value !== null && hasSettingsChanges(form, server.value))

const overSafeConcurrency = computed(
  () => form.worker_concurrency > limits.value.worker_concurrency_safe,
)

/**
 * 灵感库同步的代理（**只读**）。
 *
 * 单独抽个 computed 而不是在模板里写 `server.prompt_sync`：
 * server 还没读回来时那样会炸，而且类型检查也过不去。
 */
const promptSync = computed(
  () =>
    server.value?.prompt_sync ?? {
      proxy_id: null,
      proxy_name: '',
      message: '',
      editable: false,
      edit_path: DESIGNKIT_INSPIRATION_PATH,
    },
)

function tierHint(tier: string): string {
  const key = `designkit.settings.unitPriceHint${tier}`
  const text = t(key)
  // 没配这一档的说明时，vue-i18n 会把键名原样返回 —— 那就什么都不显示，
  // 总比在界面上印一行 `designkit.settings.unitPriceHint8K` 强。
  return text === key ? '' : text
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
    const next = await getDesignkitSettings(controller.signal)
    if (disposed) {
      return
    }
    server.value = next
    Object.assign(form, toSettingsForm(next))
    unavailable.value = false
    clearFieldErrors()
  } catch (error) {
    if (isCanceledError(error) || disposed) {
      return
    }
    const friendly = toFriendlyError(error)
    if (isSettingsUnavailableStatus(friendly.status)) {
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

/** 放弃改动，退回服务端那一份。 */
function discard(): void {
  if (server.value === null) {
    return
  }
  Object.assign(form, toSettingsForm(server.value))
  clearFieldErrors()
}

function clearFieldErrors(): void {
  fieldErrors.ratios = ''
  fieldErrors.model = ''
  fieldErrors.max_batch_items = ''
  fieldErrors.item_timeout_seconds = ''
  fieldErrors.max_dimension = ''
  fieldErrors.worker_concurrency = ''
  fieldErrors.admin_contact = ''
  fieldErrors.cleanup_retention_days = ''
  fieldErrors.rate_multiplier = ''
  fieldErrors.unit_prices = {}
}

// ---------------------------------------------------------------------------
// 校验
// ---------------------------------------------------------------------------

/** 整数范围检查。填了汉字、小数、留空都算不合法。 */
function checkInt(value: number, min: number, max: number): boolean {
  return Number.isInteger(value) && value >= min && value <= max
}

/**
 * 保存前查一遍。返回 false 时不发请求，错误已经标在对应的那一格上。
 *
 * 判据跟后端 handler/settings_handler.go 逐条对齐。前端这一层只是为了
 * 「错在哪一格」看得见 —— 后端那一层才是真正拦得住的。
 */
function validate(): boolean {
  clearFieldErrors()
  const f = form
  const l = limits.value
  let ok = true

  // 比例：去掉空行之后再判。空行是「刚点了添加还没填」，直接忽略。
  const ratios = f.ratios.map((r) => r.trim()).filter((r) => r !== '')
  if (ratios.length === 0) {
    fieldErrors.ratios = t('designkit.settings.ratiosEmpty')
    ok = false
  } else if (ratios.length > l.ratios_max) {
    fieldErrors.ratios = t('designkit.settings.ratiosTooMany', { max: l.ratios_max })
    ok = false
  } else {
    const seen = new Set<string>()
    for (const ratio of ratios) {
      if (!/^\d+:\d+$/.test(ratio)) {
        fieldErrors.ratios = t('designkit.settings.ratiosInvalid', { ratio })
        ok = false
        break
      }
      if (seen.has(ratio)) {
        fieldErrors.ratios = t('designkit.settings.ratiosDuplicate', { ratio })
        ok = false
        break
      }
      seen.add(ratio)
    }
  }
  // 空行不提交上去。
  f.ratios = ratios

  const model = f.model.trim()
  if (model === '') {
    fieldErrors.model = t('designkit.settings.modelEmpty')
    ok = false
  } else if (model.length > l.model_max_length) {
    fieldErrors.model = t('designkit.settings.modelTooLong', { max: l.model_max_length })
    ok = false
  } else if (/\s/.test(model)) {
    fieldErrors.model = t('designkit.settings.modelHasSpace')
    ok = false
  }

  if (!checkInt(f.max_batch_items, l.max_batch_items_min, l.max_batch_items_max)) {
    fieldErrors.max_batch_items = t('designkit.settings.maxBatchRange', {
      min: l.max_batch_items_min,
      max: l.max_batch_items_max,
    })
    ok = false
  }

  if (!checkInt(f.item_timeout_seconds, l.item_timeout_seconds_min, l.item_timeout_seconds_max)) {
    fieldErrors.item_timeout_seconds = t('designkit.settings.timeoutRange', {
      min: l.item_timeout_seconds_min,
      max: l.item_timeout_seconds_max,
    })
    ok = false
  }

  if (!checkInt(f.max_dimension, l.max_dimension_min, l.max_dimension_max)) {
    fieldErrors.max_dimension = t('designkit.settings.maxDimensionRange', {
      min: l.max_dimension_min,
      max: l.max_dimension_max,
    })
    ok = false
  }

  if (!checkInt(f.worker_concurrency, l.worker_concurrency_min, l.worker_concurrency_max)) {
    fieldErrors.worker_concurrency = t('designkit.settings.concurrencyRange', {
      min: l.worker_concurrency_min,
      max: l.worker_concurrency_max,
    })
    ok = false
  }

  if (f.admin_contact.trim().length > l.admin_contact_max_length) {
    fieldErrors.admin_contact = t('designkit.settings.adminContactTooLong', {
      max: l.admin_contact_max_length,
    })
    ok = false
  }

  // 保留天数：开关关着也查 —— 这个数字会原样存进去，等哪天开开关时直接生效。
  if (
    !checkInt(f.cleanup_retention_days, l.cleanup_retention_days_min, l.cleanup_retention_days_max)
  ) {
    fieldErrors.cleanup_retention_days = t('designkit.settings.cleanupDaysRange', {
      min: l.cleanup_retention_days_min,
      max: l.cleanup_retention_days_max,
    })
    ok = false
  }

  // 单价：**空 = 还没测出来**，合法；0、负数、非数字不合法。
  const priceMax = Number(l.unit_price_max)
  for (const tier of l.tiers) {
    const raw = (f.unit_prices[tier] ?? '').trim()
    if (raw === '') {
      continue
    }
    const value = Number(raw)
    if (!Number.isFinite(value)) {
      fieldErrors.unit_prices[tier] = t('designkit.settings.unitPriceInvalid', { tier })
      ok = false
      continue
    }
    if (value <= 0) {
      fieldErrors.unit_prices[tier] = t('designkit.settings.unitPriceZero', { tier })
      ok = false
      continue
    }
    if (Number.isFinite(priceMax) && value > priceMax) {
      fieldErrors.unit_prices[tier] = t('designkit.settings.unitPriceTooBig', {
        tier,
        amount: raw,
        max: l.unit_price_max,
      })
      ok = false
    }
  }

  const multiplier = f.rate_multiplier.trim()
  if (multiplier !== '') {
    const value = Number(multiplier)
    if (!Number.isFinite(value)) {
      fieldErrors.rate_multiplier = t('designkit.settings.rateMultiplierInvalid')
      ok = false
    } else if (value < Number(l.rate_multiplier_min) || value > Number(l.rate_multiplier_max)) {
      fieldErrors.rate_multiplier = t('designkit.settings.rateMultiplierRange', {
        min: l.rate_multiplier_min,
        max: l.rate_multiplier_max,
      })
      ok = false
    }
  }

  return ok
}

// ---------------------------------------------------------------------------
// 保存
// ---------------------------------------------------------------------------

async function save(): Promise<void> {
  if (saving.value || server.value === null) {
    return
  }
  if (!validate()) {
    // 页面上已经标出是哪一格了；再弹一条提示，免得错误在屏幕外面看不见。
    appStore.showError(t('designkit.errors.badRequest'))
    return
  }

  const patch = buildSettingsPatch(form, server.value)
  if (Object.keys(patch).length === 0) {
    appStore.showInfo(t('designkit.settings.noChanges'))
    return
  }

  saving.value = true
  try {
    const next = await updateDesignkitSettings(patch)
    if (disposed) {
      return
    }
    server.value = next
    Object.assign(form, toSettingsForm(next))
    warnings.value = next.warnings
    restartNote.value = next.restart_note
    savedAt.value = new Date().toLocaleTimeString()
    clearFieldErrors()
    appStore.showSuccess(t('designkit.settings.saved'))
  } catch (error) {
    if (disposed) {
      return
    }
    // 后端的中文原样显示：它已经写清楚是哪一项、为什么不行。
    appStore.showError(errorText(error))
  } finally {
    if (!disposed) {
      saving.value = false
    }
  }
}

onMounted(() => {
  void reload()
})

onUnmounted(() => {
  disposed = true
  controller?.abort()
  controller = null
})
</script>

<style scoped>
.dk-settings-page {
  display: grid;
  gap: var(--dk-space-5);
  max-width: 56rem;
}

.dk-settings-form {
  display: grid;
  gap: var(--dk-space-5);
}

/* 数字和金额输入框不需要占满整行，太宽反而看不出该填几位 */
.dk-settings-page__number {
  max-width: 12rem;
}

.dk-settings-page__price {
  display: flex;
  align-items: center;
  gap: var(--dk-space-2);
  flex-wrap: wrap;
}

.dk-settings-page__currency {
  color: var(--dk-text-secondary);
}

/* 自动清理的开关：勾选框 + 当前状态一句话，点整行都能切换 */
.dk-settings-page__toggle {
  display: inline-flex;
  align-items: center;
  gap: var(--dk-space-2);
  cursor: pointer;
  color: var(--dk-text);
}

.dk-settings-page__checkbox {
  width: 1rem;
  height: 1rem;
  accent-color: var(--dk-accent);
  cursor: pointer;
}

.dk-settings-page__footer {
  display: flex;
  align-items: center;
  gap: var(--dk-space-3);
  flex-wrap: wrap;
  padding-top: var(--dk-space-2);
}

.dk-settings-page__spacer {
  flex: 1 1 auto;
}

.dk-settings-page__link {
  color: var(--dk-accent);
  text-decoration: underline;
}

.dk-settings-page__link:hover {
  color: var(--dk-accent-strong);
}
</style>
