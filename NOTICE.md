# 来源与修改声明 / Attribution and Modifications

## 本项目基于 Sub2API 修改而来

This project is a modified version of **Sub2API**.

| | |
|---|---|
| 上游项目 / Upstream | https://github.com/Wei-Shaw/sub2api |
| 上游作者 / Original author | Wesley Liddick |
| 上游许可证 / Upstream license | GNU Lesser General Public License v3.0 or later |
| **起始基线 / Baseline** | **`v0.1.175`**（2026-08-12） |

上游完整的版权与许可条款保留在 [`LICENSE`](LICENSE)（LGPL v3）。
LGPL v3 的正文声明它以 GPL v3 为基础，因此本仓库另附 GPL v3 全文
[`COPYING`](COPYING)——上游仓库未附该文件，这里补齐。

The upstream copyright notice and license terms are preserved verbatim in
[`LICENSE`](LICENSE). Because LGPLv3 incorporates the terms of GPLv3 by
reference, the full GPLv3 text is included here as [`COPYING`](COPYING); the
upstream repository does not ship it.

## 本项目的许可证

**GNU Lesser General Public License v3.0 or later**（与上游一致）。

- 上游部分：Copyright (c) Wesley Liddick
- 本项目新增部分：Copyright (c) 2026 shinycheng

## 改了什么 / Statement of changes

在上游 Sub2API 的基础上，本项目加入了一套面向电商商品图的生成能力。
每次跟随上游版本升级后，**必须更新上面的「起始基线」和下面的清单**。

### 已完成

| 日期 | 改动 |
|---|---|
| 2026-08-12 | 建立基线 `v0.1.175`；补 `COPYING`（GPL v3 全文）与本文件；新增前端热更新开发编排 `deploy/docker-compose.designkit-dev.yml`；新增上游改动清点脚本 `designkit/bin/check-upstream-touch.sh` |
| 2026-08-13 | 生成工作台、灵感库（从 YouMind 开源库同步约 1.5 万条提示词）、批量出图、对外接口骨架；界面改为 designkit 外观并只保留白天模式；去掉上游的「部署与运营合规确认」关卡与新手引导 |
| 2026-08-14 | AI 挑提示词（读商品图 → 分类内粗筛 → 细选 5 条 → 合成 1 条）；「用这张继续生成」；出图比例改由提示词控制；内部 API Key 自动绑定分组；兑换码批量删除；登录页恢复平台标识旋转球 |
| 2026-08-15 | 全站深色单主题「DesignKit Dark」（含上游后台，调色板源头换肤）；品牌资产（logo 四方向/图标/插画）与产品端全套设计稿；官网三页（首页/申请试用/帮助中心，静态自包含 HTML，见 `designkit/website/`） |
| 2026-08-15 | 界面文案全量重写（「产品话」口径）；GitHub 独立化：README 换为本项目内容，删除上游品牌与文档文件 48 个（README_CN/JA、DEV_GUIDE、CLA、docs/ 除 legal、assets/、release.yml、cla.yml、GoReleaser 配置），界面站点名兜底与品牌字样改为 DesignKit，git 历史重置为单提交（见下）；应项目所有者要求（仅自用，不对外商业化分发），不再转录上游 README 的各项声明，仅保留本文件的来源声明、修改记录与许可证文件 |

### 本项目引入的第三方代码与素材

| 位置 | 内容 | 许可 |
|---|---|---|
| `frontend/src/features/designkit/vendor/tagcloud/` | TagCloud 2.5.0（登录页平台标识旋转球）。jsDelivr 打好的 ESM 包，**原样保存，未作改动** | MIT，全文见同目录 `LICENSE` |
| `frontend/public/designkit/platforms/*.svg` | 10 个电商平台标识 | Simple Icons，CC0-1.0，见同目录 `LICENSE-simple-icons.md` |

> ⚠ **京东 / 拼多多 / Amazon / Walmart 刻意不放图形标，用文字代替。**
> Simple Icons 按商标权利人要求下架了这几条；从维基百科热链既是外部请求，
> 收录许可也不都是自由许可（京东那张在英文维基是「合理使用」，本就不该随仓库分发）。
> **不要"顺手补齐"这四个图标。**

平台标识是各自权利人的商标，此处仅用于说明本项目适配的渠道，不表示任何关联或背书。
The platform marks are trademarks of their respective owners and are used here
only to indicate supported channels; no affiliation or endorsement is implied.

### 改动过的上游文件

本项目的代码优先放在自建目录中（`backend/internal/designkit/`、
`frontend/src/features/designkit/`、`designkit/`）。
截至 2026-08-15，相对基线的上游文件改动分三类
（完整清单随时可跑 `bash designkit/bin/check-upstream-touch.sh` 重新生成）：

**① 删除的上游文件（48 个，2026-08-15 GitHub 独立化）**：上游三份 README、
`DEV_GUIDE.md`、`CLA.md`、`docs/`（保留 `docs/legal/`，前端构建硬依赖）、
`assets/`（上游 logo 与赞助商标识）、`.github/workflows/release.yml` 与
`cla.yml`、GoReleaser 配置。上游 README 的重要提醒原文已移入本文件末尾保留。

**② 品牌字样替换（17 个文件，2026-08-15）**：站点名未配置时的兜底
「Sub2API」→「DesignKit」（前端 12 个文件、后端
`setting_features.go`/`content_moderation.go`/`config.go`/`main.go`/
`attestation.go`）；三处指向上游仓库的界面链接改指本仓库。
协议字段、模块路径、容器/数据库名等内部标识一律未动。

**③ 功能性修改（16 个文件，明细如下：6 个注册点 + 10 个其它）**：

**把新功能挂进去的注册点**（改动量刻意压到最小）

- `backend/internal/server/router.go` — 注册路由组（后端唯一的接入点）
- `frontend/src/router/index.ts` — 注册前端路由、默认落地页
- `frontend/src/components/layout/AppSidebar.vue` — 侧边栏菜单项与按角色过滤
- `frontend/src/i18n/locales/zh/index.ts`、`.../en/index.ts` — 挂载文案命名空间
- `.gitignore` — 末尾追加一个放行块。上游的 `scripts`、`tests`、`docs/*`、
  `CLAUDE.md` 等规则不带斜杠，会在任意层级命中同名目录并静默忽略本项目的文件

**其余改动**（9 个）

- `backend/internal/server/routes/admin.go`、`backend/internal/service/admin_compliance.go`、
  `backend/internal/service/admin_compliance_test.go`、
  `backend/internal/server/middleware/admin_compliance_test.go`、`frontend/src/App.vue`
  — 去掉「部署与运营合规确认」关卡
- `frontend/vite.config.ts` — 开发服务器的 `allowedHosts`（默认为空，即保持关闭）
- `frontend/tailwind.config.js` — 2026-08-15 全站深色单主题：dark 色阶换成
  DesignKit 深蓝、primary 由 teal 换成电光紫（调色板源头换肤，上游后台页
  编译期生效，替代大面积选择器覆盖）
- `frontend/src/views/admin/RedeemView.vue`、
  `frontend/src/i18n/locales/zh/admin/resources.ts`、`.../en/admin/resources.ts`
  — 兑换码批量删除的界面入口（后端接口上游本来就有）

> **⚠ 2026-08-14 更正：本节原来写着「除下列 6 个文件外不修改上游文件」，
> 并声称 `check-upstream-touch.sh` 会在 CI 中断言这一点。两句都不实**——
> 实际改动远多于 6 个文件，而该脚本自 2026-08-13 起**只清点、不拦截**（退出码恒 0），
> 对应的 CI job 是一个永远绿灯的空转。
> 保留这份清单的目的因此变成**如实披露**，而不是「白名单管控」。
> 跑 `bash designkit/bin/check-upstream-touch.sh` 可以随时重新清点。

## git 历史说明

本仓库的 git 历史于 2026-08-15 重置为单提交序列（以独立项目形态呈现）。
基于的上游版本与全部修改内容以本文件披露为准；LGPL 许可证不要求保留
版本控制历史，其要求的许可证文本、版权声明与修改声明均已保留。
上游提交历史的完整备份由项目所有者另行保存。
