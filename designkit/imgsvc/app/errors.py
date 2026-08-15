"""统一的错误类型。

这个服务**只有一种错误响应格式**（对应 设计定型.md 3.1 的思路：
调用方用强类型语言反序列化，多种格式迟早出偶发解析异常）：

    {"error": {"code": "invalid_ratio", "message": "中文说明"}}

`code` 是 ASCII 小写下划线，给程序判断用；`message` 是中文，给人看。
"""

from __future__ import annotations


class ImgSvcError(Exception):
    """所有可预期错误的基类。带 HTTP 状态码 + 机器码 + 中文说明。"""

    status_code = 500
    code = "internal_error"

    def __init__(self, message: str, code: str = "", status_code: int = 0) -> None:
        super().__init__(message)
        self.message = message
        if code:
            self.code = code
        if status_code:
            self.status_code = status_code


class BadRequest(ImgSvcError):
    """参数不对。400。"""

    status_code = 400
    code = "invalid_request"


class RatioError(BadRequest):
    """`ratio` 不是合法的比例串。400。"""

    code = "invalid_ratio"


class Unauthorized(ImgSvcError):
    """没带 / 带错 Bearer Token。401。"""

    status_code = 401
    code = "unauthorized"


class PayloadTooLarge(ImgSvcError):
    """上传体积或像素数超限。413。"""

    status_code = 413
    code = "file_too_large"


class UnsupportedImage(ImgSvcError):
    """Pillow 认不出来 / 解码失败。415。"""

    status_code = 415
    code = "unsupported_media_type"


class HeifUnsupported(ImgSvcError):
    """是 HEIC/HEIF，但本容器里没装成 pillow-heif。422。

    单独一个码，因为它跟「文件坏了」是两回事：这是**部署问题**，
    换个能装 pillow-heif 的镜像就好了，用户的图本身没毛病。
    """

    status_code = 422
    code = "heif_unsupported"


class ServiceBusy(ImgSvcError):
    """并发闸门排队超时。503。调用方可以重试。"""

    status_code = 503
    code = "busy"
