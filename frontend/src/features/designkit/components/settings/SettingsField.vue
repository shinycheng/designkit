<!--
  设置页的一个字段：标题 + 「这是干什么的」+（危险项才有的）「填错会怎样」+ 输入框 + 错误。

  为什么每一项都必须有说明：这一页的使用者是电商运营，不是运维。
  只写一个「同时出几张」的标签，他只会照直觉往大了填 —— 而填大了不报错，
  只是所有人一起变慢，谁也查不出原因。所以：

    hint    这一项是干什么的（跟上游那些字段的说明一个风格）
    danger  填错会怎样（只有危险项才有，显示成黄色提示条）
    error   这次填的哪里不对（红色，保存前的前端校验和保存失败都用它）
-->
<template>
  <div class="dk-settings-field">
    <label class="dk-label" :for="fieldId">{{ label }}</label>

    <p v-if="hint" class="dk-note dk-settings-field__hint">{{ hint }}</p>

    <div class="dk-settings-field__control">
      <!--
        用 v-bind 传插槽参数，**不要写成 `:field-id="fieldId"`**：
        插槽参数的键名是原样传的、不会像组件 props 那样自动转驼峰，
        写成短横线的话调用方 `v-slot="{ fieldId }"` 解构出来是 undefined，
        于是 <label for> 指向空字符串 —— 点标签跳不到输入框，而且不报错。
      -->
      <slot v-bind="{ fieldId }" />
    </div>

    <!--
      「填错会怎样」放在输入框**下面**而不是上面：上面的位置留给「是什么」，
      人是先看标签、再看输入框、最后才找后果的。
    -->
    <p v-if="danger" class="dk-note dk-note--warning">{{ danger }}</p>

    <p v-if="error" class="dk-note dk-note--danger" role="alert">{{ error }}</p>
  </div>
</template>

<script setup lang="ts">
import { computed, useId } from 'vue'

const props = defineProps<{
  label: string
  /** 这一项是干什么的。 */
  hint?: string
  /** 填错会怎样。只有危险项才写。 */
  danger?: string
  /** 这次填的哪里不对；空串表示没问题。 */
  error?: string
  /** 想自己指定 id 时传（`<label for>` 和输入框要对上）。 */
  id?: string
}>()

// useId() 是 Vue 3.5 起自带的；没传 id 时用它生成一个稳定的。
const generatedId = useId()
const fieldId = computed(() => props.id ?? `dk-setting-${generatedId}`)
</script>

<style scoped>
.dk-settings-field {
  display: grid;
  gap: var(--dk-space-2);
  min-width: 0;
}

.dk-settings-field__hint {
  margin: 0;
}

.dk-settings-field__control {
  min-width: 0;
}
</style>
