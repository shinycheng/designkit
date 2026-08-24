<!--
  文案检查：贴入标题或文案，查广告法违禁词 + 平台标题字数。

  三件事：

  1. **边输边查。** 接口是纯本地计算（词库编译在后端二进制里），毫秒级返回，
     所以输入防抖 500ms 自动查，不用等人点按钮；「检查」按钮保留给
     「改了平台立刻想看结果」和「不相信自动检查」的人。

  2. **标红用只读预览块，不用 contenteditable。**
     contenteditable 里程序改 HTML 会把光标踢走，输入体验直接报废。
     输入框和预览块分开：上面输入，下面看标红。
     ⚠ 预览是 v-html，必须先转义再包 <mark>（见 previewHtml），
     顺序反了就是给任何能改文案的人开 XSS 口子。

  3. **命中下标按码点对齐。** 后端 start/end 是按字（rune）数的，
     JS 里必须 Array.from(text) 切成码点数组再 slice，直接 text.slice()
     是 UTF-16 单元，文案里有 emoji 时标红会错位一格。

  外壳用上游的 AppLayout，跟其它侧边栏页面一致。
-->
<template>
  <AppLayout>
    <div class="dk-canvas">
      <header class="dk-canvas-header">
        <div class="dk-page-head">
          <p class="dk-eyebrow">{{ t('designkit.brand') }}</p>
          <h1 class="dk-page-title">{{ t('designkit.contentCheck.title') }}</h1>
          <p class="dk-page-desc">{{ t('designkit.contentCheck.description') }}</p>
        </div>
      </header>

      <div class="dk-canvas-body">
        <!-- 输入 -->
        <section class="dk-panel">
          <textarea
            v-model="text"
            class="dk-textarea dk-cc-input"
            rows="6"
            :maxlength="MAX_CONTENT_CHECK_LENGTH"
            :placeholder="t('designkit.contentCheck.placeholder')"
          ></textarea>

          <div class="dk-cc-toolbar">
            <label class="dk-label dk-label--quiet dk-cc-platform-label" for="dk-cc-platform">
              {{ t('designkit.contentCheck.platformLabel') }}
            </label>
            <select id="dk-cc-platform" v-model="platformKey" class="dk-cc-select">
              <option value="">{{ t('designkit.contentCheck.platformNone') }}</option>
              <option v-for="p in CONTENT_CHECK_PLATFORMS" :key="p.key" :value="p.key">
                {{ p.name }}
              </option>
            </select>

            <!-- 字数条：本地实时算（接口响应只用于命中），超出变红 -->
            <span class="dk-cc-count" :class="{ 'dk-cc-count--over': isOver }">
              <template v-if="selectedPlatform">
                {{ t('designkit.contentCheck.titleLen', { len: liveLen, max: selectedPlatform.max }) }}
                <template v-if="isOver">
                  · {{ t('designkit.contentCheck.titleOver', { n: liveLen - selectedPlatform.max }) }}
                </template>
              </template>
              <template v-else>
                {{ t('designkit.contentCheck.lenOnly', { len: liveLen }) }}
              </template>
            </span>

            <button
              type="button"
              class="dk-button dk-button--primary dk-button--sm dk-cc-check"
              :disabled="checking || text.trim() === ''"
              @click="runCheck()"
            >
              {{ t('designkit.contentCheck.check') }}
            </button>
          </div>

          <p class="dk-note dk-note--quiet dk-mt-1">{{ t('designkit.contentCheck.note') }}</p>
        </section>

        <!-- 检查失败 -->
        <div v-if="checkError" class="dk-alert dk-alert--danger">
          <p>{{ checkError?.message }}</p>
          <p v-if="checkError?.requestId" class="dk-alert__sub">
            {{ t('designkit.errors.requestId', { id: checkError?.requestId }) }}
          </p>
        </div>

        <!-- 结果：查过一次并且当时有文字才显示 -->
        <section v-else-if="result && checkedText !== ''" class="dk-panel">
          <div class="dk-panel-head dk-panel-head--tight">
            <h2 class="dk-panel-title">
              <template v-if="result.hits.length > 0">
                {{ t('designkit.contentCheck.hits', { count: result.hits.length }) }}
              </template>
              <template v-else>
                {{ t('designkit.contentCheck.noHits') }}
              </template>
            </h2>
          </div>

          <!-- 命中词汇总：同一个词出现几次合并成一枚「词 ×N」 -->
          <div v-if="hitSummary.length > 0" class="dk-cc-words">
            <span v-for="entry in hitSummary" :key="entry.word" class="dk-cc-word">
              {{ entry.word }}<template v-if="entry.count > 1"> ×{{ entry.count }}</template>
            </span>
          </div>

          <!-- 正文标红。只读预览：先转义再包 mark，见 previewHtml 的注释 -->
          <!-- eslint-disable-next-line vue/no-v-html -->
          <div class="dk-cc-preview" v-html="previewHtml"></div>
        </section>

        <!-- 还没查过：说明结果区在哪，别让下半页是一片空白 -->
        <div v-else class="dk-empty dk-empty--boxed">
          <img class="dk-empty__illus" src="/designkit/illustrations/empty-contentcheck.svg" alt="" />
          <p class="dk-empty__description">{{ t('designkit.contentCheck.resultsPlaceholder') }}</p>
        </div>
      </div>
    </div>
  </AppLayout>
</template>

<script setup lang="ts">
import { computed, onUnmounted, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import AppLayout from '@/components/layout/AppLayout.vue'
import {
  CONTENT_CHECK_PLATFORMS,
  MAX_CONTENT_CHECK_LENGTH,
  checkContent,
  isCanceledError,
  toFriendlyError,
} from '../api'
import type { ContentCheckResult, FriendlyError } from '../api'
// 页面里用到的通用样式（按钮、卡片、表单……），全局引入，Vite 会去重。
import '../components/designkit-ui.css'

/** 输入停下多久后自动查。接口毫秒级，500ms 是打字节奏，不是接口开销。 */
const DEBOUNCE_MS = 500

const { t } = useI18n()

const text = ref('')
const platformKey = ref('')
const checking = ref(false)
const result = ref<ContentCheckResult | null>(null)
/** 结果对应的原文快照。标红下标必须对着**查的那份**文字，不能对着输入框现值。 */
const checkedText = ref('')
const checkError = ref<FriendlyError | null>(null)

let controller: AbortController | null = null
let debounceTimer: ReturnType<typeof setTimeout> | null = null

const selectedPlatform = computed(
  () => CONTENT_CHECK_PLATFORMS.find((p) => p.key === platformKey.value) ?? null,
)

/**
 * 实时字数：按码点数（Array.from），跟后端 rune 口径一致，首尾空白不算。
 * 这里不等接口——字数条要跟着键盘走，命中列表才等接口。
 */
const liveLen = computed(() => Array.from(text.value.trim()).length)

const isOver = computed(
  () => selectedPlatform.value !== null && liveLen.value > selectedPlatform.value.max,
)

/** 命中词汇总：同词合并计数，保持首次出现的顺序。 */
const hitSummary = computed(() => {
  if (!result.value) {
    return [] as Array<{ word: string; count: number }>
  }
  const order: string[] = []
  const counts = new Map<string, number>()
  for (const hit of result.value.hits) {
    if (!counts.has(hit.word)) {
      order.push(hit.word)
    }
    counts.set(hit.word, (counts.get(hit.word) ?? 0) + 1)
  }
  return order.map((word) => ({ word, count: counts.get(word) ?? 0 }))
})

/** HTML 转义。必须在包 <mark> **之前**做，顺序反了就是 XSS。 */
function escapeHtml(raw: string): string {
  return raw
    .replace(/&/g, '&amp;')
    .replace(/</g, '&lt;')
    .replace(/>/g, '&gt;')
    .replace(/"/g, '&quot;')
    .replace(/'/g, '&#39;')
}

/**
 * 标红预览的 HTML：命中区间（可能部分重叠，先合并）包 <mark>，其余原样转义。
 * 下标按码点对齐（Array.from），直接 slice 字符串遇 emoji 会错位。
 */
const previewHtml = computed(() => {
  if (!result.value || checkedText.value === '') {
    return ''
  }
  const chars = Array.from(checkedText.value)
  // 后端命中按位置排好序了；部分重叠的区间（如「全网最低」+「最低价」）合并成一段标红。
  const ranges: Array<[number, number]> = []
  for (const hit of result.value.hits) {
    const last = ranges[ranges.length - 1]
    if (last && hit.start <= last[1]) {
      last[1] = Math.max(last[1], hit.end)
    } else {
      ranges.push([hit.start, hit.end])
    }
  }
  let html = ''
  let pos = 0
  for (const [start, end] of ranges) {
    html += escapeHtml(chars.slice(pos, start).join(''))
    html += '<mark>' + escapeHtml(chars.slice(start, end).join('')) + '</mark>'
    pos = end
  }
  html += escapeHtml(chars.slice(pos).join(''))
  return html
})

/** 真正调接口。空文本不发请求，直接清结果。 */
async function runCheck(): Promise<void> {
  if (debounceTimer !== null) {
    clearTimeout(debounceTimer)
    debounceTimer = null
  }
  const snapshot = text.value
  if (snapshot.trim() === '') {
    controller?.abort()
    result.value = null
    checkedText.value = ''
    checkError.value = null
    return
  }
  controller?.abort()
  controller = new AbortController()
  checking.value = true
  try {
    const data = await checkContent(
      { text: snapshot, platform: platformKey.value },
      controller.signal,
    )
    result.value = data
    checkedText.value = snapshot
    checkError.value = null
  } catch (error) {
    if (isCanceledError(error)) {
      return
    }
    checkError.value = toFriendlyError(error)
  } finally {
    checking.value = false
  }
}

// 边输边查：输入或换平台后 500ms 自动查一次。
watch([text, platformKey], () => {
  if (debounceTimer !== null) {
    clearTimeout(debounceTimer)
  }
  debounceTimer = setTimeout(() => {
    debounceTimer = null
    void runCheck()
  }, DEBOUNCE_MS)
})

onUnmounted(() => {
  if (debounceTimer !== null) {
    clearTimeout(debounceTimer)
    debounceTimer = null
  }
  controller?.abort()
  controller = null
})
</script>

<style scoped>
.dk-cc-input {
  min-height: 140px;
}

.dk-cc-toolbar {
  display: flex;
  align-items: center;
  flex-wrap: wrap;
  gap: 10px;
  margin-top: 10px;
}

.dk-cc-platform-label {
  margin-bottom: 0;
}

/* 下拉框对齐 .dk-input 的观感（designkit-ui.css 没有现成的 select 样式）。 */
.dk-cc-select {
  min-height: var(--dk-control-h);
  border: 1px solid var(--dk-border-strong);
  border-radius: var(--dk-radius-md);
  background: var(--dk-surface);
  color: var(--dk-text);
  padding: 6px 10px;
  font-family: inherit;
  font-size: var(--dk-text-md);
}

.dk-cc-select:focus {
  border-color: var(--dk-accent);
  box-shadow: 0 0 0 3px var(--dk-focus);
  outline: none;
}

.dk-cc-count {
  color: var(--dk-text-secondary);
  font-size: var(--dk-text-sm);
  font-variant-numeric: tabular-nums;
}

.dk-cc-count--over {
  color: var(--dk-danger);
  font-weight: 650;
}

.dk-cc-check {
  margin-left: auto;
}

.dk-cc-words {
  display: flex;
  flex-wrap: wrap;
  gap: 6px;
  margin-bottom: 10px;
}

.dk-cc-word {
  display: inline-block;
  padding: 2px 8px;
  border-radius: var(--dk-radius-md);
  background: var(--dk-danger-soft);
  color: var(--dk-danger);
  font-size: var(--dk-text-sm);
  line-height: 1.6;
}

.dk-cc-preview {
  border: 1px solid var(--dk-border);
  border-radius: var(--dk-radius-md);
  background: var(--dk-surface-subtle);
  color: var(--dk-text);
  padding: 10px 12px;
  font-size: var(--dk-text-md);
  line-height: 1.7;
  /* 保留运营贴进来的换行和连续空格 */
  white-space: pre-wrap;
  word-break: break-word;
}

/* ⚠ v-html 塞进去的元素拿不到 scoped 属性标记，普通选择器不生效（CLAUDE.md 决策 30
   踩过同一个坑），必须 :deep()。 */
.dk-cc-preview :deep(mark) {
  background: var(--dk-danger-soft);
  color: var(--dk-danger-strong);
  border-radius: 3px;
  padding: 0 1px;
}
</style>
