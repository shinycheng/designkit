# DesignKit 对外 API 对接文档（ERP / 第三方系统）

版本：v1　|　基础地址：`http://<平台地址>:8787`（下文以 `BASE` 代称）

## 1. 鉴权

所有接口都需要在请求头携带 API Key（由平台管理员在「API 对接」页面创建后提供）：

```
X-API-Key: dk_xxxxxxxxxxxxxxxx
```

- Key 无效或停用 → `401`
- 当月额度用完（若设置了额度）→ `429`

> **额度怎么算**：按**提交的任务次数**计，一次提交扣 1 次，与该次生成几张图无关
> （`n=4` 也只算 1 次）。

连通性自测：

```
GET BASE/api/v1/ping
→ 200 {"ok": true, "key_name": "XX ERP", "message": "API Key 有效"}
```

## 2. 核心流程

生成是**异步**的：提交任务 → 立即拿到 `job_id` → 轮询查询 **或** 等平台回调你的 `callback_url`。

```
ERP ── POST /api/v1/generations ──▶ 平台（返回 202 + job_id）
ERP ◀── POST callback_url ──────── 平台（任务完成后回调，带签名）
ERP ── GET /api/v1/generations/{job_id} ──▶ 平台（也可主动轮询）
```

## 3. 接口明细

### 3.1 获取提示词库分类（推荐用法）

```
GET BASE/api/v1/categories
→ 200 [{"slug": "ecommerce-main-image", "name": "电商主图", "count": 416,
        "recommended_for_ecommerce": true}, ...]
```

**推荐做法：提交任务时只传 `category_slug`，不必自己挑提示词。**
平台会看你上传的商品图，通览该分类下的提示词（超过 700 条的大分类按商品相关度
预筛后随机抽 700 条），挑出风格最搭的 4 条，再结合你的 `extra_instructions`
现场写出一条只属于这件商品的提示词。

`recommended_for_ecommerce` 为 true 的几个分类适合出商品图；
其余（漫画分镜、游戏素材等）也能用，但对电商基本无意义。

### 3.2 获取提示词模板列表（旧用法）

```
GET BASE/api/v1/templates
```

> ⚠️ 平台已改为**按分类生成**，这个接口通常返回空数组。
> 保留它只是为了兼容早期对接方；新接入请用上面的 `category_slug`。

| 字段 | 说明 |
|---|---|
| `id` | 提交任务时用的 `template_id` |
| `name` / `description` | 模板名称、说明 |
| `variables` | 模板变量定义数组：`name`(提交时的键)、`label`、`type`(text/select)、`options`、`default`、`required` |
| `requires_input_image` | 是否必须提供商品图 |
| `default_params` | 默认的 `size` / `n` / `quality` |

### 3.3 上传商品图

三种给图方式：预先上传、直接给公网 `image_urls`、或给 `images_base64`。
**可任选一种，也可以在同一次请求里混着用**，合计最多 4 张。

```
POST BASE/api/v1/uploads
Content-Type: multipart/form-data
file: <图片文件，png/jpg/jpeg/webp/heic/heif，≤20MB>

→ 200 {"id": 15, "url": "...", "width": 800, "height": 800, ...}
```

返回里的 `id` 就是提交任务时放进 `upload_ids` 数组的值（注意字段名是 `upload_ids`，复数）。

> **HEIC 只能走上传接口**：用 `image_urls` 或 `images_base64` 传图时只接受
> png / jpg / webp，HEIC/HEIF 会被拒绝（422）。iPhone 原图请用上传接口，
> 或先转成 JPG。
>
> **图片归属**：一个 API Key 只能使用自己上传的图片。用了别的 Key 的图片 id 会返回 `404`。

### 3.4 提交生成任务

```
POST BASE/api/v1/generations
Content-Type: application/json
```

请求体（`category_slug` / `template_id` / `prompt` **三者选一**，其余可选）：

```json
{
  "category_slug": "ecommerce-main-image",
  "extra_instructions": "整体偏暖色调，突出金属质感",
  "image_urls": ["https://你的图片/product.jpg"],
  "upload_ids": [15],
  "images_base64": ["iVBORw0KGgo..."],
  "n": 2,
  "size": "1024x1024",
  "quality": "high",
  "callback_url": "https://你的系统/designkit/callback",
  "external_ref": "你的单号ABC123"
}
```

| 字段 | 说明 |
|---|---|
| `category_slug` | **推荐**。提示词库分类，见 3.1。平台会看图挑风格并现场写提示词，**必须同时提供商品图** |
| `template_id` | 旧用法，模板 id（三者选一） |
| `prompt` | 自定义提示词，原样发给生图模型（三者选一） |
| `variables` | 模板变量取值（仅 `template_id` 用法需要） |
| `image_urls` / `upload_ids` / `images_base64` | 商品图，三者可混用，合计最多 4 张。`image_urls` 必须是直链、不跟随重定向；平台默认允许内网地址（方便内部图片服务器），但云元数据等危险地址始终被拒绝。若平台在「系统设置」关闭了内网访问，则内网图片请改用上传接口或 base64 |
| `n` | 生成张数 1-4。**不填不等于 1**：优先用模板自带的默认张数（模板列表里的 `default_params.n`），模板没设才用平台默认。想精确控制费用请每次显式传 `n` |
| `size` | 形如 `1024x1360` 的像素串，或 `auto`。不填同样先取模板默认值。**不是固定枚举**——只要过得了下面的护栏规则就放行，常用版位见下表 |
| `quality` | `auto` / `low` / `medium` / `high`。不填同样先取模板默认值 |
| `callback_url` | 任务完成后平台回调此地址（http/https），**最长 512 个字符**。平台默认允许内网地址；若平台在「系统设置」里关闭了内网访问，内网回调地址会被拒绝并记为 `rejected_unsafe_url` |
| `external_ref` | 你方业务单号，**最长 128 个字符**，查询与回调时原样带回，便于对账 |

**尺寸护栏规则**（不满足返回 422，`detail` 会写明是哪条）：

- 宽高都必须是 **16 的倍数**（模型会自行取整，否则出图尺寸和你要的对不上）
- 最短边 ≥ 512，最长边 ≤ 2048
- 长短边比例 ≤ 3:1
- 总像素在 512×512 ~ 2048×2048 之间

平台内置的常用电商版位（`GET /api/web/size-presets` 可取到同一份，需网页端登录态）：

| 尺寸 | 比例 | 常见用途 |
|---|---|---|
| `1024x1024` | 1:1 | 淘宝/京东主图 |
| `1024x1360` | 3:4 | 详情页、小红书 |
| `1024x1280` | 4:5 | 社媒信息流 |
| `1024x1536` | 2:3 | 长图详情 |
| `1024x1824` | 9:16 | 抖音/快手封面 |
| `1536x1024` | 3:2 | 场景与横幅 |
| `1360x1024` | 4:3 | PC 端 banner |
| `1824x1024` | 16:9 | 视频封面 |

> ⚠️ **`n` / `size` / `quality` 不填时会跟随模板的默认值**，这直接影响费用。
> 例如平台自带的「节日促销氛围图」模板默认就是 2 张——只传 `template_id` 不传 `n`，
> 实际会出 2 张、按 2 张计费。响应里的 `params` 字段会回显这次真正生效的参数，
> 可以据此核对。

响应 `202`：

```json
{"job_id": "414ce2f0a331445e...", "status": "pending", "external_ref": "你的单号ABC123", ...}
```

错误码：

| 状态码 | 含义 | `detail` 的形态 |
|---|---|---|
| `422` | 参数有问题 | **两种形态都可能，解析时要兼容**：业务校验是一句中文字符串；字段格式校验（如 `n` 超出 1-4、`external_ref` 超长）是框架返回的英文明细数组 |
| `404` | 模板不存在或已停用；或 `upload_ids` 里的图片不存在、不属于本 API Key | 中文字符串，会写明是哪一种 |
| `429` | 当月额度用完 | 中文字符串 |

两种 422 的真实返回长这样：

```json
// 业务校验（字符串）
{"detail": "尺寸不可用：宽高必须是 16 的倍数（模型会自行取整，否则出图尺寸对不上）"}

// 字段格式校验（数组，英文）
{"detail": [{"type": "less_than_equal", "loc": ["body", "n"],
             "msg": "Input should be less than or equal to 4",
             "input": 5, "ctx": {"le": 4}}]}
```

### 3.5 查询任务

```
GET BASE/api/v1/generations/{job_id}
```

```json
{
  "job_id": "414c...",
  "status": "succeeded",          // pending 排队 / processing 生成中 / succeeded 成功 / failed 失败
  "error": "",                    // 失败时的中文原因
  "external_ref": "你的单号ABC123",
  "params": {"n": 2, "size": "1024x1024", "quality": "high"},
  "images": [
    {"id": 31,
     "url": "http://.../files/outputs/202608/414c.../0.png",
     "thumbnail_url": "http://.../files/thumbnails/...jpg",
     "width": 1024, "height": 1024, "format": "png"}
  ],
  "created_at": "...", "finished_at": "..."
}
```

- **`params` 是本次任务实际生效的参数**。不传 `n` 时，从这里可以确认平台到底生成了几张图、
  会按几张计费。
- 响应里还会返回若干平台内部字段（`source`、`template_id`、`template_name`、`prompt`、
  `prompt_sent`、`callback_url`、`webhook_status`、`attempts`、`started_at` 等），
  对接方忽略即可。平台后续可能新增字段，请按「只取自己需要的字段」的方式解析。

错误：`404` 表示任务不存在，**或该任务不是本 API Key 提交的**——收到 404 请停止轮询，
不要一直重试；`401` 表示 API Key 有问题。

建议轮询间隔 3~5 秒。真实生图一般 1~5 分钟（参考图越多越慢，平台最多等 6 分钟才判超时），
开启了「AI 现场写提示词」还要再多约 30 秒。图片 URL 可直接下载保存到你方系统。

> **图片链接不需要登录就能打开**，链接本身就等于钥匙。请下载到你方系统后自行管控，
> 不要把链接转发到不该看到的地方。

> **出图可能是透明底 PNG**：平台管理员若在「系统设置 → 生图服务」把「出图底色」
> 设成透明底，返回的 PNG 会带 alpha 通道（是否真的带取决于上游模型是否支持）。
> 你方系统若要叠背景或转 JPG，**先合成底色再转**——直接转 JPG 会把透明区变成黑色。

> **注意**：`images` 的数量可能少于请求的 `n`。生成服务中途出错时，平台会保留已成功的图并把任务标记为
> `succeeded`（避免丢弃已计费的结果）。请以 `images.length` 为准，需要补齐时**只按缺的张数**
> 再发一次任务即可，不必把整单重跑。

## 4. 回调（Webhook）

任务到达终态（succeeded / failed）后，平台向 `callback_url` 发送：

```
POST callback_url
Content-Type: application/json
X-DesignKit-Timestamp: 1786162345
X-DesignKit-Signature: sha256=<签名>
```

Body 与「查询任务」返回结构一致，另含 `"event": "generation.finished"`。

**签名校验**（强烈建议）：用创建 API Key 时下发的 `webhook_secret` 计算

```
期望签名 = "sha256=" + HMAC_SHA256(webhook_secret, timestamp + "." + 原始请求体字节)
```

与请求头 `X-DesignKit-Signature` 比对一致才处理。Python 示例：

```python
import hmac, hashlib

def verify(secret: str, timestamp: str, body: bytes, signature: str) -> bool:
    expected = "sha256=" + hmac.new(
        secret.encode(), timestamp.encode() + b"." + body, hashlib.sha256
    ).hexdigest()
    return hmac.compare_digest(expected, signature)
```

- 你方需在 15 秒内返回 2xx。首次投递失败后，平台会在 8 秒、30 秒后各重投一次（含首次共 3 次尝试）
- 回调失败不影响任务本身，仍可通过查询接口取结果

> ⚠️ **必须做幂等**：同一个 `job_id` 的回调可能被送达多次。除了上面的失败重投，
> 还有两种情况会再次回调——平台重启时会补发状态还停在「发送中」的回调；
> 平台管理员在网页上对你提交的任务点「重新生成」，完成后也会再回调一次。
> 请按 `job_id` 去重，重复收到时以最后一次的内容为准，不要重复入库或重复计费。

## 5. 在线调试

浏览器打开 `BASE/docs`，展开「对外API-ERP对接」分组，填入 X-API-Key 即可在线调用。
