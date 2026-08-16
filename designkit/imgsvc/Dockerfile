# designkit 图像预处理服务
#
# 构建（在本目录下）：
#   docker build -t designkit-imgsvc:dev .
#
# 双架构（linux/amd64 + linux/arm64，群晖 NAS 是 arm64）：
#   docker buildx build --platform linux/amd64,linux/arm64 -t designkit-imgsvc:dev .
#
# ─────────────────────────────────────────────────────────────────────────
# 为什么用 bookworm 而不是 alpine
# ─────────────────────────────────────────────────────────────────────────
# pillow-heif 和 Pillow 的预编译轮子分 manylinux（glibc）和 musllinux（musl）
# 两套。alpine 用 musl，虽然也有轮子，但 Debian 这一套用的人多、踩过的坑少。
# bookworm 的 glibc 是 2.36，满足 pillow-heif 轮子要求的 manylinux_2_28。
#
# 基础镜像的 Python 补丁版本是浮动的（3.12-slim-bookworm 会跟着上游走）。
# 要完全复现某次构建，把下面这行换成带 sha256 摘要的写法：
#   FROM python:3.12-slim-bookworm@sha256:<摘要>
# Python 侧的依赖已经在 requirements.txt 里全部钉死，不受这个影响。
FROM python:3.12-slim-bookworm

# PYTHONUNBUFFERED：日志立刻出现在 docker logs 里，不然排查问题时看不到东西
# PYTHONDONTWRITEBYTECODE：容器里不需要 .pyc，少写一堆文件
ENV PYTHONUNBUFFERED=1 \
    PYTHONDONTWRITEBYTECODE=1 \
    PIP_NO_CACHE_DIR=1 \
    PIP_DISABLE_PIP_VERSION_CHECK=1

WORKDIR /app

# ─────────────────────────────────────────────────────────────────────────
# 依赖
# ─────────────────────────────────────────────────────────────────────────
# 这里**故意不装 gcc / libheif / libjpeg 之类的系统包**：
# Pillow 和 pillow-heif 的官方轮子已经把 libheif、libde265、libjpeg、zlib
# 这些原生库静态打进去了，装系统包既没必要，又会让镜像大一倍。
#
# requirements.txt 里的 `--only-binary=:all:` 保证：万一哪天某个依赖在
# arm64 上没有预编译轮子，构建会**当场报错**，而不是在 QEMU 模拟的 arm64
# 构建机上闷头编译一小时再失败。
COPY requirements.txt ./requirements.txt
RUN pip install --no-cache-dir -r requirements.txt

# ─────────────────────────────────────────────────────────────────────────
# 代码 + 模型
# ─────────────────────────────────────────────────────────────────────────
COPY app ./app
# 「高清放大」的 Real-ESRGAN 小模型（4.9MB，BSD-3-Clause，来源见
# models/LICENSE-realesrgan.md）。直接打进镜像：模型跟代码一起走版本，
# 不需要挂卷、不需要启动时联网下载。
COPY models ./models

# 不用 root 跑。这个容器不写任何文件、不挂任何卷，用固定 uid 即可。
RUN groupadd --gid 10001 imgsvc \
    && useradd --uid 10001 --gid 10001 --no-create-home --shell /usr/sbin/nologin imgsvc
USER 10001:10001

# ─────────────────────────────────────────────────────────────────────────
# 运行参数（都可以在 compose 的 environment 里覆盖）
# ─────────────────────────────────────────────────────────────────────────
# ⚠ DESIGNKIT_IMGSVC_DEFAULT_MAX_DIMENSION 直接决定出图落 2K 还是 4K 计费档
#   （最长边 ≤1024→1K、≤2048→2K、更大→4K）。默认 2048 = 2K 档。
ENV DESIGNKIT_IMGSVC_PORT=8000 \
    DESIGNKIT_IMGSVC_WORKERS=1 \
    DESIGNKIT_IMGSVC_DEFAULT_MAX_DIMENSION=2048 \
    DESIGNKIT_IMGSVC_MAX_UPLOAD_BYTES=20971520 \
    DESIGNKIT_IMGSVC_MAX_PIXELS=80000000 \
    DESIGNKIT_IMGSVC_MAX_CONCURRENCY=4

# 只在容器网络内暴露。compose 里**故意不映射宿主端口**。
EXPOSE 8000

# slim 镜像里没有 curl 也没有 wget，用 python 自带的 urllib 探活。
HEALTHCHECK --interval=30s --timeout=5s --start-period=15s --retries=3 \
    CMD ["python", "-c", "import os,sys,urllib.request; sys.exit(0 if urllib.request.urlopen('http://127.0.0.1:%s/healthz' % os.getenv('DESIGNKIT_IMGSVC_PORT', '8000'), timeout=4).status == 200 else 1)"]

# 走一层 sh -c 是为了把环境变量拼进命令行（JSON 数组形式的 CMD 不做变量展开）。
# 末尾的 exec 让 uvicorn 顶替 shell 成为容器主进程，否则 docker stop 的 SIGTERM
# 停在 shell 上，容器要硬等到超时才被杀。
CMD ["sh", "-c", "exec uvicorn app.main:app --host 0.0.0.0 --port \"${DESIGNKIT_IMGSVC_PORT:-8000}\" --workers \"${DESIGNKIT_IMGSVC_WORKERS:-1}\" --no-server-header"]
