# 登录页视觉资源与组件来源

登录页只使用现成开源组件、开源动画资产与品牌公开素材；不包含视频、AI 生成图或自行绘制的舞台动画。

## 组件与动态素材

- [Lordicon Element 2.3.1](https://github.com/lordicondev/player-element) 用于工作流 Lottie Web Component 播放，MIT License。
- 购物篮/购物车形变与金币动画使用 [Lordicon Player Element 官方示例资产](https://github.com/lordicondev/player-element/tree/1d8ec10f991ba8c0bde0c9618e55092e22ee3fe0/examples/icons)，并固定到提交 `1d8ec10f991ba8c0bde0c9618e55092e22ee3fe0`；两份 Lottie 均为纯矢量 Shape、无嵌入图片，按仓库 MIT License 使用。
- [TagCloud 2.5.0](https://github.com/cong-min/TagCloud) 用于平台 Logo 的现成 3D 旋转组件，MIT License。
- 工作流卡片图标来自项目内置的 [Lucide](https://github.com/lucide-icons/lucide) 图标子集（`frontend/vendor/lucide/`），ISC License。页面只组合这些现成图标与 UI 组件，不自行绘制图形或编写舞台动画算法。

## 平台标识

按加载方式分为三类，逐条对应 `frontend/js/app.js` 的 `COMMERCE_CHANNELS`：

### 一、仓库内本地文件（`frontend/assets/platforms/`）

| 平台 | 文件 | 来源与许可 |
|---|---|---|
| 淘宝 | `taobao.svg` | Simple Icons，CC0-1.0 |
| 小红书 | `xiaohongshu.svg` | Simple Icons，CC0-1.0 |
| 抖音电商 | `tiktok.svg` | Simple Icons 的 **TikTok** 标识，CC0-1.0 |
| 天猫 | `tmall.svg` | Wikimedia Commons 的天猫文字标 |

> 说明：抖音与 TikTok 同属字节跳动但为不同产品，二者标识并不相同。当前抖音条目复用了
> TikTok 标识作为占位，属已知不准确之处；若要对外展示，应替换为抖音自身标识或改用纯文字标签。

### 二、Simple Icons CDN（`cdn.jsdelivr.net/npm/simple-icons@16.21.0`）

eBay、Shopify、Etsy、AliExpress、Shopee、Rakuten、TikTok Shop。
[Simple Icons 16.21.0](https://github.com/simple-icons/simple-icons/tree/16.21.0) 代码与图标集合按 CC0-1.0 发布。

### 三、维基媒体公开媒体重定向

| 平台 | 来源 |
|---|---|
| Amazon | [Wikimedia Commons: Amazon 2024.svg](https://commons.wikimedia.org/wiki/File:Amazon_2024.svg) |
| Walmart | [Wikimedia Commons: Walmart logo (2025).svg](https://commons.wikimedia.org/wiki/File:Walmart_logo_(2025).svg) |
| 拼多多 | [Wikimedia Commons: Pinduoduologo.png](https://commons.wikimedia.org/wiki/File:Pinduoduologo.png) |
| 京东 | [Wikipedia: JD.com logo.png](https://en.wikipedia.org/wiki/File:JD.com_logo.png) — 该文件在英文维基以合理使用（fair use）收录，**并非自由许可**；仅作兼容性示意展示，商用前应替换为纯文字标签或取得授权。 |

## 通用声明

各品牌商标权归相应权利人所有。页面文案使用"适配"表述；展示平台标识不表示 DesignKit
与相关平台存在合作、授权或背书关系。
