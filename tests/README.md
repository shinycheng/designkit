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

- `e2e_core.py` — 核心流程 26 项：登录、上传、模板变量渲染、mock 生成、
  对外 API 提交/查询、webhook 回调签名、越权拦截等
- `e2e_security.py` — 安全与边界 27 项：首登强制改密、令牌撤销、SSRF 拦截、
  base64 边界、尺寸校验、配额原子扣减等。
  **注意**：它会把 admin 密码改成 `newpass8888`，所以要在重置数据后运行，跑完再重置

全部输出 PASS 即为通过。
