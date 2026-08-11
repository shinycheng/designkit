"""一台假的 Sub2API，用来验证开通状态机的可重入性。

**不连任何真实网关**——用户那台 192.168.31.235 全程不碰。
这里只按已核实的接口契约模拟，包括各种失败形态：
409 / 429 / requires_2fa / 403 未绑分组 / 423 合规未确认 / 500。

模拟里刻意保留的三个坑（用来证明状态机没踩）：
1. GET /api/v1/admin/users?search= 是**模糊匹配**，而且故意把近似项排在第一条
   （搜 dk7@ 会先返回 dk17@），直接取第一条就会把 Key 建到别人账号上。
2. GET /api/v1/api-keys 这条路径**永远 404**——照 Sub2API 源码注释写就是这个下场。
3. 合规 423 的响应体里 code 是**字符串**，不是整数。
"""
import json
import threading
from collections import Counter
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from urllib.parse import parse_qs, urlparse

ADMIN_KEY = "fake-admin-key-for-tests-only"


class FakeState(object):
    def __init__(self):
        self.users = []          # {id,email,username,password,concurrency,rpm_limit,allowed_groups,deleted}
        self.keys = []           # {id,key,name,group_id,owner}
        self.tokens = {}         # jwt -> email
        self.calls = Counter()   # (METHOD, path) -> 次数
        self.next_user_id = 1000
        self.next_key_id = 500
        # ── 故障注入 ──
        self.fail_admin_create_after_effect = 0  # 建完号再返回 500（模拟「远端做了、本地没记上」）
        self.fail_key_create_after_effect = 0    # 建完 Key 再返回 500
        self.fail_usage = 0                      # /v1/usage 连续返回 N 次 500
        self.login_requires_2fa = False
        self.login_status = 0                    # 非 0 时 login 直接返回这个状态码
        self.login_body = None
        self.admin_create_status = 0             # 非 0 时建号直接返回这个状态码
        self.admin_create_body = None
        self.key_create_status = 0
        self.key_create_body = None
        self.usage_status = 0
        self.usage_body = None
        self.squatted = set()                    # 被「别的租户」抢注的 custom_key
        self.hide_from_search = set()             # 反查时假装看不见的邮箱（模拟软删）
        # ── 请求体留痕 ──
        # calls 只数「打了哪条路由几次」，答不了「这几次分别提交了什么」。
        # 而「撞上 429 之后有没有换个后缀再撞一次」这条纪律，恰恰只能靠比对
        # 提交过的 custom_key 有几个**不同值**来判定（次数相同、值不同 = 换后缀了）。
        # 一律记下来，包括被故障注入拦掉、根本没建成的那几次。
        self.create_user_bodies = []
        self.create_key_bodies = []

    # —— 查询辅助 ——
    def find_user(self, email):
        for u in self.users:
            if u["email"] == email and not u["deleted"]:
                return u
        return None

    def keys_of(self, email):
        return [k for k in self.keys if k["owner"] == email]

    def count(self, method, path):
        return self.calls[(method, path)]


def _envelope(data, code=0, message="ok"):
    return {"code": code, "message": message, "data": data}


class _Handler(BaseHTTPRequestHandler):
    state = None  # 由 start() 注入

    def log_message(self, fmt, *args):  # 静音，免得刷屏
        pass

    # ── 工具 ──
    def _json(self, status, body):
        raw = json.dumps(body).encode("utf-8")
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(raw)))
        self.end_headers()
        self.wfile.write(raw)

    def _body(self):
        length = int(self.headers.get("Content-Length") or 0)
        if not length:
            return {}
        try:
            return json.loads(self.rfile.read(length).decode("utf-8"))
        except Exception:
            return {}

    def _admin_ok(self):
        return self.headers.get("x-api-key") == ADMIN_KEY

    def _bearer(self):
        raw = self.headers.get("Authorization") or ""
        return raw[7:] if raw.startswith("Bearer ") else ""

    # ── 路由 ──
    def do_GET(self):
        st = self.state
        parsed = urlparse(self.path)
        path, query = parsed.path, parse_qs(parsed.query)
        st.calls[("GET", path)] += 1

        if path == "/api/v1/api-keys":
            # 坑 2：源码注释里写的这条路径根本不存在
            return self._json(404, {"code": "NOT_FOUND", "message": "route not found"})

        if path == "/v1/usage":
            if st.fail_usage > 0:
                st.fail_usage -= 1
                return self._json(500, {"error": "internal"})
            if st.usage_status:
                return self._json(st.usage_status, st.usage_body or
                                  {"error": "API Key is not assigned to any group"})
            key = self._bearer()
            if not any(k["key"] == key for k in st.keys):
                return self._json(401, {"error": "invalid api key"})
            # 裸 JSON，不是信封
            return self._json(200, {"balance_usd": 12.5, "used": 0.0})

        if path == "/api/v1/keys":
            token = self._bearer()
            email = st.tokens.get(token)
            if not email:
                return self._json(401, _envelope(None, "UNAUTHORIZED", "token invalid"))
            items = [
                {"id": k["id"], "key": k["key"], "name": k["name"], "group_id": k["group_id"]}
                for k in st.keys_of(email)
            ]
            return self._json(200, _envelope({"items": items, "total": len(items)}))

        if path.startswith("/api/v1/admin/users"):
            if not self._admin_ok():
                return self._json(401, _envelope(None, "INVALID_ADMIN_KEY", "bad admin key"))
            rest = path[len("/api/v1/admin/users"):].strip("/")
            if rest:  # GET /api/v1/admin/users/:id
                for u in st.users:
                    if str(u["id"]) == rest:
                        return self._json(200, _envelope(_public_user(u)))
                return self._json(404, _envelope(None, "USER_NOT_FOUND", "no such user"))
            search = (query.get("search") or [""])[0].strip().lower()
            # 坑 1：search 到底怎么匹配没追到 SQL（设计文档「仍未查明」第 5 条），
            # 所以按**最模糊**的假设模拟：把域名去掉只拿前半截做子串匹配。
            # 于是搜 dk1@designkit.local 会同时命中 dk1@ 和 dk10@ —— 这正是
            # sub2api.py 文档里警告的那个形态，也是「直接取第一条」会翻车的地方。
            term = search.split("@")[0]
            page = int((query.get("page") or ["1"])[0])
            hits = []
            for u in st.users:
                if u["email"] in st.hide_from_search:
                    continue
                if term and term not in u["email"].lower():
                    continue
                hits.append(_public_user(u))
            # 坑 1 续：故意把「近似但不相等」的排在最前面
            hits.sort(key=lambda x: 1 if x["email"].lower() == search else 0)
            items = hits if page == 1 else []
            return self._json(200, _envelope({"items": items, "total": len(hits)}))

        if path == "/api/v1/settings/public":
            return self._json(200, _envelope({
                "backend_mode_enabled": False, "turnstile_enabled": False,
                "tencent_captcha_enabled": False, "aliyun_captcha_enabled": False,
                "totp_enabled": False, "version": "fake-1.0",
            }))
        if path == "/api/v1/admin/compliance":
            if not self._admin_ok():
                return self._json(401, _envelope(None, "INVALID_ADMIN_KEY", "bad admin key"))
            return self._json(200, _envelope({"required": False, "version": "2024-01"}))

        return self._json(404, {"code": "NOT_FOUND", "message": "route not found"})

    def do_POST(self):
        st = self.state
        path = urlparse(self.path).path
        st.calls[("POST", path)] += 1
        body = self._body()

        if path == "/api/v1/admin/users":
            st.create_user_bodies.append(body)
            if not self._admin_ok():
                return self._json(401, _envelope(None, "INVALID_ADMIN_KEY", "bad admin key"))
            if st.admin_create_status:
                # 坑 3：423 的 code 是**字符串**
                return self._json(st.admin_create_status, st.admin_create_body or
                                  _envelope(None, "ADMIN_COMPLIANCE_ACK_REQUIRED",
                                            "compliance ack required"))
            email = (body.get("email") or "").strip()
            if st.find_user(email) is not None:
                return self._json(409, _envelope(None, "EMAIL_EXISTS", "email exists"))
            st.next_user_id += 1
            user = {
                "id": st.next_user_id, "email": email,
                "username": body.get("username") or "",
                "password": body.get("password") or "",
                "concurrency": body.get("concurrency"),
                "rpm_limit": body.get("rpm_limit"),
                "allowed_groups": body.get("allowed_groups") or [],
                "deleted": False,
            }
            st.users.append(user)
            if st.fail_admin_create_after_effect > 0:
                # 「远端建好了，但响应没回到 designkit」——最经典的中断点
                st.fail_admin_create_after_effect -= 1
                return self._json(500, _envelope(None, "INTERNAL", "boom after create"))
            return self._json(200, _envelope(_public_user(user)))  # 200 不是 201

        if path == "/api/v1/auth/login":
            if st.login_status:
                return self._json(st.login_status, st.login_body or
                                  _envelope(None, "INVALID_CREDENTIALS", "bad credentials"))
            email = (body.get("email") or "").strip()
            user = st.find_user(email)
            if user is None or user["password"] != (body.get("password") or ""):
                return self._json(401, _envelope(None, "INVALID_CREDENTIALS", "bad credentials"))
            if st.login_requires_2fa:
                return self._json(200, _envelope({"requires_2fa": True}))
            token = "jwt-%s-%d" % (email, len(st.tokens) + 1)
            st.tokens[token] = email
            return self._json(200, _envelope({"access_token": token, "expires_in": 86400}))

        if path == "/api/v1/keys":
            st.create_key_bodies.append(body)
            token = self._bearer()
            email = st.tokens.get(token)
            if not email:
                return self._json(401, _envelope(None, "UNAUTHORIZED", "token invalid"))
            if st.key_create_status:
                return self._json(st.key_create_status, st.key_create_body or
                                  {"error": "rate limit exceeded"})
            custom = (body.get("custom_key") or "").strip()
            if custom in st.squatted:
                # 被别的租户抢注：409，而且这个用户的列表里**找不到**它
                return self._json(409, _envelope(None, "API_KEY_EXISTS", "key exists"))
            for k in st.keys:
                if k["key"] == custom:
                    return self._json(409, _envelope(None, "API_KEY_EXISTS", "key exists"))
            st.next_key_id += 1
            key = {"id": st.next_key_id, "key": custom, "name": body.get("name") or "",
                   "group_id": body.get("group_id"), "owner": email}
            st.keys.append(key)
            if st.fail_key_create_after_effect > 0:
                st.fail_key_create_after_effect -= 1
                return self._json(500, _envelope(None, "INTERNAL", "boom after key"))
            return self._json(200, _envelope(
                {"id": key["id"], "key": key["key"], "name": key["name"],
                 "group_id": key["group_id"]}))

        return self._json(404, {"code": "NOT_FOUND", "message": "route not found"})


def _public_user(u):
    return {
        "id": u["id"], "email": u["email"], "username": u["username"],
        "concurrency": u["concurrency"], "rpm_limit": u["rpm_limit"],
        "allowed_groups": u["allowed_groups"],
    }


def start():
    """起一台假 Sub2API，返回 (base_url, state, stop_fn)。"""
    state = FakeState()
    handler = type("_BoundHandler", (_Handler,), {"state": state})
    server = ThreadingHTTPServer(("127.0.0.1", 0), handler)
    thread = threading.Thread(target=server.serve_forever, daemon=True)
    thread.start()
    base_url = "http://127.0.0.1:%d" % server.server_address[1]

    def stop():
        server.shutdown()
        server.server_close()

    return base_url, state, stop
