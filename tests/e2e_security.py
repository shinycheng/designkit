"""第二轮端到端：覆盖修复后的契约变化 + 新修复项的回归。"""
import base64
import hashlib
import hmac
import io
import json
import sys
import threading
import time
from http.server import BaseHTTPRequestHandler, HTTPServer

import httpx
from PIL import Image, ImageDraw

BASE = "http://127.0.0.1:8787"
results = []


def check(name, ok, detail=""):
    results.append((name, ok, detail))
    print(("PASS  " if ok else "FAIL  ") + name + ("  | " + str(detail)[:150] if detail else ""))


received = {}

class CB(BaseHTTPRequestHandler):
    def do_POST(self):
        n = int(self.headers.get("Content-Length", 0))
        received["body"] = self.rfile.read(n)
        received["sig"] = self.headers.get("X-DesignKit-Signature", "")
        received["ts"] = self.headers.get("X-DesignKit-Timestamp", "")
        self.send_response(200); self.end_headers(); self.wfile.write(b"ok")
    def log_message(self, *a): pass

HTTPServer(("127.0.0.1", 9998), CB).serve_forever_thread = None
srv = HTTPServer(("127.0.0.1", 9998), CB)
threading.Thread(target=srv.serve_forever, daemon=True).start()

im = Image.new("RGB", (500, 500), (240, 240, 245))
ImageDraw.Draw(im).ellipse([120, 120, 380, 380], fill=(200, 60, 60))
buf = io.BytesIO(); im.save(buf, "PNG"); product = buf.getvalue()

c = httpx.Client(timeout=60)

# ---------- 首次登录强制改密流程 ----------
r = c.post(BASE + "/api/web/auth/login", json={"username": "admin", "password": "admin123456"})
check("默认口令可登录", r.status_code == 200, r.text[:80])
data = r.json()
check("登录返回 must_change_password=true", data["user"].get("must_change_password") is True)
token0 = data["token"]
auth0 = {"Authorization": "Bearer " + token0}

# 用错误原密码改密应失败
r = c.post(BASE + "/api/web/auth/change_password", headers=auth0,
           json={"old_password": "wrong", "new_password": "newpass8888"})
check("错误原密码改密被拒", r.status_code == 400)

# 正确改密
r = c.post(BASE + "/api/web/auth/change_password", headers=auth0,
           json={"old_password": "admin123456", "new_password": "newpass8888"})
check("改密成功并换发新令牌", r.status_code == 200 and bool(r.json().get("token")), r.text[:80])
token1 = r.json()["token"]
auth = {"Authorization": "Bearer " + token1}

# 旧令牌应因 token_version 递增而失效
r = c.get(BASE + "/api/web/auth/me", headers=auth0)
check("改密后旧令牌失效(401)", r.status_code == 401, "HTTP %d" % r.status_code)

# 新令牌可用，且 must_change_password 已清除
r = c.get(BASE + "/api/web/auth/me", headers=auth)
check("新令牌可用且已清除改密标记", r.status_code == 200 and r.json().get("must_change_password") is False)

# 旧默认口令不能再登录
r = c.post(BASE + "/api/web/auth/login", json={"username": "admin", "password": "admin123456"})
check("旧默认口令已失效", r.status_code == 401)

# ---------- 网页图片走相对路径 ----------
r = c.post(BASE + "/api/web/uploads", headers=auth,
           files={"file": ("p.png", product, "image/png")})
up = r.json()
check("上传成功", r.status_code == 200, r.text[:80])
check("网页端图片URL为相对路径", up["url"].startswith("/files/"), up["url"])
r = c.get(BASE + up["url"])
check("相对路径图片可访问", r.status_code == 200)

# ---------- size 校验 ----------
tpls = c.get(BASE + "/api/web/templates", headers=auth).json()
free_ok = c.post(BASE + "/api/web/generations", headers=auth, json={
    "prompt": "test white bg", "upload_ids": [up["id"]], "size": "1024x1024"})
check("合法尺寸创建成功", free_ok.status_code == 200)
job = free_ok.json()
r = c.post(BASE + "/api/web/generations", headers=auth, json={
    "prompt": "test", "upload_ids": [up["id"]], "size": "999x999"})
check("非法尺寸同步422", r.status_code == 422, r.text[:80])

# 等生成完成
for _ in range(60):
    time.sleep(1)
    job = c.get(BASE + "/api/web/generations/" + job["job_id"], headers=auth).json()
    if job["status"] in ("succeeded", "failed"): break
check("mock 生成完成", job["status"] == "succeeded", job.get("error", "")[:60])

# ---------- 删除确认（后端语义）+ 空页不再卡死靠前端，这里测删除本身 ----------
r = c.delete(BASE + "/api/web/generations/" + job["job_id"], headers=auth)
check("删除任务成功", r.status_code == 200)

# ---------- 对外 API：SSRF / base64 DoS / 超限 / data前缀 / 配额原子 ----------
key = c.post(BASE + "/api/web/apikeys", headers=auth, json={"name": "R2", "monthly_quota": 3}).json()["api_key"]
kh = {"X-API-Key": key}

# 云元数据地址始终被拦截（即使内部模式）
r = c.post(BASE + "/api/v1/generations", headers=kh, json={"prompt": "t", "image_urls": ["http://169.254.169.254/latest/meta-data/"]})
check("云元数据地址始终拦截", r.status_code == 422 and "URL 不可用" in r.text, "%d %s" % (r.status_code, r.text[:80]))
# 内网地址在内部模式下被放行（会因该地址无图而失败，但不是 SSRF 拒绝）——用一个不存在的私网地址，应是下载失败而非"不可用"
r = c.post(BASE + "/api/v1/generations", headers=kh, json={"prompt": "t", "image_urls": ["http://10.255.255.1/nope.png"]})
check("内部模式放行内网地址(仅下载失败)", r.status_code == 422 and "下载图片失败" in r.text, "%d %s" % (r.status_code, r.text[:80]))

# data: 前缀无逗号 → 422 而不是 500
r = c.post(BASE + "/api/v1/generations", headers=kh, json={"prompt": "t", "images_base64": ["data:image/png;base64"]})
check("坏base64前缀返回422非500", r.status_code == 422, "%d %s" % (r.status_code, r.text[:60]))

# 超过4张 → 422
r = c.post(BASE + "/api/v1/generations", headers=kh,
           json={"prompt": "t", "images_base64": [base64.b64encode(product).decode()] * 5})
check("超过4张图422", r.status_code == 422, r.text[:60])

# 非法 size 不落盘孤儿：带图 + 非法尺寸 → 422，且不占配额
before = c.get(BASE + "/api/web/apikeys", headers=auth).json()
used_before = next(k for k in before if k["name"] == "R2")["used_total"]
r = c.post(BASE + "/api/v1/generations", headers=kh,
           json={"prompt": "t", "images_base64": [base64.b64encode(product).decode()], "size": "1x1"})
check("非法尺寸422", r.status_code == 422)
after = c.get(BASE + "/api/web/apikeys", headers=auth).json()
used_after = next(k for k in after if k["name"] == "R2")["used_total"]
check("参数失败不扣配额", used_after == used_before, "before=%d after=%d" % (used_before, used_after))

# 正常提交一次并回调
r = c.post(BASE + "/api/v1/generations", headers=kh, json={
    "prompt": "clean gray background", "images_base64": [base64.b64encode(product).decode()],
    "callback_url": "http://127.0.0.1:9998/cb", "external_ref": "R2-001"})
check("v1 正常提交202", r.status_code == 202, r.text[:80])
vjob = r.json()
for _ in range(60):
    time.sleep(1)
    vjob = c.get(BASE + "/api/v1/generations/" + vjob["job_id"], headers=kh).json()
    if vjob["status"] in ("succeeded", "failed"): break
check("v1 任务完成", vjob["status"] == "succeeded", vjob.get("error", "")[:60])
check("v1 图片URL为绝对地址", vjob["images"] and vjob["images"][0]["url"].startswith("http"), vjob["images"][0]["url"] if vjob["images"] else "")

for _ in range(20):
    if received.get("body"): break
    time.sleep(0.5)
check("回调送达", bool(received.get("body")))
if received.get("body"):
    p = json.loads(received["body"])
    check("回调images含id/format(与查询一致)", p["images"] and "id" in p["images"][0] and "format" in p["images"][0], json.dumps(p["images"][0], ensure_ascii=False)[:120] if p["images"] else "")
    exp = "sha256=" + hmac.new(
        next(x for x in c.get(BASE+"/api/web/apikeys", headers=auth).json() if x["name"]=="R2") and b"", b"", hashlib.sha256).hexdigest()
    # 签名密钥只在创建时返回，这里用创建时保存的
check("v1 external_ref透传", vjob.get("external_ref") == "R2-001")

# 配额原子：quota=3，已用若干，连发直到 429
codes = []
for i in range(6):
    r = c.post(BASE + "/api/v1/generations", headers=kh,
               json={"prompt": "q", "images_base64": [base64.b64encode(product).decode()]})
    codes.append(r.status_code)
check("超额返回429", 429 in codes, str(codes))

print()
failed = [x for x in results if not x[1]]
print("=" * 50)
print("总计 %d 项，通过 %d，失败 %d" % (len(results), len(results) - len(failed), len(failed)))
for n, ok, d in failed:
    print("  FAIL:", n, "|", d)
sys.exit(1 if failed else 0)
