#!/bin/sh
# 容器启动入口：先用 root 把数据目录的属主纠正成 PUID/PGID，再降权运行应用。
#
# 为什么需要这一步：
# NAS 上的共享文件夹属主 uid 各不相同（群晖常见 1026），而绑挂载的目录若由 Docker
# 自动创建会归 root。容器若以固定 uid 运行就会写不进图片，表现为「上传报错、生成全失败」，
# 而且报错完全看不出根因。这里在启动时自动对齐，用户不用手工 chown。
set -e

PUID=${PUID:-10001}
PGID=${PGID:-10001}
DATA_DIR=${DESIGNKIT_DATA_DIR:-/app/data}

if [ "$(id -u)" = "0" ]; then
    # 确保目标组和用户存在（uid/gid 可能是宿主机上任意值）
    if ! getent group "$PGID" >/dev/null 2>&1; then
        groupadd -g "$PGID" designkit-run 2>/dev/null || true
    fi
    if ! getent passwd "$PUID" >/dev/null 2>&1; then
        useradd -u "$PUID" -g "$PGID" -M -s /usr/sbin/nologin designkit-run 2>/dev/null || true
    fi

    mkdir -p "$DATA_DIR"
    # 只在属主不符时才递归改，避免几万张图片每次启动都被遍历一遍
    CURRENT_UID=$(stat -c '%u' "$DATA_DIR" 2>/dev/null || echo "")
    if [ "$CURRENT_UID" != "$PUID" ]; then
        # 递归改属主是不可逆的。万一有人把整个共享文件夹（甚至 /volume1）挂到了这里，
        # 一次启动就会把人家全盘的属主改掉。所以只在「空目录」或「确实是本项目的数据目录」
        # 时才动手，认不出来就宁可不改、让用户自己处理。
        SAFE=no
        if [ -z "$(ls -A "$DATA_DIR" 2>/dev/null)" ]; then
            SAFE=yes
        else
            for marker in designkit.db uploads outputs thumbnails; do
                if [ -e "$DATA_DIR/$marker" ]; then SAFE=yes; break; fi
            done
        fi

        if [ "$SAFE" = yes ]; then
            echo "[entrypoint] 正在把 $DATA_DIR 的属主设为 $PUID:$PGID …"
            chown -R "$PUID:$PGID" "$DATA_DIR" || \
                echo "[entrypoint] 警告：修改属主失败，若上传图片报错请在宿主机手工 chown"
        else
            echo "[entrypoint] 警告：$DATA_DIR 里有本项目之外的内容，为安全起见不自动改属主。"
            echo "[entrypoint]       请确认 compose 里挂载的是专门给 DesignKit 用的目录；"
            echo "[entrypoint]       若确认无误，请在宿主机执行：chown -R $PUID:$PGID <该目录>"
        fi
    fi

    exec gosu "$PUID:$PGID" "$@"
fi

# 已经是非 root（compose 里显式指定了 user:），直接运行
exec "$@"
