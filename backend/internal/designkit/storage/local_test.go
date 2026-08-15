//go:build unit

package storage

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	dkdomain "github.com/Wei-Shaw/sub2api/internal/designkit/domain"

	"github.com/stretchr/testify/require"
)

func newTestStore(t *testing.T) *LocalStore {
	t.Helper()
	store, err := NewLocalStore(t.TempDir())
	require.NoError(t, err)
	return store
}

func TestLocalStore_PutGetDeleteExists(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)

	key := dkdomain.ImageObjectKey(time.Date(2026, 8, 13, 1, 2, 3, 0, time.UTC), "01J8ZK7Q9X2M4N6P8R0T2V4W6Y", 1, 1, 1)
	require.Equal(t, "designkit/2026/08/13/01J8ZK7Q9X2M4N6P8R0T2V4W6Y-1-1-1.png", key)

	exists, err := store.Exists(ctx, key)
	require.NoError(t, err)
	require.False(t, exists)

	_, _, err = store.Get(ctx, key)
	require.ErrorIs(t, err, dkdomain.ErrObjectNotFound)

	// 不存在的 key 删除必须是幂等的，不能报错。
	require.NoError(t, store.Delete(ctx, key))

	payload := []byte("fake-png-bytes")
	require.NoError(t, store.Put(ctx, key, "image/png", payload))

	exists, err = store.Exists(ctx, key)
	require.NoError(t, err)
	require.True(t, exists)

	got, contentType, err := store.Get(ctx, key)
	require.NoError(t, err)
	require.Equal(t, payload, got)
	require.Equal(t, "image/png", contentType)

	// 覆盖写（重试出图会覆盖同一张）。
	require.NoError(t, store.Put(ctx, key, "image/png", []byte("second")))
	got, _, err = store.Get(ctx, key)
	require.NoError(t, err)
	require.Equal(t, []byte("second"), got)

	require.NoError(t, store.Delete(ctx, key))
	exists, err = store.Exists(ctx, key)
	require.NoError(t, err)
	require.False(t, exists)
}

// TestLocalStore_RejectsTraversal 是这一层最重要的一个测试。
// key 可能来自 ERP 传进来的参数，漏一个就是整台 NAS 被读出去。
func TestLocalStore_RejectsTraversal(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)

	// 依次覆盖：相对路径穿越、绝对路径、空/重复路径段、反斜杠、冒号、
	// 空字节截断、换行、隐藏文件、撞临时文件前缀、空格、非 ASCII、空 key、超长。
	badKeys := []string{
		"../../etc/passwd",
		"..",
		"../secret.png",
		"designkit/../../etc/passwd",
		"designkit/../../../root/.ssh/id_rsa",
		"/etc/passwd",
		"/",
		"designkit//a.png",
		"designkit/./a.png",
		"designkit/a/",
		`designkit\..\..\windows\system32`,
		"designkit/a:b.png",
		"designkit/\x00a.png",
		"designkit/a\nb.png",
		"designkit/.ssh/id_rsa",
		"designkit/.dktmp-123",
		"designkit/a .png",
		"designkit/中文.png",
		"",
		strings.Repeat("a", MaxObjectKeyLen+1),
	}

	for _, key := range badKeys {
		t.Run(strings.ReplaceAll(key, "\x00", "<nul>"), func(t *testing.T) {
			err := store.Put(ctx, key, "image/png", []byte("payload"))
			require.ErrorIs(t, err, ErrInvalidKey, "Put 必须拒绝 %q", key)

			_, _, err = store.Get(ctx, key)
			require.ErrorIs(t, err, ErrInvalidKey, "Get 必须拒绝 %q", key)

			err = store.Delete(ctx, key)
			require.ErrorIs(t, err, ErrInvalidKey, "Delete 必须拒绝 %q", key)

			_, err = store.Exists(ctx, key)
			require.ErrorIs(t, err, ErrInvalidKey, "Exists 必须拒绝 %q", key)
		})
	}
}

// TestLocalStore_TraversalWritesNothingOutsideRoot 从结果侧再验一遍：
// 就算规则被改坏了，根目录之外也不该多出任何文件。
func TestLocalStore_TraversalWritesNothingOutsideRoot(t *testing.T) {
	ctx := context.Background()
	parent := t.TempDir()
	root := filepath.Join(parent, "data")
	store, err := NewLocalStore(root)
	require.NoError(t, err)

	err = store.Put(ctx, "../escaped.png", "image/png", []byte("payload"))
	require.Error(t, err)

	entries, err := os.ReadDir(parent)
	require.NoError(t, err)
	require.Len(t, entries, 1, "根目录之外不允许多出任何文件")
	require.Equal(t, "data", entries[0].Name())
}

// TestLocalStore_RejectsSymlink 软链是路径穿越的第二条路：
// 字符串比对完全看不出来，必须逐级 Lstat。
func TestLocalStore_RejectsSymlink(t *testing.T) {
	ctx := context.Background()
	base := t.TempDir()
	root := filepath.Join(base, "root")
	outside := filepath.Join(base, "outside")
	require.NoError(t, os.MkdirAll(filepath.Join(root, "designkit"), 0o755))
	require.NoError(t, os.MkdirAll(outside, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(outside, "secret.png"), []byte("TOP-SECRET"), 0o600))

	store, err := NewLocalStore(root)
	require.NoError(t, err)

	// 目录软链：<root>/designkit/link -> <base>/outside
	require.NoError(t, os.Symlink(outside, filepath.Join(root, "designkit", "link")))
	_, _, err = store.Get(ctx, "designkit/link/secret.png")
	require.ErrorIs(t, err, ErrInvalidKey)
	require.Error(t, store.Put(ctx, "designkit/link/written.png", "image/png", []byte("x")))
	require.NoFileExists(t, filepath.Join(outside, "written.png"))

	// 文件软链：<root>/designkit/leak.png -> <base>/outside/secret.png
	require.NoError(t, os.Symlink(filepath.Join(outside, "secret.png"), filepath.Join(root, "designkit", "leak.png")))
	_, _, err = store.Get(ctx, "designkit/leak.png")
	require.ErrorIs(t, err, ErrInvalidKey)

	exists, err := store.Exists(ctx, "designkit/leak.png")
	require.ErrorIs(t, err, ErrInvalidKey)
	require.False(t, exists)
}

// TestLocalStore_SymlinkedRootIsFine 根目录自己是软链要放行——
// 群晖上 /app/data 挂过去常常就是一条软链。
func TestLocalStore_SymlinkedRootIsFine(t *testing.T) {
	ctx := context.Background()
	base := t.TempDir()
	real := filepath.Join(base, "real")
	require.NoError(t, os.MkdirAll(real, 0o755))
	link := filepath.Join(base, "link")
	require.NoError(t, os.Symlink(real, link))

	store, err := NewLocalStore(link)
	require.NoError(t, err)
	require.NoError(t, store.Put(ctx, "designkit/a.png", "image/png", []byte("ok")))

	got, _, err := store.Get(ctx, "designkit/a.png")
	require.NoError(t, err)
	require.Equal(t, []byte("ok"), got)
}

// TestLocalStore_PutLeavesNoTempFile 写入是「临时文件 + rename」，
// 成功之后目录里不能留下临时文件。
func TestLocalStore_PutLeavesNoTempFile(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	key := "designkit/2026/08/13/a-1-1-1.png"
	require.NoError(t, store.Put(ctx, key, "image/png", []byte("payload")))

	entries, err := os.ReadDir(filepath.Join(store.Root(), "designkit", "2026", "08", "13"))
	require.NoError(t, err)
	require.Len(t, entries, 1)
	require.Equal(t, "a-1-1-1.png", entries[0].Name())
	require.False(t, strings.HasPrefix(entries[0].Name(), tmpPrefix))
}

func TestLocalStore_FromEnv(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "nested", "designkit")
	t.Setenv(EnvStorageDir, dir)

	store, err := NewLocalStoreFromEnv()
	require.NoError(t, err)
	resolved, err := filepath.EvalSymlinks(dir)
	require.NoError(t, err)
	require.Equal(t, resolved, store.Root())
	require.DirExists(t, dir)
}

func TestLocalStore_ContextCancelled(t *testing.T) {
	store := newTestStore(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := store.Put(ctx, "designkit/a.png", "image/png", []byte("x"))
	require.ErrorIs(t, err, context.Canceled)
}

func TestContentTypeForKey(t *testing.T) {
	require.Equal(t, "image/png", ContentTypeForKey("designkit/a.png", nil))
	require.Equal(t, "image/jpeg", ContentTypeForKey("designkit/a.JPG", nil))
	require.Equal(t, "image/webp", ContentTypeForKey("designkit/a.webp", nil))
	require.Equal(t, "image/heic", ContentTypeForKey("designkit/a.heic", nil))
	require.Equal(t, "application/octet-stream", ContentTypeForKey("designkit/a", nil))
	// 扩展名认不出来时按字节嗅探。
	require.True(t, strings.HasPrefix(ContentTypeForKey("designkit/a", []byte("plain text")), "text/plain"))
}

func TestValidateKey_AcceptsRealKeys(t *testing.T) {
	now := time.Date(2026, 8, 13, 0, 0, 0, 0, time.UTC)
	uid := "01J8ZK7Q9X2M4N6P8R0T2V4W6Y"

	keys := []string{
		dkdomain.ImageObjectKey(now, uid, 1, 1, 1),
		dkdomain.ImageObjectKey(now, uid, 9999, 99, 4),
		dkdomain.AssetObjectKey(now, uid, ".jpg"),
		dkdomain.AssetObjectKey(now, uid, ".heic"),
		dkdomain.VariantObjectKey(now, uid, dkdomain.Ratio3x4, false, 2048, ".jpg"),
		dkdomain.VariantObjectKey(now, uid, dkdomain.Ratio16x9, true, 4096, ".png"),
	}
	for _, key := range keys {
		require.NoError(t, ValidateKey(key), "真实 key 必须被放行：%s", key)
		require.NotContains(t, key, ":", "object key 里不能出现冒号")
	}
}

func TestS3StoreIsStillAStub(t *testing.T) {
	store, err := NewS3Store(S3Config{Bucket: "x"})
	require.Nil(t, store)
	require.ErrorIs(t, err, ErrS3NotImplemented)

	var empty S3Store
	require.ErrorIs(t, empty.Put(context.Background(), "designkit/a.png", "image/png", nil), ErrS3NotImplemented)
	_, _, err = empty.Get(context.Background(), "designkit/a.png")
	require.ErrorIs(t, err, ErrS3NotImplemented)
	require.ErrorIs(t, empty.Delete(context.Background(), "designkit/a.png"), ErrS3NotImplemented)
	_, err = empty.Exists(context.Background(), "designkit/a.png")
	require.ErrorIs(t, err, ErrS3NotImplemented)
}
