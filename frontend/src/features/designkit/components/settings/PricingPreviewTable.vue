<!--
  价格预览：每个比例按现在填的「图片最长边」会出多大、落哪一档、一张多少钱。

  为什么要有这张表：「最长边直接决定 2K 还是 4K 计费档」这句话，
  光写在说明里没人看得懂，也没人会去算。让管理员把 2048 改成 4096、
  马上看见「计费档」那一列从 2K 跳到 4K，这件事才算说清楚了。

  ⚠ **没填单价的档显示「价格待确认」，绝不显示 $0。**
  $0 会被读成「免费」，然后一口气排几十张。
-->
<template>
  <div class="dk-price-preview">
    <p class="dk-note">{{ t('designkit.settings.previewHint') }}</p>
    <p v-if="dirty" class="dk-note dk-note--warning">
      {{ t('designkit.settings.previewUnsaved') }}
    </p>

    <p v-if="rows.length === 0" class="dk-muted">
      {{ t('designkit.settings.previewEmpty') }}
    </p>

    <div v-else class="dk-price-preview__scroll">
      <table class="dk-price-preview__table">
        <thead>
          <tr>
            <th scope="col">{{ t('designkit.settings.previewRatio') }}</th>
            <th scope="col">{{ t('designkit.settings.previewSize') }}</th>
            <th scope="col">{{ t('designkit.settings.previewTier') }}</th>
            <th scope="col">{{ t('designkit.settings.previewUnitPrice') }}</th>
          </tr>
        </thead>
        <tbody>
          <tr v-for="row in rows" :key="row.ratio">
            <td class="dk-price-preview__ratio">{{ row.ratio }}</td>
            <td>{{ row.width }} × {{ row.height }}</td>
            <td>
              <span class="dk-badge" :class="tierBadgeClass(row.tier)">{{ row.tier }}</span>
            </td>
            <td :class="{ 'dk-price-preview__unconfirmed': row.price === null }">
              {{ priceText(row.price) }}
            </td>
          </tr>
        </tbody>
      </table>
    </div>

    <!--
      倍率单独说一句。上面那一列是**不含倍率**的单张价，跟运营在工作台看到的一致；
      倍率只作用在「这一批的总花费」上。不点破的话，管理员把倍率改成 1.2 之后
      会以为表里的数没生效。
    -->
    <p v-if="multiplier !== 1" class="dk-note">
      {{ t('designkit.settings.previewMultiplierNote', { multiplier: multiplierText }) }}
    </p>
  </div>
</template>

<script setup lang="ts">
import { computed } from 'vue'
import { useI18n } from 'vue-i18n'
import { formatPrice, toPrice } from '../../api'

interface PreviewRow {
  ratio: string
  width: number
  height: number
  tier: string
  /** null = 这一档没填单价 = 价格待确认。 */
  price: string | null
}

const props = defineProps<{
  /** 比例列表（表单里现在填的那一份，已经去掉空行和写法不对的）。 */
  ratios: string[]
  /** 现在填的最长边像素。 */
  maxDimension: number
  /** 档位 → 单价原文；空串表示这一档还没填。 */
  unitPrices: Record<string, string>
  /** 倍率原文；空串按 1 算。 */
  rateMultiplier: string
  /** 表单还没保存（表里的数是按「现在填的」算的，不是数据库里的）。 */
  dirty?: boolean
}>()

const { t } = useI18n()

/**
 * 按像素分计费档。
 *
 * **判据必须跟后端一致**（domain/ratio.go 的 ClassifyBillingTier）：
 * 取最长边，≤1024 → 1K，≤2048 → 2K，更大 → 4K。
 * 两边不一致的话，预览说 2K、实际按 4K 收钱，而且没人会发现。
 */
function classifyTier(width: number, height: number): string {
  const longest = Math.max(width, height)
  if (longest <= 1024) {
    return '1K'
  }
  if (longest <= 2048) {
    return '2K'
  }
  return '4K'
}

/**
 * 「补白边到这个比例、最长边不超过 maxDimension」之后的目标像素。
 *
 * 跟后端 domain/ratio.go 的 TargetSize **逐字对齐**：长边取 maxDimension，
 * 短边按比例缩。
 *
 * ⚠ 短边一定要用 `Math.floor` 而不是 `Math.round`：Go 那边是整数除法（直接截断）。
 * 用四舍五入的话，比例除不尽时会比后端多 1 像素 —— 平时看不出来，
 * 但正好卡在 1024 / 2048 边界上时，预览会显示成另一个计费档。
 */
function targetSize(ratio: string, maxDimension: number): { width: number; height: number } | null {
  const parts = ratio.trim().split(':')
  if (parts.length !== 2) {
    return null
  }
  const w = Number(parts[0])
  const h = Number(parts[1])
  if (!Number.isInteger(w) || !Number.isInteger(h) || w <= 0 || h <= 0) {
    return null
  }
  if (!Number.isInteger(maxDimension) || maxDimension <= 0) {
    return null
  }
  const short = (long: number, a: number, b: number) => Math.max(1, Math.floor((long * a) / b))
  if (w >= h) {
    return { width: maxDimension, height: short(maxDimension, h, w) }
  }
  return { width: short(maxDimension, w, h), height: maxDimension }
}

const multiplier = computed<number>(() => {
  const raw = props.rateMultiplier.trim()
  if (raw === '') {
    return 1
  }
  const value = Number(raw)
  return Number.isFinite(value) && value > 0 ? value : 1
})

const rows = computed<PreviewRow[]>(() => {
  const out: PreviewRow[] = []
  for (const raw of props.ratios) {
    const ratio = raw.trim()
    const size = targetSize(ratio, props.maxDimension)
    if (size === null) {
      // 写法不对的那一行不预览。编一个尺寸出来比不显示更糟。
      continue
    }
    const tier = classifyTier(size.width, size.height)
    const unit = (props.unitPrices[tier] ?? '').trim()
    let price: string | null = null
    if (unit !== '' && Number.isFinite(Number(unit))) {
      // ⚠ **不要乘倍率。** 这一列显示的是「单张价格」，必须跟运营在工作台上
      // 看到的单价是同一个数。工作台的单价是不含倍率的原值，倍率只进
      // 「这一批的预估总花费」（backend/internal/designkit/service/job.go 的
      // costOf：单价 × 张数 × 倍率）。
      // 乘了的话，倍率一旦不是 1，设置页和工作台就会显示两个不同的单价，
      // 而两边都没错——最难查的那种不一致。
      price = unit
    }
    out.push({ ratio, width: size.width, height: size.height, tier, price })
  }
  return out
})

/** 倍率显示成人看的样子：1.2 而不是 1.2000000000000002。 */
const multiplierText = computed<string>(() => String(multiplier.value))

/** null → 「价格待确认」；有值 → `$0.042`。**永远不会是 $0.00。** */
function priceText(price: string | null): string {
  return formatPrice(toPrice(price))
}

function tierBadgeClass(tier: string): string {
  if (tier === '4K') {
    return 'dk-badge--warning'
  }
  if (tier === '1K') {
    return 'dk-badge--success'
  }
  return 'dk-badge--info'
}
</script>

<style scoped>
.dk-price-preview {
  display: grid;
  gap: var(--dk-space-2);
}

/* 窄屏时表格自己横向滚，不让整个页面横着跑 */
.dk-price-preview__scroll {
  overflow-x: auto;
}

.dk-price-preview__table {
  width: 100%;
  border-collapse: collapse;
  font-size: var(--dk-text-sm);
}

.dk-price-preview__table th,
.dk-price-preview__table td {
  padding: var(--dk-space-2) var(--dk-space-3);
  text-align: left;
  white-space: nowrap;
  border-bottom: 1px solid var(--dk-border);
}

.dk-price-preview__table th {
  color: var(--dk-text-secondary);
  font-weight: 600;
}

.dk-price-preview__ratio {
  font-variant-numeric: tabular-nums;
}

.dk-price-preview__unconfirmed {
  color: var(--dk-warning);
}
</style>
