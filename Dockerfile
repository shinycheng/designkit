# 与本地开发一致，使用 Python 3.9，避免两套环境依赖版本漂移
FROM python:3.9-slim

WORKDIR /app

COPY backend/requirements.txt backend/requirements.txt
RUN pip install --no-cache-dir -r backend/requirements.txt

COPY backend backend
COPY frontend frontend

ENV DESIGNKIT_DATA_DIR=/app/data
VOLUME ["/app/data"]

EXPOSE 8787
CMD ["uvicorn", "backend.app.main:app", "--host", "0.0.0.0", "--port", "8787"]
