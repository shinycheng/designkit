# 与本地开发一致，使用 Python 3.9，避免两套环境依赖版本漂移
FROM python:3.9-slim

WORKDIR /app

# gosu 用于在入口脚本里安全降权（比 su/sudo 更适合容器）
RUN apt-get update \
    && apt-get install -y --no-install-recommends gosu \
    && rm -rf /var/lib/apt/lists/*

COPY backend/requirements.txt backend/requirements.txt
RUN pip install --no-cache-dir -r backend/requirements.txt

COPY backend backend
COPY frontend frontend
COPY docker-entrypoint.sh /usr/local/bin/docker-entrypoint.sh
RUN chmod +x /usr/local/bin/docker-entrypoint.sh && mkdir -p /app/data

# ══════════════════════════════════════════════════════════════════════
#  下面 ENV 里的后两项，是「不填时用什么」——在 .env 里填了就以 .env 为准
# ══════════════════════════════════════════════════════════════════════
#
# 【DESIGNKIT_FORWARDED_ALLOW_IPS：谁可以自称反向代理】
# uvicorn 只有在**TCP 连接的对端**在这份名单里时，才会相信请求里的
# X-Forwarded-For / X-Forwarded-Proto 两个头。默认 127.0.0.1 = 只信本机，
# 也就是「谁都冒充不了反代」——这与不配它的今天行为完全一样，升级镜像不改变
# 任何现有部署。
#
# 套了反代（Nginx / Caddy / 群晖自带反向代理）要把它改成**反代自己的地址**
# （不是访客的地址）。常见的两种写法：反代和本容器在同一台机器上跑 Docker →
# 填 Docker 网桥网段 172.16.0.0/12；反代在另一台机器 → 填那台机器的内网 IP。
# 多个地址用英文逗号隔开，也支持网段写法。
#
# 不配它、只让反代转发的后果（这正是这次要解决的问题）：
#   ① 所有请求在后端看来都来自反代自己 → 限速全站共用一个桶，
#      一个人试错把大家一起锁在门外；
#   ② request.url.scheme 永远是 http → 明明整站跑在 https 上，
#      发出去的登录 / 取图 Cookie 却可能不带 Secure 标记。
#
# ⚠ **永远不要填 `*`**。填了之后 uvicorn 会无条件相信这个头，并且取**最左边**
#   那一段——而最左边那一段正是客户端自己随手写进请求里的，谁都能伪造。
#   后果：限速按伪造的 IP 算（等于没有限速），日志里记的来源 IP 也全是假的。
#   这与 backend/app/deps.py 里 client_ip 的说明是同一件事的两面。
#
# 【它和设置里的「信任几层反向代理」(DESIGNKIT_TRUSTED_PROXY_HOPS) 的分工】
# 两个都要配，各管各的，不会打架：
#   本项（地址名单）→ 管 uvicorn：决定「要不要理会代理头」。理会之后它会把
#       本次请求的协议改成 https、把连接对端改写成真实访客 IP。
#   HOPS（层数）    → 管应用自己的限速：决定从 X-Forwarded-For 里**从右往左数
#       第几段**才是真实访客（见 deps.py 的 client_ip）。
# 换句话说：一个回答「信不信」，一个回答「信到第几段」。
# 只配前者：限速也是对的（应用拿到的连接对端已经被 uvicorn 改成真实访客），
#           但要求名单里把**每一层**代理的地址都列全；
# 只配后者：限速是对的，但协议仍然是 http，Cookie 的 Secure 判断还是错的。
# 两个都按实际情况填最稳妥，且都遵守同一条原则：宁可少不可多。
#
# 【DESIGNKIT_WORKERS：开几个进程】
# 默认 1，与今天完全一致。公网上人多了可以调到 2~4（一般不超过 CPU 核数）。
#
# ⚠ 调大之前先弄清「同时生成几张图」这一项的实际含义：生成 worker 的线程池是
#   **每个进程各建一个**的，开 N 个进程 = 设置页上填的那个数 × N 张同时在跑，
#   而界面上显示的还是原来那个数。这直接关系到花多少钱、以及生图网关会不会被打爆。
#   （这个乘法本身由 backend/app/services/worker.py 那一路负责修，
#     这里只负责把进程数这个参数暴露出来。）
# ⚠ 还有一条硬前提：**多进程只能配 PostgreSQL 用**（compose 里就是 PG，默认满足）。
#   「启动时建表 / 补列」那把进程锁是 PostgreSQL 的咨询锁，SQLite 上直接跳过
#   （见 backend/app/migrations.py 里 _acquire_process_lock 的说明）。
#   若有人不带数据库单独跑这个镜像（那时后端是 SQLite）又把这项调大，实测的表现是：
#   首次启动有几个进程以 `table users already exists` 崩掉、被自动拉起后才正常，
#   日志里先糊一屏红色 Traceback。
#
# 其余多进程相关的东西已经确认过是安全的：建表 / 迁移在 PostgreSQL 上有咨询锁串行化，
# 任务领取是带条件的原子 UPDATE（不会重复出图、不会重复花钱），
# 灵感库同步和自动开通各自有数据库锁（见 services/scheduler.py）。
ENV DESIGNKIT_DATA_DIR=/app/data \
    PYTHONUNBUFFERED=1 \
    PYTHONDONTWRITEBYTECODE=1 \
    DESIGNKIT_FORWARDED_ALLOW_IPS=127.0.0.1 \
    DESIGNKIT_WORKERS=1

EXPOSE 8787

# 以 root 进入口脚本：它按 PUID/PGID 纠正数据目录属主后再降权运行应用本身。
# 这样绑挂载到 NAS 共享文件夹时用户无需手工 chown，应用进程也不是 root。
ENTRYPOINT ["docker-entrypoint.sh"]
# 走一层 sh -c 是为了把上面两个环境变量拼进命令行（JSON 数组形式的 CMD 不做变量展开）。
# 末尾的 exec 让 uvicorn 顶替 shell 成为容器主进程，否则 docker stop 的 SIGTERM
# 停在 shell 上，容器要硬等到 stop_grace_period 超时才被杀——正在出的图会丢。
# `${VAR:-默认值}` 是二次兜底：.env 里把变量写成空值时（DESIGNKIT_WORKERS=）
# 不至于变成 `--workers ''` 让 uvicorn 直接启动失败。
#
# --proxy-headers 其实是 uvicorn 的默认值，这里显式写出来有两个作用：一是告诉读到
# 这一行的人「代理头是认的，认到什么程度由上面那份名单决定」；二是万一将来上游把
# 默认翻成关闭，我们不会在某次升级镜像后突然全站限速失效、Cookie 丢 Secure。
CMD ["sh", "-c", "exec uvicorn backend.app.main:app --host 0.0.0.0 --port 8787 --proxy-headers --forwarded-allow-ips \"${DESIGNKIT_FORWARDED_ALLOW_IPS:-127.0.0.1}\" --workers \"${DESIGNKIT_WORKERS:-1}\""]
