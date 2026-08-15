<!--
  「申请额度」（决策 19）。

  为什么必须有这个东西：充值、套餐、兑换码这些菜单对运营是隐藏的（决策 10），
  所以「余额不足」对他来说本来是死路一条——而管理员这边没有任何机制会主动知道
  有人卡住了。这个按钮就是那条出路：点一下，管理员后台多一条待处理记录。

  可以顺手写一句「要做什么」，方便管理员判断加多少。写不写都能提交。
-->
<template>
  <BaseDialog :show="show" :title="t('designkit.quota.title')" width="normal" @close="emit('close')">
    <div class="dk-stack dk-stack--sm">
      <p class="dk-text">{{ t('designkit.quota.body') }}</p>

      <textarea
        v-model="note"
        rows="3"
        class="dk-textarea"
        :maxlength="QUOTA_NOTE_MAX_LENGTH"
        :placeholder="t('designkit.quota.reasonPlaceholder')"
      ></textarea>
      <p class="dk-note dk-note--quiet dk-text-right">
        {{ note.length }} / {{ QUOTA_NOTE_MAX_LENGTH }}
      </p>

      <p v-if="doneMessage" class="dk-alert dk-alert--success">{{ doneMessage }}</p>
      <p v-if="errorMessage" class="dk-alert dk-alert--danger">{{ errorMessage }}</p>

      <p v-if="adminContact !== ''" class="dk-note">
        {{ t('designkit.quota.contact', { contact: adminContact }) }}
      </p>
    </div>

    <template #footer>
      <div class="dk-dialog-actions">
        <button type="button" class="dk-button dk-button--secondary" @click="emit('close')">
          {{ t('designkit.common.close') }}
        </button>
        <button
          v-if="doneMessage === ''"
          type="button"
          class="dk-button"
          :disabled="submitting"
          @click="submit"
        >
          {{ submitting ? t('designkit.common.loading') : t('designkit.quota.submit') }}
        </button>
      </div>
    </template>
  </BaseDialog>
</template>

<script setup lang="ts">
import { ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import BaseDialog from '@/components/common/BaseDialog.vue'
import { errorText, isCanceledError } from '../../api'
import { QUOTA_NOTE_MAX_LENGTH, createQuotaRequest } from './jobApi'

const props = defineProps<{
  show: boolean
}>()

const emit = defineEmits<{
  (e: 'close'): void
}>()

const { t } = useI18n()

const note = ref('')
const submitting = ref(false)
/** 后端拼好的「已经通知管理员了」整句中文；非空表示这次已经提交成功。 */
const doneMessage = ref('')
const errorMessage = ref('')
/** 管理员联系方式；后端没配就是空串，那一行就不显示。 */
const adminContact = ref('')

// 每次重新打开都是一次新的申请，把上次的结果清掉。
watch(
  () => props.show,
  (visible) => {
    if (visible) {
      submitting.value = false
      doneMessage.value = ''
      errorMessage.value = ''
    }
  },
)

async function submit(): Promise<void> {
  if (submitting.value) {
    return
  }
  submitting.value = true
  errorMessage.value = ''
  try {
    const result = await createQuotaRequest(note.value)
    // 后端那句话已经是完整中文（还会带上管理员联系方式），直接显示。
    doneMessage.value = result.message !== '' ? result.message : t('designkit.quota.done')
    adminContact.value = result.admin_contact ?? ''
  } catch (err) {
    if (isCanceledError(err)) {
      return
    }
    // 「已经提交过一次，管理员还没处理」也走这里——那不是故障，
    // 后端给的就是一句人话，原样显示即可。
    errorMessage.value = errorText(err)
  } finally {
    submitting.value = false
  }
}
</script>
