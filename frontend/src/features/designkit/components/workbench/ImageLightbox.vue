<!--
  看大图。

  上游全仓没有图片灯箱（CLAUDE.md 二·五 D4 核实过），所以基于 BaseDialog 自建。

  ⚠ 图片地址不能直接塞 `<img src>`：那个地址要带登录凭证请求头才放行，
  而 `<img>` 不带 → 401 + 碎图标，运营会以为「图没出来」，其实图好好地在服务器上。
  所以这里收的是**已经取好的本地地址**（父组件用 useAuthedImages() 换出来的）。
-->
<template>
  <BaseDialog :show="show" :title="title" width="extra-wide" @close="emit('close')">
    <div class="dk-lightbox-stage">
      <img v-if="src" :src="src" :alt="title" />
      <p v-else-if="failedText" class="dk-note dk-note--danger">{{ failedText }}</p>
      <p v-else class="dk-muted">{{ t('designkit.gallery.imageLoading') }}</p>
    </div>

    <p v-if="caption" class="dk-note dk-mt-3 dk-prewrap">{{ caption }}</p>

    <template #footer>
      <div class="dk-dialog-actions">
        <button type="button" class="dk-button dk-button--secondary" @click="emit('close')">
          {{ t('designkit.common.close') }}
        </button>
        <button
          type="button"
          class="dk-button"
          :disabled="!src || downloading"
          @click="emit('download')"
        >
          {{ downloading ? t('designkit.common.loading') : t('designkit.gallery.downloadOne') }}
        </button>
      </div>
    </template>
  </BaseDialog>
</template>

<script setup lang="ts">
import { useI18n } from 'vue-i18n'
import BaseDialog from '@/components/common/BaseDialog.vue'

defineProps<{
  show: boolean
  /** 已经取好的本地地址（objectURL）；还没取到时为空串。 */
  src: string
  /** 弹窗标题，例如「第 3 张」。 */
  title: string
  /** 图片下面那行说明（这里放生成用的提示词原文）。 */
  caption: string
  /** 取图失败的中文原因；有值时显示它，不显示「加载中」。 */
  failedText: string
  downloading: boolean
}>()

const emit = defineEmits<{
  (e: 'close'): void
  (e: 'download'): void
}>()

const { t } = useI18n()
</script>
