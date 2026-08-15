# 图像预处理服务（designkit-imgsvc）

一个只做一件事的小服务：**把运营上传的商品图，变成能直接发给生图网关的样子。**

具体做五件事：

| 做什么 | 为什么非做不可 |
|---|---|
| 按目标比例**补白边**（不裁产品） | 网关**忽略 size 参数，出图比例完全跟随输入图**。想要 3:4 的图，唯一办法就是先把输入图变成 3:4 |
| HEIC/HEIF **转码** | iPhone 拍的照片默认就是 HEIC，Mac 导出时还常常顶着 `.jpg` 的扩展名，不转码网关直接报「认不出格式」 |
| 按 EXIF **摆正方向** | 手机竖拍的 JPEG 是横着存、靠一个标记说明方向的。不摆正就重编码，发出去的是横躺的商品图 |
| 透明底**合成白底** | 带透明通道的 PNG 直接转 RGB，透明区会变成黑块 |
| 限制**最长边** | 直接决定出图落哪个计费档（见下面「⚠ 关于钱」） |

它**不连数据库、不碰对象存储、不认识用户**。图片字节进，图片字节出。

---

## ⚠ 两条最重要的规矩

### 一、出错就是出错，绝不「凑合着发出去」

老版本（`legacy-python` 分支的 `prepare_input_image`）用一个大 `try` 把整个流程
兜住，出任何异常就**返回原始图片字节**、日志里打一行 warning 完事。

后果是：补边失败 → 网关拿到的还是原比例的图 → **运营选了 3:4，拿回来的是 4:3，
钱一分不少地扣了**，而且没有任何人会注意到。

这个版本**没有这种兜底**。任何一步出问题，一律返回 4xx/5xx 错误码，
Go 侧把这一张标成失败。宁可这张不出，也不出一张比例错了的图。

### 二、`ratio` 只认 `3:4` 这种比例串

老版本的 `parse_ratio` 只认 `1536x1024` 这种**像素尺寸**串，传 `3:4` 进去它会
静静地返回「不用处理」，结果是**什么边都没补**——跟上面是同一类事故。

这个版本用的是新写的 `parse_aspect`，只认 `宽:高`。传 `1536x1024` 会当场返回
`400 invalid_ratio`，绝不静默放行。

---

## ⚠ 关于钱：`max_dimension` 决定计费档

网关按**实际出图的最长边**分档计价：

| 最长边 | 档位 |
|---|---|
| ≤ 1024 | 1K |
| ≤ 2048 | 2K |
| 更大 | 4K |

`max_dimension` 是**每次请求都可以显式传**的参数（默认 2048，即 2K 档），
不是藏在代码里的常量。改它就是改钱，所以它必须一眼可见。

> 注意：这只影响**我们发出去的输入图**。网关最终返回多大的图是它自己决定的
> （实测过请求 1:1、拿回 1254×1254 的情况），真实档位以出图像素为准。

---

## 接口

服务监听容器内 **8000** 端口，**不映射到宿主机**——只有同一个 docker 网络里的
sub2api 容器能访问它。

### `GET /healthz`

任何时候都返回 200，能不能干活看 `status` 字段。

```json
{
  "status": "ok",
  "service": "designkit-imgsvc",
  "version": "1.0.0",
  "pillow":      {"available": true, "version": "12.3.0"},
  "pillow_heif": {"available": true, "version": "1.4.0"},
  "heif_supported": true,
  "auth_required": true,
  "defaults": {
    "max_dimension": 2048,
    "max_dimension_min": 256,
    "max_dimension_max": 8192,
    "max_upload_bytes": 20971520,
    "max_pixels": 80000000,
    "max_concurrency": 4
  },
  "supported_ratios": ["1:1", "3:4", "4:3", "16:9", "9:16"]
}
```

`status` 只有两个值：`ok`（一切正常）、`degraded`（HEIC 解码器没装上，
JPG/PNG 照常处理，但 iPhone 的 HEIC 照片会被拒）。

**为什么不返 503**：容器的 HEALTHCHECK 只看 HTTP 状态码。没装 HEIC 解码器
的时候服务其实还能干大部分活，返 503 会让编排系统反复重启它，反而更糟。

### `POST /v1/preprocess`

`multipart/form-data`，四个字段：

| 字段 | 必填 | 默认 | 说明 |
|---|---|---|---|
| `file` | 是 | — | 原图。默认上限 20MB |
| `ratio` | 否 | 空 | 目标比例，如 `3:4`。**留空 / `auto` = 明确不补边** |
| `keep_transparency` | 否 | `false` | `true` 时保留透明通道，补的也是透明边 |
| `max_dimension` | 否 | `2048` | 输出最长边上限，允许 256~8192 |

**成功返回图片字节本身**（`Content-Type` 是 `image/png` 或 `image/jpeg`），
不是包了一层 JSON 的 base64——一张 2K 图 base64 之后要多占三分之一的内存和带宽，
拿到还得再解一次。元数据全在响应头里：

| 响应头 | 例 | 说明 |
|---|---|---|
| `X-Dk-Changed` | `true` | `false` 表示这张图什么都不用改，返回的就是**原始字节** |
| `X-Dk-Width` / `X-Dk-Height` | `1536` / `2048` | 输出图的宽高 |
| `X-Dk-Actions` | `WyLlt7Ii4uLl0=` | 做了哪些处理，**中文，JSON 数组，base64 编码** |
| `X-Dk-Action-Codes` | `padded,downscaled` | 同一批动作的 ASCII 机器码，逗号分隔，不用解码 |
| `X-Dk-Source-Width` / `X-Dk-Source-Height` | `800` / `1200` | 输入图（按 EXIF 摆正之后）的宽高 |
| `X-Dk-Source-Format` | `jpeg` | 输入图的格式 |
| `X-Dk-Format` / `X-Dk-Suffix` | `jpeg` / `.jpg` | 输出格式与建议的文件后缀 |
| `X-Dk-Bytes` | `348735` | 输出字节数 |
| `X-Dk-Ratio` | `3:4` 或 `auto` | 本次实际生效的比例 |
| `X-Dk-Max-Dimension` | `2048` | 本次实际生效的最长边上限 |

**为什么中文要 base64**：HTTP 头按标准只保证 ASCII/latin-1，中文直接塞进去在
不同的服务器和客户端上表现不一样（有的报错、有的乱码）。base64 之后全是 ASCII，
零歧义。Go 侧这样解：

```go
raw, _ := base64.StdEncoding.DecodeString(resp.Header.Get("X-Dk-Actions"))
var notes []string
_ = json.Unmarshal(raw, &notes)   // ["已补白边到 3:4 比例", "已缩小到最长边 2048"]
```

不想解码就直接用 `X-Dk-Action-Codes`，它是 ASCII 的：
`exif_rotated` / `alpha_flattened` / `alpha_kept` / `padded` / `downscaled` / `transcoded`。

#### 动作码含义

| 码 | 中文 | 什么时候出现 |
|---|---|---|
| `exif_rotated` | 已按拍摄方向摆正 | 输入图带 EXIF Orientation 且不是 1 |
| `alpha_flattened` | 透明底已合成白底 | 有透明通道且 `keep_transparency=false` |
| `alpha_kept` | 已保留透明底 | 有透明通道且 `keep_transparency=true` |
| `padded` | 已补白/透明边到 X:Y 比例 | 输入比例与目标比例不一致 |
| `downscaled` | 已缩小到最长边 N | 结果超过 `max_dimension` |
| `transcoded` | 已从 heif 转码 | 输入不是 png/jpeg/webp |

### 出错时

一律 JSON，**只有一种格式**：

```json
{"error": {"code": "invalid_ratio", "message": "比例格式不对：'1536x1024'。请用「宽:高」的写法……"}}
```

| HTTP | `code` | 什么情况 |
|---|---|---|
| 400 | `invalid_request` | 少字段、字段名写错 |
| 400 | `invalid_ratio` | `ratio` 不是 `宽:高`（包括传了老的像素串），或比例过于极端 |
| 400 | `invalid_keep_transparency` | 不是 true/false |
| 400 | `invalid_max_dimension` | 不是整数，或不在 256~8192 之间 |
| 400 | `empty_file` | 上传的是空文件 |
| 401 | `unauthorized` | 配了 token 但没带 / 带错 |
| 413 | `file_too_large` | 上传超过 20MB |
| 413 | `image_too_large` | 解出来的像素数超过 8000 万（防「1MB 的 PNG 解出来 1.2 亿像素」） |
| 415 | `unsupported_media_type` | 认不出格式，或文件损坏 |
| 422 | `heif_unsupported` | 是 HEIC/HEIF，但**服务端没装 pillow-heif**。这是部署问题，不是用户的图有问题 |
| 500 | `internal_error` | 处理过程中出了意外。**这张图没有被发送出去** |
| 503 | `busy` | 并发排满了。可以退避重试 |

Go 侧的处理原则：**只要不是 200，这一张就是失败**，不要拿原图凑合着发。
只对连接失败和 502/503/504 重试，4xx 不重试。

---

## 配置（全部走环境变量）

| 变量 | 默认 | 说明 |
|---|---|---|
| `DESIGNKIT_IMGSVC_TOKEN` | 空 | 非空时校验 `Authorization: Bearer <值>`。空 = 不校验 |
| `DESIGNKIT_IMGSVC_DEFAULT_MAX_DIMENSION` | `2048` | ⚠ 决定计费档 |
| `DESIGNKIT_IMGSVC_MIN_MAX_DIMENSION` | `256` | 请求里 `max_dimension` 的下限 |
| `DESIGNKIT_IMGSVC_MAX_MAX_DIMENSION` | `8192` | 请求里 `max_dimension` 的上限 |
| `DESIGNKIT_IMGSVC_MAX_UPLOAD_BYTES` | `20971520`（20MB） | 单张上传上限，跟界面提示保持一致 |
| `DESIGNKIT_IMGSVC_MAX_PIXELS` | `80000000` | 解码前的像素数上限 |
| `DESIGNKIT_IMGSVC_MAX_CONCURRENCY` | `4` | 同时处理几张。群晖内存小就调到 2 |
| `DESIGNKIT_IMGSVC_QUEUE_WAIT_SECONDS` | `20` | 排队等这么久还轮不到就返 503 |
| `DESIGNKIT_IMGSVC_PORT` | `8000` | 监听端口（一般不用改） |
| `DESIGNKIT_IMGSVC_WORKERS` | `1` | uvicorn 进程数。内存是按进程算的，调大之前先看 `MAX_CONCURRENCY` |

后端（Go 侧）另外读三个变量找它，配在 sub2api 容器上：
`DESIGNKIT_IMGSVC_URL`（默认 `http://designkit-imgsvc:8000`）、
`DESIGNKIT_IMGSVC_TOKEN`、`DESIGNKIT_IMGSVC_TIMEOUT_SECONDS`（默认 60）。

---

## 跟开发环境一起起（推荐）

不用单独做什么，它已经接进开发编排了：

```bash
bash designkit/bin/dk-dev.sh
```

第一次会构建这个容器（大约一两分钟）。之后：

```bash
cd deploy

# 看它有没有起来
docker compose -f docker-compose.dev.yml -f docker-compose.designkit-dev.yml ps designkit-imgsvc

# 看日志
docker compose -f docker-compose.dev.yml -f docker-compose.designkit-dev.yml logs -f designkit-imgsvc

# 从 sub2api 容器里探活（它没有对外端口，只能从容器网络里访问）
docker compose -f docker-compose.dev.yml -f docker-compose.designkit-dev.yml \
  exec sub2api wget -qO- http://designkit-imgsvc:8000/healthz

# 改了 Python 代码之后重新构建
docker compose -f docker-compose.dev.yml -f docker-compose.designkit-dev.yml \
  up -d --build designkit-imgsvc
```

> **⚠ 代理的坑**：开发编排给 sub2api 配了走宿主机的 HTTP 代理（国内装包用）。
> `NO_PROXY` 名单必须包含 `designkit-imgsvc` 这个**主机名**——Go 的代理判断是按
> 主机名匹配的，`172.16.0.0/12` 这类网段规则命不中它。
> 这一条已经写在 `deploy/docker-compose.designkit-dev.yml` 里了。
> 但如果你在 `.env` 里自己写了 `SUB2API_DEV_NO_PROXY`，**记得把
> `designkit-imgsvc` 加进你那份名单**，否则后端调它会先去连代理然后失败，
> 现象是「服务明明起着，出图却报连不上」。

---

## 单独起（调试用）

### 用 Docker

```bash
cd designkit/imgsvc
docker build -t designkit-imgsvc:dev .

# 这里为了方便调试才映射端口；正式编排里是不映射的
docker run --rm -p 8099:8000 \
  -e DESIGNKIT_IMGSVC_TOKEN=dk-local-token \
  designkit-imgsvc:dev

curl -s http://127.0.0.1:8099/healthz
```

### 不用 Docker（本机有 Python 3.10+ 就行）

```bash
cd designkit/imgsvc
python3 -m venv .venv
.venv/bin/pip install -r requirements.txt
.venv/bin/uvicorn app.main:app --host 127.0.0.1 --port 8099
```

浏览器打开 <http://127.0.0.1:8099/docs> 可以直接上传图片试。

---

## 怎么测

自带一个自检脚本，**不需要 pytest、不联网、不花一分钱**。它会自己用 Pillow
造几张真图（正常 JPG、带透明的 PNG、超大图、竖拍带 EXIF 的 JPG、坏文件），
然后逐条验证结果。

```bash
cd designkit/imgsvc

# 只跑不需要服务的那一段（比例计算、参数解析、fail-closed）
.venv/bin/python selfcheck.py --local-only

# 完整跑：先起服务，再另开一个终端跑脚本
DESIGNKIT_IMGSVC_TOKEN=dk-local-token .venv/bin/uvicorn app.main:app --port 8099
# ↓ 另一个终端
DESIGNKIT_IMGSVC_TOKEN=dk-local-token DK_SELFCHECK_BASE=http://127.0.0.1:8099 \
  .venv/bin/python selfcheck.py
```

全通过时最后一行是 `全部通过：87 项。`，任何一条挂了都会列出来并以非 0 退出码结束。

它覆盖的东西：

- `parse_aspect` 把 `1536x1024`、`3/4`、`0:4` 这些一律当错误（**不许静默不补边**）
- 五个比例（1:1 / 3:4 / 4:3 / 16:9 / 9:16）补边后的宽高**逐个对数字**，
  并断言是精确整数比、最长边不超过 `max_dimension`
- `max_dimension` 传 1024 / 4096 / `abc` 分别得到什么
- 透明底：默认合成白底（角落是纯白）；`keep_transparency=true` 时角落 alpha=0
- 5000×3000 的大图被缩到 2048 以内
- 什么都不用改的图**原样直传**（返回的字节与上传的一模一样，没被重编码撑大）
- 竖拍 EXIF 照片被摆正（源尺寸从 1200×800 变成 800×1200）
- fail-closed：出错时响应一定是 JSON，**永远不是原图字节**
- HEIC 分支、鉴权、空文件、缺字段

手工试一下也很简单：

```bash
curl -sS -D- -o out.png \
  -H "Authorization: Bearer dk-local-token" \
  -F "file=@商品图.jpg" \
  -F "ratio=3:4" \
  -F "max_dimension=2048" \
  http://127.0.0.1:8099/v1/preprocess

# 看看头里说做了什么（中文那条要解一次 base64）
```

---

## 依赖为什么要钉死到这个程度

`requirements.txt` 第一行是 `--only-binary=:all:`，意思是**只装官方预编译好的
包，不允许下载源码现场编译**。

原因：群晖 NAS 用的是 arm64，而 arm64 镜像在 GitHub Actions 上是靠 QEMU
**模拟**出来的。一旦某个依赖需要现场编译，轻则慢几十倍，重则直接失败——
而且这种失败**只在 GitHub 的构建机上出现，本机 Mac 永远复现不了**，极难排查。
加上这一行以后，缺轮子会立刻报「找不到符合条件的版本」并停下。

已经逐个确认过（用
`pip install --dry-run --only-binary=:all: --python-version 3.12 --abi cp312 --platform manylinux_2_28_aarch64 ...`
实际解析）：清单里全部 16 个包在 **aarch64 和 x86_64 上都有 cp312 的预编译轮子**，
包括两个带原生代码的大件：

- `pillow-heif==1.4.0` → `manylinux_2_26_aarch64.manylinux_2_28_aarch64`
- `pydantic-core==2.46.4` → `manylinux_2_17_aarch64.manylinux2014_aarch64`

所以间接依赖也全部钉死了：不钉的话，哪天 `pydantic-core` 发新版而 arm64 轮子
还没跟上，NAS 镜像构建就会当场失败，而那天我们大概率正在改别的东西。

**升 Python 版本前必须先查轮子。** 换基础镜像（比如 3.12 → 3.13）等于换 ABI 标签，
上面那条 `pip --dry-run` 命令要把 `--abi cp312` 换成新的再跑一遍。

**万一哪天 pillow-heif 真的没有 arm64 轮子了**，降级方案是：把
`requirements.txt` 里的 `pillow-heif` 那行删掉。服务仍然能起、能处理
JPG/PNG/WEBP，`/healthz` 会报 `"status": "degraded"`，HEIC 照片会得到
`422 heif_unsupported`（前端据此提示运营「请在 iPhone 上把格式改成兼容模式，
或先导出成 JPG」）。**不要**为了它去装编译工具链——那会让 arm64 构建从
两分钟变成一小时以上。

---

## 目录结构

```
designkit/imgsvc/
├── app/
│   ├── __init__.py     服务名和版本号
│   ├── errors.py       错误类型（HTTP 状态码 + 机器码 + 中文说明）
│   ├── imaging.py      核心：比例计算、补边、透明合白、EXIF 摆正、编码
│   └── main.py         HTTP 层：路由、鉴权、参数解析、响应头
├── Dockerfile
├── requirements.txt
├── selfcheck.py        自检脚本
└── README.md           就是这份
```

> 目录名故意避开了 `scripts`、`tests`、`docs` 这三个词：上游 `.gitignore` 里
> 这几条规则不带斜杠，会在任意层级命中同名目录，把文件悄悄吞掉——不报错、
> 不提示，等 CI 找不到文件时才发现。新建文件后可以用
> `git check-ignore -v <文件>` 确认，**没有输出才说明能提交**。
