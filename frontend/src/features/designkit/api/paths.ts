/**
 * designkit 接口的公共前缀。
 *
 * 单独一个文件，是为了让 `index.ts` 和 `inspiration.ts` 都能拿到它**而不互相 import**
 * （index.ts 末尾 `export * from './inspiration'`，反过来再 import index 就是循环引用）。
 */

/**
 * 所有 designkit 端点的公共前缀，**相对 `/api/v1`**。
 *
 * axios 单例的 baseURL 已经是 `/api/v1`，所以这里只写 `/designkit`。
 * 写成 `/api/v1/designkit` 会变成 `/api/v1/api/v1/designkit`，直接 404。
 */
export const DESIGNKIT_API_BASE_PATH = '/designkit'
