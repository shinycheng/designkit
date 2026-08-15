<!--
  本月消费卡片（CLAUDE.md 决策 16 + 决策 19）。

  显示三个数：本月出图 N 张 / 花费 $X / 余额 $Y，外加可用额。
  放两个地方：生成工作台的角落、我的图片页的顶部。

  ⚠ 这个卡片有三处**不能删**的东西，删掉就会让运营误判：

  1. **口径说明那行小字**（设计定型 5.2）。
     张数和花费是**两套口径，刻意不统一**：
       - 张数来自图片表，删掉的图不算；
       - 花费来自账单，出图失败但已经扣过钱的也算。
     所以会出现「张数是 0，但花费不是 0」。不写这句话，运营会以为系统算错了，
     跑去问管理员，管理员也解释不清。这句话是文案的一部分，不是可选提示。

  2. **余额低时的提示 +「申请额度」按钮**（决策 19）。
     上游的充值、兑换码菜单对运营是隐藏的（决策 10），运营看到「余额不够」
     本来是死路一条，而管理员那边没有任何机制会主动知道有人卡住了。

  3. **接口拿不到就如实说「还看不到」，绝不显示成 $0.00。**
     「这个月花了 $0」和「还不知道花了多少」是两回事，后者显示成前者会让人误判。

  ⚠ 跟工作台里的 `workbench/UsageSummaryCard.vue` 是**同一件事的两份实现**
  （两个并行的改动各写了一份）。要留哪一份由整合的人定，但**不要两份都留**——
  金额相关的东西有两份，早晚会一边改一边不改。
  这一份是「页面顶部的宽版」，那一份是「工作台角落的窄版」。
-->
<template>
  <section class="dk-panel dk-panel--tight">
    <!-- 标题行 -->
    <div class="dk-panel-head dk-panel-head--tight">
      <h2 class="dk-panel-title">{{ t('designkit.usage.title') }}</h2>
      <button
        type="button"
        class="dk-button dk-button--quiet dk-button--sm"
        :disabled="loading"
        @click="refresh()"
      >
        {{ t('designkit.common.refresh') }}
      </button>
    </div>

    <!-- 正在读 -->
    <p v-if="loading && !summary" class="dk-muted">
      {{ t('designkit.common.loading') }}
    </p>

    <!-- 读失败：说清楚怎么办，并给「再试一次」 -->
    <div v-else-if="loadError" class="dk-alert dk-alert--danger">
      <p>{{ loadError }}</p>
      <button
        type="button"
        class="dk-button dk-button--secondary dk-button--sm dk-alert__action"
        @click="refresh()"
      >
        {{ t('designkit.common.retry') }}
      </button>
    </div>

    <!-- 接口还没上线：如实说「看不到」，不要编一个 0 出来 -->
    <p v-else-if="!summary" class="dk-muted">
      {{ t('designkit.usage.unavailable') }}
    </p>

    <template v-else>
      <!-- 三个数 -->
      <dl class="dk-usage-grid">
        <div class="dk-usage-cell">
          <dt>{{ t('designkit.usage.title') }}</dt>
          <dd>{{ t('designkit.usage.images', { count: summary?.image_count ?? 0 }) }}</dd>
        </div>

        <div class="dk-usage-cell">
          <dt>{{ t('designkit.money.actual') }}</dt>
          <dd>{{ t('designkit.usage.cost', { amount: formatMoney(summary?.cost) }) }}</dd>
        </div>

        <div class="dk-usage-cell">
          <dt>{{ t('designkit.money.balance') }}</dt>
          <dd :class="{ 'is-low': lowBalance }">
            {{ t('designkit.usage.balance', { amount: formatMoney(summary?.balance) }) }}
          </dd>
          <dd class="dk-usage-cell__sub">
            {{ t('designkit.money.available') }} {{ formatMoney(summary?.available) }}
          </dd>
        </div>
      </dl>

      <!--
        口径说明。后端可能下发自己的说法（note 字段），没有就用我们的文案。
        ⚠ 不要因为「太长了」把它折叠起来：运营看不到这句话就会以为系统算错了。
      -->
      <p class="dk-note dk-mt-3">{{ noteText }}</p>
      <p class="dk-note dk-note--quiet dk-mt-1">
        {{ t('designkit.money.currencyNote') }}
      </p>

      <!-- 余额偏低：给提示 + 给出路（决策 19） -->
      <div v-if="lowBalance" class="dk-alert dk-alert--warning dk-mt-3">
        {{ t('designkit.errors.balance') }}
      </div>

      <!-- 申请额度：一直显示，因为这是运营唯一能自助求助的入口 -->
      <div class="dk-inline-actions dk-mt-3">
        <button
          type="button"
          class="dk-button dk-button--secondary dk-button--sm"
          @click="quotaOpen = true"
        >
          {{ t('designkit.quota.request') }}
        </button>
        <span v-if="adminContact" class="dk-note">
          {{ t('designkit.quota.contact', { contact: adminContact }) }}
        </span>
      </div>
    </template>

    <!--
      申请额度弹窗。刻意**复用**工作台那一个，不再写第二份：
      这是会影响钱的流程，两份实现早晚会一边改一边不改。
      ⚠ 那个弹窗的请求写在 `workbench/jobApi.ts` 里，它自己的注释说了
      「接口层补齐之后就删掉」——真删的时候记得这里也在用它。
    -->
    <QuotaRequestDialog :show="quotaOpen" @close="onQuotaDialogClose()" />
  </section>
</template>

<script setup lang="ts">
import { computed, onMounted, onUnmounted, ref } from 'vue'
import { useI18n } from 'vue-i18n'
import { errorText, formatMoney, getUsageSummary, isCanceledError } from '../api'
import type { UsageSummary } from '../api'
import QuotaRequestDialog from './workbench/QuotaRequestDialog.vue'

const props = withDefaults(
  defineProps<{
    /**
     * 余额低于这个数（美元）就提示「余额不够」并把「申请额度」显眼化。
     *
     * 5 美元大约是十几张图，够运营反应过来去申请，又不至于天天弹。
     * 真正拦住透支的是提交时后端的余额检查，这里只是提前打个招呼。
     */
    lowBalanceThreshold?: number
  }>(),
  { lowBalanceThreshold: 5 },
)

const { t } = useI18n()

const summary = ref<UsageSummary | null>(null)
const loading = ref(false)
const loadError = ref<string | null>(null)
const quotaOpen = ref(false)

let controller: AbortController | null = null

/**
 * 管理员联系方式。
 *
 * ⚠ 后端 `GET /me/usage/summary` 的响应里有 `admin_contact`（见
 * `backend/internal/designkit/handler/dto.go` 的 `usageSummaryDTO`），
 * 但前端 `api/types.ts` 的 `UsageSummary` 还没有这个字段——那个目录不归本文件改，
 * 所以这里就地取一下，取不到就当没配置（不显示那一行）。
 * api 层补上字段之后，这里可以直接改成 `summary.value?.admin_contact`。
 */
const adminContact = computed(() => {
  const raw = summary.value as unknown as { admin_contact?: unknown } | null
  return typeof raw?.admin_contact === 'string' ? raw.admin_contact.trim() : ''
})

/**
 * 口径说明那句话。后端下发了自己的说法就用它的，没有就用我们的文案。
 * **不管用哪一句，这一行都必须显示出来**（设计定型 5.2）。
 */
const noteText = computed(() => {
  const fromServer = (summary.value?.note ?? '').trim()
  return fromServer === '' ? t('designkit.usage.note') : fromServer
})

/** 余额是不是已经低到该提醒了。读不出数字就不提醒（宁可不说，也不要瞎说）。 */
const lowBalance = computed(() => {
  if (!summary.value) {
    return false
  }
  const value = Number(summary.value.available)
  return Number.isFinite(value) && value < props.lowBalanceThreshold
})

/** 重新读一次这三个数。工作台提交完一批之后可以调它（组件暴露了出去）。 */
async function refresh(): Promise<void> {
  controller?.abort()
  controller = new AbortController()
  loading.value = true
  loadError.value = null
  try {
    summary.value = await getUsageSummary(controller.signal)
  } catch (error) {
    if (isCanceledError(error)) {
      return
    }
    loadError.value = errorText(error)
  } finally {
    loading.value = false
  }
}

/** 关掉申请额度弹窗之后顺手刷一下：管理员有可能当场就加了额度。 */
function onQuotaDialogClose(): void {
  quotaOpen.value = false
  void refresh()
}

onMounted(() => {
  void refresh()
})

onUnmounted(() => {
  controller?.abort()
  controller = null
})

defineExpose({ refresh })
</script>
