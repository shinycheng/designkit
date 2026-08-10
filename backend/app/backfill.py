"""启动期一次性数据回填：给历史数据补上「这条记录属于谁」。

────────────────────────────────────────────────────────────────────────
这个文件在解决什么问题
────────────────────────────────────────────────────────────────────────
系统原来只有一个管理员账号，所以「记录属于谁」这件事从来没人关心过：

  - `api_keys` 表压根没有归属列（这一列是刚加的，老库里全是空的）；
  - ERP 通过对外接口建的任务和上传的图，`user_id` 一直是空的
    （`routers/v1.py` 建任务时只传了 api_key_id，没传 user_id）。

接下来「每个人只看得到自己的东西」这件事，靠的正是 `user_id` 这一列。
而 SQL 里 `NULL` 不等于任何值——`WHERE user_id = 3` 永远筛不出 user_id 为空的行。
所以升级之后如果不回填，**所有历史 ERP 任务和图片会对所有人都不可见**：
数据一条没丢、磁盘上的图也都还在，但管理员在网页上再也翻不到它们。

更麻烦的是这个故障会被掩盖很久：ERP 那边是按 api_key 查任务的
（`routers/v1.py` 的查询走 api_key_id，不看 user_id），完全不受影响，
对接方一切正常，只有网页端「历史记录突然少了一大截」，而且没人能归因。

────────────────────────────────────────────────────────────────────────
为什么失败必须被人看见（这是本文件最重要的一条设计）
────────────────────────────────────────────────────────────────────────
项目里有个反面教材：灵感库的分类标签回填在 `main.py` 里被 try/except 兜住、
失败了只打一行日志。那个回填失败顶多是分类清单空着，属于装饰性问题，
而运营同学根本不会去看容器日志。

归属回填不一样：它的产物是数据隔离的唯一依据，静默失败等于**全员历史清空**。
所以这里的做法是——回填照旧不挡启动（挡死启动会让容器在 `restart: always`
下无限重启，运营同学更没辙），但结果会写进 `app_settings` 的两个开关：

    backfill_ok     True / False   回填是不是全都做完了
    backfill_error  一句人话        没做完的原因，空串表示没问题

这两个值有两个消费者：
  1. 管理员界面顶部的红色横幅（「历史数据归属回填未完成…」）；
  2. 将来「开放注册」的前置校验——没回填完不许开注册，
     否则新用户和无主的历史数据会搅在一起，越往后越难收拾。

读取用本文件的 `get_backfill_health(db)`，不要各自去拼 SQL。

────────────────────────────────────────────────────────────────────────
为什么可以放心重复执行
────────────────────────────────────────────────────────────────────────
开工前先数一遍「还有多少行要补」，一行都不用补就整个跳过，连一条 UPDATE 都不发
（日常重启走的都是这条路）。真要补的时候，每条 UPDATE 的 WHERE 里都带着
`user_id IS NULL`，补过的行不会被第二次改写。所以重启多少次都是同一个结果。

多进程（gunicorn 多 worker / 多容器）同时启动也不会互相打架：
三张表的处理顺序是固定的（api_keys → generation_jobs → uploads），
两个进程不会反着加锁而死锁；条件相同的 UPDATE 谁先跑完，后一个就改到 0 行。
"""
import logging
from typing import Any, Dict, List, Optional, Tuple

from sqlalchemy import func, select, update
from sqlalchemy.orm import Session

from .models import ApiKey, AppSetting, GenerationJob, Upload, User

logger = logging.getLogger("designkit.backfill")

# 健康标志存在 app_settings 里的键名。改名会同时打断前端横幅和开放注册的闸门，
# 所以哪怕只是觉得名字不好听也别改——真要改，先把这两个消费者一起改掉。
BACKFILL_OK_KEY = "backfill_ok"
BACKFILL_ERROR_KEY = "backfill_error"

# 没有任何记录时的默认值：**默认认为是好的**。
# 全新安装的库根本没有历史数据要回填，这时候弹一条红色横幅纯属误报，
# 而误报几次之后，真出事那次的横幅也就没人看了。
_DEFAULT_HEALTH = {BACKFILL_OK_KEY: True, BACKFILL_ERROR_KEY: ""}

# 回填中途被打断时留在库里的说明。写它是为了盖住这种情况：
# 进程刚跑到一半就被强杀（NAS 重启、容器 OOM、数据库连接被掐），
# 代码根本没机会走到任何 except 分支，标志会停在「进行中」这个状态上，
# 下次启动重试成功后再被翻回 True。
_INTERRUPTED_MESSAGE = (
    "历史数据归属回填没有跑完就被打断了（可能是升级过程中重启了容器）。"
    "重启一次系统会自动重试；如果这条提示反复出现，请联系技术支持。"
)


def _read_setting(db: Session, key: str, default: Any) -> Any:
    """读一条设置。故意不走 settings_service.get——原因见 _write_setting。"""
    row = db.get(AppSetting, key)
    if row is not None and isinstance(row.value, dict) and "v" in row.value:
        return row.value["v"]
    return default


def _write_setting(db: Session, key: str, value: Any) -> None:
    """写一条设置（不提交，由调用方统一 commit）。

    为什么不用 `services/settings_service.set_many`：那个函数开头有一句
    `if key not in RUNTIME_DEFAULTS: continue`——键不在默认表里就**静默丢掉**。
    而 backfill_ok / backfill_error 眼下并不在 RUNTIME_DEFAULTS 里，
    真用了 set_many，这个「让失败被看见」的机制自己会先静默失效一次，
    刚好把本文件想防的坑原样踩一遍。

    存储格式和 settings_service 完全一致（都是 `{"v": 值}` 包一层），
    所以哪天把这两个键补进 RUNTIME_DEFAULTS，set_many / get_all 能直接读写它们，
    不需要改数据、也不需要改这里。
    """
    row = db.get(AppSetting, key)
    if row is None:
        db.add(AppSetting(key=key, value={"v": value}))
    else:
        # 整个 dict 换掉（而不是改 dict 里的键），SQLAlchemy 才认得出这是一次修改
        row.value = {"v": value}


def get_backfill_health(db: Session) -> Dict[str, Any]:
    """读回填健康状态，给设置接口 / 前端横幅 / 开放注册闸门用。

    返回 `{"backfill_ok": bool, "backfill_error": str}`。
    库里没有记录时返回默认值（ok=True，见 _DEFAULT_HEALTH 的说明）。
    """
    ok = _read_setting(db, BACKFILL_OK_KEY, _DEFAULT_HEALTH[BACKFILL_OK_KEY])
    error = _read_setting(db, BACKFILL_ERROR_KEY, _DEFAULT_HEALTH[BACKFILL_ERROR_KEY])
    return {BACKFILL_OK_KEY: bool(ok), BACKFILL_ERROR_KEY: str(error or "")}


def _set_health(db: Session, ok: bool, error: str = "") -> None:
    """写健康标志并立刻提交。

    单独提交（不跟回填的 UPDATE 混在一个事务里）是有意的：回填失败时事务要回滚，
    而标志恰恰是那次失败留下的唯一线索，绝不能被一起滚掉。
    """
    _write_setting(db, BACKFILL_OK_KEY, bool(ok))
    _write_setting(db, BACKFILL_ERROR_KEY, error or "")
    db.commit()


def _safe_mark_failed(db: Session, message: str) -> None:
    """把失败原因写进健康标志。这是失败之后唯一还能做的事，本身再失败就只剩日志了。"""
    try:
        db.rollback()
        _set_health(db, False, message)
    except Exception:  # noqa: BLE001
        # 走到这里说明数据库基本上是不可用的，此时应用大概率也起不来，
        # 打一条 critical 至少让日志里留下痕迹。
        logger.critical("连回填失败标志都写不进数据库，界面上将看不到这次失败")


def _safe_mark_ok(db: Session) -> bool:
    """把标志翻回「一切正常」，返回有没有写成功。

    写不进去时**返回 False**（而不是当作成功）：数据其实已经补好了，但库里还留着
    「未完成」那条记录，界面上的红色横幅不会消失。这时候报成功等于骗人——
    横幅明明还亮着。下次重启会发现没活可干，顺手把标志改回来（见 run_backfill 开头）。
    """
    try:
        _set_health(db, True, "")
        return True
    except Exception:  # noqa: BLE001
        db.rollback()
        logger.exception("回填已完成，但状态标志写不进数据库，界面上的告警横幅不会自动消失")
        return False


def _owner_of_api_key(model) -> Any:
    """「这条记录用的那把 Key 属于谁」——一个可以直接嵌进 UPDATE 的子查询。

    写成关联子查询而不是 `UPDATE ... FROM`，是因为后者的写法 SQLite 和
    PostgreSQL 并不一致（老版本 SQLite 干脆不支持）。实测这个写法在两个数据库上
    编译出来的 SQL 一模一样，本机跑通就等于 NAS 上也跑得通。
    """
    return (
        select(ApiKey.user_id)
        .where(ApiKey.id == model.api_key_id)
        .correlate(model)
        .scalar_subquery()
    )


def _pending_conditions(model) -> List[Any]:
    """筛出「还没有归属、但顺着 api_key 能查出归属」的行。

    第三个条件（子查询结果非空）不能省。少了它，那些指向「已经不存在的 Key」
    或者「Key 本身也还没有归属」的记录会一直被算作待处理，
    UPDATE 却永远补不上它们——结果就是红色横幅永久挂在页面上、
    重启多少次都不消失。而横幅一旦变成常态，就再也没人当回事了。
    """
    return [
        model.user_id.is_(None),
        model.api_key_id.is_not(None),
        _owner_of_api_key(model).is_not(None),
    ]


def _count(db: Session, model, conditions: List[Any]) -> int:
    return int(
        db.execute(
            select(func.count()).select_from(model).where(*conditions)
        ).scalar_one()
    )


def _count_orphans(db: Session, model) -> int:
    """数一数「有 api_key_id、但顺着它查不到归属」的行。

    正常情况下应该是 0。真出现了也没法自动补——那把 Key 已经不在了，
    谁建的这条记录已经无从考证。所以只打日志提醒，不把它算成回填失败：
    这种行本来就无解，为它把红色横幅永久点亮反而会淹没真正的故障。
    """
    return _count(
        db,
        model,
        [
            model.user_id.is_(None),
            model.api_key_id.is_not(None),
            _owner_of_api_key(model).is_(None),
        ],
    )


def _fill_owner_from_api_key(db: Session, model) -> int:
    """把 api_key 的归属抄到记录上，返回改了多少行。

    性能：`uploads` 有十几万行历史记录，这条语句会把它整张扫一遍，
    每行再按主键去 `api_keys`（只有个位数行）查一次归属——十来万次主键查询，
    实测是秒级。它只在升级后的第一次启动跑这一次，为它专门建索引不划算。
    """
    stmt = (
        update(model)
        .where(*_pending_conditions(model))
        .values(user_id=_owner_of_api_key(model))
    )
    # synchronize_session=False：这里是「按条件批量改」，改完之后本次会话里
    # 已经加载的对象不需要同步（启动期也没有别的对象），交给数据库直接执行最快。
    result = db.execute(stmt.execution_options(synchronize_session=False))
    return int(result.rowcount or 0)


def _survey(db: Session) -> Tuple[int, int, int]:
    """数一遍三处还有多少行要补：(api_keys, generation_jobs, uploads)。

    ⚠️ 后两个数字有个**先后顺序的坑**，改这里之前务必读完：
    任务/上传是不是「待回填」，取决于它用的那把 Key 有没有归属。
    所以在 api_keys 补完之前数，后两个数一定偏小——极端情况下
    「1 把无主 Key + 它名下 5 万条任务」数出来是 (1, 0, 0)。
    这正是本函数只用来**判断有没有活干**、而不用来决定「跳过哪一步」的原因：
    真正干活的时候三步都要跑（UPDATE 本身没活干就是条空语句，不花钱）。
    第一版就是照着前两个数去 `if pending_jobs:` 分支，结果 Key 补上了、
    任务一条没补，最后靠复核那一步才把问题兜住。
    """
    return (
        _count(db, ApiKey, [ApiKey.user_id.is_(None)]),
        _count(db, GenerationJob, _pending_conditions(GenerationJob)),
        _count(db, Upload, _pending_conditions(Upload)),
    )


def _platform_owner(db: Session) -> Optional[User]:
    """存量数据归谁：**建得最早的那个管理员**。

    按 id 升序取第一个，而不是「任意一个管理员」，是为了让结果稳定可复现：
    多进程同时启动、或者出问题后重跑一次，落到的都得是同一个人，
    否则历史数据会被分给不同的人，事后连「到底归了谁」都说不清。
    """
    return (
        db.query(User)
        .filter(User.role == "admin")
        .order_by(User.id)
        .first()
    )


def run_backfill(db: Session) -> bool:
    """跑一遍归属回填，返回是否全部成功。

    调用方（`main.py` 的 lifespan）应当：
      1. 在 `seed(db)` **之后**调用——回填要用到管理员账号，
         全新安装时那个账号正是 seed 建出来的；
      2. 用 try/except 兜住，别让它挡死启动；
      3. 返回 False 时打一条 error 日志。界面上的红色横幅由本函数写进
         app_settings 的标志驱动，不依赖调用方做任何事。
    """
    try:
        pending_keys, pending_jobs, pending_uploads = _survey(db)
    except Exception as exc:  # noqa: BLE001 —— 连数一下都失败，说明库结构不对
        db.rollback()
        logger.exception("清点待回填数据失败，归属回填没有执行")
        _safe_mark_failed(db, "清点历史数据时数据库报错：%s" % exc)
        return False

    total = pending_keys + pending_jobs + pending_uploads
    # total 为 0 时可以放心跳过：走到这里说明每把 Key 都有归属了，
    # 后两个数字不再受 _survey 里说的那个顺序问题影响，是准的。
    if total == 0:
        # 绝大多数重启都会走到这里：没活可干，一条 UPDATE 都不发。
        # 顺手把标志修好——上一次失败留下的 False，会在问题实际解决后自动清掉，
        # 不然横幅会一直挂着，用户以为没修好。
        if not get_backfill_health(db)[BACKFILL_OK_KEY]:
            logger.info("历史数据归属已经补齐，清除之前的回填告警")
            return _safe_mark_ok(db)
        return True

    # 这里的任务/上传条数是「至少」——原因见 _survey 的注释，实际条数在补完
    # 对接密钥之后才数得准，准确值由下面那条「已回填 …」日志给出。
    logger.info(
        "开始回填历史数据归属：待处理对接密钥 %d 条，生成任务至少 %d 条、上传图片至少 %d 条",
        pending_keys, pending_jobs, pending_uploads,
    )
    # 先把标志打到「未完成」再干活：万一干到一半进程被强杀（升级时重启容器、
    # 容器被 OOM 杀掉），代码走不到任何 except 分支，但库里留着这条记录，
    # 界面上的横幅照样会亮。干完了再翻回 True。
    try:
        _set_health(db, False, _INTERRUPTED_MESSAGE)
    except Exception:  # noqa: BLE001
        db.rollback()
        logger.exception("写入回填状态失败，回填照常执行，但界面上看不到进度")

    try:
        if pending_keys:
            owner = _platform_owner(db)
            if owner is None:
                # 一个管理员都没有，这批 Key 归不了任何人。后面两步也就无从谈起。
                db.rollback()
                message = (
                    "系统里找不到管理员账号，%d 条历史对接密钥没法确定归属，"
                    "由此关联的历史任务和图片也补不上。请联系技术支持。" % pending_keys
                )
                logger.error(message)
                _safe_mark_failed(db, message)
                return False
            changed = (
                db.query(ApiKey)
                .filter(ApiKey.user_id.is_(None))
                .update({ApiKey.user_id: owner.id}, synchronize_session=False)
            )
            logger.info(
                "已把 %d 条历史对接密钥归到管理员「%s」名下",
                changed, owner.username,
            )

        # 顺序固定：必须先补 api_keys 的归属，下面两步才抄得到东西。
        # 这两步**不加 `if pending_xxx:` 判断**——刚补完 Key，之前数出来的 0
        # 已经过期了（原因见 _survey 的注释）。没活干时这两条 UPDATE 就是空转，
        # 而且整个函数一辈子也就在升级那天真正跑一次，多发两条语句无所谓。
        changed_jobs = _fill_owner_from_api_key(db, GenerationJob)
        changed_uploads = _fill_owner_from_api_key(db, Upload)
        logger.info(
            "已回填 %d 条历史生成任务、%d 张历史上传图片的归属",
            changed_jobs, changed_uploads,
        )

        db.commit()
    except Exception as exc:  # noqa: BLE001
        db.rollback()
        logger.exception("回填历史数据归属失败")
        _safe_mark_failed(
            db,
            "回填历史数据归属时数据库报错：%s。"
            "部分历史记录可能在网页上看不到，请联系技术支持。" % exc,
        )
        return False

    # 复核：真按预期补上了吗。UPDATE 不报错不等于补到位——比如 WHERE 条件里
    # 有个想不到的边界情况，rowcount 是 0 而程序一路绿灯。数据隔离这种事
    # 值得多花一次 count 去确认，而这几条 count 一辈子只跑这一次。
    try:
        left_keys, left_jobs, left_uploads = _survey(db)
    except Exception as exc:  # noqa: BLE001
        db.rollback()
        logger.exception("回填后复核失败")
        _safe_mark_failed(db, "回填后复核历史数据时数据库报错：%s" % exc)
        return False

    if left_keys or left_jobs or left_uploads:
        message = (
            "历史数据归属没有全部补上（还剩对接密钥 %d 条、生成任务 %d 条、"
            "上传图片 %d 条），这部分历史记录在网页上可能看不到，请联系技术支持。"
            % (left_keys, left_jobs, left_uploads)
        )
        logger.error(message)
        _safe_mark_failed(db, message)
        return False

    for model, label in ((GenerationJob, "生成任务"), (Upload, "上传图片")):
        orphans = _count_orphans(db, model)
        if orphans:
            logger.warning(
                "有 %d 条历史%s关联的对接密钥已经不存在，无法确定归属，"
                "这部分记录只有通过对外接口按密钥查询才看得到（不影响系统运行）",
                orphans, label,
            )

    logger.info("历史数据归属回填完成")
    return _safe_mark_ok(db)
