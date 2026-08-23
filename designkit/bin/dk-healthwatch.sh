#!/bin/sh
# designkit 巡检 —— 在群晖 NAS 上跑（DSM 任务计划以 root 身份，每 10 分钟一次）。
# 怎么在 DSM 里配，见 designkit/docs/配置手册.md「第 10 步·b 巡检与告警」。
#
# 查四样：
#   1. 后端健康接口  http://127.0.0.1:18080/v1/designkit/health
#      （HTTP 打不打得开是一回事，body 里自报 degraded 是另一回事，两样都查。
#        这个接口设计成恒返 HTTP 200、把降级写在 body 里，所以不能只看状态码。）
#   2. 图像处理服务  它没映射宿主端口，只能 docker exec 进容器里探 /healthz
#   3. 六个容器      是不是都在 running
#   4. 磁盘          /volume3 用量超过 85% 就告警（备份和出图都会因为磁盘满而失败）
#
# 有异常时：
#   - 中文告警行追加到  backups/alerts.log       历史，只增不删
#   - 本轮全部告警写进  backups/ALERT-最新.txt    当前状态，每次覆盖
#   - 调一次 send_webhook（默认什么都不做；要接微信/钉钉往那个函数里加，见下面注释）
# 恢复正常后：ALERT-最新.txt 自动改写成「一切正常」，历史仍留在 alerts.log。
#
# 退出码：有告警 = 1，全正常 = 0。
# ⚠ 本脚本假定以 root 运行（任务计划里用户选 root），所以没有套 sudo。

set -u

# ---- 下面这些都可以用环境变量覆盖，平时不用动 ----
DOCKER="${DK_DOCKER:-/usr/local/bin/docker}"
BACKUP_ROOT="${DK_BACKUP_ROOT:-/volume3/docker/designkit-dev/backups}"
HEALTH_URL="${DK_HEALTH_URL:-http://127.0.0.1:18080/v1/designkit/health}"
IMGSVC_CONTAINER="${DK_IMGSVC_CONTAINER:-designkit-imgsvc}"
DISK_PATH="${DK_DISK_PATH:-/volume3}"
DISK_LIMIT="${DK_DISK_LIMIT:-85}"
# 六个容器，一个都不能少
# ⚠ designkit-frontend-dev 刻意不在必查名单里：它是开发用热更新服务器
# （13000，只绑本机），生产运行不需要它，停着是常态——列进来只会天天误报。
CONTAINERS="sub2api-dev sub2api-postgres-dev sub2api-redis-dev designkit-imgsvc designkit-rembg"

ALERTS_LOG="$BACKUP_ROOT/alerts.log"
LATEST="$BACKUP_ROOT/ALERT-最新.txt"

# ─────────────────────────────────────────────────────────────────────────
# 以后要接微信 / 钉钉 / 飞书通知，只改这个函数，别的地方都不用动。
# $1 = 本轮全部告警文字（多行）。
#
# 钉钉群机器人示例（把 XXXX 换成机器人的 access_token；机器人的「安全设置」
# 选「自定义关键词」、关键词填「告警」，消息里带这两个字才发得出去）：
#
#   TEXT=$(printf '%s' "$1" | tr '\n"' '；一')   # 压成一行、去掉引号，避免拼出坏 JSON
#   curl -s -m 10 'https://oapi.dingtalk.com/robot/send?access_token=XXXX' \
#     -H 'Content-Type: application/json' \
#     -d "{\"msgtype\":\"text\",\"text\":{\"content\":\"designkit 告警：$TEXT\"}}"
#
# 企业微信群机器人同理，只是地址换成：
#   https://qyapi.weixin.qq.com/cgi-bin/webhook/send?key=你的KEY
# ─────────────────────────────────────────────────────────────────────────
send_webhook() {
  :   # 现在没配任何通知渠道，什么都不做
}

mkdir -p "$BACKUP_ROOT"
TMP=$(mktemp) || exit 1
BODY=$(mktemp) || { rm -f "$TMP"; exit 1; }
trap 'rm -f "$TMP" "$BODY"' EXIT

now() { date '+%Y-%m-%d %H:%M:%S'; }

alert() {
  echo "[$(now)] 告警：$1" >> "$TMP"
}

# ---- 1. 后端健康接口 ----
if command -v curl >/dev/null 2>&1; then
  CODE=$(curl -s -m 10 -o "$BODY" -w '%{http_code}' "$HEALTH_URL" 2>/dev/null || echo 000)
  if [ "$CODE" != "200" ]; then
    alert "后端健康接口打不开（HTTP $CODE）：$HEALTH_URL —— 后端多半没起来，看下面的容器状态"
  elif ! grep -q '"status":"ok"' "$BODY"; then
    # degraded 时 body 里带中文原因（degraded_reasons），原样抄进告警
    alert "后端自报降级：$(cat "$BODY")"
  fi
else
  alert "NAS 上找不到 curl，探不了后端健康接口（这是巡检脚本自己的问题，不代表系统坏了）"
fi

# ---- 2. 图像处理服务（没映射宿主端口，进容器里用 python 探）----
# 容器本身不在跑的情况，下面第 3 项会报，这里不重复报。
if "$DOCKER" inspect -f '{{.State.Status}}' "$IMGSVC_CONTAINER" 2>/dev/null | grep -q '^running$'; then
  "$DOCKER" exec "$IMGSVC_CONTAINER" python -c \
    "import sys,urllib.request; sys.exit(0 if urllib.request.urlopen('http://127.0.0.1:8000/healthz', timeout=5).status == 200 else 1)" \
    >/dev/null 2>&1 \
    || alert "图像处理服务 /healthz 探活失败（容器在跑但服务没响应）——补边、白底、放大都会失败"
fi

# ---- 3. 六个容器 ----
for c in $CONTAINERS; do
  st=$("$DOCKER" inspect -f '{{.State.Status}}' "$c" 2>/dev/null || echo "不存在")
  [ "$st" = "running" ] || alert "容器 $c 状态是「$st」（应为 running）"
done

# ---- 4. 磁盘 ----
USED=$(df -P "$DISK_PATH" 2>/dev/null | awk 'NR==2 {gsub("%","",$5); print $5}')
if [ -z "${USED:-}" ]; then
  alert "读不到 $DISK_PATH 的磁盘用量（df 失败）"
elif [ "$USED" -gt "$DISK_LIMIT" ] 2>/dev/null; then
  alert "磁盘 $DISK_PATH 已用 ${USED}%（超过 ${DISK_LIMIT}%）——先删旧备份、旧图片，不然出图和备份都会失败"
fi

# ---- 汇总 ----
if [ -s "$TMP" ]; then
  cat "$TMP" >> "$ALERTS_LOG"
  {
    echo "designkit 巡检发现问题（生成于 $(now)；每 10 分钟巡检一次，恢复后本文件会自动改写）"
    echo "怎么处理：看 designkit/docs/配置手册.md「第 10 步·b 巡检与告警」；历史告警在 alerts.log"
    echo
    cat "$TMP"
  } > "$LATEST"
  send_webhook "$(cat "$TMP")"
  exit 1
fi

# 全正常。上一轮有告警的话，把「最新」文件改写成正常，并在历史里记一笔恢复。
if [ -f "$LATEST" ] && ! grep -q "一切正常" "$LATEST"; then
  echo "[$(now)] 恢复：本轮巡检一切正常" >> "$ALERTS_LOG"
  echo "[$(now)] 一切正常（此前的告警已恢复，历史见 alerts.log）" > "$LATEST"
fi
exit 0
