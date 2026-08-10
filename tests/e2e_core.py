"""端到端验证：网页流程 + 对外 API 流程 + webhook 回调签名。

**这个脚本会把 admin 的密码从初始口令改掉**（服务端有强制改密的闸门，
不先改掉的话，后面每一个业务接口都会返回 403「请先修改初始密码，再使用其他功能」）。
所以它必须跑在**自己的一份全新数据**上，不能和 tests/e2e_security.py 共用同一份——
那个脚本第一项要验的正是「默认口令可登录」。跑法见 tests/README.md，照抄即可。

服务地址默认 http://127.0.0.1:8787，也可以用环境变量 DESIGNKIT_E2E_BASE 指到别的端口。
"""
import base64
import hashlib
import hmac
import io
import json
import os
import sys
import threading
import time
from http.server import BaseHTTPRequestHandler, HTTPServer

import httpx
from PIL import Image, ImageDraw

BASE = os.environ.get("DESIGNKIT_E2E_BASE", "http://127.0.0.1:8787").rstrip("/")
# 本脚本把 admin 的密码改成这个。跑完这份数据就「用过了」，下次要重新来一份干净的。
NEW_PASSWORD = "corepass8888"
results = []


def check(name, ok, detail=""):
    results.append((name, ok, detail))
    print(("PASS  " if ok else "FAIL  ") + name + ("  | " + str(detail)[:160] if detail else ""))


# ---------- 回调接收器 ----------
received = {}

class CallbackHandler(BaseHTTPRequestHandler):
    def do_POST(self):
        length = int(self.headers.get("Content-Length", 0))
        body = self.rfile.read(length)
        received["body"] = body
        received["signature"] = self.headers.get("X-DesignKit-Signature", "")
        received["timestamp"] = self.headers.get("X-DesignKit-Timestamp", "")
        self.send_response(200)
        self.end_headers()
        self.wfile.write(b"ok")
    def log_message(self, *args):
        pass

server = HTTPServer(("127.0.0.1", 9999), CallbackHandler)
threading.Thread(target=server.serve_forever, daemon=True).start()

# ---------- 造一张测试商品图 ----------
im = Image.new("RGB", (600, 600), (240, 240, 245))
d = ImageDraw.Draw(im)
d.ellipse([150, 150, 450, 450], fill=(200, 60, 60))
d.text((250, 520), "TEST PRODUCT", fill=(60, 60, 60))
buf = io.BytesIO()
im.save(buf, "PNG")
product_png = buf.getvalue()

client = httpx.Client(timeout=60)

def full(u):
    return (BASE + u) if u and u.startswith("/") else u

# ========== 网页端流程 ==========
r = client.post(BASE + "/api/web/auth/login", json={"username": "admin", "password": "admin123456"})
check("网页登录", r.status_code == 200, r.text[:100])
token = r.json()["token"]
auth = {"Authorization": "Bearer " + token}

# 首次登录必须先改掉初始密码，否则服务端会把后面每一个业务接口都拦成 403。
# 这不是测试脚本的特殊要求，真人第一次登录看到的也是同一道改密页。
r = client.post(BASE + "/api/web/auth/change_password", headers=auth,
                json={"old_password": "admin123456", "new_password": NEW_PASSWORD})
check("首登强制改密并换发新令牌", r.status_code == 200 and bool(r.json().get("token")), r.text[:120])
token = r.json()["token"]
auth = {"Authorization": "Bearer " + token}

r = client.get(BASE + "/api/web/templates", headers=auth)
templates = r.json()
check("模板列表(种子数据)", r.status_code == 200 and len(templates) >= 6, "共 %d 个" % len(templates))

r = client.post(BASE + "/api/web/uploads", headers=auth,
                files={"file": ("product.png", product_png, "image/png")})
check("上传商品图", r.status_code == 200, r.text[:120])
upload = r.json()

r = client.get(full(upload["url"]))
check("上传图可访问", r.status_code == 200, upload["url"])

scene_tpl = next(t for t in templates if t["variables"])
r = client.post(BASE + "/api/web/generations", headers=auth, json={
    "template_id": scene_tpl["id"],
    "variables": {"scene": "现代客厅的木质茶几上", "style": "高级质感"},
    "upload_ids": [upload["id"]],
    "n": 2, "size": "1024x1024", "quality": "high",
    "extra_instructions": "整体偏暖色调",
})
check("创建生成任务(模板+变量)", r.status_code == 200, r.text[:150])
job = r.json()
check("提示词变量渲染", "现代客厅的木质茶几上" in job["prompt"] and "整体偏暖色调" in job["prompt"], job["prompt"][:120])

# 缺少必填变量应报错
r = client.post(BASE + "/api/web/generations", headers=auth, json={
    "template_id": scene_tpl["id"], "variables": {}, "upload_ids": [upload["id"]],
})
check("缺少必填变量被拦截(422)", r.status_code == 422, r.text[:100])

# 轮询任务完成
for _ in range(60):
    time.sleep(1)
    r = client.get(BASE + "/api/web/generations/" + job["job_id"], headers=auth)
    job = r.json()
    if job["status"] in ("succeeded", "failed"):
        break
check("mock 生成完成", job["status"] == "succeeded", "status=%s err=%s" % (job["status"], job.get("error", "")[:80]))
check("生成 2 张图", len(job["images"]) == 2, "实际 %d 张" % len(job["images"]))
if job["images"]:
    r = client.get(full(job["images"][0]["url"]))
    check("结果图可访问", r.status_code == 200 and r.headers.get("content-type", "").startswith("image/"))
    r = client.get(full(job["images"][0]["thumbnail_url"]))
    check("缩略图可访问", r.status_code == 200)

# 列表
r = client.get(BASE + "/api/web/generations?page=1&page_size=5", headers=auth)
check("任务列表分页", r.status_code == 200 and r.json()["total"] >= 1, "total=%s" % r.json().get("total"))

# 未登录被拦
r = client.get(BASE + "/api/web/generations")
check("未登录被拦截(401)", r.status_code == 401)

# 数据库文件不可被静态目录访问
r = client.get(BASE + "/files/designkit.db")
check("数据库文件不可下载", r.status_code == 404, "HTTP %d" % r.status_code)
r = client.get(BASE + "/files/.secret_key")
check("密钥文件不可下载", r.status_code == 404, "HTTP %d" % r.status_code)

# ========== 对外 API 流程 ==========
r = client.post(BASE + "/api/web/apikeys", headers=auth, json={"name": "E2E测试ERP", "monthly_quota": 100})
check("创建API Key", r.status_code == 200, r.text[:100])
key_data = r.json()
api_key = key_data["api_key"]
webhook_secret = key_data["webhook_secret"]
kh = {"X-API-Key": api_key}

r = client.get(BASE + "/api/v1/ping", headers=kh)
check("v1 ping", r.status_code == 200, r.text[:80])

r = client.get(BASE + "/api/v1/ping", headers={"X-API-Key": "dk_wrong"})
check("v1 错误Key被拦截(401)", r.status_code == 401)

r = client.get(BASE + "/api/v1/templates", headers=kh)
check("v1 模板列表", r.status_code == 200 and len(r.json()) >= 6)

r = client.post(BASE + "/api/v1/generations", headers=kh, json={
    "prompt": "极简风格的浅灰色背景商品图，柔和阴影",
    "images_base64": [base64.b64encode(product_png).decode()],
    "n": 1, "size": "1024x1024",
    "callback_url": "http://127.0.0.1:9999/cb",
    "external_ref": "ERP-ORDER-001",
})
check("v1 创建任务(base64图+回调)", r.status_code == 202, r.text[:150])
v1_job = r.json()

for _ in range(60):
    time.sleep(1)
    r = client.get(BASE + "/api/v1/generations/" + v1_job["job_id"], headers=kh)
    v1_job = r.json()
    if v1_job["status"] in ("succeeded", "failed"):
        break
check("v1 任务完成", v1_job["status"] == "succeeded", "status=%s err=%s" % (v1_job["status"], v1_job.get("error", "")[:80]))
check("v1 external_ref 保留", v1_job.get("external_ref") == "ERP-ORDER-001")

# 等回调
for _ in range(20):
    if received.get("body"):
        break
    time.sleep(0.5)
check("webhook 回调已送达", bool(received.get("body")))
if received.get("body"):
    payload = json.loads(received["body"])
    check("回调带 external_ref 与图片", payload.get("external_ref") == "ERP-ORDER-001" and len(payload.get("images", [])) == 1,
          json.dumps(payload, ensure_ascii=False)[:150])
    expected = "sha256=" + hmac.new(
        webhook_secret.encode(),
        (received["timestamp"] + ".").encode() + received["body"],
        hashlib.sha256).hexdigest()
    check("webhook 签名校验通过", received["signature"] == expected,
          "got=%s" % received["signature"][:30])

# 越权：网页任务不能被 API Key 查询
r = client.get(BASE + "/api/v1/generations/" + job["job_id"], headers=kh)
check("v1 无法越权查看网页任务(404)", r.status_code == 404)

print()
failed = [x for x in results if not x[1]]
print("=" * 50)
print("总计 %d 项，通过 %d 项，失败 %d 项" % (len(results), len(results) - len(failed), len(failed)))
sys.exit(1 if failed else 0)
