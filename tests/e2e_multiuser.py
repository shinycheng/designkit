"""端到端：成员账号（role=member）这条路第一次被真实账号走一遍。

在这次改造之前，系统里**从来没有过创建成员账号的入口**，所以「只能看自己的图」
「只能改自己的任务」这些代码写是写了，却从没有被真实账号执行过。开放成员账号的
那一刻，它们第一次真正承担隔离责任——这个脚本就是盯着那一刻的。

它按真人的顺序走一遍：
  管理员建号 → 成员首次登录被强制改密 → 上传商品图 → 生成 → 取图 →
  另一个成员试着看/用他的东西（全部要落空）→ 退出登录 → 被停用。

**这个脚本会改掉 admin 的初始密码、并建出两个成员账号**，所以必须跑在
自己的一份全新数据上，不能和 tests/e2e_core.py、tests/e2e_security.py 共用。
跑法见 tests/README.md，照抄即可。

服务地址默认 http://127.0.0.1:8787，也可以用环境变量 DESIGNKIT_E2E_BASE 指到别的端口。
"""
import io
import os
import sys
import time

import httpx
from PIL import Image, ImageDraw

BASE = os.environ.get("DESIGNKIT_E2E_BASE", "http://127.0.0.1:8787").rstrip("/")
ADMIN_PASSWORD = "adminpass8888"
A_INIT, A_PASSWORD = "alice-init-8888", "alice-new-8888"
B_INIT, B_PASSWORD = "bob-init-8888", "bob-new-8888"

results = []


def check(name, ok, detail=""):
    results.append((name, ok, detail))
    print(("PASS  " if ok else "FAIL  ") + name + ("  | " + str(detail)[:160] if detail else ""))


def png(color=(200, 60, 60)):
    im = Image.new("RGB", (600, 600), (240, 240, 245))
    ImageDraw.Draw(im).ellipse([150, 150, 450, 450], fill=color)
    buf = io.BytesIO()
    im.save(buf, "PNG")
    return buf.getvalue()


def login(client, username, password):
    """登录并返回 (响应, Authorization 头)。登录成功时客户端会自动收下取图 Cookie。"""
    r = client.post(BASE + "/api/web/auth/login",
                    json={"username": username, "password": password})
    if r.status_code != 200:
        return r, None
    return r, {"Authorization": "Bearer " + r.json()["token"]}


def change_password(client, auth, old, new):
    r = client.post(BASE + "/api/web/auth/change_password", headers=auth,
                    json={"old_password": old, "new_password": new})
    if r.status_code != 200:
        return r, auth
    return r, {"Authorization": "Bearer " + r.json()["token"]}


# ========== 管理员：登录 + 强制改密 ==========
admin = httpx.Client(timeout=60)
r, admin_auth = login(admin, "admin", "admin123456")
check("管理员用初始口令登录", r.status_code == 200, r.text[:100])
r, admin_auth = change_password(admin, admin_auth, "admin123456", ADMIN_PASSWORD)
check("管理员改掉初始密码", r.status_code == 200, r.text[:100])

# ========== 管理员建两个成员账号 ==========
r = admin.post(BASE + "/api/web/users", headers=admin_auth, json={
    "username": "e2e.alice", "display_name": "成员A", "role": "member", "password": A_INIT})
check("建成员账号 A", r.status_code == 200, r.text[:150])
user_a = r.json()
check("新账号必须改初始密码", user_a.get("must_change_password") is True, user_a)

r = admin.post(BASE + "/api/web/users", headers=admin_auth, json={
    "username": "e2e.bob", "display_name": "成员B", "role": "member", "password": B_INIT})
check("建成员账号 B", r.status_code == 200, r.text[:150])
user_b = r.json()

# 成员列表是管理员天天看的页面，也是最容易「顺手多返回一个字段」的地方。
# 这几个名字一旦出现在响应里，就等于把成员的网关 Key 片段或口令挂在了网页上。
r = admin.get(BASE + "/api/web/users", headers=admin_auth)
leaks = [w for w in ("api_key_tail", "api_key_enc", "password_hash", "sk-") if w in r.text]
check("成员列表不泄露 Key 片段或口令", r.status_code == 200 and not leaks, "泄露了：%s" % leaks)

# ========== 成员 A：首次登录 → 强制改密 ==========
alice = httpx.Client(timeout=60)
r, a_auth = login(alice, "e2e.alice", A_INIT)
check("成员 A 用初始密码登录", r.status_code == 200, r.text[:100])

# 没改密之前，业务接口一律 403，且文案是给非技术用户看的那一句
r = alice.get(BASE + "/api/web/templates", headers=a_auth)
check("未改初始密码时业务接口被拦(403)", r.status_code == 403, "HTTP %d" % r.status_code)
check("拦截文案是中文人话",
      r.json().get("detail") == "请先修改初始密码，再使用其他功能", r.text[:120])

r, a_auth = change_password(alice, a_auth, A_INIT, A_PASSWORD)
check("成员 A 改密成功", r.status_code == 200, r.text[:100])

# ========== 成员 B：同样走一遍 ==========
bob = httpx.Client(timeout=60)
r, b_auth = login(bob, "e2e.bob", B_INIT)
check("成员 B 用初始密码登录", r.status_code == 200, r.text[:100])
r, b_auth = change_password(bob, b_auth, B_INIT, B_PASSWORD)
check("成员 B 改密成功", r.status_code == 200, r.text[:100])

# ========== 成员在 mock 模式下不需要配 Key 就能生图 ==========
# 这条守的是「模拟生图模式豁免」那条规则。少了它，全新部署下成员一点生成
# 就被拦成「你的账号还没有开通生图额度」，而管理员自己试永远试不出来
# （管理员会回落到全局 Key），这个坑没人复现得了。
r = alice.get(BASE + "/api/web/account", headers=a_auth)
check("成员 A 能看到自己的账户信息", r.status_code == 200, r.text[:100])
account = r.json()
check("mock 模式下成员没配 Key 也显示可以生图",
      account.get("gateway", {}).get("can_generate") is True, account.get("gateway"))
check("我的账户里不出现任何 Key 的片段",
      "api_key" not in r.text and "sk-" not in r.text, r.text[:150])

# ========== 成员 A：上传 + 生成 + 取图 ==========
r = alice.post(BASE + "/api/web/uploads", headers=a_auth,
               files={"file": ("a.png", png(), "image/png")})
check("成员 A 上传商品图", r.status_code == 200, r.text[:120])
upload_a = r.json()

r = alice.post(BASE + "/api/web/generations", headers=a_auth, json={
    "prompt": "干净的浅灰背景商品图", "upload_ids": [upload_a["id"]],
    "n": 1, "size": "1024x1024"})
check("成员 A 创建生成任务", r.status_code == 200, r.text[:150])
job_a = r.json()

for _ in range(60):
    time.sleep(1)
    job_a = alice.get(BASE + "/api/web/generations/" + job_a["job_id"], headers=a_auth).json()
    if job_a["status"] in ("succeeded", "failed"):
        break
check("成员 A 的任务生成成功（没配 Key 也不该被拦）",
      job_a["status"] == "succeeded",
      "status=%s err=%s" % (job_a["status"], str(job_a.get("error"))[:100]))
check("成员 A 拿到 1 张图", len(job_a.get("images") or []) == 1,
      "实际 %d 张" % len(job_a.get("images") or []))

image_url = (job_a.get("images") or [{}])[0].get("url", "")
thumb_url = (job_a.get("images") or [{}])[0].get("thumbnail_url", "")
check("网页端图片地址是相对路径（靠 Cookie 取图，不带签名）",
      image_url.startswith("/files/"), image_url)

r = alice.get(BASE + image_url)
check("成员 A 能打开自己的图", r.status_code == 200, "HTTP %d" % r.status_code)
r = alice.get(BASE + thumb_url)
check("成员 A 能打开自己的缩略图", r.status_code == 200, "HTTP %d" % r.status_code)

# ========== 成员 B 拿不到 A 的任何东西 ==========
r = bob.get(BASE + image_url)
check("成员 B 打不开 A 的图（404，不确认它存在）", r.status_code == 404, "HTTP %d" % r.status_code)

stranger = httpx.Client(timeout=30)
r = stranger.get(BASE + image_url)
check("没有任何凭证的人打不开图（403）", r.status_code == 403, "HTTP %d" % r.status_code)

r = bob.get(BASE + "/api/web/generations/" + job_a["job_id"], headers=b_auth)
check("成员 B 查不到 A 的任务（必须 404 而不是 403）", r.status_code == 404, "HTTP %d" % r.status_code)
check("越权与不存在的文案逐字相同", r.json().get("detail") == "任务不存在", r.text[:120])

# B 拿 A 的 upload_id 建任务：不挡的话，B 就能把 A 的商品图当输入图使用
r = bob.post(BASE + "/api/web/generations", headers=b_auth, json={
    "prompt": "借用别人的图", "upload_ids": [upload_a["id"]], "n": 1, "size": "1024x1024"})
check("成员 B 用不了 A 的上传图（404）", r.status_code == 404, "%d %s" % (r.status_code, r.text[:100]))

r = bob.get(BASE + "/api/web/generations", headers=b_auth)
ids = [x["job_id"] for x in (r.json().get("items") or [])]
check("成员 B 的历史列表里看不到 A 的任务", job_a["job_id"] not in ids, ids[:5])

# 成员不该碰得到管理员的页面
r = bob.get(BASE + "/api/web/users", headers=b_auth)
check("成员打不开「成员账号」页(403)", r.status_code == 403, "HTTP %d" % r.status_code)
r = bob.get(BASE + "/api/web/apikeys", headers=b_auth)
check("成员建不了对外 API Key(403，入口目前仍限管理员)", r.status_code == 403, "HTTP %d" % r.status_code)

# ========== 管理员：可读、可删，但不能替别人花钱 ==========
r = admin.get(BASE + "/api/web/generations/" + job_a["job_id"], headers=admin_auth)
check("管理员能查看成员的任务（排障需要）", r.status_code == 200, "HTTP %d" % r.status_code)
r = admin.get(BASE + image_url)
check("管理员能打开成员的图（排障需要）", r.status_code == 200, "HTTP %d" % r.status_code)

# 「重新生成」会立刻花掉**任务归属人**的网关额度，且发出去就收不回来，
# 所以管理员点不了。返回 404 而不是 403，与「查不到」保持同一句话。
r = admin.post(BASE + "/api/web/generations/" + job_a["job_id"] + "/retry", headers=admin_auth)
check("管理员不能替成员点重试（404，花的是成员的钱）", r.status_code == 404,
      "%d %s" % (r.status_code, r.text[:100]))
r = admin.post(BASE + "/api/web/generations/" + job_a["job_id"] + "/supplement",
               headers=admin_auth, json={"n": 1})
check("管理员不能替成员点补图（404）", r.status_code == 404, "%d %s" % (r.status_code, r.text[:100]))

# ========== 模板封面：全站共用，谁都打得开 ==========
tpls = admin.get(BASE + "/api/web/templates", headers=admin_auth).json()
check("管理员能看到模板库", isinstance(tpls, list) and len(tpls) >= 1, "共 %d 个" % len(tpls))
r = admin.post(BASE + "/api/web/templates", headers=admin_auth, json={
    "name": "e2e 封面测试模板", "prompt_template": "白底商品图，柔和阴影"})
check("管理员新建一个模板", r.status_code == 200, r.text[:120])
tpl = r.json()
r = admin.post(BASE + "/api/web/templates/%d/thumbnail" % tpl["id"], headers=admin_auth,
               files={"file": ("cover.png", png((60, 120, 200)), "image/png")})
check("管理员上传模板封面", r.status_code == 200, r.text[:120])
cover_url = r.json().get("thumbnail_url") or ""
r = bob.get(BASE + cover_url)
# 模板封面落在 uploads/ 下、却在 uploads 表里没有行。少了这条特判，
# 改完鉴权之后全站模板封面会对所有人 404，而这个现象和「图片鉴权」的关系
# 光看界面根本联想不到。
check("模板封面对别的成员也能打开", r.status_code == 200,
      "%s -> HTTP %d" % (cover_url, r.status_code))

# ========== 退出登录：只踢这一台设备 ==========
alice_phone = httpx.Client(timeout=60)   # 同一个人的另一台设备
r, phone_auth = login(alice_phone, "e2e.alice", A_PASSWORD)
check("成员 A 在另一台设备上也登录了", r.status_code == 200, r.text[:100])
check("另一台设备也能打开自己的图",
      alice_phone.get(BASE + image_url).status_code == 200)

r = alice.post(BASE + "/api/web/auth/logout")
check("退出登录接口返回成功", r.status_code == 200, r.text[:100])
r = alice.get(BASE + image_url)
check("退出后这台设备的图片链接立刻失效(403)", r.status_code == 403, "HTTP %d" % r.status_code)
# 下面两条一起才守得住「退出登录只退这一台设备」这个决定：
# 哪天有人把它改成「更彻底的退出」（token_version +1），第二条会当场变红。
r = alice_phone.get(BASE + image_url)
check("另一台设备不受影响，图片照常打开", r.status_code == 200, "HTTP %d" % r.status_code)
r = alice_phone.get(BASE + "/api/web/auth/me", headers=phone_auth)
check("另一台设备的登录令牌也照常有效", r.status_code == 200, "HTTP %d" % r.status_code)

# ========== 停用成员：登录与图片链接一起失效 ==========
r = admin.post(BASE + "/api/web/users/%d/toggle" % user_a["id"], headers=admin_auth)
check("管理员停用成员 A", r.status_code == 200 and r.json().get("is_active") is False, r.text[:120])
r = alice_phone.get(BASE + "/api/web/auth/me", headers=phone_auth)
check("被停用后手上的令牌立刻失效(401)", r.status_code == 401, "HTTP %d" % r.status_code)
r = alice_phone.get(BASE + image_url)
check("被停用后图片链接立刻失效(403)", r.status_code == 403, "HTTP %d" % r.status_code)
r, _ = login(alice_phone, "e2e.alice", A_PASSWORD)
# 密码是对的，但账号被停用了 → 403「账号已停用」。
# 刻意不和「用户名或密码错误」(401) 混成一句：被停用的同事看到「密码错误」
# 只会一遍遍重试、然后来问密码是不是被改了，而真正该做的是去找管理员。
check("被停用后无法重新登录（403 且说明原因）",
      r.status_code == 403 and r.json().get("detail") == "账号已停用",
      "%d %s" % (r.status_code, r.text[:100]))

# 停用不是删除：管理员还能把他放回来（界面上刻意没有「删除成员」）
r = admin.post(BASE + "/api/web/users/%d/toggle" % user_a["id"], headers=admin_auth)
check("停用可以撤销（没有删除成员这个操作）",
      r.status_code == 200 and r.json().get("is_active") is True, r.text[:120])
r = admin.request("DELETE", BASE + "/api/web/users/%d" % user_b["id"], headers=admin_auth)
check("确实没有「删除成员」接口(404/405)", r.status_code in (404, 405), "HTTP %d" % r.status_code)

# ========== 删除任务：不花钱，所以管理员可以帮着清理 ==========
r = admin.delete(BASE + "/api/web/generations/" + job_a["job_id"], headers=admin_auth)
check("管理员可以删除成员的任务（删除不花钱）", r.status_code == 200, r.text[:120])
r = admin.get(BASE + image_url)
check("任务删掉后图片跟着取不到了", r.status_code == 404, "HTTP %d" % r.status_code)

print()
failed = [x for x in results if not x[1]]
print("=" * 50)
print("总计 %d 项，通过 %d 项，失败 %d 项" % (len(results), len(results) - len(failed), len(failed)))
for name, _ok, detail in failed:
    print("  FAIL:", name, "|", detail)
sys.exit(1 if failed else 0)
