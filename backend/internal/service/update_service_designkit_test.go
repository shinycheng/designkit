//go:build unit

package service

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// designkit：版本检查已关闭（装配点 ProvideUpdateService 调 DisableRemoteCheck），
// 关闭后 UpdateService 绝不能再访问 GitHub，也不该碰缓存。
// 这两个桩的任何方法被调到都直接判失败。

type dkNoNetworkGitHubClient struct{ t *testing.T }

func (s *dkNoNetworkGitHubClient) FetchLatestRelease(context.Context, string) (*GitHubRelease, error) {
	s.t.Fatal("版本检查已关闭，不应调用 FetchLatestRelease")
	return nil, nil
}

func (s *dkNoNetworkGitHubClient) FetchRecentReleases(context.Context, string, int) ([]*GitHubRelease, error) {
	s.t.Fatal("版本检查已关闭，不应调用 FetchRecentReleases")
	return nil, nil
}

func (s *dkNoNetworkGitHubClient) DownloadFile(context.Context, string, string, int64) error {
	s.t.Fatal("版本检查已关闭，不应调用 DownloadFile")
	return nil
}

func (s *dkNoNetworkGitHubClient) FetchChecksumFile(context.Context, string) ([]byte, error) {
	s.t.Fatal("版本检查已关闭，不应调用 FetchChecksumFile")
	return nil, nil
}

type dkNoTouchUpdateCache struct{ t *testing.T }

func (s *dkNoTouchUpdateCache) GetUpdateInfo(context.Context) (string, error) {
	s.t.Fatal("版本检查已关闭，不应读更新缓存")
	return "", nil
}

func (s *dkNoTouchUpdateCache) SetUpdateInfo(context.Context, string, time.Duration) error {
	s.t.Fatal("版本检查已关闭，不应写更新缓存")
	return nil
}

// 走装配点建服务：ProvideUpdateService 本身就是被测对象——
// 它必须交回一个已经关闭远程检查的 UpdateService。
func newDisabledUpdateService(t *testing.T) *UpdateService {
	return ProvideUpdateService(
		&dkNoTouchUpdateCache{t: t},
		&dkNoNetworkGitHubClient{t: t},
		BuildInfo{Version: "0.1.175", BuildType: "release"},
	)
}

func TestDesignkitUpdateCheckDisabledReturnsCurrentAsLatest(t *testing.T) {
	svc := newDisabledUpdateService(t)

	for _, force := range []bool{false, true} {
		info, err := svc.CheckUpdate(context.Background(), force)
		require.NoError(t, err)
		require.False(t, info.HasUpdate)
		require.Equal(t, "0.1.175", info.CurrentVersion)
		require.Equal(t, "0.1.175", info.LatestVersion)
		require.Equal(t, "release", info.BuildType)
		require.Empty(t, info.Warning)
	}
}

func TestDesignkitUpdateCheckDisabledBlocksPerformUpdate(t *testing.T) {
	svc := newDisabledUpdateService(t)

	err := svc.PerformUpdate(context.Background())

	require.ErrorIs(t, err, ErrNoUpdateAvailable)
}

func TestDesignkitUpdateCheckDisabledRollbackHasNoCandidates(t *testing.T) {
	svc := newDisabledUpdateService(t)

	versions, err := svc.ListRollbackVersions(context.Background())
	require.NoError(t, err)
	require.Empty(t, versions)

	err = svc.RollbackToVersion(context.Background(), "0.1.100")
	require.ErrorIs(t, err, ErrRollbackVersionNotAllowed)
}
