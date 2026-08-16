<!--
  第二步：让 AI 配提示词。

  ⚠ 2026-08-14 整体重写。原来这一版**只有一个「自己写一条」的自由文本框**，
  第二步叫「挑提示词」却根本没得挑（monica 原话：「并没有挑」）。
  那不是设计，是遗留：写它的时候后端灵感库端点还没做，所以只放了手写框。

  现在的流程（monica 拍板）：

    选一个大致分类（不确定就选「全部」）
      → 写一句商品特点（**不是提示词**，是关于商品的事实）
      → AI 看着商品图，从该分类里挑 5 条参考
      → 合成**一条**最终提示词，出 **1 张**图

  三条容易改错的地方：

  1. **「自己写一条」被删掉了**，这是 monica 明确要求的。
     运营改提示词的地方变成了下面那个「AI 推荐的提示词」——它本身可编辑。
     不要因为「万一有人想直接写」就把手写框加回来：两个框会让运营
     搞不清哪个才算数，而张数是按 modelValue 算的，多一个来源就多一处能算错钱。

  2. **对外契约没变**：仍然通过 v-model 吐 `string[]`。
     工作台按它算张数和报价（`assets.length × allPrompts.length`）。
     最终提示词非空 = `[那一条]`，空 = `[]`。**永远不要吐出多于 1 条**——
     吐 5 条就是 5 张图的钱，而 monica 明确要的是「5 条合成 1 条、出 1 张」。

  3. **等待要说清楚**。三趟对话串行，十几秒起步。十几秒在界面上就是「卡死」，
     不写明白运营一定会反复点，而每点一次都是真的在调模型。
-->
<template>
  <section class="dk-panel">
    <header class="dk-panel-head">
      <div>
        <h2 class="dk-panel-title">{{ t('designkit.workbench.stepPrompt') }}</h2>
        <p class="dk-panel-hint">{{ t('designkit.suggest.description') }}</p>
      </div>
      <div class="dk-panel-actions">
        <span v-if="finalPrompt.trim() !== ''" class="dk-count-pill">
          {{ t('designkit.common.pieces', { count: 1 }) }}
        </span>
      </div>
    </header>

    <!-- ── 分类 ───────────────────────────────────── -->
    <label class="dk-label" :for="categoryId">{{ t('designkit.suggest.categoryLabel') }}</label>
    <select :id="categoryId" v-model="categorySlug" class="dk-select" :disabled="running">
      <!-- 空串 = 全部。后端拿到空串会走「自己看图判分类」那一步。 -->
      <option value="">{{ t('designkit.suggest.allCategories') }}</option>
      <option v-for="one in categories" :key="one.slug" :value="one.slug">{{ one.name }}</option>
    </select>
    <p class="dk-note dk-mt-1">
      {{ categoriesError ? t('designkit.suggest.categoryFailed') : t('designkit.suggest.categoryHint') }}
    </p>

    <!-- ── 商品特点（原来那个「自己写一条」的位置）──────── -->
    <label class="dk-label dk-mt-3" :for="featuresId">{{ t('designkit.suggest.featuresLabel') }}</label>
    <textarea
      :id="featuresId"
      v-model="features"
      rows="3"
      class="dk-textarea"
      :disabled="running"
      :placeholder="t('designkit.suggest.featuresPlaceholder')"
    ></textarea>
    <p class="dk-note dk-mt-1">{{ t('designkit.suggest.featuresHint') }}</p>
    <p v-if="featuresTooLong" class="dk-note dk-note--warning">
      {{ t('designkit.suggest.featuresTooLong', { max: FEATURES_MAX }) }}
    </p>

    <!-- ── 推荐按钮 ───────────────────────────────── -->
    <button
      type="button"
      class="dk-button dk-button--primary dk-mt-3"
      :disabled="!canSuggest"
      @click="runSuggest"
    >
      {{ running
        ? t('designkit.suggest.running')
        : (hasResult ? t('designkit.suggest.again') : t('designkit.suggest.button')) }}
    </button>

    <!-- 没传商品图时说清楚为什么点不了，别让按钮灰着不给理由。 -->
    <p v-if="assetCount === 0" class="dk-note dk-note--warning dk-mt-2">
      {{ t('designkit.suggest.needAsset') }}
    </p>
    <!-- ⚠ 这句必须显示。见文件头第 3 条。 -->
    <p v-else-if="running" class="dk-note dk-mt-2">{{ t('designkit.suggest.runningHint') }}</p>

    <p v-if="errorText !== ''" class="dk-note dk-note--warning dk-mt-2">{{ errorText }}</p>

    <!-- ── 结果 ───────────────────────────────────── -->
    <template v-if="hasResult">
      <label class="dk-label dk-mt-4" :for="promptId">{{ t('designkit.suggest.resultTitle') }}</label>
      <textarea
        :id="promptId"
        v-model="finalPrompt"
        rows="7"
        class="dk-textarea"
        :placeholder="t('designkit.suggest.resultHint')"
      ></textarea>
      <div class="dk-editor-result-actions dk-mt-1">
        <p class="dk-note">{{ t('designkit.suggest.resultHint') }}</p>
        <!--
          存的是**当前编辑后**的内容（绑定的就是 finalPrompt），不是 AI 的原始输出。
          不花钱、不出图，存完下次在灵感库的「我的提示词」页签里能直接「用它生成」。
        -->
        <button
          type="button"
          class="dk-button dk-button--secondary dk-button--sm"
          :disabled="savingMine || finalPrompt.trim() === ''"
          @click="saveToMyPrompts"
        >
          {{ savingMine ? t('designkit.common.loading') : t('designkit.myPrompts.saveFromResult') }}
        </button>
      </div>

      <!--
        判成了哪个分类必须让运营看见：选「全部」时这是他唯一能发现
        「AI 判错了、该换个分类重来」的线索。
      -->
      <p v-if="usedCategoryName !== ''" class="dk-note dk-mt-1">
        {{ guessedCategory
          ? t('designkit.suggest.guessedCategory', { name: usedCategoryName })
          : t('designkit.suggest.usedCategory', { name: usedCategoryName }) }}
      </p>
      <p v-if="serverNote !== ''" class="dk-note dk-note--warning dk-mt-1">{{ serverNote }}</p>

      <!-- 参考依据，折叠。不给的话推荐就是个黑盒。 -->
      <details v-if="candidates.length > 0" class="dk-mt-2">
        <summary class="dk-note">{{ t('designkit.suggest.candidatesTitle') }}</summary>
        <ul class="dk-prompt-list dk-mt-2">
          <li v-for="(one, index) in candidates" :key="one.uid" class="dk-prompt-item">
            <span class="dk-prompt-item__index">{{ index + 1 }}</span>
            <span class="dk-prompt-item__text dk-clamp-2">
              {{ one.title.trim() === '' ? t('designkit.suggest.candidateUntitled') : one.title }}
            </span>
          </li>
        </ul>
        <p class="dk-note dk-mt-1">{{ t('designkit.suggest.candidatesHint') }}</p>
      </details>
    </template>
  </section>
</template>

<script setup lang="ts">
import { computed, onMounted, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import { useAppStore } from '@/stores'
import {
  createMyPrompt,
  isEmptySuggestion,
  isSuggestUnavailableError,
  listPromptCategories,
  suggestPrompt,
  toFriendlyError,
  type PromptCategory,
  type SuggestPromptCandidate,
} from '../../api'

const props = defineProps<{
  /**
   * 最终提示词。**最多一条**——出图张数按它算，见文件头第 2 条。
   */
  modelValue: string[]
  /** 第一步选了几张商品图。0 张时推荐按钮点不了（AI 要看着图）。 */
  assetCount: number
  /** 第一张商品图的编号，推荐时发给后端。没有就是空串。 */
  firstAssetUid: string
  /** 其余商品图的编号。整批一起给 AI 看，写出来的提示词才适合整批。 */
  extraAssetUids: string[]
}>()

const emit = defineEmits<{
  (e: 'update:modelValue', value: string[]): void
}>()

const { t } = useI18n()

/** 商品特点的长度上限。纯粹是防手滑粘一整篇文章进来，不是业务规则。 */
const FEATURES_MAX = 500

const categoryId = 'designkit-suggest-category'
const featuresId = 'designkit-suggest-features'
const promptId = 'designkit-suggest-prompt'

const categories = ref<PromptCategory[]>([])
const categoriesError = ref(false)
const categorySlug = ref('')
const features = ref('')

const running = ref(false)
const errorText = ref('')

const finalPrompt = ref('')
const candidates = ref<SuggestPromptCandidate[]>([])
const usedCategoryName = ref('')
/** 分类是 AI 判的（运营选的是「全部」），文案要换一句，让他知道可以纠正。 */
const guessedCategory = ref(false)
const serverNote = ref('')
const hasResult = ref(false)

const featuresTooLong = computed(() => features.value.length > FEATURES_MAX)

const canSuggest = computed(
  () => !running.value && props.assetCount > 0 && props.firstAssetUid !== '' && !featuresTooLong.value,
)

/**
 * 往外吐的永远是 0 条或 1 条。
 *
 * ⚠ 不要图省事写成 `finalPrompt.value.split('\n')` —— 提示词正文本来就带换行，
 * 那会把一条切成十几条，报价和扣费当场翻十几倍。
 */
watch(finalPrompt, (value) => {
  const one = value.trim()
  emit('update:modelValue', one === '' ? [] : [one])
})

/**
 * 外部清空时（提交成功后工作台会重置）跟着清掉结果区，
 * 否则运营会看到「上一批的提示词还在，但张数是 0」这种自相矛盾的状态。
 */
watch(
  () => props.modelValue,
  (value) => {
    if (value.length === 0 && finalPrompt.value.trim() !== '') {
      finalPrompt.value = ''
      hasResult.value = false
      candidates.value = []
      usedCategoryName.value = ''
      serverNote.value = ''
    }
  },
)

onMounted(async () => {
  try {
    const list = await listPromptCategories()
    // 字段名是 categories 不是 items（见 api/inspiration.ts 的 PromptCategoryList）。
    categories.value = list.categories ?? []
  } catch {
    // 读不到分类不是死路：空串（全部分类）照样能推荐，后端会自己判。
    categoriesError.value = true
  }
})

const appStore = useAppStore()

/** 「存为我的提示词」进行中：禁掉按钮，连点等于存出两条一样的。 */
const savingMine = ref(false)

/**
 * 把结果框里**当前这份**（可能已被运营改过）存进「我的提示词」。
 * 不花钱、不出图；成功只弹一句 toast，不打断出图流程。
 * 失败显示后端的中文原因（上限 200 条那类），兜底一句「没保存成功」。
 */
async function saveToMyPrompts() {
  const body = finalPrompt.value.trim()
  if (savingMine.value || body === '') {
    return
  }
  savingMine.value = true
  try {
    // 标题留空：卡片会拿正文开头顶上，比机器起的名字更认得出。
    await createMyPrompt({ title: '', body })
    appStore.showSuccess(t('designkit.myPrompts.saved'))
  } catch (error) {
    appStore.showError(toFriendlyError(error).message || t('designkit.myPrompts.failed'))
  } finally {
    savingMine.value = false
  }
}

async function runSuggest() {
  if (!canSuggest.value) {
    return
  }
  running.value = true
  errorText.value = ''
  try {
    const result = await suggestPrompt({
      asset_uid: props.firstAssetUid,
      extra_asset_uids: props.extraAssetUids,
      category_slug: categorySlug.value,
      features: features.value.trim(),
    })
    if (isEmptySuggestion(result)) {
      errorText.value = t('designkit.suggest.empty')
      return
    }
    finalPrompt.value = result.prompt
    candidates.value = result.candidates ?? []
    usedCategoryName.value = result.category?.name ?? ''
    // 运营选的是「全部」而后端回了一个具体分类 = 这是 AI 判的。
    guessedCategory.value = categorySlug.value === ''
    serverNote.value = (result.note ?? '').trim()
    hasResult.value = true
  } catch (error) {
    errorText.value = isSuggestUnavailableError(error)
      ? t('designkit.suggest.unavailable')
      : t('designkit.suggest.failed')
  } finally {
    running.value = false
  }
}
</script>

<style scoped>
.dk-editor-result-actions {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: var(--dk-space-2);
  flex-wrap: wrap;
}
</style>
