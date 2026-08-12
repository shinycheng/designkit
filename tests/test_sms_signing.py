"""阿里云短信签名算法：对着官方文档的测试向量逐字核。

为什么值得单独一个文件长期跑：签名算错的表现是**一直 403**，而阿里云返回的
错误信息看不出是哪一步出的问题——编码差一个字符和密钥完全填错，报的是同一句话。
真机调试它要么花钱发短信，要么对着一堆百分号肉眼比对。

所以这里用**阿里云官方文档给出的那组示例数据**当测试向量（它给了确定的
AccessKeySecret 和期望签名，见 help.aliyun.com 的「RPC 签名机制」）。
只要这一组对得上，编码规则、拼接顺序、密钥后缀、摘要算法就全对。

注意向量用的是 ECS 的 DescribeDedicatedHosts，不是短信接口——签名机制是
所有 RPC 风格接口共用的，换成短信只是参数不同。这正是它能当测试向量的原因：
不用真发一条短信（那要花钱），就能证明签名对。
"""
import atexit
import base64
import hashlib
import hmac
import os
import shutil
import sys
import tempfile
import unittest

_HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, os.path.dirname(_HERE))

if not os.environ.get("DESIGNKIT_DATA_DIR"):
    _TMP = tempfile.mkdtemp(prefix="dk-sms-signing-")
    os.environ["DESIGNKIT_DATA_DIR"] = _TMP
    atexit.register(shutil.rmtree, _TMP, ignore_errors=True)
os.environ.setdefault("DESIGNKIT_PROVIDER", "mock")

from backend.app.services import sms  # noqa: E402

# ── 阿里云官方文档「RPC 签名机制」里的那组示例 ──
_PARAMS = {
    "AccessKeyId": "testid",
    "Action": "DescribeDedicatedHosts",
    "Format": "JSON",
    "RegionId": "cn-beijing",
    "SignatureMethod": "HMAC-SHA1",
    "SignatureNonce": "edb2b34af0af9a6d14deaf7c1a5315eb",
    "SignatureVersion": "1.0",
    "Timestamp": "2023-03-13T08:34:30Z",
    "Version": "2014-05-26",
}
_SECRET = "testsecret"
_EXPECTED_QUERY = (
    "AccessKeyId=testid&Action=DescribeDedicatedHosts&Format=JSON"
    "&RegionId=cn-beijing&SignatureMethod=HMAC-SHA1"
    "&SignatureNonce=edb2b34af0af9a6d14deaf7c1a5315eb&SignatureVersion=1.0"
    "&Timestamp=2023-03-13T08%3A34%3A30Z&Version=2014-05-26"
)
_EXPECTED_STS = (
    "GET&%2F&AccessKeyId%3Dtestid%26Action%3DDescribeDedicatedHosts%26Format%3DJSON"
    "%26RegionId%3Dcn-beijing%26SignatureMethod%3DHMAC-SHA1"
    "%26SignatureNonce%3Dedb2b34af0af9a6d14deaf7c1a5315eb%26SignatureVersion%3D1.0"
    "%26Timestamp%3D2023-03-13T08%253A34%253A30Z%26Version%3D2014-05-26"
)
_EXPECTED_SIGNATURE = "9NaGiOspFP5UPcwX8Iwt2YJXXuk="


class OfficialVectorTests(unittest.TestCase):
    def test_canonical_query_matches_the_official_example(self):
        """参数要按字典序排、值要百分号编码，冒号变 %3A。"""
        self.assertEqual(sms._canonical_query(_PARAMS), _EXPECTED_QUERY)

    def test_string_to_sign_matches_the_official_example(self):
        """整个查询串**要再被编码一遍**，所以 %3A 会变成 %253A。

        漏掉这一层是最常见的错误，而且拼出来的串看起来完全正常。
        """
        sts = "GET&%s&%s" % (
            sms.percent_encode("/"), sms.percent_encode(sms._canonical_query(_PARAMS)))
        self.assertEqual(sts, _EXPECTED_STS)

    def test_signature_matches_the_official_example(self):
        """密钥必须是 AccessKeySecret + "&"，输出是 Base64 不是十六进制。"""
        sts = "GET&%s&%s" % (
            sms.percent_encode("/"), sms.percent_encode(sms._canonical_query(_PARAMS)))
        signature = base64.b64encode(hmac.new(
            (_SECRET + "&").encode("utf-8"), sts.encode("utf-8"), hashlib.sha1,
        ).digest()).decode("ascii")
        self.assertEqual(signature, _EXPECTED_SIGNATURE)


class PercentEncodeTests(unittest.TestCase):
    """三处和标准 urlencode 不一样的地方——「一直 403」多半就出在这儿。"""

    def test_space_becomes_percent_20_not_plus(self):
        self.assertEqual(sms.percent_encode("a b"), "a%20b")

    def test_star_is_encoded(self):
        self.assertEqual(sms.percent_encode("a*b"), "a%2Ab")

    def test_tilde_is_not_encoded(self):
        self.assertEqual(sms.percent_encode("a~b"), "a~b")

    def test_slash_is_encoded(self):
        """默认 safe='/' 会放过斜杠，那样待签名串的第二段就错了。"""
        self.assertEqual(sms.percent_encode("/"), "%2F")

    def test_chinese_is_utf8_percent_encoded(self):
        self.assertEqual(sms.percent_encode("中"), "%E4%B8%AD")


if __name__ == "__main__":
    unittest.main()
