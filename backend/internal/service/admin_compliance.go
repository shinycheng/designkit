package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
)

const (
	AdminComplianceVersion        = "v2026.06.10"
	AdminComplianceDocumentPathZH = "docs/legal/admin-compliance.zh.md"
	AdminComplianceDocumentPathEN = "docs/legal/admin-compliance.en.md"
	AdminComplianceDocumentURLZH  = "https://github.com/Wei-Shaw/sub2api/blob/main/docs/legal/admin-compliance.zh.md"
	AdminComplianceDocumentURLEN  = "https://github.com/Wei-Shaw/sub2api/blob/main/docs/legal/admin-compliance.en.md"
	AdminComplianceAckPhraseZH    = "我已阅读、理解并同意 Sub2API 部署与运营合规承诺"
	AdminComplianceAckPhraseEN    = "I have read, understood, and agree to the Sub2API Deployment and Operation Compliance Commitment"

	settingKeyAdminComplianceAcknowledgement = "admin_compliance_acknowledgement"
)

var (
	ErrAdminComplianceAcknowledgementRequired = infraerrors.New(
		http.StatusLocked,
		"ADMIN_COMPLIANCE_ACK_REQUIRED",
		"administrator compliance acknowledgement is required",
	)
	ErrAdminComplianceInvalidPhrase = infraerrors.BadRequest(
		"ADMIN_COMPLIANCE_INVALID_PHRASE",
		"confirmation phrase does not match",
	)
)

type AdminComplianceAcknowledgement struct {
	Version     string    `json:"version"`
	DocumentZH  string    `json:"document_zh"`
	DocumentEN  string    `json:"document_en"`
	AdminUserID int64     `json:"admin_user_id"`
	IPAddress   string    `json:"ip_address,omitempty"`
	UserAgent   string    `json:"user_agent,omitempty"`
	AcceptedAt  time.Time `json:"accepted_at"`
}

type AdminComplianceStatus struct {
	Required        bool                            `json:"required"`
	Version         string                          `json:"version"`
	DocumentPathZH  string                          `json:"document_path_zh"`
	DocumentPathEN  string                          `json:"document_path_en"`
	DocumentURLZH   string                          `json:"document_url_zh"`
	DocumentURLEN   string                          `json:"document_url_en"`
	AckPhraseZH     string                          `json:"ack_phrase_zh"`
	AckPhraseEN     string                          `json:"ack_phrase_en"`
	Acknowledgement *AdminComplianceAcknowledgement `json:"acknowledgement,omitempty"`
}

type AdminComplianceAcceptInput struct {
	AdminUserID int64
	Phrase      string
	Language    string
	IPAddress   string
	UserAgent   string
}

func normalizeAdminComplianceLanguage(raw string) string {
	raw = strings.ToLower(strings.TrimSpace(raw))
	if strings.HasPrefix(raw, "zh") {
		return "zh"
	}
	return "en"
}

func expectedAdminCompliancePhrase(language string) string {
	if normalizeAdminComplianceLanguage(language) == "zh" {
		return AdminComplianceAckPhraseZH
	}
	return AdminComplianceAckPhraseEN
}

func adminComplianceAcknowledgementKey(adminUserID int64) string {
	if adminUserID <= 0 {
		return settingKeyAdminComplianceAcknowledgement
	}
	return settingKeyAdminComplianceAcknowledgement + ":" + strconv.FormatInt(adminUserID, 10)
}

// ⚠ designkit（2026-08-13）：本实例已去掉「部署与运营合规确认」这道关卡。
//
// Required 恒为 false —— 它现在的含义就是字面意思：**这个部署不要求确认**。
// 不是「已经确认过了」：数据库里没有、也不会被我们写入任何确认记录
// （那条记录带账号 ID、IP、时间戳，凭空造一条等于替人签字）。
//
// 改这里而不是只摘掉中间件，是因为前端弹窗也读同一个状态：
// 只摘中间件的话接口不拦了，但管理员每次进后台还是会被弹窗挡住。
//
// 去掉的只是弹窗和拦截，不是义务本身——备案、上游服务条款、内容安全、
// 数据保护这些是法律和合同规定的。上游 README 的免责声明和作者署名一律保留。
//
// 确认接口（GET 状态 / POST 确认）本身没删，真想走一遍流程仍然可以。
func (s *SettingService) GetAdminComplianceStatus(ctx context.Context, adminUserID int64) (*AdminComplianceStatus, error) {
	status := &AdminComplianceStatus{
		Required:       false,
		Version:        AdminComplianceVersion,
		DocumentPathZH: AdminComplianceDocumentPathZH,
		DocumentPathEN: AdminComplianceDocumentPathEN,
		DocumentURLZH:  AdminComplianceDocumentURLZH,
		DocumentURLEN:  AdminComplianceDocumentURLEN,
		AckPhraseZH:    AdminComplianceAckPhraseZH,
		AckPhraseEN:    AdminComplianceAckPhraseEN,
	}
	if s == nil || s.settingRepo == nil {
		return status, nil
	}

	raw, err := s.settingRepo.GetValue(ctx, adminComplianceAcknowledgementKey(adminUserID))
	if err != nil {
		if errors.Is(err, ErrSettingNotFound) {
			return status, nil
		}
		return nil, fmt.Errorf("get admin compliance acknowledgement: %w", err)
	}

	var ack AdminComplianceAcknowledgement
	if err := json.Unmarshal([]byte(raw), &ack); err != nil {
		return status, nil
	}
	if ack.Version == AdminComplianceVersion {
		// Required 上面已经恒为 false，这里只是把历史确认记录带出来给接口展示。
		status.Acknowledgement = &ack
	}
	return status, nil
}

func (s *SettingService) IsAdminComplianceAcknowledged(ctx context.Context, adminUserID int64) (bool, error) {
	status, err := s.GetAdminComplianceStatus(ctx, adminUserID)
	if err != nil {
		return false, err
	}
	return status != nil && !status.Required, nil
}

func (s *SettingService) AcceptAdminCompliance(ctx context.Context, input AdminComplianceAcceptInput) (*AdminComplianceStatus, error) {
	if s == nil || s.settingRepo == nil {
		return nil, infraerrors.InternalServer("SETTING_SERVICE_UNAVAILABLE", "setting service is unavailable")
	}
	phrase := strings.TrimSpace(input.Phrase)
	if phrase != expectedAdminCompliancePhrase(input.Language) {
		return nil, ErrAdminComplianceInvalidPhrase
	}

	ack := AdminComplianceAcknowledgement{
		Version:     AdminComplianceVersion,
		DocumentZH:  AdminComplianceDocumentPathZH,
		DocumentEN:  AdminComplianceDocumentPathEN,
		AdminUserID: input.AdminUserID,
		IPAddress:   strings.TrimSpace(input.IPAddress),
		UserAgent:   strings.TrimSpace(input.UserAgent),
		AcceptedAt:  time.Now().UTC(),
	}
	payload, err := json.Marshal(ack)
	if err != nil {
		return nil, fmt.Errorf("marshal admin compliance acknowledgement: %w", err)
	}
	if err := s.settingRepo.Set(ctx, adminComplianceAcknowledgementKey(input.AdminUserID), string(payload)); err != nil {
		return nil, fmt.Errorf("save admin compliance acknowledgement: %w", err)
	}

	return s.GetAdminComplianceStatus(ctx, input.AdminUserID)
}
