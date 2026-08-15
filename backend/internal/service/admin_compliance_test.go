package service

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

type adminComplianceRepoStub struct {
	values map[string]string
}

func (r *adminComplianceRepoStub) Get(ctx context.Context, key string) (*Setting, error) {
	if value, ok := r.values[key]; ok {
		return &Setting{Key: key, Value: value}, nil
	}
	return nil, ErrSettingNotFound
}

func (r *adminComplianceRepoStub) GetValue(ctx context.Context, key string) (string, error) {
	setting, err := r.Get(ctx, key)
	if err != nil {
		return "", err
	}
	return setting.Value, nil
}

func (r *adminComplianceRepoStub) Set(ctx context.Context, key, value string) error {
	if r.values == nil {
		r.values = map[string]string{}
	}
	r.values[key] = value
	return nil
}

func (r *adminComplianceRepoStub) GetMultiple(ctx context.Context, keys []string) (map[string]string, error) {
	return map[string]string{}, nil
}

func (r *adminComplianceRepoStub) SetMultiple(ctx context.Context, settings map[string]string) error {
	return nil
}

func (r *adminComplianceRepoStub) GetAll(ctx context.Context) (map[string]string, error) {
	return map[string]string{}, nil
}

func (r *adminComplianceRepoStub) Delete(ctx context.Context, key string) error {
	delete(r.values, key)
	return nil
}

// designkit（2026-08-13）：这个用例原来叫 TestAdminComplianceStatusRequiresAckWhenMissing，
// 断言「没有确认记录时 Required=true」。本实例已去掉这道关卡（monica 要求），
// 所以断言反过来：**没有确认记录时也不要求确认**。
//
// 没有删掉这个用例，是因为它守着一件仍然重要的事——
// Required 必须**恒为 false**，不能哪天被谁改回去而没人发现。
// 其余字段（版本号、确认短语、文档路径）照旧断言：确认接口本身还留着。
func TestAdminComplianceStatusNeverRequiresAck(t *testing.T) {
	svc := NewSettingService(&adminComplianceRepoStub{}, &config.Config{})

	status, err := svc.GetAdminComplianceStatus(context.Background(), 1)
	require.NoError(t, err)
	require.False(t, status.Required, "designkit 已去掉合规确认关卡，Required 必须恒为 false")
	require.Equal(t, AdminComplianceVersion, status.Version)
	require.Equal(t, AdminComplianceAckPhraseZH, status.AckPhraseZH)
	require.Equal(t, AdminComplianceDocumentPathZH, status.DocumentPathZH)
}

func TestAcceptAdminComplianceRejectsWrongPhrase(t *testing.T) {
	svc := NewSettingService(&adminComplianceRepoStub{}, &config.Config{})

	_, err := svc.AcceptAdminCompliance(context.Background(), AdminComplianceAcceptInput{
		AdminUserID: 1,
		Language:    "zh",
		Phrase:      "我同意",
	})
	require.Error(t, err)
	require.True(t, errors.Is(err, ErrAdminComplianceInvalidPhrase))
}

func TestAcceptAdminCompliancePersistsCurrentVersion(t *testing.T) {
	repo := &adminComplianceRepoStub{}
	svc := NewSettingService(repo, &config.Config{})

	status, err := svc.AcceptAdminCompliance(context.Background(), AdminComplianceAcceptInput{
		AdminUserID: 42,
		Language:    "zh-CN",
		Phrase:      AdminComplianceAckPhraseZH,
		IPAddress:   "203.0.113.10",
		UserAgent:   "test-agent",
	})
	require.NoError(t, err)
	require.False(t, status.Required)
	require.NotNil(t, status.Acknowledgement)
	require.Equal(t, int64(42), status.Acknowledgement.AdminUserID)
	require.Equal(t, "203.0.113.10", status.Acknowledgement.IPAddress)

	var stored AdminComplianceAcknowledgement
	require.NoError(t, json.Unmarshal([]byte(repo.values[adminComplianceAcknowledgementKey(42)]), &stored))
	require.Equal(t, AdminComplianceVersion, stored.Version)
	require.Equal(t, AdminComplianceDocumentPathZH, stored.DocumentZH)
}

// designkit（2026-08-13）：原来断言「确认记录是旧版本时重新要求确认」。
// 关卡去掉之后 Required 恒为 false，所以这里只保留另一半仍然成立的行为：
// **版本对不上的旧记录不算数**，不会被当成有效确认展示出去。
func TestAdminComplianceOldVersionAckIsNotShown(t *testing.T) {
	old, err := json.Marshal(AdminComplianceAcknowledgement{Version: "v2026.01.01"})
	require.NoError(t, err)
	svc := NewSettingService(&adminComplianceRepoStub{
		values: map[string]string{adminComplianceAcknowledgementKey(1): string(old)},
	}, &config.Config{})

	status, err := svc.GetAdminComplianceStatus(context.Background(), 1)
	require.NoError(t, err)
	require.False(t, status.Required, "designkit 已去掉合规确认关卡")
	require.Nil(t, status.Acknowledgement, "版本对不上的旧记录不该被当成有效确认")
}

func TestAdminComplianceStatusIsPerAdminUser(t *testing.T) {
	current, err := json.Marshal(AdminComplianceAcknowledgement{
		Version:     AdminComplianceVersion,
		AdminUserID: 1,
	})
	require.NoError(t, err)
	svc := NewSettingService(&adminComplianceRepoStub{
		values: map[string]string{adminComplianceAcknowledgementKey(1): string(current)},
	}, &config.Config{})

	statusForUserOne, err := svc.GetAdminComplianceStatus(context.Background(), 1)
	require.NoError(t, err)
	require.False(t, statusForUserOne.Required)
	require.NotNil(t, statusForUserOne.Acknowledgement, "自己的确认记录应该读得到")

	// designkit：关卡去掉后两个人都不要求确认；这里断言的是另一半——
	// 确认记录仍然是**按管理员分开存的**，不会把 1 号的记录串到 2 号头上。
	statusForUserTwo, err := svc.GetAdminComplianceStatus(context.Background(), 2)
	require.NoError(t, err)
	require.False(t, statusForUserTwo.Required)
	require.Nil(t, statusForUserTwo.Acknowledgement, "不该拿到别人的确认记录")
}
