# realesr-general-x4v3.onnx 的来源与许可证

本目录下的 `realesr-general-x4v3.onnx` 是「高清放大」功能用的
Real-ESRGAN 小模型（SRVGGNetCompact，4 倍通用超分辨率），
**BSD-3-Clause 许可证**，版权归 Xintao Wang。

## 来源链

- 原始权重：[`realesr-general-x4v3.pth`](https://github.com/xinntao/Real-ESRGAN/releases/download/v0.2.5.0/realesr-general-x4v3.pth)
  —— Real-ESRGAN 官方 v0.2.5.0 release，
  sha256 `8dc7edb9ac80ccdc30c3a5dca6616509367f05fbc184ad95b731f05bece96292`
- ONNX 转换：Hugging Face
  [`CoderViking/realesr-general-x4v3-onnx`](https://huggingface.co/CoderViking/realesr-general-x4v3-onnx)
  （同样标注 BSD-3-Clause；仓库里附带可复现的导出脚本 `export_realesr.py`：
  PyTorch 2.8.0 TorchScript exporter、opset 17、fp32、开常量折叠）
- 本仓库这一份的 sha256：
  `1940a93ee08283a0a7286183186357b1688fe9fa8ede74604b424586aaddf112`
  （2026-08-16 下载时校验过 ONNX protobuf 头和算子表：Conv / PRelu /
  DepthToSpace / Resize / Add，动态 H×W 输入）

## 模型接口（app/upscale.py 依赖这几条，换模型前先核对）

- 输入 `input`：`[1, 3, H, W]`，NCHW、RGB、float32、取值 [0, 1]，H/W 动态
- 输出 `output`：`[1, 3, 4H, 4W]`，**图内没有 clip**，转 uint8 前要自己 clamp

## 许可证原文（BSD 3-Clause）

> Copyright (c) 2021, Xintao Wang
> All rights reserved.
>
> Redistribution and use in source and binary forms, with or without
> modification, are permitted provided that the following conditions are met:
>
> 1. Redistributions of source code must retain the above copyright notice,
>    this list of conditions and the following disclaimer.
>
> 2. Redistributions in binary form must reproduce the above copyright notice,
>    this list of conditions and the following disclaimer in the documentation
>    and/or other materials provided with the distribution.
>
> 3. Neither the name of the copyright holder nor the names of its contributors
>    may be used to endorse or promote products derived from this software
>    without specific prior written permission.
>
> THIS SOFTWARE IS PROVIDED BY THE COPYRIGHT HOLDERS AND CONTRIBUTORS
> "AS IS" AND ANY EXPRESS OR IMPLIED WARRANTIES, INCLUDING, BUT NOT LIMITED
> TO, THE IMPLIED WARRANTIES OF MERCHANTABILITY AND FITNESS FOR A PARTICULAR
> PURPOSE ARE DISCLAIMED. IN NO EVENT SHALL THE COPYRIGHT HOLDER OR
> CONTRIBUTORS BE LIABLE FOR ANY DIRECT, INDIRECT, INCIDENTAL, SPECIAL,
> EXEMPLARY, OR CONSEQUENTIAL DAMAGES (INCLUDING, BUT NOT LIMITED TO,
> PROCUREMENT OF SUBSTITUTE GOODS OR SERVICES; LOSS OF USE, DATA, OR
> PROFITS; OR BUSINESS INTERRUPTION) HOWEVER CAUSED AND ON ANY THEORY OF
> LIABILITY, WHETHER IN CONTRACT, STRICT LIABILITY, OR TORT (INCLUDING
> NEGLIGENCE OR OTHERWISE) ARISING IN ANY WAY OUT OF THE USE OF THIS
> SOFTWARE, EVEN IF ADVISED OF THE POSSIBILITY OF SUCH DAMAGE.
