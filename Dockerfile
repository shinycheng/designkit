# 与本地开发一致，使用 Python 3.9，避免两套环境依赖版本漂移
FROM python:3.9-slim

WORKDIR /app

COPY backend/requirements.txt backend/requirements.txt
RUN pip install --no-cache-dir -r backend/requirements.txt

COPY backend backend
COPY frontend frontend

ENV DESIGNKIT_DATA_DIR=/app/data \
    PYTHONUNBUFFERED=1 \
    PYTHONDONTWRITEBYTECODE=1

# 不以 root 运行：容器写出的图片和数据库在宿主机上若归 root，用户备份删除都要 sudo。
# 同时把 /app 的属组权限放开——NAS 上共享文件夹的属主 uid 各不相同，
# compose 里可用 PUID/PGID 覆盖运行身份，这里保证任意 uid 都能读写工作目录。
RUN useradd --uid 10001 --create-home --shell /usr/sbin/nologin designkit \
    && mkdir -p /app/data \
    && chown -R designkit:root /app \
    && chmod -R g=u /app
USER 10001

EXPOSE 8787
CMD ["uvicorn", "backend.app.main:app", "--host", "0.0.0.0", "--port", "8787"]
