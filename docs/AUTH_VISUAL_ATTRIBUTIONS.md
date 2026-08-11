# 登录页视觉资源与组件来源

登录页只使用现成开源组件、开源动画资产与自由许可的品牌素材；不包含视频、AI 生成图或自行绘制的舞台动画。

## 零外部域名

**登录页（以及整个前端）不请求任何外部域名。** 所有第三方组件与素材都已经复制进
仓库，按本地路径加载。

为什么要这么做：公共 CDN（jsdelivr、unpkg 之类）一旦被投毒或被中间人替换，它送来的
JS 是以**我们自己站点的身份**执行的，等于全站 XSS——能把当时所有在线用户那份 7 天
有效的登录令牌整包读走。系统面向公网开放注册之后，这不是「概率低所以先放着」的问题。
附带好处：内网、离线、CDN 被墙的环境下登录页照样完整可用，也不会把访客 IP 泄露给第三方。

**改动这些文件时的规矩：**

- 不要把本地路径改回 `https://cdn...`，也不要新增 `<link rel="preconnect">`、
  外部字体、统计脚本。
- 升级某个组件＝重新下载对应版本**覆盖本地文件**，然后回来更新本文档里的版本号。
- 改完自查一遍：`grep -rn "https://" frontend/` 的结果里，应当只剩注释、
  文档链接和输入框 placeholder 示例，没有任何真会发出去的请求。

## 组件与动态素材

| 用途 | 仓库内位置 | 上游与版本 | 许可 |
|---|---|---|---|
| `<lord-icon>` Web Component | `frontend/vendor/lordicon/element.js` | [@lordicon/element 2.3.1](https://github.com/lordicondev/player-element) | MIT |
| 上一项依赖的 Lottie 播放器（内含 lottie-web） | `frontend/vendor/lordicon/player.js` | [@lordicon/web 1.2.1](https://github.com/lordicondev/player-web) | MIT |
| 金币动画 | `frontend/assets/lottie/coins.json` | [player-element 示例资产](https://github.com/lordicondev/player-element/tree/1d8ec10f991ba8c0bde0c9618e55092e22ee3fe0/examples/icons)，固定到提交 `1d8ec10f…` | MIT（仓库 LICENSE.md 覆盖全仓，含 examples） |
| 购物车形变动画 | `frontend/assets/lottie/morph-shopping.json` | 同上 | 同上 |
| 平台标识 3D 旋转 | `frontend/vendor/tagcloud/tagcloud.js` | [TagCloud 2.5.0](https://github.com/cong-min/TagCloud) | MIT |
| 界面图标子集 | `frontend/vendor/lucide/icons.js` | [Lucide 0.468.0](https://github.com/lucide-icons/lucide) | ISC |

许可全文分别放在 `frontend/vendor/lordicon/LICENSE`、`frontend/vendor/tagcloud/LICENSE`、
`frontend/vendor/lucide/LICENSE`。

两份 Lottie 均为纯矢量 Shape、无嵌入图片。页面只组合这些现成图标与 UI 组件，
不自行绘制图形，也不自己写舞台动画算法。

> `element.js` 相对上游改了**一处**：它原本 `import` 的是 jsDelivr 的绝对路径
> `/npm/@lordicon/web@1.2.1/+esm`，改成同目录的 `./player.js`。不改的话浏览器会拿这个
> 路径去请求**我们自己的域名**然后 404。文件头部的注释里也写了这件事。升级时记得重做这一步。

## 平台标识

逐条对应 `frontend/js/app.js` 的 `COMMERCE_CHANNELS`，分两类。

### 一、仓库内 SVG 文件（`frontend/assets/platforms/`）

| 平台 | 文件 | 来源与许可 |
|---|---|---|
| 淘宝 | `taobao.svg` | Simple Icons，CC0-1.0 |
| 小红书 | `xiaohongshu.svg` | Simple Icons，CC0-1.0 |
| 抖音电商 | `tiktok.svg` | Simple Icons 的 **TikTok** 标识，CC0-1.0 |
| TikTok Shop | `tiktok.svg`（与上一条共用） | 同上 |
| eBay | `ebay.svg` | Simple Icons，CC0-1.0 |
| Shopify | `shopify.svg` | Simple Icons，CC0-1.0 |
| Etsy | `etsy.svg` | Simple Icons，CC0-1.0 |
| AliExpress | `aliexpress.svg` | Simple Icons，CC0-1.0 |
| Shopee | `shopee.svg` | Simple Icons，CC0-1.0 |
| Rakuten | `rakuten.svg` | Simple Icons，CC0-1.0 |
| 天猫 | `tmall.svg` | Wikimedia Commons 的天猫文字标 |

Simple Icons 部分取自 [16.21.0](https://github.com/simple-icons/simple-icons/tree/16.21.0)，
其代码与图标集合按 CC0-1.0 发布；许可全文见 `frontend/assets/platforms/LICENSE-simple-icons.md`。

> 说明：抖音与 TikTok 同属字节跳动但为不同产品，二者标识并不相同。当前抖音条目复用了
> TikTok 标识作为占位，属已知不准确之处；若要对外展示，应替换为抖音自身标识或改用纯文字标签。

### 二、纯文字标（不使用图形）

京东（`JD.com`）、拼多多、Amazon、Walmart 四家在页面上是**文字**，不是图片。

原因有两条，缺一条也仍然成立：

1. **不外链。** 这四家原先是从维基百科 / 维基共享资源热链图片的，那是外部请求，
   与上面「零外部域名」冲突。
2. **收录许可不都是自由许可。** Simple Icons 没有收录这四家（该项目按商标权利人要求
   下架了这些条目），可选的来源里，京东那张在英文维基是以**合理使用（fair use）**收录的，
   **并非自由许可**，本就不该随仓库分发。与其分发来路不清的商标图形，不如写成文字。

文字标不涉及他人图形著作权；平台名称本身属于商标，使用见下方声明。

## 通用声明

各品牌商标权归相应权利人所有。页面文案使用"适配"表述；展示平台标识不表示 DesignKit
与相关平台存在合作、授权或背书关系。
