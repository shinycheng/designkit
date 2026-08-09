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

ENV DESIGNKIT_DATA_DIR=/app/data \
    PYTHONUNBUFFERED=1 \
    PYTHONDONTWRITEBYTECODE=1

EXPOSE 8787

# 以 root 进入口脚本：它按 PUID/PGID 纠正数据目录属主后再降权运行应用本身。
# 这样绑挂载到 NAS 共享文件夹时用户无需手工 chown，应用进程也不是 root。
ENTRYPOINT ["docker-entrypoint.sh"]
CMD ["uvicorn", "backend.app.main:app", "--host", "0.0.0.0", "--port", "8787"]
