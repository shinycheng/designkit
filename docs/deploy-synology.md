# 部署到群晖 NAS（照着做就行）

适用：群晖 DSM 7.x + Container Manager。全程约 20 分钟，其中大部分时间在等下载。

> **为什么部署在 NAS 而不是云服务器**：你的生图网关是局域网地址
> `192.168.31.235:8090`，云服务器访问不到它。NAS 在同一个局域网里，直连即可。

---

## 第 0 步：确认你的群晖跑得动

先在 DSM 里查两件事：

**① 芯片架构**（决定能不能装）
控制面板 → 信息中心 → 常规 → 看「CPU 型号」

- Intel / AMD 开头（如 Celeron J4125）→ ✅ 没问题
- Realtek / Marvell / Annapurna 开头（如 RTD1619B）→ ⚠️ 是 ARM 芯片，
  能跑但构建镜像会很慢（可能 20-40 分钟），内存也往往偏小

**② 内存**：信息中心 → 常规 → 「内存」

- 4GB 以上 → ✅ 舒服
- 2GB → ⚠️ 勉强能跑（PostgreSQL + 应用 + AI 请求），建议先关掉其他占内存的套件
- 1GB → ❌ 不建议，容易被系统杀进程

---

## 第 1 步：开启 SSH

DSM → 控制面板 → 终端机和 SNMP → 勾选「启动 SSH 功能」→ 应用

端口保持默认 22 即可（这是局域网内部访问，不用改）。

---

## 第 2 步：安装 Container Manager

DSM → 套件中心 → 搜索「Container Manager」→ 安装

（DSM 6.x 里这个套件叫「Docker」，一样用）

---

## 第 3 步：把项目传到 NAS

在**你的 Mac 上**打开终端，执行（把 `你的用户名` 和 `NAS的IP` 换成实际值）：

```bash
cd /Users/monica/Desktop
rsync -av --exclude 'data' --exclude '.venv' --exclude '.git' \
  designkit/ 你的用户名@NAS的IP:/volume1/docker/designkit/
```

> 如果提示 `rsync: command not found` 或路径不存在，也可以用 File Station
> 把 designkit 文件夹拖到 NAS 的 `docker` 共享文件夹里，
> 但**不要**拖 `data`、`.venv`、`.git` 这三个（体积大且没用）。

---

## 第 4 步：写配置文件

SSH 登录 NAS：

```bash
ssh 你的用户名@NAS的IP
```

查出 data 目录该用的用户 id（**这一步很关键**，不做的话容器写不进图片）：

```bash
id
```

会输出类似 `uid=1026(monica) gid=100(users)`，记下这两个数字。

然后创建配置文件：

```bash
cd /volume1/docker/designkit
cat > .env <<'EOF'
# ---- 数据库（密码自己改，可以带特殊符号）----
POSTGRES_DB=designkit
POSTGRES_USER=designkit
POSTGRES_PASSWORD=在这里写一个你自己的强密码

# ---- 让局域网内的电脑都能访问 ----
DESIGNKIT_BIND_HOST=0.0.0.0
DESIGNKIT_HOST_PORT=8787

# ---- 文件权限：换成上一步 id 命令输出的数字 ----
PUID=1026
PGID=100

# ---- 生图网关 ----
DESIGNKIT_PROVIDER=openai
OPENAI_BASE_URL=http://192.168.31.235:8090/v1
OPENAI_API_KEY=在这里填你的网关 Key
DESIGNKIT_IMAGE_MODEL=gpt-image-2
DESIGNKIT_TEXT_MODEL=gpt-5.6-sol

# ---- 对外访问地址：换成 NAS 的实际 IP ----
DESIGNKIT_PUBLIC_BASE_URL=http://NAS的IP:8787
EOF
chmod 600 .env
```

> `chmod 600` 让这个文件只有你能读——里面有数据库密码和网关 Key。

---

## 第 5 步：先建好 data 目录（**别跳过**）

图片都存在这个目录里。如果不先建、让 Docker 自动创建，它会归 root 所有，
容器以你的身份运行就写不进去——表现为上传图片报错、生成全部失败。

```bash
cd /volume1/docker/designkit
mkdir -p data
sudo chown -R $(id -u):$(id -g) data
```

## 第 6 步：启动

```bash
cd /volume1/docker/designkit
sudo docker compose up -d --build
```

第一次会下载并构建镜像，Intel 机型约 3-8 分钟，ARM 机型可能 20-40 分钟。

看到两个容器都是 `Started` 就成功了。检查一下：

```bash
sudo docker compose ps
sudo docker compose logs designkit | tail -20
```

日志里出现「生成 worker 已启动」「灵感库自动同步调度器已启动」就正常。

---

## 第 7 步：打开使用

浏览器访问：**http://NAS的IP:8787**

- 初始账号 `admin`，密码 `admin123456`，**首次登录会强制你改密码**
- 进「灵感库」→ 点右上角「同步上游」，约 15 秒拉回 1.4 万条提示词
- 进「系统设置 → 生图服务」→ 点「测试已保存的连接」，确认网关通了

---

## 第 8 步：设置每日自动备份（建议做）

⚠️ **注意**：PostgreSQL 部署下，只拷 `data` 文件夹**不会**备份到数据库。

DSM → 控制面板 → 任务计划 → 新增 → 计划的任务 → 用户定义的脚本

- 用户：`root`
- 计划：每天，选个你不用 NAS 的时间（比如凌晨 3 点）
- 脚本内容：

```bash
cd /volume1/docker/designkit
mkdir -p /volume1/docker/designkit-backup
DATE=$(date +%Y%m%d)
docker compose exec -T db pg_dump -U designkit designkit | gzip > /volume1/docker/designkit-backup/db-$DATE.sql.gz
tar czf /volume1/docker/designkit-backup/images-$DATE.tar.gz -C /volume1/docker/designkit data
# 只保留最近 14 天
find /volume1/docker/designkit-backup -name '*.gz' -mtime +14 -delete
```

---

## 以后怎么更新代码

在 Mac 上改完并推到 GitHub 后：

```bash
ssh 你的用户名@NAS的IP
cd /volume1/docker/designkit
sudo docker compose down
# 重新上传代码（在 Mac 上执行第 3 步的 rsync），然后：
sudo docker compose up -d --build
```

数据库和图片都在数据卷/data 目录里，重建容器不会丢。

---

## 出问题了怎么查

| 现象 | 原因与解决 |
|---|---|
| 浏览器打不开 8787 | 先在 NAS 上 `curl localhost:8787` 试试。通了说明是防火墙：控制面板 → 安全性 → 防火墙，放行 8787 |
| 容器起来又退出 | `sudo docker compose logs designkit`。若提示 `POSTGRES_PASSWORD` 未设置，是 `.env` 没写对或没在同一目录 |
| 上传图片报错、生成失败 | 多半是权限：`sudo chown -R 1026:100 /volume1/docker/designkit/data`（数字换成你 `id` 的输出） |
| 测试连接失败 | NAS 能不能连到网关：`curl http://192.168.31.235:8090/v1/models`。不通就是两台机器不在同一网段或网关没开 |
| 生图一直失败 | 看日志里的中文报错；也可能是网关那台机器关机了 |
| 构建卡住很久 | ARM 机型正常现象，耐心等；实在不行改用 Intel 机型或小主机 |

---

## 安全提醒

- **不要**把 8787 端口转发到公网（路由器端口映射）。当前是 HTTP 明文，
  登录密码会在网上裸奔。需要外网访问请用群晖自带的 QuickConnect，
  或加一层反向代理配 HTTPS。
- `.env` 里有数据库密码和网关 Key，别把它上传到任何地方（项目的 `.gitignore` 已排除）。
