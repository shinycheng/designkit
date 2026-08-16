<!--
  灵感库：一万多条现成的提示词，按分类翻、按关键词搜，看中哪条带到工作台去出图。

  版式：外壳用上游的 `AppLayout`（跟 /workbench、/gallery 完全一致），
  里面是「（管理员才有的）同步区 → 搜索 + 分类 → 卡片网格 → 加载更多」。

  三件事值得先说清楚：

  1. **只有「加载更多」，没有页码。**
     后端是游标分页（拿上一条的位置往下翻），给不出「第 5 页」。
     但它给得出**总条数**，所以上面那行「共 N 条」是真的，运营据此判断
     该接着翻还是该换个词搜。

  2. **空列表要分清是哪一种空。**
     「还没同步过」和「搜不到」是两回事：前者要让运营去找管理员，
     后者要让他换个词。显示同一句「暂无数据」的话，运营会一直换词搜，
     搜到怀疑人生——库里根本是空的。

  3. **「用它生成」不花钱。** 它只是把提示词带到工作台的输入框里，
     真正开始花钱的是工作台上那个「提交出图」（还要再确认一次）。
     所以这里的按钮可以随便点，不需要二次确认。
-->
<template>
  <AppLayout>
    <div class="dk-insp-page">
      <header class="dk-canvas-header">
        <div class="dk-page-head">
          <p class="dk-eyebrow">{{ t('designkit.brand') }}</p>
          <h1 class="dk-page-title">{{ t('designkit.inspiration.title') }}</h1>
          <p class="dk-page-desc">{{ t('designkit.inspiration.description') }}</p>
        </div>
      </header>

      <!--
        管理员的同步区。**非管理员整块不渲染**（不是隐藏）：
        后端那几个端点只放管理员过，渲染出来只会得到一片报错。
      -->
      <InspirationSyncPanel v-if="isAdmin" @synced="reloadAll()" />

      <!-- ── 搜索 + 分类 ─────────────────────────────────────── -->
      <section class="dk-panel">
        <label class="dk-label" for="dk-insp-search">
          {{ t('designkit.inspiration.searchLabel') }}
        </label>
        <div class="dk-insp-search">
          <input
            id="dk-insp-search"
            v-model="keywordInput"
            type="search"
            class="dk-input"
            :placeholder="t('designkit.inspiration.searchPlaceholder')"
          />
          <button
            v-if="keywordInput !== ''"
            type="button"
            class="dk-button dk-button--quiet dk-button--sm"
            @click="clearKeyword()"
          >
            {{ t('designkit.common.clear') }}
          </button>
        </div>
        <p v-if="keywordTooLong" class="dk-note dk-note--danger dk-mt-2">
          {{ t('designkit.inspiration.searchTooLong', { max: MAX_PROMPT_KEYWORD_LENGTH }) }}
        </p>

        <PromptCategoryFilter
          v-model="category"
          class="dk-mt-4"
          :categories="categories"
          :total-count="totalPromptCount"
          :show-mine="true"
          :mine-count="myCount"
          :disabled="loading"
        />

        <p class="dk-note dk-mt-3">{{ resultSummary }}</p>
        <p v-if="categoriesError !== ''" class="dk-note dk-note--danger">{{ categoriesError }}</p>
      </section>

      <!-- ── 列表 ───────────────────────────────────────────── -->
      <section class="dk-panel">
        <!-- 「我的提示词」页签里多一个「新建」（其余页签是共享目录，只能看）。 -->
        <div v-if="mineSelected" class="dk-insp-mine-head">
          <button type="button" class="dk-button dk-button--sm" @click="openCreate()">
            {{ t('designkit.myPrompts.create') }}
          </button>
        </div>

        <!-- 第一次加载 -->
        <p v-if="loading && prompts.length === 0" class="dk-muted">
          {{ t('designkit.common.loading') }}
        </p>

        <!-- 读失败 -->
        <div v-else-if="loadError && prompts.length === 0" class="dk-alert dk-alert--danger">
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

        <!-- 库是空的：还没同步过。**不要显示一个空白列表。** -->
        <div v-else-if="libraryEmpty" class="dk-empty">
          <p class="dk-empty__title">{{ t('designkit.inspiration.emptyTitle') }}</p>
          <p class="dk-empty__description">
            {{ isAdmin
              ? t('designkit.inspiration.emptyForAdmin')
              : t('designkit.inspiration.emptyForUser') }}
          </p>
          <RouterLink class="dk-button" :to="DESIGNKIT_WORKBENCH_PATH">
            {{ t('designkit.inspiration.goWorkbench') }}
          </RouterLink>
        </div>

        <!-- 「我的提示词」还一条没存过（没在搜索时才算空，搜不到是另一回事） -->
        <div v-else-if="mineEmpty" class="dk-empty">
          <p class="dk-empty__description">{{ t('designkit.myPrompts.empty') }}</p>
          <button type="button" class="dk-button" @click="openCreate()">
            {{ t('designkit.myPrompts.create') }}
          </button>
        </div>

        <!-- 库里有词，但这个条件下搜不到 -->
        <div v-else-if="prompts.length === 0" class="dk-empty">
          <p class="dk-empty__title">{{ t('designkit.inspiration.noResultTitle') }}</p>
          <p class="dk-empty__description">{{ t('designkit.inspiration.noResultHint') }}</p>
          <button type="button" class="dk-button dk-button--secondary" @click="resetFilters()">
            {{ t('designkit.inspiration.resetFilters') }}
          </button>
        </div>

        <!-- 有结果 -->
        <div v-else class="dk-insp-grid">
          <PromptCard
            v-for="prompt in prompts"
            :key="prompt.uid"
            :prompt="prompt"
            :editable="mineSelected"
            @open="mineSelected ? openEdit(prompt) : openDetail(prompt)"
            @use="useFromCard(prompt)"
            @edit="openEdit(prompt)"
            @delete="removeMine(prompt)"
          />
        </div>
      </section>

      <!-- 翻页：只有「加载更多」（后端是游标分页，没有页码） -->
      <div v-if="prompts.length > 0" class="dk-pager">
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
        <p v-if="loadError && prompts.length > 0" class="dk-note dk-note--danger">
          {{ loadError?.message }}
        </p>
      </div>

      <!-- 署名：上游是 CC BY 4.0，整页显示一次即可（不必逐条重复）。
           「我的提示词」页签里不显示——那些是运营自己写的，不是 YouMind 的。 -->
      <p v-if="!mineSelected" class="dk-note dk-note--quiet">
        {{ t('designkit.inspiration.attribution') }}
      </p>

      <PromptDetailDialog
        :show="detailPrompt !== null"
        :prompt="detailPrompt"
        @close="detailPrompt = null"
        @use="sendToWorkbench"
      />

      <MyPromptDialog
        :show="editorOpen"
        :mode="editorMode"
        :initial-title="editorTitle"
        :initial-body="editorBody"
        :saving="editorSaving"
        :error="editorError"
        @close="editorOpen = false"
        @save="saveMine"
      />
    </div>
  </AppLayout>
</template>

<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import { RouterLink, useRoute, useRouter } from 'vue-router'
import AppLayout from '@/components/layout/AppLayout.vue'
import { useAppStore, useAuthStore } from '@/stores'
import {
  MAX_PROMPT_KEYWORD_LENGTH,
  MY_PROMPTS_FILTER,
  createMyPrompt,
  deleteMyPrompt,
  isCanceledError,
  listPromptCategories,
  listPrompts,
  toFriendlyError,
  updateMyPrompt,
} from '../api'
import type { FriendlyError, MyPromptInput, Prompt, PromptCategory } from '../api'
import { DESIGNKIT_WORKBENCH_PATH } from '../nav'
import { usePromptHandoffStore } from '../stores/promptHandoff'
import InspirationSyncPanel from '../components/inspiration/InspirationSyncPanel.vue'
import MyPromptDialog from '../components/inspiration/MyPromptDialog.vue'
import PromptCard from '../components/inspiration/PromptCard.vue'
import PromptCategoryFilter from '../components/inspiration/PromptCategoryFilter.vue'
import PromptDetailDialog from '../components/inspiration/PromptDetailDialog.vue'
// 页面里用到的通用样式（按钮、卡片、空状态、翻页），全局引入，Vite 会去重。
import '../components/designkit-ui.css'

/**
 * 一次拉多少条。
 *
 * 24 是三列刚好八行。后端上限是 100，但**不要真的要 100**：
 * 它要多取一条来判断「还有没有下一页」，要满 100 时实际只会给回 99 条。
 */
const PAGE_SIZE = 24

/** 搜索防抖：手一停再搜，别每敲一个字就发一次请求。 */
const SEARCH_DEBOUNCE_MS = 350

const { t } = useI18n()
const route = useRoute()
const router = useRouter()
const appStore = useAppStore()
const authStore = useAuthStore()
const handoff = usePromptHandoffStore()

/** 管理员才看得到同步区。**用 v-if 整块不渲染**，不是 CSS 隐藏。 */
const isAdmin = computed(() => authStore.isAdmin)

// ---- 筛选条件（记在网址上，刷新页面还在原地） ----
/** 输入框里的字。防抖之后才写进 keyword。 */
const keywordInput = ref(queryValue('keyword'))
/** 真正拿去查询的关键词。 */
const keyword = ref(keywordInput.value)
/** 选中的分类 slug；空串 = 全部。 */
const category = ref(queryValue('category'))

// ---- 数据 ----
const categories = ref<PromptCategory[]>([])
const totalPromptCount = ref(0)
const categoriesError = ref('')

const prompts = ref<Prompt[]>([])
const total = ref(0)
const cursor = ref<string | null>(null)
const hasMore = ref(false)
const loading = ref(false)
const loadingMore = ref(false)
const loadError = ref<FriendlyError | null>(null)

/** 打开详情的那一条；null = 没开。 */
const detailPrompt = ref<Prompt | null>(null)

// ---- 我的提示词 ----
/** 「我的提示词」页签上的条数；null = 还没拿到（页签照常显示，只是不带数字）。 */
const myCount = ref<number | null>(null)
/** 新建 / 编辑弹窗。 */
const editorOpen = ref(false)
const editorMode = ref<'create' | 'edit'>('create')
/** 正在编辑哪条；新建时是空串。 */
const editorUid = ref('')
const editorTitle = ref('')
const editorBody = ref('')
const editorSaving = ref(false)
const editorError = ref('')

let listController: AbortController | null = null
let categoryController: AbortController | null = null
let searchTimer: number | null = null

/** 关键词超长：后端会 400（不会静默截断），所以前端先拦一道。 */
const keywordTooLong = computed(() => keywordInput.value.trim().length > MAX_PROMPT_KEYWORD_LENGTH)

/** 现在开的是不是「我的提示词」页签（category 里放的是前端内部标记）。 */
const mineSelected = computed(() => category.value === MY_PROMPTS_FILTER)

/** 有没有在筛（决定空列表该说哪句话）。 */
const filtering = computed(() => keyword.value.trim() !== '' || category.value !== '')

/** 「我的提示词」一条都没存过（没在搜索时才算空，搜不到是另一回事）。 */
const mineEmpty = computed(
  () => mineSelected.value && !loading.value && prompts.value.length === 0 && keyword.value.trim() === '',
)

/**
 * 整个灵感库是不是空的（还没同步过）。
 *
 * 判据是「没在筛、也一条都没有」——在筛的时候搜不到是另一回事。
 */
const libraryEmpty = computed(
  () => !loading.value && prompts.value.length === 0 && !filtering.value && total.value === 0,
)

/** 「共 N 条」/「找到 N 条」。灵感库一万多条，不给总数运营不知道该搜还是该翻。 */
const resultSummary = computed(() => {
  if (loading.value && prompts.value.length === 0) {
    return t('designkit.common.loading')
  }
  if (filtering.value) {
    return t('designkit.inspiration.resultFiltered', { count: total.value })
  }
  return t('designkit.inspiration.resultTotal', { count: total.value })
})

function queryValue(name: string): string {
  const raw = route.query[name]
  const value = Array.isArray(raw) ? raw[0] : raw
  return typeof value === 'string' ? value.trim() : ''
}

// ---------------------------------------------------------------------------
// 拉数据
// ---------------------------------------------------------------------------

async function fetchCategories(): Promise<void> {
  categoryController?.abort()
  categoryController = new AbortController()
  categoriesError.value = ''
  try {
    const data = await listPromptCategories(categoryController.signal)
    categories.value = data.categories
    totalPromptCount.value = data.total_prompt_count
  } catch (error) {
    if (isCanceledError(error)) {
      return
    }
    // 分类拉不到不该让整页瘫掉：列表还能用，只是少了那排标签。
    categories.value = []
    categoriesError.value = toFriendlyError(error).message
  }
}

/**
 * 拉一页提示词。
 *
 * @param append true = 接在现有列表后面（「加载更多」），false = 从头重来
 */
async function fetchPage(append: boolean): Promise<void> {
  if (keywordTooLong.value) {
    // 超长的词后端会 400，没必要发出去。
    return
  }
  listController?.abort()
  listController = new AbortController()
  const signal = listController.signal
  if (append) {
    loadingMore.value = true
  } else {
    loading.value = true
  }
  loadError.value = null
  try {
    // 「我的提示词」页签走 source=user（MY_PROMPTS_FILTER 是前端内部标记，
    // 绝不能当分类 slug 发出去——后端只认 source 参数）。
    const page = await listPrompts(
      mineSelected.value
        ? {
            source: 'user',
            keyword: keyword.value,
            cursor: append ? cursor.value : null,
            limit: PAGE_SIZE,
            signal,
          }
        : {
            category: category.value,
            keyword: keyword.value,
            cursor: append ? cursor.value : null,
            limit: PAGE_SIZE,
            signal,
          },
    )
    prompts.value = append ? [...prompts.value, ...page.items] : page.items
    total.value = page.total
    cursor.value = page.next_cursor
    hasMore.value = page.has_more
    // 没在搜索时，这一页的 total 就是「我存了多少条」，顺手把页签上的数字校准。
    if (mineSelected.value && keyword.value.trim() === '') {
      myCount.value = page.total
    }
  } catch (error) {
    if (isCanceledError(error)) {
      return
    }
    loadError.value = toFriendlyError(error)
  } finally {
    if (!signal.aborted) {
      loading.value = false
      loadingMore.value = false
    }
  }
}

function reload(): void {
  cursor.value = null
  hasMore.value = false
  void fetchPage(false)
}

/** 同步跑完之后：分类条数和列表都变了，两样都要重新拉。 */
function reloadAll(): void {
  void fetchCategories()
  reload()
}

function loadMore(): void {
  if (!hasMore.value || loadingMore.value || loading.value) {
    return
  }
  void fetchPage(true)
}

// ---------------------------------------------------------------------------
// 筛选
// ---------------------------------------------------------------------------

// 打字防抖：停 350 毫秒再搜。
watch(keywordInput, (value) => {
  if (searchTimer !== null) {
    window.clearTimeout(searchTimer)
  }
  searchTimer = window.setTimeout(() => {
    if (keywordTooLong.value) {
      // 超长的词后端会 400。这里干脆不查，让上面那句红字告诉他精简一下，
      // 列表保持原样——**不静默截断**，否则他搜的是 A、结果是 B。
      return
    }
    keyword.value = value.trim()
  }, SEARCH_DEBOUNCE_MS)
})

// 条件变了：重新查，并把条件写回网址（刷新页面还在原地）。
watch([keyword, category], () => {
  syncQuery()
  reload()
})

function syncQuery(): void {
  const query: Record<string, string> = {}
  for (const [key, value] of Object.entries(route.query)) {
    if (typeof value === 'string' && key !== 'keyword' && key !== 'category') {
      query[key] = value
    }
  }
  if (keyword.value !== '') {
    query.keyword = keyword.value
  }
  if (category.value !== '') {
    query.category = category.value
  }
  // replace 不是 push：翻找提示词时按十几次「后退」才退得出这个页面，太难用。
  void router.replace({ query })
}

function clearKeyword(): void {
  keywordInput.value = ''
  keyword.value = ''
}

function resetFilters(): void {
  keywordInput.value = ''
  keyword.value = ''
  category.value = ''
}

// ---------------------------------------------------------------------------
// 带到工作台
// ---------------------------------------------------------------------------

function openDetail(prompt: Prompt): void {
  detailPrompt.value = prompt
}

/**
 * 卡片上的「用它生成」。
 *
 * 有占位符的条目**先打开详情**让运营填：不填就带走的话，
 * `{主体}` 这种占位符会被原样当成文字念进出图请求里，出来的图必然不对，
 * 而那时候钱已经花了。没有占位符的直接带走，不多一次点击。
 */
function useFromCard(prompt: Prompt): void {
  if (prompt.variables.length > 0) {
    openDetail(prompt)
    return
  }
  sendToWorkbench({ uid: prompt.uid, title: prompt.title, text: prompt.body })
}

/**
 * 真正带过去：存进 pinia，然后跳工作台。
 *
 * **不走网址传参**：提示词动辄上千字，中文编码之后更长，
 * 中间任何一层反向代理都可能把它截断——而运营看不出来（见 stores/promptHandoff.ts）。
 */
function sendToWorkbench(payload: { uid: string; title: string; text: string }): void {
  handoff.send({ uid: payload.uid, title: payload.title, text: payload.text })
  detailPrompt.value = null
  appStore.showSuccess(t('designkit.inspiration.added'))
  void router.push(DESIGNKIT_WORKBENCH_PATH)
}

// ---------------------------------------------------------------------------
// 我的提示词（新建 / 编辑 / 删除）
// ---------------------------------------------------------------------------

/**
 * 拉「我存了多少条」（页签上的数字）。拉不到就不显示数字，页签照常 ——
 * 入口不能跟着一个次要数字一起消失。
 */
async function fetchMineCount(): Promise<void> {
  try {
    const page = await listPrompts({ source: 'user', limit: 1 })
    myCount.value = page.total
  } catch {
    myCount.value = null
  }
}

function openCreate(): void {
  editorMode.value = 'create'
  editorUid.value = ''
  editorTitle.value = ''
  editorBody.value = ''
  editorError.value = ''
  editorOpen.value = true
}

function openEdit(prompt: Prompt): void {
  editorMode.value = 'edit'
  editorUid.value = prompt.uid
  editorTitle.value = prompt.title
  editorBody.value = prompt.body
  editorError.value = ''
  editorOpen.value = true
}

/**
 * 弹窗点了「保存」。失败时弹窗保持打开、错误显示在弹窗里
 * （后端的中文文案自带原因和下一步：上限、youmind 不可改、越权）。
 */
async function saveMine(input: MyPromptInput): Promise<void> {
  if (editorSaving.value) {
    return
  }
  editorSaving.value = true
  editorError.value = ''
  try {
    if (editorMode.value === 'create') {
      await createMyPrompt(input)
    } else {
      await updateMyPrompt(editorUid.value, input)
    }
    editorOpen.value = false
    appStore.showSuccess(t('designkit.myPrompts.saved'))
    void fetchMineCount()
    if (mineSelected.value) {
      reload()
    }
  } catch (error) {
    editorError.value = toFriendlyError(error).message || t('designkit.myPrompts.failed')
  } finally {
    editorSaving.value = false
  }
}

/** 卡片上确认过的删除（原地确认在 PromptCard 里，这里直接删）。 */
async function removeMine(prompt: Prompt): Promise<void> {
  try {
    await deleteMyPrompt(prompt.uid)
    appStore.showSuccess(t('designkit.myPrompts.deleted'))
    void fetchMineCount()
    reload()
  } catch (error) {
    appStore.showError(toFriendlyError(error).message || t('designkit.myPrompts.failed'))
  }
}

onMounted(() => {
  void fetchCategories()
  void fetchMineCount()
  reload()
})

onBeforeUnmount(() => {
  if (searchTimer !== null) {
    window.clearTimeout(searchTimer)
  }
  listController?.abort()
  categoryController?.abort()
})
</script>

<style scoped>
.dk-insp-search {
  display: flex;
  align-items: center;
  gap: var(--dk-space-2);
}

.dk-insp-mine-head {
  display: flex;
  justify-content: flex-end;
  margin-bottom: var(--dk-space-3);
}

.dk-insp-search .dk-input {
  flex: 1;
  min-width: 0;
}

.dk-insp-grid {
  display: grid;
  gap: var(--dk-space-4);
  grid-template-columns: repeat(auto-fill, minmax(240px, 1fr));
  align-items: start;
}

@media (max-width: 639px) {
  .dk-insp-grid {
    grid-template-columns: minmax(0, 1fr);
    gap: var(--dk-space-3);
  }
}
</style>
