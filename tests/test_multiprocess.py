"""多进程回归：把「开多个 uvicorn worker」这件事真的做一遍再来断言。

════════════════════════════════════════════════════════════════════════
 这一组为什么必须存在
════════════════════════════════════════════════════════════════════════
仓库里另外那十几组测试全都跑在**一个进程**里。它们全绿的时候，下面这几种
事故照样成立——因为它们只在「同一份代码同时被好几个进程各跑一遍」时才出现：

1. **同时生成几张图被悄悄乘上了进程数。**
   worker.start() 在每个进程里各建一个 ThreadPoolExecutor(并发数)。
   管理员在设置页填「同时生成 2 张」，开 4 个 worker 实际是 8 张同时在跑，
   而界面上显示的还是 2。这不是性能问题，是**费用**问题：每一张图都是钱，
   网关那边还可能有并发上限，一超就整批失败，运营看到的只是「今天老是失败」。
   现在的修法是「只有拿到数据库租约的那个进程派发任务」，这一组就是来盯着
   那把租约的——**单进程测试测不出它，因为单进程本来就只有一个派发者**。

2. **领导者进程被杀之后没人接手，从此不出图。**
   租约有 30 秒 TTL、待命进程 5 秒轮询一次，所以最坏 35 秒有人顶上。
   这个数字写在 docs/deploy-server.md 里，是运营「要不要再重启一次」的依据。
   没有测试盯着，改一行常量就能让它变成「十分钟不出图」而没人发现。

3. **全新安装、多进程同时启动时把库建坏。**
   建表、补列、写种子数据这三件事，四个进程会同时抢着做。

4. **限速被进程数除了。**
   限速如果记在进程内存里，设置里写「10 次」实际是「10 × 进程数」，
   而且界面上完全看不出来。

5. **真实客户端 IP 取错。**
   套上反向代理之后，uvicorn 自己会改写 request.client，deps.client_ip 又会
   再算一遍 X-Forwarded-For。两者叠加的结果**只有真的起 uvicorn 才看得到**，
   用 TestClient 测永远碰不到 uvicorn 那一层。

════════════════════════════════════════════════════════════════════════
 这一组慢在哪、为什么值得等（**请不要因为它慢就把它删掉**）
════════════════════════════════════════════════════════════════════════
慢是**结构性的**，不是写得差，优化不掉：

- 它要真的把 uvicorn 起起来（每回 4 个工作进程），一共起 5 回，每回 1~2 秒；
- 生成任务用的是模拟生图，每张**故意 sleep 1.2 秒**（services/provider.py，
  为了让前端进度条像真的）。要量「同时在跑几张」，就必须真的让它跑一会儿；
- 「领导者被 kill -9 之后谁来接手」这一项，**必须真的等**租约过期。
  租约 30 秒是产品决定（见 worker.py 文件头），测试无权把它调小——
  调小就等于没测那个会上线的数字。这一项一个人就要等约 40 秒。

本机实测整组 74 秒。作为对照：这一组能挡住的是「每张图多花一倍的钱」和
「服务器重启后再也不出图」，两者都不会报错、手工点一遍全都正常。
（改造之前的代码在「并发上限」那几项里量到的是 6 张，设置里只准 2 张。）

**这一组不用你手工起服务**：uvicorn 由测试自己拉起、自己收拾干净。

════════════════════════════════════════════════════════════════════════
 一条已知问题（本组**故意**没有断言它，免得交付一份红的测试）
════════════════════════════════════════════════════════════════════════
**全新安装的第一次启动**（数据库文件还不存在）开多 worker 时，会有进程崩掉：

    sqlite3.OperationalError: table users already exists
    sqlite3.IntegrityError: UNIQUE constraint failed: users.username

前者是 SQLite 上 create_all 没有锁保护（migrations.py 的进程锁只在
PostgreSQL 上有），后者是 seed()「先查有没有、再插入」在两种数据库上都没有锁。
uvicorn 会把崩掉的子进程重新拉起来，第二次启动时表和管理员都已存在，于是
自愈——所以线上的表现是「第一次启动日志里有几段红字，服务照常能用」。

本组的做法是：**断言最终状态是对的**（管理员只有一个、种子只写了一份、
四个进程最终都在、healthz 通）+ **断言第二次启动零报错**。
等哪天有人把这两处补上锁了，请把 ColdStartRaceTests 里的说明删掉，
并加一条「冷启动日志里不许出现 Traceback」。

════════════════════════════════════════════════════════════════════════
 安全：这一组碰不到你的 data/
════════════════════════════════════════════════════════════════════════
每一台测试服务器都用**自己的临时目录**，而且启动子进程时**显式写死**
DESIGNKIT_DATABASE_URL 指向那个临时目录里的 sqlite 文件。
显式写死是必须的：config.py 会读仓库根目录的 .env，万一那里填着
DESIGNKIT_DATABASE_URL=群晖上的生产库，只设 DESIGNKIT_DATA_DIR 是拦不住的——
测试会连上生产库建表、写种子、跑迁移。这一行就是那道闸门，别删。
"""
import atexit
import json
import logging
import os
import re
import shutil
import signal
import socket
import subprocess
import sys
import tempfile
import threading
import time
import unittest
import urllib.error
import urllib.request
from datetime import datetime

_HERE = os.path.dirname(os.path.abspath(__file__))
_ROOT = os.path.dirname(_HERE)
# 不写死绝对路径：仓库被挪到别的目录之后，写死的路径会让整组 import 失败，
# 而报错完全看不出根因（同 test_invite_register.py 里的那一处）。
if _ROOT not in sys.path:
    sys.path.insert(0, _ROOT)

# 导入 backend.app.config 会在 import 期间就建数据目录。绝不能碰用户放着网关 Key
# 和生产数据的那个 data/，所以在导入之前先把它指到临时位置。
if not os.environ.get("DESIGNKIT_DATA_DIR"):
    _TMP_DATA_DIR = tempfile.mkdtemp(prefix="dk-mp-data-")
    os.environ["DESIGNKIT_DATA_DIR"] = _TMP_DATA_DIR
    atexit.register(shutil.rmtree, _TMP_DATA_DIR, ignore_errors=True)
os.environ.setdefault("DESIGNKIT_PROVIDER", "mock")

from sqlalchemy import create_engine, event, func, inspect as sa_inspect, select  # noqa: E402
from sqlalchemy.orm import sessionmaker  # noqa: E402

from backend.app.models import (  # noqa: E402
    Base, GeneratedImage, GenerationJob, PromptCategory,
    PromptTemplate, RateLimitState, SyncState, User,
)
from backend.app.seed import DEFAULT_ADMIN_USERNAME, SEED_CATEGORIES, SEED_TEMPLATES  # noqa: E402
from backend.app.services import scheduler as scheduler_locks  # noqa: E402
from backend.app.services import worker as worker_module  # noqa: E402

# uvicorn 工作进程数。4 是「开多 worker 会不会把并发乘 4」这个 bug 的原始复现条件，
# 也是 docs/deploy-server.md 里给的推荐值，所以照着 4 来测。
WORKERS = 4
# 测试用的「同时生成几张」。取 2 是因为：坏掉的写法在 4 个进程下会跑到 8 张，
# 和 2 差得足够远，采样绝不会看走眼。
CONCURRENCY = 2

_WORKER_START_RE = re.compile(r"生成 worker 已启动（进程 (\d+)，令牌 (\S+?)）")
_LEADER_RE = re.compile(r"本进程（(\d+)，令牌 (\S+?)）取得生成派发权")
_STANDBY_RE = re.compile(r"本进程（(\d+)）转入待命")

# 起服务和跑任务的各种等待上限。写成常量是为了让「机器慢」只改一处；
# 每一个都远大于实测值（实测见 tests/README.md）。
BOOT_TIMEOUT = 90
JOB_TIMEOUT = 240
# 领导者被 kill -9 之后允许的最长接管时间。
# 30 秒租约 + 5 秒待命轮询 = 设计上的最坏 35 秒，这里再留 15 秒给机器抖动。
# **不要为了让测试跑快就调小它**：它测的就是那个会上线的数字。
TAKEOVER_TIMEOUT = int(
    worker_module.LEASE_TTL.total_seconds() + worker_module.STANDBY_RETRY_SECONDS + 15
)


# ══════════════════════════════════════════════════════════════════════
#  测试服务器：真的起一台 uvicorn（多工作进程），跑完自己收拾干净
# ══════════════════════════════════════════════════════════════════════

_LIVE_SERVERS = []


def _stop_all_servers():
    """兜底：无论测试怎么崩，都别在使用者的电脑上留下一堆 uvicorn 进程。"""
    for server in list(_LIVE_SERVERS):
        try:
            server.stop()
        except Exception:
            pass


atexit.register(_stop_all_servers)


def tearDownModule():
    _stop_all_servers()


def _free_port():
    sock = socket.socket()
    sock.bind(("127.0.0.1", 0))
    port = sock.getsockname()[1]
    sock.close()
    return port


class MultiWorkerServer(object):
    """一台跑在 127.0.0.1 上的真实 uvicorn，带 N 个工作进程。

    为什么是 `python -m uvicorn` 而不是 `.venv/bin/uvicorn`：用当前解释器启动，
    换个虚拟环境、换台机器都不会突然找不到命令，报错也不会变成
    「No such file or directory」这种看不出根因的样子。
    """

    def __init__(self, tag, workers=WORKERS, extra_env=None, data_dir=None):
        self.tag = tag
        self.workers = workers
        self.extra_env = dict(extra_env or {})
        self.owns_data_dir = data_dir is None
        self.data_dir = data_dir or tempfile.mkdtemp(prefix="dk-mp-%s-" % tag)
        self.db_path = os.path.join(self.data_dir, "designkit.db")
        self.log_path = os.path.join(self.data_dir, "uvicorn-%s.log" % tag)
        self.port = None
        self.proc = None
        self._log_handle = None
        self._engine = None
        self._Session = None

    # ---------------------------------------------------------- 生命周期

    def _env(self):
        env = dict(os.environ)
        env.update({
            "DESIGNKIT_DATA_DIR": self.data_dir,
            # ⚠ 这一行是安全闸门，别删（理由见本文件开头「安全」一节）：
            # config.py 会读仓库根目录的 .env，那里可能填着生产数据库地址。
            "DESIGNKIT_DATABASE_URL": "sqlite:///" + self.db_path,
            "DESIGNKIT_PROVIDER": "mock",
            # 灵感库自动同步会去 raw.githubusercontent.com 拉一万多条数据。
            # 它在启动 30 秒后开跑，而本组最长的一项要跑 40 多秒，正好会撞上。
            # 测试不许联网，所以这里必须关掉。
            "DESIGNKIT_INSPIRATION_AUTO_SYNC": "false",
            "DESIGNKIT_WORKER_CONCURRENCY": str(CONCURRENCY),
        })
        env.update(self.extra_env)
        # 分开写的 PG 连接参数会绕过上面那一行（config.py 里它排在第二优先级），
        # 所以一并清掉。
        for key in ("DESIGNKIT_DB_HOST", "DESIGNKIT_DB_PORT", "DESIGNKIT_DB_NAME",
                    "DESIGNKIT_DB_USER", "DESIGNKIT_DB_PASSWORD"):
            env.pop(key, None)
        return env

    def start(self, wait_workers=True):
        os.makedirs(self.data_dir, exist_ok=True)
        self.port = _free_port()
        self._log_handle = open(self.log_path, "w")
        self.proc = subprocess.Popen(
            [sys.executable, "-m", "uvicorn", "backend.app.main:app",
             "--host", "127.0.0.1", "--port", str(self.port),
             "--workers", str(self.workers)],
            cwd=_ROOT, stdout=self._log_handle, stderr=subprocess.STDOUT,
            env=self._env(),
        )
        _LIVE_SERVERS.append(self)
        self._wait_healthz()
        if wait_workers:
            self.wait_for_workers(self.workers)
        return self

    def stop(self):
        if self.proc is not None:
            self.proc.terminate()
            try:
                self.proc.wait(timeout=30)
            except subprocess.TimeoutExpired:
                self.proc.kill()
                self.proc.wait(timeout=10)
            self.proc = None
        if self._log_handle is not None:
            self._log_handle.close()
            self._log_handle = None
        if self in _LIVE_SERVERS:
            _LIVE_SERVERS.remove(self)

    def cleanup(self):
        self.stop()
        if self.owns_data_dir:
            shutil.rmtree(self.data_dir, ignore_errors=True)

    def _wait_healthz(self):
        deadline = time.time() + BOOT_TIMEOUT
        while time.time() < deadline:
            if self.proc.poll() is not None:
                raise AssertionError(
                    "uvicorn（%s）还没起来就退出了，返回码 %s\n%s"
                    % (self.tag, self.proc.returncode, self.log_tail()))
            try:
                with urllib.request.urlopen(
                        self.url("/healthz"), timeout=3) as response:
                    if response.status == 200:
                        return
            except Exception:
                time.sleep(0.1)
        raise AssertionError("uvicorn（%s）%d 秒内没能应答 /healthz\n%s"
                             % (self.tag, BOOT_TIMEOUT, self.log_tail()))

    def wait_for_workers(self, count):
        """等到 count 个工作进程都跑完了各自的启动流程（各打了一行「已启动」）。"""
        deadline = time.time() + BOOT_TIMEOUT
        while time.time() < deadline:
            if len(self.worker_pids()) >= count:
                return
            time.sleep(0.1)
        raise AssertionError(
            "%d 秒内只等到 %d 个工作进程就绪（要 %d 个）\n%s"
            % (BOOT_TIMEOUT, len(self.worker_pids()), count, self.log_tail()))

    # ---------------------------------------------------------- 日志

    def log_text(self):
        try:
            with open(self.log_path, "r", encoding="utf-8", errors="replace") as handle:
                return handle.read()
        except IOError:
            return ""

    def log_tail(self, chars=4000):
        return "———— %s 的日志末尾 ————\n%s" % (self.tag, self.log_text()[-chars:])

    def worker_pids(self):
        """打过「生成 worker 已启动」的进程号（去重，保序）。"""
        seen = []
        for pid, _token in _WORKER_START_RE.findall(self.log_text()):
            if pid not in seen:
                seen.append(pid)
        return seen

    def leaders(self):
        """取得过派发权的 (进程号, 令牌) 列表，按发生顺序。"""
        return _LEADER_RE.findall(self.log_text())

    def standby_pids(self):
        return _STANDBY_RE.findall(self.log_text())

    def wait_for_leader(self, at_least=1, timeout=BOOT_TIMEOUT):
        deadline = time.time() + timeout
        while time.time() < deadline:
            leaders = self.leaders()
            if len(leaders) >= at_least:
                return leaders
            time.sleep(0.05)
        raise AssertionError("%d 秒内没等到第 %d 个领导者\n%s"
                             % (timeout, at_least, self.log_tail()))

    # ---------------------------------------------------------- 数据库

    def session(self):
        """连到这台服务器自己的库。用文件库 + WAL，父进程读、服务端写，互不阻塞。"""
        if self._Session is None:
            self._engine = create_engine(
                "sqlite:///" + self.db_path,
                connect_args={"check_same_thread": False, "timeout": 30},
                future=True,
            )

            @event.listens_for(self._engine, "connect")
            def _pragma(dbapi_connection, _record):
                cursor = dbapi_connection.cursor()
                cursor.execute("PRAGMA journal_mode=WAL")
                cursor.execute("PRAGMA busy_timeout=30000")
                cursor.close()

            self._Session = sessionmaker(
                bind=self._engine, autoflush=False, expire_on_commit=False, future=True)
        return self._Session()

    def submit_jobs(self, count, prefix="并发验证"):
        """直接往队列表里塞任务（数据库就是任务队列）。

        为什么不走网页接口：走接口要先登录、先改初始密码、先传图，
        那些每一条都另有测试盯着，在这里只会让这一组更慢、更容易因为
        不相干的改动变红。这一组要测的是**派发**，所以从队列这一层进。
        """
        session = self.session()
        try:
            user_id = session.execute(
                select(User.id).order_by(User.id.asc())).scalars().first()
            assert user_id is not None, "库里没有管理员，seed 没跑？"
            ids = []
            for index in range(count):
                job = GenerationJob(
                    source="web", user_id=user_id, template_name=prefix,
                    prompt_final="%s 占位提示词 %d" % (prefix, index),
                    params={"n": 1, "size": "512x512"},
                    input_paths=[], status="pending",
                )
                session.add(job)
                ids.append(job.id)
            session.commit()
            return ids
        finally:
            session.close()

    # ---------------------------------------------------------- HTTP

    def url(self, path):
        return "http://127.0.0.1:%d%s" % (self.port, path)

    def post_json(self, path, payload, headers=None):
        """回 (状态码, 响应头, 解析好的 JSON)。4xx/5xx 不抛异常——它们本来就是被测行为。

        响应头的键统一转成小写：HTTP 头本来就不区分大小写，而 urllib 给的是
        服务端实际发出的那个写法（starlette 发的是全小写）。不统一的话，
        断言会因为「大小写不一样」而失败，报出来的是 KeyError，看不出根因。
        """
        body = json.dumps(payload).encode("utf-8")
        request = urllib.request.Request(
            self.url(path), data=body, method="POST",
            headers=dict({"Content-Type": "application/json"}, **(headers or {})),
        )
        try:
            with urllib.request.urlopen(request, timeout=30) as response:
                raw = response.read()
                return response.status, _lower_keys(response.headers), _loads(raw)
        except urllib.error.HTTPError as error:
            raw = error.read()
            return error.code, _lower_keys(error.headers), _loads(raw)


def _lower_keys(headers):
    return dict((str(key).lower(), value) for key, value in headers.items())


def _loads(raw):
    try:
        return json.loads(raw.decode("utf-8"))
    except Exception:
        return {}


# ══════════════════════════════════════════════════════════════════════
#  量「同时在跑几张」的两种办法（互相印证）
# ══════════════════════════════════════════════════════════════════════

def sample_running(server, total_jobs, timeout=JOB_TIMEOUT, ignore_ids=(),
                   stop_after_idle=None):
    """每 50 毫秒数一次 status='processing' 的行数 —— 那就是「此刻同时在跑几张」。

    返回 (采样序列, 完成数)。

    ⚠ 每次读之前都要 rollback()：SQLAlchemy 的 Session 第一次查询就开了事务，
    而 SQLite 在事务里看到的是**开始那一刻的快照**，不 rollback 的话后面几百次
    采样会拿到同一个数字，测试变成一条直线还照样绿。
    """
    ignore = set(ignore_ids)
    samples = []
    done = 0
    session = server.session()
    deadline = time.time() + timeout
    idle_since = None
    try:
        while time.time() < deadline:
            session.rollback()
            running_ids = set(session.execute(
                select(GenerationJob.id)
                .where(GenerationJob.status == "processing")).scalars().all())
            running = len(running_ids - ignore)
            done = session.execute(
                select(func.count()).select_from(GenerationJob)
                .where(GenerationJob.status.in_(("succeeded", "failed")))).scalar_one()
            samples.append(running)
            if done >= total_jobs:
                break
            if stop_after_idle is not None:
                # 给「接管之后再看一会儿」用：连着一段时间没有新任务开跑就收工
                if running == 0:
                    idle_since = idle_since or time.time()
                    if time.time() - idle_since >= stop_after_idle:
                        break
                else:
                    idle_since = None
            time.sleep(0.05)
    finally:
        session.close()
    return samples, done


def peak_by_intervals(server, ignore_ids=()):
    """事后按每条任务的 [started_at, finished_at] 区间算最大重叠数。

    这一种和采样法互相印证：采样有可能「正好没采到那一瞬间」，
    而区间重叠是精确的，它算的是数据库里落下来的事实。
    """
    ignore = set(ignore_ids)
    session = server.session()
    try:
        rows = session.execute(select(
            GenerationJob.id, GenerationJob.started_at, GenerationJob.finished_at)).all()
    finally:
        session.close()
    events = []
    for job_id, started, finished in rows:
        if job_id in ignore or started is None:
            continue
        events.append((started, 1))
        events.append((finished or datetime.utcnow(), -1))
    # 同一时刻先记结束再记开始：一张结束、另一张接上，不算重叠
    events.sort(key=lambda item: (item[0], item[1]))
    current = peak = 0
    for _moment, delta in events:
        current += delta
        peak = max(peak, current)
    return peak


# ══════════════════════════════════════════════════════════════════════
#  一、并发上限：多进程下「同时生成几张」必须还是设置里那个数
# ══════════════════════════════════════════════════════════════════════

class GenerationConcurrencyCapTests(unittest.TestCase):
    """4 个 uvicorn 工作进程 + 设置「同时生成 2 张」→ 全局就得是 2 张。

    坏掉的写法（每个进程各建一个线程池、各自派发）在这里会量到 8，
    而在任何单进程测试里都量不到——它连第二个进程都没有。
    """

    server = None
    samples = None
    job_ids = None

    @classmethod
    def setUpClass(cls):
        cls.server = MultiWorkerServer("cap").start()
        try:
            cls.server.wait_for_leader()
            cls.job_ids = cls.server.submit_jobs(12)
            cls.samples, cls.done = sample_running(cls.server, len(cls.job_ids))
        except Exception:
            cls.server.cleanup()
            raise

    @classmethod
    def tearDownClass(cls):
        if cls.server is not None:
            cls.server.cleanup()

    def test_four_separate_operating_system_processes_really_started(self):
        """先证明这一组测的确实是「多进程」，不是「多线程」。

        没有这一条，别的断言全是假绿：只起一个进程的话，并发上限当然是对的。
        以后谁想「简化」成线程池模拟多进程，这一条会当场变红。
        """
        pids = self.server.worker_pids()
        self.assertEqual(len(pids), WORKERS,
                         "只等到 %d 个工作进程\n%s" % (len(pids), self.server.log_tail()))
        self.assertNotIn(str(os.getpid()), pids, "工作进程不该是测试进程自己")
        self.assertNotIn(str(self.server.proc.pid), pids, "工作进程不该是 uvicorn 主进程")

    def test_only_one_process_holds_the_dispatch_lease(self):
        """4 个进程里只有 1 个在派发，另外 3 个待命——而且待命日志只打一次。

        「只打一次」是刻意的：待命进程 5 秒轮询一次，每次都打的话，
        4 个进程一天能刷出几十万行，把真正要看的那几行盖掉。
        """
        leaders = self.server.leaders()
        self.assertEqual(len(leaders), 1,
                         "取得派发权的进程不止一个：%r\n%s" % (leaders, self.server.log_tail()))
        standby = self.server.standby_pids()
        self.assertEqual(sorted(standby), sorted(set(standby)),
                         "待命日志重复打印了：%r" % (standby,))
        self.assertEqual(len(set(standby)), WORKERS - 1,
                         "待命进程应该正好是 %d 个，实际 %r" % (WORKERS - 1, standby))

    def test_the_lease_row_in_the_database_points_at_that_leader(self):
        """租约不是只写在日志里：sync_state 表里那一行要对得上。

        对得上才有意义——排查时运营看的是日志，而真正决定谁能派发的是这一行。
        任务名 `generation` 和灵感库、自动开通并列，共用同一张表、互不干扰。
        """
        leader_pid, leader_token = self.server.leaders()[0]
        session = self.server.session()
        try:
            row = session.get(SyncState, worker_module.LEASE_TASK)
            self.assertIsNotNone(row, "sync_state 里没有 generation 这一行")
            self.assertEqual(row.lock_owner, leader_token)
            self.assertTrue(row.lock_owner.startswith("pid%s-" % leader_pid),
                            "令牌里应该带着进程号，方便和日志对照：%r" % (row.lock_owner,))
            self.assertIsNotNone(row.lock_until)
            self.assertGreater(row.lock_until, datetime.utcnow(),
                               "租约已经过期了，说明续租没在跑")
        finally:
            session.close()

    def test_sampled_concurrency_never_exceeds_the_setting(self):
        """量法一：每 50 毫秒数一次「此刻有几张在跑」，峰值不许超过设置值。"""
        peak = max(self.samples) if self.samples else -1
        self.assertGreater(len(self.samples), 20, "采样次数太少，结论不可信")
        self.assertLessEqual(
            peak, CONCURRENCY,
            "同时在跑 %d 张，设置里只准 %d 张——多开的 worker 把并发乘上去了。"
            "\n采样序列：%r\n%s" % (peak, CONCURRENCY, self.samples[:200],
                                   self.server.log_tail()))
        # 反面：如果一张都没真的并行过，上面那条断言是白给的
        self.assertGreaterEqual(
            peak, CONCURRENCY,
            "从来没跑满过 %d 张，这次测量说明不了问题（是不是任务根本没跑起来？）"
            % CONCURRENCY)

    def test_interval_overlap_confirms_the_same_cap(self):
        """量法二：事后按 [开始时间, 结束时间] 算最大重叠，和采样法互相印证。

        采样可能「正好没采到那一瞬间」，区间重叠算的是数据库里落下来的事实。
        两种量法都得出同一个数，才排除得掉「测量方法本身有问题」。
        """
        peak = peak_by_intervals(self.server)
        self.assertLessEqual(peak, CONCURRENCY,
                             "按时间区间算，最多有 %d 张在同时跑\n%s"
                             % (peak, self.server.log_tail()))
        self.assertGreaterEqual(peak, CONCURRENCY)

    def test_every_job_ran_exactly_once(self):
        """每个任务恰好出一张图：多进程下不许重复领取，重复一次就是多花一次钱。

        任务领取靠的是「status 写在 WHERE 里 + 判 rowcount==1」，
        这一条就是它在真多进程下的验收。
        """
        session = self.server.session()
        try:
            rows = session.execute(select(
                GenerationJob.id, GenerationJob.status, GenerationJob.attempts)).all()
            self.assertEqual(len(rows), len(self.job_ids))
            for job_id, status, attempts in rows:
                self.assertEqual(status, "succeeded", "任务 %s 状态是 %s" % (job_id, status))
                self.assertLessEqual(attempts or 0, 0, "任务 %s 重试过，mock 模式不该失败" % job_id)
            image_counts = session.execute(
                select(GeneratedImage.job_id, func.count(GeneratedImage.id))
                .group_by(GeneratedImage.job_id)).all()
            self.assertEqual(len(image_counts), len(self.job_ids))
            for job_id, count in image_counts:
                self.assertEqual(count, 1, "任务 %s 出了 %d 张图（要 1 张）" % (job_id, count))
        finally:
            session.close()


# ══════════════════════════════════════════════════════════════════════
#  二、领导者租约：派发的那个进程被杀之后，有人接手，而且不会变成两倍
# ══════════════════════════════════════════════════════════════════════

class LeaderFailoverTests(unittest.TestCase):
    """把正在派发的那个进程 kill -9，看多久有人顶上、顶上之后上限还成不成立。

    kill -9 不是假设：容器被 OOM 杀、NAS 断电、部署时滚动重启，都是这个形态。
    「没人接手」的表现是**从此不出图，而日志里一个 ERROR 都没有**——
    运营只会看到任务一直排队，谁也说不出哪里坏了。
    """

    server = None

    @classmethod
    def setUpClass(cls):
        cls.server = MultiWorkerServer("failover").start()
        try:
            leaders = cls.server.wait_for_leader()
            cls.first_leader_pid = int(leaders[0][0])
            cls.job_ids = cls.server.submit_jobs(24, prefix="接管验证")

            # 等到确实有任务在跑，再动手杀——否则杀的是一个还没开始干活的进程，
            # 测出来的「接管」没有意义。
            cls.orphaned = cls._wait_until_busy()
            cls.succeeded_at_kill = cls._succeeded_count()
            cls._guard_pid(cls.first_leader_pid)
            os.kill(cls.first_leader_pid, signal.SIGKILL)
            cls.killed_at = time.time()
            # 再取一次快照并合并：万一在「看到有任务在跑」和「动手杀」之间领导者又
            # 领了一批，那一批同样会随进程一起死，统计并发时也必须排除。
            # 这一刻已经没有领导者了（下一任要等租约过期），所以这时还是 processing
            # 的，一定是死掉那个进程留下的，不会误伤别人正在跑的任务。
            cls.orphaned = set(cls.orphaned) | cls._processing_ids()

            leaders = cls.server.wait_for_leader(at_least=2, timeout=TAKEOVER_TIMEOUT)
            cls.takeover_seconds = time.time() - cls.killed_at
            cls.second_leader_pid = int(leaders[1][0])
            cls.second_leader_token = leaders[1][1]

            # 接管之后再量一段时间的并发。被 kill 掉的那个进程手上的任务会永远停在
            # processing（要等 STUCK_RESET_SECONDS 才回队列，那是既有设计），
            # 统计时必须把它们排除，否则会被一直算成「在跑」。
            cls.samples_after, _done = sample_running(
                cls.server, len(cls.job_ids), timeout=45,
                ignore_ids=cls.orphaned, stop_after_idle=3)
        except Exception:
            cls.server.cleanup()
            raise

    @classmethod
    def tearDownClass(cls):
        if cls.server is not None:
            cls.server.cleanup()

    @classmethod
    def _guard_pid(cls, pid):
        """只杀这台测试服务器自己的进程。

        进程号来自我们几秒钟前刚拿到的这台服务器的日志，被系统回收再分配的可能性
        可以忽略；但「别把测试进程或 uvicorn 主进程杀了」这两条还是要挡一下——
        真发生了会表现成整组测试莫名其妙地消失，而不是一条失败。
        """
        assert pid != os.getpid(), "差点杀掉测试进程自己"
        assert pid != cls.server.proc.pid, "差点杀掉 uvicorn 主进程"
        assert str(pid) in cls.server.worker_pids(), "这个进程号不是本测试起的工作进程"

    @classmethod
    def _processing_ids(cls):
        session = cls.server.session()
        try:
            return set(session.execute(
                select(GenerationJob.id)
                .where(GenerationJob.status == "processing")).scalars().all())
        finally:
            session.close()

    @classmethod
    def _succeeded_count(cls):
        session = cls.server.session()
        try:
            return session.execute(
                select(func.count()).select_from(GenerationJob)
                .where(GenerationJob.status == "succeeded")).scalar_one()
        finally:
            session.close()

    @classmethod
    def _wait_until_busy(cls):
        deadline = time.time() + 60
        while time.time() < deadline:
            running = cls._processing_ids()
            if running:
                return running
            time.sleep(0.05)
        raise AssertionError("等了 60 秒也没有任务开始跑\n%s" % cls.server.log_tail())

    def test_another_process_takes_over_within_one_lease_ttl(self):
        """最坏 35 秒必须有人接手：租约 30 秒 + 待命进程 5 秒轮询一次。

        这个数字是写进 docs/deploy-server.md 的承诺（「进程被强杀最多等 35 秒，
        这期间不出新图属于正常，不用重启第二次」）。改常量就得改文档，
        这一条是它们俩之间的连接线。
        """
        self.assertLessEqual(
            self.takeover_seconds, TAKEOVER_TIMEOUT,
            "过了 %.1f 秒才有人接手（上限 %d 秒）" % (self.takeover_seconds, TAKEOVER_TIMEOUT))
        # 反面：立刻接手说明租约根本没生效（比如谁把 TTL 改成了 0）
        self.assertGreater(
            self.takeover_seconds, 1.0,
            "领导者刚被杀就有人接手（%.1fs），租约是不是形同虚设？" % self.takeover_seconds)

    def test_the_new_leader_is_a_different_living_process(self):
        """接手的必须是**另一个**进程，而且全程只有这两任领导者。

        uvicorn 会把被杀的子进程重新拉起来，那个新进程也会来抢派发权——
        它抢到不算错，但「同时有两个在派发」就是错。
        """
        self.assertNotEqual(self.second_leader_pid, self.first_leader_pid)
        self.assertEqual(len(self.server.leaders()), 2,
                         "领导者出现了 %d 次，接管期间可能有两个进程在同时派发\n%s"
                         % (len(self.server.leaders()), self.server.log_tail()))

    def test_the_lease_row_now_belongs_to_the_new_leader(self):
        session = self.server.session()
        try:
            row = session.get(SyncState, worker_module.LEASE_TASK)
            self.assertEqual(row.lock_owner, self.second_leader_token)
            self.assertTrue(row.lock_owner.startswith("pid%d-" % self.second_leader_pid))
        finally:
            session.close()

    def test_the_cap_still_holds_after_the_takeover(self):
        """接手之后并发上限仍然是设置值，不会因为「换了个人派发」变成两倍。

        这是这一组最要紧的一条：租约做对了但交接没做对的样子，正是
        「旧的还在派发、新的也开始派发」——而两边都跑得好好的，没有任何报错。
        """
        peak = max(self.samples_after) if self.samples_after else -1
        self.assertGreater(len(self.samples_after), 20, "接管后采样太少，结论不可信")
        self.assertLessEqual(
            peak, CONCURRENCY,
            "接管之后同时在跑 %d 张，设置里只准 %d 张\n%s"
            % (peak, CONCURRENCY, self.server.log_tail()))
        self.assertGreaterEqual(
            peak, CONCURRENCY,
            "接管之后一直没跑满 %d 张，队列是不是空了？（这次测量说明不了问题）"
            % CONCURRENCY)

    def test_the_queue_keeps_draining_after_the_takeover(self):
        """接手之后任务真的继续出图了——不是「选出了新领导者但没人干活」。

        只看日志有「取得派发权」是不够的：租约拿到了、派发循环却卡住的话，
        日志上完全看不出区别。
        """
        succeeded_now = self._succeeded_count()
        self.assertGreater(
            succeeded_now, self.succeeded_at_kill,
            "领导者被杀时已完成 %d 张，接管之后还是 %d 张——选出了新领导者却没人干活"
            "\n%s" % (self.succeeded_at_kill, succeeded_now, self.server.log_tail()))


# ══════════════════════════════════════════════════════════════════════
#  三、多进程同时启动：建表 / 补列 / 写种子数据不许把库搞坏
# ══════════════════════════════════════════════════════════════════════

class ColdStartRaceTests(unittest.TestCase):
    """全新的空目录上一次起 4 个进程，四个进程同时抢着建表、写种子数据。

    ⚠ 已知问题：**冷启动**（数据库文件还不存在）时会有进程崩掉一次再被
    uvicorn 拉起来，日志里能看到
    `table users already exists` / `UNIQUE constraint failed: users.username`。
    原因是 SQLite 上 create_all 没有进程锁（migrations.py 那把锁只在 PostgreSQL
    上有），而 seed() 的「先查再插」在两种数据库上都没有锁。
    详见本文件开头。所以这里断言的是**最终状态对不对**，外加**第二次启动零报错**——
    等哪天那两处补上锁了，请回来加一条「冷启动日志里不许出现 Traceback」。
    """

    server = None       # 第一次启动（空目录，四个进程抢着建库）
    warm = None         # 第二次启动（库已经在了）
    data_dir = None

    @classmethod
    def setUpClass(cls):
        cls.data_dir = tempfile.mkdtemp(prefix="dk-mp-coldstart-")
        try:
            # 第一次：空目录，四个进程同时建库
            cls.server = MultiWorkerServer("cold", data_dir=cls.data_dir).start()
            cls.server.wait_for_leader()
            cls.cold_log = cls.server.log_text()
            cls.cold_worker_pids = cls.server.worker_pids()
            cls.server.stop()
            # 第二次：同一个目录，库已经在了。这一次**一个报错都不许有**。
            cls.warm = MultiWorkerServer("warm", data_dir=cls.data_dir).start()
            cls.warm.wait_for_leader()
            cls.warm_log = cls.warm.log_text()
        except Exception:
            cls.tearDownClass()
            raise

    @classmethod
    def tearDownClass(cls):
        if cls.warm is not None:
            cls.warm.stop()
        if cls.server is not None:
            cls.server.stop()
        shutil.rmtree(cls.data_dir, ignore_errors=True)

    def test_the_service_answers_after_a_cold_multi_process_start(self):
        """服务最终是能用的：healthz 通，而且四个工作进程全都跑完了启动流程。

        MultiWorkerServer.start() 里已经等过这两件事，等不到会直接抛异常，
        这里再断言一次是为了让失败时看到的是「冷启动没起来」而不是一堆连锁错误。
        """
        self.assertEqual(len(self.cold_worker_pids), WORKERS, self.server.log_tail())
        self.assertEqual(len(set(self.cold_worker_pids)), WORKERS, "进程号有重复")

    def test_the_admin_account_is_created_exactly_once(self):
        """四个进程抢着写种子数据，管理员只能有一个。

        多出一个的后果不是「多一行」：登录时按用户名查到两条，
        改密码改的是哪一条全看数据库的心情，而两条的密码互不相同。
        """
        session = self.warm.session()
        try:
            admins = session.execute(
                select(User).where(User.username == DEFAULT_ADMIN_USERNAME)).scalars().all()
            self.assertEqual(len(admins), 1, "管理员账号有 %d 个" % len(admins))
            self.assertEqual(
                session.execute(select(func.count()).select_from(User)).scalar_one(), 1)
        finally:
            session.close()

    def test_seed_templates_and_categories_are_written_exactly_once(self):
        """示例模板与分类不许被写成两份（表面上只是「模板列表里每个都出现两次」）。"""
        session = self.warm.session()
        try:
            templates = session.execute(
                select(func.count()).select_from(PromptTemplate)).scalar_one()
            categories = session.execute(
                select(func.count()).select_from(PromptCategory)).scalar_one()
        finally:
            session.close()
        self.assertEqual(templates, len(SEED_TEMPLATES))
        self.assertEqual(categories, len(SEED_CATEGORIES))

    def test_every_table_declared_in_models_exists(self):
        """抢着建表之后，models.py 声明的每一张表都要在。

        少一张的表现是「某个页面一打开就 500」，而且只在多进程部署上出现。
        """
        session = self.warm.session()
        try:
            existing = set(sa_inspect(session.get_bind()).get_table_names())
        finally:
            session.close()
        missing = sorted(set(Base.metadata.tables.keys()) - existing)
        self.assertEqual(missing, [], "少建了这些表：%r" % (missing,))

    def test_a_restart_on_an_existing_database_logs_no_errors_at_all(self):
        """第二次启动（库已经在了）必须干干净净：一个 Traceback 都不许有。

        这一条守的是「升级重启」这个每次部署都要走的路径：
        重启时如果有进程崩一次再被拉起来，运营看到的是日志里一片红字，
        而系统其实是好的——分辨不出「这次是不是真出事了」比出事本身更麻烦。
        """
        self.assertNotIn("Traceback", self.warm_log, self.warm.log_tail(6000))
        self.assertNotIn("Application startup failed", self.warm_log,
                         self.warm.log_tail(6000))

    def test_only_one_dispatch_lease_row_exists(self):
        """派发权那一行是按任务名建的，四个进程抢完还是只有一行。

        顺带确认它和灵感库、自动开通那两把锁**共用同一张表、各占一行**——
        这就是「加一个任务名就多一把锁、不用改表结构」这句话的验收。
        """
        session = self.warm.session()
        try:
            names = session.execute(select(SyncState.name)).scalars().all()
        finally:
            session.close()
        self.assertEqual(names.count(worker_module.LEASE_TASK), 1,
                         "generation 租约行有 %d 行" % names.count(worker_module.LEASE_TASK))


# ══════════════════════════════════════════════════════════════════════
#  四、限速跨进程共用一个桶 + 真实客户端 IP
# ══════════════════════════════════════════════════════════════════════

# 子进程脚本：**另一个进程**里装一份同样的应用，连**同一个库**，做一次失败登录。
# 它不经过 uvicorn，所以「两个进程各调一次」是确定的——不用赌请求被分到了哪个
# 工作进程上。用的是真实的 database.SessionLocal（靠环境变量指向同一个库），
# 而不是测试里手工造的引擎，这样连「限速到底写进哪个库」也一并验证了。
_ONE_FAILED_LOGIN = '''\
import json, os, sys
sys.path.insert(0, %(root)r)
os.environ["DESIGNKIT_DATA_DIR"] = %(data_dir)r
os.environ["DESIGNKIT_DATABASE_URL"] = %(db_url)r
os.environ["DESIGNKIT_PROVIDER"] = "mock"
os.environ["DESIGNKIT_RATELIMIT_LOGIN_MAX"] = %(max_attempts)r

from fastapi import FastAPI
from fastapi.testclient import TestClient
from backend.app.routers import auth as auth_router

app = FastAPI()
app.include_router(auth_router.router)
client = TestClient(app)
response = client.post("/api/web/auth/login",
                       json={"username": sys.argv[1], "password": "definitely-not-it"})
print(json.dumps({"pid": os.getpid(), "status": response.status_code}))
'''


class CrossProcessRateLimitTests(unittest.TestCase):
    """限速必须记在数据库里，让所有进程共用一个桶。

    记在进程内存里的话，设置里写「3 次」，开 4 个 worker 实际是 12 次，
    而设置页上显示的还是 3——**界面上完全看不出来**，日志里也没有痕迹。
    """

    MAX_ATTEMPTS = 3
    server = None

    @classmethod
    def setUpClass(cls):
        cls.server = MultiWorkerServer("ratelimit", extra_env={
            "DESIGNKIT_RATELIMIT_LOGIN_MAX": str(cls.MAX_ATTEMPTS),
            "DESIGNKIT_RATELIMIT_LOGIN_WINDOW_MINUTES": "15",
            "DESIGNKIT_RATELIMIT_LOGIN_BLOCK_MINUTES": "15",
            # 反代那一层：告诉应用「前面有一层反向代理」，
            # 于是真实来源要从 X-Forwarded-For 的**右边**数第 1 段。
            "DESIGNKIT_TRUSTED_PROXY_HOPS": "1",
        }).start()

    @classmethod
    def tearDownClass(cls):
        if cls.server is not None:
            cls.server.cleanup()

    # -------------------------------------------------------------- 工具

    def _buckets(self):
        session = self.server.session()
        try:
            return session.execute(
                select(RateLimitState.key, RateLimitState.attempts,
                       RateLimitState.blocked_until)
                .where(RateLimitState.scope == "login")).all()
        finally:
            session.close()

    def _clear_buckets(self):
        session = self.server.session()
        try:
            for row in session.execute(select(RateLimitState)).scalars().all():
                session.delete(row)
            session.commit()
        finally:
            session.close()

    def _login(self, password, headers=None):
        return self.server.post_json(
            "/api/web/auth/login",
            {"username": DEFAULT_ADMIN_USERNAME, "password": password},
            headers=headers)

    def setUp(self):
        # 每个用例自己一份干净的桶：这几项共用一台服务器（起服务不便宜），
        # 而限速状态是全局的，不清的话跑在后面的用例会莫名其妙一上来就被封。
        self._clear_buckets()

    # -------------------------------------------------------------- 用例

    def test_two_separate_processes_each_count_into_the_same_bucket(self):
        """**真的另起两个 python 进程**，各做一次失败登录，计数要累加成 2。

        这是本组最直白的一条：进程内存计数在这里当场变红（会是两个各自为政的
        「1 次」，数据库里一行都没有），而在任何单进程测试里都是绿的。
        """
        script = os.path.join(self.server.data_dir, "one_failed_login.py")
        with open(script, "w", encoding="utf-8") as handle:
            handle.write(_ONE_FAILED_LOGIN % {
                "root": _ROOT, "data_dir": self.server.data_dir,
                "db_url": "sqlite:///" + self.server.db_path,
                "max_attempts": str(self.MAX_ATTEMPTS),
            })
        pids = []
        for _ in range(2):
            completed = subprocess.run(
                [sys.executable, script, DEFAULT_ADMIN_USERNAME],
                stdout=subprocess.PIPE, stderr=subprocess.PIPE, timeout=300)
            self.assertEqual(completed.returncode, 0,
                             completed.stderr.decode("utf-8", "replace"))
            payload = json.loads(
                completed.stdout.decode("utf-8").strip().splitlines()[-1])
            self.assertEqual(payload["status"], 401)
            pids.append(payload["pid"])
        self.assertNotEqual(pids[0], pids[1], "两次是同一个进程跑的，这条没测到东西")
        self.assertNotIn(os.getpid(), pids)

        buckets = self._buckets()
        self.assertEqual(len(buckets), 1, "两个进程各开了一个桶：%r" % (buckets,))
        self.assertEqual(buckets[0][1], 2,
                         "两个进程各调一次，计数应该是 2，实际 %r" % (buckets[0][1],))

    def test_the_real_multi_worker_server_blocks_after_the_configured_attempts(self):
        """整条链路走一遍：4 个工作进程的真实服务，撞满设置值之后连正确密码也进不来。

        三次失败**并发发出**，让它们尽量落到不同的工作进程上。
        计数是数据库里的条件 UPDATE（attempts+1 在数据库里算），并发不会丢数。
        """
        results = []
        lock = threading.Lock()

        def _hit():
            status, _headers, _body = self._login("definitely-not-it")
            with lock:
                results.append(status)

        threads = [threading.Thread(target=_hit) for _ in range(self.MAX_ATTEMPTS)]
        for thread in threads:
            thread.start()
        for thread in threads:
            thread.join(timeout=60)
        self.assertEqual(results, [401] * self.MAX_ATTEMPTS, results)

        buckets = self._buckets()
        self.assertEqual(len(buckets), 1, "不该开出第二个桶：%r" % (buckets,))
        self.assertEqual(buckets[0][1], self.MAX_ATTEMPTS,
                         "并发下丢了计数：%r" % (buckets[0][1],))

        status, headers, body = self._login("admin123456")   # 密码是对的
        self.assertEqual(status, 429, body)
        self.assertIn("分钟后再试", body.get("detail", ""))
        # 标准头，给脚本和反代看；缺了它，重试的人只能靠猜
        self.assertGreater(int(headers["retry-after"]), 0)

    def test_a_forged_left_hand_xff_cannot_open_a_new_bucket(self):
        """X-Forwarded-For 要从**右边**数：最左边那一段是客户端自己写的。

        这一条和 test_invite_register 里的同名用例看着像，实测的东西不一样：
        那边直接调应用，碰不到 uvicorn；这边请求真的穿过 uvicorn，而 uvicorn
        自己也会按 --forwarded-allow-ips 改写 request.client。
        两层叠在一起会不会打架，**只有真的起 uvicorn 才看得出来**。
        """
        real = "203.0.113.7"
        for index in range(self.MAX_ATTEMPTS):
            status, _headers, _body = self._login(
                "definitely-not-it",
                {"X-Forwarded-For": "10.0.0.%d, %s" % (index + 1, real)})
            self.assertEqual(status, 401)

        buckets = self._buckets()
        self.assertEqual(len(buckets), 1,
                         "伪造的前缀各开了一个桶，限速等于不存在：%r" % ([b[0] for b in buckets],))
        self.assertTrue(buckets[0][0].startswith(real + "|"),
                        "桶名里记的不是真实来源：%r" % (buckets[0][0],))

        # 换一个从没出现过的伪造前缀，还是同一个桶，照样被拦
        status, _headers, body = self._login(
            "admin123456", {"X-Forwarded-For": "8.8.8.8, " + real})
        self.assertEqual(status, 429, body)

    def test_two_different_real_sources_get_their_own_buckets(self):
        """反面：右边那一段不同 = 真的是两个来源，必须各算各的。

        少了这一条，「全站共用一个桶」也能让上一条通过——那样一个人被锁，
        所有人跟着一起登不进来，而且看起来像是「系统坏了」。
        """
        for _ in range(self.MAX_ATTEMPTS):
            self._login("definitely-not-it", {"X-Forwarded-For": "203.0.113.7"})
        status, _headers, _body = self._login(
            "admin123456", {"X-Forwarded-For": "203.0.113.7"})
        self.assertEqual(status, 429)
        # 另一个真实来源不受影响（密码对就该进得来）
        status, _headers, body = self._login(
            "admin123456", {"X-Forwarded-For": "198.51.100.9"})
        self.assertEqual(status, 200, body)


# ══════════════════════════════════════════════════════════════════════
#  五、租约本身的几条行为（在本进程里跑，快）
# ══════════════════════════════════════════════════════════════════════

class _CountingHandler(logging.Handler):
    """只数条数、不输出内容。用来断言「这一步不该打日志」。

    Python 3.10 才有 assertNoLogs，这个项目锁在 3.9，所以自己数。
    """

    def __init__(self):
        logging.Handler.__init__(self, level=logging.DEBUG)
        self.count = 0

    def emit(self, _record):
        self.count += 1


class LeaseMechanicsTests(unittest.TestCase):
    """上面那些要真起进程，慢；这几条只验租约自己的逻辑，几毫秒一条。

    它们守的是**文档里写给运营的那几句话**：日志里搜什么词、重启之后要等多久。
    措辞和行为一旦对不上，运营照着文档查会一无所获，而且不会有任何报错。
    """

    def setUp(self):
        self.tmpdir = tempfile.mkdtemp(prefix="dk-mp-lease-")
        self.engine = create_engine(
            "sqlite:///" + os.path.join(self.tmpdir, "t.db"),
            connect_args={"check_same_thread": False, "timeout": 30}, future=True)
        Base.metadata.create_all(self.engine)
        self.Session = sessionmaker(
            bind=self.engine, autoflush=False, expire_on_commit=False, future=True)
        # 把 worker 模块里的 SessionLocal 换成指向临时库的那个。
        # 换的是模块属性，不是 database.py 本身，跑完还原，不影响别的测试。
        self._real_session_local = worker_module.SessionLocal
        worker_module.SessionLocal = self.Session

        # 日志级别必须自己调到 INFO。默认是 WARNING，那时候 logger.info() 根本不会
        # 走到 handler——「数一数打了几条」会永远数出 0，几条断言全变成假绿。
        self._logger = logging.getLogger("designkit.worker")
        self._old_level = self._logger.level
        self._logger.setLevel(logging.INFO)
        self._counter = _CountingHandler()
        self._logger.addHandler(self._counter)

    def tearDown(self):
        self._logger.removeHandler(self._counter)
        self._logger.setLevel(self._old_level)
        worker_module.SessionLocal = self._real_session_local
        self.engine.dispose()
        shutil.rmtree(self.tmpdir, ignore_errors=True)

    def _lease_row(self):
        session = self.Session()
        try:
            return session.get(SyncState, worker_module.LEASE_TASK)
        finally:
            session.close()

    def test_the_leader_log_line_carries_the_pid_and_the_cap(self):
        """「取得生成派发权」这几个字是 docs/deploy-server.md 让运营去日志里搜的词。

        这一行同时回答两个问题：现在是哪个进程在派发、上限是几张。
        改了措辞就等于把文档里那句「在日志里搜『取得生成派发权』」变成了空话。
        """
        first = worker_module.GenerationWorker()
        first._concurrency = 3
        with self.assertLogs("designkit.worker", level="INFO") as captured:
            self.assertTrue(first._ensure_leadership())
        text = "\n".join(captured.output)
        self.assertIn("取得生成派发权", text)
        self.assertIn(str(os.getpid()), text)
        self.assertIn("上限 3 张", text)

    def test_a_standby_process_logs_once_and_then_keeps_quiet(self):
        """第二个进程抢不到派发权时，只说一次「转入待命」，之后闭嘴。

        待命进程 5 秒轮询一次。每轮都打的话，4 个进程一天几十万行，
        真正要看的那几行会被彻底盖掉。
        """
        leader = worker_module.GenerationWorker()
        self.assertTrue(leader._ensure_leadership())

        standby = worker_module.GenerationWorker()
        with self.assertLogs("designkit.worker", level="INFO") as captured:
            self.assertFalse(standby._ensure_leadership())
        self.assertIn("转入待命", "\n".join(captured.output))

        # 再抢几次：不该再打日志。assertNoLogs 是 Python 3.10 才有的，
        # 这个项目是 3.9，所以自己挂一个数日志条数的 handler。
        before = self._counter.count
        for _ in range(3):
            self.assertFalse(standby._ensure_leadership())
        self.assertEqual(self._counter.count - before, 0, "待命日志又打了一遍")

    def test_stopping_returns_the_lease_so_a_restart_does_not_wait(self):
        """正常停机会**主动交还**派发权，下一个进程立刻就能接上。

        不交还的话，重启后新进程要干等租约过期（最多 30 秒），
        表现是「重启完半分钟不出图」——而 docs/deploy-server.md 明确承诺
        「正常重启（docker restart）立刻恢复出图」。这一条就是那句承诺的验收。
        """
        leader = worker_module.GenerationWorker()
        self.assertTrue(leader._ensure_leadership())
        self.assertIsNotNone(self._lease_row().lock_owner)

        leader.stop()
        self.assertIsNone(self._lease_row().lock_owner, "停机没有交还派发权")

        successor = worker_module.GenerationWorker()
        self.assertTrue(successor._ensure_leadership(), "交还之后应当立刻抢得到")

    def test_a_stopping_process_never_grabs_the_lease_back(self):
        """已经在停机的进程不许再把派发权抢回来。

        抢回来的后果：进程一退，这把锁挂在那儿没人续租，接班的进程白等一整个
        租约（30 秒不出图），而日志里看起来一切正常。
        """
        leaving = worker_module.GenerationWorker()
        leaving.stop()                      # 没当过领导者，stop 只是置了停止位
        self.assertFalse(leaving._ensure_leadership())
        self.assertIsNone(self._lease_row(), "停机中的进程不该去建租约行")

    def test_the_heartbeat_says_how_many_are_running_and_how_many_are_queued(self):
        """心跳这一行是回答「为什么图出得慢」的唯一依据，措辞不能变。

        慢有三种完全不同的原因：队列积压、并发被调小了、派发权跑到别的进程上。
        这一行同时回答这三个，没有它只能靠猜。
        """
        session = self.Session()
        try:
            for index in range(3):
                session.add(GenerationJob(
                    source="web", template_name="心跳",
                    prompt_final="排队中 %d" % index,
                    params={"n": 1}, input_paths=[], status="pending"))
            session.commit()
        finally:
            session.close()

        leader = worker_module.GenerationWorker()
        leader._concurrency = 2
        leader._inflight = 1
        with self.assertLogs("designkit.worker", level="INFO") as captured:
            leader._heartbeat()
        text = "\n".join(captured.output)
        self.assertIn("生成派发中", text)
        self.assertIn("正在生成 1/2 张", text)
        self.assertIn("队列里还有 3 个", text)

    def test_the_heartbeat_stays_silent_when_there_is_nothing_to_do(self):
        """闲着的时候一个字都不打——不然日志里全是「正在生成 0/2 张」。

        assertLogs 在「一条都没打」时会失败，正好拿来当断言用。
        """
        leader = worker_module.GenerationWorker()
        before = self._counter.count
        leader._heartbeat()
        self.assertEqual(self._counter.count - before, 0, "空闲时不该打心跳")

    def test_the_lease_shares_the_table_with_the_other_two_locks(self):
        """`generation` 只是 sync_state 里多出来的一行，和另外两把锁井水不犯河水。

        这就是「加一个任务名就多一把锁、不用改表结构、老库不用迁移」的验收。
        """
        leader = worker_module.GenerationWorker()
        self.assertTrue(leader._ensure_leadership())

        session = self.Session()
        try:
            # 灵感库那把锁照样抢得到——两把锁互不影响
            self.assertTrue(scheduler_locks._acquire(session, "另一路任务"))
            names = session.execute(select(SyncState.name)).scalars().all()
        finally:
            session.close()
        self.assertIn(worker_module.LEASE_TASK, names)
        self.assertIn(scheduler_locks.TASK_NAME, names)
        self.assertNotEqual(worker_module.LEASE_TASK, scheduler_locks.TASK_NAME)


if __name__ == "__main__":
    unittest.main()
