//go:build unit

package service

// 「用户记录」（管理端）薄编排的单测：详情组装、取图字节那条路的每一种失败形态。
// 不碰真数据库和对象存储（CLAUDE.md 第三节）。

import (
	"context"
	"errors"
	"testing"

	dkdomain "github.com/Wei-Shaw/sub2api/internal/designkit/domain"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---- 假持久层 ----

type fakeAdminRecordsStore struct {
	users []*dkdomain.RecordUser

	sessions     []*dkdomain.ChatSessionAdminView
	lastSessions struct {
		userID        int64
		limit, offset int
	}

	session    *dkdomain.ChatSessionAdminView
	sessionErr error
	messages   []*dkdomain.ChatMessage

	jobs []*dkdomain.JobAdminView

	job    *dkdomain.JobAdminView
	jobErr error
	items  []*dkdomain.JobItemAdminView

	item          *dkdomain.JobItem
	itemErr       error
	lastItemQuery struct {
		jobID int64
		seq   int
	}

	images     []*dkdomain.Image
	lastItemID int64

	asset        *dkdomain.Asset
	assetErr     error
	lastAssetUID string
}

func (f *fakeAdminRecordsStore) ListRecordUsers(_ context.Context) ([]*dkdomain.RecordUser, error) {
	return f.users, nil
}

func (f *fakeAdminRecordsStore) ListChatSessions(_ context.Context, userID int64, limit, offset int) ([]*dkdomain.ChatSessionAdminView, error) {
	f.lastSessions.userID = userID
	f.lastSessions.limit = limit
	f.lastSessions.offset = offset
	return f.sessions, nil
}

func (f *fakeAdminRecordsStore) GetChatSessionByUID(_ context.Context, _ string) (*dkdomain.ChatSessionAdminView, error) {
	if f.sessionErr != nil {
		return nil, f.sessionErr
	}
	return f.session, nil
}

func (f *fakeAdminRecordsStore) ListChatMessages(_ context.Context, _ int64) ([]*dkdomain.ChatMessage, error) {
	return f.messages, nil
}

func (f *fakeAdminRecordsStore) ListJobs(_ context.Context, _ int64, _, _ int) ([]*dkdomain.JobAdminView, error) {
	return f.jobs, nil
}

func (f *fakeAdminRecordsStore) GetJobByUID(_ context.Context, _ string) (*dkdomain.JobAdminView, error) {
	if f.jobErr != nil {
		return nil, f.jobErr
	}
	return f.job, nil
}

func (f *fakeAdminRecordsStore) ListJobItemsWithImageFlag(_ context.Context, _ int64) ([]*dkdomain.JobItemAdminView, error) {
	return f.items, nil
}

func (f *fakeAdminRecordsStore) GetJobItemBySeq(_ context.Context, jobID int64, seq int) (*dkdomain.JobItem, error) {
	f.lastItemQuery.jobID = jobID
	f.lastItemQuery.seq = seq
	if f.itemErr != nil {
		return nil, f.itemErr
	}
	return f.item, nil
}

func (f *fakeAdminRecordsStore) ListCurrentImagesByItem(_ context.Context, itemID int64) ([]*dkdomain.Image, error) {
	f.lastItemID = itemID
	return f.images, nil
}

func (f *fakeAdminRecordsStore) GetAssetByUID(_ context.Context, uid string) (*dkdomain.Asset, error) {
	f.lastAssetUID = uid
	if f.assetErr != nil {
		return nil, f.assetErr
	}
	return f.asset, nil
}

// ---- 假对象存储 ----

type fakeRecordsObjectStore struct {
	data        []byte
	contentType string
	getErr      error
	lastKey     string
}

func (f *fakeRecordsObjectStore) Put(_ context.Context, _ string, _ string, _ []byte) error {
	return nil
}

func (f *fakeRecordsObjectStore) Get(_ context.Context, key string) ([]byte, string, error) {
	f.lastKey = key
	if f.getErr != nil {
		return nil, "", f.getErr
	}
	return f.data, f.contentType, nil
}

func (f *fakeRecordsObjectStore) Delete(_ context.Context, _ string) error { return nil }

func (f *fakeRecordsObjectStore) Exists(_ context.Context, _ string) (bool, error) {
	return true, nil
}

func newRecordsService(t *testing.T, store *fakeAdminRecordsStore, objects dkdomain.ObjectStore) *AdminRecordsService {
	t.Helper()
	deps := AdminRecordsDeps{Records: store}
	if objects != nil {
		deps.Store = objects
	}
	svc, err := NewAdminRecordsService(deps)
	require.NoError(t, err)
	return svc
}

func assertRecordsErrCode(t *testing.T, err error, wantCode string) {
	t.Helper()
	require.Error(t, err)
	dkErr, ok := dkdomain.AsDesignkitError(err)
	require.True(t, ok, "必须是带我方错误码的 DesignkitError: %v", err)
	assert.Equal(t, wantCode, dkErr.Code)
}

// ---- 装配 ----

// 持久层缺席就报错 —— 启动期就该炸，别拖到管理员点的时候。
func TestNewAdminRecordsServiceRequiresStore(t *testing.T) {
	_, err := NewAdminRecordsService(AdminRecordsDeps{})
	require.Error(t, err)
}

// ---- 列表透传 ----

// user_id 筛选原样透传到持久层（0 = 全部账户也照传，不在这一层偷换语义）。
func TestAdminRecordsListPassesFilterThrough(t *testing.T) {
	store := &fakeAdminRecordsStore{}
	svc := newRecordsService(t, store, nil)

	_, err := svc.ListChatSessions(context.Background(), 42, 10, 5)
	require.NoError(t, err)
	assert.Equal(t, int64(42), store.lastSessions.userID)
	assert.Equal(t, 10, store.lastSessions.limit)
	assert.Equal(t, 5, store.lastSessions.offset)

	_, err = svc.ListChatSessions(context.Background(), 0, 50, 0)
	require.NoError(t, err)
	assert.Equal(t, int64(0), store.lastSessions.userID, "0（全部账户）也要原样透传")
}

// ---- 会话详情 ----

// 详情 = 本体 + 消息，消息由会话的内部 id 关联。
func TestGetChatSessionAssemblesMessages(t *testing.T) {
	store := &fakeAdminRecordsStore{
		session: &dkdomain.ChatSessionAdminView{
			ChatSession: dkdomain.ChatSession{ID: 11, UID: "01J8ZK7Q9X2M4N6P8R0T2V4W6Y"},
			UserEmail:   "yunying@example.com",
		},
		messages: []*dkdomain.ChatMessage{
			{ID: 1, Role: dkdomain.ChatRoleUser, Content: "在吗"},
			{ID: 2, Role: dkdomain.ChatRoleAssistant, Content: "在"},
		},
	}
	svc := newRecordsService(t, store, nil)

	session, messages, err := svc.GetChatSession(context.Background(), "01J8ZK7Q9X2M4N6P8R0T2V4W6Y")
	require.NoError(t, err)
	require.NotNil(t, session)
	assert.Equal(t, "yunying@example.com", session.UserEmail)
	require.Len(t, messages, 2)
}

// 仓储的 ErrNotFound 翻成我方错误码（会话不存在 / 已被用户删掉，同一个形态）。
func TestGetChatSessionTranslatesNotFound(t *testing.T) {
	store := &fakeAdminRecordsStore{
		sessionErr: dkdomain.ErrNotFound,
	}
	svc := newRecordsService(t, store, nil)

	_, _, err := svc.GetChatSession(context.Background(), "01J8ZK7Q9X2M4N6P8R0T2V4W6Y")
	assertRecordsErrCode(t, err, dkdomain.ErrCodeChatSessionNotFound)
}

// ---- 取图字节 ----

func recordsTestJob() *dkdomain.JobAdminView {
	return &dkdomain.JobAdminView{Job: dkdomain.Job{ID: 21, UID: "01J8ZK7Q9X2M4N6P8R0T2V4W6Y"}}
}

// 正常路：批次 → item → 当前版本第一张图 → store.Get(object_key)。
func TestOpenJobItemContentReadsFirstCurrentImage(t *testing.T) {
	store := &fakeAdminRecordsStore{
		job:  recordsTestJob(),
		item: &dkdomain.JobItem{ID: 31, Seq: 3, Status: dkdomain.ItemStatusSucceeded},
		images: []*dkdomain.Image{
			{ID: 41, ImageIndex: 1, ObjectKey: "designkit/2026/08/23/x-3-1-1.png", ContentType: "image/png"},
			{ID: 42, ImageIndex: 2, ObjectKey: "designkit/2026/08/23/x-3-1-2.png", ContentType: "image/png"},
		},
	}
	objects := &fakeRecordsObjectStore{data: []byte{0x89, 'P', 'N', 'G'}, contentType: "image/png"}
	svc := newRecordsService(t, store, objects)

	data, contentType, err := svc.OpenJobItemContent(context.Background(), "01J8ZK7Q9X2M4N6P8R0T2V4W6Y", 3)
	require.NoError(t, err)
	assert.Equal(t, []byte{0x89, 'P', 'N', 'G'}, data)
	assert.Equal(t, "image/png", contentType)
	assert.Equal(t, int64(21), store.lastItemQuery.jobID, "item 要按批次的内部 id 查")
	assert.Equal(t, 3, store.lastItemQuery.seq)
	assert.Equal(t, int64(31), store.lastItemID, "图要按 item 的内部 id 查")
	assert.Equal(t, "designkit/2026/08/23/x-3-1-1.png", objects.lastKey,
		"必须取 image_index 最小的那张（列表已按 index 升序）")
}

// 批次不存在 → DK_JOB_NOT_FOUND；seq 不合法 → DK_ITEM_NOT_FOUND。
func TestOpenJobItemContentNotFoundShapes(t *testing.T) {
	store := &fakeAdminRecordsStore{jobErr: dkdomain.ErrNotFound}
	svc := newRecordsService(t, store, &fakeRecordsObjectStore{})

	_, _, err := svc.OpenJobItemContent(context.Background(), "01J8ZK7Q9X2M4N6P8R0T2V4W6Y", 1)
	assertRecordsErrCode(t, err, dkdomain.ErrCodeJobNotFound)

	store = &fakeAdminRecordsStore{job: recordsTestJob()}
	svc = newRecordsService(t, store, &fakeRecordsObjectStore{})
	_, _, err = svc.OpenJobItemContent(context.Background(), "01J8ZK7Q9X2M4N6P8R0T2V4W6Y", 0)
	assertRecordsErrCode(t, err, dkdomain.ErrCodeItemNotFound)

	store = &fakeAdminRecordsStore{job: recordsTestJob(), itemErr: dkdomain.ErrNotFound}
	svc = newRecordsService(t, store, &fakeRecordsObjectStore{})
	_, _, err = svc.OpenJobItemContent(context.Background(), "01J8ZK7Q9X2M4N6P8R0T2V4W6Y", 9)
	assertRecordsErrCode(t, err, dkdomain.ErrCodeItemNotFound)
}

// 还没出好（非终态、没图）→ 「还没出好」；终态没图 → 干净的 DK_IMAGE_NOT_FOUND。
func TestOpenJobItemContentNoImageStates(t *testing.T) {
	store := &fakeAdminRecordsStore{
		job:  recordsTestJob(),
		item: &dkdomain.JobItem{ID: 31, Seq: 1, Status: dkdomain.ItemStatusRunning},
	}
	svc := newRecordsService(t, store, &fakeRecordsObjectStore{})

	_, _, err := svc.OpenJobItemContent(context.Background(), "01J8ZK7Q9X2M4N6P8R0T2V4W6Y", 1)
	assertRecordsErrCode(t, err, dkdomain.ErrCodeImageNotFound)
	dkErr, _ := dkdomain.AsDesignkitError(err)
	assert.Contains(t, dkErr.Message, "还没出好", "非终态要说清是在跑，不是图丢了")

	store.item = &dkdomain.JobItem{ID: 31, Seq: 1, Status: dkdomain.ItemStatusFailed}
	_, _, err = svc.OpenJobItemContent(context.Background(), "01J8ZK7Q9X2M4N6P8R0T2V4W6Y", 1)
	assertRecordsErrCode(t, err, dkdomain.ErrCodeImageNotFound)
}

// 对象存储缺席 / 读失败 → DK_STORAGE_ERROR，**不是 404** ——
// 报成「找不到」会让管理员以为记录没了，实际要查的是磁盘。
func TestOpenJobItemContentStorageFailures(t *testing.T) {
	store := &fakeAdminRecordsStore{
		job:    recordsTestJob(),
		item:   &dkdomain.JobItem{ID: 31, Seq: 1, Status: dkdomain.ItemStatusSucceeded},
		images: []*dkdomain.Image{{ID: 41, ImageIndex: 1, ObjectKey: "k", ContentType: "image/png"}},
	}

	// Store 为 nil（存储目录建不出来时的装配形态）。
	svc := newRecordsService(t, store, nil)
	_, _, err := svc.OpenJobItemContent(context.Background(), "01J8ZK7Q9X2M4N6P8R0T2V4W6Y", 1)
	assertRecordsErrCode(t, err, dkdomain.ErrCodeStorageError)

	// 库里有记录、盘上没文件。
	svc = newRecordsService(t, store, &fakeRecordsObjectStore{getErr: errors.New("no such file")})
	_, _, err = svc.OpenJobItemContent(context.Background(), "01J8ZK7Q9X2M4N6P8R0T2V4W6Y", 1)
	assertRecordsErrCode(t, err, dkdomain.ErrCodeStorageError)
}

// ---- 对话附图字节 ----

// 正常路：uid → asset → store.Get(object_key)；contentType 缺失时用记录里的兜底。
func TestOpenAssetContentReadsStore(t *testing.T) {
	store := &fakeAdminRecordsStore{
		asset: &dkdomain.Asset{ID: 51, UID: "01J8ZK7Q9X2M4N6P8R0T2V4W6Z",
			ObjectKey: "designkit/assets/2026/08/23/a.png", ContentType: "image/png"},
	}
	objects := &fakeRecordsObjectStore{data: []byte{0x89, 'P', 'N', 'G'}, contentType: ""}
	svc := newRecordsService(t, store, objects)

	data, contentType, err := svc.OpenAssetContent(context.Background(), "01J8ZK7Q9X2M4N6P8R0T2V4W6Z")
	require.NoError(t, err)
	assert.Equal(t, []byte{0x89, 'P', 'N', 'G'}, data)
	assert.Equal(t, "image/png", contentType, "store 没回 contentType 时用记录里的兜底")
	assert.Equal(t, "01J8ZK7Q9X2M4N6P8R0T2V4W6Z", store.lastAssetUID)
	assert.Equal(t, "designkit/assets/2026/08/23/a.png", objects.lastKey)
}

// 素材不存在（或已被主人删掉）→ DK_ASSET_NOT_FOUND。
func TestOpenAssetContentTranslatesNotFound(t *testing.T) {
	store := &fakeAdminRecordsStore{assetErr: dkdomain.ErrNotFound}
	svc := newRecordsService(t, store, &fakeRecordsObjectStore{})

	_, _, err := svc.OpenAssetContent(context.Background(), "01J8ZK7Q9X2M4N6P8R0T2V4W6Z")
	assertRecordsErrCode(t, err, dkdomain.ErrCodeAssetNotFound)
}

// 对象存储缺席 / 读失败 → DK_STORAGE_ERROR（口径同批次缩略图，不误报 404）。
func TestOpenAssetContentStorageFailures(t *testing.T) {
	store := &fakeAdminRecordsStore{
		asset: &dkdomain.Asset{ID: 51, UID: "01J8ZK7Q9X2M4N6P8R0T2V4W6Z", ObjectKey: "k"},
	}

	svc := newRecordsService(t, store, nil)
	_, _, err := svc.OpenAssetContent(context.Background(), "01J8ZK7Q9X2M4N6P8R0T2V4W6Z")
	assertRecordsErrCode(t, err, dkdomain.ErrCodeStorageError)

	svc = newRecordsService(t, store, &fakeRecordsObjectStore{getErr: errors.New("no such file")})
	_, _, err = svc.OpenAssetContent(context.Background(), "01J8ZK7Q9X2M4N6P8R0T2V4W6Z")
	assertRecordsErrCode(t, err, dkdomain.ErrCodeStorageError)
}

// store 没回 contentType 时用图片记录里的兜底。
func TestOpenJobItemContentFallsBackToRecordContentType(t *testing.T) {
	store := &fakeAdminRecordsStore{
		job:    recordsTestJob(),
		item:   &dkdomain.JobItem{ID: 31, Seq: 1, Status: dkdomain.ItemStatusSucceeded},
		images: []*dkdomain.Image{{ID: 41, ImageIndex: 1, ObjectKey: "k", ContentType: "image/png"}},
	}
	svc := newRecordsService(t, store, &fakeRecordsObjectStore{data: []byte{1}, contentType: ""})

	_, contentType, err := svc.OpenJobItemContent(context.Background(), "01J8ZK7Q9X2M4N6P8R0T2V4W6Y", 1)
	require.NoError(t, err)
	assert.Equal(t, "image/png", contentType)
}
