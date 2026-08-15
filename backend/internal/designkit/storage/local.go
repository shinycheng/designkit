package storage

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	dkdomain "github.com/Wei-Shaw/sub2api/internal/designkit/domain"
)

const (
	// EnvStorageDir 本地对象存储的根目录。
	EnvStorageDir = "DESIGNKIT_STORAGE_DIR"

	// DefaultStorageDir 环境变量没设时的默认根目录。
	// 容器里把它挂到宿主的持久化卷上；群晖那台机器上没有 S3，这就是唯一的落盘位置。
	DefaultStorageDir = "/app/data/designkit"

	// dirPerm / filePerm 目录和文件的权限。
	// 群晖上应用容器和备份脚本可能是不同用户，所以给同组和其他人读权限。
	dirPerm  fs.FileMode = 0o755
	filePerm fs.FileMode = 0o644

	// tmpPrefix 临时文件前缀。以 `.` 开头，正好被 ValidateKey 挡住，
	// 所以任何合法 key 都不可能撞上一个临时文件。
	tmpPrefix = ".dktmp-"
)

// LocalStore 是 domain.ObjectStore 的本地磁盘实现。
//
// 并发安全：所有方法都不持有跨调用状态，写入走「临时文件 + rename」，
// 同一个 key 被两个 goroutine 同时写时，最后一个 rename 赢，不会出现半张图。
type LocalStore struct {
	// root 已经解析过符号链接的绝对路径。
	// 根目录本身允许是软链（群晖上 /app/data 常常就是），
	// 但**根目录以下**任何一级都不允许再有软链，见 checkNoSymlink。
	root string
}

// 编译期断言：接口对齐。
var _ dkdomain.ObjectStore = (*LocalStore)(nil)

// NewLocalStoreFromEnv 按 DESIGNKIT_STORAGE_DIR 建本地存储，没设就用 DefaultStorageDir。
func NewLocalStoreFromEnv() (*LocalStore, error) {
	root := strings.TrimSpace(os.Getenv(EnvStorageDir))
	if root == "" {
		root = DefaultStorageDir
	}
	return NewLocalStore(root)
}

// NewLocalStore 建本地存储。root 不存在会被创建出来。
//
// 这里就把根目录建好并解析成绝对路径，是为了让后面每一次读写都能用
// 「前缀比对」判断有没有跑出根目录——延迟到第一次写再建，
// 会让「根目录配错了」这种问题在半夜第一张图上才暴露。
func NewLocalStore(root string) (*LocalStore, error) {
	trimmed := strings.TrimSpace(root)
	if trimmed == "" {
		return nil, errors.New("designkit storage: 根目录不能为空（检查 " + EnvStorageDir + "）")
	}
	abs, err := filepath.Abs(trimmed)
	if err != nil {
		return nil, fmt.Errorf("designkit storage: 解析根目录失败：%w", err)
	}
	if err := os.MkdirAll(abs, dirPerm); err != nil {
		return nil, fmt.Errorf("designkit storage: 创建根目录 %s 失败：%w", abs, err)
	}
	// 根目录自己可以是软链（群晖上很常见），这里解析成真实路径，
	// 后面的前缀比对才不会因为「/app/data 是软链」而全部误判成越界。
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return nil, fmt.Errorf("designkit storage: 解析根目录软链失败：%w", err)
	}
	return &LocalStore{root: resolved}, nil
}

// Root 返回根目录的绝对路径（已解析软链），给日志和健康检查用。
func (s *LocalStore) Root() string {
	if s == nil {
		return ""
	}
	return s.root
}

// Put 写入一个对象。同 key 重复写会覆盖（重试出图就是覆盖同一张）。
//
// contentType 在本地实现里**不落盘**：我们的 key 一律带正确的扩展名，
// Get 时按扩展名还原 MIME 即可，不值得为它多写一个 sidecar 文件。
// （换 S3 实现时它会被真的写进对象元数据。）
func (s *LocalStore) Put(ctx context.Context, key string, contentType string, data []byte) error {
	_ = contentType
	if err := ctx.Err(); err != nil {
		return err
	}
	abs, err := s.resolve(key)
	if err != nil {
		return err
	}

	dir := filepath.Dir(abs)
	if err := os.MkdirAll(dir, dirPerm); err != nil {
		return fmt.Errorf("designkit storage: 创建目录失败 %s：%w", dir, err)
	}
	// MkdirAll 会沿用已经存在的软链目录，所以建完之后要再查一次。
	if err := s.checkNoSymlink(abs); err != nil {
		return err
	}

	tmp, err := os.CreateTemp(dir, tmpPrefix)
	if err != nil {
		return fmt.Errorf("designkit storage: 创建临时文件失败：%w", err)
	}
	tmpName := tmp.Name()
	// 失败路径上要把临时文件清掉，否则数据目录会慢慢被半截文件塞满。
	committed := false
	defer func() {
		if !committed {
			_ = tmp.Close()
			_ = os.Remove(tmpName)
		}
	}()

	if _, err := tmp.Write(data); err != nil {
		return fmt.Errorf("designkit storage: 写临时文件失败：%w", err)
	}
	// fsync 之后再 rename：不 fsync 的话，掉电后可能出现「文件名在、内容是空的」。
	if err := tmp.Sync(); err != nil {
		return fmt.Errorf("designkit storage: 刷盘失败：%w", err)
	}
	// CreateTemp 建出来的是 0600。改成 0644 是为了让备份脚本 / 另一个容器也能读。
	// 改不动不算失败（群晖上挂 SMB/NTFS 的目录不支持 chmod）——
	// 文件本身照样能用，为这个把出好的图丢掉不值得。
	if err := tmp.Chmod(filePerm); err != nil {
		slog.Debug("designkit storage: 设置文件权限失败，按默认权限继续",
			slog.String("path", tmpName), slog.Any("error", err))
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("designkit storage: 关闭临时文件失败：%w", err)
	}
	if err := os.Rename(tmpName, abs); err != nil {
		return fmt.Errorf("designkit storage: 提交文件失败：%w", err)
	}
	committed = true

	// 目录项也刷一下盘。失败不算错（有的文件系统不支持对目录 fsync），
	// 只是掉电窗口稍大一点。
	if dirFile, err := os.Open(dir); err == nil {
		_ = dirFile.Sync()
		_ = dirFile.Close()
	}
	return nil
}

// Get 读出一个对象。不存在返回 domain.ErrObjectNotFound。
//
// 返回的 contentType 由 key 的扩展名还原，扩展名认不出来时按字节嗅探。
func (s *LocalStore) Get(ctx context.Context, key string) ([]byte, string, error) {
	if err := ctx.Err(); err != nil {
		return nil, "", err
	}
	abs, err := s.resolve(key)
	if err != nil {
		return nil, "", err
	}

	// Lstat 而不是 Stat：目标本身是软链时要拒绝，不是跟过去读。
	info, err := os.Lstat(abs)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, "", fmt.Errorf("%w: %s", dkdomain.ErrObjectNotFound, key)
		}
		return nil, "", fmt.Errorf("designkit storage: 读取失败 %s：%w", key, err)
	}
	if !info.Mode().IsRegular() {
		return nil, "", fmt.Errorf("%w: %q 不是普通文件", ErrInvalidKey, key)
	}

	data, err := os.ReadFile(abs)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, "", fmt.Errorf("%w: %s", dkdomain.ErrObjectNotFound, key)
		}
		return nil, "", fmt.Errorf("designkit storage: 读取失败 %s：%w", key, err)
	}
	return data, ContentTypeForKey(key, data), nil
}

// Delete 删除一个对象。key 不存在时返回 nil（幂等）。
func (s *LocalStore) Delete(ctx context.Context, key string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	abs, err := s.resolve(key)
	if err != nil {
		return err
	}
	if err := os.Remove(abs); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("designkit storage: 删除失败 %s：%w", key, err)
	}
	return nil
}

// Exists 判断对象在不在。软链、目录一律当成「不在」。
func (s *LocalStore) Exists(ctx context.Context, key string) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	abs, err := s.resolve(key)
	if err != nil {
		return false, err
	}
	info, err := os.Lstat(abs)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return false, nil
		}
		return false, fmt.Errorf("designkit storage: 探测失败 %s：%w", key, err)
	}
	return info.Mode().IsRegular(), nil
}

// resolve 把 key 变成 root 下的绝对路径，并做三重防护：
//
//  1. ValidateKey：字符集 + 逐段检查，`..` 和绝对路径在这一步就被挡掉；
//  2. Clean 之后的前缀比对：万一 ValidateKey 哪天被改松了，这一条还兜着；
//  3. checkNoSymlink：逐级 Lstat，路径里有软链一律拒绝。
//
// 第 2、3 条是刻意的重复防护。路径穿越一旦漏一个，泄露的是整台 NAS。
func (s *LocalStore) resolve(key string) (string, error) {
	if s == nil || s.root == "" {
		return "", errors.New("designkit storage: 存储未初始化")
	}
	if err := ValidateKey(key); err != nil {
		return "", err
	}

	abs := filepath.Clean(filepath.Join(s.root, filepath.FromSlash(key)))
	if abs == s.root || !strings.HasPrefix(abs, s.root+string(os.PathSeparator)) {
		return "", fmt.Errorf("%w: %q 解析后跑出了根目录", ErrInvalidKey, key)
	}
	if err := s.checkNoSymlink(abs); err != nil {
		return "", err
	}
	return abs, nil
}

// checkNoSymlink 从根目录往下逐级 Lstat，只要有一级是符号链接就拒绝。
//
// 为什么必须查：前缀比对只看字符串，`<root>/designkit/x` 里的 `designkit`
// 要是被人做成指向 `/etc` 的软链，字符串比对完全看不出来，
// 读能读出 /etc/passwd，写能写进系统目录。
//
// 路径中间某一级还不存在时直接返回 nil —— 不存在就不可能是软链，
// 更深的层级也必然不存在。
func (s *LocalStore) checkNoSymlink(abs string) error {
	rel, err := filepath.Rel(s.root, abs)
	if err != nil {
		return fmt.Errorf("%w: 无法定位 %q", ErrInvalidKey, abs)
	}
	cur := s.root
	for _, segment := range strings.Split(rel, string(os.PathSeparator)) {
		cur = filepath.Join(cur, segment)
		info, err := os.Lstat(cur)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return fmt.Errorf("designkit storage: 检查路径失败 %s：%w", cur, err)
		}
		if info.Mode()&fs.ModeSymlink != 0 {
			return fmt.Errorf("%w: 路径里存在符号链接 %s", ErrInvalidKey, cur)
		}
	}
	return nil
}

// extContentTypes 扩展名 → MIME。
//
// 刻意不依赖 mime.TypeByExtension：那个函数在不同镜像里读的是不同的
// /etc/mime.types，同一份代码在开发机和群晖上可能给出不同结果，
// 而 HEIC 这类新格式很多系统的表里根本没有。
var extContentTypes = map[string]string{
	".png":  "image/png",
	".jpg":  "image/jpeg",
	".jpeg": "image/jpeg",
	".webp": "image/webp",
	".gif":  "image/gif",
	".bmp":  "image/bmp",
	".heic": "image/heic",
	".heif": "image/heif",
	".avif": "image/avif",
	".json": "application/json",
	".txt":  "text/plain; charset=utf-8",
}

// ContentTypeForKey 由 key 的扩展名还原 MIME；认不出来就按字节嗅探，
// 再认不出来给 application/octet-stream。
func ContentTypeForKey(key string, data []byte) string {
	ext := strings.ToLower(filepath.Ext(key))
	if ct, ok := extContentTypes[ext]; ok {
		return ct
	}
	if len(data) > 0 {
		return http.DetectContentType(data)
	}
	return "application/octet-stream"
}
