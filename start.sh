#!/bin/bash
# DesignKit 启动脚本：在终端里运行  ./start.sh
set -e
cd "$(dirname "$0")" || exit 1

# 1. 检查 python3
if ! command -v python3 >/dev/null 2>&1; then
  echo "❌ 未找到 python3。请先安装 Python 3.9 及以上版本（https://www.python.org/downloads/）。"
  exit 1
fi

# 2. 检查版本 >= 3.9
if ! python3 -c 'import sys; sys.exit(0 if sys.version_info >= (3, 9) else 1)'; then
  echo "❌ Python 版本过低，需要 3.9 及以上。当前版本："
  python3 --version
  exit 1
fi

# 3. 安装依赖（用 marker 文件判断是否装成功，避免半装状态被误判为已装）
if [ ! -x .venv/bin/uvicorn ] || [ ! -f .venv/.installed ]; then
  echo "首次运行，正在创建 Python 环境并安装依赖（约 1-2 分钟）…"
  python3 -m venv .venv
  .venv/bin/pip install --upgrade pip
  .venv/bin/pip install -r backend/requirements.txt
  touch .venv/.installed
fi

# 4. 读取 .env 里的端口/主机（若存在）
HOST="127.0.0.1"
PORT="8787"
if [ -f .env ]; then
  set -a; . ./.env; set +a
  HOST="${DESIGNKIT_HOST:-$HOST}"
  PORT="${DESIGNKIT_PORT:-$PORT}"
fi

echo ""
echo "  DesignKit 正在启动…"
echo "  启动后请用浏览器打开： http://${HOST}:${PORT}"
echo "  初始账号 admin / admin123456（首次登录会要求你修改密码）"
echo "  按 Ctrl+C 停止服务"
echo ""
exec .venv/bin/uvicorn backend.app.main:app --host "$HOST" --port "$PORT"
