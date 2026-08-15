# 调研：GitHub 电商 AI 开源项目——哪些值得并入 designkit

> 2026-08-16。monica 提出「看看 GitHub 上好的电商 AI 项目，可以合并进来」。
> 五个方向并行调研，每个项目的 star / 许可证 / 硬件要求都经 GitHub 页面实查。
> 评估前提：群晖 NAS **没有显卡（CPU-only）**、28GB 内存；项目 LGPL-3.0；
> 出图走 ChatGPT 云端。**「运营点按钮要几分钟出不来 = 不可用」是硬标准。**

## 总结论（决策用）

### 一档：建议做（许可证干净、CPU 真能跑、运营价值直接）

| # | 功能 | 用什么 | 许可证 | NAS 上速度 | 工作量 |
|---|---|---|---|---|---|
| 1 | **一键抠图 / 白底主图** | rembg（24.3k★，活跃）独立容器，默认 isnet 模型 | 代码 MIT，权重 Apache ✅ | 单张 3~10 秒 | 2~3 天 |
| 2 | **高清放大（低清图救活 / 出图放大到 2K-4K）** | Real-ESRGAN 小模型 + onnxruntime，并入现有 imgsvc | BSD ✅ | 单张约 1~1.5 分钟（异步） | 3~6 天 |
| 3 | **违禁词 + 标题规则检查** | sensitive-word 词库（Apache ✅）+ Go 自写匹配 + 广告法极限词表 + 各平台标题字数表 | 全绿 ✅ | 毫秒级 | 2~3 天 |

### 二档：先不做，条件触发再启动

- **商品图重打光**（IC-Light 那种「只改光不改物」）：本地必须 GPU。真需要时走
  Replicate 托管 API（~$0.015/次，3~5 天接入）。触发条件：运营实测反馈
  gpt-image-2 的打光效果不够用。
- **虚拟试穿 / AI 模特图**：开源四家**权重全是非商用许可证（NC），跟有没有显卡无关，
  直接出局**。第一步先在灵感库加 3~5 条「模特上身」提示词模板（半天）实测
  gpt-image-2 的效果；不够用再接 fal.ai 的 Kolors API（$0.07/张，2~3 天）。

### 三档：明确不引入（记下原因，防止以后重复调研）

| 项目 | 否决原因 |
|---|---|
| BRIA RMBG-1.4/2.0 | 权重 CC BY-NC，公司内部生产也算商用，用了即违约。rembg 里能选到 `bria-rmbg`，**部署文档已标禁用** |
| ComfyUI / Fooocus | GPL-3.0 + 实质需要 GPU + 界面没法给运营用；Fooocus 已冻结 |
| Upscayl | AGPL + 硬性要求 Vulkan 显卡 |
| SwinIR | CPU 上小时级；无 CPU 优化版 |
| chaiNNer | GPL 桌面应用，形态不匹配 |
| EcomGPT | 2023 论文仓已死 + 7B 模型要 GPU + 许可证不明 |
| wordscheck | 名义开源实际只有二进制、无许可证声明 |
| Enthusiast | MIT 且活跃，但与已有 AI 对话页重叠大，还要多养一套 Django+Postgres；只抄它的「目录批量丰富」工作流思路即可 |
| IDM-VTON / OOTDiffusion / CatVTON | 权重全是 CC BY-NC-SA（非商用）+ 需要 GPU |

### 一个佐证

InvokeAI（27.9k★，Apache）官方对「没有显卡怎么办」的答案就是：
**「接 GPT Image 这类云端 API」**——正是 designkit 现在的架构。
方向上我们没有走偏，缺的只是围绕它的周边能力（抠图、放大、规则检查）。

---

以下为五个方向的完整调研原文。



---

# 方向 A：抠图 / 去背景 / 白底图

# 调研 A：抠图 / 去背景 / 白底图（CPU-only 群晖，LGPL-3.0 项目，内部商用）

## 结论先行

**首选 rembg（MIT，官方 Docker 镜像自带 HTTP server，CPU 单张 3~10 秒）作为独立容器接入；模型选 `isnet-general-use` 或 `u2net`。⛔ 避开 BRIA RMBG 系列——权重许可证是非商用（NC），公司内部商用也不行。**

---

## 逐项评估

### 1. rembg ★推荐

| 项 | 内容 |
|---|---|
| 地址 / star / 活跃 | github.com/danielgatis/rembg，**24.3k star**，最近提交 **2026-08-06**（实查 commits 页），非常活跃 |
| 许可证 | 代码 **MIT** ✅。内置模型分开看：u2net/u2netp/isnet 权重 Apache-2.0 ✅，birefnet 系列权重 MIT ✅；⚠ 它也提供 `bria-rmbg` session——**那个模型权重是非商用的，别选它**（见第 3 条） |
| 解决什么问题 | 商品图一键去背景 → 透明 PNG，配合 imgsvc 已有的「透明合白」直接得到白底主图 |
| CPU-only 可行性 | **可行，这是它的主场**。走 onnxruntime，官方提供 `pip install "rembg[cpu]"` 和 CPU 版 Docker 镜像。u2net/isnet 的 onnx 权重各约 176MB（u2netp 仅 ~4.5MB），内存占用 1~2GB/次，28GB 内存毫无压力。社区实测 CPU 单张 **3~10 秒**（取决于分辨率），x86 NAS 落在这个区间 |
| 怎么并入 | **独立容器最省事**：官方镜像自带 `rembg s` HTTP server，Go 后端直接 POST 图片。也可作为库并进 imgsvc，但它要求 **Python ≥3.11 <3.14**，要先核对 imgsvc 的 Python 版本；独立容器可绕开这个约束 |
| 工作量 | **2~3 天**：容器编排 + Go 侧调用 + 前端「生成白底图」按钮 + 文档。若只做 API 不做界面约 1 天 |

### 2. BiRefNet

| 项 | 内容 |
|---|---|
| 地址 / star / 活跃 | github.com/ZhengPeng7/BiRefNet，**~4k star**，2025 下半年仍有更新，活跃 |
| 许可证 | 代码和**原版权重都是 MIT** ✅（注意：BRIA RMBG-2.0 用的是同一架构但换了私有数据训练，那份权重是 NC——两者别混为一谈） |
| 解决什么问题 | 目前开源里发丝级/复杂边缘抠图质量最好的一档，对毛绒、镂空、细网纹商品效果明显好于 u2net |
| CPU-only 可行性 | **勉强**。README 全按 GPU 写（FP32 推理需 4.8GB 显存，A100 上 87ms/张）；完整版 onnx 权重约 900MB+，CPU 单张估计 **30 秒~2 分钟**，仅 lite/tiny 变体（~200MB）能压到 ~10 秒级。28GB 内存装得下，但慢 |
| 怎么并入 | **不要直接集成**——rembg 已内置 `birefnet-general` / `birefnet-general-lite` session，想用它就在 rembg 里换个模型名，零额外代码 |
| 工作量 | 经 rembg 用：**0 天**（改一个配置值）。直接集成 PyTorch 版：3~5 天，不值 |

### 3. BRIA RMBG-1.4 / RMBG-2.0 ⛔ 许可证红灯

| 项 | 内容 |
|---|---|
| 地址 / 热度 | huggingface.co/briaai/RMBG-2.0（月下载 65 万+，质量口碑确实好） |
| 许可证 | 🔴 **权重是 CC BY-NC 4.0（2.0）/ bria-rmbg-1.4（1.4），均仅限非商用；商用必须跟 BRIA 签付费协议**（HF 页面原文实查）。designkit 是公司内部生产用途 = 商用，**不适用**。注意这不是 GPL 式传染问题，是使用权限制——比传染更直接：用了就是违约。rembg 里能选到 `bria-rmbg`，部署文档里要明确写「禁止选这个模型」 |
| CPU-only | 1.4（44M 参数，IS-Net 底子）CPU 能跑；2.0（0.2B 参数，BiRefNet 底子）CPU 很慢。但许可证已一票否决，不展开 |
| 结论 | **不用。** |

### 4. backgroundremover（nadermx）

| 项 | 内容 |
|---|---|
| 地址 / star / 活跃 | github.com/nadermx/backgroundremover，**~8k star**，维护中但节奏一般 |
| 许可证 | 代码 MIT ✅，模型 Apache-2.0 ✅ |
| 解决什么问题 | 与 rembg 同类（同样是 u2net 系），额外多一个**视频去背景**能力 |
| CPU-only 可行性 | 可以（默认 CPU，自动检测 GPU），但走 **PyTorch 全家桶**，镜像和内存都比 rembg 的 onnxruntime 方案重；模型还是 u2net 那几个，质量无增益 |
| 怎么并入 | 独立容器或并入 imgsvc（Python ≥3.6，版本约束松） |
| 工作量 | 2~3 天。但相对 rembg 没有优势（视频功能 designkit 用不上），**不选** |

### 5. transparent-background（InSPyReNet）

| 项 | 内容 |
|---|---|
| 地址 / star / 活跃 | github.com/plemeri/transparent-background，**1.3k star**，近期更新停在 2024 年前后，**维护偏冷** |
| 许可证 | MIT ✅ |
| 解决什么问题 | 高分辨率抠图（ACCV 2022 的 InSPyReNet），细节介于 u2net 和 BiRefNet 之间 |
| CPU-only 可行性 | 可以（README 有专门的 CPU-only 安装指引，且有 `fast` 模式），但依赖 PyTorch，base 权重约 350~400MB，CPU 单张估计 10~30 秒 |
| 怎么并入 | pip 库并入 imgsvc 或独立容器 |
| 工作量 | 2~3 天。质量/速度/活跃度都不如「rembg + 可切 birefnet-lite」组合，**不选** |

---

## 给 designkit 的落地建议

1. **形态**：独立容器跑官方 `danielgatis/rembg` CPU 镜像（`rembg s` HTTP 模式），与 imgsvc 平级；Go 后端加一个「去背景」步骤调它，返回的透明 PNG 交给 imgsvc 现有的「透明合白」出白底图。避免把 rembg 塞进 imgsvc——rembg 要求 Python ≥3.11 <3.14，会跟 imgsvc 现有环境耦合。
2. **模型档位**：默认 `isnet-general-use`（约 176MB，CPU 3~10 秒/张，商品图口碑好于 u2net）；给「精细模式」留 `birefnet-general-lite`（更慢但边缘更好）；低配兜底 `u2netp`（4.5MB，最快最糙）。全部 MIT/Apache 权重，与 LGPL-3.0 项目并存无传染问题。
3. **红线写进文档**：rembg 的模型列表里 `bria-rmbg` 一律禁用（权重非商用）；群晖无 GPU，`birefnet` 完整版（非 lite）也别开放给运营（单张分钟级，体验是「卡死了」）。
4. **总工作量**：容器 + 后端接口 + 前端按钮 + 契约测试 + 文档 ≈ **2~3 天**。

数据来源：github.com/danielgatis/rembg（README、commits、releases 实查）、github.com/ZhengPeng7/BiRefNet、huggingface.co/briaai/RMBG-2.0、huggingface.co/briaai/RMBG-1.4、github.com/nadermx/backgroundremover、github.com/plemeri/transparent-background；CPU 耗时区间来自社区实测口碑（dev.to/medium 的对比评测及 rembg issue 区），部署前应在群晖上用真实商品图复测。

---

# 方向 B：画质增强 / 放大

# 调研 B：画质增强 / 放大 / 清晰化（CPU-only 群晖 NAS）

## 结论速览

| 项目 | 许可证 | CPU-only NAS 可行性 | 结论 |
|---|---|---|---|
| **Real-ESRGAN**（用小模型 + ONNX） | BSD-3-Clause ✅ | ✅ 1024px 约 1~2 分钟 | **推荐，唯一可行路线** |
| Real-ESRGAN 大模型 x4plus | BSD-3-Clause ✅ | ❌ 20~30 分钟/张 + 内存爆 | 排除 |
| Upscayl | **AGPL-3.0 ⚠️红** | ❌ 硬性要求 Vulkan GPU | 排除 |
| SwinIR | Apache-2.0 ✅ | ❌ 小时级/张，需 GPU | 排除 |
| chaiNNer | **GPL-3.0 ⚠️红** | ⚠️ 能跑但是 Electron 桌面应用 | 不集成 |

## 实测基准（本机 Apple M4 10 核，PyTorch CPU fp32，架构与官方 basicsr 一致）

| 模型架构 | 参数量 | 输入→输出 | 实测耗时 |
|---|---|---|---|
| SRVGGNetCompact（= realesr-general-x4v3） | 1.21M | 512→2048 | **2.6 秒** |
| SRVGGNetCompact | 1.21M | 1024→4096 | **11.2 秒** |
| RRDBNet（= RealESRGAN_x4plus） | 16.7M | 512→2048 | **56.5 秒** |
| RRDBNet | 16.7M | 1024→4096 全图 | **进程被系统杀掉**（峰值内存 >13GB）；必须切 512 tile，按 4 块折算 ≈ 3.8 分钟 |

**换算到群晖**：28GB x86_64 无显卡的群晖（Ryzen V1500B/R1600 档，多核性能约为 M4 的 1/5~1/7，估算值）：
- 小模型 1024px：**约 1~1.5 分钟/张** —— 可接受（对比：AI 推荐提示词本来就要等 1~2 分钟）
- 大模型 1024px（切块后）：**约 20~30 分钟/张** —— 不可用

---

## 1. Real-ESRGAN

1. **地址/热度/活跃**：github.com/xinntao/Real-ESRGAN，36.5k stars；最后 push 2024-08，最后 release v0.3.0 是 2022-09 → **维护已停滞，但模型成熟稳定、社区生态最大**（Upscayl/chaiNNer 底层用的都是它的模型）。
2. **许可证**：BSD-3-Clause ✅（配套 ncnn 版仓库是 MIT ✅；模型权重随 BSD 仓库发布）。
3. **解决什么**：供应商给的糊图/小图放大 4 倍并去噪去压缩伪影，救活当主图；gpt-image-2 出的 1024px 图放大到 2K/4K 印 banner/详情页。
4. **CPU-only 能不能跑**：**能，但只有小模型能跑**。
   - `realesr-general-x4v3`（小模型，**4.9MB**）：见上表，NAS 约 1~1.5 分钟/张，内存 1~2GB ✅。质量比大模型略低，商品图场景够用。
   - `RealESRGAN_x4plus`（大模型，64MB）：NAS 上 20~30 分钟/张，且 1024px 全图推理需 >13GB 内存（实测在 M4 Mac 上直接被杀），必须切 tile ❌。
   - 坑①：PyTorch 版 CPU 推理**必须加 `--fp32`**（fp16 算子 CPU 没实现，官方 FAQ 明写）。
   - 坑②：官方 ncnn 版（Real-ESRGAN-ncnn-vulkan，MIT，2.2k stars）主打 Vulkan GPU；`-g -1` 有 CPU 模式但**处理线程只有 1 条**，比 onnxruntime 慢，不走这条路。
   - **ONNX CPU 优化版：有**。官方带 pytorch2onnx 脚本，社区有现成 `realesr-general-x4v3.onnx`（4.87MB，Hugging Face 多处托管）；用 onnxruntime（MIT ✅）跑，CPU 多线程正常吃满。
5. **怎么并入**：加进现有 Python imgsvc（onnxruntime + numpy，无 torch 依赖，镜像只增 ~50MB）。新增一个"高清放大"接口，切 512 tile + 8px 重叠推理控内存；走异步任务队列出结果（1 分钟级不能同步等）。
6. **工作量**：imgsvc 侧 2~4 天（tile 推理 + 队列 + selfcheck + 单测）；Go 转发 + 前端"高清放大"按钮 + 文案 1~2 天。**合计约 3~6 天**。

## 2. Upscayl

1. github.com/upscayl/upscayl，48.3k stars；活跃（push 2026-08，release v2.15.0 2024-12）。
2. **AGPL-3.0 ⚠️标红**（CLI 版 upscayl-ncnn 同样 AGPL-3.0）。
3. 给普通人一键放大图片的桌面软件。
4. **CPU-only 跑不了，官方 FAQ 原话**："NCNN Vulkan requires a Vulkan-compatible GPU. Upscayl won't work with **most** iGPUs or CPUs." 硬性要求 Vulkan 显卡。
5/6. 不适用。它本质 = Real-ESRGAN 模型 + ncnn-vulkan 的 Electron 壳，模型我们直接从上游拿，无需碰 AGPL 代码。

## 3. SwinIR

1. github.com/JingyunLiang/SwinIR，5.6k stars；研究代码，实质更新停在 2022（最后 push 2024-05 为小修）。
2. Apache-2.0 ✅。
3. 论文级画质（细节恢复优于 Real-ESRGAN），但没有产品化。
4. **CPU-only 不可行**：Transformer 注意力对 CPU 极不友好，比 RRDB 还慢数倍——RRDB 在 M4 上 512px 已要 57 秒，SwinIR 在 NAS 上 1024px 是**小时级**。官方只给 GPU 用法（README 全部按 RTX 2080Ti 报数），**无官方 ncnn/onnx CPU 优化版**。
5/6. 不适用。
1. github.com/chaiNNer-org/chaiNNer，6.0k stars；活跃（push 2026-07，v0.25.1 2025-10）。

## 4. chaiNNer

2. **GPL-3.0 ⚠️标红**。
3. 节点式图像处理桌面软件，把"放大→修脸→压缩"串成流水线批量跑。
4. CPU 能跑（官方明确 PyTorch/NCNN/ONNX 均有 CPU fallback），但跑的还是上面同一批模型，速度同上表。
5. **不并入**：Electron 桌面 GUI + 自带 Python 环境，虽有 `chainner run xx.chn` 命令行模式（PR #1489），仍是完整桌面应用，塞进 NAS 容器不现实；GPL 也有传染顾虑。定位只能是"管理员在自己电脑上手动批处理"，与"运营在网页上点按钮"不匹配。
6. 0（不集成）。

---

## 给决策的两句话

- **可行解只有一个**：`realesr-general-x4v3`（BSD）+ onnxruntime（MIT）进 imgsvc，NAS 上约 1~1.5 分钟/张，异步队列交付。许可证全绿，不新增容器。
- **管理预期**：CPU-only 下"论文级最高画质"（大模型/SwinIR）确实做不到，需 GPU；小模型对商品图（放大+去糊）效果够用，但别按"无损神图"宣传。

## 来源

- [Real-ESRGAN](https://github.com/xinntao/Real-ESRGAN) / [FAQ（--fp32）](https://github.com/xinntao/Real-ESRGAN/blob/master/docs/FAQ.md) / [issue #921 CPU 单核](https://github.com/xinntao/Real-ESRGAN/issues/921) / [issue #22 CPU 慢](https://github.com/xinntao/Real-ESRGAN/issues/22)
- [Real-ESRGAN-ncnn-vulkan](https://github.com/xinntao/Real-ESRGAN-ncnn-vulkan)（MIT，`-g -1`=CPU 单线程）
- [Upscayl](https://github.com/upscayl/upscayl) / [upscayl-ncnn](https://github.com/upscayl/upscayl-ncnn)（AGPL、需 Vulkan GPU）
- [SwinIR](https://github.com/JingyunLiang/SwinIR)
- [chaiNNer](https://github.com/chaiNNer-org/chaiNNer) / [CLI 模式 PR #1489](https://github.com/chaiNNer-org/chaiNNer/pull/1489)
- [realesr-general-x4v3.onnx（4.87MB）](https://huggingface.co/OwlMaster/AllFilesRope/blob/main/realesr-general-x4v3.onnx) / [OpenModelDB 条目](https://openmodeldb.info/models/4x-realesr-general-x4v3)
- star 数/许可证/push 时间均经 GitHub API 实查（2026-08-16）；耗时为本机实测（脚本：`/private/tmp/claude-501/-Users-monica-Desktop-designkit/45faedb6-4c97-4759-8d0b-49008f056743/scratchpad/bench.py`）

---

# 方向 C：打光 / 场景合成 / 工作流

# 调研 C：商品图打光 / 场景合成 / 图片编辑工作流

## 先回答核心问题

**是的，这一类本地方案实际上全都要 GPU。** 四个候选全是 Stable Diffusion 系（SD1.5 / SDXL / Flux）推理工具，其中两个（ComfyUI、Fooocus）名义上有 CPU 模式，但在群晖那颗低功耗 x86 CPU 上是「每张几十分钟到小时级」的量级，对运营是不可用的。Fooocus 自己的 README 写明 CPU 模式要 32GB 内存（NAS 只有 28GB，低于门槛）且比 RTX 3060 慢约 17 倍。**不存在能在这台 NAS 上实用化的本地方案，不必硬找。**

**云端替代路是存在的，而且一半已经在手上**（见文末）。

---

## 逐项评估

### 1. IC-Light（打光/重打光专门模型）

| 项 | 结论 |
|---|---|
| 地址 / star | github.com/lllyasviel/IC-Light，8.5k star |
| 活跃度 | **半休眠**：最后一次提交 2025-02-20，且近一年提交全是改 README |
| 许可证 | Apache-2.0 ✅（代码；SD1.5 底模另有其自身许可） |
| 解决什么 | 给商品/人物照片**重新打光**：文字描述光源方向和氛围（"左侧柔光""夕阳背光"），生成打光后的图，商品轮廓保持较好——正是"商品图打光"这个词的本尊 |
| CPU-only 能跑吗 | **需要 GPU。** README 只给 CUDA 安装路径，无 CPU 支持说明。SD1.5 底模 ~2GB + IC-Light 权重 ~1.7GB，理论上塞得进 28GB 内存，但 NAS 级 CPU 推理一张 512px 估计 10~30 分钟起（估算值），不可用 |
| 怎么并入 | 唯一现实路径是**调外部 API**：Replicate 上有现成托管（`zsxkib/ic-light`，实查 ~$0.015/次、约 16 秒出图，另有 background 版）。imgsvc 或 Go worker 加一个 HTTP 调用 |
| 工作量 | 走 Replicate：3~5 天（含前端入口、计费记账、失败重试）。⚠ 代价：引入第二个付费外部服务 + 图片出境到 Replicate，与现在「ChatGPT Plus 包月一个口」的模式相悖 |

### 2. ComfyUI（节点式工作流引擎）

| 项 | 结论 |
|---|---|
| 地址 / star | github.com/comfyanonymous/ComfyUI，127.8k star |
| 活跃度 | 非常活跃：每周发版节奏 |
| 许可证 | **🔴 GPL-3.0**。并进 LGPL-3.0 代码库有传染问题；作为独立容器走 HTTP 调用属于聚合、不传染，且内部使用不构成分发，法律上能绕开——但引入即多一条要向后来者解释的合规线 |
| 解决什么 | 电商向 workflow（换背景、重打光、放模特手里）在其生态里最全，理论上「一个引擎跑所有电商工作流」 |
| CPU-only 能跑吗 | 有 `--cpu` 参数，**但实际跑的还是 SD/Flux 模型，NAS CPU 上同样几十分钟一张**。`--cpu` 是给开发调试用的，不是生产方案。**实质需要 GPU** |
| 怎么并入 | 独立容器 + 另购 GPU 主机才成立；且节点图对非技术运营完全不可暴露，只能后端封装成固定 workflow |
| 工作量 | 不含买 GPU 机器：7~10 天。**当前部署环境下不成立** |

### 3. Fooocus（傻瓜化 SDXL 出图）

| 项 | 结论 |
|---|---|
| 地址 / star | github.com/lllyasviel/Fooocus，52.3k star |
| 活跃度 | **冻结**：官方声明进入 LTS「只修 bug」，作者最后实质提交 2024-08，2025-09 那次只是 dependabot 升 CI 依赖 |
| 许可证 | **🔴 GPL-3.0**（同 ComfyUI 的合规说明） |
| 解决什么 | 不会写提示词也能出好图——但这个价值点跟本项目重合（我们已有灵感库 + AI 配提示词） |
| CPU-only 能跑吗 | README 明确写 CPU 模式：**最低 32GB 内存**（NAS 28GB 不达标）、比 RTX 3060 **慢 ~17 倍**。实质需要 GPU |
| 怎么并入 | 是 Gradio 桌面向应用、非 API-first，封装成服务要自己扒。且 SDXL 底座已被作者明言不再升级 |
| 工作量 | 不评估。**冻结项目 + GPL + 硬件不达标，三重排除** |

### 4. InvokeAI（专业创作画布）

| 项 | 结论 |
|---|---|
| 地址 / star | github.com/invoke-ai/InvokeAI，27.9k star |
| 活跃度 | 活跃：持续开发中，1.9 万+ 提交 |
| 许可证 | Apache-2.0 ✅ |
| 解决什么 | 带图层/局部重绘的统一画布，设计师手工精修商品图用 |
| CPU-only 能跑吗 | 官方要求表：SD1.5 最低 4GB VRAM、SDXL 8GB、Flux 10GB。官方原话：**"Local generation is slow without a GPU, but API-backed models (e.g. GPT Image, Gemini) work well"** —— 即本地生成实质需要 GPU；它自己的 CPU 出路也是接云端 API |
| 怎么并入 | 并不进：它是完整的另一个创作应用（自带账号、库、画布），不是可嵌组件。给非技术运营用等于再引入一套要学的软件 |
| 工作量 | 不建议并入。它的价值是佐证：**行业头部工具在无 GPU 场景下的官方答案就是「转发云端 API（GPT Image）」**——恰好是本项目已有的架构 |

---

## 云端 API 替代路径（结论）

1. **场景合成：gpt-image-2 已经覆盖，不需要引入任何新东西。** 「商品图 + 提示词 → 放进新场景/新背景/新光线」正是现有出图链路每天在干的事，灵感库里电商主图模板本质就是场景合成提示词。已知短板要如实认：它是**整图重绘**，商品细节（logo 文字、纹理）不保证像素级还原，也没有蒙版级控制——这是模型能力边界，换本地方案（同为扩散模型重绘）也一样有。
2. **专门的"只改光不改物"重打光**：gpt-image-2 用提示词能做个大概，要 IC-Light 那种效果，现实选项是 **Replicate 托管的 IC-Light（~$0.015/次、16 秒）**。可做但建议**默认不做**：多一个付费口、多一条图片出境路径、多一套计费对账，换来的能力与 gpt-image-2 重叠度不低。等运营真实反馈「gpt-image-2 打光不够用」再启动，届时 3~5 天可接上（挂进 imgsvc 或 Go worker 均可，推荐 Go worker——与现有 job/计费/存储流水线同层）。
3. **不要为这个方向购置 GPU 或引入 ComfyUI/Fooocus**：GPL 合规成本 + 硬件成本 + 运营不可用的界面，三头亏。

---

# 方向 D：电商文案 / listing 工具

# 调研方向 D 结论：电商文案 / listing / 标题优化开源工具

**先说总体结论**：这个品类里「结构化模板 + 平台规则 + 批量处理」三样俱全的成型开源产品**基本不存在**——该市场被商业 SaaS 占据（SuperListing、ListingForge、KwickMetrics 等，全闭源）。开源侧实际存在的只有三类：① 违禁词/合规检测引擎（成熟、活跃）；② prompt/skill 包（按要求排除）；③ 绑死特定电商平台的插件（Magento/WooCommerce，无法并入）。**方向 D 值得引入的是「平台规则」这一半，「文案生成」那一半用现有 gpt-5.6-sol 自建更划算。**

## 值得报的项目

### 1. sensitive-word（违禁词检测引擎）
| 项 | 内容 |
|---|---|
| 地址 / star / 活跃度 | [github.com/houbb/sensitive-word](https://github.com/houbb/sensitive-word)，**6.0k star**，持续维护中（当前 v0.29.5，仍在发版） |
| 许可证 | **Apache-2.0**（可并入 LGPL 项目，无传染问题） |
| 解决什么 | 运营写的标题/文案发布前自动标出违禁词、脏词、违法词——这正是通用对话做不到的「平台规则」硬检查（LLM 判违禁词会漏、会编） |
| CPU-only 能跑吗 | **能，且极轻**。DFA 算法纯文本匹配，毫秒级，无模型无 GPU。Java 进程约 200~300MB 内存；只取词库自实现则 <50MB |
| 怎么并入 | 两条路：**A（推荐）**只取它的 6 万条词库文件（Apache-2.0 txt），在 Go 后端用 Aho-Corasick 自写匹配（Go 有现成库），不引入 Java；**B** 整库跑 Java sidecar 容器暴露 HTTP |
| 工作量 | 路 A 约 **1~2 天**（含前端标红展示）；路 B 约 2~3 天 |
| 缺口提醒 | 内置词库偏涉政/色情/赌毒分级，**广告法极限词（「最高级」「第一」「国家级」）不全**，需从 [advertising_law_checker](https://github.com/521xueweihan/advertising_law_checker)（MIT，14 star，基本不更新，仅当词表来源用）等仓库补一份静态词表，几百词，半天活 |

### 2. Enthusiast（电商 AI agent 平台，目录批量丰富）
| 项 | 内容 |
|---|---|
| 地址 / star / 活跃度 | [github.com/upsidelab/enthusiast](https://github.com/upsidelab/enthusiast)，**165 star**，活跃（最近提交 2026-06-23，1.7.0 版，496 commits） |
| 许可证 | **MIT** |
| 解决什么 | 把非结构化商品资料（表格/文档）批量抽取成结构化描述、属性、翻译——「目录批量丰富」工作流，带 RAG 和向量检索，不是单条对话 |
| CPU-only 能跑吗 | **能**。它自身不跑模型，调 OpenAI 兼容 API（也可指向自托管 LLM——NAS 上别选这个）。平台本体 Django+PostgreSQL+React，CPU 足够 |
| 怎么并入 | 独立容器组（自带 docker-compose），不进现有代码。⚠ 与 designkit 已有的 gpt-5.6-sol 对话页功能重叠不小，且要多养一套 Django+Postgres |
| 工作量 | 独立部署试用 **1 天**；真要和 designkit 账号/计费打通 **5 天以上**。更现实的用法是**抄它的目录丰富工作流设计**，在 designkit 内自建 |

### 3. wordscheck（开箱即用违禁词服务）⚠ 红旗
| 项 | 内容 |
|---|---|
| 地址 / star | [github.com/bosnzt/wordscheck](https://github.com/bosnzt/wordscheck)，600 star |
| 许可证 | **🚩 未声明许可证，且仓库只有预编译二进制、无源码**——名义开源实际闭源。默认版权保留，并入 LGPL 项目有法律风险 |
| 解决什么 | docker 一条命令起一个违禁词检测 HTTP JSON API，~100MB 内存，CPU 毫秒级 |
| 怎么并入 / 工作量 | 独立容器，0.5 天就通。**但因许可证问题不建议采用**，列出仅供对比——它证明了「违禁词 HTTP 小服务」这个形态半天能自建 |

### 4. EcomGPT（不推荐，如实报）
| 项 | 内容 |
|---|---|
| 地址 / star / 活跃度 | [github.com/Alibaba-NLP/EcomGPT](https://github.com/Alibaba-NLP/EcomGPT)，277 star，**已停更**（仅 12 commits，2023 年论文配套仓库） |
| 许可证 | **🚩 未标注**（模型权重在 ModelScope，条款另算） |
| 解决什么 | 电商任务微调过的 7B 模型（BLOOMZ 底座），电商 NER/分类/摘要比通用模型强 |
| CPU-only 能跑吗 | **实际跑不动**：7B 模型在无显卡 NAS 上只能量化后跑，单条生成分钟级，28GB 内存勉强够但体验不可用。**需要 GPU** |
| 结论 | 项目已死 + 需要 GPU + 许可证不明，三条各自都足以否掉 |

## 已核实并排除的（防止重复调研）

| 项目 | 排除原因 |
|---|---|
| [nexscope-ai/eCommerce-Skills](https://github.com/nexscope-ai/eCommerce-Skills)（690 star，MIT） | 157 个 markdown skill，纯 prompt 包装，无平台规则数据、无批量代码 |
| [zhouzilai626/ecommerce-detail-page-generator](https://github.com/zhouzilai626/ecommerce-detail-page-generator)（18 star） | 纯 prompt/skill 模板库，无代码；虽有平台适配文档但无具体字数/违禁词规则 |
| [Nutlope/description-generator](https://github.com/Nutlope/description-generator) | Together AI 演示 demo，纯 prompt 包装 |
| [BWB03/helm-amazon-title-workflow](https://github.com/BWB03/helm-amazon-title-workflow)（3 star） | 太新太小，本质是 prompt 工作流 + 一个校验脚本；其处理的 Amazon 75 字符新规（2026-07-27 生效）可作规则表输入，项目本身不必引 |
| Magento 系（[creatuity](https://github.com/creatuity/magento2-openai-content-generator) / [mage-os-lab](https://github.com/mage-os-lab/module-catalog-data-ai) / [mageprince](https://github.com/mageprince/magento2-mage-ai)） | 有批量 CLI/cron，但全是 Magento PHP 插件，脱离 Magento 无法运行 |
| [konsheng/Sensitive-lexicon](https://github.com/konsheng/Sensitive-lexicon) 等词库仓库 | 偏涉政/色情审核词表，非电商极限词；可作补充词源但非工具 |

## 给 designkit 的落地建议（最小组合）

1. **词库 + Go 侧 Aho-Corasick 自建违禁词检查**（sensitive-word 词库 Apache-2.0 + 广告法极限词静态表）——约 2 天，覆盖「违禁词」规则。
2. **标题字数规则做成一张静态配置表**（淘宝 30 汉字 / 京东 45 / Amazon 75 字符等），前端实时字数条 + 提交校验——1 天内，不需要任何外部项目。
3. 文案生成本身沿用现有 gpt-5.6-sol 通道 + 复用 designkit 已有的批量任务基建（决策 12 的批量框架），比引入 Enthusiast 整套平台便宜一个数量级。

Sources: [houbb/sensitive-word](https://github.com/houbb/sensitive-word) · [upsidelab/enthusiast](https://github.com/upsidelab/enthusiast) · [bosnzt/wordscheck](https://github.com/bosnzt/wordscheck) · [Alibaba-NLP/EcomGPT](https://github.com/Alibaba-NLP/EcomGPT) · [521xueweihan/advertising_law_checker](https://github.com/521xueweihan/advertising_law_checker) · [nexscope-ai/eCommerce-Skills](https://github.com/nexscope-ai/eCommerce-Skills) · [zhouzilai626/ecommerce-detail-page-generator](https://github.com/zhouzilai626/ecommerce-detail-page-generator) · [BWB03/helm-amazon-title-workflow](https://github.com/BWB03/helm-amazon-title-workflow)

---

# 方向 E：虚拟试穿 / AI 模特图

# 调研方向 E：虚拟试穿 / AI 模特图

## 结论先行

1. **CPU-only NAS 上：四个候选全部没戏，如实报。** 全是扩散模型（SD/SDXL 底座），官方只提供 CUDA 路径；最小的 CatVTON 也有 899M 参数、官方标注 1024×768 需 <8G **VRAM**。28GB 内存装得下权重，但 NAS 级 x86 CPU 推一张图是 20–60 分钟量级且无人维护 CPU 路径——不是"慢"，是"不可用"。
2. **许可证全灭：三个开源候选全是 CC BY-NC-SA 4.0（非商业条款，标红）**，第四个（Kolors）权重根本没开源。NC 条款与 designkit 冲突两次：电商运营出图卖货本身就是商业用途；决策 33 的商业形态是整套系统卖给别的团队。**自部署路线在许可证层面就出局，跟硬件无关。**
3. **合规且便宜的云 API 存在**：fal.ai / Kling 官方的 Kolors Virtual Try-On，**$0.07/张**（实查）。Replicate 上的 IDM-VTON $0.025/张更便宜，但第三方部署不改变 NC 许可证，商用仍违规，不建议。
4. **gpt-image-2 提示词方式基本覆盖需求，建议先不引入任何新东西。** 详见末节。

## 逐项评估

| | IDM-VTON | OOTDiffusion | CatVTON | Kolors-Virtual-Try-On |
|---|---|---|---|---|
| 地址 | github.com/yisol/IDM-VTON | github.com/levihsu/OOTDiffusion | github.com/Zheng-Chong/CatVTON | huggingface.co/spaces/Kwai-Kolors/Kolors-Virtual-Try-On |
| Star（实查） | 5.1k | 6.6k | 1.8k | 10.2k likes（HF Space） |
| 最近提交（实查） | 2025-03-07（仅加 LICENSE 文件；实质更新止于 2024-12） | **2024-05-13，停更两年多** | 2025-02-24（发 CatV2TON） | Space 在线运行中，作为 demo 活跃 |
| 许可证（实查） | ⚠ **CC BY-NC-SA 4.0（非商业）** | ⚠ **CC BY-NC-SA 4.0（非商业）** | ⚠ **CC BY-NC-SA 4.0（非商业）** | ⚠ **权重未开源**——官方明说"API 更好维护，开源以后再说"，只有 Kling.AI 商业 API |
| 解决什么问题 | 平铺服装图 + 人物图 → 该人穿上这件衣服，细节保真是四者中口碑最好 | 同上，出图偏"模特棚拍"风 | 同上，主打轻量（比前两者小一个量级） | 同上，快手官方模型，效果商业级，只针对上装优化 |
| CPU-only 能跑吗 | **不能。** SDXL 双 UNet 架构，README 只按 GPU（accelerate/CUDA）写，Replicate 官方部署用 A100-80G | **不能。** 只在 Ubuntu+GPU 测试过，demo 靠 A100 | **理论上最接近能跑**（899M 参数、<8G VRAM），但仍是扩散模型，NAS CPU 一张图数十分钟，官方无 CPU 支持 | **无权重可下载**，本地部署选项不存在 |
| 并入方式 | 不适用（许可证+硬件双出局） | 不适用（同左，且项目已死） | 不适用（许可证出局） | **调外部 API**：fal.ai `$0.07/张`，输入人物图 URL + 服装图 URL 各一张 |
| 集成工作量 | — | — | — | 约 **2–3 天**：Go worker 加一种任务类型 + 前端"试穿"入口 + 记账（可沿用 $1=1 张的内部账，$0.07 成本可忽略） |

## 云端 API 选项（只列许可证干净的）

| 服务 | 价格 | 输入 | 备注 |
|---|---|---|---|
| fal.ai `kling/v1-5/kolors-virtual-try-on` | **$0.07/次**（实查） | 人物图 + 服装图（平铺或上身均可，上装优化） | 最省事，REST，两张图进一张图出 |
| Kling AI 官方 API | 商用 API，按量 | 同上 | 中文服务商，需商务开通 |
| Replicate `cuuupid/idm-vton` | $0.025/次 | 同上 | ⚠ 便宜但底层模型是 NC 许可证，商用不合规，**不建议** |

## gpt-image-2 是否已覆盖（重点问题）

**基本覆盖，建议第一步不引入任何新组件。**依据：

- 学术评测（GPT-4o 原生图像生成报告，arxiv 2505.05501）实测 GPT 系模型做试穿能正确转移袖长、领型、印花 logo、材质纹理，连毛衣渐变纹理和衬衫上的品牌字体都能复现；专用 VTON 模型的优势集中在**条纹对位、多色图案精确对齐**这类像素级保真上。
- 运营的真实需求是"平铺图 → 模特上身营销图"——gpt-image-2 **一张服装图 + 一句提示词**就能做（模型自己生成模特），这正好走现有工作台流程，集成成本为零；专用 VTON 的差异化能力其实是"穿在**指定这个人**身上"，运营暂时没这个需求。
- 成本口径一致：内部账 $1/张，走同一个 Plus 额度，无新账号、无新计费。

**边界如实说**：带精细条纹/密集 logo/文字图案的服装，gpt-image-2 可能改样，这是它相对专用 VTON 的真实短板。

**建议动作**：在灵感库加 3–5 条"模特上身"提示词模板（半天），用真实商品（专挑带 logo 和条纹的）实测一轮；只有实测保真度不达标、或将来出现"指定模特换装"需求时，再接 fal.ai Kolors API（$0.07/张，2–3 天）。**四个开源项目一个都不要自部署。**

Sources:
- [IDM-VTON GitHub](https://github.com/yisol/IDM-VTON) / [commits](https://github.com/yisol/IDM-VTON/commits/main)
- [OOTDiffusion GitHub](https://github.com/levihsu/OOTDiffusion) / [LICENSE](https://github.com/levihsu/OOTDiffusion/blob/main/LICENSE)
- [CatVTON GitHub](https://github.com/Zheng-Chong/CatVTON)
- [Kolors-Virtual-Try-On HF Space](https://huggingface.co/spaces/Kwai-Kolors/Kolors-Virtual-Try-On) / [权重未开源的官方答复](https://huggingface.co/spaces/Kwai-Kolors/Kolors-Virtual-Try-On/discussions/167)
- [fal.ai Kling Kolors Try-On 定价](https://fal.ai/models/fal-ai/kling/v1-5/kolors-virtual-try-on)
- [Replicate cuuupid/idm-vton](https://replicate.com/cuuupid/idm-vton) / [Segmind IDM-VTON 定价](https://www.segmind.com/models/idm-vton/pricing)
- [GPT-4o 原生图像生成能力评测（含试穿）](https://arxiv.org/pdf/2505.05501)
- [开源 VTON vs 托管 API 对比 (2026)](https://fitroom.app/blog/open-source-vton-models-vs-managed-apis)