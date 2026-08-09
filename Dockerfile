# 与本地开发一致，使用 Python 3.9，避免两套环境依赖版本漂移
FROM python:3.9-slim

WORKDIR /app

COPY backend/requirements.txt backend/requirements.txt
RUN pip install --no-cache-dir -r backend/requirements.txt

COPY backend backend
COPY frontend frontend

ENV DESIGNKIT_DATA_DIR=/app/data

# 用固定 uid 的非 root 用户跑：容器以 root 运行时，绑挂载写出的图片和数据库
# 在宿主机上都会变成 root 所有，用户备份、删除都要 sudo
RUN useradd --uid 10001 --create-home --shell /usr/sbin/nologin designkit \
    && mkdir -p /app/data \
    && chown -R designkit:designkit /app
USER designkit

VOLUME ["/app/data"]
EXPOSE 8787
CMD ["uvicorn", "backend.app.main:app", "--host", "0.0.0.0", "--port", "8787"]
