/**
 * designkit 的 composable 统一出口。
 *
 * 页面里这样引：
 * ```ts
 * import { useJobPolling, useAuthedImages } from '@/features/designkit/composables'
 * ```
 */

export { useJobPolling } from './useJobPolling'
export type { JobProgress, UseJobPollingOptions, UseJobPollingReturn } from './useJobPolling'

export { useAuthedImages } from './useAuthedImage'
export type { UseAuthedImagesOptions, UseAuthedImagesReturn } from './useAuthedImage'
