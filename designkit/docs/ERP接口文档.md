# DesignKit ERP 接口文档

> 面向对接 DesignKit 的外部系统（ERP）开发者。更新日期：2026-08-16。
> 本文档以 `backend/internal/designkit/handler/` 的实际代码为准逐条核对过，
> 是 `/v1/designkit/*` 的**对外契约**：路径、字段名、错误码定了不改。
>
> ⚠ 《设计定型.md》第三节那张路径表是设计期快照，里面混有从未实现的路径
> （对接方照着写会 404）。**与本文档不一致时，一律以本文档为准。**
> 逐条更正见[附录 C](#附录-c设计定型md-幻影路径更正表)。

---

## 1. 基础约定

| 项 | 约定 |
|---|---|
| 前缀 | `https://<部署地址>/v1/designkit`（下文路径均省略前缀） |
| 请求/响应 | JSON，UTF-8。上传用 `multipart/form-data`；`.../content` 端点直接返回图片二进制 |
| 时间 | RFC3339，UTC，如 `2026-08-16T03:20:00Z` |
| 金额 | **一律字符串**（如 `"12.50"`），配套 `currency` 字段恒为 `"USD"`。用 decimal/BigDecimal 解析，不要用浮点数 |
| 编号 | 各资源的 `uid` 是 26 位 ULID 字符串。数据库自增 id 一律不对外暴露 |
| 图片地址 | 各响应里的 `content_url` / `items_url` 是运行时拼出的相对路径（带前缀），**不要存长期缓存**，要图就现请求 |

两条容易踩的：

- **`/api/v1/designkit/*` 是浏览器前端用的**（jwt 登录态），ERP 不要调。
  两条前缀挂的是同一批 handler，但鉴权方式不同。
- 响应 JSON 的**任何层级都不会出现名为 `code` 的字段**。错误码字段叫
  `error_code`（错误信封）/ `last_error_code`（批次）/ `error_code`（单张）。
  按 `code` 找字段会永远取不到值。

---

## 2. 鉴权与额度语义

所有端点都要带 API Key：

```
Authorization: Bearer <API Key>
```

也接受 `x-api-key: <API Key>` 请求头。**Key 绝不能放 URL 参数**——
`?key=` / `?api_key=` 会被直接拒绝（400），因为 query 会进各级访问日志。

ERP 使用**单独开的账号**的 API Key：出的图记在它名下、费用单独结算，
与运营在界面上的操作互不干扰。Key 由管理员手动开一次，见运维文档。

### 2.1 两类端点，两套准入（重要）

| 类别 | 端点 | 准入 |
|---|---|---|
| **花钱的写端点** | `POST /jobs`、`POST /jobs/:uid/items/:seq/retry`、`POST /chat/messages` | 完整计费准入：余额不足返回 402 `DK_INSUFFICIENT_BALANCE`，Key 额度耗尽返回 403 `DK_QUOTA_EXHAUSTED` |
| **其余全部端点** | 查询、取图、上传、报价、停止排队、灵感库、放大、文案检查…… | 轻量 Key 鉴权，**不做计费准入** |

**额度用尽只影响提交新任务，不影响查询和取图。**
Key 的日消费封顶被打满、余额扣到 0，`GET /jobs/:uid`、`GET .../content`
照常能用——已经付过钱的图永远取得回来。据此可以放心给 ERP Key 设日消费上限。

Key 被停用、对应账号被停用时所有端点都返回 401/403。
Key 上配了 IP 白/黑名单的话按配置生效（不匹配返回 403）。

---

## 3. 错误格式（只有一种）

所有端点、所有 HTTP 4xx/5xx，响应体都是同一个信封（中间件统一改写，
上游透出的其他格式也会被翻译成这一种）：

```json
{
  "error": {
    "type": "invalid_request_error",
    "error_code": "DK_RATIO_NOT_ALLOWED",
    "message": "不支持这个出图比例，请从列表里选一个。",
    "request_id": "8f2c1a6e-…"
  }
}
```

| 字段 | 说明 |
|---|---|
| `type` | 只有四个取值：`invalid_request_error` / `authentication_error` / `rate_limit_error` / `api_error` |
| `error_code` | 我方错误码，全部 `DK_` 前缀的 UPPER_SNAKE。**做程序分支用它**，全集见[附录 A](#附录-a错误码表) |
| `message` | 中文说明，可直接透给使用者看。措辞可能调整，**不要按 message 做分支** |
| `request_id` | 本次请求的追踪号，可能缺席（极少数拿不到时省略） |

HTTP 状态码保留各自语义（400/401/402/403/404/409/413/415/422/429/5xx），
重试策略可以按状态码定；语义分支按 `error_code` 定。

**裸 404**（响应体不是上面这个信封、没有 `DK_` 错误码）有专门含义：
**这个端点在当前部署里不存在 / 功能未上线**，不是业务错误。
`POST /prompts/suggest` 和「本批次新增」的端点要按这个约定判功能是否可用。

---

## 4. 排障：request_id

每个响应（含成功）都带响应头：

```
X-Designkit-Request-Id: <追踪号>
```

错误信封里的 `request_id` 与它同值。**报障时带上这个号 + 完整错误体**，
管理员在服务端日志里能直接定位到这一次请求。

---

## 5. 防重复提交（Idempotency-Key）

`POST /jobs` 和 `POST /jobs/:uid/items/:seq/retry` **必须**带请求头：

```
Idempotency-Key: <你生成的唯一标识>
```

规则：

- 1~128 个可见 ASCII 字符（不能有空格和中文）。建议用订单号或 UUID。
- 缺失直接 400 `DK_IDEMPOTENCY_KEY_REQUIRED`。
- **同 key + 同内容** → 命中重放：不新建任务、不重复扣费，原样返回第一次的
  结果，并带响应头 `X-Idempotency-Replayed: true`。超时重发就该这么发——
  **用同一个 key 原样重发**。
- **同 key + 不同内容** → 409 `DK_IDEMPOTENCY_KEY_CONFLICT`。
- 重放窗口约 24 小时。作用域按用户隔离，不用担心跟别的账号撞 key。
- 协调器繁忙/退避时返回 409/429，可能带 `Retry-After` 响应头（秒）。

其余写端点（stop、上传等）不要求这个头。

---

## 6. 限流与轮询建议

- 收到 **429** 时：响应可能带 `Retry-After: <秒>` 头，有就按它退避，
  没有就等几秒再试。
- 轮询任务进度（`GET /jobs/:uid`、`GET /jobs/:uid/items`）**间隔 ≥ 2 秒**。
- 轮询放大状态（`GET /assets/:uid/upscale`）建议 5 秒一次（一张要一两分钟）。
- 生成过程中单张图撞上游并发/限流时，`error_code` 会出现
  `DK_UPSTREAM_BUSY` / `DK_RATE_LIMITED`——这是系统内部自动排队重试的中间态，
  ERP 不需要为它做什么，继续轮询即可。

---

## 7. 关键契约：顺序与计数

这几条是写死的对外契约，服务端有专门的契约测试守着：

1. **图片序号 `seq` 从 1 开始。**「第 1 张」就是第一张。
2. **批量展开顺序固定为「商品图外层、提示词内层」**。
   提示词顺序 = `prompt_uids` 数组顺序，后接 `prompts`（手写词）数组顺序：

   ```
   asset_uids = [图A, 图B]，prompt_uids = [词1, 词2]
   → seq 1: 图A×词1   seq 2: 图A×词2   seq 3: 图B×词1   seq 4: 图B×词2
   ```

3. **`GET /jobs/:uid/items` 返回按 `seq` 升序**，与「按序号取第几张」严格对应
   （服务端显式 ORDER BY，handler 再排一遍双保险）。
4. **判断有没有出图一律看 `success_count`，不看 `status`。**
   `cancelled` 不代表没花钱：停止排队时已经在生成的那几张会跑完、会计费、
   图会正常入库。
5. `image_index` 从 1 开始；只有上游一次返回多张时才会出现大于 1 的值。
6. **提示词快照**：任务里存的是提交那一刻的提示词原文。灵感库后来改词、
   下架，历史任务的 `prompt_text` 不变。

---

## 8. 端点总览

标 💰 的走计费准入（见 2.1），标 🆕 的是**本批次新增**（见 9.8 的部署确认提示）。

| 方法与路径 | 说明 |
|---|---|
| `POST /assets` | 上传商品图 |
| `GET /assets/:uid` | 商品图信息 |
| `GET /assets/:uid/content` | 商品图字节 |
| `POST /assets/from-image/:uid` | 把一张结果图变成新的商品图（继续生成） |
| `POST /assets/:uid/remove-background` | 生成白底图（不花钱，本地抠图） |
| `POST /assets/:uid/upscale` | 高清放大（异步，不花钱） |
| `GET /assets/:uid/upscale` | 放大进度 |
| 🆕 `DELETE /assets/:uid` | 删除商品图（软删） |
| `GET /ratios` | 可用出图比例与单价 |
| `POST /jobs/estimate` | 报价（不建任务、不冻结额度） |
| 💰 `POST /jobs` | 提交一批出图任务 |
| `GET /jobs` | 任务列表（游标分页） |
| `GET /jobs/:uid` | 任务详情 |
| `GET /jobs/:uid/items` | 全部单张明细（按 seq 升序） |
| `GET /jobs/:uid/items/:seq` | 单张明细 |
| `GET /jobs/:uid/items/:seq/content` | 单张结果图字节 |
| 🆕 `GET /jobs/:uid/images.zip` | 整批打包下载（zip，只含出成功的） |
| `POST /jobs/:uid/stop` | 停止排队（不花钱，止损用） |
| 💰 `POST /jobs/:uid/items/:seq/retry` | 重试一张（重新收一次费） |
| 🆕 `DELETE /jobs/:uid` | 删除任务记录（软删，不停止生成、不退费） |
| `GET /images/:uid` | 结果图信息 |
| `GET /images/:uid/content` | 结果图字节 |
| `GET /prompt-categories` | 灵感库分类 |
| `GET /prompts` | 提示词检索（游标分页） |
| `GET /prompts/:uid` | 提示词详情 |
| `POST /prompts/suggest` | AI 挑提示词（1~2 分钟，不计入出图费） |
| 💰 `POST /chat/messages` | AI 对话发消息（按 token 计费） |
| `GET /chat/sessions` | 对话会话列表 |
| `GET /chat/sessions/:uid` | 会话与全部消息 |
| `DELETE /chat/sessions/:uid` | 删除会话 |
| `POST /content/check` | 文案检查（违禁词 + 标题字数，纯计算） |
| `GET /me/usage/summary` | 本月出图/花费/余额 |
| `POST /me/quota-requests` | 申请额度 |

灵感库同步（`/prompts/sync*`）和系统设置（`/admin/settings`）**只挂浏览器前缀、
仅管理员**，ERP 前缀下不存在这些路径。

---

## 9. 端点详情

### 9.1 素材（商品图）

#### POST /assets —— 上传商品图

`multipart/form-data`，文件字段名 **`file`**。
限制：单张 ≤ **20MB**；格式 JPG / PNG / WEBP / HEIC（HEIC 会在出图前自动转码）。

```
201 →
{
  "uid": "01J8ZK3V9WX2P6Q8R4T1M5N7E9",
  "content_type": "image/jpeg",
  "byte_size": 2048576,
  "width": 1200, "height": 1200,          // 可能为 null（尚未解析）
  "origin": "erp",
  "created_at": "2026-08-16T03:20:00Z",
  "content_url": "/v1/designkit/assets/01J8…/content"
}
```

错误：413 `DK_IMAGE_TOO_LARGE`、415 `DK_UNSUPPORTED_IMAGE_FORMAT`、
400 `DK_INVALID_REQUEST`（没带 `file` 字段 / 空文件）。

同一账号重复上传相同字节的图按 sha256 去重：返回同一条 asset（仍是 201）。

#### GET /assets/:uid —— 商品图信息

`200` 返回上面的 assetDTO。不存在、已删除、**或不属于这把 Key 的账号**，
一律 404 `DK_ASSET_NOT_FOUND`（不区分「存在但不是你的」）。

#### GET /assets/:uid/content —— 商品图字节

`200` 直接返回图片二进制（`Content-Type` 为图片类型，
`Cache-Control: private, max-age=300`）。错误同上。

#### POST /assets/from-image/:uid —— 用结果图继续生成

把一张**已出好的结果图**（`:uid` 是结果图的 uid）在服务端复制成一条新的
**商品图**，返回的 asset uid 可直接塞进下一次 `POST /jobs` 的 `asset_uids`。
图片字节不经过调用方。无请求体。

```
201 → assetDTO（与 POST /assets 同形状）
```

重复调用按 sha256 去重，拿到同一条 asset。
错误：404 `DK_IMAGE_NOT_FOUND`。

#### POST /assets/:uid/remove-background —— 生成白底图

抠掉背景、合成白底，存成一条**新的**商品图。走本地抠图服务，**不花钱**。
无请求体。

```
201 → assetDTO（新商品图）
```

抠图服务未配置时返回错误信封（中文说明「还没准备好」），不是裸 404。
错误：404 `DK_ASSET_NOT_FOUND`。

#### POST /assets/:uid/upscale —— 高清放大（异步）

把一张商品图 ×4 放大（本地推理，**不花钱**）。无请求体。**统一返回 202**，
按 `task.status` 分支，不要按 HTTP 状态码分支：

```
202 →
{
  "task": {
    "asset_uid": "01J8…",
    "status": "queued",                  // queued / running / done / failed
    "error_message": "…",                // failed 时才有
    "error_code": "DK_UPSCALE_FAILED",   // failed 时才有
    "result": { …assetDTO… },            // done 时才有：新的商品图
    "created_at": "…", "updated_at": "…"
  }
}
```

重复 POST 同一张 = 拿到现有任务（或已完成的结果），不会排两遍。
错误：404 `DK_ASSET_NOT_FOUND`、429 `DK_UPSCALE_QUEUE_FULL`。

#### GET /assets/:uid/upscale —— 放大进度

`200` 返回同上的 `{task: …}`。
404 `DK_UPSCALE_NOT_FOUND` = 这张图从来没排过放大（或不是本账号的图）。
放大任务存在数据库里：**服务重启后没放完的会自动接着放**，不用重新 POST，
继续轮询即可。

#### 🆕 DELETE /assets/:uid —— 删除商品图

软删：这张图从可用素材里去掉，**历史任务的引用不受影响**（任务详情仍能
看到当时用的图），文件字节保留（图片永久保留是拍板过的）。无请求体。

```
200 → { "ok": true }
```

错误：404 `DK_ASSET_NOT_FOUND`（含重复删除）。

### 9.2 比例与报价

#### GET /ratios —— 可用出图比例

```
200 →
{
  "ratios": [
    {
      "ratio": "1:1",
      "target_width": 1024, "target_height": 1024,
      "pricing_tier": "1K",
      "unit_price": "1.00",        // null = 价格待实测确认
      "price_confirmed": true,     // false 时 unit_price 必为 null
      "is_default": true
    },
    …
  ],
  "currency": "USD"
}
```

比例是配置项，当前配置为 `1:1` / `3:4` / `4:3` / `16:9` / `9:16`。
**以本接口实时返回为准**，不要把比例列表写死在 ERP 侧。
`price_confirmed=false` 时不要向使用者展示任何猜测单价。

#### POST /jobs/estimate —— 报价

只算账：不建任务、不冻结额度、不花钱。请求体与 `POST /jobs` 的任务描述部分相同：

```json
{
  "ratio": "1:1",
  "asset_uids": ["01J8…A", "01J8…B"],
  "prompt_uids": ["01J8…P"],
  "prompts": ["手写的提示词也可以"],
  "model": "",                 // 空 = 系统默认
  "keep_transparency": false
}
```

```
200 →
{
  "item_count": 4,             // = 商品图数 × 提示词总数
  "asset_count": 2,
  "prompt_count": 2,
  "max_batch_items": 50,       // 单次批量上限（配置项），超了提交会被拒
  "pricing_tier": "1K",
  "unit_price": "1.00",        // null = 价格待确认
  "estimated_cost": "4.00",    // null = 价格待确认
  "price_confirmed": true,
  "price_note": "",            // 价格待确认时的中文说明
  "currency": "USD",
  "balance": "20.00",
  "available": "16.00",        // 余额 - 未结算批次冻结的部分
  "sufficient": true,
  "shortfall": "0"             // 不够时是还差多少
}
```

### 9.3 任务（批次）

#### 💰 POST /jobs —— 提交一批

必带 `Idempotency-Key`（第 5 节）。请求体 = estimate 的字段 + 两个可选项：

```json
{
  "ratio": "1:1",
  "asset_uids": ["01J8…A"],
  "prompt_uids": ["01J8…P"],
  "prompts": [],
  "model": "",
  "keep_transparency": false,
  "name": "双十一主图第一批",              // 选填
  "callback_url": "https://erp.example.com/hook"   // 选填，见第 11 节现状
}
```

校验规则：`ratio` 必须在 `GET /ratios` 列表里；`asset_uids` ≥ 1 条；
`prompt_uids` + `prompts` 合计 ≥ 1 条；两边各 ≤ 500 条（入口防爆闸），
真正的单批上限看 estimate 的 `max_batch_items`（默认 50，超出 400
`DK_BATCH_TOO_LARGE`）；`callback_url` 要么不填、要么是完整 http/https
网址（≤1024 字符）。

`keep_transparency`：按批生效。`true` = 预处理补边时保留透明底，
`false` = 合成白底；**不传 = 合成白底**（系统默认）。整批统一，
提交后不可改；重试的那张沿用本批的值。

提交成功即**冻结** `estimated_cost`，出完按实际张数结算多退少不补；
可用额不够直接 402 拒绝，**不允许透支**。

```
201 →
{
  "uid": "01J8…J",
  "status": "created",
  "item_count": 4,
  "currency": "USD",
  "estimated_cost": "4.00",
  "revision": 0,
  "name": "双十一主图第一批",
  "ratio": "1:1",
  "model": "gpt-image-2",
  "created_at": "…",
  "callback_url": "https://erp.example.com/hook"    // 没填时是 null
}
```

响应刻意不内联 items——进度去 `GET /jobs/:uid/items` 拿。
命中幂等重放时响应头带 `X-Idempotency-Replayed: true`。

错误：402 `DK_INSUFFICIENT_BALANCE`、403 `DK_QUOTA_EXHAUSTED`、
400 `DK_RATIO_NOT_ALLOWED` / `DK_BATCH_TOO_LARGE` / `DK_INVALID_REQUEST`、
404 `DK_ASSET_NOT_FOUND` / `DK_PROMPT_NOT_FOUND`、
409 `DK_IDEMPOTENCY_KEY_CONFLICT`。

#### GET /jobs —— 任务列表

查询参数：

| 参数 | 说明 |
|---|---|
| `status` | 过滤状态，可重复出现或逗号分隔：`?status=running&status=succeeded` 或 `?status=running,succeeded` |
| `cursor` | 上一页的 `next_cursor`。**不透明字符串，不要解析** |
| `limit` | 每页条数，默认 20，最大 100 |

```
200 →
{
  "items": [ …jobDTO… ],
  "has_more": true,
  "next_cursor": "MTcy…"        // has_more=false 时是 null
}
```

游标分页（不是 offset），持续写入时也不漏数据、不重复。

jobDTO 字段：

```json
{
  "uid": "01J8…J",
  "name": "双十一主图第一批",
  "status": "partially_failed",
  "origin": "erp",
  "ratio": "1:1",
  "model": "gpt-image-2",
  "item_count": 4,
  "success_count": 3,
  "fail_count": 1,
  "cancelled_count": 0,
  "currency": "USD",
  "estimated_cost": "4.00",
  "actual_cost": "3.00",             // 结算前是 null
  "revision": 5,
  "last_error_code": null,           // 整批失败时是 DK_ 错误码
  "last_error_message": null,        // 对应中文
  "created_at": "…", "updated_at": "…",
  "started_at": "…", "finished_at": "…",
  "settled_at": "…", "cancel_requested_at": null,
  "items_url": "/v1/designkit/jobs/01J8…J/items"
}
```

#### GET /jobs/:uid —— 任务详情

`200` 返回 jobDTO。404 `DK_JOB_NOT_FOUND`。

#### GET /jobs/:uid/items —— 单张明细

```
200 → { "items": [ …jobItemDTO… ] }     // 按 seq 升序，全量返回，无分页
```

jobItemDTO 字段：

```json
{
  "seq": 1,
  "status": "succeeded",             // pending / running / succeeded / failed / cancelled
  "prompt_text": "提交那一刻的提示词原文快照",
  "asset_uid": null,                 // 当前版本恒为 null，先按 null 兼容
  "prompt_uid": null,                // 同上
  "attempt_count": 1,
  "max_attempts": 3,
  "error_code": null,                // 失败时是 DK_ 错误码
  "error_message": null,             // 对应中文
  "currency": "USD",
  "billed_cost": "1.00",             // 未计费时是 null
  "billing_tier": "2K",              // 未计费时是 null
  "created_at": "…", "started_at": "…", "finished_at": "…",
  "images": [ …imageDTO… ],          // 当前版本的结果图，按 image_index 升序
  "content_url": "/v1/designkit/jobs/01J8…J/items/1/content"
}
```

#### GET /jobs/:uid/items/:seq —— 单张详情

`200` 返回 jobItemDTO。404 `DK_ITEM_NOT_FOUND`（seq 不合法也是它）。

#### GET /jobs/:uid/items/:seq/content —— 单张结果图字节

查询参数 `image_index` 默认 1。`200` 返回图片二进制。
图还没出来时 404 `DK_IMAGE_NOT_FOUND`——轮询 item 的 `status` 变成
`succeeded` 之后再取图。

#### 🆕 GET /jobs/:uid/images.zip —— 整批打包下载

把这一批**出成功的每一张**打进一个 zip 返回（`Content-Type: application/zip`）。
包内文件名 `第{seq}张.png`，`{seq}` 与 `GET /jobs/:uid/items` 里的 seq 严格对应；
失败、停止、还没出的不进包。`Content-Disposition` 带批次名
（中文按 RFC 5987 编码在 `filename*` 里）。

一张成功的图都没有时 404 `DK_IMAGE_NOT_FOUND`——**不会返回空 zip**。
其余错误：404 `DK_JOB_NOT_FOUND`（含不是这把 Key 的任务）、`DK_STORAGE_ERROR`。

#### POST /jobs/:uid/stop —— 停止排队

**不花钱端点**（额度耗尽时也能用，专门用来止损）。无请求体。语义：
还没开始的不再生成、不扣费；**已经在生成的会跑完并正常计费**，图正常入库。

```
200 →
{
  "stopped_count": 3,           // 这次停掉的（不再生成、不扣费）
  "still_running_count": 2,     // 会跑完并计费的
  "message": "已经停止排队。…", // 拼好的中文，可直接展示
  "job": { …jobDTO… }           // 停止后的最新状态
}
```

错误：404 `DK_JOB_NOT_FOUND`、409 `DK_ILLEGAL_STATE_TRANSITION`（已是终态）。

#### 💰 POST /jobs/:uid/items/:seq/retry —— 重试一张

必带 `Idempotency-Key`。**重试一张 = 重新出一张图 = 重新收一次费。**
每张累计上限 `max_attempts`（默认 3）次。无请求体。

```
200 →
{
  "item": { …jobItemDTO… },     // 已退回排队中，attempt_count 已 +1，images 为空数组
  "message": "已经重新排队。…",
  "remaining_attempts": 1
}
```

错误：409 `DK_MAX_ATTEMPTS_EXCEEDED` / `DK_JOB_ALREADY_SETTLED` /
`DK_ILLEGAL_STATE_TRANSITION`、402 `DK_INSUFFICIENT_BALANCE`、
404 `DK_ITEM_NOT_FOUND`。

#### 🆕 DELETE /jobs/:uid —— 删除任务记录

软删：这条批次从 `GET /jobs` 列表和详情里消失。删的只是记录的可见性——
数据、结果图文件、账单都还在，**已扣的费用不退**。无请求体。

**没结束的批次不让删**（409 `DK_ILLEGAL_STATE_TRANSITION`，提示先停止排队、
等它结束再删）。这是刻意的：删掉一个还在跑的批次，图照样出、钱照样扣，
调用方却失去了停止入口。正确顺序：`POST /jobs/:uid/stop` → 轮询到终态 → 删。

```
200 → { "ok": true }
```

错误：404 `DK_JOB_NOT_FOUND`（含重复删除）、
409 `DK_ILLEGAL_STATE_TRANSITION`（批次未结束）。

### 9.4 结果图

#### GET /images/:uid —— 结果图信息

```
200 →
{
  "uid": "01J8…I",
  "job_uid": "01J8…J",          // 属于哪一批
  "seq": 1,                     // 那一批的第几张
  "attempt": 1,
  "image_index": 1,
  "is_current": true,           // false = 被重试出的新图替代的旧版本
  "content_type": "image/png",
  "byte_size": 1834021,
  "width": 1254, "height": 1254,
  "billing_tier": "2K",
  "created_at": "…",
  "content_url": "/v1/designkit/images/01J8…I/content"
}
```

#### GET /images/:uid/content —— 结果图字节

`200` 返回图片二进制。404 `DK_IMAGE_NOT_FOUND`。

### 9.5 灵感库

#### GET /prompt-categories —— 分类列表

```
200 →
{
  "categories": [
    { "slug": "ecommerce-hero", "name": "电商主图", "name_en": "E-commerce Hero",
      "sort_order": 10, "prompt_count": 428 }
  ],
  "total_prompt_count": 15045
}
```

分类对外标识是 `slug`（没有 uid）。

#### GET /prompts —— 检索

查询参数：`category`（分类 slug，不传 = 全部）、`keyword`（≤64 字，
标题和正文都搜；**库内标题几乎全是英文，建议用英文关键词**）、
`cursor`、`limit`（默认 20，最大 100）。

```
200 →
{
  "items": [ …promptDTO… ],
  "total": 428,                 // 符合条件的总条数
  "has_more": true,
  "next_cursor": "cDEy…"
}
```

promptDTO：

```json
{
  "uid": "01J8…P",
  "title": "Minimalist product hero shot",
  "body": "提示词完整正文，不截断",
  "variables": [ { "name": "art_style", "example": "watercolor" } ],
  "preview_url": null,
  "category_slug": "ecommerce-hero",
  "category_name": "电商主图",
  "source": "youmind",          // youmind = 开源库同步；user = 自建
  "is_enabled": true,
  "created_at": "…", "updated_at": "…"
}
```

#### GET /prompts/:uid —— 提示词详情

`200` 返回 promptDTO。已下架的也能查到（`is_enabled=false`）。
404 `DK_PROMPT_NOT_FOUND`。

#### POST /prompts/suggest —— AI 挑提示词

让 AI 看着商品图从灵感库挑参考并合成**一条**最终提示词。
**要跑 1~2 分钟**（后端串行多趟对话），客户端超时设 ≥ 240 秒。
不计入出图费（对话费另计，约 $0.1~$0.34/次）。不要求 `Idempotency-Key`。

```json
{
  "asset_uid": "01J8…A",              // 必填，主商品图
  "extra_asset_uids": ["01J8…B"],     // 选填，同批其余图
  "category_slug": "",                // 空串 = 全部分类，由 AI 自己判
  "features": "米白色针织开衫，秋冬新品",  // 选填，≤500 字
  "force": false                      // 选填，true = 跳过缓存强制重新推荐
}
```

```
200 →
{
  "prompt": "合成出来的最终提示词全文",
  "category": { "slug": "ecommerce-hero", "name": "电商主图" },
  "candidates": [ { "uid": "01J8…P", "title": "…" } ],   // 参考过的 5 条
  "note": "",                         // 可空的中文说明
  "cached_at": "2026-08-24T02:03:04Z" // 只在命中缓存时出现，见下
}
```

**结果有缓存**：输入完全相同（同一批 `asset_uid` + 同分类 + 同一句 `features`）的
重复请求，24 小时内直接返回上次的结果 —— 秒回、不再产生对话费，响应里多一个
`cached_at`（那次结果生成的时间，RFC3339 UTC）。要强制重新推荐就传 `"force": true`，
新结果会顶掉缓存。缓存在后端进程内，服务重启后自然失效。

**裸 404 = 该功能在此部署未上线**（见第 3 节），不是业务错误。
业务错误照常走错误信封（如 404 `DK_ASSET_NOT_FOUND`）。

自建提示词（`POST /prompts` 等）**尚未提供**（见附录 C）。ERP 侧自己的
提示词直接放在 `POST /jobs` 的 `prompts` 数组里传原文即可，效果相同。

### 9.6 AI 对话

#### 💰 POST /chat/messages —— 发消息

同步等 AI 回复（几秒到几十秒），客户端超时设 ≥ 120 秒。按 token 计费，
走计费准入。

```json
{
  "session_uid": "",               // 空 = 新建会话
  "text": "帮我想三个卖点文案",
  "asset_uids": ["01J8…A"]         // 选填，附带商品图
}
```

```
200 →
{
  "session_uid": "01J8…S",
  "title": "会话标题",
  "user_message":      { "id": 1, "role": "user",      "content": "…", "asset_uids": ["01J8…A"], "created_at": "…" },
  "assistant_message": { "id": 2, "role": "assistant", "content": "…", "asset_uids": [],          "created_at": "…" }
}
```

**流式模式（可选，2026-08-24 加入）**：请求体加 `"stream": true`，响应变为
`text/event-stream`（SSE）。帧格式（对外契约）：

| 帧 | data 内容 |
|---|---|
| `event: delta` | `{"text":"…"}`，回复正文的一段，按序推送 |
| `event: done` | 与非流式成功响应同构的完整 JSON（成功收尾，随后关流） |
| `event: error` | 标准错误信封 JSON（仅流开始后出错才用；流开始前的失败仍是普通 JSON 错误 + 状态码） |

计费与非流式一致（按 token，一次发送计一次）；中途断开连接不影响服务端完成与落库。

#### GET /chat/sessions —— 会话列表

`200 → { "sessions": [ { "uid", "title", "created_at", "updated_at" } ] }`

#### GET /chat/sessions/:uid —— 会话与消息

`200 → { "session": {…}, "messages": [ …messageDTO… ] }`
404 `DK_CHAT_SESSION_NOT_FOUND`。

#### DELETE /chat/sessions/:uid —— 删除会话

`200 → { "ok": true }`

### 9.7 文案检查与消费

#### POST /content/check —— 文案检查

纯计算，不花钱、不落库。一次 ≤ 10000 字。

```json
{ "text": "要检查的文字", "platform": "taobao" }   // platform 选填
```

```
200 →
{
  "hits": [ { "word": "命中的违禁词", "start": 5, "end": 8 } ],   // 空时是 []
  "title_len": 28,          // 按字（rune）计，首尾空白不算
  "title_max": 30,          // 0 = 没传 platform，不校验字数
  "platform_name": "淘宝"
}
```

`platform` 可选值：`taobao` / `tmall` / `jd` / `pdd` / `douyin` / `amazon`。
传了不认识的值返回 400（错误信里带可选清单）。

#### GET /me/usage/summary —— 本月消费

```
200 →
{
  "period_start": "2026-08-01T00:00:00Z",
  "period_end":   "2026-09-01T00:00:00Z",     // 不含这一刻
  "image_count": 37,
  "currency": "USD",
  "cost": "37.00",
  "balance": "63.00",
  "available": "59.00",       // 余额 - 未结算批次冻结的部分
  "admin_contact": "…"        // 没配置时是空串
}
```

#### POST /me/quota-requests —— 申请额度

余额不足时通知管理员。请求体可整个不传，或 `{ "note": "≤200 字的说明" }`。

```
201 →
{
  "status": "pending",
  "note": null,
  "created_at": "…",
  "admin_contact": "…",
  "message": "已经把申请提交给管理员了…"
}
```

已有一条未处理的申请时返回 409（提示不用重复提交）。

### 9.8 「本批次新增」端点的部署确认

`DELETE /assets/:uid`、`DELETE /jobs/:uid` 两个端点随
2026-08-16 这一批代码上线。对接前先在目标环境各调一次确认已部署：
**未部署时返回裸 404**（响应体不是第 3 节的错误信封）——按第 3 节的约定，
这表示「端点不存在」，做好降级（跳过删除/自建词功能）即可，其余接口不受影响。

---

## 10. 状态机

任务（`job.status`）：

| 状态 | 含义 | 终态 |
|---|---|---|
| `created` | 已建单，额度已冻结，还没开始 | |
| `holding` | 排队等待进入生成 | |
| `running` | 生成中 | |
| `settling` | 图都出完了，正在结算（按实际张数多退） | |
| `succeeded` | 全部成功，已结算 | ✅ |
| `partially_failed` | 部分成功，已结算 | ✅ |
| `failed` | 全部失败，已结算 | ✅ |
| `cancelled` | 停止排队后收尾完成。**`success_count` 可能大于 0**（已开跑的照常出图计费） | ✅ |

单张（`item.status`）：`pending` → `running` → `succeeded` / `failed` /
`cancelled`。`failed` 的单张可在 `max_attempts` 内调 retry（重新收费）。

轮询终止条件：`job.status` 进入终态。取图条件：看各 item 的
`status == "succeeded"`（或 job 的 `success_count`），**不要看 job 状态推断**。

---

## 11. 回调通知（现状：字段已收、暂不投递）

`POST /jobs` 的 `callback_url` 当前版本的真实行为：

- **已接收**：格式校验（完整 http/https 网址、≤1024 字符，不合法 400）；
- **已入库**：查询任务时原样回显；
- **不投递**：任务完成时**不会向这个地址发起任何 HTTP 请求**。

所以现在**不要等回调**，一律按第 6 节轮询。字段现在就传是安全且推荐的——
投递功能上线后同一批老任务不用改造。届时本文档会补充投递格式、
签名与重试语义。

---

## 附录 A：错误码表

`error_code` 全集（也会出现在 `last_error_code` / item 的 `error_code` 里）。
message 是默认文案，部分场景会替换成带具体数字的版本（如「还差 $12.30」）。

| error_code | HTTP | 默认文案 |
|---|---|---|
| `DK_INSUFFICIENT_BALANCE` | 402 | 余额不足，这一批出不了。可以点「申请额度」通知管理员，或者少选几张再提交。 |
| `DK_QUOTA_EXHAUSTED` | 403 | 这个账号的用量额度已经用完了，请联系管理员调整额度。 |
| `DK_UPSTREAM_BUSY` | 429 | 出图通道正忙，这张排在后面了，稍等会自动重试。 |
| `DK_RATE_LIMITED` | 429 | 操作太频繁了，请等几秒再试。 |
| `DK_NO_AVAILABLE_ACCOUNT` | 503 | 暂时没有可用的出图账号，请稍后再试；一直这样就是账号出问题了，请联系管理员。 |
| `DK_IMAGE_NOT_ENABLED` | 403 | 当前账号所在的分组没有开通出图功能，请联系管理员在分组设置里打开图片生成。 |
| `DK_MODEL_NOT_ALLOWED` | 403 | 当前分组不允许使用这个出图模型，请联系管理员把模型加进白名单。 |
| `DK_IMAGES_API_UNSUPPORTED` | 404 | 当前分组绑定的账号类型不支持出图接口，请联系管理员换一个分组。 |
| `DK_UPSTREAM_BASE_URL_INVALID` | 502 | 出图账号的「中转地址」填得不对，连不上，请联系管理员检查账号配置。 |
| `DK_UPSTREAM_UNAUTHORIZED` | 502 | 出图账号的密钥无效或已过期，请联系管理员更新。 |
| `DK_CONTENT_BLOCKED` | 422 | 这条提示词或这张商品图被内容审核拦下了，换个说法或换张图再试。 |
| `DK_TIMEOUT` | 504 | 这张图可能已经生成并计费，但我们没收到结果。需要的话可以手动重试（会重新收费）。 |
| `DK_CANCELED` | 500 | 这张图的请求被中断了。 |
| `DK_UPSTREAM_ERROR` | 502 | 出图失败，请稍后重试；一直失败就把这一行的错误码发给管理员。 |
| `DK_STALE` | 500 | 系统重启导致这批任务中断了，可以重新提交。已经出好的图都在作品库里。 |
| `DK_INVALID_REQUEST` | 400 | 提交的内容有问题，请检查后重试。 |
| `DK_RATIO_NOT_ALLOWED` | 400 | 不支持这个出图比例，请从列表里选一个。 |
| `DK_BATCH_TOO_LARGE` | 400 | 一次提交的张数超过上限了，请少选几张商品图或提示词。 |
| `DK_IDEMPOTENCY_KEY_REQUIRED` | 400 | 提交请求缺少防重复标识，请刷新页面重试。 |
| `DK_IDEMPOTENCY_KEY_CONFLICT` | 409 | 这个提交标识刚才用过了，但内容不一样，请刷新页面重新提交。 |
| `DK_UNSUPPORTED_IMAGE_FORMAT` | 415 | 这个图片格式不支持，请上传 JPG、PNG、WEBP 或 HEIC。 |
| `DK_IMAGE_TOO_LARGE` | 413 | 图片太大了，请压到 20MB 以内再上传。 |
| `DK_MAX_ATTEMPTS_EXCEEDED` | 409 | 这张图已经重试到上限了，不能再试。想继续可以重新提交一批。 |
| `DK_JOB_ALREADY_SETTLED` | 409 | 这批任务已经结算，不能再重试了，请重新提交。 |
| `DK_ILLEGAL_STATE_TRANSITION` | 409 | 这个任务的状态已经变了，请刷新页面看最新进度。 |
| `DK_JOB_NOT_FOUND` | 404 | 找不到这个任务，可能已经被删除了。 |
| `DK_ITEM_NOT_FOUND` | 404 | 找不到这一张图的记录。 |
| `DK_ASSET_NOT_FOUND` | 404 | 找不到这张商品图，可能已经被删除了。 |
| `DK_IMAGE_NOT_FOUND` | 404 | 找不到这张结果图，可能已经被删除了。 |
| `DK_PROMPT_NOT_FOUND` | 404 | 找不到这条提示词，可能已经从灵感库下架了。 |
| `DK_CHAT_SESSION_NOT_FOUND` | 404 | 找不到这组对话，可能已经被删除了。 |
| `DK_UNAUTHORIZED` | 401 | 登录已过期，请重新登录。（ERP 场景的文案是「API Key 无效或已停用，请联系管理员核对。」等） |
| `DK_FORBIDDEN` | 403 | 没有权限做这个操作。 |
| `DK_API_KEY_MISSING` | 403 | 账号还没开通出图权限，请联系管理员。 |
| `DK_UPSCALE_UNAVAILABLE` | 503 | 放大功能还没准备好，请联系管理员。 |
| `DK_UPSCALE_QUEUE_FULL` | 429 | 排队满了，等会儿再试。 |
| `DK_UPSCALE_NOT_FOUND` | 404 | 没有这张图的放大任务，重新点「高清放大」。 |
| `DK_UPSCALE_FAILED` | 502 | 放大失败，重试一次。 |
| `DK_PREPROCESS_FAILED` | 502 | 商品图处理失败（补白边或格式转换），请换一张图再试。 |
| `DK_STORAGE_ERROR` | 500 | 图片存取失败，请稍后重试；一直失败请联系管理员。 |
| `DK_SYNC_IN_PROGRESS` | 409 | 灵感库正在同步，请等这一次跑完再点。 |
| `DK_INTERNAL` | 500 | 系统开小差了，请稍后重试；一直这样请联系管理员。 |

分支建议：402/`DK_INSUFFICIENT_BALANCE` 与 403/`DK_QUOTA_EXHAUSTED` 提示补额度；
429 按 `Retry-After` 退避；404 类不要重试；409 类刷新状态再决定；
5xx / `DK_TIMEOUT` 可重试，但 `DK_TIMEOUT` 的那张**可能已计费**，
重试前先查一次 item（图可能稍后就到了）。

---

## 附录 B：ERP 不该调的路径

以下路径只在浏览器前缀 `/api/v1/designkit` 下、且仅管理员可用，
在 `/v1/designkit` 下不存在（裸 404）：

- `POST /prompts/sync`、`GET /prompts/sync/latest`、`GET|PUT /prompts/sync/settings`
- `GET|PUT /admin/settings`

---

## 附录 C：《设计定型.md》幻影路径更正表

《设计定型.md》第三节的路径表里，以下 5 条曾长期「设计里有、代码里没有」。
**以本文档为准**，现状如下：

| 设计定型.md 里的路径 | 现状 |
|---|---|
| `DELETE /assets/:uid` | **本批次新增**，已收录（9.1 节） |
| `DELETE /jobs/:uid` | **本批次新增**，已收录（9.3 节） |
| `POST /assets/:uid/preprocess` | **不存在**，调用返回裸 404。补白边等预处理在出图流程里自动做，不单独提供接口，也没有提供的计划 |
| `POST /prompts`、`PUT /prompts/:uid`、`DELETE /prompts/:uid` | **仍未挂载**，调用返回裸 404（运营自建提示词，repository 已实现但没有路由）。ERP 要用自己的提示词，走 `POST /jobs` 的 `prompts` 数组传原文 |

另外两条设计文档没写、但实际存在的：`POST /assets/:uid/upscale` /
`GET /assets/:uid/upscale`（9.1 节）和 `POST /content/check`（9.7 节）。
