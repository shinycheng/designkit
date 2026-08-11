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

> **改完 `.env` 想生效，一律用 `docker compose up -d`**，不要用 `restart`。
> `restart` 只是把容器停了再开，不会重新读取 `.env`，改了等于没改。

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
  （那个窗口关不掉，不想现在设就点窗口左下角的「退出登录」）
- 进「系统设置 → 生图服务」填入网关 Key → **先点「保存此区域」** → 再点
  「测试已保存的连接」。顺序反了会提示「请先保存当前更改」，测试不会真的执行
- 进「灵感库」→ 点右上角「同步上游」，约 1-2 分钟拉回 1.4 万条提示词。
  同步过程中页面会显示进度，不要重复点

> 部署用的配置模板默认已是**真实生图**模式，Key 没填之前点生成会报
> 「尚未配置生图 API Key」。所以上面填 Key 这步要先做。

给同事开账号：左侧导航「成员」→「新建成员」。系统**不提供自助注册**，
所有账号都由你来建、初始密码由你当面给。详细步骤（含停用、重置密码、
给每个人配自己的网关 Key）见 [README 第七章](../README.md#七给同事开账号成员账号)。

---

## 第 6 步：设置每日自动备份（建议做）

⚠️ 要备的是**四样**：数据库、图片、`data/.secret_key`、`data/.enc_key`。
数据库在 Docker 数据卷里，**只拷 `data` 文件夹不会备份到数据库**；
后两个是隐藏文件（File Station 默认看不见），下面的 `tar` 命令会把它们
一起打包，不用另外操作，但**备完一定要按本节末尾的两条命令检查一遍**。

它们各自丢了会怎样：

| 文件 | 丢了的表现 |
|---|---|
| `data/.secret_key` | 所有人被踢下线要重新登录；发给 ERP 对接方的 Webhook Secret 全部作废，对方一路验签失败且没有任何提示 |
| `data/.enc_key` | **全体成员突然都不能生图**，而数据库看起来完好、界面上还写着「已配置 Key」。只能到网关后台把每个人的 Key 重新抄一遍再逐个填回去 |

DSM → 控制面板 → 任务计划 → 新增 → 计划的任务 → 用户定义的脚本

- 用户：`root`
- 计划：每天，选个你不用 NAS 的时间（比如凌晨 3 点）
- 脚本内容（整段复制粘贴）：

```bash
set -e
cd /volume1/docker/designkit
BACKUP_DIR=/volume1/docker/designkit-backup
mkdir -p "$BACKUP_DIR"
DATE=$(date +%Y%m%d)

# 数据库：先导成 .part 临时文件，成功了再改成正式名字。
# 为什么不直接 `pg_dump | gzip > 文件`：数据库容器没起来时 pg_dump 会失败，
# 但 gzip 照样会生成一个看起来正常、其实是空的 .gz，整条命令还返回「成功」，
# 任务计划里显示绿色的「已完成」——等真要恢复那天才发现备份是空的。
# 配合开头的 set -e，导出一失败脚本就中止，不会留下假备份，DSM 也会报失败。
#
# --clean --if-exists 让导出的 SQL 自带「先删旧表」的语句，
# 否则这份备份只能灌进一个空库，往有数据的库里灌会全程报「表已存在」。
docker compose exec -T db pg_dump --clean --if-exists -U designkit designkit > "$BACKUP_DIR/db-$DATE.sql.part"
gzip -f "$BACKUP_DIR/db-$DATE.sql.part"
mv "$BACKUP_DIR/db-$DATE.sql.part.gz" "$BACKUP_DIR/db-$DATE.sql.gz"

# 图片 + 两把钥匙：整个 data 目录，隐藏的 .secret_key / .enc_key 都在里面
tar czf "$BACKUP_DIR/images-$DATE.tar.gz" -C /volume1/docker/designkit data

# 只保留最近 14 天
find "$BACKUP_DIR" -type f \( -name '*.gz' -o -name '*.part' \) -mtime +14 -delete
```

**建好之后手工跑一次**（任务计划里选中它 → 运行），然后 SSH 上去检查一遍，
确认备份不是空的——这两条都要有输出：

```bash
cd /volume1/docker/designkit-backup
gunzip -c db-$(date +%Y%m%d).sql.gz | grep -c 'CREATE TABLE'
# ↑ 应该是个大于 0 的数字（表的数量）

tar tzf images-$(date +%Y%m%d).tar.gz | grep -E 'data/\.(secret|enc)_key$'
# ↑ 必须打印出**两行**：data/.enc_key 和 data/.secret_key。只有一行就是没备全
```

> 备份文件建议再往别的地方拷一份（另一块硬盘 / 另一台机器）。
> 放在同一台 NAS 上，硬盘坏了就一起没了。

---

## 第 7 步：让同事们用起来之前，先过一遍这个清单

只有你一个人用的时候，下面这些都不重要；一旦要给同事开账号，
每一条不做都会变成一次说不清原因的故障。

- [ ] **管理员自己的密码已经改掉了**，不是还在用 `admin123456`
- [ ] **每个同事一个账号，不要共用一个**。共用的话，生成记录混在一起分不清是谁的，
      任何一个人离职都得让全组改密码。建号方式见
      [README 第七章](../README.md#七给同事开账号成员账号)
- [ ] **初始密码当面给**，别发在群里。他登录后系统会强制他改掉
- [ ] **决定钱怎么算**：
      - 大家花一个账户的钱 → 什么都不用做（默认就是这样）
      - 各花各的 → 先在「成员账号」页给**每个人**配好网关 Key，
        **最后**再到「系统设置 → 图片访问与多用户」把「生图费用怎么算」
        改成「每人一把 Key」。顺序反了的话，没配 Key 的人一点生成就被拦住
- [ ] **（可选）想让系统自己去网关开号领 Key，先做完这两件事再让同事进来**：
      在「系统设置 → 网关自动开通」里填好网关后台地址、网关管理员 Key（**你自己**
      去网关后台生成，不要用任何文档里的示例值）、目标分组 id，保存并打开开关；
      然后 ① 点一次「测试能不能自动开通」，把它读回来的**你这台网关的实际设置**
      看一遍（后台模式 / 人机验证只要开着就一定不通）；
      ② 建一个测试成员，等它显示「已开通」后用这个账号**真的生成一张图**——
      自检只能证明网关愿意受理这把 Key，证明不了真能出图。
      详见 [docs/auto-provision.md](auto-provision.md)。
      **不开这个功能也完全没问题**，那就照上一条手工给每个人填 Key
- [ ] **`DESIGNKIT_PUBLIC_BASE_URL` 填的是同事们真正能打开的地址**
      （NAS 的局域网 IP，不是 `127.0.0.1`）。填错的表现是：你自己一切正常，
      同事那边图片全裂
- [ ] **备份任务已经建好并手工跑过一次**，而且用第 6 步末尾那两条命令检查过。
      成员的网关 Key 存在数据库里、解密钥匙在 `data/.enc_key`，
      两样缺一，恢复出来就是「全体都不能生图」
- [ ] **知道人走了怎么办**：用「停用」，不要想着删除（系统也没有删除功能）。
      停用是立刻生效的，而且是**一次断干净**——他的登录、他手上的图片链接、
      **以及他名下所有还开着的 ERP API Key** 同时失效，用这些 Key 对接的 ERP
      当场就会收到「API Key 无效或已停用」（停用时的提示会告诉你这次一共关掉了几把）。
      ⚠️ 这一步**不会自动还原**：以后把账号「启用」回来，那些 Key 仍然是停用状态，
      ERP 还是不通。要恢复对接，得到「API 对接」页把对应的 Key **逐把重新打开**。
      只想断掉某一条对接、不影响这个人继续用网页的，就别动账号，
      直接到「API 对接」页单独停那一把 Key

**怎么恢复**：见 [README 的「怎么恢复」](../README.md#怎么恢复)，**要整段照着做**。
里面有两处不能省，少哪一个都会得到一个「看起来正常、其实是坏的」数据库：

1. **灌备份之前先把数据库清空**——就是 README 恢复步骤里那条
   `DROP SCHEMA public CASCADE; CREATE SCHEMA public;`。不清空的话，
   旧备份会灌到一半停下来，而停下来之前它已经把库里的各种约束删光了，
   系统还能照常启动、页面看着一切正常，坏了却看不出来。
2. **`psql` 必须带 `-v ON_ERROR_STOP=1`**，否则出错也会一路往下跑，
   最后得到一个新旧数据混在一起的库，还显示成功。

---

## 以后怎么更新

代码推到 GitHub 后，GitHub 会自动构建新镜像。**但升级前有两步不能省**，
顺序照下面来。

### 第 1 步：先备份

把第 6 步那个备份任务手工跑一次（任务计划 → 选中 → 运行），
并按第 6 步末尾的两条命令确认备份不是空的。

升级会改数据库的表结构。万一新版本有问题，光把镜像换回旧版是不够的——
表结构已经变了，还得把数据也灌回去，那就只能靠这份备份。

### 第 2 步：钉住当前版本，并抄在纸上

`.env` 里的 `DESIGNKIT_VERSION` 默认是 `latest`（永远跟最新）。这个默认值的问题是：
出事要往回退的时候，你说不清楚「原来跑的是哪一版」——`latest` 今天指向的东西
和上周不是一回事。所以升级前先查出当前版本并钉住：

```bash
cd /volume1/docker/designkit

# 打印出来是 7 位字符，例如 6ca035d
sudo docker inspect --format '{{index .Config.Labels "org.opencontainers.image.revision"}}' designkit | cut -c1-7
```

把这 7 位字符**抄在纸上**，前面加 `sha-`（例如 `sha-6ca035d`），
然后编辑 `.env`，把这一行改成：

```
DESIGNKIT_VERSION=sha-6ca035d
```

> 每次推送代码，GitHub 都会自动为该次提交打一个 `sha-` 开头的镜像标签，
> 所以任何一版都能这样退回去。
> 如果上面那条命令打印出来是空的（早期镜像没带这个标记），就去 GitHub 仓库页面 →
> 右侧 **Packages** → `designkit`，在标签列表里挑日期对得上的 `sha-xxxxxxx`。

### 第 3 步：升级

想升到最新，把 `.env` 里的 `DESIGNKIT_VERSION` 改回 `latest`（或改成新版本的标签），
然后：

```bash
cd /volume1/docker/designkit
sudo docker compose pull && sudo docker compose up -d
```

数据库和图片都在卷 / `data` 目录里，换镜像不会丢。

### 新版本有问题，怎么退回去

把 `.env` 里的 `DESIGNKIT_VERSION` 改回纸上抄的那个 `sha-xxxxxxx`，然后：

```bash
cd /volume1/docker/designkit
sudo docker compose up -d
```

如果退回旧镜像后页面还是不正常（一般是因为新版本已经改过表结构），
就再按 [README 的「怎么恢复」](../README.md#怎么恢复) 把升级前那份备份灌回去。
**注意灌之前必须已经把版本钉回旧版**，否则新表结构配旧数据只会更乱。

---

## 出问题了怎么查

| 现象 | 原因与解决 |
|---|---|
| `docker compose up` 报 `请在 .env 里设置 POSTGRES_PASSWORD` | `.env` 没改密码，或者 `.env` 不在当前目录 |
| 拉镜像报 `denied` / `not found` | 镜像跟随公开仓库自动公开，正常不会出现。若真遇到，去 GitHub 仓库 → Packages → designkit → Package settings → Change visibility 确认是 Public |
| 浏览器打不开 8787 | 先在 NAS 上 `curl localhost:8787` 试试。通了说明是防火墙：控制面板 → 安全性 → 防火墙，放行 8787 |
| 容器一直 `unhealthy` | `sudo docker compose logs designkit`，多半是连不上数据库。注意 Docker **不会**自动重启 unhealthy 的容器，看到了要自己查 |
| 上传图片报错、生成失败 | 看 `sudo docker compose logs designkit` 里有没有 `[entrypoint]` 开头的警告：提示「目录里有本项目之外的内容」就是挂载目录挂错了，换成专门给 DesignKit 用的空目录；提示「修改属主失败」就在 NAS 上手工执行 `sudo chown -R PUID:PGID 该目录` |
| 图能生成，但在 File Station 里打不开 | `.env` 里 `PUID`/`PGID` 和你的账号对不上。改完 `.env` 后必须用 `sudo docker compose up -d`——**`restart` 不会重新读取 `.env`**，改了也没用 |
| 重启后有个任务卡在「生成中」，既不能重试也删不掉 | 服务重启时只会捞回已开始超过 75 分钟的中断任务。等一个多小时后再重启一次服务，它会自动回到队列重新执行 |
| 测试连接失败 | NAS 能不能连到网关：`curl http://192.168.31.235:8090/v1/models`。不通就是两台机器不在同一网段，或网关那台机器关机了 |
| 灵感库同步失败 | NAS 访问 GitHub 受限。到「系统设置 → 运行参数 → 同步代理」填代理地址（如 `http://192.168.31.x:7890`），保存后点「测试同步连接」。注意代理要填 **NAS 能访问到的地址**，不能填 `127.0.0.1`——那指的是容器自己 |
| 页面能打开，但图片全是裂的 | 图片现在要凭登录状态才能取。先退出登录再重新登录一次（会重新下发取图凭证）。还是不行就看 `DESIGNKIT_PUBLIC_BASE_URL` 是不是填成了 `127.0.0.1`——那样别人的浏览器会去找他们自己的电脑要图 |
| 某个成员点生成就报「你的账号还没有开通生图额度」 | 计费方式已经是「每人一把 Key」，但这个人还没配 Key。到「成员账号」页给他配上即可。**这种失败重试多少次都一样**，别让他反复点。开了「自动开通」的话，新人刚建号的那一两分钟里也会看到这句话，等一下再刷新；一直不好就看「成员账号」页那一行标的是什么（见下一条） |
| 「成员账号」页有人一直标着**「需要手工处理」** | 自动开通在这个人身上走不通。那一行下面写着人话原因（例如「他在网关那边开了两步验证」）。**有一小撮人永远开不通，这是设计的一部分**：点「改用手工填 Key」，到网关后台给他开一把 Key 粘进来，存进去立刻生效。各种原因的对照表见 [docs/auto-provision.md](auto-provision.md) 第六节 |
| 「系统设置」里自动开通面板顶部出现**红条「自动开通已暂停」** | 网关那边有前提条件不满足（后台模式开着、人机验证开着、合规承诺没签、管理员 Key 失效……），红条里写着具体是哪一个、该去哪里点什么。这是系统主动暂停的——不暂停的话，全站成员会被一路刷到失败上限、集体降级成「等人工发 Key」。处理完点红条下面的「我已处理，恢复自动开通」。**期间不影响任何人登录和生图**，只是新成员要你手工发 Key |
| 升级网关（Sub2API）之后，所有新成员都开不通了 | 自动开通依赖的是网关的实现细节而不是公开契约，升级可能整条失效（老成员照常生图，不受影响）。按 [docs/auto-provision.md](auto-provision.md) 第九节复核；在修好之前，照常用「改用手工填 Key」发 Key，业务不会停 |
| 恢复备份之后，所有人突然都不能生图 | 恢复时漏了 `data/.enc_key`（只灌了数据库）。把备份里的那个文件放回 `data` 目录再 `sudo docker compose up -d`。找不回来的话，只能到网关后台把每个人的 Key 重抄一遍、在「成员账号」页逐个重填 |
| 对接方说所有历史图片突然打不开 | 图片链接带签名、默认 7 天有效，且**停用一把 API Key 会让它发出的所有链接立刻失效**。先确认那把 Key 是不是被停用了；长期留存图片请让对方改用 `docs/erp-api.md` 3.6 节那条直取端点 |
| 在「系统设置」改了「允许跨站调用的地址」，保存成功但没生效 | 这一项**必须重启服务**才生效：`cd /volume1/docker/designkit && sudo docker compose up -d`。反复保存没有用 |
| 升级后页面顶部出现红色横幅**「历史数据归属回填没有完成」** | 系统**没有坏，数据也一条没丢**。升级时要给老记录补一句「这条属于谁」，这一步没做完；后果是一部分历史 ERP 任务和图片暂时**在网页上翻不到**（ERP 那头按 API Key 查不受影响）。横幅上跟着的那句话就是原因。处理：`cd /volume1/docker/designkit && sudo docker compose restart designkit` 重启一次，再刷新网页——重启会自动重试，补完横幅就自己消失了。这条命令可以反复执行，不会把数据改乱（这里不改配置，所以用 `restart` 就够了）。重启并刷新后横幅还在，就执行 `sudo docker compose logs designkit`，把最后几十行发给开发者。另：这条横幅只有管理员看得到，同事那边不会被吓到 |

---

## 安全提醒

- **不要**把 8787 端口转发到公网（路由器端口映射）。当前是 HTTP 明文，
  登录密码会在网上裸奔。需要外网访问请用群晖自带的 QuickConnect，
  或加一层反向代理配 HTTPS。
- `.env` 里有数据库密码，别把它上传到任何地方。
