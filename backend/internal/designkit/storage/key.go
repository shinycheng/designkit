package storage

import (
	"errors"
	"fmt"
	"strings"
)

// ErrInvalidKey key 不合法（含路径穿越、非法字符、超长等）。
//
// **拿到它一律拒绝**，不要尝试「清洗一下再用」——清洗过的路径穿越
// 是历史上最常见的绕过来源（`....//` 清洗一次变成 `../`）。
var ErrInvalidKey = errors.New("designkit storage: 非法的 object key")

const (
	// MaxObjectKeyLen key 的长度上限。
	// designkit_images.object_key / designkit_assets.object_key 都是 VARCHAR(512)，
	// 超过这个长度的 key 就算写进对象存储也存不进数据库，不如在这里先拦掉。
	MaxObjectKeyLen = 512

	// maxKeySegmentLen 单个路径段的长度上限。
	// 绝大多数文件系统（ext4/xfs/btrfs/APFS）的单段上限是 255 字节。
	maxKeySegmentLen = 255
)

// ValidateKey 校验一个 object key 能不能安全地映射成 root 下的相对路径。
//
// 规则（全部是白名单，宁可误伤也不放行）：
//
//   - 非空，长度 ≤ MaxObjectKeyLen；
//   - 只允许 A-Z a-z 0-9 和 `.` `_` `-` `/` 四个符号。
//     这一条就把 `\`（Windows 分隔符）、`:`（Windows 盘符 / ADS）、空字节、
//     换行、空格、中文全挡掉了；
//   - 不能以 `/` 开头（绝对路径）或结尾（目录）；
//   - 按 `/` 切开之后，每一段都不能为空（挡 `a//b`）、不能是 `.` 或 `..`
//     （挡路径穿越）、不能以 `.` 开头（挡 `.ssh`、`.dktmp-` 这类隐藏文件和
//     我们自己的临时文件）、不能以 `.` 结尾、长度 ≤ 255。
//
// 我们真实用到的 key 全部由 domain.ImageObjectKey / AssetObjectKey /
// VariantObjectKey 生成，天然满足以上全部规则（比例里的 `:` 在 VariantObjectKey
// 里已经换成了 `x`）。
func ValidateKey(key string) error {
	if key == "" {
		return fmt.Errorf("%w: key 是空的", ErrInvalidKey)
	}
	if len(key) > MaxObjectKeyLen {
		return fmt.Errorf("%w: key 长度 %d 超过上限 %d", ErrInvalidKey, len(key), MaxObjectKeyLen)
	}
	if strings.HasPrefix(key, "/") {
		return fmt.Errorf("%w: key 不能以 / 开头（那是绝对路径）：%q", ErrInvalidKey, key)
	}
	if strings.HasSuffix(key, "/") {
		return fmt.Errorf("%w: key 不能以 / 结尾（那是目录）：%q", ErrInvalidKey, key)
	}
	for i := 0; i < len(key); i++ {
		if !isAllowedKeyByte(key[i]) {
			return fmt.Errorf("%w: key 里出现了不允许的字符 %q：%q", ErrInvalidKey, string(key[i]), key)
		}
	}

	for _, segment := range strings.Split(key, "/") {
		switch {
		case segment == "":
			return fmt.Errorf("%w: key 里有空的路径段（连续的 /）：%q", ErrInvalidKey, key)
		case segment == "." || segment == "..":
			return fmt.Errorf("%w: key 里有 %q 这样的相对路径段：%q", ErrInvalidKey, segment, key)
		case strings.HasPrefix(segment, "."):
			return fmt.Errorf("%w: key 的路径段不能以 . 开头：%q", ErrInvalidKey, key)
		case strings.HasSuffix(segment, "."):
			return fmt.Errorf("%w: key 的路径段不能以 . 结尾：%q", ErrInvalidKey, key)
		case len(segment) > maxKeySegmentLen:
			return fmt.Errorf("%w: key 的路径段超过 %d 字节：%q", ErrInvalidKey, maxKeySegmentLen, key)
		}
	}
	return nil
}

// isAllowedKeyByte 白名单字符集。
func isAllowedKeyByte(b byte) bool {
	switch {
	case b >= 'a' && b <= 'z':
		return true
	case b >= 'A' && b <= 'Z':
		return true
	case b >= '0' && b <= '9':
		return true
	case b == '.' || b == '_' || b == '-' || b == '/':
		return true
	default:
		return false
	}
}
