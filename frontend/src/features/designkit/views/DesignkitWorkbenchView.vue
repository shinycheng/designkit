<!--
  生成工作台。

  一条完整的路：**上传商品图 → 选比例 → 写提示词 → 看预估 → 确认提交 → 看进度 → 看结果图**。

  这个页面唯一的责任是「把这几步串起来」，每一步的细节都在
  components/workbench/ 下的小组件里。串起来的时候有四件事只能在这一层做：

  1. **防重复标识（提交）**：打开确认弹窗的那一刻生成一个存住，
     整个确认流程复用同一个。运营手滑双击 = 两个批次 = 两份钱。
     提交成功之后立刻丢掉，下一批重新生成——不同批次绝不能复用。
  2. **报价防抖**：选图、打字、换比例都会改变要出几张，每敲一个字就发一次请求
     既打后端也让数字乱跳。停 400 毫秒再算。
  3. **余额不足要给出路**（决策 19）：后端返回 402 时把它的中文原样显示，
     旁边必须有「申请额度」按钮——充值和兑换码菜单对运营是隐藏的，
     只说「余额不足」等于把人堵死。
  4. **提交按钮防重复点击**：submitting 期间按钮禁用，请求在途不再发第二次。

  ⚠ 版式（老 designkit 的「studio」）：**中间画布区 + 右边 400px 配置区**。
  画布区放「上传的商品图」和「出图结果」，配置区放「比例 / 提示词 / 预估 / 提交」。
  屏幕窄于 1100px（笔记本竖屏、平板、手机）时配置区变成**从底部拉起的抽屉**，
  收起时只留一条「出图设置」按钮，正文照样看得全。版式规则在
  components/designkit-ui.css 的「工作台：画布 + 配置区」一节。

  外壳用上游的 `AppLayout`（左侧边栏 + 右内容区），跟 /dashboard、/keys、
  /subscriptions 这些页面完全一致。
  （2026-08-13 改：原来用的是我们自己那份窄图标栏外壳 DesignkitShell，
   monica 要「和其他侧栏一样，直接显示在当前页面的右侧」，所以换回来了。
   配色仍然是 designkit 那一套——换肤层是全局的，跟用哪个外壳无关。）
-->
<template>
  <AppLayout>
    <div class="dk-studio" :class="{ 'is-config-open': configOpen }">
      <!-- ── 中间：画布（上传的图 + 出图结果） ──────────────── -->
      <section class="dk-canvas">
        <header class="dk-canvas-header">
          <div class="dk-page-head">
            <p class="dk-eyebrow">{{ t('designkit.brand') }}</p>
            <h1 class="dk-page-title">{{ t('designkit.workbench.title') }}</h1>
            <p class="dk-page-desc">{{ t('designkit.workbench.description') }}</p>
          </div>
          <p class="dk-canvas-status">{{ batchSummary }}</p>
        </header>

        <div class="dk-canvas-body">
          <!--
            本月出图 N 张 / 花费 $X / 余额 $Y（决策 16）。
            用的是 components/UsageCard.vue，不再写第二份——
            金额相关的东西有两份，早晚会一边改一边不改。
            它是横排三格的宽卡片，所以放在画布区顶部，不塞进 400px 的配置区。
          -->
          <UsageCard ref="usageCard" />

          <AssetUploader ref="assetUploader" v-model="assets" />

          <!-- 提交之后：进度和结果图 -->
          <JobProgressPanel
            v-if="currentJobUid !== ''"
            :key="currentJobUid"
            :job-uid="currentJobUid"
            :unit-price="unitPrice"
            @finished="onJobFinished"
            @balance-changed="refreshUsage"
            @continue-with-asset="onContinueWithAsset"
            @upscaled-asset="onUpscaledAsset"
          />

          <!-- 还没提交过：画布上先摆一句话，别是一片空白 -->
          <div v-else class="dk-result-state">
            <p class="dk-result-state__title">出好的图会显示在这里</p>
            <p class="dk-result-state__description">
              先选商品图，在右侧配好提示词和比例，点「提交出图」。
            </p>
          </div>
        </div>
      </section>

      <!-- 窄屏才出现：点开右边那块配置区。带上张数，收着也知道排了多少。 -->
      <button type="button" class="dk-config-fab" @click="configOpen = true">
        出图设置 · {{ t('designkit.common.pieces', { count: assets.length * allPrompts.length }) }}
      </button>

      <!-- 抽屉拉起时点空白处收起来 -->
      <button
        type="button"
        class="dk-config-scrim"
        aria-label="收起出图设置"
        @click="configOpen = false"
      ></button>

      <!-- ── 右边：配置区（比例 / 提示词 / 预估 / 提交） ───── -->
      <aside class="dk-config" aria-label="出图设置">
        <div class="dk-config__head">
          <span class="dk-panel-title">出图设置</span>
          <button
            type="button"
            class="dk-button dk-button--quiet dk-button--sm"
            @click="configOpen = false"
          >
            收起
          </button>
        </div>

        <div class="dk-config-scroll">
          <!--
            ⚠ 顺序必须跟标题里的编号一致：第二步（提示词）在上，第三步（比例）在下。
            原来是 RatioPicker 排在最前面，于是配置区从上往下读是
            「第三步 → 第二步 → 花费预估」，monica 2026-08-14 指出来的就是这个。
            标题文案在 i18n 的 workbench.stepUpload/stepPrompt/stepRatio，
            改这里的排列顺序时记得回去对一眼编号。
          -->

          <!--
            从灵感库带过来的提示词。**只有真的带过来了才出现**，
            平时这一块不占地方。
            它跟下面手写的那一块是分开的两份：手写框是「一行一条」的自由文本，
            带过来的这些是已经填好占位符的完整段落，混进同一个输入框会被
            换行切碎（提示词里本来就有换行）。
          -->
          <section v-if="libraryPrompts.length > 0" class="dk-panel dk-config-section">
            <header class="dk-panel-head dk-panel-head--tight">
              <div>
                <h2 class="dk-panel-title">{{ t('designkit.inspiration.fromLibraryTitle') }}</h2>
                <p class="dk-panel-hint">{{ t('designkit.inspiration.fromLibraryHint') }}</p>
              </div>
              <button
                type="button"
                class="dk-button dk-button--quiet dk-button--sm"
                @click="clearLibraryPrompts()"
              >
                {{ t('designkit.common.clear') }}
              </button>
            </header>
            <ul class="dk-prompt-list dk-mt-3">
              <li
                v-for="(item, index) in libraryPrompts"
                :key="`${index}-${item.uid ?? ''}`"
                class="dk-prompt-item"
              >
                <span class="dk-prompt-item__index">{{ index + 1 }}</span>
                <!--
                  只显示两行：灵感库那批词动辄上千字，全展开会把右边这条 400px
                  的配置区顶成一条长得看不到底的滚动条，「提交出图」按钮直接被挤到天边。
                  想看全文回灵感库点开那条就行。
                -->
                <span class="dk-prompt-item__text dk-clamp-2" :title="item.text">
                  {{ item.text }}
                </span>
                <button
                  type="button"
                  class="dk-button dk-button--quiet dk-button--sm"
                  @click="removeLibraryPrompt(index)"
                >
                  {{ t('designkit.common.remove') }}
                </button>
              </li>
            </ul>
          </section>

          <!--
            第二步。2026-08-14 起它不再是「手写框」，而是
            「选分类 → 写商品特点 → AI 看图推荐 → 合成一条可编辑的提示词」。

            它需要知道第一步选了几张图、以及这些图的编号 —— AI 要看着图才推荐得出来。
            ⚠ **整批都发**（后端封顶 3 张），不是只发第一张：那条提示词要用在整批上，
            只看第一张的话，后面几张是别的角度甚至别的颜色时就不贴了。
            但仍然只出**一条**提示词、每张商品图各出 1 张图 —— 不是每张图各推一次，
            那会让等待和花费成倍涨。
          -->
          <PromptEditor
            v-model="prompts"
            class="dk-config-section"
            :asset-count="assets.length"
            :first-asset-uid="assets.length > 0 ? assets[0].uid : ''"
            :extra-asset-uids="assets.slice(1).map((a) => a.uid)"
          />
          <RatioPicker v-model="ratio" class="dk-config-section" />
          <EstimatePanel
            v-model:name="jobName"
            class="dk-config-section"
            :asset-count="assets.length"
            :prompt-count="allPrompts.length"
            :estimate="estimate"
            :price-note="priceNote"
            :loading="estimating"
            :load-error="estimateError"
            :submitting="submitting"
            :submit-error="submitError"
            @submit="openConfirmDialog"
            @request-quota="quotaDialogOpen = true"
          />
        </div>
      </aside>
    </div>

    <!-- 提交前的二次确认：这是花钱前的最后一道关 -->
    <ConfirmSubmitDialog
      :show="confirmDialogOpen"
      :count="estimate?.item_count ?? 0"
      :cost="estimatedCost"
      :unit-price="unitPrice"
      :price-note="priceNote"
      :submitting="submitting"
      @confirm="submit"
      @cancel="closeConfirmDialog"
    />

    <!-- 申请额度（决策 19） -->
    <QuotaRequestDialog :show="quotaDialogOpen" @close="quotaDialogOpen = false" />
  </AppLayout>
</template>

<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import AppLayout from '@/components/layout/AppLayout.vue'
import { useAppStore } from '@/stores/app'
import {
  getAsset,
  PRICE_UNCONFIRMED,
  createJob,
  errorText,
  isCanceledError,
  newIdempotencyKey,
  toFriendlyError,
} from '../api'
import type { DesignkitAsset, JobEstimate, Price } from '../api'
import { CONTINUE_ASSET_KEY, usePromptHandoffStore } from '../stores/promptHandoff'
import type { HandoffPrompt } from '../stores/promptHandoff'
import UsageCard from '../components/UsageCard.vue'
import AssetUploader from '../components/workbench/AssetUploader.vue'
import PromptEditor from '../components/workbench/PromptEditor.vue'
import RatioPicker from '../components/workbench/RatioPicker.vue'
import EstimatePanel from '../components/workbench/EstimatePanel.vue'
import ConfirmSubmitDialog from '../components/workbench/ConfirmSubmitDialog.vue'
import QuotaRequestDialog from '../components/workbench/QuotaRequestDialog.vue'
import JobProgressPanel from '../components/workbench/JobProgressPanel.vue'
import { estimateJobWithNote } from '../components/workbench/jobApi'
import type { SubmitErrorView } from '../components/workbench/viewTypes'
// 页面里用到的通用样式（按钮、卡片、画布、配置区……），全局引入，Vite 会去重。
import '../components/designkit-ui.css'

/** 报价防抖：手一停再算，别每敲一个字就发一次。 */
const ESTIMATE_DEBOUNCE_MS = 400

const { t } = useI18n()
const appStore = useAppStore()
const handoff = usePromptHandoffStore()

// ---- 版式（跟业务无关，只管界面怎么摆） ----
/**
 * 窄屏时右边那块配置区是不是拉起来了。
 *
 * 屏幕宽于 1100px 时它一直贴在右边，这个开关不起作用（CSS 里就没读它）。
 */
const configOpen = ref(false)

// ---- 运营选的东西 ----
/** 已经传好的商品图，**顺序就是出图的外层顺序**。 */
const assets = ref<DesignkitAsset[]>([])

/** 第一步那个上传组件。「用这张继续生成」要调它的 addExistingAsset。 */
const assetUploader = ref<InstanceType<typeof AssetUploader> | null>(null)

/** 最终提示词（第二步 AI 配出来的那条，可编辑）。 */
const prompts = ref<string[]>([])

/**
 * 从灵感库带过来的提示词（点了「用它生成」的那些）。
 *
 * 跟第二步那份**分开存**：第二步现在是 AI 合成的一条完整提示词，
 * 而灵感库带过来的是已经填好占位符的整段（本身就可能有换行），
 * 塞进同一个地方会被按换行切碎，一条变成七八条 —— 张数和钱都跟着翻倍。
 *
 * 出图时两份合起来（见 allPrompts），**灵感库的排在前面**。
 */
const libraryPrompts = ref<HandoffPrompt[]>([])

/** 出图比例，由 RatioPicker 拉到列表后填上默认值。 */
const ratio = ref('')

/** 给这批起的名字，可以不填。 */
const jobName = ref('')

/**
 * 「用这张继续生成」：把刚出好的那张图（已经在服务端变成一条新商品图）
 * 交给第一步的上传组件。
 *
 * ⚠ **必须调组件的 addExistingAsset，不能直接改 assets 这个数组。**
 * AssetUploader 是单向的：它渲染自己内部的 entries，`modelValue` 从头到尾没被读过。
 * 直接改数组的话 ① 界面上看不见（还显示「还没有选商品图」），
 * ② 运营下次再传图时会被内部列表覆盖掉，那张图静默消失。
 * 2026-08-14 monica 实测发现的就是①。
 *
 * 返回 false = 没加成（重复了或到上限了），这时候不要报「已加好」骗人。
 */
/**
 * 从「我的图片」点「用这张继续生成」跳过来时，把那条商品图接进第一步。
 *
 * 编号放在 sessionStorage 里（见 stores/promptHandoff.ts 的 CONTINUE_ASSET_KEY）。
 * **取到就立刻删掉**：不删的话运营下次自己打开工作台，那张图会莫名其妙又出现一次，
 * 而张数是按商品图条数算的 —— 他会多花一份钱还不知道为什么。
 */
async function pickUpContinuedAsset() {
  let uid = ''
  try {
    uid = sessionStorage.getItem(CONTINUE_ASSET_KEY) ?? ''
    sessionStorage.removeItem(CONTINUE_ASSET_KEY)
  } catch {
    // 隐私模式下 sessionStorage 会抛。跳过去之后运营重新传一张就是了。
    return
  }
  if (uid === '') {
    return
  }
  try {
    const asset = await getAsset(uid)
    onContinueWithAsset(asset)
  } catch {
    // 那条素材没了（被清理过）。静默跳过 —— 弹一句「上一张图找不到了」
    // 对运营没有任何用，他重新传一张就是了。
  }
}

function onContinueWithAsset(asset: DesignkitAsset): boolean {
  const added = assetUploader.value?.addExistingAsset(asset) ?? false
  appStore.showToast(
    added ? 'success' : 'info',
    added
      ? t('designkit.gallery.continueDone')
      : t('designkit.gallery.continueAlreadyAdded'),
  )
  return added
}

/**
 * 「高清放大」放完了：把产物（一条新的商品图）塞进第一步。
 *
 * **静默加**，不再弹提示：「放大完成，已存为新图」那条 toast 已经由
 * 进度面板发过了，这里再弹一条就是同一件事说两遍。加不进去（重复 / 到上限）
 * 也不吭声——重复恰恰说明它已经在列表里了，正是运营想要的状态。
 * 走 addExistingAsset 而不是直接改数组的理由见 onContinueWithAsset 上面那段。
 */
function onUpscaledAsset(asset: DesignkitAsset): void {
  assetUploader.value?.addExistingAsset(asset, t('designkit.upscale.name'))
}

// ---- 报价 ----
const estimate = ref<JobEstimate | null>(null)
/** 价格待确认时后端给的中文说明；空串表示价格已确认。 */
const priceNote = ref('')
const estimating = ref(false)
const estimateError = ref<string | null>(null)

// ---- 提交 ----
const submitting = ref(false)
const submitError = ref<SubmitErrorView | null>(null)
const confirmDialogOpen = ref(false)
const quotaDialogOpen = ref(false)
/** 提交成功之后盯着的那个批次；空串 = 还没提交过。 */
const currentJobUid = ref('')

const usageCard = ref<InstanceType<typeof UsageCard> | null>(null)

/**
 * 提交用的防重复标识。
 *
 * **打开确认弹窗时生成一个存住**：运营在弹窗上犹豫、点了又点、网络超时后重发，
 * 都用同一个，后端据此认出「这是同一次提交」，只建一个批次、只扣一份钱。
 * 提交成功后清空——不同批次绝不能复用同一个（同一个标识配不同内容会被拒绝）。
 */
let submitIdempotencyKey = ''

let estimateTimer: number | null = null
let estimateController: AbortController | null = null

/**
 * 这一批实际要用的全部提示词，**顺序就是出图的内层顺序**。
 *
 * 灵感库带过来的排在手写的前面：运营是先挑了词、再补两句自己的，
 * 出图顺序跟他脑子里的顺序一致，看结果时才对得上号。
 */
const allPrompts = computed<string[]>(() => [
  ...libraryPrompts.value.map((one) => one.text),
  ...prompts.value,
])

// 页面一挂载就看一眼有没有「用这张继续生成」带过来的商品图。
onMounted(() => {
  void pickUpContinuedAsset()
})

/**
 * 接住灵感库带过来的提示词。
 *
 * 用 watch 而不是只在 onMounted 里取一次：从灵感库跳过来时这个页面可能是
 * 新挂载的，也可能是已经挂载着的（路由被缓存、或者运营在两个页面之间来回切），
 * 只写 onMounted 的话第二次带过来就没反应了 —— 而运营会以为是自己没点中。
 * `immediate` 覆盖「新挂载」那一支。
 *
 * takeAll() 是**取走并清空**：留副本的话，运营在这里删掉一条、
 * 一刷新它又冒出来，怎么删都删不掉。
 */
watch(
  () => handoff.count,
  (count) => {
    if (count === 0) {
      return
    }
    const taken = handoff.takeAll()
    if (taken.length === 0) {
      return
    }
    libraryPrompts.value = [...libraryPrompts.value, ...taken]
  },
  { immediate: true },
)

/** 从这一批里去掉灵感库带来的某一条。 */
function removeLibraryPrompt(index: number): void {
  libraryPrompts.value = libraryPrompts.value.filter((_, i) => i !== index)
}

/** 把灵感库带来的全部清掉（手写的那些不受影响）。 */
function clearLibraryPrompts(): void {
  libraryPrompts.value = []
}

/** 单张预估价；报价还没回来时算「待确认」，绝不当成 0。 */
const unitPrice = computed<Price>(() => estimate.value?.unit_price ?? PRICE_UNCONFIRMED)

/** 这一批的预估总花费；同上。 */
const estimatedCost = computed<Price>(() => estimate.value?.estimated_cost ?? PRICE_UNCONFIRMED)

/** 三样都齐了才算得出价。 */
const canEstimate = computed(
  () => ratio.value !== '' && assets.value.length > 0 && allPrompts.value.length > 0,
)

/**
 * 画布右上角那行小字：「3 张商品图 × 4 条提示词 = 12 张」。
 *
 * 只是把两个数乘一下给人看，**不参与报价**——真正花多少钱一律以
 * 后端 `estimate` 为准（价格待确认时那边会说「算不出总价」，绝不显示 $0）。
 */
const batchSummary = computed(() =>
  t('designkit.workbench.summaryFormula', {
    assets: assets.value.length,
    prompts: allPrompts.value.length,
    total: assets.value.length * allPrompts.value.length,
  }),
)

// ---------------------------------------------------------------------------
// 报价
// ---------------------------------------------------------------------------

// 三个来源里任何一个变了都要重算。上传组件和提示词组件每次都吐一个新数组，
// 所以按引用比较就够，不用 deep（deep 会在每次轮询无关的改动上白跑一遍）。
// 提示词看的是 allPrompts（手写的 + 灵感库带来的），少看一份就会出现
// 「界面上有 6 条、报价按 4 条算」——那是会让人少估钱的错。
watch([assets, allPrompts, ratio], () => {
  scheduleEstimate()
})

// 排下去之后把窄屏的配置抽屉收起来，让运营直接看到画布上的进度。
// 纯版式，不碰提交流程。
watch(currentJobUid, (uid) => {
  if (uid !== '') {
    configOpen.value = false
  }
})

function scheduleEstimate(): void {
  if (estimateTimer !== null) {
    window.clearTimeout(estimateTimer)
    estimateTimer = null
  }
  // 选择变了，上一次算的价就不作数了——别让运营看着一个过时的数字点提交。
  submitError.value = null
  if (!canEstimate.value) {
    estimateController?.abort()
    estimate.value = null
    priceNote.value = ''
    estimateError.value = null
    estimating.value = false
    return
  }
  estimating.value = true
  estimateTimer = window.setTimeout(() => {
    void runEstimate()
  }, ESTIMATE_DEBOUNCE_MS)
}

async function runEstimate(): Promise<void> {
  estimateController?.abort()
  estimateController = new AbortController()
  const signal = estimateController.signal
  estimating.value = true
  estimateError.value = null
  try {
    const result = await estimateJobWithNote(
      {
        ratio: ratio.value,
        asset_uids: assets.value.map((a) => a.uid),
        prompts: allPrompts.value,
      },
      signal,
    )
    if (signal.aborted) {
      return
    }
    estimate.value = result.estimate
    priceNote.value = result.priceNote
  } catch (err) {
    if (isCanceledError(err)) {
      return
    }
    estimate.value = null
    priceNote.value = ''
    estimateError.value = errorText(err)
  } finally {
    if (!signal.aborted) {
      estimating.value = false
    }
  }
}

// ---------------------------------------------------------------------------
// 提交（这一步开始花钱）
// ---------------------------------------------------------------------------

function openConfirmDialog(): void {
  if (assets.value.length === 0) {
    appStore.showWarning(t('designkit.workbench.needAsset'))
    return
  }
  if (allPrompts.value.length === 0) {
    appStore.showWarning(t('designkit.workbench.needPrompt'))
    return
  }
  submitError.value = null
  // 一次确认流程一个标识，从这里开始，到提交成功为止都用它。
  submitIdempotencyKey = newIdempotencyKey()
  confirmDialogOpen.value = true
}

function closeConfirmDialog(): void {
  if (submitting.value) {
    // 请求还在路上，这时候关掉会让人以为没提交上去，然后再点一次。
    return
  }
  confirmDialogOpen.value = false
  submitIdempotencyKey = ''
}

async function submit(): Promise<void> {
  // 防重复点击：请求在途时按钮已经禁用，这里再挡一次（键盘回车也会触发）。
  if (submitting.value) {
    return
  }
  submitting.value = true
  submitError.value = null
  try {
    // ⚠ 灵感库带来的那些也走 prompts（纯文本），**不走 prompt_uids**。
    // 传 uid 的话后端会照灵感库里的原文出图，运营刚才填进占位符的内容就没了 ——
    // 界面上看着是他填好的那段，实际出的是另一段，而钱已经花了。
    // 代价是历史记录里 prompt_uid 是空的（认不出来自哪一条），这个代价可以接受。
    const created = await createJob(
      {
        ratio: ratio.value,
        asset_uids: assets.value.map((a) => a.uid),
        prompts: allPrompts.value,
        name: jobName.value,
      },
      submitIdempotencyKey,
    )
    confirmDialogOpen.value = false
    // 提交成功就把标识丢掉：下一批必须是新的，否则后端会当成同一次提交。
    submitIdempotencyKey = ''
    currentJobUid.value = created.uid
    appStore.showSuccess(t('designkit.workbench.submitted'))
    refreshUsage()
  } catch (err) {
    if (isCanceledError(err)) {
      return
    }
    const friendly = toFriendlyError(err)
    confirmDialogOpen.value = false
    submitError.value = {
      // 后端的中文原话（余额不足时会写明「还差 $X」）+ 小字错误码。
      text: errorText(friendly),
      requestId: friendly.requestId,
      // 余额不足必须给「申请额度」：运营没有充值入口，不给就是死路（决策 19）。
      showQuota: friendly.kind === 'balance',
    }
    appStore.showError(friendly.message)
    // 提交没成功，这个标识还没被后端认下来，留着给「再点一次」用。
  } finally {
    submitting.value = false
  }
}

// ---------------------------------------------------------------------------
// 出完之后
// ---------------------------------------------------------------------------

function refreshUsage(): void {
  void usageCard.value?.refresh()
}

function onJobFinished(): void {
  refreshUsage()
  // 重新报个价：余额变了，接着排下一批时看到的数才是对的。
  scheduleEstimate()
}

onBeforeUnmount(() => {
  if (estimateTimer !== null) {
    window.clearTimeout(estimateTimer)
  }
  estimateController?.abort()
})
</script>
