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
# 谁可以自称反向代理：只有 TCP 连接的对端在这份名单里时，uvicorn 才会相信请求里的
# X-Forwarded-For / X-Forwarded-Proto。默认 127.0.0.1 = 只信本机。
# 本机开发一般用不到；在服务器上裸跑（不用 Docker）并且前面套了 Nginx/Caddy 时，
# 要在 .env 里把它改成**反代自己的地址**。
# ⚠ 永远不要填 `*`：那会让 uvicorn 无条件相信这个头并取最左边那一段，
#   而最左边那一段是客户端自己随手写的，等于把限速直接关掉。
# 它和设置里的「信任几层反向代理」分工不同、两个都要配，详见 Dockerfile 里的长注释。
# 写成 ${...:-默认值}：这样临时试一下也可以直接
#   DESIGNKIT_FORWARDED_ALLOW_IPS=172.16.0.0/12 ./start.sh
# 不用非得写进 .env（.env 里填了的话，下面重读一次，以 .env 为准）。
FORWARDED_ALLOW_IPS="${DESIGNKIT_FORWARDED_ALLOW_IPS:-127.0.0.1}"
# 开几个进程。默认 1，和以前完全一样。
#
# ⚠ 本机这条路（start.sh）**请保持 1**，两个原因：
#   ① 本机用的是 SQLite，而「启动时建表 / 补列」那把进程锁是 PostgreSQL 的咨询锁，
#      SQLite 上直接跳过（见 backend/app/migrations.py 里 _acquire_process_lock 的说明）。
#      实测填 4：首次启动时会有 3 个进程以 `table users already exists` 崩掉、
#      再被自动拉起来才好——服务最终能用，但日志里先糊一屏红色 Traceback，
#      非技术用户看到这个只会以为装坏了。
#   ② 生成 worker 的线程池是**每个进程各建一个**的，开 N 个进程 =
#      设置页上「同时生成几张图」那个数 × N，而界面上显示的还是原来那个数。
# 真要开多进程，是在服务器上（PostgreSQL + Docker）用 DESIGNKIT_WORKERS，
# 详见 Dockerfile 和 docker-compose.yml 里的说明。
WORKERS="${DESIGNKIT_WORKERS:-1}"
if [ -f .env ]; then
  set -a; . ./.env; set +a
  HOST="${DESIGNKIT_HOST:-$HOST}"
  PORT="${DESIGNKIT_PORT:-$PORT}"
  FORWARDED_ALLOW_IPS="${DESIGNKIT_FORWARDED_ALLOW_IPS:-$FORWARDED_ALLOW_IPS}"
  WORKERS="${DESIGNKIT_WORKERS:-$WORKERS}"
fi

echo ""
echo "  DesignKit 正在启动…"
echo "  启动后请用浏览器打开： http://${HOST}:${PORT}"
echo "  初始账号 admin / admin123456（首次登录会要求你修改密码）"
echo "  按 Ctrl+C 停止服务"
echo ""
# --proxy-headers 是 uvicorn 的默认值，显式写出来是为了让读到这一行的人知道
# 「代理头是认的，认到什么程度由 --forwarded-allow-ips 那份名单决定」，
# 也免得将来上游改默认值时我们在某次升级后突然全站限速失效。
exec .venv/bin/uvicorn backend.app.main:app \
  --host "$HOST" --port "$PORT" \
  --proxy-headers --forwarded-allow-ips "$FORWARDED_ALLOW_IPS" \
  --workers "$WORKERS"
