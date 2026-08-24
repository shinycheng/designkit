<!--
  一批图的详情：这一批什么时候出的、出了多少张、花了多少钱、每一张长什么样。

  **进度和每一张图直接复用工作台的 `JobProgressPanel`**，不再写第二份。
  理由很实在：那块东西里面有两个会花钱的按钮（「停止排队」和「重试这一张」，
  决策 20 / 21），文案和二次确认都必须一字不差。同一件事写两份，
  早晚会一边改一边不改，而改错的代价是运营被多扣钱。

  所以这个文件只负责工作台没有的那一半：**这一批的档案信息**
  （名字、提交时间、完成时间、比例、状态、花了多少），以及返回列表。

  ⚠ 图必须按序号（seq）升序显示——「第几张」是发给 ERP 的对外契约，
  界面上的第 3 张必须就是接口里 seq=3 的那一张。后端查询里写死了 ORDER BY seq，
  `JobProgressPanel` 按数组原序渲染，不做二次排序，这条链不要在任何一环打断。
-->
<template>
  <div>
    <!-- 返回列表 -->
    <button type="button" class="dk-button dk-button--quiet dk-button--sm" @click="emit('back')">
      ← {{ t('designkit.job.backToList') }}
    </button>

    <!-- 这一批的档案信息 -->
    <section class="dk-panel dk-mt-3">
      <div class="dk-history-card__title">
        <span>{{ displayName }}</span>
        <GalleryStatusBadge v-if="jobStatus" :status="jobStatus" kind="job" />
        <!--
          删除这一批记录（软删）。**只在批次结束后显示**：删掉一个还在跑的批次，
          图照样出、钱照样扣，而「停止排队」的按钮跟着记录一起消失了 ——
          后端对没结束的批次也会拒绝，这里干脆不给入口。
        -->
        <button
          v-if="canDelete"
          type="button"
          class="dk-button dk-button--danger dk-button--sm dk-ml-auto"
          :disabled="deleting"
          @click="confirmingDelete = true"
        >
          {{ t('designkit.del.jobButton') }}
        </button>
      </div>

      <dl class="dk-job-facts dk-mt-3">
        <div>
          <dt>{{ t('designkit.job.createdAt') }}</dt>
          <dd>{{ formatDateTimeToMinute(job?.created_at) }}</dd>
        </div>
        <div v-if="job?.finished_at">
          <dt>{{ t('designkit.job.finishedAt') }}</dt>
          <dd>{{ formatDateTimeToMinute(job?.finished_at) }}</dd>
        </div>
        <div>
          <dt>{{ t('designkit.job.ratio') }}</dt>
          <dd>{{ ratioText }}</dd>
        </div>
        <div>
          <dt>{{ t('designkit.job.total', { count: job?.item_count ?? 0 }) }}</dt>
          <dd>
            {{
              t('designkit.job.counts', {
                success: job?.success_count ?? 0,
                fail: job?.fail_count ?? 0,
                cancelled: job?.cancelled_count ?? 0,
              })
            }}
          </dd>
        </div>
        <!-- 没结算完时 actual_cost 是 null，**不能显示成 $0.00**，改显示预计花费 -->
        <div>
          <dt>{{ costLabel }}</dt>
          <dd>{{ costAmount }}</dd>
        </div>
      </dl>

      <!--
        ⚠「已停止」不等于没花钱：已经开始生成的那几张会跑完并照常扣费（决策 21）。
        不写清楚会被投诉「我明明停了还扣我钱」。
      -->
      <p v-if="showCancelledCostNote" class="dk-alert dk-alert--warning dk-mt-3">
        {{ t('designkit.job.cancelledCostNote') }}
      </p>

      <!-- 读档案信息本身失败了（跟出图没关系，多半是网络或登录过期） -->
      <div v-if="headerError" class="dk-alert dk-alert--danger dk-mt-3">
        <p>{{ headerError }}</p>
        <button
          type="button"
          class="dk-button dk-button--secondary dk-button--sm dk-alert__action"
          @click="loadJob()"
        >
          {{ t('designkit.common.retry') }}
        </button>
      </div>
    </section>

    <!--
      进度 + 每一张图 + 停止排队 + 重试 + 看大图 + 下载。
      单价是重试确认弹窗要显示的（重试 = 重新出一张 = 重新收一次钱，决策 20），
      从 `GET /ratios` 里按这一批的比例查出来；查不到就是「价格待确认」，
      弹窗会显示「单价还没测出来」，**绝不会显示成免费**。
    -->
    <div class="dk-mt-4">
      <JobProgressPanel
        :job-uid="jobUid"
        :unit-price="unitPrice"
        @finished="loadJob()"
        @balance-changed="onBalanceChanged()"
        @continue-with-asset="onContinueWithAsset"
      />
    </div>

    <!--
      「打包下载这一批」：后端 GET /jobs/:uid/images.zip 把出成功的每一张
      打成一个 zip（包内文件名「第{seq}张.png」，跟界面上的序号严格对应）。
      地址要带登录凭证，不能 window.open（不带凭证头，拿到的是 401）——
      跟单张下载同一个做法：downloadContent 用 axios 取成 blob 再触发保存。
      没有出成功的图时不显示按钮（后端对空批次会 404，画一个必失败的按钮更糟）。
    -->
    <div v-if="canDownloadZip" class="dk-mt-3">
      <button
        type="button"
        class="dk-button dk-button--secondary dk-button--sm"
        :disabled="zipDownloading"
        @click="downloadZip()"
      >
        {{ zipDownloading ? t('designkit.common.loading') : t('designkit.gallery.downloadJob') }}
      </button>
    </div>

    <!--
      「删除这一批记录」的二次确认。文案的两句都跟后端语义核对过：
      列表和详情按 user_deleted_at 过滤 → 结果图确实从「我的图片」消失；
      软删不碰账 → 已扣的费用确实不退。
    -->
    <ConfirmDialog
      :show="confirmingDelete"
      :title="t('designkit.del.jobButton')"
      :message="t('designkit.del.jobConfirm')"
      :confirm-text="t('designkit.common.delete')"
      :cancel-text="t('designkit.common.cancel')"
      :danger="true"
      @confirm="confirmDeleteJob()"
      @cancel="confirmingDelete = false"
    />
  </div>
</template>

<script setup lang="ts">
import { computed, onMounted, onUnmounted, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import { useRouter } from 'vue-router'
import { formatDateTimeToMinute } from '@/utils/format'
import { useAppStore } from '@/stores/app'
import ConfirmDialog from '@/components/common/ConfirmDialog.vue'
import {
  DESIGNKIT_API_BASE_PATH,
  PRICE_UNCONFIRMED,
  deleteJob,
  downloadContent,
  errorText,
  formatMoney,
  getJob,
  isCanceledError,
  isTerminalJobStatus,
  listRatios,
  ratioLabel,
  toFriendlyError,
} from '../../api'
import type { DesignkitAsset, Job, Price } from '../../api'
import { CONTINUE_ASSET_KEY } from '../../stores/promptHandoff'
import { DESIGNKIT_WORKBENCH_PATH } from '../../nav'
import GalleryStatusBadge from './GalleryStatusBadge.vue'
import JobProgressPanel from '../workbench/JobProgressPanel.vue'

const props = defineProps<{ jobUid: string }>()

const emit = defineEmits<{
  (e: 'back'): void
  /** 钱动过了（停止排队 / 重试），外面的消费卡片该刷新了。 */
  (e: 'balanceChanged'): void
}>()

const { t } = useI18n()
const router = useRouter()
const appStore = useAppStore()

/**
 * 「用这张继续生成」——从「我的图片」点的时候。
 *
 * 这个页面本身没有第一步可以塞，所以**带着新商品图跳到工作台**。
 * 编号走 sessionStorage 交接（跟灵感库那条「用它生成」同一个套路，见
 * stores/promptHandoff.ts 的长注释）：网址保持干净的 /workbench，
 * 而且整页刷新之后还接得上。
 *
 * ⚠ 只传**编号**，不传图片本身。工作台拿到编号会自己去查这条素材。
 */
function onContinueWithAsset(asset: DesignkitAsset) {
  try {
    sessionStorage.setItem(CONTINUE_ASSET_KEY, asset.uid)
  } catch {
    // 隐私模式下 sessionStorage 会抛。跳过去之后运营重新传一张就是了，
    // 不值得为它拦住跳转。
  }
  router.push(DESIGNKIT_WORKBENCH_PATH)
}

const job = ref<Job | null>(null)
const headerError = ref<string | null>(null)
/** 这一批用的比例对应的单张预估价；查不到就是「价格待确认」。 */
const unitPrice = ref<Price>(PRICE_UNCONFIRMED)

let controller: AbortController | null = null
/** 已经查过单价的那个比例，避免每次刷新档案都重查一遍价目表。 */
let loadedPriceRatio = ''

/** 批次状态。单独拎出来，模板里就不必依赖类型收窄（`job` 可能是 null）。 */
const jobStatus = computed(() => job.value?.status ?? '')

const displayName = computed(() => {
  const name = (job.value?.name ?? '').trim()
  return name === '' ? t('designkit.job.untitled') : name
})

/** 例：`3:4 · 详情竖版`。没配中文说明的比例就只显示比例本身。 */
const ratioText = computed(() => {
  const ratio = job.value?.ratio ?? ''
  const label = ratioLabel(ratio)
  return label === '' ? ratio : `${ratio} · ${label}`
})

const settled = computed(() => typeof job.value?.actual_cost === 'string')
const costLabel = computed(() =>
  settled.value ? t('designkit.money.actual') : t('designkit.money.estimated'),
)
const costAmount = computed(() =>
  settled.value ? formatMoney(job.value?.actual_cost) : formatMoney(job.value?.estimated_cost),
)

/** 运营点过「停止排队」就要提醒：已经在生成的那几张照样扣费。 */
const showCancelledCostNote = computed(() => {
  const current = job.value
  if (!current) {
    return false
  }
  return current.status === 'cancelled' || current.cancel_requested_at !== null
})

// ---------------------------------------------------------------------------
// 打包下载这一批
// ---------------------------------------------------------------------------

/** 打包下载在途。期间按钮禁用，防连点重复打整包。 */
const zipDownloading = ref(false)

/** 有出成功的图才给打包入口（空批次后端会 404）。 */
const canDownloadZip = computed(() => (job.value?.success_count ?? 0) > 0)

/** Windows 上文件名里这几个字符会被拒绝，换成下划线（同 JobProgressPanel）。 */
function safeFileName(raw: string): string {
  return raw.replace(/[\\/:*?"<>|]/g, '_').trim()
}

/**
 * 打包下载：整批 zip 由后端现拼现流，前端只负责带凭证取回并触发保存。
 * 保存的文件名用批次名（后端的 Content-Disposition 也带同一个名字，
 * 但 downloadContent 走 blob 链接，浏览器只认 anchor 上的 download 属性）。
 */
async function downloadZip(): Promise<void> {
  if (zipDownloading.value) {
    return
  }
  zipDownloading.value = true
  try {
    await downloadContent(
      `${DESIGNKIT_API_BASE_PATH}/jobs/${encodeURIComponent(props.jobUid)}/images.zip`,
      `${safeFileName(displayName.value)}.zip`,
    )
  } catch (error) {
    if (!isCanceledError(error)) {
      appStore.showError(errorText(error))
    }
  } finally {
    zipDownloading.value = false
  }
}

// ---------------------------------------------------------------------------
// 删除这一批记录（软删）
// ---------------------------------------------------------------------------

/** 「删除这一批记录」的确认弹窗开没开。 */
const confirmingDelete = ref(false)
/** 删除接口在途。期间按钮禁用，防连点。 */
const deleting = ref(false)

/** 只有结束了的批次才给删除入口（没结束的后端也会拒绝，见按钮上的注释）。 */
const canDelete = computed(() => {
  const current = job.value
  return current !== null && isTerminalJobStatus(current.status)
})

/**
 * 确认删除：软删这一批，然后回到列表（列表会自己刷新，这一条已经不在了）。
 * 失败时留在原地：记录还在，运营能直接再试。
 */
async function confirmDeleteJob(): Promise<void> {
  confirmingDelete.value = false
  if (deleting.value) {
    return
  }
  deleting.value = true
  try {
    await deleteJob(props.jobUid)
    appStore.showToast('success', t('designkit.del.done'))
    emit('back')
  } catch (error) {
    appStore.showToast('error', deleteErrorMessage(error))
  } finally {
    deleting.value = false
  }
}

/**
 * 删失败给运营看什么：带 DK_ 码的业务错误显示后端的中文原文
 * （比如没结束的批次会收到「先点停止排队」那句），其余显示「没删掉，重试一次。」
 */
function deleteErrorMessage(error: unknown): string {
  const friendly = toFriendlyError(error)
  if (typeof friendly.code === 'string' && friendly.code.startsWith('DK_')) {
    return friendly.message
  }
  return t('designkit.del.failed')
}

/**
 * 读这一批的档案信息。
 *
 * 进度是 `JobProgressPanel` 自己在轮询，这里**只在打开时、以及跑完 / 动过钱之后**
 * 各读一次，不跟着一起轮询——同一份数据一秒钟问两遍是白白打后端。
 */
async function loadJob(): Promise<void> {
  controller?.abort()
  controller = new AbortController()
  headerError.value = null
  try {
    job.value = await getJob(props.jobUid, controller.signal)
    await loadUnitPrice()
  } catch (error) {
    if (isCanceledError(error)) {
      return
    }
    headerError.value = errorText(error)
  }
}

/**
 * 查这一批的比例对应的单价（重试确认弹窗要显示）。
 *
 * 查不到、或者价目表还没实测出来，就保持「价格待确认」——
 * **绝不能退化成 0**，那会让运营以为重试不要钱。
 */
async function loadUnitPrice(): Promise<void> {
  const ratio = job.value?.ratio
  if (!ratio) {
    return
  }
  // 同一个比例只查一次：这一批的比例不会中途变，档案信息每刷新一次就重查一遍
  // 价目表是白打后端。
  if (ratio === loadedPriceRatio) {
    return
  }
  loadedPriceRatio = ratio
  try {
    const list = await listRatios()
    const hit = list.ratios.find((option) => option.ratio === ratio)
    unitPrice.value = hit ? hit.unit_price : PRICE_UNCONFIRMED
  } catch {
    // 价目表读不到不影响看图，静默退回「价格待确认」即可。
    unitPrice.value = PRICE_UNCONFIRMED
  }
}

function onBalanceChanged(): void {
  emit('balanceChanged')
  void loadJob()
}

watch(
  () => props.jobUid,
  (uid) => {
    if (uid) {
      job.value = null
      loadedPriceRatio = ''
      unitPrice.value = PRICE_UNCONFIRMED
      void loadJob()
    }
  },
)

onMounted(() => {
  if (props.jobUid) {
    void loadJob()
  }
})

onUnmounted(() => {
  controller?.abort()
  controller = null
})
</script>
