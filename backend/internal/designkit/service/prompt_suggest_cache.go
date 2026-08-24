package service

// prompt_suggest_cache.go —— 推荐结果的进程内缓存。
//
// 为什么要有它：一次推荐是三四趟真实的模型对话，$0.09~$0.34、一两分钟。
// 输入完全相同（同一批商品图 + 同一个分类 + 同一句商品特点）时答案可以复用——
// 不复用的话，运营手滑连点、刷新页面后再点，每一下都是真金白银。
//
// 为什么进程内 map 就够：整个系统只允许跑一个后端实例（CLAUDE.md 第七节，
// worker 没有分布式锁，多副本本来就不行）。单实例约束是既有决策，
// 缓存搭它的便车，不引 Redis、不落库。代价是重启后缓存清空——
// 那只是「下一次推荐重新花一次钱」，不是事故。
//
// 淘汰两条线：
//   - TTL 24 小时：灵感库每天同步一次，隔天的推荐该按新库重挑；
//   - 上限 200 条 LRU：一条结果几 KB，200 条封顶几 MB，够全公司点一天。
//
// ⚠ 「重新推荐」按钮走 Force 跳过 Get（运营明确要新答案），
// 但新结果照样 Put 回来 —— 下一次相同输入命中的是这份最新的。

import (
	"container/list"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	// suggestCacheTTL 一条缓存能活多久。
	suggestCacheTTL = 24 * time.Hour

	// suggestCacheCapacity 最多存多少条，超了按 LRU 淘汰最久没用过的。
	suggestCacheCapacity = 200
)

// suggestCacheKey 算缓存键。
//
// 参与的字段：用户 + 排序后的商品图 uid 列表 + 分类 + 商品特点（去首尾空白）+ 模型名。
//
//   - 图的**顺序不参与**：同一批图换个勾选顺序还是同一批图，答案没理由不同；
//   - 用户参与：asset 归属是按用户校验的，跨用户共享缓存等于跳过归属校验；
//   - 模型名参与：哪天换了对话模型，旧缓存整体自动失效，不用手动清。
//
// 每一段都以 NUL 结尾、uid 列表前面带条数 —— 不这么做的话，
// 变长的 uid 列表会和后面的分类串出「a,b + c」「a + b,c」算出同一个键的歧义。
func suggestCacheKey(userID int64, assetUIDs []string, categorySlug, features, model string) string {
	uids := make([]string, 0, len(assetUIDs))
	for _, uid := range assetUIDs {
		uid = strings.TrimSpace(uid)
		if uid != "" {
			uids = append(uids, uid)
		}
	}
	sort.Strings(uids)

	h := sha256.New()
	writePart := func(s string) {
		_, _ = h.Write([]byte(s))
		_, _ = h.Write([]byte{0})
	}
	var userBuf [8]byte
	binary.BigEndian.PutUint64(userBuf[:], uint64(userID))
	_, _ = h.Write(userBuf[:])
	var countBuf [8]byte
	binary.BigEndian.PutUint64(countBuf[:], uint64(len(uids)))
	_, _ = h.Write(countBuf[:])
	for _, uid := range uids {
		writePart(uid)
	}
	writePart(strings.TrimSpace(categorySlug))
	writePart(strings.TrimSpace(features))
	writePart(strings.TrimSpace(model))
	return hex.EncodeToString(h.Sum(nil))
}

// suggestCacheEntry 缓存里的一条。
type suggestCacheEntry struct {
	key string
	// result 存的是「新鲜」形态（CachedAt 恒为零值），命中时由 Get 填时间。
	result SuggestResult
	// savedAt 这条结果生成（写入）的时间。TTL 按它算，命中时它就是 CachedAt。
	savedAt time.Time
}

// suggestCache 定长 LRU + TTL。所有方法并发安全。
type suggestCache struct {
	mu       sync.Mutex
	ttl      time.Duration
	capacity int
	// now 可注入的时钟，只为让单测拨快时间验 TTL。业务代码不要碰它。
	now func() time.Time
	// order 队首 = 最近用过。淘汰从队尾拿。
	order *list.List
	items map[string]*list.Element
}

func newSuggestCache(capacity int, ttl time.Duration) *suggestCache {
	return &suggestCache{
		ttl:      ttl,
		capacity: capacity,
		now:      time.Now,
		order:    list.New(),
		items:    make(map[string]*list.Element, capacity),
	}
}

// Get 查一条。命中返回**副本**（调用方改它不会脏了缓存），
// CachedAt 填成这条结果最初生成的时间。过期视同不存在，顺手删掉。
func (c *suggestCache) Get(key string) (*SuggestResult, bool) {
	if c == nil {
		return nil, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()

	el, ok := c.items[key]
	if !ok {
		return nil, false
	}
	entry, entryOK := el.Value.(*suggestCacheEntry)
	if !entryOK {
		return nil, false
	}
	if c.now().Sub(entry.savedAt) > c.ttl {
		c.order.Remove(el)
		delete(c.items, key)
		return nil, false
	}
	c.order.MoveToFront(el)
	out := copySuggestResult(entry.result)
	out.CachedAt = entry.savedAt
	return &out, true
}

// Put 存一条（同键覆盖并重置 TTL）。满了先淘汰最久没用过的。
func (c *suggestCache) Put(key string, result SuggestResult) {
	if c == nil || c.capacity <= 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()

	if el, ok := c.items[key]; ok {
		if entry, entryOK := el.Value.(*suggestCacheEntry); entryOK {
			entry.result = copySuggestResult(result)
			entry.savedAt = c.now()
			c.order.MoveToFront(el)
			return
		}
		// 类型不对只可能是编程错误：丢掉这条坏项，往下走当新插入。
		c.order.Remove(el)
		delete(c.items, key)
	}
	for c.order.Len() >= c.capacity {
		oldest := c.order.Back()
		if oldest == nil {
			break
		}
		c.order.Remove(oldest)
		if oldestEntry, oldestOK := oldest.Value.(*suggestCacheEntry); oldestOK {
			delete(c.items, oldestEntry.key)
		}
	}
	c.items[key] = c.order.PushFront(&suggestCacheEntry{
		key:     key,
		result:  copySuggestResult(result),
		savedAt: c.now(),
	})
}

// copySuggestResult 深拷贝一份（Candidates 是切片，浅拷贝会共享底层数组）。
// 存进去的一律抹掉 CachedAt：缓存里只有「新鲜」形态，时间由 Get 统一填。
func copySuggestResult(in SuggestResult) SuggestResult {
	out := in
	out.CachedAt = time.Time{}
	out.Candidates = append([]SuggestCandidate(nil), in.Candidates...)
	return out
}
