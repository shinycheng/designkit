<template>
  <span v-if="version" class="text-xs text-gray-500 dark:text-dark-400"> v{{ version }} </span>
</template>

<script setup lang="ts">
// designkit（2026-08-24 拍板）：版本检查彻底关闭，徽标只显示当前版本号。
//
// 这里原来是管理员专属的「检查更新 / 在线升级 / 回滚」下拉面板。那套机制
// 查的是上游 Wei-Shaw/sub2api 的 release，对本仓库没有意义；在线升级和
// 回滚命令更会把系统整个换成上游版本。后端已在装配点关闭对 GitHub 的访问
// （backend/internal/service/wire.go 的 ProvideUpdateService），check-updates
// 接口恒返回「当前即最新」；这里相应删掉整个升级/回滚 UI，管理员和普通
// 用户看到的都是同一行静态版本号（来源是公开站点配置的 version 字段，
// 由 AppSidebar 传入，跟原来非管理员看到的完全一致）。
defineProps<{
  version?: string
}>()
</script>
