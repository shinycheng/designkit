<!--
  第一步：选商品图。

  为什么自己写而不用上游的 ImageUpload.vue（CLAUDE.md 二·五 D4）：
  那个是给「站点 Logo」设计的——转 base64 塞进接口、默认上限 300KB、只能单选、
  没有拖拽、没有进度。商品图是几 MB 的照片、一次几十张，完全对不上。

  这里要扛住运营真实会传上来的东西：
    - 苹果手机拍的 HEIC（浏览器**显示不出来**，但传上去后端能处理，所以
      预览失败不能当成上传失败）；
    - 微信发过来又另存的图（格式经常和扩展名对不上，本地只做「肯定不行」的拦截，
      拿不准的一律放行交给后端判断）；
    - 带透明底的 PNG（后端会按比例补白边，这里不用管）。

  每一张的失败都单独显示在那一张上：**哪一张、为什么、怎么办**，
  不要笼统地弹一句「上传失败」——运营选了 20 张，不知道是哪张出问题等于没提示。
-->
<template>
  <section class="dk-panel">
    <header class="dk-panel-head">
      <!-- 编号圆点是视觉锚，不替代标题里的「第一步」，读屏直接读标题。
           外层多包一个 div：让 .dk-step 是它爹的最后一个孩子，
           命中 .dk-step:last-child 的单行网格（不然圆点下面多出 20px 连线位）。 -->
      <div>
        <div
          class="dk-step"
          :class="{ 'is-done': stepState === 'done', 'is-current': stepState === 'current' }"
        >
          <span class="dk-step__dot" aria-hidden="true">
            <svg
              v-if="stepState === 'done'"
              viewBox="0 0 12 12"
              fill="none"
              stroke="currentColor"
              stroke-width="2"
              stroke-linecap="round"
              stroke-linejoin="round"
            >
              <path d="M2 6.5 5 9.5 10 3" />
            </svg>
            <template v-else>1</template>
          </span>
          <div>
            <h2 class="dk-panel-title dk-step__title">{{ t('designkit.workbench.stepUpload') }}</h2>
            <p class="dk-panel-hint">{{ t('designkit.upload.hint') }}</p>
          </div>
        </div>
      </div>
      <div class="dk-panel-actions">
        <span v-if="entries.length > 0" class="dk-count-pill">
          {{ t('designkit.common.selectedCount', { count: readyAssets.length }) }}
        </span>
        <button
          v-if="entries.length > 0"
          type="button"
          class="dk-button dk-button--quiet dk-button--sm"
          @click="clearAll"
        >
          {{ t('designkit.common.clear') }}
        </button>
      </div>
    </header>

    <!-- 拖拽 / 点击选择 -->
    <button
      type="button"
      class="dk-dropzone"
      :class="{ 'is-dragover': dragging }"
      @click="openPicker"
      @dragenter.prevent.stop="onDragEnter"
      @dragover.prevent.stop="onDragOver"
      @dragleave.prevent.stop="onDragLeave"
      @drop.prevent.stop="onDrop"
    >
      <span class="dk-dropzone__title">{{ t('designkit.upload.dropHere') }}</span>
      <span class="dk-dropzone__meta">{{ t('designkit.upload.hint') }}</span>
      <span class="dk-button dk-button--secondary dk-button--sm dk-mt-1">
        {{ t('designkit.upload.choose') }}
      </span>
    </button>

    <input
      ref="inputEl"
      type="file"
      class="dk-file-input"
      multiple
      :accept="ACCEPT_ATTRIBUTE"
      @change="onPick"
    />

    <!-- 一次选太多的提示 -->
    <p v-if="countLimitHit" class="dk-note dk-note--warning dk-mt-3">
      {{ t('designkit.upload.countLimit', { max: MAX_ASSETS }) }}
    </p>

    <!-- 缩略图 -->
    <ul v-if="entries.length > 0" class="dk-thumb-grid dk-mt-4">
      <li
        v-for="entry in entries"
        :key="entry.id"
        class="dk-thumb"
        :class="{ 'is-failed': entry.status === 'failed' }"
      >
        <div class="dk-thumb__media">
          <img
            v-if="entry.previewUrl && !entry.previewBroken"
            :src="entry.previewUrl"
            :alt="entry.name"
            @error="entry.previewBroken = true"
          />
          <!-- HEIC 之类浏览器画不出来的格式：说清楚「显示不出来 ≠ 没传上去」 -->
          <div v-else class="dk-thumb__fallback">
            {{ t('designkit.upload.previewFailed') }}
          </div>

          <!-- 上传中的遮罩 -->
          <div
            v-if="entry.status === 'uploading' || entry.status === 'pending'"
            class="dk-thumb__overlay"
          >
            {{ t('designkit.upload.uploading', { percent: entry.percent }) }}
          </div>

          <!-- 移除 -->
          <button
            type="button"
            class="dk-thumb__remove"
            :title="t('designkit.upload.remove')"
            @click="removeEntry(entry.id)"
          >
            {{ t('designkit.common.remove') }}
          </button>
        </div>

        <div class="dk-thumb__body">
          <p class="dk-thumb__name" :title="entry.name">{{ entry.name }}</p>
          <template v-if="entry.status === 'done'">
            <p class="dk-note dk-note--success">
              {{ t('designkit.upload.done') }}
            </p>
            <!--
              生成白底图：抠掉背景 + 合成白底，产出一条新的商品图并自动加进列表。
              一次只抠一张（whitebgUid 非 null 时全部禁用）：接口要等几十秒，
              并发点一排只会把抠图容器打挂。
            -->
            <button
              v-if="entry.asset"
              type="button"
              class="dk-button dk-button--secondary dk-button--sm dk-button--block"
              :disabled="whitebgUid !== null"
              @click="makeWhiteBackground(entry)"
            >
              {{ whitebgUid === entry.asset?.uid ? t('designkit.whitebg.running') : t('designkit.whitebg.button') }}
            </button>
            <!--
              高清放大：×4，**异步排队**（一张约 1~2 分钟），完成后新图自动加进列表。
              跟白底图不同，可以几张同时排——排队和串行都在后端管着（队列封顶 10），
              这里只是各自轮询状态。按钮文案就是进度：排队中… → 放大中… → 恢复原名。
            -->
            <button
              v-if="entry.asset"
              type="button"
              class="dk-button dk-button--secondary dk-button--sm dk-button--block"
              :disabled="!!upscaleStates[entry.asset.uid]"
              @click="upscaleEntry(entry)"
            >
              {{ upscaleLabelOf(entry.asset?.uid) }}
            </button>
            <!--
              删除素材：把这条商品图从服务端删掉（软删）并移出列表。
              跟右上角的「移除这张」是两件事：移除只是这一批不用它，图还留着，
              下次还能搜到复用；删除是以后不再出现在商品图列表里。
              历史批次不受影响（批次里存的是提示词快照和结果图），所以确认文案敢这么写。
            -->
            <button
              v-if="entry.asset"
              type="button"
              class="dk-button dk-button--danger dk-button--sm dk-button--block"
              :disabled="deleting"
              @click="deleteTarget = entry"
            >
              {{ t('designkit.del.assetButton') }}
            </button>
          </template>
          <template v-else-if="entry.status === 'failed'">
            <p class="dk-note dk-note--danger">{{ entry.error }}</p>
            <button
              type="button"
              class="dk-button dk-button--secondary dk-button--sm dk-button--block"
              @click="retryEntry(entry.id)"
            >
              {{ t('designkit.upload.retry') }}
            </button>
          </template>
        </div>
      </li>
    </ul>

    <p v-else class="dk-note dk-mt-4">
      {{ t('designkit.workbench.emptyAssets') }}
    </p>

    <!-- 「删除素材」的二次确认。文案敢写「不受影响」是核对过后端语义的（软删，批次存快照）。 -->
    <ConfirmDialog
      :show="deleteTarget !== null"
      :title="t('designkit.del.assetButton')"
      :message="t('designkit.del.assetConfirm')"
      :confirm-text="t('designkit.common.delete')"
      :cancel-text="t('designkit.common.cancel')"
      :danger="true"
      @confirm="confirmDeleteAsset()"
      @cancel="deleteTarget = null"
    />
  </section>
</template>

<script setup lang="ts">
import { computed, onBeforeUnmount, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import { useAppStore } from '@/stores/app'
import ConfirmDialog from '@/components/common/ConfirmDialog.vue'
import {
  ACCEPT_ATTRIBUTE,
  checkUploadFile,
  deleteAsset,
  errorText,
  fetchContentBlob,
  isCanceledError,
  isTerminalUpscaleStatus,
  isUpscaleQueueFullError,
  isUpscaleUnavailableError,
  removeBackground,
  startUpscale,
  toFriendlyError,
  uploadAsset,
  waitForUpscale,
} from '../../api'
import type { DesignkitAsset } from '../../api'
import type { WorkbenchStepState } from './viewTypes'

/**
 * 一次最多选这么多张。
 *
 * 真正的批量上限是后端报价里的 `max_batch_items`（张数 = 商品图 × 提示词），
 * 那个由「花费预估」那一块去拦。这里只是防止有人一口气拖 500 张进来把浏览器卡死。
 */
const MAX_ASSETS = 100

/** 同时最多传几张。浏览器对同一个站点只开 6 条连接，传太猛会把查进度挤到后面。 */
const UPLOAD_CONCURRENCY = 3

/** 界面上的一张图（本地文件 + 上传状态）。 */
interface UploadEntry {
  /** 界面用的行号，跟后端无关。 */
  id: number
  file: File
  name: string
  status: 'pending' | 'uploading' | 'done' | 'failed'
  /** 0~100。 */
  percent: number
  /** 失败原因，已经是中文整句。 */
  error: string | null
  /** 上传成功之后后端给的那张图。 */
  asset: DesignkitAsset | null
  /** 本地预览地址（不用走后端，也就不用带登录凭证）。 */
  previewUrl: string | null
  /** 浏览器显示不出来（HEIC 基本都是这样）。**不代表上传失败。** */
  previewBroken: boolean
  controller: AbortController | null
}

withDefaults(
  defineProps<{
    /** 已经传好的商品图，顺序就是出图时的外层顺序。 */
    modelValue: DesignkitAsset[]
    /** 标题前编号圆点的状态，由页面层推导（viewTypes.ts 的说明）。 */
    stepState?: WorkbenchStepState
  }>(),
  { stepState: 'todo' },
)

const emit = defineEmits<{
  (e: 'update:modelValue', value: DesignkitAsset[]): void
}>()

const { t } = useI18n()
const appStore = useAppStore()

const entries = ref<UploadEntry[]>([])
const inputEl = ref<HTMLInputElement | null>(null)
const dragging = ref(false)
const countLimitHit = ref(false)

let nextId = 1
/** 拖拽进出子元素也会触发 dragleave，用计数抵消，否则边框会一直闪。 */
let dragDepth = 0
let running = 0

/** 传好了的那些，**保持运营选择的顺序**（顺序就是出图顺序，不许排序）。 */
const readyAssets = computed<DesignkitAsset[]>(() =>
  entries.value.filter((e) => e.status === 'done' && e.asset !== null).map((e) => e.asset as DesignkitAsset),
)

// 传完一张就告诉父组件一次，报价能立刻跟着变。
watch(
  readyAssets,
  (list) => {
    emit('update:modelValue', list)
  },
  { deep: false },
)

/**
 * 从外部塞一张**已经存在**的商品图进来（「用这张继续生成」）。
 *
 * ⚠ 为什么必须走这个方法，而不是父组件直接改 v-model 绑的那个数组：
 *
 *   这个组件是**单向**的 —— 模板渲染的是内部的 `entries`，`modelValue` 从头到尾
 *   没被读过，只用来往外吐。父组件往数组里塞，① 界面上看不见（还显示「还没有选
 *   商品图」），② 更糟的是运营下次再传一张图时，`readyAssets` 那个 watch 会用
 *   内部列表**覆盖**父组件的，刚塞进去的那张**静默消失**——而张数和报价是按
 *   父组件那个数组算的，运营会看到张数忽然变少却不知道为什么。
 *
 *   2026-08-14 monica 实测发现的就是①。
 *
 * `file` 那一格塞一个空的占位 File：这条 entry 是「已经在服务端了」的状态，
 * 不会再被上传，只有重传失败那条路会用到 file，而 done 状态走不到那里。
 *
 * 返回 false 表示没加（重复了，或者到上限了）。
 */
function addExistingAsset(asset: DesignkitAsset, label?: string): boolean {
  if (!asset?.uid) {
    return false
  }
  // 去重：同一张图重复点「继续生成」，后端按 sha256 返回同一条 asset。
  // 不判的话列表里会出现两条一模一样的，而**张数是按条数算的，直接多花一份钱**。
  if (entries.value.some((e) => e.asset?.uid === asset.uid)) {
    return false
  }
  if (entries.value.length >= MAX_ASSETS) {
    countLimitHit.value = true
    return false
  }
  entries.value = [
    ...entries.value,
    {
      id: nextId++,
      file: new File([], asset.uid),
      name: label ?? t('designkit.upload.fromResult'),
      status: 'done',
      percent: 100,
      error: null,
      asset,
      // 预览稍后异步取（见下面 loadRemotePreview）。先按「还没取到」显示，
      // 不要一上来就 previewBroken=true —— 那会闪一下「预览不出来」再变成图。
      previewUrl: null,
      previewBroken: false,
      controller: null,
    },
  ]
  // 不 await：让这张图立刻出现在列表里（运营要的是「看到它进去了」），
  // 缩略图晚半秒补上就行。取失败也不影响出图。
  void loadRemotePreview(asset)
  return true
}

/**
 * 取服务器上那张图当缩略图。
 *
 * **为什么不能直接 `<img :src="asset.content_url">`**：那个地址要带登录凭证
 * 请求头才放行，而 `<img>` 标签不会带任何请求头，结果是 401 + 一个碎图标 ——
 * 运营会以为图没了。所以先取成字节再转本地地址，跟「我的图片」那边同一套做法
 * （composables/useAuthedImage.ts 有完整解释）。
 *
 * 取失败只是没有缩略图，**不影响这张图能不能用来出图**，所以静默处理：
 * 为一张看不到的缩略图弹一句红色错误，只会让运营以为出图也坏了。
 */
async function loadRemotePreview(asset: DesignkitAsset): Promise<void> {
  const url = asset.content_url
  if (!url) {
    return
  }
  try {
    const blob = await fetchContentBlob(url)
    const entry = entries.value.find((e) => e.asset?.uid === asset.uid)
    // 取的过程中运营可能已经把它删了。
    if (!entry) {
      return
    }
    entry.previewUrl = URL.createObjectURL(blob)
  } catch {
    const entry = entries.value.find((e) => e.asset?.uid === asset.uid)
    if (entry) {
      // 跟 HEIC 显示不出来是同一个样子：告诉运营「预览不出来但图是好的」。
      entry.previewBroken = true
    }
  }
}

defineExpose({ addExistingAsset })

/** 正在抠图的那张（asset uid）。null = 没有在抠。一次只允许一张。 */
const whitebgUid = ref<string | null>(null)

/**
 * 「生成白底图」：调后端抠掉背景、合成白底，产出一条**新的**商品图。
 *
 * ⚠ 成功后必须走 addExistingAsset 把新图加进列表（组件是单向的，
 * 理由见 addExistingAsset 的注释）。它自带按 uid 去重：重复点同一张，
 * 后端按 sha256 返回同一条 asset，列表里也只有一条。
 *
 * 抠图在服务端要等几十秒（首次还可能在下模型），接口超时 90 秒
 * （api 的 removeBackground），期间按钮全部禁用、这一张显示「抠图中…」。
 */
async function makeWhiteBackground(entry: UploadEntry): Promise<void> {
  const uid = entry.asset?.uid
  if (!uid || whitebgUid.value !== null) {
    return
  }
  whitebgUid.value = uid
  try {
    const asset = await removeBackground(uid)
    const added = addExistingAsset(asset, t('designkit.whitebg.name'))
    if (added || entries.value.some((e) => e.asset?.uid === asset.uid)) {
      // 加进去了，或者本来就在列表里（重复点同一张）——两种情况都属实。
      appStore.showToast('success', t('designkit.whitebg.done'))
    } else {
      // 没加进去只剩一种可能：到了张数上限（countLimitHit 已经在页面上说了原因）。
      appStore.showToast('error', t('designkit.upload.countLimit', { max: MAX_ASSETS }))
    }
  } catch (err) {
    appStore.showToast('error', whitebgErrorMessage(err))
  } finally {
    whitebgUid.value = null
  }
}

/**
 * 失败时给运营看什么：
 *   - 后端的 DK_ 错误信封：message 已经是中文（含「还没准备好」那句），原样用；
 *   - 裸 404（后端还没这个接口）：显示「还没准备好」；
 *   - 其余（超时、断网）：显示「没抠出来，重试一次。」
 */
function whitebgErrorMessage(err: unknown): string {
  const friendly = toFriendlyError(err)
  if (typeof friendly.code === 'string' && friendly.code.startsWith('DK_')) {
    return friendly.message
  }
  if (friendly.status === 404) {
    return t('designkit.whitebg.unavailable')
  }
  return t('designkit.whitebg.failed')
}

// ---------------------------------------------------------------------------
// 删除素材（软删；历史批次不受影响）
// ---------------------------------------------------------------------------

/** 正在等确认的那一条。null = 弹窗关着。 */
const deleteTarget = ref<UploadEntry | null>(null)
/** 有一条正在删（接口在途）。期间所有「删除素材」按钮禁用，防连点。 */
const deleting = ref(false)

/**
 * 确认删除：调后端软删这条商品图，然后从列表里移出。
 *
 * 删成功后走 removeEntry（它顺手回收预览的 objectURL）；
 * 删失败列表**不动**，运营看到图还在，跟提示「没删掉」对得上。
 */
async function confirmDeleteAsset(): Promise<void> {
  const entry = deleteTarget.value
  deleteTarget.value = null
  const uid = entry?.asset?.uid
  if (!entry || !uid || deleting.value) {
    return
  }
  deleting.value = true
  try {
    await deleteAsset(uid)
    removeEntry(entry.id)
    appStore.showToast('success', t('designkit.del.done'))
  } catch (err) {
    appStore.showToast('error', deleteErrorMessage(err))
  } finally {
    deleting.value = false
  }
}

/**
 * 删失败给运营看什么：带 DK_ 码的业务错误显示后端的中文原文
 * （比如「找不到这张商品图，可能已经被删除了」），其余（断网、超时）
 * 显示「没删掉，重试一次。」
 */
function deleteErrorMessage(err: unknown): string {
  const friendly = toFriendlyError(err)
  if (typeof friendly.code === 'string' && friendly.code.startsWith('DK_')) {
    return friendly.message
  }
  return t('designkit.del.failed')
}

// ---------------------------------------------------------------------------
// 高清放大（异步排队 + 每 5 秒轮询）
// ---------------------------------------------------------------------------

/** 每张图的放大进度（asset uid → queued/running）。不在表里 = 没在放。 */
const upscaleStates = ref<Record<string, 'queued' | 'running'>>({})

/** 每张在轮询的图一只取消器，页面关掉时全部叫停（别让轮询打一晚上后端）。 */
const upscaleControllers = new Map<string, AbortController>()

/** 按钮文案就是进度：排队中… → 放大中… → （结束后恢复）高清放大。 */
function upscaleLabelOf(uid: string | undefined): string {
  const state = uid ? upscaleStates.value[uid] : undefined
  if (state === 'queued') {
    return t('designkit.upscale.queued')
  }
  if (state === 'running') {
    return t('designkit.upscale.running')
  }
  return t('designkit.upscale.button')
}

function setUpscaleState(uid: string, status: string): void {
  if (status === 'queued' || status === 'running') {
    upscaleStates.value = { ...upscaleStates.value, [uid]: status }
  }
}

function clearUpscaleState(uid: string): void {
  const next = { ...upscaleStates.value }
  delete next[uid]
  upscaleStates.value = next
}

/**
 * 「高清放大」：排进后端队列（立刻返回），然后每 5 秒问一次，直到 done/failed。
 *
 * ⚠ 成功后必须走 addExistingAsset 把新图加进列表（组件是单向的，
 * 理由见 addExistingAsset 的注释）。重复点安全：后端按任务去重、
 * 结果按 sha256 去重，列表里也按 uid 去重，怎么点都只有一份。
 */
async function upscaleEntry(entry: UploadEntry): Promise<void> {
  const uid = entry.asset?.uid
  if (!uid || upscaleStates.value[uid]) {
    return
  }
  const controller = new AbortController()
  upscaleControllers.set(uid, controller)
  upscaleStates.value = { ...upscaleStates.value, [uid]: 'queued' }
  try {
    let task = await startUpscale(uid)
    setUpscaleState(uid, task.status)
    if (!isTerminalUpscaleStatus(task.status)) {
      task = await waitForUpscale(uid, {
        signal: controller.signal,
        onStatus: (latest) => setUpscaleState(uid, latest.status),
      })
    }
    if (task.status === 'done' && task.result) {
      const result = task.result
      const added = addExistingAsset(result, t('designkit.upscale.name'))
      if (added || entries.value.some((e) => e.asset?.uid === result.uid)) {
        appStore.showToast('success', t('designkit.upscale.done'))
      } else {
        // 没加进去只剩一种可能：到了张数上限。
        appStore.showToast('error', t('designkit.upload.countLimit', { max: MAX_ASSETS }))
      }
    } else {
      // failed：后端的 error_message 已经是中文（「放大超时了…」这类），原样显示。
      appStore.showToast('error', task.error_message || t('designkit.upscale.failed'))
    }
  } catch (err) {
    if (!isCanceledError(err)) {
      appStore.showToast('error', upscaleErrorMessage(err))
    }
  } finally {
    upscaleControllers.delete(uid)
    clearUpscaleState(uid)
  }
}

/**
 * 放大没排进去（或轮询出错）时给运营看什么：
 *   - 排队满了 → 「排队满了，等会儿再试。」
 *   - 裸 404（后端还没上这个功能）→ 「还没准备好」；
 *   - 带 DK_ 码的业务错误（图没了 / 任务丢了）→ 后端的中文原样显示；
 *   - 其余（断网之类）→ 「放大失败，重试一次。」
 */
function upscaleErrorMessage(err: unknown): string {
  if (isUpscaleQueueFullError(err)) {
    return t('designkit.upscale.queueFull')
  }
  if (isUpscaleUnavailableError(err)) {
    return t('designkit.upscale.unavailable')
  }
  const friendly = toFriendlyError(err)
  if (typeof friendly.code === 'string' && friendly.code.startsWith('DK_')) {
    return friendly.message
  }
  return t('designkit.upscale.failed')
}

function openPicker(): void {
  inputEl.value?.click()
}

function onDragEnter(): void {
  dragDepth += 1
  dragging.value = true
}

function onDragOver(): void {
  dragging.value = true
}

function onDragLeave(): void {
  dragDepth = Math.max(0, dragDepth - 1)
  if (dragDepth === 0) {
    dragging.value = false
  }
}

function onDrop(event: DragEvent): void {
  dragDepth = 0
  dragging.value = false
  const files = event.dataTransfer?.files
  if (files && files.length > 0) {
    addFiles(Array.from(files))
  }
}

function onPick(event: Event): void {
  const input = event.target as HTMLInputElement
  if (input.files && input.files.length > 0) {
    addFiles(Array.from(input.files))
  }
  // 清空，否则同一个文件再选一次不会触发 change。
  input.value = ''
}

function addFiles(files: File[]): void {
  countLimitHit.value = false
  for (const file of files) {
    if (entries.value.length >= MAX_ASSETS) {
      countLimitHit.value = true
      break
    }

    const entry: UploadEntry = {
      id: nextId++,
      file,
      name: file.name,
      status: 'pending',
      percent: 0,
      error: null,
      asset: null,
      previewUrl: null,
      previewBroken: false,
      controller: null,
    }

    // 本地预检：太大 / 明显不是图片的，别让运营等传完 20MB 才被拒绝。
    const check = checkUploadFile(file)
    if (!check.ok) {
      entry.status = 'failed'
      entry.error = check.reasonKey ? t(check.reasonKey) : t('designkit.upload.failed')
      entries.value = [...entries.value, entry]
      continue
    }

    // HEIC 在多数浏览器里显示不出来，createObjectURL 不会报错、<img> 会 onerror，
    // 那时显示「显示不出来但已经传上去了」，不能当成上传失败。
    try {
      entry.previewUrl = URL.createObjectURL(file)
    } catch {
      entry.previewBroken = true
    }

    entries.value = [...entries.value, entry]
  }
  pump()
}

function pump(): void {
  while (running < UPLOAD_CONCURRENCY) {
    const next = entries.value.find((e) => e.status === 'pending')
    if (!next) {
      return
    }
    running += 1
    void upload(next).finally(() => {
      running -= 1
      pump()
    })
  }
}

async function upload(entry: UploadEntry): Promise<void> {
  entry.status = 'uploading'
  entry.percent = 0
  entry.error = null
  const controller = new AbortController()
  entry.controller = controller

  try {
    const asset = await uploadAsset(entry.file, {
      signal: controller.signal,
      onProgress: (percent) => {
        entry.percent = percent
      },
    })
    entry.asset = asset
    entry.status = 'done'
    entry.percent = 100
    // 触发一次重算：entries 是 ref<数组>，改数组元素的字段不会让 computed 重跑。
    entries.value = [...entries.value]
  } catch (err) {
    if (isCanceledError(err)) {
      // 运营自己删掉了这一张，或者离开了页面。不是错误，也不用提示。
      return
    }
    entry.status = 'failed'
    // errorText() 已经是「中文 +（错误码）」，运营截图就能报障。
    entry.error = errorText(err)
    entries.value = [...entries.value]
  } finally {
    entry.controller = null
  }
}

function retryEntry(id: number): void {
  const entry = entries.value.find((e) => e.id === id)
  if (!entry) {
    return
  }
  // 本地预检没过的（太大、格式不对）重传也一样，直接换一张更快。
  const check = checkUploadFile(entry.file)
  if (!check.ok) {
    entry.error = check.reasonKey ? t(check.reasonKey) : t('designkit.upload.failed')
    entries.value = [...entries.value]
    return
  }
  entry.status = 'pending'
  entry.error = null
  entries.value = [...entries.value]
  pump()
}

function removeEntry(id: number): void {
  const entry = entries.value.find((e) => e.id === id)
  if (!entry) {
    return
  }
  entry.controller?.abort()
  if (entry.previewUrl) {
    URL.revokeObjectURL(entry.previewUrl)
  }
  entries.value = entries.value.filter((e) => e.id !== id)
  countLimitHit.value = false
}

function clearAll(): void {
  for (const entry of entries.value) {
    entry.controller?.abort()
    if (entry.previewUrl) {
      URL.revokeObjectURL(entry.previewUrl)
    }
  }
  entries.value = []
  countLimitHit.value = false
}

// 不回收 objectURL 就是内存泄漏：几十张几 MB 的图能占掉几百 MB。
// 放大的轮询也要一起叫停：不停的话页面关了它还每 5 秒打一次后端。
onBeforeUnmount(() => {
  for (const entry of entries.value) {
    entry.controller?.abort()
    if (entry.previewUrl) {
      URL.revokeObjectURL(entry.previewUrl)
    }
  }
  for (const controller of upscaleControllers.values()) {
    controller.abort()
  }
  upscaleControllers.clear()
})
</script>
