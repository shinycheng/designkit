# 端到端回归测试

每次改动代码后，跑一遍这两个脚本确认没把已有功能改坏。

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

## Provider 兼容回归

这组测试使用本地假上游，不会调用真实生图服务或产生费用：

```bash
.venv/bin/python -m unittest discover -s tests -p 'test_provider_compat.py' -v
```

- `test_provider_compat.py` — 覆盖 OpenAI Images 兼容网关拒绝多图 `n`
  参数时的单图拆分回退，并确保无关 400 不会误触发。

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
