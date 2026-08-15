<!--
  设置页的一张分区卡片。

  版式跟上游「系统设置」（views/admin/SettingsView.vue）的卡片一致：
  标题 + 一句说明的表头，下面是表单区。这里用 designkit 自己的 dk-panel
  （全站换肤之后，dk-panel 就是上游 `card` 在 designkit 里的样子），
  所以跟工作台、灵感库那几页也是同一个观感。

  用 <fieldset> 而不是 <div>：整页保存时可以一次性把所有输入框禁用掉
  （:disabled），不用给每个输入框都挂一遍。
-->
<template>
  <fieldset class="dk-panel dk-settings-section" :disabled="disabled">
    <div class="dk-panel-head dk-panel-head--tight">
      <div>
        <legend class="dk-panel-title">{{ title }}</legend>
        <p v-if="hint" class="dk-panel-hint">{{ hint }}</p>
      </div>
      <slot name="actions" />
    </div>

    <div class="dk-settings-section__body">
      <slot />
    </div>
  </fieldset>
</template>

<script setup lang="ts">
defineProps<{
  title: string
  /** 一句话说明这一块是干什么的。 */
  hint?: string
  /** 保存过程中整块禁用，避免改到一半被提交上去。 */
  disabled?: boolean
}>()
</script>

<style scoped>
/* fieldset 自带边框和内边距，先清掉再让 dk-panel 生效。 */
.dk-settings-section {
  min-width: 0;
  border: 1px solid var(--dk-border);
  margin: 0;
}

.dk-settings-section__body {
  display: grid;
  gap: var(--dk-space-5);
  margin-top: var(--dk-space-4);
}

/* legend 默认脱离文档流，改成普通块级元素才能跟上游卡片标题对齐。 */
.dk-settings-section :deep(legend) {
  display: block;
  float: none;
  padding: 0;
}
</style>
