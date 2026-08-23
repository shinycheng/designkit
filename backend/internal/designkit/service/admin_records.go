package service

// admin_records.go —— 「用户记录」（管理端）的薄编排。
//
// 列表是仓储的直通车，详情是「本体 + 子表」的两趟组装，取图字节复用
// JobService.OpenJobItemContent 同一条路（item → 当前版本的图 → store.Get）。
//
// **这一层刻意不做归属校验**：它就是给管理员跨用户看的。
// 挡普通用户的责任在路由层（register_business.go 的 RequireAdmin），
// 单测守着「非管理员 403、service 一次都不被调到」这条线。
//
// 软删过滤在仓储的 SQL 里（会话 deleted_at IS NULL、批次 user_deleted_at
// IS NULL）：管理员看到的跟用户自己看到的一致，不把删掉的翻出来。

import (
	"context"
	"errors"

	dkdomain "github.com/Wei-Shaw/sub2api/internal/designkit/domain"
)

// AdminRecordsStore 是本服务对持久层的全部要求。
// repository.AdminRecordsRepo 实现了它；单测塞假实现（不碰真数据库）。
type AdminRecordsStore interface {
	ListRecordUsers(ctx context.Context) ([]*dkdomain.RecordUser, error)

	ListChatSessions(ctx context.Context, userID int64, limit, offset int) ([]*dkdomain.ChatSessionAdminView, error)
	GetChatSessionByUID(ctx context.Context, uid string) (*dkdomain.ChatSessionAdminView, error)
	ListChatMessages(ctx context.Context, sessionID int64) ([]*dkdomain.ChatMessage, error)

	ListJobs(ctx context.Context, userID int64, limit, offset int) ([]*dkdomain.JobAdminView, error)
	GetJobByUID(ctx context.Context, uid string) (*dkdomain.JobAdminView, error)
	ListJobItemsWithImageFlag(ctx context.Context, jobID int64) ([]*dkdomain.JobItemAdminView, error)
	GetJobItemBySeq(ctx context.Context, jobID int64, seq int) (*dkdomain.JobItem, error)
	ListCurrentImagesByItem(ctx context.Context, itemID int64) ([]*dkdomain.Image, error)
}

// AdminRecordsDeps 装配 AdminRecordsService 要的东西。
type AdminRecordsDeps struct {
	// Records 持久层。必填。
	Records AdminRecordsStore
	// Store 对象存储，只有「取结果图字节」用它。
	// **允许为 nil**：缺席时列表和详情照常，取图返回中文的存储错误。
	// ⚠ typed-nil：装配处必须只在非 nil 时赋值（module.go 的老规矩）。
	Store dkdomain.ObjectStore
}

// AdminRecordsService 「用户记录」的业务实现。
type AdminRecordsService struct {
	records AdminRecordsStore
	store   dkdomain.ObjectStore
}

// NewAdminRecordsService 装配。持久层缺席就报错 —— 启动期就该炸。
func NewAdminRecordsService(deps AdminRecordsDeps) (*AdminRecordsService, error) {
	if deps.Records == nil {
		return nil, errors.New("designkit: 用户记录服务缺持久层")
	}
	return &AdminRecordsService{records: deps.Records, store: deps.Store}, nil
}

// ListUsers 有记录的账户（给筛选下拉用）。
func (s *AdminRecordsService) ListUsers(ctx context.Context) ([]*dkdomain.RecordUser, error) {
	users, err := s.records.ListRecordUsers(ctx)
	if err != nil {
		return nil, mapJobRepoError(err, dkdomain.ErrCodeInternal)
	}
	return users, nil
}

// ListChatSessions 会话列表。userID=0 看全部账户。
func (s *AdminRecordsService) ListChatSessions(ctx context.Context, userID int64, limit, offset int) ([]*dkdomain.ChatSessionAdminView, error) {
	sessions, err := s.records.ListChatSessions(ctx, userID, limit, offset)
	if err != nil {
		return nil, mapJobRepoError(err, dkdomain.ErrCodeChatSessionNotFound)
	}
	return sessions, nil
}

// GetChatSession 一个会话 + 它的全部消息（按 id 升序，仓储的 SQL 定死的）。
func (s *AdminRecordsService) GetChatSession(ctx context.Context, uid string) (*dkdomain.ChatSessionAdminView, []*dkdomain.ChatMessage, error) {
	session, err := s.records.GetChatSessionByUID(ctx, uid)
	if err != nil {
		return nil, nil, mapJobRepoError(err, dkdomain.ErrCodeChatSessionNotFound)
	}
	if session == nil {
		return nil, nil, dkdomain.NewError(dkdomain.ErrCodeChatSessionNotFound)
	}
	messages, err := s.records.ListChatMessages(ctx, session.ID)
	if err != nil {
		return nil, nil, mapJobRepoError(err, dkdomain.ErrCodeChatSessionNotFound)
	}
	return session, messages, nil
}

// ListJobs 批次列表。userID=0 看全部账户。
func (s *AdminRecordsService) ListJobs(ctx context.Context, userID int64, limit, offset int) ([]*dkdomain.JobAdminView, error) {
	jobs, err := s.records.ListJobs(ctx, userID, limit, offset)
	if err != nil {
		return nil, mapJobRepoError(err, dkdomain.ErrCodeJobNotFound)
	}
	return jobs, nil
}

// GetJob 一个批次 + 它的每一张（按 seq 升序，仓储的 SQL 定死的）。
func (s *AdminRecordsService) GetJob(ctx context.Context, uid string) (*dkdomain.JobAdminView, []*dkdomain.JobItemAdminView, error) {
	job, err := s.records.GetJobByUID(ctx, uid)
	if err != nil {
		return nil, nil, mapJobRepoError(err, dkdomain.ErrCodeJobNotFound)
	}
	if job == nil {
		return nil, nil, dkdomain.NewError(dkdomain.ErrCodeJobNotFound)
	}
	items, err := s.records.ListJobItemsWithImageFlag(ctx, job.ID)
	if err != nil {
		return nil, nil, mapJobRepoError(err, dkdomain.ErrCodeItemNotFound)
	}
	return job, items, nil
}

// OpenJobItemContent 取某一张当前版本第一张结果图的字节（缩略图用）。
//
// 走跟 JobService.OpenJobItemContent 相同的路：批次 → item → 当前版本的图 →
// store.Get。「还没出好」和「出了但没有图」分开说，跟用户侧同一套文案口径。
func (s *AdminRecordsService) OpenJobItemContent(ctx context.Context, jobUID string, seq int) ([]byte, string, error) {
	job, err := s.records.GetJobByUID(ctx, jobUID)
	if err != nil {
		return nil, "", mapJobRepoError(err, dkdomain.ErrCodeJobNotFound)
	}
	if job == nil {
		return nil, "", dkdomain.NewError(dkdomain.ErrCodeJobNotFound)
	}
	if seq <= 0 {
		// seq 从 1 开始是对外契约的一部分，0 和负数一律当「没有这一张」。
		return nil, "", dkdomain.NewError(dkdomain.ErrCodeItemNotFound)
	}

	item, err := s.records.GetJobItemBySeq(ctx, job.ID, seq)
	if err != nil {
		return nil, "", mapJobRepoError(err, dkdomain.ErrCodeItemNotFound)
	}
	if item == nil {
		return nil, "", dkdomain.NewError(dkdomain.ErrCodeItemNotFound)
	}

	images, err := s.records.ListCurrentImagesByItem(ctx, item.ID)
	if err != nil {
		return nil, "", mapJobRepoError(err, dkdomain.ErrCodeImageNotFound)
	}
	var target *dkdomain.Image
	for _, img := range images {
		if img != nil {
			target = img // 已按 image_index 升序，第一张就是「第 1 张」
			break
		}
	}
	if target == nil {
		if !item.Status.IsTerminal() {
			return nil, "", dkdomain.NewError(dkdomain.ErrCodeImageNotFound).
				WithMessage("这一张还没出好，请等它跑完再看。")
		}
		return nil, "", dkdomain.NewError(dkdomain.ErrCodeImageNotFound)
	}

	if s.store == nil {
		return nil, "", dkdomain.NewError(dkdomain.ErrCodeStorageError).
			WithMessage("图片存储没有配置好，暂时取不到图，请联系管理员。")
	}
	data, contentType, err := s.store.Get(ctx, target.ObjectKey)
	if err != nil {
		// 库里有记录、存储里没文件不是「找不到」，是存储出了问题（同用户侧的口径）。
		return nil, "", dkdomain.NewError(dkdomain.ErrCodeStorageError).WithCause(err)
	}
	if contentType == "" {
		contentType = target.ContentType
	}
	return data, contentType, nil
}
