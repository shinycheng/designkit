# 部署到群晖 NAS

适用：群晖 DSM 7.x + Container Manager。全程约 10 分钟，大部分时间在等镜像下载。

镜像由 GitHub 自动构建好，**不用上传源码，也不用在 NAS 上编译**。

> **为什么部署在 NAS 而不是云服务器**：你的生图网关是局域网地址
> `192.168.31.235:8090`，云服务器访问不到它。NAS 在同一个局域网里，直连即可。

---

## 第 0 步：确认你的群晖跑得动

DSM → 控制面板 → 信息中心 → 常规：

- **CPU**：Intel / AMD / Realtek / ARM 都可以（镜像同时提供 x86 和 ARM 两个版本）
- **内存**：4GB 以上舒服；2GB 勉强能跑；1GB 不建议

---

## 第 1 步：装 Container Manager 并开 SSH

- 套件中心 → 搜索「Container Manager」→ 安装（DSM 6.x 里叫「Docker」）
- 控制面板 → 终端机和 SNMP → 勾选「启动 SSH 功能」→ 应用

---

## 第 2 步：下载两个配置文件

SSH 登录 NAS（把 `你的用户名` 和 `NAS的IP` 换成实际值）：

```bash
ssh 你的用户名@NAS的IP
```

然后执行：

```bash
mkdir -p /volume1/docker/designkit && cd /volume1/docker/designkit
curl -fLo docker-compose.yml https://raw.githubusercontent.com/shinycheng/designkit/main/docker-compose.yml
curl -fLo .env https://raw.githubusercontent.com/shinycheng/designkit/main/example.env
```

---

## 第 3 步：改配置

先查出你的用户 id：

```bash
id
```

输出类似 `uid=1026(monica) gid=100(users)`，记下这两个数字。

然后编辑 `.env`：

```bash
vi .env
```

> 不熟悉 vi 的话，也可以在 DSM 的 **File Station** 里找到
> `docker/designkit/.env`，右键「用文本编辑器打开」直接改。
> （File Station 默认不显示以 `.` 开头的文件，需要在「设置」里勾选「显示隐藏文件」）

必须改的只有 4 处：

| 项 | 改成什么 |
|---|---|
| `POSTGRES_PASSWORD` | 自己想一个强密码，可以带特殊符号 |
| `DESIGNKIT_DATA_LOCATION` | `/volume1/docker/designkit/data` |
| `PUID` / `PGID` | 上面 `id` 命令输出的那两个数字 |
| `DESIGNKIT_PUBLIC_BASE_URL` | `http://NAS的IP:8787` |

网关 Key（`OPENAI_API_KEY`）**建议留空**——启动后在网页「系统设置 → 生图服务」里填，
这样 Key 不会躺在文件里被别人看到。

---

## 第 4 步：启动

```bash
cd /volume1/docker/designkit
sudo docker compose up -d
```

第一次要下载镜像，约 2-5 分钟。看到两个容器都是 `Started` 就成了：

```bash
sudo docker compose ps
```

`STATUS` 那列出现 `healthy` 说明应用和数据库都正常。

---

## 第 5 步：打开使用

浏览器访问：**http://NAS的IP:8787**

- 初始账号 `admin`，密码 `admin123456`，**首次登录会强制你改密码**
- 进「系统设置 → 生图服务」填入网关 Key，点「测试已保存的连接」确认通了
- 进「灵感库」→ 点右上角「同步上游」，约 15 秒拉回 1.4 万条提示词

---

## 第 6 步：设置每日自动备份（建议做）

⚠️ 数据库在 Docker 数据卷里，**只拷 `data` 文件夹不会备份到数据库**。

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
find /volume1/docker/designkit-backup -name '*.gz' -mtime +14 -delete
```

---

## 以后怎么更新

代码推到 GitHub 后，GitHub 会自动构建新镜像。在 NAS 上执行：

```bash
cd /volume1/docker/designkit
sudo docker compose pull && sudo docker compose up -d
```

数据库和图片都在卷/data 目录里，换镜像不会丢。

想锁定某个版本不自动跟进，把 `.env` 里的 `DESIGNKIT_VERSION` 从 `latest`
改成具体标签（如 `v1.0.0`）即可。

---

## 出问题了怎么查

| 现象 | 原因与解决 |
|---|---|
| `docker compose up` 报 `请在 .env 里设置 POSTGRES_PASSWORD` | `.env` 没改密码，或者 `.env` 不在当前目录 |
| 拉镜像报 `denied` / `not found` | 镜像跟随公开仓库自动公开，正常不会出现。若真遇到，去 GitHub 仓库 → Packages → designkit → Package settings → Change visibility 确认是 Public |
| 浏览器打不开 8787 | 先在 NAS 上 `curl localhost:8787` 试试。通了说明是防火墙：控制面板 → 安全性 → 防火墙，放行 8787 |
| 容器一直 `unhealthy` | `sudo docker compose logs designkit`，多半是连不上数据库 |
| 上传图片报错、生成失败 | `.env` 里 `PUID`/`PGID` 填错了。改对后 `sudo docker compose restart designkit`，容器会自动纠正目录属主 |
| 测试连接失败 | NAS 能不能连到网关：`curl http://192.168.31.235:8090/v1/models`。不通就是两台机器不在同一网段，或网关那台机器关机了 |

---

## 安全提醒

- **不要**把 8787 端口转发到公网（路由器端口映射）。当前是 HTTP 明文，
  登录密码会在网上裸奔。需要外网访问请用群晖自带的 QuickConnect，
  或加一层反向代理配 HTTPS。
- `.env` 里有数据库密码，别把它上传到任何地方。
