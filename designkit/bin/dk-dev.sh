#!/usr/bin/env bash
# 一条命令起本机开发环境：PostgreSQL + Redis + 后端 + 前端热更新。
# 本机不需要装 Node、pnpm、Go 或 PostgreSQL，全在容器里。
#
#   designkit/bin/dk-dev.sh           启动（前台，Ctrl-C 停止）
#   designkit/bin/dk-dev.sh up -d     后台启动
#   designkit/bin/dk-dev.sh down      停止并删除容器（数据保留在 deploy/postgres_data）
#   designkit/bin/dk-dev.sh logs -f   看日志
#
# 起来之后：
#   http://localhost:3000   前端（改代码秒级刷新）← 开发看这个
#   http://localhost:8080   后端 API

set -euo pipefail
cd "$(git rev-parse --show-toplevel)/deploy"

# 首次运行时生成 .env：密码随机，不写死在文件里
if [ ! -f .env ]; then
  echo "首次运行，正在生成 deploy/.env …"
  {
    echo "# 本机开发用的随机密码，不要用到线上"
    echo "POSTGRES_PASSWORD=$(head -c 24 /dev/urandom | base64 | tr -d '/+=' | head -c 24)"
    echo "JWT_SECRET=$(head -c 48 /dev/urandom | base64 | tr -d '/+=' | head -c 48)"
    echo "ADMIN_EMAIL=admin@designkit.local"
    echo "# 管理员密码留空的话，容器启动日志里会打印一个随机初始密码"
    echo "ADMIN_PASSWORD="
    echo "# 绑定地址是两个变量（2026-08-23 拆分，旧的 BIND_HOST 已不再被读取）："
    echo "# SERVER_BIND 管后端入口，DEV_BIND 管 vite 开发服务器。本机开发都留回环。"
    echo "SERVER_BIND=127.0.0.1"
    echo "DEV_BIND=127.0.0.1"
  } > .env
  echo "已生成 deploy/.env（已被 .gitignore 忽略，不会提交）"
  echo
fi

# 没带参数就默认 up。注意不能写成 "${@:-up}" —— 那是标量展开，
# 参数超过一个时会被拼成一个字符串，docker compose 会认不出来。
[ $# -eq 0 ] && set -- up

exec docker compose \
  -f docker-compose.dev.yml \
  -f docker-compose.designkit-dev.yml \
  "$@"
