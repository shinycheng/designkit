#!/bin/sh
# designkit 每日备份 —— 在群晖 NAS 上跑（DSM 任务计划以 root 身份，每天 3:00）。
# 怎么在 DSM 里配，见 designkit/docs/配置手册.md 第 10 步。
#
# 一次运行做三件事：
#   1. 导出整个数据库    docker exec pg_dump --clean --if-exists
#   2. 打包图片目录      tar czf（生成的图 + 上传的商品图都在里面）
#   3. 清理旧备份        只保留最近 14 份，更早的自动删掉（不会撑爆 NAS）
#
# 结果落在  /volume3/docker/designkit-dev/backups/<日期-时间>/
#   ├── db.sql          数据库（用户、余额、任务、提示词……）
#   └── images.tar.gz   图片
# 每次运行在 backups/backup.log 追加一行「成功/失败 + 大小」。
#
# 手动跑一次（在 Mac 上）：
#   ssh dk-nas 'sudo bash /volume3/docker/designkit-dev/src/designkit/bin/dk-backup.sh'
#
# ⚠ 本脚本假定以 root 运行（任务计划里用户选 root），所以没有套 sudo。
# ⚠ 恢复步骤见配置手册第 10 步——恢复会覆盖现有数据，别顺手敲。
# ⚠ 老式的平铺备份（backups/db-*.sql、backups/images-*.tar.gz）不归它管：
#   不会被清理、也不会被覆盖，占地方了自己删。

set -u

# ---- 下面这些都可以用环境变量覆盖，平时不用动 ----
DOCKER="${DK_DOCKER:-/usr/local/bin/docker}"
BACKUP_ROOT="${DK_BACKUP_ROOT:-/volume3/docker/designkit-dev/backups}"
# 图片在这个目录下的 designkit/ 子目录里（跟配置手册第 10 步原来那条命令一致）
DATA_DIR="${DK_DATA_DIR:-/volume3/docker/designkit-dev/src/deploy/data}"
PG_CONTAINER="${DK_PG_CONTAINER:-sub2api-postgres-dev}"
PG_USER="${DK_PG_USER:-sub2api}"
PG_DB="${DK_PG_DB:-sub2api}"
KEEP="${DK_BACKUP_KEEP:-14}"

STAMP=$(date +%Y%m%d-%H%M%S)
DEST="$BACKUP_ROOT/$STAMP"
LOG="$BACKUP_ROOT/backup.log"

mkdir -p "$DEST" || { echo "建不出备份目录 $DEST"; exit 1; }

# 写一行结果：backup.log 一份、屏幕一份（DSM 任务计划的运行记录能看到屏幕这份）
log_line() {
  echo "[$(date '+%Y-%m-%d %H:%M:%S')] $1" | tee -a "$LOG"
}

fail() {
  log_line "失败：$1（半成品在 $DEST，错误详情看该目录下的 error.log）"
  exit 1
}

# ---- 1. 数据库 ----
"$DOCKER" exec "$PG_CONTAINER" pg_dump -U "$PG_USER" -d "$PG_DB" --clean --if-exists \
  > "$DEST/db.sql" 2>"$DEST/error.log" \
  || fail "数据库导出出错（容器 $PG_CONTAINER 是不是没在跑？）"
[ -s "$DEST/db.sql" ] || fail "数据库导出是空文件"

# ---- 2. 图片 ----
[ -d "$DATA_DIR/designkit" ] || fail "图片目录不存在：$DATA_DIR/designkit"
tar czf "$DEST/images.tar.gz" -C "$DATA_DIR" designkit 2>>"$DEST/error.log" \
  || fail "图片打包出错（tar）"

# 走到这儿说明没出错，别留个空的 error.log 吓人
rm -f "$DEST/error.log"

# ---- 3. 清理：只保留最近 KEEP 份 ----
# 备份目录名就是时间戳，按名字排序 = 按时间排序（glob 展开天然有序），从最旧的删起。
# 只匹配「8位数字-6位数字」这个形状的目录；别的东西（老式平铺备份、日志）一概不碰。
set --
for d in "$BACKUP_ROOT"/[0-9][0-9][0-9][0-9][0-9][0-9][0-9][0-9]-[0-9][0-9][0-9][0-9][0-9][0-9]; do
  [ -d "$d" ] && set -- "$@" "$d"
done
while [ $# -gt "$KEEP" ]; do
  rm -rf "$1"
  shift
done
COUNT=$#

# ---- 4. 结果一行 ----
DB_SIZE=$(du -h "$DEST/db.sql" | cut -f1)
IMG_SIZE=$(du -h "$DEST/images.tar.gz" | cut -f1)
TOTAL_SIZE=$(du -sh "$DEST" | cut -f1)
log_line "成功：数据库 ${DB_SIZE} + 图片 ${IMG_SIZE}，共 ${TOTAL_SIZE} → $DEST（现存 ${COUNT} 份）"
