"""防 SSRF 的地址校验：拒绝 URL 指向危险的内部地址。

对外 API 的 image_urls（服务端代下载）和 webhook 回调（服务端代发）都用它。

设计权衡：本平台「先内部用」，ERP 和图片服务器往往就在内网/同机，
因此默认允许私网/环回地址（allow_internal=True），只把**永远危险**的目标写死拦掉：
云服务器元数据地址（169.254.169.254 等链路本地段，可窃取云主机凭证）、组播、保留段。
将来转为公网 SaaS 时，把系统设置里的「允许回调/取图到内网地址」关掉即可全面拦截私网。
"""
import ipaddress
import socket
from urllib.parse import urlparse


class UnsafeUrlError(Exception):
    """URL 指向了不允许访问的地址。"""


def _classify(ip_str: str, allow_internal: bool) -> bool:
    """返回该 IP 是否允许访问。"""
    try:
        ip = ipaddress.ip_address(ip_str)
    except ValueError:
        return False
    # 无论如何都拦截：链路本地（含云元数据 169.254.169.254）、组播、保留、未指定地址
    if ip.is_link_local or ip.is_multicast or ip.is_reserved or ip.is_unspecified:
        return False
    # 私网 / 环回：内部模式允许（内网 ERP、同机服务），公网模式拦截
    if ip.is_private or ip.is_loopback:
        return allow_internal
    return True


def assert_safe_url(url: str, allow_internal: bool = True) -> None:
    """校验 URL 协议合法且解析出的所有 IP 均可访问，否则抛 UnsafeUrlError。"""
    if not (url.startswith("http://") or url.startswith("https://")):
        raise UnsafeUrlError("必须是 http(s) 链接")
    host = urlparse(url).hostname
    if not host:
        raise UnsafeUrlError("无法解析主机名")
    try:
        infos = socket.getaddrinfo(host, None)
    except socket.gaierror:
        raise UnsafeUrlError("域名无法解析")
    resolved = {info[4][0] for info in infos}
    if not resolved:
        raise UnsafeUrlError("域名无法解析出地址")
    for ip_str in resolved:
        if not _classify(ip_str, allow_internal):
            raise UnsafeUrlError("不允许指向该地址（内网/元数据/保留段）")
