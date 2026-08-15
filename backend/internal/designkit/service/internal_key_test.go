//go:build unit

package service

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	dkdomain "github.com/Wei-Shaw/sub2api/internal/designkit/domain"
	upstreamservice "github.com/Wei-Shaw/sub2api/internal/service"
)

// ---------------------------------------------------------------------------
// 假的上游 APIKeyService
// ---------------------------------------------------------------------------

type ikFakeProvisioner struct {
	mu sync.Mutex

	stored []upstreamservice.APIKey
	nextID int64

	searchCalls int
	createCalls int
	createErr   error

	updateCalls int
	updateErr   error

	// lastCreate 记下最后一次 Create 收到的参数，用来断言我们传对了。
	lastCreateUserID int64
	lastCreateReq    upstreamservice.CreateAPIKeyRequest

	// lastUpdate 同理，用来断言补分组时传的是哪一把 Key、哪个分组。
	lastUpdateID     int64
	lastUpdateUserID int64
	lastUpdateReq    upstreamservice.UpdateAPIKeyRequest
}

func newIKFakeProvisioner() *ikFakeProvisioner {
	return &ikFakeProvisioner{nextID: 100}
}

func (f *ikFakeProvisioner) SearchAPIKeys(_ context.Context, userID int64, keyword string, limit int) ([]upstreamservice.APIKey, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.searchCalls++

	out := make([]upstreamservice.APIKey, 0, len(f.stored))
	for _, key := range f.stored {
		if key.UserID != userID {
			continue
		}
		// 模仿上游 repo 的 NameContainsFold。
		if keyword != "" && !strings.Contains(strings.ToLower(key.Name), strings.ToLower(keyword)) {
			continue
		}
		out = append(out, key)
		if len(out) >= limit {
			break
		}
	}
	return out, nil
}

func (f *ikFakeProvisioner) Create(_ context.Context, userID int64, req upstreamservice.CreateAPIKeyRequest) (*upstreamservice.APIKey, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.createCalls++
	f.lastCreateUserID = userID
	f.lastCreateReq = req
	if f.createErr != nil {
		return nil, f.createErr
	}

	key := upstreamservice.APIKey{
		ID:      f.nextID,
		UserID:  userID,
		Name:    req.Name,
		GroupID: req.GroupID,
		Status:  upstreamservice.StatusActive,
	}
	f.nextID++
	f.stored = append(f.stored, key)

	created := key
	return &created, nil
}

func (f *ikFakeProvisioner) GetByID(_ context.Context, id int64) (*upstreamservice.APIKey, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, key := range f.stored {
		if key.ID != id {
			continue
		}
		// 上游 GetByID 走 WithUser().WithGroup()，关联对象是带出来的。
		full := key
		full.User = &upstreamservice.User{
			ID:          key.UserID,
			Role:        "user",
			Concurrency: 5,
			Status:      upstreamservice.StatusActive,
		}
		if key.GroupID != nil {
			full.Group = &upstreamservice.Group{
				ID:                   *key.GroupID,
				Name:                 "designkit",
				Platform:             "openai",
				Status:               upstreamservice.StatusActive,
				Hydrated:             true,
				AllowImageGeneration: true,
			}
		}
		return &full, nil
	}
	return nil, upstreamservice.ErrAPIKeyNotFound
}

// Update 模仿上游 api_key_service.go:759：先校验归属，再只改传进来的字段。
func (f *ikFakeProvisioner) Update(_ context.Context, id int64, userID int64, req upstreamservice.UpdateAPIKeyRequest) (*upstreamservice.APIKey, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.updateCalls++
	f.lastUpdateID = id
	f.lastUpdateUserID = userID
	f.lastUpdateReq = req
	if f.updateErr != nil {
		return nil, f.updateErr
	}
	for i := range f.stored {
		if f.stored[i].ID != id {
			continue
		}
		// 归属校验：上游在 :769 判 apiKey.UserID != userID → ErrInsufficientPerms。
		if f.stored[i].UserID != userID {
			return nil, upstreamservice.ErrInsufficientPerms
		}
		if req.GroupID != nil {
			f.stored[i].GroupID = req.GroupID
		}
		if req.Name != nil {
			f.stored[i].Name = *req.Name
		}
		if req.Status != nil {
			f.stored[i].Status = *req.Status
		}
		updated := f.stored[i]
		return &updated, nil
	}
	return nil, upstreamservice.ErrAPIKeyNotFound
}

func (f *ikFakeProvisioner) counts() (search, create int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.searchCalls, f.createCalls
}

func (f *ikFakeProvisioner) updates() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.updateCalls
}

// ---------------------------------------------------------------------------
// 测试
// ---------------------------------------------------------------------------

// 首次进工作台：库里什么都没有，自动建一把，并且带回 User / Group。
func TestEnsureInternalKey_CreatesOnFirstUse(t *testing.T) {
	repo := newIKFakeProvisioner()
	svc := NewInternalKeyService(repo, StaticGroupID(7))

	key, err := svc.EnsureInternalKey(context.Background(), 42)
	if err != nil {
		t.Fatalf("EnsureInternalKey 失败: %v", err)
	}
	if key.UserID != 42 {
		t.Fatalf("Key 属于用户 %d，应为 42", key.UserID)
	}
	if key.Name != InternalAPIKeyName {
		t.Fatalf("Key 名字 = %q，应为 %q", key.Name, InternalAPIKeyName)
	}
	if key.GroupID == nil || *key.GroupID != 7 {
		t.Fatalf("Key 没绑到 designkit 专属分组: %v", key.GroupID)
	}
	// 出图时要用这两个：User 做计费准入，Group 判分组有没有开生图。
	if key.User == nil || key.Group == nil {
		t.Fatal("返回的 Key 必须带 User 和 Group")
	}

	if repo.lastCreateUserID != 42 {
		t.Fatalf("Create 的 userID = %d，应为 42", repo.lastCreateUserID)
	}
	// 额度 / 限额一律留 0（= 不限）：钱由 designkit 自己的冻结账管，
	// 这里再设一层上限只会出现「余额够但 Key 限额挡住」的迷惑现象。
	if repo.lastCreateReq.Quota != 0 || repo.lastCreateReq.ExpiresInDays != nil {
		t.Fatalf("内部 Key 不该带额度或有效期: %+v", repo.lastCreateReq)
	}
}

// 已经建出来但**没绑分组**的那把，要自动补上分组。
//
// 这一条是 2026-08-14 线上真事故的回归测试：分组解析器上线之前 Key 就已经建好了，
// 而解析器只在 create 那一刻生效，于是那把 Key 永远没有分组，
// 每次出图都报「没有可用的出图账号」，管理员在后台看分组、看账号却全是对的。
func TestEnsureInternalKey_BindsGroupOnExistingUngroupedKey(t *testing.T) {
	repo := newIKFakeProvisioner()
	repo.stored = append(repo.stored, upstreamservice.APIKey{
		// GroupID 刻意留 nil：这就是解析器上线前建出来的那种。
		ID: 55, UserID: 42, Name: InternalAPIKeyName, Status: upstreamservice.StatusActive,
	})
	svc := NewInternalKeyService(repo, StaticGroupID(7))

	key, err := svc.EnsureInternalKey(context.Background(), 42)
	if err != nil {
		t.Fatalf("EnsureInternalKey 失败: %v", err)
	}
	if key.GroupID == nil || *key.GroupID != 7 {
		t.Fatalf("没给已存在的 Key 补上分组: %v", key.GroupID)
	}
	// 必须是「改」而不是「再建一把」——重复建会让用量散在两把 Key 上。
	if _, create := repo.counts(); create != 0 {
		t.Fatalf("不该建新 Key，Create 被调了 %d 次", create)
	}
	if repo.updates() != 1 {
		t.Fatalf("Update 应该只被调 1 次，实际 %d 次", repo.updates())
	}
	if repo.lastUpdateID != 55 || repo.lastUpdateUserID != 42 {
		t.Fatalf("Update 改错了对象: id=%d userID=%d", repo.lastUpdateID, repo.lastUpdateUserID)
	}
	// 出图要用这两个关联对象，补完分组之后必须重新带出来。
	if key.User == nil || key.Group == nil {
		t.Fatal("补完分组返回的 Key 必须带 User 和 Group")
	}
}

// 已经有分组的**一律不碰**——那可能是管理员有意调过的。
func TestEnsureInternalKey_LeavesExistingGroupAlone(t *testing.T) {
	repo := newIKFakeProvisioner()
	existing := int64(9)
	repo.stored = append(repo.stored, upstreamservice.APIKey{
		ID: 55, UserID: 42, Name: InternalAPIKeyName,
		GroupID: &existing, Status: upstreamservice.StatusActive,
	})
	// 解析器给的是 7，跟已有的 9 不一样——正是「会不会被覆盖」的判据。
	svc := NewInternalKeyService(repo, StaticGroupID(7))

	key, err := svc.EnsureInternalKey(context.Background(), 42)
	if err != nil {
		t.Fatalf("EnsureInternalKey 失败: %v", err)
	}
	if key.GroupID == nil || *key.GroupID != 9 {
		t.Fatalf("管理员设的分组被覆盖了: %v", key.GroupID)
	}
	if repo.updates() != 0 {
		t.Fatalf("有分组的 Key 不该被 Update，实际调了 %d 次", repo.updates())
	}
}

// 补分组失败**不能**把提交打回去：出图那一步自己会报更贴切的错。
func TestEnsureInternalKey_GroupBindFailureIsNotFatal(t *testing.T) {
	repo := newIKFakeProvisioner()
	repo.stored = append(repo.stored, upstreamservice.APIKey{
		ID: 55, UserID: 42, Name: InternalAPIKeyName, Status: upstreamservice.StatusActive,
	})
	repo.updateErr = errors.New("上游拒绝了")
	svc := NewInternalKeyService(repo, StaticGroupID(7))

	key, err := svc.EnsureInternalKey(context.Background(), 42)
	if err != nil {
		t.Fatalf("补分组失败不该让 EnsureInternalKey 报错: %v", err)
	}
	if key == nil || key.ID != 55 {
		t.Fatalf("应该原样返回那把 Key，实际: %+v", key)
	}
}

// 第二次调用要复用同一把，绝不能每次进工作台建一把。
func TestEnsureInternalKey_ReusesExisting(t *testing.T) {
	repo := newIKFakeProvisioner()
	svc := NewInternalKeyService(repo, StaticGroupID(7))

	first, err := svc.EnsureInternalKey(context.Background(), 42)
	if err != nil {
		t.Fatalf("第一次失败: %v", err)
	}
	second, err := svc.EnsureInternalKey(context.Background(), 42)
	if err != nil {
		t.Fatalf("第二次失败: %v", err)
	}
	if first.ID != second.ID {
		t.Fatalf("两次拿到不同的 Key: %d vs %d", first.ID, second.ID)
	}
	if _, create := repo.counts(); create != 1 {
		t.Fatalf("Create 被调了 %d 次，应该只有 1 次", create)
	}
}

// 进程重启后缓存是空的，靠名字也得能把它找回来（而不是再建一把）。
func TestEnsureInternalKey_FindsExistingByNameWithoutCache(t *testing.T) {
	repo := newIKFakeProvisioner()
	repo.stored = append(repo.stored, upstreamservice.APIKey{
		ID: 55, UserID: 42, Name: InternalAPIKeyName, Status: upstreamservice.StatusActive,
	})
	svc := NewInternalKeyService(repo, StaticGroupID(7))

	key, err := svc.EnsureInternalKey(context.Background(), 42)
	if err != nil {
		t.Fatalf("EnsureInternalKey 失败: %v", err)
	}
	if key.ID != 55 {
		t.Fatalf("应该找回已有的 55，实际 %d", key.ID)
	}
	if _, create := repo.counts(); create != 0 {
		t.Fatal("已经有一把了还去建新的")
	}
}

// 名字只是「包含」而不是完全相等的，不算数（运营自己起的 designkit-internal-备份）。
func TestEnsureInternalKey_IgnoresSimilarlyNamedKeys(t *testing.T) {
	repo := newIKFakeProvisioner()
	repo.stored = append(repo.stored, upstreamservice.APIKey{
		ID: 55, UserID: 42, Name: InternalAPIKeyName + "-备份", Status: upstreamservice.StatusActive,
	})
	svc := NewInternalKeyService(repo, StaticGroupID(7))

	key, err := svc.EnsureInternalKey(context.Background(), 42)
	if err != nil {
		t.Fatalf("EnsureInternalKey 失败: %v", err)
	}
	if key.ID == 55 {
		t.Fatal("名字只是相似的 Key 不该被当成内部专用 Key")
	}
	if _, create := repo.counts(); create != 1 {
		t.Fatal("应该建一把新的")
	}
}

// 运营连点两下「提交」：并发调用只能建出一把。
func TestEnsureInternalKey_ConcurrentCallsCreateOnlyOne(t *testing.T) {
	repo := newIKFakeProvisioner()
	svc := NewInternalKeyService(repo, StaticGroupID(7))

	const n = 8
	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		ids  = map[int64]int{}
		errs []error
	)
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			key, err := svc.EnsureInternalKey(context.Background(), 42)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				errs = append(errs, err)
				return
			}
			ids[key.ID]++
		}()
	}
	wg.Wait()

	if len(errs) != 0 {
		t.Fatalf("并发调用出错: %v", errs)
	}
	if len(ids) != 1 {
		t.Fatalf("并发调用建出了多把 Key: %v", ids)
	}
	if _, create := repo.counts(); create != 1 {
		t.Fatalf("Create 被调了 %d 次", create)
	}
}

// 绑不上分组 = 管理员还没把这个人加进 designkit 分组。
// 必须给中文出路，不能把上游那句英文抛给运营。
func TestEnsureInternalKey_GroupNotAllowedGivesChineseError(t *testing.T) {
	repo := newIKFakeProvisioner()
	repo.createErr = upstreamservice.ErrGroupNotAllowed
	svc := NewInternalKeyService(repo, StaticGroupID(7))

	_, err := svc.EnsureInternalKey(context.Background(), 42)
	if err == nil {
		t.Fatal("应该失败")
	}
	dkErr, ok := dkdomain.AsDesignkitError(err)
	if !ok {
		t.Fatalf("错误类型不对: %T", err)
	}
	if dkErr.Code != dkdomain.ErrCodeAPIKeyMissing {
		t.Fatalf("错误码 = %s，应为 %s", dkErr.Code, dkdomain.ErrCodeAPIKeyMissing)
	}
	if !strings.Contains(dkErr.Message, "联系管理员") {
		t.Fatalf("文案要告诉运营该找谁: %q", dkErr.Message)
	}
	if !errors.Is(err, upstreamservice.ErrGroupNotAllowed) {
		t.Fatal("原始错误应该还能被 errors.Is 找到，方便排障")
	}
}

// 管理员把这把 Key 停用了 = 故意不让这个人出图。
// **绝不能**因为找不到可用的就再建一把绕过去。
func TestEnsureInternalKey_DoesNotBypassDisabledKey(t *testing.T) {
	repo := newIKFakeProvisioner()
	repo.stored = append(repo.stored, upstreamservice.APIKey{
		ID: 55, UserID: 42, Name: InternalAPIKeyName, Status: "disabled",
	})
	svc := NewInternalKeyService(repo, StaticGroupID(7))

	_, err := svc.EnsureInternalKey(context.Background(), 42)
	if err == nil {
		t.Fatal("Key 被停用时应该报错")
	}
	if _, create := repo.counts(); create != 0 {
		t.Fatal("不许绕过被停用的 Key 再建一把")
	}
	dkErr, _ := dkdomain.AsDesignkitError(err)
	if dkErr == nil || dkErr.Code != dkdomain.ErrCodeAPIKeyMissing {
		t.Fatalf("错误码不对: %v", err)
	}
}

// 缓存里那把在库里被删掉了 → 重新建一把，而不是一直报错。
func TestEnsureInternalKey_RecreatesWhenCachedKeyDeleted(t *testing.T) {
	repo := newIKFakeProvisioner()
	svc := NewInternalKeyService(repo, StaticGroupID(7))

	first, err := svc.EnsureInternalKey(context.Background(), 42)
	if err != nil {
		t.Fatalf("第一次失败: %v", err)
	}

	// 管理员在后台把它删了。
	repo.mu.Lock()
	repo.stored = nil
	repo.mu.Unlock()

	second, err := svc.EnsureInternalKey(context.Background(), 42)
	if err != nil {
		t.Fatalf("Key 被删后应该重建: %v", err)
	}
	if second.ID == first.ID {
		t.Fatal("应该是一把新的 Key")
	}
}

func TestEnsureInternalKey_RejectsAnonymous(t *testing.T) {
	svc := NewInternalKeyService(newIKFakeProvisioner(), StaticGroupID(7))

	_, err := svc.EnsureInternalKey(context.Background(), 0)
	dkErr, ok := dkdomain.AsDesignkitError(err)
	if !ok || dkErr.Code != dkdomain.ErrCodeUnauthorized {
		t.Fatalf("应该是 %s，实际 %v", dkdomain.ErrCodeUnauthorized, err)
	}
}

// 不配分组也能建（但会拿不到账号，注释里写清楚了）；这里只断言不报错、不绑组。
func TestEnsureInternalKey_WithoutGroupResolver(t *testing.T) {
	repo := newIKFakeProvisioner()
	svc := NewInternalKeyService(repo, nil)

	key, err := svc.EnsureInternalKey(context.Background(), 42)
	if err != nil {
		t.Fatalf("EnsureInternalKey 失败: %v", err)
	}
	if key.GroupID != nil {
		t.Fatalf("不该绑分组: %v", *key.GroupID)
	}
}

// InternalKeyService 同时也是 GatewayInvoker 要的 APIKeyLoader，
// 装配时一个对象就够了。
func TestInternalKeyService_ImplementsAPIKeyLoader(t *testing.T) {
	repo := newIKFakeProvisioner()
	svc := NewInternalKeyService(repo, StaticGroupID(7))

	created, err := svc.EnsureInternalKey(context.Background(), 42)
	if err != nil {
		t.Fatalf("EnsureInternalKey 失败: %v", err)
	}

	var loader APIKeyLoader = svc
	loaded, err := loader.GetByID(context.Background(), created.ID)
	if err != nil {
		t.Fatalf("GetByID 失败: %v", err)
	}
	if loaded.ID != created.ID || loaded.User == nil {
		t.Fatalf("GetByID 返回的不对: %+v", loaded)
	}
}

func TestStaticGroupID(t *testing.T) {
	id, err := StaticGroupID(9)(context.Background())
	if err != nil || id == nil || *id != 9 {
		t.Fatalf("StaticGroupID(9) = %v, %v", id, err)
	}
	id, err = StaticGroupID(0)(context.Background())
	if err != nil || id != nil {
		t.Fatalf("StaticGroupID(0) 应该返回 nil: %v, %v", id, err)
	}
}
