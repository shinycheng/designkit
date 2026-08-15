package storage

import (
	"context"
	"errors"

	dkdomain "github.com/Wei-Shaw/sub2api/internal/designkit/domain"
)

// ErrS3NotImplemented S3 实现还没做。
//
// 本轮**刻意只留空壳**：monica 的群晖上没有 S3，本地磁盘实现就是生产实现。
// 留这个文件是为了把「换存储」的成本钉死在一个包里——
// 哪天真上了对象存储，只要把这几个方法填上、启动时换一个构造函数，
// service 层一行都不用改（它只认 domain.ObjectStore）。
var ErrS3NotImplemented = errors.New("designkit storage: S3 存储尚未实现（本轮只有本地磁盘实现）")

// S3Config S3 兼容对象存储的连接参数。
//
// TODO(designkit): 真要实现时注意三件事，别踩上游踩过的坑：
//  1. **key 由我们生成并入库**，绝不要用「上传完返回 URL、URL 入库」那一套——
//     上游默认签发 24 小时失效的 presigned 链接，存进库过期后拼不回来
//     （CLAUDE.md 第二·五节 C）。URL 一律运行时按 key 现签。
//  2. Delete 对不存在的 key 必须返回 nil（S3 的 DeleteObject 本来就是幂等的，
//     但 MinIO / R2 在个别版本上会返 404，要显式吞掉）。
//  3. Get 对不存在的 key 必须翻译成 domain.ErrObjectNotFound，
//     不要把 aws smithy 的错误类型漏到 service 层去。
type S3Config struct {
	// Endpoint 例如 https://s3.example.com；MinIO / R2 / OSS 都填这里。
	Endpoint string
	// Region S3 区域，MinIO 之类填 us-east-1 即可。
	Region string
	// Bucket 桶名。
	Bucket string
	// AccessKeyID / SecretAccessKey 凭据。
	//
	// ⚠ 密钥类信息一律由使用者自己在网页界面或环境变量里填，
	// 不代填、不写进代码或配置文件（CLAUDE.md 第五节）。
	AccessKeyID     string
	SecretAccessKey string
	// KeyPrefix 桶内的公共前缀，留空表示直接用 key。
	KeyPrefix string
}

// S3Store 是 domain.ObjectStore 的 S3 兼容实现（**空壳**）。
type S3Store struct {
	// Config 连接参数。字段导出是刻意的：空壳里没有任何方法读它，
	// 未导出的话 staticcheck 会判成 unused。
	Config S3Config
}

// 编译期断言：空壳也必须严格实现同一个接口，
// 这样以后填实现时签名一定是对的。
var _ dkdomain.ObjectStore = (*S3Store)(nil)

// NewS3Store 目前一律返回 ErrS3NotImplemented。
//
// 刻意返回错误而不是返回一个「能构造但一调就报错」的对象：
// 让配置错误在**启动时**炸，而不是在半夜第一张图落盘时炸。
func NewS3Store(cfg S3Config) (*S3Store, error) {
	_ = cfg
	return nil, ErrS3NotImplemented
}

// Put 未实现。
func (s *S3Store) Put(ctx context.Context, key string, contentType string, data []byte) error {
	_, _, _, _ = ctx, key, contentType, data
	return ErrS3NotImplemented
}

// Get 未实现。
func (s *S3Store) Get(ctx context.Context, key string) ([]byte, string, error) {
	_, _ = ctx, key
	return nil, "", ErrS3NotImplemented
}

// Delete 未实现。
func (s *S3Store) Delete(ctx context.Context, key string) error {
	_, _ = ctx, key
	return ErrS3NotImplemented
}

// Exists 未实现。
func (s *S3Store) Exists(ctx context.Context, key string) (bool, error) {
	_, _ = ctx, key
	return false, ErrS3NotImplemented
}
