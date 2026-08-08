# DesignKit 对外 API 对接文档（ERP / 第三方系统）

版本：v1　|　基础地址：`http://<平台地址>:8787`（下文以 `BASE` 代称）

## 1. 鉴权

所有接口都需要在请求头携带 API Key（由平台管理员在「API 对接」页面创建后提供）：

```
X-API-Key: dk_xxxxxxxxxxxxxxxx
```

- Key 无效或停用 → `401`
- 当月额度用完（若设置了额度）→ `429`

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

### 3.1 获取提示词模板列表

```
GET BASE/api/v1/templates
```

返回模板数组，关键字段：

| 字段 | 说明 |
|---|---|
| `id` | 提交任务时用的 `template_id` |
| `name` / `description` | 模板名称、说明 |
| `variables` | 模板变量定义数组：`name`(提交时的键)、`label`、`type`(text/select)、`options`、`default`、`required` |
| `requires_input_image` | 是否必须提供商品图 |
| `default_params` | 默认的 `size` / `n` / `quality` |

### 3.2 上传商品图（可选）

三种给图方式任选其一：预先上传拿 `upload_id`、直接给公网 `image_urls`、或给 `images_base64`。

```
POST BASE/api/v1/uploads
Content-Type: multipart/form-data
file: <图片文件，png/jpg/webp，≤20MB>

→ 200 {"id": 15, "url": "...", "width": 800, "height": 800, ...}
```

### 3.3 提交生成任务

```
POST BASE/api/v1/generations
Content-Type: application/json
```

请求体（`template_id` 与 `prompt` 二选一，其余可选）：

```json
{
  "template_id": 2,
  "variables": {"scene": "现代客厅的木质茶几上", "style": "高级质感"},
  "extra_instructions": "整体偏暖色调",
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
| `template_id` | 模板 id；不用模板时改传 `prompt`（自定义提示词） |
| `variables` | 模板变量取值，键为模板 `variables[].name` |
| `image_urls` / `upload_ids` / `images_base64` | 商品图，三者可混用，合计最多 4 张。`image_urls` 必须是直链、不跟随重定向；平台默认允许内网地址（方便内部图片服务器），但云元数据等危险地址始终被拒绝。若平台在「系统设置」关闭了内网访问，则内网图片请改用上传接口或 base64 |
| `n` | 生成张数 1-4，默认 1 |
| `size` | `1024x1024` / `1536x1024` / `1024x1536` |
| `quality` | `auto` / `low` / `medium` / `high` |
| `callback_url` | 任务完成后平台回调此地址（http/https） |
| `external_ref` | 你方业务单号，查询与回调时原样带回，便于对账 |

响应 `202`：

```json
{"job_id": "414ce2f0a331445e...", "status": "pending", "external_ref": "你的单号ABC123", ...}
```

错误：`422` 参数问题（`detail` 里有中文原因）、`404` 模板不存在、`429` 额度用完。

### 3.4 查询任务

```
GET BASE/api/v1/generations/{job_id}
```

```json
{
  "job_id": "414c...",
  "status": "succeeded",          // pending 排队 / processing 生成中 / succeeded 成功 / failed 失败
  "error": "",                    // 失败时的中文原因
  "external_ref": "你的单号ABC123",
  "images": [
    {"url": "http://.../files/outputs/202608/414c.../0.png",
     "thumbnail_url": "http://.../files/thumbnails/...jpg",
     "width": 1024, "height": 1024, "format": "png"}
  ],
  "created_at": "...", "finished_at": "..."
}
```

建议轮询间隔 3~5 秒；真实生图一般 20 秒 ~ 2 分钟。图片 URL 可直接下载保存到你方系统。

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

- 你方需在 15 秒内返回 2xx，否则平台按 8 秒、30 秒间隔共重试 3 次
- 回调失败不影响任务本身，仍可通过查询接口取结果

## 5. 在线调试

浏览器打开 `BASE/docs`，展开「对外API-ERP对接」分组，填入 X-API-Key 即可在线调用。
