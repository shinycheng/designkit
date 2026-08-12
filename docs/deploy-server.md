# 部署到公网服务器（正式环境）

这份文档是**正式上线**的完整流程：一台云服务器 + 一个域名 + HTTPS，
让同事在家、在外地、用手机都能打开，也能让陌生人自己注册进来。

> **和群晖那份的关系**
> [docs/deploy-synology.md](deploy-synology.md) 是**局域网测试环境**的指南：
> 用来先把功能跑顺、把备份练一遍。它里面那些 Container Manager、File Station、
> DSM 任务计划、PUID/PGID 的做法，在普通服务器上都用不上，所以另写了这一份。
> 已经在群晖上跑过一轮的话，这里只有第 3 步（HTTPS）和第 8 步（上线清单）是全新的。

**整个过程大约 1 小时**，其中等域名解析生效可能要等十几分钟。
命令都可以整段复制粘贴，需要你改的地方都用 `你的域名` / `你的邮箱` 这种字样标出来了。

> 📌 **全文的两个约定**
> 1. 命令前面的 `sudo` **一个都不能省**。你 SSH 上服务器用的是普通账号，
>    而 Docker 只让 root 使唤，不加 `sudo` 会得到一句英文的
>    `permission denied ... docker daemon socket`——那不是你装错了。
>    敲下去如果提示输密码，输**你自己的登录密码**（屏幕上不显示，输完直接回车）。
> 2. 本文假设你把 DesignKit 装在 `/opt/designkit`。你想换别的目录也行，
>    把后面所有命令里的 `/opt/designkit` 一起换掉即可。

> ⚠️ **哪些步骤我没有在真机上验证过**
> 写这份文档的机器上没有 Docker、没有 Caddy、没有 Nginx，也没有一台公网服务器，
> 所以**第 1、3、4、5、6、7 步的命令我只能按官方文档和代码写出来，没有在真机上跑通过**。
> 每一节开头都会再标一次。
> 反过来，**凡是和 DesignKit 自身设置有关的部分（第 2 步的配置项名字、
> 第 3.3 节反代之后要配什么、第 8 步清单里每一条的开关位置和实际拦截行为）
> 我都在本机真的跑起来验证过**，第 9 节列了验证方式。

---

## 目录

- [第 1 步：准备机器、域名、Docker](#第-1-步准备机器域名docker)
- [第 2 步：把服务拉起来](#第-2-步把服务拉起来)
- [第 3 步：HTTPS + 反向代理（重点）](#第-3-步https--反向代理重点)
- [第 4 步：防火墙](#第-4-步防火墙)
- [第 5 步：每天自动备份](#第-5-步每天自动备份)
- [第 6 步：恢复演练（每半年一次）](#第-6-步恢复演练每半年一次)
- [第 7 步：升级与回滚](#第-7-步升级与回滚)
- [第 8 步：放人进来之前的检查清单](#第-8-步放人进来之前的检查清单)
- [第 9 步：这份文档验证到什么程度](#第-9-步这份文档验证到什么程度)

---

## 第 1 步：准备机器、域名、Docker

> ⚠️ 这一节的命令**我没有在真机上验证过**（本机没有 Docker，也没有服务器）。
> 它们来自 Docker 官方文档，是最常见的装法。

### 1.1 买什么样的机器

| 项目 | 建议 | 为什么 |
|---|---|---|
| CPU / 内存 | **2 核 4G 起步** | 应用本身很轻，但同一台机器上还跑着 PostgreSQL。2G 内存也能开机，一边生图一边有人翻历史就容易被系统杀进程 |
| 系统盘 | **40G 以上** | 系统 + Docker 镜像大约占 5–8G，剩下的留给图片 |
| 数据盘 | 看出图量：**每 1000 张成品图约 1–3G** | 上传的原图 + 生成图 + 缩略图都存在 `data/` 里，只增不减（系统没有自动清理） |
| 操作系统 | **Ubuntu 22.04 或 24.04 LTS**（本文按它写） | 用得最多、出问题一搜就有答案。Debian 12 也完全一样 |
| 带宽 | 1M 也能用，**3–5M 更舒服** | 主要是同事下载成品图。生图本身是服务器去调网关，不占你的下行 |
| 机房位置 | **同事在哪就买哪儿的** | 图片是大文件，跨境线路会让「点开一张图要转好几秒」 |

> 💡 **别买「按流量计费」的机器**。生成图动辄几 MB，一天几百张翻下来流量很可观，
> 账单会难以预估。选固定带宽的。

> 💡 **磁盘会满，而且满了之后的表现很难看**：上传报错、生成失败，
> 日志里是一句英文的 `No space left on device`。买机器时把数据盘一次买够，
> 或者选一个能在线扩容的。平时用 `df -h` 看一眼占用。

### 1.2 域名解析

你需要一个域名（比如 `designkit.example.com`）。到域名商后台加一条 **A 记录**，
指向服务器的公网 IP。

改完等 5–15 分钟，在**你自己的电脑上**（不是服务器上）执行下面这条确认已经生效：

```bash
nslookup designkit.example.com
```

打印出来的 `Address` 要和服务器的公网 IP 一致。**这一步没成之前，第 3 步的证书一定申请不下来**
（Let's Encrypt 要通过这个域名回访你的服务器来确认它真是你的）。

### 1.3 装 Docker

SSH 登录服务器后执行：

```bash
# Docker 官方安装脚本（Ubuntu / Debian 通用）
curl -fsSL https://get.docker.com | sudo sh

# 确认装好了：两条都应该打印出版本号
sudo docker --version
sudo docker compose version
```

第二条必须能打印版本号。如果提示 `docker: 'compose' is not a docker command`，
说明装的是老版本，执行 `sudo apt install docker-compose-plugin` 补上。

---

## 第 2 步：把服务拉起来

> ✅ 这一节里 DesignKit 的**配置项名字和作用我都验证过**（见第 9 节）。
> ❌ 但「在服务器上执行 docker compose up」这个动作本身我没在真机上跑过。

建目录、下载两个文件：

```bash
sudo mkdir -p /opt/designkit
cd /opt/designkit
sudo curl -fLo docker-compose.yml https://raw.githubusercontent.com/shinycheng/designkit/main/docker-compose.yml
sudo curl -fLo .env https://raw.githubusercontent.com/shinycheng/designkit/main/example.env
```

**不需要下载源码，也不需要在服务器上编译。** 镜像是 GitHub 自动构建好的现成镜像
（x86 和 ARM 两种都有，会自动挑对的那个）。

### 2.1 改 `.env`

```bash
sudo nano /opt/designkit/.env
```

（`nano` 里改完按 `Ctrl+O` 回车保存，`Ctrl+X` 退出。）

**公网部署必须改的 7 项**——比局域网多出来的那几项就是这份文档存在的理由：

| 配置项 | 填成什么 | 不改会怎样 |
|---|---|---|
| `POSTGRES_PASSWORD` | 一串长密码，可以带特殊符号 | 空着容器直接拒绝启动（故意的，防默认弱口令上生产） |
| `DESIGNKIT_DATA_LOCATION` | `/opt/designkit/data` | 图片和两把钥匙存这里，见第 5 步 |
| `PUID` / `PGID` | 在服务器上执行 `id` 看到的数字（Ubuntu 上常见 `1000`） | 填错通常是你在服务器上打不开生成的图，系统本身能用 |
| `DESIGNKIT_PUBLIC_BASE_URL` | **`https://你的域名`**，末尾不要带斜杠 | 对外 API 和回调里的图片链接会是本机地址，对接方全打不开，而系统这边一条报错都没有 |
| `DESIGNKIT_BIND_HOST` | **`127.0.0.1`** | ⚠️ 不改的话 8787 端口直接暴露在公网上，任何人都能绕过 HTTPS 用明文访问，登录密码在网上裸奔 |
| `DESIGNKIT_TRUSTED_PROXY_HOPS` | **`1`**（前面还套了 Cloudflare 之类的 CDN 就填 `2`） | 全站所有人在限速那里共用一个桶：一个人被限速，全公司都登不上 |
| `DESIGNKIT_FORWARDED_ALLOW_IPS` | **`172.16.0.0/12`**（反代装在同一台机器上时） | 见下面 3.3 节的详细解释 |

还有两项按需要改：

| 配置项 | 说明 |
|---|---|
| `DESIGNKIT_PROVIDER` | 先填 `mock`（模拟生图、不花钱）把流程跑通，确认没问题再改成 `openai` |
| `DESIGNKIT_WORKERS` | 开几个进程处理网页请求。默认 `1` 就够用；人多了可以填 `2`~`4`（别超过 CPU 核数） |

> 📌 **`DESIGNKIT_FORWARDED_ALLOW_IPS` 和 `DESIGNKIT_WORKERS` 这两行，
> 你下载到的 `.env` 模板里可能还没有。** 找不到的话就**自己在文件末尾加上**，
> 一行一个，格式和别的行一样（`名字=值`，等号两边不要有空格）：
>
> ```bash
> DESIGNKIT_FORWARDED_ALLOW_IPS=172.16.0.0/12
> DESIGNKIT_WORKERS=1
> ```
>
> 模板里没有不代表不生效——`docker-compose.yml` 会把 `.env` 里的**每一行**
> 都传给容器。

> **`OPENAI_API_KEY` 建议留空**，启动之后在网页「系统设置 → 生图服务」里填。
> 写在这个文件里的话，任何能看到这台服务器的人都能拿走你的 Key。

### 2.2 启动

```bash
cd /opt/designkit
sudo docker compose up -d
```

这一条命令会起两个容器：PostgreSQL 数据库 + DesignKit 应用。
应用会等数据库健康检查通过后再启动，首次会自动建表。

确认起来了：

```bash
sudo docker compose ps
# 两个容器的 STATUS 都应该是 Up (healthy)，healthy 可能要等 40 秒左右才出现

curl -s http://127.0.0.1:8787/healthz
# 应该打印 {"status":"ok"}
```

> 此刻**只有服务器自己能访问**（因为 `DESIGNKIT_BIND_HOST=127.0.0.1`），
> 这是对的，别急。下一步给它套上 HTTPS 之后，外面才进得来。

### 2.3 先改掉管理员密码

**这一步不能拖到最后。** 系统的初始账号是 `admin` / `admin123456`，
这是写在公开文档里的、全世界都知道的密码。

现在还没配好 HTTPS，从外面进不来，所以用 SSH 隧道先进去改掉：
在**你自己的电脑上**（不是服务器上）开一个终端执行：

```bash
ssh -L 8787:127.0.0.1:8787 你的用户名@服务器IP
```

这个窗口**不要关**，然后在自己电脑的浏览器里打开 <http://127.0.0.1:8787>，
用 `admin` / `admin123456` 登录——系统会**强制**你先改密码，改一个长的。
改完把这个 SSH 窗口关掉即可。

---

## 第 3 步：HTTPS + 反向代理（重点）

> ⚠️ 这一节的**反代配置本身我没有在真机上验证过**（本机没装 Caddy / Nginx / certbot，
> 没法执行它们的语法检查）。所以每种方案我都给了一条**官方的配置检查命令**，
> 请务必先跑那一条再重启服务。
> ✅ 但「反代配好之后 DesignKit 这边要配什么、配错会怎样」（3.3、3.4 两节）
> 是我在本机上真的发请求验证过的。

「反向代理」这个词听着吓人，其实做的事情很简单：

```
同事的浏览器 --HTTPS(443)--> 反向代理 --HTTP--> 127.0.0.1:8787 (DesignKit)
                              ↑
                         证书在这里，自动申请、自动续期
```

**两种方案二选一：**

- **Caddy（推荐）**：配置只有 4 行，证书全自动，你什么都不用管。没有历史包袱就选它。
- **Nginx**：这台机器上已经跑着别的网站、已经在用 Nginx 的话选它。

### 3.1 方案 A：Caddy（推荐，最省事）

**装 Caddy**（Ubuntu / Debian，来自 Caddy 官方文档）：

```bash
sudo apt install -y debian-keyring debian-archive-keyring apt-transport-https curl
curl -1sLf 'https://dl.cloudsmith.io/public/caddy/stable/gpg.key' \
  | sudo gpg --dearmor -o /usr/share/keyrings/caddy-stable-archive-keyring.gpg
curl -1sLf 'https://dl.cloudsmith.io/public/caddy/stable/debian.deb.txt' \
  | sudo tee /etc/apt/sources.list.d/caddy-stable.list
sudo apt update
sudo apt install -y caddy
```

**写配置**：

```bash
sudo nano /etc/caddy/Caddyfile
```

把文件内容**整个换成**下面这段（只改第一行的域名）：

```caddyfile
# ↓↓↓ 只改这一行：换成你自己的域名 ↓↓↓
designkit.example.com {
	# 把请求转给本机的 DesignKit。
	# Caddy 会自动带上 X-Forwarded-For（把访客真实 IP 追加在最右边）
	# 和 X-Forwarded-Proto: https —— DesignKit 的限速和取图 Cookie 都靠这两个头，
	# 所以这里不需要你手工写任何 header 配置。
	reverse_proxy 127.0.0.1:8787

	# 文本响应压缩一下，图片本来就是压缩过的，Caddy 会自动跳过。
	encode zstd gzip

	# 单次请求体最大 64MB。
	# DesignKit 自己限制单张图 20MB，但一次可以传好几张；给到 64MB 有富余。
	# 留这一条是为了挡住「有人往上传接口灌一个 10G 的文件」把磁盘写满。
	request_body {
		max_size 64MB
	}
}
```

> ⚠️ Caddyfile 的缩进**必须用 Tab 或空格保持一致**，而且大括号的位置不能改。
> 上面这段是可以直接粘贴的完整内容，别只粘中间几行。

**检查配置有没有写错**（这一条别省，写错了 Caddy 起不来）：

```bash
sudo caddy validate --config /etc/caddy/Caddyfile
# 打印 "Valid configuration" 才算过
```

**生效**：

```bash
sudo systemctl reload caddy
sudo systemctl status caddy --no-pager
```

现在用浏览器打开 `https://你的域名`，应该能看到登录页，地址栏是小锁头。

**证书续期你不用管**：Caddy 在第一次访问时自动向 Let's Encrypt 申请证书，
并在到期前 30 天自动续，续完自动加载，不用重启、不用配定时任务。
想确认它确实拿到了证书：

```bash
sudo journalctl -u caddy --no-pager | grep -i "certificate obtained"
```

**打不开时先看这里：**

| 现象 | 多半是 |
|---|---|
| 浏览器一直转圈 / 连接超时 | 80、443 端口没放行，见第 4 步；或者域名解析还没生效（回到 1.2 确认） |
| 提示证书无效 / 不安全 | 证书还没申请下来。`sudo journalctl -u caddy -n 50 --no-pager` 看最后几行的英文原因，最常见的是 80 端口不通（Let's Encrypt 要走 80 来验证） |
| 能打开但显示 502 | Caddy 通了，但 DesignKit 没起来。`sudo docker compose ps` 看容器状态 |

### 3.2 方案 B：Nginx + certbot

> 这条路比 Caddy 多几步，因为证书要单独申请、单独配续期。

**装 Nginx 和 certbot**：

```bash
sudo apt update
sudo apt install -y nginx certbot python3-certbot-nginx
```

**先写一份只有 HTTP 的配置**（证书还没申请下来，这时候写 443 会让 Nginx 起不来）：

```bash
sudo nano /etc/nginx/sites-available/designkit
```

内容（只改 `server_name` 那一行）：

```nginx
server {
    listen 80;
    listen [::]:80;
    # ↓↓↓ 只改这一行：换成你自己的域名 ↓↓↓
    server_name designkit.example.com;

    # 单次请求体最大 64MB。
    # Nginx 默认只有 1MB，不改的话传商品图会得到一句英文的 413 Request Entity Too Large，
    # 而页面上只表现为「上传失败」，看不出原因。
    client_max_body_size 64m;

    location / {
        proxy_pass http://127.0.0.1:8787;
        proxy_http_version 1.1;

        proxy_set_header Host              $host;
        proxy_set_header X-Real-IP         $remote_addr;
        # ↓ 这一行是限速能不能正常工作的关键：
        #   $proxy_add_x_forwarded_for 会把访客的真实 IP **追加到最右边**，
        #   DesignKit 就是从右边数第 1 段取真实 IP 的（对应 DESIGNKIT_TRUSTED_PROXY_HOPS=1）。
        proxy_set_header X-Forwarded-For   $proxy_add_x_forwarded_for;
        # ↓ 这一行决定取图 Cookie 带不带 Secure 标记。少了它，
        #   浏览器可能把凭证跟着明文请求带出去。
        proxy_set_header X-Forwarded-Proto $scheme;

        # 上传大图、生成任务查询都可能慢一点，默认 60 秒偏紧
        proxy_read_timeout 300s;
        proxy_send_timeout 300s;
    }
}
```

启用并检查：

```bash
sudo ln -sf /etc/nginx/sites-available/designkit /etc/nginx/sites-enabled/designkit
sudo rm -f /etc/nginx/sites-enabled/default   # 去掉 Nginx 自带的欢迎页
sudo nginx -t          # 必须打印 syntax is ok / test is successful
sudo systemctl reload nginx
```

**申请证书**（把域名和邮箱换成你自己的）：

```bash
sudo certbot --nginx -d designkit.example.com -m 你的邮箱@example.com --agree-tos --redirect
```

`--redirect` 会让 certbot 顺手加上「http 自动跳 https」。
这条命令跑完，certbot 会**自动改写**上面那个配置文件：加上 443 端口、
证书路径、以及 80 跳 443 的规则。你原来写的 `client_max_body_size` 和
那几行 `proxy_set_header` 会被原样保留在 443 那一段里。

**确认改写结果**（这一步别省，确认那几行代理头还在）：

```bash
sudo grep -E "listen|X-Forwarded|client_max_body_size" /etc/nginx/sites-available/designkit
```

应该能看到 `listen 443 ssl`、两行 `X-Forwarded-*`、以及 `client_max_body_size 64m`。
万一那几行 `proxy_set_header` 不见了，把它们手工补回 443 那个 `location /` 里，
再 `sudo nginx -t && sudo systemctl reload nginx`。

**证书自动续期**：apt 装的 certbot 自带一个定时任务，你只要确认它在：

```bash
systemctl list-timers | grep certbot     # 应该能看到一行 certbot.timer
sudo certbot renew --dry-run             # 演练一次续期，最后打印 "Congratulations" 才算过
```

`--dry-run` 是演练，不会真的换证书，也不会消耗 Let's Encrypt 的申请次数。
**这一条一定要跑**——证书 90 天到期，续期没配好的话，三个月后的某一天全公司突然打不开，
而那天你多半已经忘了这套东西是怎么装的了。

### 3.3 反代之后，必须在 DesignKit 里配的 4 项

> ✅ 这一节的四项我**都在本机验证过**：改了会怎样、不改会怎样，见第 9 节。

套上反向代理之后，DesignKit 看到的「请求从哪儿来」全变了，
有四项配置必须跟着改。**这四项没有一项会报错**——配错了只会在某天以一种
看不出原因的方式出问题，所以请照着表一项项对。

| # | 配什么 | 在哪里配 | 填什么 | 配错的表现 |
|---|---|---|---|---|
| ① | 信任几层反向代理<br>`DESIGNKIT_TRUSTED_PROXY_HOPS` | **`.env` 文件**（网页设置页里**没有**这一项） | 一层 Nginx/Caddy 填 `1`；前面还有 CDN 填 `2` | 填 `0`：全站所有人共用一个限速桶，**一个人输错几次密码，全公司都登不上** |
| ② | 谁可以自称反向代理<br>`DESIGNKIT_FORWARDED_ALLOW_IPS` | **`.env` 文件** | 反代和 DesignKit 在同一台机器上（本文的情况）填 `172.16.0.0/12`；反代在另一台机器上填**那台机器的内网 IP** | 填错：代理头被忽略，效果同上；**填 `*` 最糟**，见下面的警告 |
| ③ | 对外访问地址 | 网页「系统设置 → **安全与网络**」 | `https://你的域名`（末尾不带斜杠） | 对外 API 和回调里的图片链接指向服务器自己，对接方拿到一堆打不开的链接，而系统这边**一条报错都没有** |
| ④ | 允许跨站调用的地址 | 网页「系统设置 → **图片访问与多用户**」 | `https://你的域名`（要对接 ERP 就用英文逗号加上 ERP 的地址） | 留着默认的 `*`：别人的网页可以拿着你同事的登录状态来读数据 |

> 🚨 **`DESIGNKIT_FORWARDED_ALLOW_IPS` 永远不要填 `*`。**
> 填了之后程序会无条件相信 `X-Forwarded-For` 这个头，并且取**最左边**那一段——
> 而最左边那一段是**客户端自己随手写的**，谁都能伪造。后果是登录限速、注册限速、
> 短信限速**全部当场作废**（攻击者每次请求换一个假 IP），而且日志里记的来源 IP 全是假的，
> 事后连是谁干的都查不出来。

> 💡 **为什么 `172.16.0.0/12`**：反代在宿主机上，DesignKit 在容器里，
> 请求经过 Docker 的网络转发之后，容器看到的来源地址是 Docker 网桥的地址，
> 通常落在 `172.16.x.x` ~ `172.31.x.x` 这个段里。填这个网段就是「只相信从本机
> Docker 网桥进来的请求」。想知道确切地址，看一眼应用日志里记的来源 IP 即可。

> 💡 **①和②是两件事，两个都要配**：②决定「要不要读 `X-Forwarded-For` 这个头」，
> ①决定「读了之后从右边数第几段才是真实访客」。只配一个是不够的。

**①②改完必须重新创建容器才生效**（改 `.env` 之后不要用 `restart`，它不会重新读这个文件）：

```bash
cd /opt/designkit
sudo docker compose up -d
```

**④改完也必须重启服务才生效**（页面会提示保存成功，但要重启才真正起作用，这是正常的，
不要反复保存）：

```bash
cd /opt/designkit
sudo docker compose up -d
```

### 3.4 怎么确认反代真的配对了

> ✅ 下面这两条自查我在本机验证过（用 curl 直接构造代理头，观察系统行为）。

**自查一：取图凭证有没有带 `Secure`**

在服务器上执行（把域名换成你的；密码是**输入时不显示**的，
这样写是为了不让密码留在命令历史里）：

```bash
read -s -p "管理员密码（输入时不显示，输完直接回车）: " DK_PASS; echo
curl -s -i -X POST https://你的域名/api/web/auth/login \
  -H 'Content-Type: application/json' \
  -d "{\"username\":\"admin\",\"password\":\"$DK_PASS\"}" | grep -i set-cookie
unset DK_PASS
```

打印出来的那行**末尾必须有 `Secure`**，像这样：

```
set-cookie: dk_files=...; HttpOnly; Max-Age=604800; Path=/files; SameSite=lax; Secure
```

**没有 `Secure`** 就说明反代没有把 `X-Forwarded-Proto: https` 传进来
（Nginx 少了那行 `proxy_set_header X-Forwarded-Proto $scheme`）。
后果是取图凭证有可能被浏览器跟着明文请求带出去。

**自查二：限速看到的是不是真实 IP**

这一条没有一条命令能直接看出来，但有一个**很典型的反面现象**可以判断：

> 如果某天有同事说「我明明没输错密码，一登录就提示『尝试次数过多，请 15 分钟后再试』」，
> 而且是**好几个人同时**这样——那基本可以断定 ① 或 ② 配错了，
> 全站所有人被算成同一个人。

想主动验证的话，让一位同事从**手机 4G 网络**（不要连公司 WiFi）故意输错 3 次密码，
然后你在电脑上正常登录。你应该**完全不受影响**。如果你也被锁了，就是配错了。

---

## 第 4 步：防火墙

> ⚠️ 这一节的命令**我没有在真机上验证过**。

**目标：外面只能看到 22（SSH）、80、443 三个端口。**
8787（DesignKit）和 5432（数据库）一个都不能露出去。

```bash
sudo ufw default deny incoming
sudo ufw default allow outgoing
sudo ufw allow OpenSSH        # ⚠️ 这一条一定要在 enable 之前执行，否则你会把自己关在门外
sudo ufw allow 80/tcp
sudo ufw allow 443/tcp
sudo ufw enable               # 会问 y/n，输 y
sudo ufw status verbose       # 确认列出来的只有 22 / 80 / 443
```

> 🚨 **`sudo ufw allow OpenSSH` 忘了执行会把你自己锁在服务器外面**，
> 只能去云服务商的控制台开「VNC / 远程连接」进去救。先执行它，再 `enable`。

### 4.1 一个必须知道的坑：Docker 会绕过 ufw

**`ufw deny 8787` 挡不住 Docker 发布出去的端口。**
Docker 直接往 iptables 里插自己的规则，那条链在 ufw 的规则**之前**就被处理掉了。
也就是说：如果 `.env` 里 `DESIGNKIT_BIND_HOST` 还是默认的 `0.0.0.0`，
哪怕你的 ufw 里写着「拒绝一切」，8787 在公网上**照样是开的**。

**所以真正的护栏是第 2 步里那一行 `DESIGNKIT_BIND_HOST=127.0.0.1`**，
它让 8787 只监听本机，Docker 压根不会往公网发布这个端口。防火墙是第二道保险，不是第一道。

**数据库不用管**：`docker-compose.yml` 里的数据库容器**根本没有对外映射端口**，
只在 compose 内部网络里可达。请也不要自己给它加端口映射——数据库直接连公网是灾难。

### 4.2 验证一遍（在**你自己的电脑**上执行，不是服务器上）

```bash
# 这两条都应该失败（连接被拒绝或超时），失败才是对的
curl -m 5 http://你的域名:8787/healthz
nc -z -w 5 你的域名 5432 && echo "危险：数据库端口是开的" || echo "正常：数据库端口不通"

# 这一条应该成功，打印出登录页的 HTML
curl -sI https://你的域名 | head -1
```

### 4.3 云服务商的安全组

阿里云、腾讯云、AWS 这些都还有一层**独立于服务器之外**的「安全组 / 防火墙」，
在网页控制台里配。它和服务器上的 ufw 是**两道各自独立的门**，两道都要过。

到控制台把入方向规则改成：只放行 `22`、`80`、`443`，其余全拒。
安全组是在服务器**之外**生效的，所以它**能**挡住上面说的「Docker 绕过 ufw」那个问题——
是一道很值得配的保险。

---

## 第 5 步：每天自动备份

> ⚠️ 这一节的 systemd / cron 配置**我没有在真机上验证过**。
> 其中的备份命令本身和 README、群晖那份文档里的是同一套（那套是验证过的），
> 这里只是把「DSM 任务计划」换成了 systemd 定时器。

**要备的是四样，缺一不可**：

| 内容 | 在哪儿 |
|---|---|
| 数据库（模板、任务、设置、成员账号、成员的网关 Key 密文） | **PostgreSQL 容器的数据卷里**，不在 `data/` 目录 |
| 图片（上传图、生成图、缩略图） | `data/` 下的子目录 |
| **`data/.secret_key`**（登录令牌与回调签名的主密钥） | `data/` 里的隐藏文件，`ls` 默认看不见 |
| **`data/.enc_key`**（解开成员个人网关 Key 的钥匙） | 同上 |

> 🚨 **只拷 `data` 目录备不到数据库；只导数据库备不到那两把钥匙。**
> 少了 `.enc_key` 的后果是「**全体成员突然都不能生图**」，
> 而数据库看起来完好无损、界面上每个人还明明白白写着「已配置 Key」——
> 这种故障从现象上完全看不出根因。下面的脚本两样都会备，而且会自检。

### 5.1 备份脚本

```bash
sudo nano /usr/local/bin/designkit-backup.sh
```

整段粘贴进去：

```bash
#!/bin/bash
# DesignKit 每日备份。四样都备：数据库、图片、.secret_key、.enc_key。
set -euo pipefail

APP_DIR=/opt/designkit
BACKUP_DIR=/opt/designkit-backup
KEEP_DAYS=14

mkdir -p "$BACKUP_DIR"
DATE=$(date +%Y%m%d)
cd "$APP_DIR"

# ── 数据库 ──
# 先导成 .part 临时文件，成功了再改成正式名字。
# 为什么不直接 `pg_dump | gzip > 文件`：数据库容器没起来时 pg_dump 会失败，
# 但 gzip 照样会生成一个看起来正常、其实是空的 .gz，整条命令还返回「成功」——
# 等到真要恢复那天才发现备份是空的。
#
# --clean --if-exists 让导出的 SQL 自带「先删旧表」的语句，
# 否则这份备份只能灌进一个空库，往有数据的库里灌会全程报「表已存在」。
docker compose exec -T db pg_dump --clean --if-exists -U designkit designkit \
  > "$BACKUP_DIR/db-$DATE.sql.part"
gzip -f "$BACKUP_DIR/db-$DATE.sql.part"
mv "$BACKUP_DIR/db-$DATE.sql.part.gz" "$BACKUP_DIR/db-$DATE.sql.gz"

# ── 图片 + 两把钥匙（整个 data 目录，隐藏文件也在里面）──
tar czf "$BACKUP_DIR/images-$DATE.tar.gz" -C "$APP_DIR" data

# ── 自检：备出来的东西不是空的。这三条别删，它们是这个脚本存在的意义 ──
if ! gunzip -c "$BACKUP_DIR/db-$DATE.sql.gz" | grep -q 'CREATE TABLE'; then
  echo "备份自检失败：数据库备份里一张表都没有，这份备份是坏的" >&2
  exit 1
fi
if ! tar tzf "$BACKUP_DIR/images-$DATE.tar.gz" | grep -q 'data/\.secret_key$'; then
  echo "备份自检失败：没备到 data/.secret_key" >&2
  exit 1
fi
if ! tar tzf "$BACKUP_DIR/images-$DATE.tar.gz" | grep -q 'data/\.enc_key$'; then
  echo "备份自检失败：没备到 data/.enc_key" >&2
  exit 1
fi

# ── 只保留最近 14 天 ──
find "$BACKUP_DIR" -type f \( -name '*.gz' -o -name '*.part' \) -mtime +$KEEP_DAYS -delete

echo "备份完成：$BACKUP_DIR/db-$DATE.sql.gz 和 $BACKUP_DIR/images-$DATE.tar.gz"
```

给它执行权限：

```bash
sudo chmod +x /usr/local/bin/designkit-backup.sh
```

> 📌 这个脚本**必须以 root 身份运行**（它要使唤 Docker）。下面的 systemd 和 cron
> 本来就是以 root 跑的，所以不用管；想手工跑就用 `sudo /usr/local/bin/designkit-backup.sh`。

### 5.2 让它每天自动跑（systemd 定时器）

```bash
sudo nano /etc/systemd/system/designkit-backup.service
```

```ini
[Unit]
Description=DesignKit 每日备份
# 备份要连数据库容器，所以必须等 Docker 起来
After=docker.service
Requires=docker.service

[Service]
Type=oneshot
ExecStart=/usr/local/bin/designkit-backup.sh
```

```bash
sudo nano /etc/systemd/system/designkit-backup.timer
```

```ini
[Unit]
Description=每天凌晨 3 点跑一次 DesignKit 备份

[Timer]
OnCalendar=*-*-* 03:00:00
# Persistent=true 的意思是：到点时机器正好关着，开机后补跑一次。
# 没有它的话，凌晨 3 点服务器在重启，这一天的备份就悄悄没了。
Persistent=true

[Install]
WantedBy=timers.target
```

启用：

```bash
sudo systemctl daemon-reload
sudo systemctl enable --now designkit-backup.timer
systemctl list-timers | grep designkit    # 能看到下次运行时间就对了
```

### 5.3 **立刻手工跑一次**（这一步别省）

```bash
sudo systemctl start designkit-backup.service
sudo systemctl status designkit-backup.service --no-pager
```

最后应该看到 `备份完成：...`。如果看到 `备份自检失败：...`，
说明有一样没备到，**现在就查清楚**——别等到真出事那天。

看完整日志：

```bash
sudo journalctl -u designkit-backup.service -n 50 --no-pager
```

### 5.4 不想用 systemd？cron 版本

```bash
sudo crontab -e
```

在最后加一行：

```cron
0 3 * * * /usr/local/bin/designkit-backup.sh >> /var/log/designkit-backup.log 2>&1
```

> ⚠️ **cron 出错是完全静默的**，不会有任何提示。用 cron 的话，请每隔一段时间
> 自己看一眼 `sudo tail -20 /var/log/designkit-backup.log`，
> 确认最后一行是「备份完成」而不是「备份自检失败」。
> systemd 那条路可以用 `systemctl status` 一眼看出成败，所以更推荐。

### 5.5 备份要往别的地方再拷一份

**备份和数据在同一块硬盘上，硬盘坏了就一起没了。** 加一条往别处同步：

```bash
# 举例：每天备份完之后同步到另一台机器（需要先配好免密登录）
rsync -az --delete /opt/designkit-backup/ 备份用户@另一台机器:/backup/designkit/
```

把这条加到备份脚本最后一行，或者单独做一个定时任务。

---

## 第 6 步：恢复演练（每半年一次）

> ⚠️ 这一节**我没有在真机上验证过**（本机没有 Docker，起不了演练环境）。
> 但其中「演练环境要先关掉哪几个开关、SQL 怎么写」我在本机的数据库上验证过，
> 程序确实会读到改后的值（见第 9 节）。

**没有演练过的备份，等于没有备份。**
真正出事那天才第一次执行恢复，你会同时遇到三件事：数据没了、命令不熟、还在着急。

演练的原则是：**在一套完全独立的环境里恢复，绝对不碰正在跑的这一套。**

### 6.1 为什么不能「就地恢复试试」

两个原因，第二个更要命：

1. 恢复会把数据库清空再灌回去，**演练一次就把演练之后产生的数据抹掉了**。
2. 演练环境启动后，它会**照着备份里的设置真的伸手到外面去**：
   - 备份里 `provider` 是 `openai` → 它可能真的去调生图网关，**真的花钱**；
   - 备份里「自动开通」是开着的 → 它可能真的跑到网关后台去建号；
   - 备份里有没送达的回调 → 它启动时会**补投一次**，对接方的 ERP 会收到重复通知。

所以下面的步骤里有一段 SQL，专门在启动应用**之前**把这三件事掐掉。

### 6.2 搭一套演练环境

```bash
# 1) 建演练目录，把配置复制过来
sudo mkdir -p /opt/designkit-drill
sudo cp /opt/designkit/docker-compose.yml /opt/designkit-drill/
sudo cp /opt/designkit/.env /opt/designkit-drill/
cd /opt/designkit-drill

# 2) 去掉写死的容器名。
#    docker-compose.yml 里写着 container_name: designkit / designkit_db，
#    不删掉的话演练容器会和正在跑的那套**撞名字**，起不来。
sudo sed -i '/container_name:/d' docker-compose.yml

# 3) 改演练用的 .env
sudo nano .env
```

`.env` 里改这几项（其余原样不动）：

```bash
DESIGNKIT_DATA_LOCATION=/opt/designkit-drill/data   # 独立的数据目录，绝不能指向正式那个
DESIGNKIT_HOST_PORT=18787                           # 换个端口，避免和正式的 8787 撞
DESIGNKIT_BIND_HOST=127.0.0.1                       # 只让本机访问
DESIGNKIT_WORKERS=1
```

> 🚨 **`DESIGNKIT_DATA_LOCATION` 一定要改。** 忘了改的话，演练环境会往**正式的
> data 目录**里写东西，第 6.3 步的解压更会**直接覆盖正式环境的两把钥匙**。
> 改完请再看一眼确认。

### 6.3 恢复到演练环境

```bash
cd /opt/designkit-drill

# 1) 只启动数据库，先不要启动应用（原因见 6.1 的第 2 条）
sudo docker compose -p designkit-drill up -d db
sleep 20

# 2) 灌回数据库（把日期换成你要恢复的那天）
#    -v ON_ERROR_STOP=1 一定要带：出错就立刻停下并返回失败。
#    不带的话 psql 会一路报错一路往下跑，最后给你一个新旧混杂的库，退出码还是 0。
gunzip -c /opt/designkit-backup/db-20260810.sql.gz \
  | sudo docker compose -p designkit-drill exec -T db \
      psql -v ON_ERROR_STOP=1 -U designkit -d designkit

# 3) 把图片和两把钥匙解到演练目录
#    ⚠️ 这一条会往 -C 指定的目录里写 data/，务必确认写的是 designkit-drill
sudo tar xzf /opt/designkit-backup/images-20260810.tar.gz -C /opt/designkit-drill

# 4) 【关键】把三件会「伸手到外面」的事掐掉，再启动应用
sudo docker compose -p designkit-drill exec -T db \
  psql -v ON_ERROR_STOP=1 -U designkit -d designkit <<'SQL'
-- 生图切成模拟模式：演练绝不能真的调网关、真的花钱。
-- 用 INSERT ... ON CONFLICT 而不是 UPDATE：备份里如果压根没存过这一项，
-- UPDATE 会影响 0 行、悄悄什么都没做，而程序会退回去用「真实调用」那个默认值。
INSERT INTO app_settings (key, value) VALUES
  ('provider',                '{"v": "mock"}'),
  ('sub2api_auto_provision',  '{"v": false}'),
  ('inspiration_auto_sync',   '{"v": false}'),
  ('self_register_enabled',   '{"v": false}'),
  ('phone_register_enabled',  '{"v": false}')
ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value;

-- 清掉所有回调地址：否则演练环境一启动就会把「未送达的回调」补投给对接方的 ERP，
-- 对方会收到一批重复的通知，而且完全看不出是演练发出来的。
UPDATE generation_jobs SET callback_url = NULL;
SQL

# 5) 现在才启动应用
sudo docker compose -p designkit-drill up -d
```

### 6.4 验收：确认恢复出来的东西是好的

```bash
# 服务活着
curl -s http://127.0.0.1:18787/healthz     # 应该打印 {"status":"ok"}
```

然后在**你自己的电脑**上开一条隧道看界面：

```bash
ssh -L 18787:127.0.0.1:18787 你的用户名@服务器IP
```

浏览器打开 <http://127.0.0.1:18787>，**逐条确认**：

- [ ] 能用备份那天的密码登录（密码是备份那天的，不是现在的）
- [ ] 「历史记录」里能看到备份那天之前的生成记录
- [ ] **随便点开一张历史图，图能正常显示**——这一条最重要，它同时证明了
      图片文件和 `.secret_key` 都恢复对了
- [ ] 「成员账号」页里的人都在，而且状态显示正常
- [ ] 「系统设置 → 生图服务」里显示的接口地址和模型名，和你记忆中的一致

> 💡 图能显示但页面提示要重新登录，是**正常现象**：换回了备份那天的 `.secret_key`，
> 之前那把钥匙签发的登录状态自然失效了。

### 6.5 拆掉演练环境

**演练完一定要拆干净**，否则它会一直占着内存和磁盘：

```bash
cd /opt/designkit-drill
sudo docker compose -p designkit-drill down -v      # -v 连演练用的数据库卷一起删
cd /
sudo rm -rf /opt/designkit-drill
```

> ⚠️ 执行前**再确认一遍你在 `/opt/designkit-drill`，不是 `/opt/designkit`**。
> `down -v` 会把数据库卷删掉，在正式目录里执行就是把生产数据删了。

### 6.6 真出事时怎么恢复正式环境

演练走通之后，真出事那天的步骤见
[README「怎么恢复」](../README.md#怎么恢复)，**要整段照着做**。
和演练的唯一区别是：正式环境**不做** 6.3 的第 4 步（那些开关本来就该保持原样），
但**必须先停应用**：

```bash
cd /opt/designkit
sudo docker compose stop designkit     # 不停的话它占着数据库连接，清库会卡住
# …照 README 的步骤清库、灌回、解压…
sudo docker compose start designkit
```

> **先分清你要的是哪一种回退**：绝大多数「升级后有毛病」的情况，
> 数据本身是好的，只要把镜像钉回旧版本就行（见第 7 步），**根本不用碰备份**。
> 「灌回备份」会把备份那天之后的全部记录抹掉，是最后手段。

---

## 第 7 步：升级与回滚

> ⚠️ 这一节**我没有在真机上验证过**，但流程和 README、群晖那份文档里的完全一致。

### 7.1 升级前必做两件事，顺序不能反

**① 先备份**：

```bash
sudo systemctl start designkit-backup.service
sudo systemctl status designkit-backup.service --no-pager    # 确认是「备份完成」
```

**② 再钉住当前版本，并抄在纸上**：

```bash
cd /opt/designkit
sudo docker inspect --format '{{index .Config.Labels "org.opencontainers.image.revision"}}' \
  designkit | cut -c1-7
```

打印出来是 7 位字符（例如 `6ca035d`）。**把它抄在纸上**，前面加 `sha-`，
然后编辑 `.env`，把 `DESIGNKIT_VERSION=latest` 改成 `DESIGNKIT_VERSION=sha-6ca035d`，
执行一次 `sudo docker compose up -d` 确认这一版能正常起来。

> **为什么必须钉版本**：`latest` 今天指向的东西和上周不是一回事。
> 不钉的话，新版出问题时你连「该退回哪一版」都说不清楚。

### 7.2 升级

```bash
cd /opt/designkit
# 把 .env 里的 DESIGNKIT_VERSION 改成 latest（或新版本的标签），然后：
sudo docker compose pull
sudo docker compose up -d
sudo docker compose ps          # 两个都要 Up (healthy)
curl -s http://127.0.0.1:8787/healthz
```

升级会自动补数据库新增的列，不需要你手动做什么。

### 7.3 新版本有问题，怎么退回去

```bash
cd /opt/designkit
sudo nano .env      # 把 DESIGNKIT_VERSION 改回纸上抄的那个 sha-xxxxxxx
sudo docker compose up -d
```

**数据不用动**：旧版程序跑在升级过的数据库上是正常的，不会丢记录。
这是风险最小的一条回退路径，绝大多数情况用它就够了。

只有「数据本身也要退回到备份那天」时，才走第 6.6 节的灌回备份。

---

## 第 8 步：放人进来之前的检查清单

在给同事发地址、或者打开自助注册之前，**整份过一遍**。

> ✅ 这一节里每一条的开关位置和实际拦截行为，我都在本机跑起来验证过（见第 9 节）。

### 8.1 系统会**强制**你做到的（做不到就打不开注册）

这三条是**写在代码里的硬闸门**，不是提醒。想打开自助注册（邀请码、手机号两条路都算）
而其中任何一条没满足，保存的时候会被直接拒绝，并把还差哪几条一条条列给你看。
**闸门是双向的**：注册开着的时候，你也改不回不安全的值。

| # | 要求 | 在哪里 | 默认值 |
|---|---|---|---|
| ① | 「允许访问内网图片和回调地址」必须**关掉** | 系统设置 → **安全与网络** | 默认**开着**，要你自己去关 |
| ② | 「对外访问地址」必须是 **`https://你的域名`** | 系统设置 → **安全与网络** | 默认是本机地址，必须改 |
| ③ | 「图片链接需要凭证才能打开」必须**开着** | 系统设置 → **图片访问与多用户** | 默认就是开的，不用动 |

**各自不做的后果**（这也是它们被做成硬闸门而不是提醒的原因）：

- ① 开着，注册进来的陌生人可以让本系统**替他去访问你内网里的任意地址**，
  包括生图网关的管理后台——那上面能建号、能改余额、能读别人的明文 Key。
- ② 填错，每个新用户拿到的图片链接都指向他自己的电脑，全部打不开，
  **而系统这边一条报错都不会有**。公网上还必须是 `https`：注册页要填密码，
  `http` 上是明文过网。
- ③ 关掉，知道地址就能下载任何人的商品图和出图结果。内部几个人用还能靠
  「地址没人知道」凑合，谁都能注册之后就等于谁都能进来翻别人的图。

> 💡 **不打开自助注册的话，这三条系统不会强制**——但②和③在公网上照样该做，
> ①在你要对外提供 ERP 接口时也该关。清单里列出来是因为它们最容易漏。

### 8.2 系统**不强制**、但漏了会出事的

- [ ] **管理员密码已经改掉**，不是还在用 `admin123456`（第 2.3 步；这是公开文档里写着的密码）
- [ ] **`DESIGNKIT_BIND_HOST=127.0.0.1`**，8787 没有直接暴露在公网（第 4.2 步验证过）
- [ ] **`DESIGNKIT_TRUSTED_PROXY_HOPS` 已按反代层数填对**（第 3.3 节①）。
      不做的话限速形同虚设，而且**一个人被限速会连累全公司登不上**
- [ ] **`DESIGNKIT_FORWARDED_ALLOW_IPS` 已填成反代的地址，且不是 `*`**（第 3.3 节②）
- [ ] **「允许跨站调用的地址」已从 `*` 改成你自己的域名**（系统设置 → 图片访问与多用户）。
      ⚠️ 改完**必须重启服务**（`sudo docker compose up -d`）才生效，
      页面提示保存成功也不会立刻起作用，这是正常的，不要反复保存
- [ ] **HTTPS 证书能自动续期**（第 3.1 / 3.2 节最后那条验证命令跑过了）
- [ ] **备份定时任务已建好、手工跑过一次、而且自检通过**（第 5.3 步）
- [ ] **恢复演练做过一次**（第 6 步）。没演练过的备份不算备份
- [ ] **每个同事一个账号，不要共用**。共用的话生成记录混在一起分不清是谁的，
      任何一个人离职都得让全组改密码
- [ ] **决定钱怎么算**：大家花一个账户的钱 → 什么都不用做（默认就是）；
      各花各的 → 先在「成员账号」页给**每个人**配好网关 Key，**最后**才去
      「系统设置 → 图片访问与多用户」把「生图费用怎么算」改成「每人一把 Key」。
      顺序反了的话，没配 Key 的人一点生成就被拦住

### 8.3 如果要开手机号注册，再加两条

- [ ] **「短信服务」已经从「调试模式」切到「阿里云」**。
      调试模式下验证码是**直接显示在注册页上**的，等于没有验证——
      公网上谁都能注册进来。设置页上会显著标出当前处在哪种模式，上线前确认它写着「真实发送」
- [ ] **已经点过「试发一条」并真的收到短信**。这是唯一能确认「真的能发出去」的办法。
      详见 [README 第九章](../README.md#九让别人用手机号自己注册短信验证码)

> 🚨 短信是花钱的，而且被刷的代价不只是钱：阿里云会以「疑似恶意」
> **把你的短信签名封掉**，重新申请要按工作日算。三层限速的默认值已经调得比较保守，
> 别调大。上面那条 `DESIGNKIT_TRUSTED_PROXY_HOPS` 对「同一 IP 每小时」这一层
> 是决定性的——填 `0` 的话这一层会退化成「全站每小时 20 条」，把正常用户一起挡住。

### 8.4 上线后隔一天回来看一眼

- `sudo docker compose ps` —— 两个容器都还是 `Up (healthy)` 吗
- `sudo docker compose logs --tail 100 designkit` —— 有没有反复出现的报错
- `systemctl list-timers | grep designkit` —— 备份定时器还在，而且跑过一次了
- `df -h` —— 磁盘还有多少空间

---

## 第 9 步：这份文档验证到什么程度

写这份文档的机器上**没有 Docker、没有 Caddy、没有 Nginx、没有公网服务器**，
所以下面分得很清楚，请按这个来判断哪些地方要多留个心眼。

### ✅ 我真的跑起来验证过的（在本机用临时数据目录 + 模拟生图模式）

| 验证了什么 | 怎么验的 | 结论 |
|---|---|---|
| `DESIGNKIT_TRUSTED_PROXY_HOPS` 这个环境变量真的有用 | 带着这个变量启动服务，再从接口读回设置 | 填 `1` 时读回来就是 `1` |
| 网页设置页里**没有**「信任几层反代」这一项 | 搜遍前端源码 | 确认没有，所以文档里说它**只能在 `.env` 里配** |
| 填 `1` 之后限速取的是不是真实 IP | 构造一个伪造的 `X-Forwarded-For: 1.2.3.4, 203.0.113.9` 去登录，再看数据库里的限速记录 | 记的是 `203.0.113.9`（反代追加的真实段），伪造的 `1.2.3.4` 被无视 |
| `X-Forwarded-Proto` 决定取图 Cookie 带不带 `Secure` | 带这个头和不带这个头各登录一次，对比响应 | 带 `https` 时 Cookie 末尾有 `Secure`，不带时没有——所以反代必须传这个头 |
| 8.1 那三道硬闸门的**实际行为和报错文案** | 在三条都不满足、只满足一条、地址填成 `http://` 等情况下分别去打开注册 | 每种情况都被拒绝，并逐条列出还差什么；三条都满足后能存进去 |
| 闸门是**双向**的 | 注册打开之后，再去把「允许访问内网」改回开、把「图片凭证」改回关 | 两次都被拒绝，理由写得很清楚 |
| 「允许跨站调用的地址」的格式要求 | 填 `designkit.example.com`（不带协议头）和 `https://designkit.example.com` 各存一次 | 前者被拒并提示要带 `http://` 或 `https://`；后者存得进去 |
| `docker-compose.yml` 能正确解析，端口映射确实受 `DESIGNKIT_BIND_HOST` 控制 | 解析这个文件 | 映射写的是 `${DESIGNKIT_BIND_HOST:-0.0.0.0}:${DESIGNKIT_HOST_PORT:-8787}:8787`，填 `127.0.0.1` 就只监听本机 |
| 数据库端口确实没有对外映射 | 读 `docker-compose.yml` | 数据库容器没有 `ports` 配置，只在 compose 内部网络可达 |
| 多进程启动能起来，而且「同时生成几张」不会被乘几倍 | 用第 2 步里那套参数（含 `--workers 2`）启动，看日志 | 两个进程都起来了，只有一个拿到「派发权」真的发任务，另一个待命 |
| 第 6.3 步演练用的那段 SQL 写法对不对 | 在本机数据库上执行同样的语句，再从接口读回设置 | 五项开关都变成了预期的值，程序确实读到了 |

### ❌ 我**没有**在真机上验证过的

- **第 1 步**：买机器、装 Docker 的命令（来自 Docker 官方文档）
- **第 3 步**：Caddy 和 Nginx 的安装、配置文件语法、certbot 申请证书与自动续期。
  这台机器上没有这三个程序，连它们自带的语法检查都跑不了。
  **所以每种方案我都给了官方的检查命令**（`caddy validate` / `nginx -t` /
  `certbot renew --dry-run`），**请务必先跑那一条再重启服务**
- **第 4 步**：ufw 防火墙命令、以及「Docker 绕过 ufw」这个结论
  （这是 Docker 一个众所周知的行为，但我没在真机上复现过）
- **第 5 步**：systemd 定时器、cron 的配置。备份命令本身和 README、
  群晖那份文档里的是同一套
- **第 6 步**：整套恢复演练（起不了 Docker）。其中 SQL 那一段在本机数据库上验过
- **第 7 步**：升级与回滚的 Docker 命令

### 建议的做法

**先在一台便宜的临时服务器上照着走一遍**（几块钱一天，走完就退），
把第 3、4、5、6 步真的做一次。走通之后再在正式机器上做，
那时你已经知道每一步该看到什么了。
