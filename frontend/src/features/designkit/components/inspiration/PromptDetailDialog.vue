<!--
  一条提示词的详情。

  这里是运营做决定的地方，所以三样东西必须完整、可核对：

  1. **完整正文**（不截断、可复制）——列表里只显示三行，真正要用的是这里这段；
  2. **要填的空**（占位符）——上游那批提示词里有 `{主体}`、`{main_color}` 这种占位符，
     不填就带走的话，出图时它会被原样当成文字念进去。所以这里给每个占位符一个输入框，
     下面实时显示「填完之后长什么样」，运营看到的就是最终会拿去出图的那段字；
  3. **示例图**（如果上游给了）——旁边写清楚「这是别人用这条词出的图，
     不是用你的商品图出的」，否则运营会拿它当成效果承诺。

  留空的占位符**原样保留** `{像这样}`，不悄悄删掉：
  「柔和的{光线}」变成「柔和的」运营看不出少了东西，出的图却会莫名其妙。
  下面会数出还有几处没填，提醒一句，但**不拦着他带走**——工作台里还能改。
-->
<template>
  <BaseDialog
    :show="show"
    :title="t('designkit.inspiration.detailTitle')"
    width="wide"
    @close="emit('close')"
  >
    <div v-if="prompt" class="dk-stack dk-stack--sm">
      <!-- 已经下架：不拦着用，但要说清楚 -->
      <p v-if="retired" class="dk-alert dk-alert--warning">
        {{ t('designkit.inspiration.disabled') }}
      </p>

      <div class="dk-insp-detail">
        <!-- 左：示例图（外部网站的图，拉不到就换成一句说明） -->
        <div v-if="previewURL !== ''" class="dk-insp-detail__media">
          <img
            v-if="!previewFailed"
            :src="previewURL"
            :alt="headline"
            referrerpolicy="no-referrer"
            @error="previewFailed = true"
          />
          <p v-else class="dk-note dk-note--quiet">
            {{ t('designkit.inspiration.previewFailed') }}
          </p>
          <p class="dk-note dk-note--quiet">{{ t('designkit.inspiration.previewNote') }}</p>
        </div>

        <!-- 右：标题、占位符、最终正文 -->
        <div class="dk-insp-detail__main">
          <h3 class="dk-insp-detail__title">{{ headline }}</h3>
          <p v-if="categoryName !== ''" class="dk-note dk-note--quiet">
            {{ t('designkit.inspiration.categoryTitle') }}：{{ categoryName }}
          </p>

          <!-- 要填的空 -->
          <section v-if="variables.length > 0" class="dk-insp-vars">
            <h4 class="dk-label">{{ t('designkit.inspiration.variablesTitle') }}</h4>
            <p class="dk-note">{{ t('designkit.inspiration.variablesHint') }}</p>
            <div
              v-for="variable in variables"
              :key="variable.name"
              class="dk-insp-var"
            >
              <label class="dk-label dk-label--quiet" :for="`dk-var-${variable.name}`">
                {{ variable.name }}
              </label>
              <input
                :id="`dk-var-${variable.name}`"
                v-model="values[variable.name]"
                type="text"
                class="dk-input"
                :placeholder="variable.example || t('designkit.inspiration.variablePlaceholder')"
              />
            </div>
            <p v-if="unfilled > 0" class="dk-note dk-note--warning">
              {{ t('designkit.inspiration.variablesUnfilled', { count: unfilled }) }}
            </p>
          </section>

          <!-- 最终正文：这就是点「用它生成」会带走的那段字 -->
          <section class="dk-insp-body">
            <div class="dk-panel-head dk-panel-head--tight">
              <h4 class="dk-label">{{ t('designkit.inspiration.bodyLabel') }}</h4>
              <button
                type="button"
                class="dk-button dk-button--quiet dk-button--sm"
                @click="copyBody"
              >
                {{ copied ? t('designkit.common.copied') : t('designkit.common.copy') }}
              </button>
            </div>
            <p class="dk-insp-body__text dk-prewrap">{{ finalText }}</p>
          </section>
        </div>
      </div>

      <p class="dk-note dk-note--quiet">{{ t('designkit.inspiration.attribution') }}</p>
    </div>

    <template #footer>
      <div class="dk-dialog-actions">
        <button type="button" class="dk-button dk-button--secondary" @click="emit('close')">
          {{ t('designkit.common.close') }}
        </button>
        <button type="button" class="dk-button" :disabled="finalText === ''" @click="use">
          {{ t('designkit.inspiration.useThis') }}
        </button>
      </div>
    </template>
  </BaseDialog>
</template>

<script setup lang="ts">
import { computed, reactive, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import BaseDialog from '@/components/common/BaseDialog.vue'
import { applyPromptVariables, countUnfilledVariables } from '../../api'
import type { Prompt } from '../../api'

const props = defineProps<{
  show: boolean
  /** 正在看的那条；关着的时候是 null。 */
  prompt: Prompt | null
}>()

const emit = defineEmits<{
  (e: 'close'): void
  /** 带走：uid 留个来源记号，text 是替换完占位符的最终正文。 */
  (e: 'use', payload: { uid: string; title: string; text: string }): void
}>()

const { t } = useI18n()

/** 每个占位符填了什么。key 是变量名。 */
const values = reactive<Record<string, string>>({})

const previewFailed = ref(false)
const copied = ref(false)

/**
 * 换一条（或者重新打开）就把输入框重置一遍。
 *
 * **默认填上上游给的默认值**：那本来就是这条提示词设计时的取值，
 * 直接就能用；运营想换再改。留空会让人以为「必须自己想一个」。
 */
watch(
  () => [props.prompt?.uid, props.show] as const,
  () => {
    for (const key of Object.keys(values)) {
      delete values[key]
    }
    for (const variable of props.prompt?.variables ?? []) {
      values[variable.name] = variable.example ?? ''
    }
    previewFailed.value = false
    copied.value = false
  },
  { immediate: true },
)

// 下面这几个都做成 computed 而不是在模板里点 `prompt.xxx`：
// prompt 可能是 null（弹窗关着的时候），模板里逐处判空既啰嗦又容易漏一处。
/** 示例图地址；没有就是空串。 */
const previewURL = computed(() => (props.prompt?.preview_url ?? '').trim())
/** 占位变量；没有就是空数组，模板可以直接 v-for。 */
const variables = computed(() => props.prompt?.variables ?? [])
const categoryName = computed(() => props.prompt?.category_name ?? '')
/** 这条已经下架了（历史收藏点进来才会遇到）。 */
const retired = computed(() => props.prompt !== null && !props.prompt.is_enabled)

/** 标题；没有标题的条目拿正文开头顶上（不能留空白）。 */
const headline = computed(() => {
  const prompt = props.prompt
  if (!prompt) {
    return ''
  }
  const title = prompt.title.trim()
  if (title !== '') {
    return title
  }
  const body = prompt.body.trim().replace(/\s+/g, ' ')
  return body.length > 40 ? `${body.slice(0, 40)}…` : body
})

/** 替换完占位符的最终正文。**「用它生成」带走的就是它。** */
const finalText = computed(() => {
  const prompt = props.prompt
  if (!prompt) {
    return ''
  }
  return applyPromptVariables(prompt.body, values).trim()
})

/** 还有几处没填。只提醒，不阻止。 */
const unfilled = computed(() => {
  const prompt = props.prompt
  if (!prompt) {
    return 0
  }
  return countUnfilledVariables(prompt.body, prompt.variables, values)
})

/**
 * 复制到剪贴板。
 *
 * ⚠ 群晖内网是 http 不是 https，浏览器在非安全环境里**没有** `navigator.clipboard`，
 * 所以必须有降级路径，否则这个按钮在 NAS 上点了没反应（还不报错）。
 */
async function copyBody(): Promise<void> {
  const text = finalText.value
  if (text === '') {
    return
  }
  let ok = false
  try {
    if (navigator.clipboard && window.isSecureContext) {
      await navigator.clipboard.writeText(text)
      ok = true
    }
  } catch {
    ok = false
  }
  if (!ok) {
    ok = copyByTextarea(text)
  }
  if (ok) {
    copied.value = true
    window.setTimeout(() => {
      copied.value = false
    }, 2000)
  }
}

/** 老办法：塞一个看不见的 textarea，选中，execCommand('copy')。 */
function copyByTextarea(text: string): boolean {
  try {
    const area = document.createElement('textarea')
    area.value = text
    area.setAttribute('readonly', 'readonly')
    area.style.position = 'fixed'
    area.style.top = '-1000px'
    area.style.opacity = '0'
    document.body.appendChild(area)
    area.select()
    const ok = document.execCommand('copy')
    document.body.removeChild(area)
    return ok
  } catch {
    return false
  }
}

function use(): void {
  const prompt = props.prompt
  if (!prompt || finalText.value === '') {
    return
  }
  emit('use', { uid: prompt.uid, title: headline.value, text: finalText.value })
}
</script>

<style scoped>
.dk-insp-detail {
  display: grid;
  gap: var(--dk-space-4);
  grid-template-columns: minmax(0, 260px) minmax(0, 1fr);
  align-items: start;
}

/* 窄屏：示例图挪到上面，正文占满一行 */
@media (max-width: 767px) {
  .dk-insp-detail {
    grid-template-columns: minmax(0, 1fr);
  }
}

.dk-insp-detail__media {
  display: grid;
  gap: var(--dk-space-2);
  min-width: 0;
}

.dk-insp-detail__media img {
  width: 100%;
  border-radius: var(--dk-radius-md);
  border: 1px solid var(--dk-border);
  background: var(--dk-surface-subtle);
  display: block;
}

.dk-insp-detail__main {
  display: grid;
  gap: var(--dk-space-3);
  min-width: 0;
}

.dk-insp-detail__title {
  font-size: var(--dk-text-lg);
  font-weight: 600;
  color: var(--dk-text);
  line-height: var(--dk-leading-tight);
  margin: 0;
}

.dk-insp-vars {
  display: grid;
  gap: var(--dk-space-2);
  padding: var(--dk-space-3);
  border: 1px solid var(--dk-border);
  border-radius: var(--dk-radius-md);
  background: var(--dk-surface-subtle);
}

.dk-insp-var {
  display: grid;
  gap: var(--dk-space-1);
}

.dk-insp-body {
  display: grid;
  gap: var(--dk-space-2);
  min-width: 0;
}

.dk-insp-body__text {
  max-height: 320px;
  overflow-y: auto;
  padding: var(--dk-space-3);
  border: 1px solid var(--dk-border);
  border-radius: var(--dk-radius-md);
  background: var(--dk-surface);
  color: var(--dk-text);
  font-size: var(--dk-text-sm);
  line-height: var(--dk-leading);
  word-break: break-word;
  margin: 0;
}
</style>
