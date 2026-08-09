# 回归测试

每次改动代码后，跑一遍**全部 6 组**测试确认没把已有功能改坏，共 126 项：

| 组 | 文件 | 项数 | 要不要起服务 |
|---|---|---|---|
| 端到端·核心流程 | `e2e_core.py` | 26 | 要 |
| 端到端·安全边界 | `e2e_security.py` | 27 | 要 |
| Provider 兼容与报错可读性 | `test_provider_compat.py` | 25 | 不要 |
| 灵感库与预处理 | `test_inspiration_convert.py` | 17 | 不要 |
| 跨库兼容与定时同步 | `test_db_and_scheduler.py` | 14 | 不要 |
| 出图比例护栏与画幅措辞 | `test_sizing.py` | 17 | 不要 |

后四组是纯单元测试，不联网、不花钱、不用起服务，也不用重置数据。

## 端到端测试（前两组）

**前提**：先把数据重置为全新状态并启动服务（测试依赖初始账号 admin/admin123456）：

```bash
rm -rf data && ./start.sh
```

另开一个终端窗口运行：

```bash
.venv/bin/python tests/e2e_core.py
```

```bash
.venv/bin/python tests/e2e_security.py
```

- `e2e_core.py` — 核心流程 26 项：登录、上传、模板变量渲染、mock 生成、
  对外 API 提交/查询、webhook 回调签名、越权拦截等
- `e2e_security.py` — 安全与边界 27 项：首登强制改密、令牌撤销、SSRF 拦截、
  base64 边界、尺寸校验、配额原子扣减等。
  **注意**：它会把 admin 密码改成 `newpass8888`，所以要在重置数据后运行，跑完再重置

全部输出 PASS 即为通过。

> 两个 e2e 脚本里的服务地址写死为 `http://127.0.0.1:8787`（`BASE` 常量）。
> 想跑在别的端口，改这个常量即可。
>
> **不想动本机数据**（比如已经填了网关 Key、有历史生成记录）时，可以用独立数据目录
> 另起一个测试服务，再把脚本里的 `BASE` 指过去：
>
> ```bash
> DESIGNKIT_DATA_DIR=/tmp/dk-test DESIGNKIT_PROVIDER=mock \
>   .venv/bin/uvicorn backend.app.main:app --host 127.0.0.1 --port 8788
> ```

## Provider 兼容回归

这组测试使用本地假上游，不会调用真实生图服务或产生费用：

```bash
.venv/bin/python -m unittest discover -s tests -p 'test_provider_compat.py' -v
```

- `test_provider_compat.py` — 覆盖三类问题：①网关拒绝多图 `n` 时的单图拆分回退
  （并确保无关 400 不会误触发）；②「HTTP 200 但业务失败」的 `{code,msg}` 信封、
  `images`/`results` 等非标准字段名、HTML 错误页的识别；③API 地址结尾拼接
  （自带 `/api/v3` 的网关不能被拼成 `/api/v3/v1/...`）。

## 灵感库与预处理单测

```bash
.venv/bin/python -m unittest discover -s tests -p 'test_inspiration_convert.py' -v
```

- `test_inspiration_convert.py` — YouMind `{argument}` 变量语法转换（同名复用/撞名加序号/中文变量）
  与输入图预处理（比例补边、透明合白底、坏图回退），不联网、不花钱。

## 跨库兼容与定时同步单测

```bash
.venv/bin/python -m unittest discover -s tests -p 'test_db_and_scheduler.py' -v
```

- `test_db_and_scheduler.py` — 用 PostgreSQL 方言静态编译全部建表与运行期 SQL
  （本机无需装 PG），并验证搜索大小写不敏感、调度器到期判断与失败退避。

## 出图比例护栏与画幅措辞单测

```bash
.venv/bin/python -m unittest discover -s tests -p 'test_sizing.py' -v
```

- `test_sizing.py` — 校验尺寸护栏（16 的倍数、边长与总像素上下限、长短比 ≤3）、
  比例反算、以及提示词里的画幅措辞是否与实际尺寸一致。
  还验证 `enforce_aspect` / `enforce_transparent_background` 是**幂等**的——
  补图任务会把已加工过的提示词再跑一遍，不幂等就会越叠越长。
