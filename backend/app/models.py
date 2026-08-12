import uuid
from datetime import datetime
from typing import Optional

from sqlalchemy import (
    JSON,
    Boolean,
    DateTime,
    Float,
    ForeignKey,
    Index,
    Integer,
    String,
    Text,
)
from sqlalchemy.orm import Mapped, mapped_column, relationship

from .database import Base


def _uuid() -> str:
    return uuid.uuid4().hex


def utcnow() -> datetime:
    return datetime.utcnow()


class User(Base):
    __tablename__ = "users"

    id: Mapped[int] = mapped_column(Integer, primary_key=True)
    username: Mapped[str] = mapped_column(String(64), unique=True, index=True)
    password_hash: Mapped[str] = mapped_column(String(256))
    display_name: Mapped[str] = mapped_column(String(64), default="")
    role: Mapped[str] = mapped_column(String(16), default="member")  # admin / member
    is_active: Mapped[bool] = mapped_column(Boolean, default=True)
    # 仍是初始默认密码时为 True，登录后前端强制改密
    must_change_password: Mapped[bool] = mapped_column(Boolean, default=False)
    # 令牌版本：改密码/停用时 +1，使已签发的旧 JWT 立即失效
    token_version: Mapped[int] = mapped_column(Integer, default=0)
    # 预留：转对外产品时的每月生成额度（None = 不限制）
    monthly_quota: Mapped[Optional[int]] = mapped_column(Integer, nullable=True)
    created_at: Mapped[datetime] = mapped_column(DateTime, default=utcnow)


# ══════════════════════════════════════════════════════════════════════
#  身份体系：一个人 = 一行 users + 若干种登录方式
# ══════════════════════════════════════════════════════════════════════

# auth_identities.provider 的取值：这个人是用哪一种方式登录进来的。
# **本期只会出现 "password" 一种**，其余三个先把名字定下来，是为了让以后加
# 手机号 / 微信 / QQ 时只需要往这张表里插行，不用回头改任何已有的表或代码。
#
# 为什么本期只做 password：手机号验证码要先申请短信签名和模板（需备案主体、
# 1~3 个工作日），而且短信是真花钱、被刷就是费用黑洞；微信 / QQ 要开放平台的
# 企业资质和应用审核，周期以周计。邀请码零成本、当天能上、随时能撤。
AUTH_IDENTITY_PROVIDERS = (
    "password",  # 用户名 + 密码（identifier = 用户名）
    "phone",     # 手机号 + 短信验证码（identifier = 手机号，11 位纯数字）
    "wechat",    # 微信开放平台（identifier = openid）
    "qq",        # QQ 互联（identifier = openid）
)


class AuthIdentity(Base):
    """一种「登录方式」和它指向的人。一个 User 可以有多行。

    为什么要单独一张表，而不是继续往 users 上加 phone / wechat_openid 这些列：
    ① 每加一种登录方式就要给 users 加一列，而「给老表加列」在本项目里是有类型
       陷阱的一条路（详见 migrations.py 文件头），新表则是零成本；
    ② 「这个标识有没有被别人占用」需要唯一约束，摊在 users 上就是每种方式一个
       部分唯一索引，SQLite 和 PostgreSQL 的写法还不一样；
    ③ 一个人绑了几种方式、哪种是什么时候绑的、最近一次用哪种登的，摊平在 users
       上就只能靠一堆互相矛盾的空值去猜。

    这张表是**新表**，由 create_all 自动建出、带齐全部列和索引，对存量数据零影响
    （新表随时能建，老表加列才有风险——见 migrations.py 的「纪律三」）。
    所以这里故意一次把四种方式要用的列都留好，以后加手机号是纯增量。

    ⚠ 本期的登录仍然走 users.password_hash（routers/auth.py 读的是那里），
    这张表在本期只用于**记录一个账号是怎么来的、绑了哪些方式**，不是登录的判据。
    存量用户在这张表里没有任何行，这是正常的、也不需要回填——回填与否都不影响
    任何人登录。真要把密码校验搬到这张表来，是一次单独的、需要连同 auth.py
    和 users.password_hash 一起改的动作，不要顺手做。
    """

    __tablename__ = "auth_identities"

    id: Mapped[int] = mapped_column(Integer, primary_key=True)
    # 这行身份属于谁。删用户时连带删掉他的所有登录方式（CASCADE）：
    # 登录方式脱离了人没有任何意义，留着反而会让那个手机号 / openid 永久被占着，
    # 本人换个账号重新绑时会撞唯一约束，而错误信息里完全看不出是被自己的旧号占了。
    user_id: Mapped[int] = mapped_column(
        ForeignKey("users.id", ondelete="CASCADE"), index=True
    )
    # 取值见上面的 AUTH_IDENTITY_PROVIDERS
    provider: Mapped[str] = mapped_column(String(16))
    # 这种方式下的标识：用户名 / 手机号 / openid。
    # **写进来之前必须先归一化**（用户名统一小写、手机号只留 11 位数字、
    # openid 原样），归一化由写入方负责。不归一的话，"Tom" 和 "tom" 会各占一行、
    # 都能建出来，然后登录时按哪一个查就成了玄学。
    identifier: Mapped[str] = mapped_column(String(128))
    # 这种方式自带的凭据（密码类方式的哈希）。**本期恒为 NULL**：
    # 密码的唯一真相仍在 users.password_hash，两处都存必然会出现「改了一处、
    # 另一处还是老密码」，而且是那种「有时能登进去有时登不进去」的鬼故事。
    # 留这一列是为了将来搬家时不用给老表加列，不是让本期两头写。
    credential: Mapped[Optional[str]] = mapped_column(String(256), nullable=True)
    # 这个标识核验过了没（手机号收到过验证码 / 第三方回调验过 openid）。
    # password 方式没有「核验」这一说，建行时直接填上创建时间即可。
    verified_at: Mapped[Optional[datetime]] = mapped_column(DateTime, nullable=True)
    # 第三方带回来的附加信息（微信 unionid、昵称等）。用 JSON 是因为每家字段都不同，
    # 为它们各加几列会把这张表撑成一张只有零星几格有值的宽表。
    # ⚠ 别往这里塞 access_token / refresh_token 之类的凭据：这一列是明文的。
    extra: Mapped[Optional[dict]] = mapped_column(JSON, nullable=True)
    last_login_at: Mapped[Optional[datetime]] = mapped_column(DateTime, nullable=True)
    created_at: Mapped[datetime] = mapped_column(DateTime, default=utcnow)

    __table_args__ = (
        # 唯一约束建在 (provider, identifier) 上：**同一种方式下的标识不能重复**。
        # 不能只对 identifier 建唯一——手机号 13800000000 和某个恰好长一样的
        # 用户名会互相撞车；也不能不建——那样同一个微信号能绑到两个人身上，
        # 登录时查出两行，代码只会拿第一行，等于随机登进别人的账号。
        Index(
            "uq_auth_identities_provider_identifier",
            "provider", "identifier",
            unique=True,
        ),
        # 一个人同一种方式只留一行：换手机号是「改这一行」或「删了重绑」，
        # 不是「再加一行」。少了这条，一个人会绑上两个手机号，管理员想解绑时
        # 解掉一个还剩一个，而界面上只显示一个，怎么解都解不干净。
        # 将来真要支持「一人多号」，把这条索引去掉就行（DROP INDEX 两边写法一致，
        # 是个纯放宽的变更）；反过来事后再加就得先清理存量重复，代价大得多。
        Index(
            "uq_auth_identities_user_provider",
            "user_id", "provider",
            unique=True,
        ),
    )


# ══════════════════════════════════════════════════════════════════════
#  邀请码：本期「让用户自己开户」的唯一入口
# ══════════════════════════════════════════════════════════════════════


class InviteCode(Base):
    """一张邀请码。发给谁、谁才能注册。

    为什么用邀请码开第一版：零成本、不用等任何资质、当天能上，限速压力也小，
    出了事随手作废即可。手机号 / 微信排在后面（要资质、要审核、短信要花钱）。

    ⚠ 码是**明文**存在这一列里的，这是有意的取舍：
    运营同学要在界面上看见码、复制下来发给人，还要能重发给同一个人——哈希之后
    这三件事全做不了（只剩「作废重发一张新码」这一条路，对方手里的旧码会突然失效，
    而他完全不知道为什么）。代价是：拿到数据库或备份的人就拿到了所有未用的码。
    这个代价可以接受，因为一张码只能换来一个**普通成员**账号（拿不到管理员权限、
    拿不到别人的数据、也拿不到网关 Key），而且管理员随时能作废。
    → 但这也意味着：**别把 max_uses 设得很大又不设有效期**，那等于一张长期
      有效的万能通行证躺在库里。界面上创建时的默认值见 config.RUNTIME_DEFAULTS
      里的 invite_code_default_*。

    并发要点（写给实现兑换接口的人）：`used_count < max_uses` 这个判断
    **不能先 SELECT 再 UPDATE**——两个人同时用最后一次名额时会双双通过检查，
    结果一张一次性码开出两个号。要用带条件的原子 UPDATE + 看 rowcount：

        UPDATE invite_codes SET used_count = used_count + 1
         WHERE id = :id AND used_count < max_uses AND revoked_at IS NULL

    rowcount == 1 才算抢到名额（SQLite 与 PostgreSQL 都支持这个写法，
    与 SyncState 那把分布式锁是同一个套路）。
    """

    __tablename__ = "invite_codes"

    id: Mapped[int] = mapped_column(Integer, primary_key=True)
    # 码本身。**存归一化后的形式**（统一大写、去掉空格和连字符），
    # 校验时也要先按同样的规则归一化再查——否则用户从聊天记录里复制过来带个
    # 尾随空格，或者手打成小写，就会得到「邀请码无效」，而他看着屏幕上一模一样的
    # 字符完全无法理解。长度给到 32 是给将来换更长的码留余地，今天用不满。
    code: Mapped[str] = mapped_column(String(32), unique=True, index=True)
    # 这张码最多能用几次 / 已经用了几次。
    # max_uses = 1 是最常见的用法：一张码对一个人，谁用了一目了然。
    max_uses: Mapped[int] = mapped_column(Integer, default=1)
    used_count: Mapped[int] = mapped_column(Integer, default=0)
    # 有效期。NULL = 永不过期（**不推荐**，理由见类文档）。
    expires_at: Mapped[Optional[datetime]] = mapped_column(DateTime, nullable=True)
    # 作废时间。NULL = 没作废。
    # 为什么用时间戳而不是一个 is_revoked 布尔：出事时第一个要回答的问题是
    # 「这码是什么时候停的、停之前有没有被用过」——布尔答不了，时间戳一眼就能
    # 和下面 invite_redemptions 的 created_at 对上。
    revoked_at: Mapped[Optional[datetime]] = mapped_column(DateTime, nullable=True)
    # 备注：这张码是发给谁的（「给仓库小王」）。纯给人看的，回收时全靠它。
    note: Mapped[str] = mapped_column(String(128), default="")
    # 谁建的。人被删掉时置空而不是连带删码——码可能还在别人手里等着用，
    # 因为发码的管理员离职就让码悄悄消失，对方会遇到「昨天还好好的今天就无效了」。
    created_by_user_id: Mapped[Optional[int]] = mapped_column(
        ForeignKey("users.id", ondelete="SET NULL"), nullable=True, index=True
    )
    created_at: Mapped[datetime] = mapped_column(DateTime, default=utcnow, index=True)


class InviteRedemption(Base):
    """一次「用邀请码注册成功」的记录。**只在真的开出账号之后才写**。

    为什么必须有这张表：出问题时（比如某张码被人转发到群里刷号）要能回答
    「这张码到底被谁、什么时候、注册成了哪个账号」。光看 InviteCode.used_count
    只知道被用了几次，查不出是谁——那时候唯一能做的就是把所有新号一起停掉。

    注意与 used_count 的关系：used_count 是抢名额用的原子计数器，
    这张表是给人看的流水。两者理论上应当一一对应，但**不要拿这张表的行数
    去当作名额判据**（并发下先加计数、后写流水，中间那一瞬间对不上是正常的）。
    """

    __tablename__ = "invite_redemptions"

    id: Mapped[int] = mapped_column(Integer, primary_key=True)
    # 用的哪张码。码被删掉时置空，但下面 code_snapshot 里还留着码的字面值——
    # 用 CASCADE 的话，管理员删掉一张有问题的码，正好把「这张码开了哪些号」
    # 这份唯一的证据一起删了，而那恰恰是他删码时最需要的东西。
    invite_code_id: Mapped[Optional[int]] = mapped_column(
        ForeignKey("invite_codes.id", ondelete="SET NULL"), nullable=True, index=True
    )
    # 码的字面快照，供上面那种「码已删」的情况下仍能对上号
    code_snapshot: Mapped[str] = mapped_column(String(32), default="")
    # 注册成了哪个账号。删用户时置空，同样靠下面的快照留痕。
    user_id: Mapped[Optional[int]] = mapped_column(
        ForeignKey("users.id", ondelete="SET NULL"), nullable=True, index=True
    )
    username_snapshot: Mapped[str] = mapped_column(String(64), default="")
    # 注册请求来自哪个 IP。45 是 IPv6 字符串的最大长度（含 IPv4 映射写法）。
    # ⚠ 群晖上套了反代时，request.client.host 是反代自己的地址、全站一个值，
    # 这一列会全是同一个 IP。它只是个线索，不要拿来当判据。
    client_ip: Mapped[str] = mapped_column(String(45), default="")
    # 浏览器 UA，截断存。用来看「同一张码的几次使用是不是同一台设备」。
    user_agent: Mapped[str] = mapped_column(String(256), default="")
    created_at: Mapped[datetime] = mapped_column(DateTime, default=utcnow, index=True)


# ══════════════════════════════════════════════════════════════════════
#  手机号验证码：这张表的每一行都对应一条**真花钱**的短信
# ══════════════════════════════════════════════════════════════════════

# phone_verification_codes.scope 的取值：这条码是拿来干什么的。
# 用一列 scope 区分，而不是每种用途开一张表：三种用途的字段完全一样，
# 拆表只会让「清理过期记录」「按手机号查最近一条」这类操作要写三遍。
#
# 为什么必须区分：验证码在库里就是一串哈希，不带用途的话，
# 一条「注册用」的码可以被拿去当「换绑用」的码提交——换绑接口会认为
# 「这个人证明了自己拥有这个手机号」，而他其实只是刚给一个陌生号发了条注册短信。
# 校验时必须 phone + scope 一起匹配，少一边这道防线就没了。
PHONE_CODE_SCOPES = (
    "register",  # 手机号自助注册（本期要做的）
    "login",     # 已绑手机号的人用验证码登录（本期不做，先把取值定下来）
    "rebind",    # 换绑手机号（本期不做）
)

# phone_verification_codes.channel 的取值：这条码到底是**怎么送出去**的。
# 这一列是本期的核心安全要求之一：调试模式下短信根本没发出去，
# 而库里的记录和真发过一模一样。不记下渠道，事后没有任何办法回答
# 「那天用户说没收到短信，到底是没发还是运营商吞了」。
PHONE_CODE_CHANNELS = (
    "debug",   # 调试模式：**没有真的发短信**，码写进服务端日志 + 直接显示在界面上
    "aliyun",  # 阿里云短信服务，真的发出去了、也真的花钱了
)


class PhoneVerificationCode(Base):
    """一次「往某个手机号发验证码」的记录。**发出去之前先写这一行。**

    ⚠ **这是新表**，由 create_all 自动建出、带齐全部列和索引，对存量数据零影响
    （见 migrations.py 纪律三：能建新表就别给老表加列）。所以这里一次把三种用途
    （注册 / 登录 / 换绑）要用的列都建齐，以后开新用途是纯增量。

    ════════════════════════════════════════════════════════════════
     一、验证码**绝不明文存库**，而且光做哈希还不够
    ════════════════════════════════════════════════════════════════
    code_hash 里存的必须是**带密钥的哈希**（HMAC），不能是裸 sha256(码)。
    理由是算术性的：验证码只有 6 位数字，一共 100 万种可能，任何人拿到一份
    数据库备份，用一台笔记本几秒钟就能把全表的裸哈希反查成明文——那等于没存哈希。
    加了密钥（服务端的 SECRET_KEY，不在数据库里）之后，光有库就算不出码。

    约定的算法写在 services 那一层（本文件只定列），格式是：

        hmac_sha256(SECRET_KEY, "手机号|用途|验证码")  → 64 位十六进制

    把手机号和用途一起拌进去，是为了让一条哈希只在**它自己那一行**成立：
    否则两个人恰好拿到同一个 6 位码时，哈希一模一样，A 的码能验开 B 的记录。

    列宽给到 128 而不是刚好 64：将来换算法（比如前面加 "v2$" 之类的标记）
    不用回头给老表加列——那条路在本项目里是有类型陷阱的。

    副作用要知情：换掉 DESIGNKIT_SECRET_KEY 会让**已经发出去、还没用的码**
    全部作废。影响面只有几分钟（码的有效期就那么长），用户重发一次即可，
    比起「拿到备份就能算出所有验证码」这个代价，完全值得。

    ════════════════════════════════════════════════════════════════
     二、为什么发送前就要写这一行，而不是发成功了再写
    ════════════════════════════════════════════════════════════════
    短信调用是网络请求，可能超时、可能「上游已经发了但我们没收到回执」。
    先写行、再发送，最坏情况是库里多一条没送达的记录（用户重发即可）；
    反过来先发后写，一旦写库失败，钱花了、短信到了用户手机上，
    可系统这边查无此码，用户填了正确的验证码却被告知「验证码错误」——
    这种故障没有任何人能自查出来。

    但**必须先过限速再写行**（限速用 services/ratelimit.py，不要自己造一套）。
    这也是这张表不需要额外行数封顶的原因：能插进来的行数被三层限速卡死了
    （全站每日上限默认 200 条），不像 rate_limit_state 那张表的 key 是
    攻击者可以随便造的。

    ════════════════════════════════════════════════════════════════
     三、怎么清理过期记录（不清的话这张表只会一直长）
    ════════════════════════════════════════════════════════════════
    照抄 services/ratelimit.py 里那套已经在跑的做法，不要另发明一个定时任务：

      * **什么时候清**：每次成功插入一行之后顺手清一次，进程内 60 秒节流
        （ratelimit._maybe_cleanup 就是这么做的）。平时零额外开销，
        而只有「有人在发码」的时候表才会变大，触发时机正好对得上。
      * **清什么**：`expires_at < now - 24 小时` 的行，一次最多删 1000 条。
        已经用掉的行也一样按这条规则走（它们的 expires_at 同样会过期），
        不用额外判断 consumed_at。
      * **为什么多留 24 小时**：出事时第一个要回答的问题是「这个号今晚
        到底被发了几条」，全删干净就答不了了；另外群晖上 NTP 校时把系统时间
        往回拨过，留一天的余量能扛住时钟回拨（rate_limit_state 那边留的是
        「窗口 × 2」，同一个理由）。
      * **为什么分批删**：PostgreSQL 上一条大批量 DELETE 会把表锁住一会儿，
        正好卡住同时在发码的人。

    ════════════════════════════════════════════════════════════════
     四、手机号是个人信息
    ════════════════════════════════════════════════════════════════
    库里必须存完整号码（不然没法按号查、没法比对），但**任何日志、任何返回给
    前端的报错里都只许出现脱敏形式**（前 3 后 4，如 138****8888）。
    这张表和 auth_identities 一起，就是全系统仅有的两处存完整手机号的地方。
    """

    __tablename__ = "phone_verification_codes"

    id: Mapped[int] = mapped_column(Integer, primary_key=True)
    # 手机号，**存归一化后的 11 位纯数字**（去空格、去 +86、去连字符）。
    # 归一化由写入方负责，口径必须和 auth_identities.identifier 完全一致——
    # 两边不一致的表现是：验证码验过了，注册时却说这个号已被占用（或者反过来），
    # 而两条记录看起来一模一样。列宽 20 是给「万一以后要存国际号码」留的余量。
    phone: Mapped[str] = mapped_column(String(20))
    # 用途，取值见上面的 PHONE_CODE_SCOPES。校验时必须和 phone 一起匹配。
    scope: Mapped[str] = mapped_column(String(16))
    # 验证码的带密钥哈希，格式见类文档第一节。**任何时候都不许把明文写进来。**
    code_hash: Mapped[str] = mapped_column(String(128))
    # 什么时候过期。过期判断一律用「expires_at > now」这一个条件，
    # 不要再去算「created_at + 有效期」——有效期是个可以在界面上改的设置项，
    # 改小之后按 created_at 算，已经发出去的码会当场全部失效（用户莫名其妙）。
    # 把到期时刻在发码那一刻就钉死，改设置只影响以后发的码。
    expires_at: Mapped[datetime] = mapped_column(DateTime)
    # 已经拿这条码试了几次。超过上限就作废，防的是「有效期内慢慢猜 6 位数」：
    # 5 分钟内不限次数地猜，100 万种可能其实是猜得完的。
    # 计数必须用「带条件的 UPDATE + 看 rowcount」加，不能先读后写（并发下会丢次数）。
    attempts: Mapped[int] = mapped_column(Integer, default=0)
    # 什么时候被用掉的。NULL = 还没用过。
    # 为什么用时间戳而不是 is_used 布尔（和 InviteCode.revoked_at 同一个理由）：
    # 出事时要回答的是「这条码是几点被用掉的、和那个账号的创建时间对不对得上」，
    # 布尔答不了。另外「一条码只能用一次」必须靠
    #     UPDATE ... SET consumed_at = now WHERE id = :id AND consumed_at IS NULL
    # 这条带条件的 UPDATE + rowcount == 1 来保证，不能先查后写——
    # 两个请求同时提交同一个码时，先查后写会双双通过，一个码开出两个号。
    consumed_at: Mapped[Optional[datetime]] = mapped_column(DateTime, nullable=True)
    # 这条码是怎么送出去的，取值见 PHONE_CODE_CHANNELS。
    # debug = 根本没发短信。这一列存在的意义就是让「以为发了其实没发」这件事
    # 在事后查得出来（界面上也必须显著标明，见 config.py 的 sms_provider）。
    channel: Mapped[str] = mapped_column(String(16), default="debug")
    # 上游返回的回执号（阿里云的 BizId）。用户报「没收到」时，拿它去阿里云控制台
    # 查这条短信的送达状态，是唯一能分清「我们没发」「运营商没送到」的凭据。
    # 调试模式下恒为 NULL。
    provider_msg_id: Mapped[Optional[str]] = mapped_column(String(64), nullable=True)
    # 换绑 / 验证码登录时，是**谁**在请求（注册时为 NULL，那时人还不存在）。
    # 必须存下来：否则一个人可以在自己的账号下申请一条 rebind 码，
    # 再拿去改别人的手机号，而服务端完全分辨不出这条码是谁申请的。
    # 删用户时置空而不是连带删（SET NULL）：这些行本来就活不过一天，
    # 而删用户时若连带删，正好把「他做过什么」这份线索一起删掉。
    user_id: Mapped[Optional[int]] = mapped_column(
        ForeignKey("users.id", ondelete="SET NULL"), nullable=True
    )
    # 是谁请求发的这条短信。45 是 IPv6 字符串的最大长度（含 IPv4 映射写法）。
    # ⚠ 套了反向代理时这里全是反代自己的地址，取真实 IP 要靠 deps.client_ip +
    # trusted_proxy_hops 配对（见 config.py 里那一项）。它是线索，不是判据。
    client_ip: Mapped[str] = mapped_column(String(45), default="")
    # 发出去的时间。同时也是「重发冷却」的判据来源（冷却本身由 ratelimit 算，
    # 这一列是给人查流水用的）。
    created_at: Mapped[datetime] = mapped_column(DateTime, default=utcnow)

    __table_args__ = (
        # ── 查询用：按手机号 + 用途取「最新的那一条」──
        # 真实语句长这样：
        #     WHERE phone = ? AND scope = ? AND consumed_at IS NULL AND expires_at > now
        #     ORDER BY id DESC LIMIT 1
        # 索引的列序 (phone, scope, id) 就是按这条语句排的：前两列是等值匹配，
        # 最后一列让数据库能直接反着扫出最新一行，不用把这个号的所有历史码
        # 都取出来再排序。consumed_at / expires_at 不进索引——它们过滤掉的行
        # 本来就没几条（一个号一天最多十条），进了索引反而让索引变宽、写入变慢。
        Index(
            "ix_phone_verification_codes_lookup",
            "phone", "scope", "id",
        ),
        # ── 清理用：按到期时间删旧行（见类文档第三节）──
        # 没有这条索引，每清一次都要全表扫；而清理是在「用户正等着收短信」
        # 的那条路径上顺手做的，扫全表会让发码变慢。
        Index(
            "ix_phone_verification_codes_expires_at",
            "expires_at",
        ),
        # ── 这里**故意没有任何唯一约束**，这是想清楚之后的决定 ──
        # 看起来最像的一条是「同一个手机号 + 同一个用途，同时只能有一条没用掉的码」。
        # 不建它的三个理由：
        #  ① 那是个**部分**唯一索引（要带 WHERE consumed_at IS NULL AND
        #     expires_at > now），SQLite 和 PostgreSQL 的写法不一样，而这个项目
        #     两边都要能跑；而且「未过期」是个随时间变化的条件，索引里根本表达不了。
        #  ② 它会让「重发」变成一个必须先作废旧码的原子操作，两个请求同时点重发
        #     就会有一个撞唯一约束、返回 500——而用户看到的只是「重新发送」按钮报错。
        #  ③ 真正要保证的「一条码只能用一次」靠的是 consumed_at 那条带条件的
        #     UPDATE（见上面的列注释），不是唯一约束。唯一约束在这里既挡不住
        #     该挡的，又会挡住不该挡的。
        # 允许同一个号存在多条未过期的码，代价是「旧码在有效期内仍然能用」——
        # 校验时只认最新一条（上面那条索引就是干这个的）即可，
        # 或者重发时把旧码一并标成已用，两种都行，选哪种由服务层决定。
    )


# user_gateway_accounts.state 的取值。用字符串而不是布尔，是因为「开通网关账号」
# 天然是个多步骤过程（建号 → 发 Key → 可用），中途会卡在哪一步必须能看出来；
# 用 True/False 只会逼着后面再加第二个、第三个布尔字段，然后互相矛盾。
GATEWAY_ACCOUNT_STATES = (
    "manual",        # 管理员手工把 Key 填进来的，没走自动开通流程
    "pending",       # 已提交开通请求，还没拿到结果
    "user_created",  # 网关那边账号建好了，但 Key 还没发下来
    "key_issued",    # Key 拿到了，还没验证过能不能真的生图
    "active",        # 可以正常用
    "failed",        # 开通失败，失败原因在 last_error 里
    "disabled",      # 管理员主动停用
)


class UserGatewayAccount(Base):
    """每个用户在生图网关（自建 Sub2API）上的账号与专属 Key。

    为什么要单开一张表，而不是给 app_settings 加个用户维度：app_settings 的主键
    就是设置名本身（见本文件末尾的 AppSetting），一个 openai_api_key 全库只有一行，
    「每人一把 Key」在那张表里物理上存不下。而 base_url / image_model / text_model
    这些是全体共用同一个网关的，只有 Key 因人而异——所以拆出来最干净。

    这张表是新表，由 create_all 自动建出、带齐全部列，对存量数据零影响。
    所以这里**故意一次把以后要用的列也建齐**（remote_* 是代客开通账号用的，
    balance_* 是余额快照用的，眼下都还没人读）：等以后再加就得走 ALTER TABLE
    那条路，而那条路在本项目里是有类型陷阱的（详见 migrations.py 文件头）。
    新表随时能建，老表加列才有风险，这是本项目的一条硬规矩。

    安全约定：api_key_enc / remote_password_enc 里存的**永远是密文**
    （由 services/secrets_box.py 加解密），任何时候都不许把明文写进这两列。
    api_key_tail 只留 Key 的末尾几位，用途仅限于后端排错时对上是哪一把，
    **任何接口都不要把它返回给前端**——多个后台把同一把 Key 的后几位摆在一起，
    就足够把人、Key、账单三边对上号了。
    """

    __tablename__ = "user_gateway_accounts"

    id: Mapped[int] = mapped_column(Integer, primary_key=True)
    # 一个人只能有一个网关账号：唯一索引建在这张全新的空表上，不可能撞上存量重复值
    user_id: Mapped[int] = mapped_column(
        ForeignKey("users.id", ondelete="CASCADE"), unique=True, index=True
    )
    provider: Mapped[str] = mapped_column(String(16), default="sub2api")
    # 取值见上面的 GATEWAY_ACCOUNT_STATES
    state: Mapped[str] = mapped_column(String(16), default="manual")
    api_key_enc: Mapped[Optional[str]] = mapped_column(Text, nullable=True)  # 密文，绝不明文
    api_key_tail: Mapped[str] = mapped_column(String(8), default="")  # 仅后端排错用
    remote_user_id: Mapped[Optional[str]] = mapped_column(String(64), nullable=True)
    remote_email: Mapped[Optional[str]] = mapped_column(String(128), nullable=True)
    remote_password_enc: Mapped[Optional[str]] = mapped_column(Text, nullable=True)  # 密文
    attempts: Mapped[int] = mapped_column(Integer, default=0)  # 自动开通试了几次
    last_error: Mapped[str] = mapped_column(Text, default="")  # 最后一次失败的原因（人话）
    balance_usd: Mapped[Optional[float]] = mapped_column(Float, nullable=True)  # 余额快照
    balance_synced_at: Mapped[Optional[datetime]] = mapped_column(DateTime, nullable=True)
    created_at: Mapped[datetime] = mapped_column(DateTime, default=utcnow)
    updated_at: Mapped[datetime] = mapped_column(DateTime, default=utcnow, onupdate=utcnow)


class ApiKey(Base):
    """对外 API（ERP 等平台对接）使用的密钥。"""

    __tablename__ = "api_keys"

    id: Mapped[int] = mapped_column(Integer, primary_key=True)
    # 这把 Key 属于谁。没有它，ERP 建的任务 job.user_id 恒为 NULL，
    # 「按人取网关 Key、按人算账」整条链路在数据模型层就断了。
    # 可空是为了兼容存量 Key（升级后由启动期回填补上归属）；删用户时置空而不是
    # 连带删 Key —— ERP 那头还在用同一把 Key 调接口，突然 401 没人查得出原因。
    # 注意：老库是走 ALTER TABLE 补的这一列，只有 INTEGER、**没有外键约束**
    #（见 migrations.py），所以归属校验一律在应用层做，不要指望数据库兜底。
    user_id: Mapped[Optional[int]] = mapped_column(
        ForeignKey("users.id", ondelete="SET NULL"), nullable=True, index=True
    )
    name: Mapped[str] = mapped_column(String(64))  # 例如「XX ERP 生产环境」
    key_prefix: Mapped[str] = mapped_column(String(16))  # 展示用前缀 dk_xxxx
    key_hash: Mapped[str] = mapped_column(String(64), unique=True, index=True)  # sha256
    webhook_secret: Mapped[str] = mapped_column(String(64))  # 回调签名密钥
    is_active: Mapped[bool] = mapped_column(Boolean, default=True)
    monthly_quota: Mapped[Optional[int]] = mapped_column(Integer, nullable=True)  # None=不限
    used_total: Mapped[int] = mapped_column(Integer, default=0)
    used_month: Mapped[int] = mapped_column(Integer, default=0)  # 当月已用次数
    used_month_key: Mapped[str] = mapped_column(String(8), default="")  # 如 202608
    last_used_at: Mapped[Optional[datetime]] = mapped_column(DateTime, nullable=True)
    created_at: Mapped[datetime] = mapped_column(DateTime, default=utcnow)


class PromptCategory(Base):
    __tablename__ = "prompt_categories"

    id: Mapped[int] = mapped_column(Integer, primary_key=True)
    name: Mapped[str] = mapped_column(String(64), unique=True)
    sort: Mapped[int] = mapped_column(Integer, default=0)


class PromptTemplate(Base):
    """提示词模板。prompt_template 中可用 {变量名} 占位，由 variables 定义。

    source 说明：
    - user     用户自己创建/采用的正式模板（生成页可见，取决于 is_enabled）
    - youmind  从 YouMind 开源库同步来的灵感库条目（默认 is_enabled=False，
               只出现在「灵感库」页；采用时会复制成一条 source=user 的新模板）
    """

    __tablename__ = "prompt_templates"

    id: Mapped[int] = mapped_column(Integer, primary_key=True)
    name: Mapped[str] = mapped_column(String(128))
    source: Mapped[str] = mapped_column(String(16), default="user", index=True)
    # 外部来源标识，如 youmind:29918；同步时按它幂等更新，采用的副本也带上它用于标记「已采用」
    source_ref: Mapped[Optional[str]] = mapped_column(String(64), nullable=True, index=True)
    # 上游的全部分类标签（JSON 数组）。一条提示词常同时属于多个分类，
    # 只记一个会让「电商主图」这类分类看起来几乎是空的（实测 416 条只剩 18 条）
    source_slugs: Mapped[Optional[str]] = mapped_column(Text, nullable=True)
    # 模板属于谁：NULL = 平台公共库（存量 14573 条全是 NULL，升级后行为完全不变）。
    # 眼下只写不读、还没有任何地方按它过滤，先加列是因为「加列」这件事在老库上
    # 有成本、在新库上没有，早加早省事；等到真要分库时才加，就得赶在功能上线前
    # 抢着做一次有风险的迁移。
    # 故意**不加外键约束**：模板是内容资产，删一个用户不该把他建过的模板一起带走，
    # 也不该因为外键的存在让「删用户」变成一个要小心翼翼评估级联的动作。
    # 归属校验在应用层做（services/jobs.py 取模板那一处）。
    owner_user_id: Mapped[Optional[int]] = mapped_column(Integer, nullable=True)
    category_id: Mapped[Optional[int]] = mapped_column(
        ForeignKey("prompt_categories.id", ondelete="SET NULL"), nullable=True
    )
    description: Mapped[str] = mapped_column(Text, default="")
    prompt_template: Mapped[str] = mapped_column(Text)
    # [{"name":"scene","label":"场景","type":"text|select","options":[...],"default":"","required":true}]
    variables: Mapped[list] = mapped_column(JSON, default=list)
    # {"size":"1024x1024","n":1,"quality":"high"}
    default_params: Mapped[dict] = mapped_column(JSON, default=dict)
    requires_input_image: Mapped[bool] = mapped_column(Boolean, default=True)
    # 加索引是给「按图片路径反查这张图属于谁」用的：/files 鉴权路由每放行一张缩略图
    # 都要来查一次，而这张表有 14573 行，不加索引等于每张图都全表扫一遍，
    # 历史页一屏几十张缩略图会卡到用户以为系统坏了
    thumbnail_path: Mapped[Optional[str]] = mapped_column(
        String(256), nullable=True, index=True
    )
    is_enabled: Mapped[bool] = mapped_column(Boolean, default=True)
    sort: Mapped[int] = mapped_column(Integer, default=0)
    created_at: Mapped[datetime] = mapped_column(DateTime, default=utcnow)
    updated_at: Mapped[datetime] = mapped_column(DateTime, default=utcnow, onupdate=utcnow)

    category = relationship("PromptCategory", lazy="joined")


class Upload(Base):
    __tablename__ = "uploads"

    id: Mapped[int] = mapped_column(Integer, primary_key=True)
    user_id: Mapped[Optional[int]] = mapped_column(
        ForeignKey("users.id", ondelete="SET NULL"), nullable=True
    )
    api_key_id: Mapped[Optional[int]] = mapped_column(
        ForeignKey("api_keys.id", ondelete="SET NULL"), nullable=True
    )
    original_name: Mapped[str] = mapped_column(String(256), default="")
    # 相对 data 目录，如 uploads/202608/xx.png。
    # 加索引同样是给 /files 鉴权路由反查归属用的：这张表有十几万行历史记录，
    # 每次取图都全表扫的话，页面会慢到没法用（详见 prompt_templates.thumbnail_path）
    path: Mapped[str] = mapped_column(String(256), index=True)
    width: Mapped[int] = mapped_column(Integer, default=0)
    height: Mapped[int] = mapped_column(Integer, default=0)
    size_bytes: Mapped[int] = mapped_column(Integer, default=0)
    created_at: Mapped[datetime] = mapped_column(DateTime, default=utcnow)


JOB_STATUSES = ("pending", "processing", "succeeded", "failed")


class GenerationJob(Base):
    __tablename__ = "generation_jobs"

    id: Mapped[str] = mapped_column(String(32), primary_key=True, default=_uuid)
    source: Mapped[str] = mapped_column(String(8), default="web")  # web / api
    user_id: Mapped[Optional[int]] = mapped_column(
        ForeignKey("users.id", ondelete="SET NULL"), nullable=True
    )
    api_key_id: Mapped[Optional[int]] = mapped_column(
        ForeignKey("api_keys.id", ondelete="SET NULL"), nullable=True
    )
    template_id: Mapped[Optional[int]] = mapped_column(Integer, nullable=True)
    template_name: Mapped[str] = mapped_column(String(128), default="")  # 快照，模板删了也能看
    prompt_final: Mapped[str] = mapped_column(Text)  # 模板渲染 + 变量 + 补充要求（配置产物）
    # 实际发给生图模型的提示词。开启 AI 合成时是「看图重写」后的版本，否则同 prompt_final
    prompt_sent: Mapped[Optional[str]] = mapped_column(Text, nullable=True)
    params: Mapped[dict] = mapped_column(JSON, default=dict)  # {"size","n","quality"}
    input_paths: Mapped[list] = mapped_column(JSON, default=list)  # 商品图相对路径列表
    status: Mapped[str] = mapped_column(String(16), default="pending", index=True)
    error: Mapped[str] = mapped_column(Text, default="")
    attempts: Mapped[int] = mapped_column(Integer, default=0)
    next_attempt_at: Mapped[Optional[datetime]] = mapped_column(DateTime, nullable=True)
    callback_url: Mapped[Optional[str]] = mapped_column(String(512), nullable=True)
    external_ref: Mapped[Optional[str]] = mapped_column(String(128), nullable=True)  # ERP 单号等
    webhook_status: Mapped[str] = mapped_column(String(64), default="")
    created_at: Mapped[datetime] = mapped_column(DateTime, default=utcnow, index=True)
    started_at: Mapped[Optional[datetime]] = mapped_column(DateTime, nullable=True)
    finished_at: Mapped[Optional[datetime]] = mapped_column(DateTime, nullable=True)

    # order_by 是对外契约的一部分，不要删：
    # docs/erp-api.md 承诺「直取图端点的 index 与查询任务返回的 images 数组下标
    # 一一对应」。查询接口走的是这里的 job.images，直取图端点走的是
    # routers/v1.py 里显式的 order_by(GeneratedImage.id.asc())——两边必须是同一个
    # 顺序。不写 order_by 时顺序由数据库决定（SQLite 和 PostgreSQL 不保证一致），
    # 对接方按 index=1 取回来的就可能不是它在查询结果里看到的第 2 张图，
    # **而且两边都不会报错**，出了问题也查不到这里。
    # id 升序 = 入库顺序 = 生成时的第几张（storage 存盘用的也是同一个序号）。
    images = relationship(
        "GeneratedImage",
        back_populates="job",
        cascade="all, delete-orphan",
        lazy="selectin",
        order_by="GeneratedImage.id",
    )


class GeneratedImage(Base):
    __tablename__ = "generated_images"

    id: Mapped[int] = mapped_column(Integer, primary_key=True)
    job_id: Mapped[str] = mapped_column(
        ForeignKey("generation_jobs.id", ondelete="CASCADE"), index=True
    )
    path: Mapped[str] = mapped_column(String(256))
    thumb_path: Mapped[Optional[str]] = mapped_column(String(256), nullable=True)
    width: Mapped[int] = mapped_column(Integer, default=0)
    height: Mapped[int] = mapped_column(Integer, default=0)
    format: Mapped[str] = mapped_column(String(8), default="png")
    created_at: Mapped[datetime] = mapped_column(DateTime, default=utcnow)

    job = relationship("GenerationJob", back_populates="images")


class AppSetting(Base):
    __tablename__ = "app_settings"

    key: Mapped[str] = mapped_column(String(64), primary_key=True)
    value: Mapped[dict] = mapped_column(JSON)  # {"v": ...} 包一层，兼容任意类型


class SyncState(Base):
    """后台定时任务的执行状态与分布式锁。

    锁用「带条件的 UPDATE + rowcount」实现，SQLite 和 PostgreSQL 都适用：
    多进程部署（gunicorn/多容器）时只有抢到锁的那个进程会真正执行同步，
    lock_until 到期自动释放，避免进程崩溃后锁永久悬挂。
    """

    __tablename__ = "sync_state"

    name: Mapped[str] = mapped_column(String(64), primary_key=True)  # 如 inspiration
    lock_until: Mapped[Optional[datetime]] = mapped_column(DateTime, nullable=True)
    # 持锁者令牌：释放时校验归属，避免清掉别人的锁
    lock_owner: Mapped[Optional[str]] = mapped_column(String(64), nullable=True)
    last_started_at: Mapped[Optional[datetime]] = mapped_column(DateTime, nullable=True)
    last_success_at: Mapped[Optional[datetime]] = mapped_column(DateTime, nullable=True)
    last_status: Mapped[str] = mapped_column(String(16), default="idle")  # idle/running/success/failed
    last_message: Mapped[str] = mapped_column(Text, default="")
    consecutive_failures: Mapped[int] = mapped_column(Integer, default=0)


class RateLimitState(Base):
    """限速计数器（登录失败次数、以后的短信验证码/注册次数）。

    为什么要落到数据库、而不是继续用进程内的字典（原来 routers/auth.py 就是那样）：
      1. 多进程 / 多容器部署时每个进程各存一份，配的「10 次」实际是「10 × 进程数」；
      2. 服务一重启，攻击者攒下的失败记录全部清零，重启就等于解封；
      3. 进程内的字典**只在登录成功时才清**，失败记录会一直留着，而 key 由
         「IP + 用户名」拼成、两段都是攻击者随便写的，等于给了一条无上限的内存增长路径。
    落库之后三条一起解决：全进程共用同一份计数、重启不丢、过期行由 services/ratelimit.py
    定期删掉（还带每个 scope 的行数上限）。

    ⚠️ **这是新表**，由 create_all 自动建出、带齐全部列，对存量数据零影响
    （见 migrations.py 纪律三：能建新表就别给老表加列）。

    主键是 (scope, key) 两列：
      - scope 是「哪一类限速」，例如 login；以后加 sms_code / register 只是多一个取值，
        不用改表结构，也不会和登录那一路互相干扰。
      - key 是「限谁」，登录这一路是 "IP|用户名"。它的内容由外部输入拼出来，
        所以 services/ratelimit.py 会先截断到 128 字符再写进来——列宽在 PostgreSQL 上
        是硬约束，超长会直接报错（SQLite 反而会照单全收），两边行为不一致最难查。

    计数用「带条件的 UPDATE + 看 rowcount」保证原子性，写法照抄上面 SyncState 的锁，
    SQLite 与 PostgreSQL 都适用；不引入 Redis（用户是群晖 NAS 单机部署，
    多一个中间件就多一份她要维护、要备份、会挂掉的东西）。
    """

    __tablename__ = "rate_limit_state"

    scope: Mapped[str] = mapped_column(String(32), primary_key=True)
    key: Mapped[str] = mapped_column(String(128), primary_key=True)
    # 当前计数窗口的起点。窗口过期后整行会被重置成「新窗口的第 1 次」，
    # 而不是删掉再插入——重置是一条带条件的 UPDATE，并发下天然只有一个人做得成。
    window_start: Mapped[datetime] = mapped_column(DateTime, default=utcnow)
    # 本窗口内已经失败了几次。列名不叫 count：count 在 PostgreSQL 里是聚合函数名，
    # 拿它当列名虽然合法，但拼出来的 SQL 读起来极易误会。
    attempts: Mapped[int] = mapped_column(Integer, default=0)
    # 封锁到什么时候（None = 没被封）。计数超过阈值时才写上，
    # 到点自动失效，不需要任何人去手工解封。
    blocked_until: Mapped[Optional[datetime]] = mapped_column(DateTime, nullable=True)
    # 最后一次动这行的时间。带索引是给定期清理用的：清理就是按它排序删旧行，
    # 没有索引的话，行数一多每次清理都要全表扫。
    updated_at: Mapped[datetime] = mapped_column(DateTime, default=utcnow, index=True)
