package worker

import (
	cryptorand "crypto/rand"
	"encoding/binary"
	mathrand "math/rand/v2"
	"time"
)

// ============================================================================
// ULID 生成
// ============================================================================
//
// 结果图要有自己的对外编号（designkit_images.uid），格式是 26 位 ULID ——
// domain.IsValidULID 就是照这个校验的。
//
// 为什么自己写而不是引一个库：go.mod 是上游文件，**不在允许触碰的 5 个文件里**，
// 加一个依赖就等于改上游文件。ULID 的编码规则本身只有二十几行，
// 自己写比破合并纪律便宜得多。
//
// 格式（跟 ULID 规范一致）：
//
//	48 bit 毫秒时间戳 + 80 bit 随机数 = 128 bit
//	按 Crockford Base32（去掉了容易看混的 I / L / O / U）编码成 26 个字符
//	26 × 5 = 130 bit，最前面补 2 个 0
//
// 时间戳在前的好处：uid 按字典序排就是按创建时间排，排障时一眼能看出先后。

// crockfordAlphabet 必须跟 domain 里 IsValidULID 用的字符集**逐字相同**，
// 否则我们生成的 uid 会被自己的校验函数判成非法。
const crockfordAlphabet = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

// ulidLen 一个 ULID 的字符数。
const ulidLen = 26

// NewULID 生成一个 26 位 ULID。
//
// 随机源优先用 crypto/rand；万一它出错（几乎只会发生在系统熵源坏掉时），
// 退回 math/rand/v2 —— **宁可随机性弱一点，也不能让出图流程因为取不到随机数而中断**：
// 那时候图已经出了、钱已经花了，卡在「给图起编号」这一步是最不值的。
func NewULID() string {
	return newULIDAt(time.Now())
}

func newULIDAt(t time.Time) string {
	var raw [16]byte

	ms := uint64(t.UTC().UnixMilli())
	raw[0] = byte(ms >> 40)
	raw[1] = byte(ms >> 32)
	raw[2] = byte(ms >> 24)
	raw[3] = byte(ms >> 16)
	raw[4] = byte(ms >> 8)
	raw[5] = byte(ms)

	if _, err := cryptorand.Read(raw[6:]); err != nil {
		var fallback [8]byte
		binary.BigEndian.PutUint64(fallback[:], mathrand.Uint64())
		copy(raw[6:], fallback[:])
		binary.BigEndian.PutUint16(raw[14:], uint16(mathrand.Uint32()))
	}

	return encodeCrockford(raw)
}

// encodeCrockford 把 128 bit 编成 26 个字符。
//
// 实现方式是最笨但最不会错的那种：把 16 字节看成一个 130 bit 的大整数
// （最高 2 位补 0），从高位开始每 5 bit 取一个字符。
func encodeCrockford(raw [16]byte) string {
	out := make([]byte, ulidLen)
	for i := 0; i < ulidLen; i++ {
		var v byte
		for k := 0; k < 5; k++ {
			v = v<<1 | bitAt(raw, i*5+k)
		}
		out[i] = crockfordAlphabet[v]
	}
	return string(out)
}

// bitAt 取 130 bit 视图里第 idx 位（0 = 最高位）。前两位恒为 0。
func bitAt(raw [16]byte, idx int) byte {
	if idx < 2 {
		return 0
	}
	pos := idx - 2
	return (raw[pos/8] >> (7 - uint(pos%8))) & 1
}
