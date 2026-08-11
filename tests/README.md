# 回归测试

每次改动代码后，跑一遍**全部 14 组**测试确认没把已有功能改坏，共 411 项：

| 组 | 文件 | 项数 | 要不要起服务 |
|---|---|---|---|
| 自动开通·状态机与可重入 | `test_provisioning.py` | 54 | 不要 |
| 端到端·多用户隔离 | `e2e_multiuser.py` | 52 | 要 |
| 图片访问鉴权（/files） | `test_file_access.py` | 50 | 不要 |
| 自动开通·端到端六条线 | `test_provision_regression.py` | 36 | 不要 |
| 端到端·安全边界 | `e2e_security.py` | 30 | 要 |
| 多用户隔离与按人计费 | `test_multiuser_isolation.py` | 29 | 不要 |
| 端到端·核心流程 | `e2e_core.py` | 27 | 要 |
| 自动开通·触发时机与定时任务 | `test_provision_schedule.py` | 27 | 不要 |
| Provider 兼容与报错可读性 | `test_provider_compat.py` | 25 | 不要 |
| 启动迁移（老库补列补索引） | `test_migrations.py` | 22 ⚠️ | 不要 |
| 灵感库与预处理 | `test_inspiration_convert.py` | 17 | 不要 |
| 出图比例护栏与画幅措辞 | `test_sizing.py` | 17 | 不要 |
| 跨库兼容与定时同步 | `test_db_and_scheduler.py` | 14 | 不要 |
| 按分类生成与同步代理 | `test_category_mode.py` | 11 | 不要 |

三组端到端要起服务，另外十一组是纯单元测试（共 302 项），不联网、不花钱、不用起服务。

> `fake_sub2api.py` 不是测试，是上面三组「自动开通」共用的**假网关**（一台按真实
> 接口契约模拟的本地 HTTP 服务，含 409/429/两步验证/未绑分组等失败形态）。
> 它的文件名故意不叫 `test_` 开头，所以不会被当成测试跑，也不计进项数。
> 三组共用同一份，**不要各拷一份**——分叉之后，改了假网关只有一组会变红，
> 另外两组会一直用着旧行为，那比没有测试更糟。

> ⚠️ `test_migrations.py` 是唯一一组**必须在两种数据库上各跑一遍**的测试。
> 直接跑只会跑 SQLite 那一半，另外 10 项会显示 `skipped`——
> **skipped 不等于通过**。推送前请按下面「启动迁移单测」一节把 PostgreSQL 那一半也跑掉。

## 十一组单元测试：一条命令全跑完

```bash
rm -rf /tmp/dk-unittest && DESIGNKIT_DATA_DIR=/tmp/dk-unittest DESIGNKIT_PROVIDER=mock \
  .venv/bin/python -m unittest discover -s tests -p 'test_*.py'
```

末尾出现 `Ran 302 tests ... OK (skipped=10)` 就是全过了
（那 10 项 skipped 是 PostgreSQL 迁移，见上面的 ⚠️）。

> **跑完大约要两三分钟，中间会安静好一阵子，不是卡住了。**（实测 302 项用了
> 154 秒。）慢的是三组「自动开通」，它们占了其中大半：每一项都真的起一台假网关、
> 真的走一遍 HTTP，还要等几次退避和限流。屏幕上会滚过很多行红字，例如
> 「用户 12 的自动开通失败（E_LOGIN_2FA）：该用户在 Sub2API 开启了两步验证……」
> ——**那是测试故意造出来的失败，不是出错**。只看最后一行 `OK` 就行。

> **`DESIGNKIT_DATA_DIR=/tmp/dk-unittest` 是干什么的**：把测试用的数据目录
> 挪到 `/tmp` 里，免得碰到项目里的 `data/`——那里面放着你的网关 Key、
> 数据库和历史生成记录。
>
> 忘了写也不会出事：不设这个变量时，整次运行会自己开一个临时目录（十一个文件共用
> 这一个），跑完自动删掉。**单元测试在任何跑法下都不会动你的 `data/`**（已实测）。
> 显式写上它的好处是看得见测试写到哪儿去了。
>
> **`rm -rf` 可以省**：同一个数据目录反复跑不会撞车（已实测连跑三次、
> 以及「整套 → 单组 → 整套」都正常）。加上它只是让每次都从干净状态开始，
> 排查问题时更好定位。
>
> 早期不是这样：有测试会建固定用户名的账号、还会把 `designkit.db` 和 `.secret_key`
> 覆写成假文件来验证「这两个文件绝对下载不到」——那等于**把数据库和登录密钥写坏**，
> 第二遍跑直接 `database disk image is malformed`，显示成
> `Ran 0 tests ... FAILED (errors=1)`，看起来像代码坏了。
> 现在改成用户名带随机后缀、且那两个文件**只在不存在时才造假的**
> （已存在就原样不动，那反而是更强的用例）。
> 提这一段是因为：**将来再写测试时，绝对不要往数据目录里写这两个文件名**——
> 万一有人把 `DESIGNKIT_DATA_DIR` 指向真实数据目录跑一次，数据库会当场报废。

想单独跑某一组，把 `-p` 后面换成那一组的文件名即可：

```bash
rm -rf /tmp/dk-unittest && DESIGNKIT_DATA_DIR=/tmp/dk-unittest DESIGNKIT_PROVIDER=mock \
  .venv/bin/python -m unittest discover -s tests -p 'test_file_access.py' -v
```

## 三组端到端测试（每一组都要用**自己的一份全新数据**）

三个脚本都会改动账号数据，而且要求各不相同，**不能接在一起跑同一份数据**：

| 脚本 | 它对数据做了什么 | 它要求数据是什么样 |
|---|---|---|
| `e2e_core.py` | 把 admin 密码改成 `corepass8888` | admin 还是初始口令 |
| `e2e_security.py` | 把 admin 密码改成 `newpass8888` | admin 还是初始口令 |
| `e2e_multiuser.py` | 改 admin 密码、建两个成员账号 | admin 还是初始口令 |

以前的写法是「重置一次数据，两个脚本按顺序跑」。现在不行了：服务端加了
「首次登录必须改掉初始密码，不改就用不了任何功能」这道闸门，所以每个脚本
一上来都得先把初始密码改掉；而后面的脚本第一件事又是用初始密码登录。
两件事凑在一起，只能是**一个脚本一份干净数据**。

### 跑法（三个脚本各来一遍，把下面的 `core` 换成 `security` / `multiuser`）

第一个终端窗口——起一个只给测试用的服务，数据放在 `/tmp` 里，
**碰不到你自己的 `data/` 目录**：

```bash
rm -rf /tmp/dk-e2e-core
DESIGNKIT_DATA_DIR=/tmp/dk-e2e-core DESIGNKIT_PROVIDER=mock \
  .venv/bin/uvicorn backend.app.main:app --host 127.0.0.1 --port 8788
```

第二个终端窗口——把脚本指到刚起的那个服务上：

```bash
DESIGNKIT_E2E_BASE=http://127.0.0.1:8788 .venv/bin/python tests/e2e_core.py
```

跑完回第一个窗口按 `Ctrl+C` 停掉服务，然后换下一个脚本重来一遍
（`rm -rf` 那一行的目录名和脚本名一起换，别只换一个）。

- `e2e_core.py` — 核心流程 27 项：首登强制改密、上传、模板变量渲染、mock 生成、
  对外 API 提交/查询、webhook 回调签名、越权拦截等
- `e2e_security.py` — 安全与边界 30 项：默认口令与强制改密、令牌撤销、
  图片必须凭登录才能打开、SSRF 拦截、base64 边界、尺寸校验、配额原子扣减等
- `e2e_multiuser.py` — 多用户隔离 52 项：管理员建成员账号、成员首登改密、
  成员在模拟生图模式下不配 Key 也能出图、另一个成员看不到/用不了他的图和任务、
  管理员可读不可写（不能替别人点重试花别人的钱）、退出登录只踢当前设备、
  停用账号后登录与图片链接一起失效

全部输出 PASS 即为通过。

> 服务地址默认是 `http://127.0.0.1:8787`。上面用环境变量 `DESIGNKIT_E2E_BASE`
> 指到了 8788，是为了和你平时 `./start.sh` 起的那个服务岔开端口，
> 免得测试脚本跑到你正在用的那套数据上去。

## 网关自动开通单测（三组）

```bash
rm -rf /tmp/dk-unittest && DESIGNKIT_DATA_DIR=/tmp/dk-unittest DESIGNKIT_PROVIDER=mock \
  .venv/bin/python -m unittest discover -s tests -p 'test_provision*.py' -v
```

守的是「成员一注册，系统自动去自建网关（Sub2API）上给他开一个账号、
拿一把他自己的 Key」这条链路。三组各盯一段，共 117 项：

- `test_provisioning.py` — 54 项，**状态机内部**：七个状态之间每一条转移、
  每一种失败的分类与退避排期、409 之后怎么反查认领、撞上抢注怎么换算。
- `test_provision_regression.py` — 36 项，**从接口进、从接口出的最终表现**：
  管理员点建号会不会失败、成员登不登得进来、界面上显示的是不是人话、
  接口回出去的 JSON 里有没有夹带凭据。
- `test_provision_schedule.py` — 27 项，**触发时机**：建号接口一步外部请求都不发、
  后台每分钟那一轮、余额同步、多进程锁、管理员点「重新开通」。

**全程不碰任何真实网关**（包括你 NAS 上那台）。三组共用同目录的
`fake_sub2api.py`——一台跑在 127.0.0.1 上的假 Sub2API，按已核实的接口契约模拟，
包括 409 邮箱已存在、429 建 Key 限流、两步验证、403 未绑分组、423 合规未确认
这些失败形态，还刻意保留了三个真实存在的坑（模糊搜索会把「像但不等」的排在前面、
`/api/v1/api-keys` 这条路径永远 404、合规 423 的 code 是字符串不是数字）。

### 这三组里最要紧的六条线，为什么必须长期跑

这六种事故的共同点是**不会报错**，一次性手测全都通过：

1. **可重入**——worker 随时可能被杀（NAS 断电、容器重启、部署滚动），
   任何一步都可能执行到一半就没了。重跑必须收敛到同一个结果。
   坏掉的样子：Sub2API 上多出一个没人认领的账号；或者更贵的——
   本地把新 Key 覆盖上去，旧 Key 还挂在网关上继续计费，而账单上那一笔
   再也对不上是谁花的。
2. **降级**（`GatewayDownTests` 那一组，**不许删**）——网关整个连不上时，
   建成员照常成功、成员照常登录进工作台，只是显示「需要手工处理」。
   坏掉的样子：网关一停机，新人一个都进不来，而他们本来只是暂时不能生图。
3. **不可重试的绝不重试**——两步验证、密码对不上、建 Key 撞上 429。
   最贵的是最后一个：换个后缀再撞一次，就把这个成员在网关那边**锁死一小时**。
4. **密码清空**——默认配置下开通成功后立刻清掉成员在网关的密码。
   坏掉了没有任何症状，只是库里长期躺着一堆可登录凭据。
5. **不泄露**——有一项专门装了个假日志处理器，把整条链路的 DEBUG 全量日志
   抓下来全文搜，断言密码、完整 Key、管理员 Key、登录令牌一个都不出现；
   另外几项把成员列表、重试按钮、系统设置、自检这四个接口的响应也搜了一遍。
   日志文件是会被打包发给别人排错的，所以「打进日志」等于「发出去了」。
6. **冒烟不过就不算开通**——Key 发出来了不等于能用（没绑分组的 Key 一张图也
   发不出去）。只有拿这把 Key 真去网关问一次才算数，问不通就绝不能标成「已开通」。

> **这三组绝对不会连你那台真实的 Sub2API。** 测试里用到的地址全是
> `127.0.0.1` 上临时起的假服务，管理员 Key 是一个写死的假字符串
> （`fake-admin-key-for-tests-only`）。**不要**把真实的网关地址或管理员 Key
> 填进测试代码——真实密钥只在网页界面上填，这是本项目的一条硬规矩。

> 需要注意的是，这三组能证明的是「designkit 这边的逻辑对不对」，
> 证明不了「你那台 Sub2API 上的开关是什么状态」。上线前还得在设置页点一次
> 「测试能不能自动开通」（那是真的去读你那台实例），
> 并用一个测试账号**真的生成一张图**——冒烟只能证明网关愿意受理这把 Key，
> 证明不了能出图。

## 图片访问鉴权单测

```bash
rm -rf /tmp/dk-unittest && DESIGNKIT_DATA_DIR=/tmp/dk-unittest DESIGNKIT_PROVIDER=mock \
  .venv/bin/python -m unittest discover -s tests -p 'test_file_access.py' -v
```

- `test_file_access.py` — 50 项，守 `/files/<路径>` 这条取图路由。
  在这次改动之前，图片目录是**一点鉴权都没有**的：知道地址就能下载任何人的
  商品图和出图结果，而地址除了任务号之外全是可枚举的。这组测试盯着四件事：
  ①路径白名单（`..`、编码过的 `%2f`、大写后缀、非十六进制任务号一律拒）；
  ②网页端 Cookie（本人可取、别人 404、管理员可排障、改密码/停用后旧凭证立刻失效）；
  ③对外 API 的签名链接（签名/到期时间/Key 编号任改一位都失效，停用 Key 立刻全断）；
  ④没有凭证时按 `files_signed_only` 开关行事，并把次数记进计数器。

> 里面有一组叫 `RawAsgiTraversalTests` 的测试，写法看着很奇怪——它不用普通的
> HTTP 客户端，而是手工拼一个请求塞给应用。**这是故意的，别去「简化」它**：
> 普通客户端（和浏览器）都会在发出去之前先把地址里的 `../` 抹平，请求根本
> 到不了服务端，测出来是一片假绿。真正要防的是有人用工具把没抹平的 `../`
> 原样打进来，只有这种写法碰得到那条路径。

## 多用户隔离与按人计费单测

```bash
rm -rf /tmp/dk-unittest && DESIGNKIT_DATA_DIR=/tmp/dk-unittest DESIGNKIT_PROVIDER=mock \
  .venv/bin/python -m unittest discover -s tests -p 'test_multiuser_isolation.py' -v
```

- `test_multiuser_isolation.py` — 29 项，守「谁的图，花谁的钱，别人看不到」。
  ①`resolve_for_user` 的五条规则（模拟生图模式豁免、共用一把 Key 时照旧、
  成员用自己的 Key、无主任务与管理员回落全局 Key）；
  ②**五种失败场景一律报错、绝不去用管理员那把全局 Key**；
  ③开了「每人一把 Key」而成员没配 Key 时，任务必须**永久失败、不进重试队列、
  一张图都不生**；④跨用户越权（拿别人的上传 id 建任务、查别人的任务、
  替别人点重试）一律 404。

> 为什么这一组必须长期跑，而不是上线前手工验一次：这里的每一种事故都**不会报错**。
> 「找不到成员的 Key 就用全局那把顶上」看起来还很贴心——生图照常成功、
> 界面一切正常，只有账单在悄悄地全记到管理员头上，而且日志里没有任何痕迹
> 能看出这一笔是谁花的。没有测试盯着，下一个人「顺手优化」一下就能把它打开。

## Provider 兼容回归

这组测试使用本地假上游，不会调用真实生图服务或产生费用：

```bash
rm -rf /tmp/dk-unittest && DESIGNKIT_DATA_DIR=/tmp/dk-unittest DESIGNKIT_PROVIDER=mock \
  .venv/bin/python -m unittest discover -s tests -p 'test_provider_compat.py' -v
```

- `test_provider_compat.py` — 覆盖三类问题：①网关拒绝多图 `n` 时的单图拆分回退
  （并确保无关 400 不会误触发）；②「HTTP 200 但业务失败」的 `{code,msg}` 信封、
  `images`/`results` 等非标准字段名、HTML 错误页的识别；③API 地址结尾拼接
  （自带 `/api/v3` 的网关不能被拼成 `/api/v3/v1/...`）。

## 启动迁移单测（这组要跑两遍）

`test_migrations.py` 守的是「升级时给老库补新列、补索引」这条路。它失败的样子最难查：
本机一路绿灯，推到群晖上**容器无限重启**，界面上什么都看不到。

### 第一遍：SQLite（本机默认，什么都不用装）

```bash
.venv/bin/python -m unittest discover -s tests -p 'test_migrations.py' -v
```

会用临时文件建库，跑完自动删掉，不碰 `data/` 目录，也不会动你已经填好的网关 Key。
输出末尾会是 `OK (skipped=10)`——那 10 项是 PostgreSQL 的，得按下面第二遍来跑。

### 第二遍：PostgreSQL（推送前必须跑，不能只信第一遍）

**为什么不能只跑 SQLite**：SQLite 对建表语句里的类型名几乎来者不拒，写了
`DATETIME`、`BOOLEAN DEFAULT 0` 这种 PostgreSQL 根本不认的写法，在 Mac 上照样绿灯，
推到群晖（用的是 PostgreSQL）上启动就崩。只跑 SQLite 比不跑更糟——
它会让人以为这条路已经有测试守着了。

需要一个 PostgreSQL。**本机装了 Docker 的话**，起一个用完就扔的：

```bash
docker run --rm -d --name designkit-pgtest \
  -e POSTGRES_PASSWORD=testonly -e POSTGRES_DB=designkit_test \
  -p 55432:5432 postgres:16-alpine
```

等几秒钟让它起来，然后：

```bash
DESIGNKIT_TEST_DATABASE_URL='postgresql+psycopg://postgres:testonly@127.0.0.1:55432/designkit_test' \
  .venv/bin/python -m unittest discover -s tests -p 'test_migrations.py' -v
```

跑完把容器删掉（数据一起没，本来就是临时的）：

```bash
docker rm -f designkit-pgtest
```

这一遍的输出末尾应该是 `OK`，**没有 skipped**。看到 `skipped=10` 就说明变量没生效，
等于第二遍没跑。

**本机没装 Docker**（monica 的 Mac 就没有）时，可以装一个免安装的临时 PostgreSQL，
装到临时目录里、不污染项目的 `.venv`：

```bash
.venv/bin/pip install --target /tmp/pgtest-lib pgserver
```

然后开一个终端窗口把它跑起来（这个窗口会一直占着，别关）：

```bash
PYTHONPATH=/tmp/pgtest-lib .venv/bin/python -c "
import pgserver, time
s = pgserver.get_server('/tmp/pgtest-data', cleanup_mode=None)
s.psql('CREATE DATABASE designkit_test')
print('把下面这行整个复制走（只有地址，不含变量名）：', flush=True)
print(s.get_uri(database='designkit_test'), flush=True)
time.sleep(3600)
"
```

> `/tmp/pgtest-data` 这个路径**别改深**。这种跑法是通过一个「socket 文件」连数据库的，
> 而这个文件的完整路径超过 103 个字符系统就不认了，报出来的是
> `Unix-domain socket path ... is too long` 这种完全看不出根因的错。照抄上面的路径最省事。

把它打印出来的那一行贴到另一个终端窗口，接上测试命令即可：

```bash
DESIGNKIT_TEST_DATABASE_URL=<粘贴上面打印的地址> \
  .venv/bin/python -m unittest discover -s tests -p 'test_migrations.py' -v
```

跑完要**手工收尾**，两条命令（按 `Ctrl+C` 只会停掉那个 python 脚本，
数据库进程是它在后台另起的，**不会跟着退出**，会一直留在你的 Mac 上跑）：

```bash
PYTHONPATH=/tmp/pgtest-lib .venv/bin/python -c "
import pgserver
pgserver.get_server('/tmp/pgtest-data', cleanup_mode='delete').cleanup()"
```

```bash
rm -rf /tmp/pgtest-data /tmp/pgtest-lib
```

不收拾也不影响用电脑，只是白占着约 45 MB 和一个后台进程；重启 Mac 后进程会没，
但 `/tmp/pgtest-data` 可能还在。想确认停干净了，执行 `pgrep -fl postgres`，
没有输出就是干净的。

> **三道保险，别绕过去**。这组测试会建表、灌数据、删 schema，指错库就是一场事故：
> 1. 变量名是 `DESIGNKIT_TEST_DATABASE_URL`，**不是** `DESIGNKIT_DATABASE_URL`。
>    后者指的是真实数据库，而且 `.env` 里可能已经填了它。
> 2. 库名里必须含 `test`（例如 `designkit_test`），否则测试直接报错，一行 SQL 都不执行。
> 3. 所有表都建在一个专用 schema 里，跑完整个删掉，碰不到同一个库里别的表。
>
> 所以**绝对不要**把群晖上那个 `designkit` 生产库填进来。真想用 NAS 上现成的数据库，
> 也请先 `CREATE DATABASE designkit_test` 单独建一个空库。

`test_migrations.py` 覆盖的内容：老库（只有旧列的历史快照）跑完升级后，
`models.py` 里声明的每一列每一个索引都要在（**加了模型字段却忘了登记迁移会当场变红**）；
重复启动幂等；补列失败必须挡死启动而不是偷偷放过；唯一索引撞上存量重复数据时
不许把整批升级带崩、且日志得是运营看得懂的人话、清掉重复后重启能自动补上。
PostgreSQL 侧另有三项：别的 schema 里有同名表时不许把补列静默跳过、
两个进程同时启动时后来的要排队并报中文人话、升级完要把锁还回去。

## 灵感库与预处理单测

```bash
rm -rf /tmp/dk-unittest && DESIGNKIT_DATA_DIR=/tmp/dk-unittest DESIGNKIT_PROVIDER=mock \
  .venv/bin/python -m unittest discover -s tests -p 'test_inspiration_convert.py' -v
```

- `test_inspiration_convert.py` — YouMind `{argument}` 变量语法转换（同名复用/撞名加序号/中文变量）
  与输入图预处理（比例补边、透明合白底、坏图回退），不联网、不花钱。

## 跨库兼容与定时同步单测

```bash
rm -rf /tmp/dk-unittest && DESIGNKIT_DATA_DIR=/tmp/dk-unittest DESIGNKIT_PROVIDER=mock \
  .venv/bin/python -m unittest discover -s tests -p 'test_db_and_scheduler.py' -v
```

- `test_db_and_scheduler.py` — 用 PostgreSQL 方言静态编译全部建表与运行期 SQL
  （本机无需装 PG），并验证搜索大小写不敏感、调度器到期判断与失败退避，
  以及灵感库同步的互斥锁（手动同步和自动同步必须抢同一把锁，
  否则会把上万条灵感库写重复）。

> 这一组里有四项要真的连一下数据库。以前在一个全新的数据目录上跑，
> 这四项会一起报 `no such table: sync_state`——而本文档推荐的恰恰就是
> 「用临时数据目录跑」，等于谁照着做谁踩。现在它会自己先把表建出来，
> 空目录、老目录都能直接跑。

## 出图比例护栏与画幅措辞单测

```bash
rm -rf /tmp/dk-unittest && DESIGNKIT_DATA_DIR=/tmp/dk-unittest DESIGNKIT_PROVIDER=mock \
  .venv/bin/python -m unittest discover -s tests -p 'test_sizing.py' -v
```

- `test_sizing.py` — 校验尺寸护栏（16 的倍数、边长与总像素上下限、长短比 ≤3）、
  比例反算、以及提示词里的画幅措辞是否与实际尺寸一致。
  还验证 `enforce_aspect` / `enforce_transparent_background` 是**幂等**的——
  补图任务会把已加工过的提示词再跑一遍，不幂等就会越叠越长。

## 按分类生成单测

```bash
rm -rf /tmp/dk-unittest && DESIGNKIT_DATA_DIR=/tmp/dk-unittest DESIGNKIT_PROVIDER=mock \
  .venv/bin/python -m unittest discover -s tests -p 'test_category_mode.py' -v
```

- `test_category_mode.py` — 校验多分类归属（一条提示词同属多个分类时，
  按任一分类都要能筛到）、通览清单只含标题、按模型给出的编号取回全文并保序、
  超大分类的预筛与随机抽样（**新同步进来的条目必须够得着**——按 id 取前 N 条时
  它们永远排在后面，同步了也用不上）、以及同步代理只作用于灵感库下载。
  这条链路是生成页的主路径，出问题的表现是
  「选了分类却出通用图」或「某个分类看起来几乎是空的」，都不容易被直接看出来。
