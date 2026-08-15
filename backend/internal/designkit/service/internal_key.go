package service

// internal_key.go —— 每个运营用户一把「内部专用 API Key」。
//
// 为什么必须有这东西：
//
//	上游 OpenAIGatewayHandler.Images() 的第一件事就是
//	middleware.GetAPIKeyFromContext(c)，取不到直接 401（openai_images.go:29-33）；
//	而且 usage_logs.api_key_id 是 NOT NULL + 外键，出图必然要写一条用量记录。
//	运营是用浏览器 JWT 登录的，**登录态里根本没有 API Key**。
//	所以服务端要在他第一次进工作台时，自动给他建一把、并且界面上不暴露。
//
// 这把 Key 只在**浏览器提交的批次**里用（designkit_jobs.api_key_id）。
// ERP 提交的批次用 ERP 自己那把 Key —— 出的图记在它名下、费用单独算
// （CLAUDE.md 决策 4），所以出图时是按 job 行上记的 api_key_id 取 Key，
// 不是「再找一次这个用户的内部 Key」。
//
// 权限边界（CLAUDE.md 第二节末尾那条技术决定）：
// 上游 APIKeyService.Create(ctx, userID, req) 的 userID 是入参、函数体内**没有
// 管理员校验**（api_key_service.go:461-563），技术上可以替任意人签发。
// 我们只用它给「当前登录的这个人自己」签发，绝不接受外部传进来的 userID。
// 分组绑定权限仍然由上游的 canUserBindGroup 把关（:450-459），
// 绑不上会返回 ErrGroupNotAllowed，我们翻译成中文的
// 「账号还没开通出图权限，请联系管理员。」。

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"

	dkdomain "github.com/Wei-Shaw/sub2api/internal/designkit/domain"
	upstreamservice "github.com/Wei-Shaw/sub2api/internal/service"
)

// InternalAPIKeyName 内部专用 Key 的名字，也是找回它的唯一线索。
//
// 规矩：
//   - 只用 ASCII，不带 & < > " ' —— 上游 Create 会对 name 做 html.EscapeString，
//     带了这些字符就存不回原样，下次按名字找就找不到了；
//   - 前缀 designkit- 让前端能一眼认出并在「我的密钥」页面隐藏它
//     （那个页面本来对普通运营就是隐藏的，这是第二层保险）。
const InternalAPIKeyName = "designkit-internal"

// internalKeySearchLimit 按名字回查时取多少条。
// 正常情况下一个用户只会有一把，取 20 条是给「历史上误建了几把」留余地。
const internalKeySearchLimit = 20

// APIKeyProvisioner 是上游 *service.APIKeyService 的最小切面。
//
// 三个方法都由上游原样提供，签名逐字核对过：
//
//	SearchAPIKeys(ctx, userID, keyword, limit) —— api_key_service.go:1066，
//	  按 name 做 ContainsFold 模糊匹配，只在该用户名下找（repo:679-699）
//	Create(ctx, userID, req)                   —— api_key_service.go:461
//	GetByID(ctx, id)                           —— api_key_service.go:691，
//	  内部走 WithUser().WithGroup()，出图需要的关联对象靠它带出来
//	Update(ctx, id, userID, req)               —— api_key_service.go:759，
//	  只用来给「已经建出来但没绑分组」的内部 Key 补上分组，见 ensureGroupBound。
//	  它自己会先校验归属（:769 apiKey.UserID != userID → ErrInsufficientPerms）。
type APIKeyProvisioner interface {
	SearchAPIKeys(ctx context.Context, userID int64, keyword string, limit int) ([]upstreamservice.APIKey, error)
	Create(ctx context.Context, userID int64, req upstreamservice.CreateAPIKeyRequest) (*upstreamservice.APIKey, error)
	GetByID(ctx context.Context, id int64) (*upstreamservice.APIKey, error)
	Update(ctx context.Context, id int64, userID int64, req upstreamservice.UpdateAPIKeyRequest) (*upstreamservice.APIKey, error)
}

// GroupIDResolver 告诉我们内部专用 Key 该绑哪个分组。
//
// 返回 nil 表示不绑分组。不绑的后果要知道：
//   - 上游 RequireGroupAssignment 中间件会拦未分组的 Key（我们这条路径绕过了它，
//     所以不会当场报错）；
//   - 但账号调度是按分组选账号的，没有分组就只能走「允许未分组 Key 调度」那条
//     系统设置，默认是关的 —— 表现为出图报「没有可用的出图账号」。
//
// 所以部署时应该实实在在给一个 designkit 专属分组。
type GroupIDResolver func(ctx context.Context) (*int64, error)

// StaticGroupID 把一个固定的分组 ID 包成 GroupIDResolver。
func StaticGroupID(id int64) GroupIDResolver {
	return func(context.Context) (*int64, error) {
		if id <= 0 {
			return nil, nil
		}
		v := id
		return &v, nil
	}
}

// InternalKeyService 负责「保证这个用户有一把可用的内部专用 Key」。
type InternalKeyService struct {
	keys    APIKeyProvisioner
	groupID GroupIDResolver

	mu    sync.Mutex
	locks map[int64]*sync.Mutex
	// cache 记 userID → apiKeyID。**只缓存 ID，不缓存整个对象**：
	// 分组、状态、余额随时可能被管理员改，每次都用 GetByID 拿最新的，
	// 免得拿着一份过期快照去出图（比如 Key 已经被停用了还在扣钱）。
	cache map[int64]int64
}

// NewInternalKeyService 造一个内部 Key 服务。
// groupID 可以为 nil，表示不绑分组（后果见 GroupIDResolver 的注释）。
func NewInternalKeyService(keys APIKeyProvisioner, groupID GroupIDResolver) *InternalKeyService {
	return &InternalKeyService{
		keys:    keys,
		groupID: groupID,
		locks:   make(map[int64]*sync.Mutex),
		cache:   make(map[int64]int64),
	}
}

// 编译期断言：本服务也能直接当 GatewayInvoker 的 APIKeyLoader 用，
// 这样装配时一个对象就够了。
var _ APIKeyLoader = (*InternalKeyService)(nil)

// GetByID 透传上游的按 ID 取 Key（实现 APIKeyLoader）。
func (s *InternalKeyService) GetByID(ctx context.Context, id int64) (*upstreamservice.APIKey, error) {
	if s == nil || s.keys == nil {
		return nil, errors.New("designkit: internal key service 未装配")
	}
	return s.keys.GetByID(ctx, id)
}

// EnsureInternalKey 保证 userID 名下有一把可用的内部专用 Key，并返回它
// （**带 User 和 Group**，可以直接塞进合成 gin.Context）。
//
// 调用时机：运营首次进工作台、以及每次浏览器提交批次之前。
// 返回的 error 一律是 *domain.DesignkitError，Message 是给运营看的中文。
func (s *InternalKeyService) EnsureInternalKey(ctx context.Context, userID int64) (*upstreamservice.APIKey, error) {
	if s == nil || s.keys == nil {
		return nil, dkdomain.NewError(dkdomain.ErrCodeInternal).
			WithCause(errors.New("designkit: internal key service 未装配"))
	}
	if userID <= 0 {
		return nil, dkdomain.NewError(dkdomain.ErrCodeUnauthorized).
			WithCause(errors.New("designkit: userID 必须大于 0"))
	}

	// 每个用户一把锁：同一个人连点两下「提交」不会建出两把 Key。
	// 注意这只在单进程内有效，多副本部署仍可能各建一把（见文件末尾说明）。
	lock := s.userLock(userID)
	lock.Lock()
	defer lock.Unlock()

	if cached, ok := s.cachedKeyID(userID); ok {
		key, err := s.loadUsable(ctx, cached, userID)
		if err == nil {
			return key, nil
		}
		var dkErr *dkdomain.DesignkitError
		if errors.As(err, &dkErr) && dkErr.Code == dkdomain.ErrCodeAPIKeyMissing && !errors.Is(err, errInternalKeyGone) {
			// Key 还在，只是不可用（被管理员停用了）→ 如实报错，
			// **不要**偷偷再建一把绕过去。
			return nil, err
		}
		// Key 已经不在了（被删了）→ 清缓存，走下面重新找 / 重新建。
		s.forget(userID)
	}

	if key, err := s.findExisting(ctx, userID); err != nil {
		return nil, err
	} else if key != nil {
		s.remember(userID, key.ID)
		return s.ensureGroupBound(ctx, key, userID), nil
	}

	return s.create(ctx, userID)
}

// ensureGroupBound 给「已经建出来但没绑分组」的内部 Key 补上分组。
//
// 为什么必须有这一段：groupID 解析器只在 create 那一刻生效，而 Key 是**一次性**
// 建出来的，之后每次提交都走 findExisting 直接返回。所以在解析器上线之前
// 就已经建好的那些 Key（以及任何一次解析失败时建出来的 Key）会永远没有分组，
// 表现是每次出图都报「没有可用的出图账号」，而管理员在后台看分组、看账号全是对的。
//
// 2026-08-14 实测就撞上了：Key 在 15:34 建出来（那会儿解析器还没上线），
// 15:45 解析器上线，15:49 提交仍然失败 —— 因为那把 Key 根本不会被重新创建。
//
// 边界：
//   - 只动**我们自己**那把（名字已经在 findExisting 里精确比对过）、
//     且**当前没有分组**的 Key。已经有分组的一律不碰 —— 那可能是管理员
//     有意调过的，我们没有资格覆盖。
//   - 补不上（分组解析不出来、上游拒绝）**不报错**，返回原来那把 Key：
//     出图那一步自己会报「没有可用的出图账号」，那是更贴切的报错位置。
//     为了补分组失败就把提交整个打回去，只会把一个可恢复的配置问题
//     变成运营看不懂的系统错误。
func (s *InternalKeyService) ensureGroupBound(ctx context.Context, key *upstreamservice.APIKey, userID int64) *upstreamservice.APIKey {
	if key == nil || key.GroupID != nil || s.groupID == nil {
		return key
	}

	groupID, err := s.groupID(ctx)
	if err != nil || groupID == nil {
		// 解析器自己已经打过日志说清楚该改什么了，这里不重复刷屏。
		return key
	}

	updated, err := s.keys.Update(ctx, key.ID, userID, upstreamservice.UpdateAPIKeyRequest{
		GroupID: groupID,
	})
	if err != nil {
		slog.Warn("designkit 想给内部专用 Key 补上分组但没成功，出图可能会报「没有可用的出图账号」",
			slog.Int64("api_key_id", key.ID),
			slog.Int64("group_id", *groupID),
			slog.Any("error", err))
		return key
	}
	slog.Info("designkit 给已存在的内部专用 Key 补上了分组",
		slog.Int64("api_key_id", key.ID),
		slog.Int64("group_id", *groupID))

	// Update 返回的对象跟 Create 一样不带 User / Group 关联，出图要用，所以重取一次。
	// 取不回来就用原来那把（分组已经写进库了，下次进来自然是对的）。
	if updated != nil {
		if reloaded, loadErr := s.loadUsable(ctx, key.ID, userID); loadErr == nil {
			return reloaded
		}
	}
	return key
}

// errInternalKeyGone 内部信号：这把 Key 在库里已经查不到了。
var errInternalKeyGone = errors.New("designkit: internal api key gone")

// loadUsable 取一把 Key 并校验归属与状态。
func (s *InternalKeyService) loadUsable(ctx context.Context, keyID, userID int64) (*upstreamservice.APIKey, error) {
	key, err := s.keys.GetByID(ctx, keyID)
	if err != nil || key == nil {
		return nil, dkdomain.NewError(dkdomain.ErrCodeAPIKeyMissing).
			WithCause(fmt.Errorf("%w: id=%d: %v", errInternalKeyGone, keyID, err))
	}
	if key.UserID != userID {
		return nil, dkdomain.NewError(dkdomain.ErrCodeForbidden).
			WithCause(fmt.Errorf("designkit: api key %d 属于用户 %d，不是 %d", key.ID, key.UserID, userID))
	}
	if !key.IsActive() {
		return nil, dkdomain.NewError(dkdomain.ErrCodeAPIKeyMissing).
			WithCause(fmt.Errorf("designkit: api key %d 状态为 %s", key.ID, key.Status))
	}
	if key.User == nil {
		return nil, dkdomain.NewError(dkdomain.ErrCodeAPIKeyMissing).
			WithCause(fmt.Errorf("designkit: api key %d 没有加载到用户信息", key.ID))
	}
	return key, nil
}

// findExisting 按名字把已经建过的那把找回来。
//
// 找不到返回 (nil, nil) —— 那是正常的「第一次进工作台」。
func (s *InternalKeyService) findExisting(ctx context.Context, userID int64) (*upstreamservice.APIKey, error) {
	found, err := s.keys.SearchAPIKeys(ctx, userID, InternalAPIKeyName, internalKeySearchLimit)
	if err != nil {
		return nil, dkdomain.NewError(dkdomain.ErrCodeInternal).WithCause(err)
	}

	// SearchAPIKeys 是模糊匹配（NameContainsFold），可能捞到运营自己起的
	// "designkit-internal-备份" 之类的名字，所以这里再做一次**精确**比对。
	//
	// 挑选规则：同名的里面固定取 **id 最小的那把**（最早建的）。
	// 万一历史上误建了多把，不同副本、不同时刻的选择都一致，账不会来回跳。
	var (
		active   *upstreamservice.APIKey
		inactive *upstreamservice.APIKey
	)
	for i := range found {
		item := found[i]
		if item.UserID != userID {
			continue
		}
		if !strings.EqualFold(strings.TrimSpace(item.Name), InternalAPIKeyName) {
			continue
		}
		picked := item
		if item.IsActive() {
			if active == nil || picked.ID < active.ID {
				active = &picked
			}
			continue
		}
		if inactive == nil || picked.ID < inactive.ID {
			inactive = &picked
		}
	}

	candidate := active
	if candidate == nil {
		if inactive != nil {
			// 有但被停用了 —— 那是管理员**故意**停的。
			// 这时候再建一把新的等于绕过管理员的处置，绝对不行：如实报错。
			return nil, dkdomain.NewError(dkdomain.ErrCodeAPIKeyMissing).
				WithCause(fmt.Errorf("designkit: 用户 %d 的内部专用 Key（id=%d）已被停用", userID, inactive.ID))
		}
		return nil, nil
	}

	// SearchAPIKeys 不带 User / Group 关联，出图需要，所以再取一次完整对象。
	return s.loadUsable(ctx, candidate.ID, userID)
}

// create 建一把新的内部专用 Key。
func (s *InternalKeyService) create(ctx context.Context, userID int64) (*upstreamservice.APIKey, error) {
	var groupID *int64
	if s.groupID != nil {
		resolved, err := s.groupID(ctx)
		if err != nil {
			return nil, dkdomain.NewError(dkdomain.ErrCodeInternal).WithCause(err)
		}
		groupID = resolved
	}

	created, err := s.keys.Create(ctx, userID, upstreamservice.CreateAPIKeyRequest{
		Name:    InternalAPIKeyName,
		GroupID: groupID,
		// 额度 / 有效期 / 窗口限额全部留 0 = 不限。
		// 出图的钱由 designkit 自己的冻结账管（决策 14），
		// 这里再加一层上限只会出现「余额够但 Key 限额挡住」的迷惑现象。
	})
	if err != nil {
		// 绑不上分组 = 管理员还没把这个人加进 designkit 专属分组。
		// 这是运营最可能撞到的一种，必须给中文出路。
		if errors.Is(err, upstreamservice.ErrGroupNotAllowed) {
			return nil, dkdomain.NewError(dkdomain.ErrCodeAPIKeyMissing).WithCause(err)
		}
		return nil, dkdomain.NewError(dkdomain.ErrCodeAPIKeyMissing).WithCause(err)
	}
	if created == nil || created.ID <= 0 {
		return nil, dkdomain.NewError(dkdomain.ErrCodeAPIKeyMissing).
			WithCause(errors.New("designkit: 上游建 Key 返回空"))
	}

	// Create 返回的对象是它自己拼的，**没有 User / Group 关联**
	// （api_key_service.go:531-546 只填了标量字段），所以必须再 GetByID 取一次。
	key, err := s.loadUsable(ctx, created.ID, userID)
	if err != nil {
		return nil, err
	}
	s.remember(userID, key.ID)
	return key, nil
}

// ---- 进程内小状态 ----

func (s *InternalKeyService) userLock(userID int64) *sync.Mutex {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.locks == nil {
		s.locks = make(map[int64]*sync.Mutex)
	}
	lock, ok := s.locks[userID]
	if !ok {
		lock = &sync.Mutex{}
		s.locks[userID] = lock
	}
	return lock
}

func (s *InternalKeyService) cachedKeyID(userID int64) (int64, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	id, ok := s.cache[userID]
	return id, ok
}

func (s *InternalKeyService) remember(userID, keyID int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cache == nil {
		s.cache = make(map[int64]int64)
	}
	s.cache[userID] = keyID
}

func (s *InternalKeyService) forget(userID int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.cache, userID)
}

// ⚠ 多副本部署的已知缺口：
// 上面的 per-user mutex 只在**一个进程内**有效。两个副本同时收到同一个人的
// 首次提交时，可能各建一把内部 Key。后果不是重复扣钱（幂等键是
// (request_id, api_key_id)，而一张图只会被一个 worker 领走、只发一次），
// 而是这个人的用量会分散在两把 Key 上，查账时要合并看。
// 真要根治得在数据库上加唯一约束或用 advisory lock；本轮先记在这里，
// 因为群晖阶段就是单副本（CLAUDE.md 决策 13）。
