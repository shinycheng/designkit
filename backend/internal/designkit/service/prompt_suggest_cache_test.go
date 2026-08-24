//go:build unit

package service

// prompt_suggest_cache_test.go —— 推荐结果缓存的单测：命中 / 过期 / force / LRU 淘汰。
//
// 这里守的是钱：一次推荐 = 三四趟真实的模型对话（$0.09~$0.34）。
// 「命中缓存」唯一说得通的判据是 **chat.calls 没涨** —— 少调一趟就是少花一趟的钱；
// 反过来「该失效没失效」的坏处是运营改了库、换了天，拿到的还是昨天的答案。

import (
	"context"
	"testing"
	"time"
)

// sgCacheReplies 一轮完整推荐的三趟预置回答（判分类 → 细选 → 合成）。
func sgCacheReplies(finalPrompt string) []string {
	return []string{
		`{"category":"ecommerce-main-image"}`,
		`[2,4,6,8,10]`,
		finalPrompt,
	}
}

// ---------------------------------------------------------------------------
// 命中
// ---------------------------------------------------------------------------

// 相同输入第二次直接返回缓存：一趟模型都不调，结果带 CachedAt。
func TestSuggest_CacheHitSkipsChat(t *testing.T) {
	chat := &sgFakeChat{replies: sgCacheReplies("合成出来的最终提示词")}
	catalog := &sgFakeCatalog{categories: sgCategories(), prompts: sgPrompts(1, 50)}
	svc := sgFixture(t, chat, catalog, &sgFakeAssets{})

	first, err := svc.Suggest(context.Background(), sgInput())
	if err != nil {
		t.Fatalf("第一次 Suggest 失败: %v", err)
	}
	if !first.CachedAt.IsZero() {
		t.Fatal("现算的结果 CachedAt 必须是零值，否则前端会把新鲜结果标成「缓存的」")
	}
	callsAfterFirst := chat.calls

	second, err := svc.Suggest(context.Background(), sgInput())
	if err != nil {
		t.Fatalf("第二次 Suggest 失败: %v", err)
	}
	if chat.calls != callsAfterFirst {
		t.Fatalf("命中缓存还调了模型（%d → %d 趟）—— 这正是缓存要省掉的钱", callsAfterFirst, chat.calls)
	}
	if second.Prompt != first.Prompt {
		t.Fatalf("缓存返回的提示词变了：%q != %q", second.Prompt, first.Prompt)
	}
	if len(second.Candidates) != len(first.Candidates) {
		t.Fatalf("缓存返回的参考条数变了：%d != %d", len(second.Candidates), len(first.Candidates))
	}
	if second.CachedAt.IsZero() {
		t.Fatal("命中缓存必须带 CachedAt —— 前端靠它显示「这是 X 前的推荐结果」")
	}
}

// 换任何一个输入（商品特点 / 分类）都不该命中 —— 那是新问题，该重新问。
func TestSuggest_CacheMissesOnDifferentInput(t *testing.T) {
	chat := &sgFakeChat{replies: append(
		sgCacheReplies("第一条"),
		sgCacheReplies("第二条")...,
	)}
	catalog := &sgFakeCatalog{categories: sgCategories(), prompts: sgPrompts(1, 50)}
	svc := sgFixture(t, chat, catalog, &sgFakeAssets{})

	if _, err := svc.Suggest(context.Background(), sgInput()); err != nil {
		t.Fatalf("第一次 Suggest 失败: %v", err)
	}
	callsAfterFirst := chat.calls

	in := sgInput()
	in.Features = "换了一句商品特点"
	res, err := svc.Suggest(context.Background(), in)
	if err != nil {
		t.Fatalf("第二次 Suggest 失败: %v", err)
	}
	if chat.calls == callsAfterFirst {
		t.Fatal("改了商品特点还命中缓存 —— 运营改什么都拿到同一个答案")
	}
	if !res.CachedAt.IsZero() {
		t.Fatal("现算的结果不该带 CachedAt")
	}
}

// 同一批图换个顺序还是同一批图，应该命中（键里的 uid 列表是排过序的）。
func TestSuggest_CacheKeyIgnoresAssetOrder(t *testing.T) {
	chat := &sgFakeChat{replies: sgCacheReplies("最终提示词")}
	catalog := &sgFakeCatalog{categories: sgCategories(), prompts: sgPrompts(1, 50)}
	svc := sgFixture(t, chat, catalog, &sgFakeAssets{})

	in := sgInput()
	in.ExtraAssetUIDs = []string{"a2", "a3"}
	if _, err := svc.Suggest(context.Background(), in); err != nil {
		t.Fatalf("第一次 Suggest 失败: %v", err)
	}
	callsAfterFirst := chat.calls

	in.ExtraAssetUIDs = []string{"a3", "a2"}
	res, err := svc.Suggest(context.Background(), in)
	if err != nil {
		t.Fatalf("第二次 Suggest 失败: %v", err)
	}
	if chat.calls != callsAfterFirst {
		t.Fatal("同一批图换个顺序就不命中 —— 排序进键就是为了防这个")
	}
	if res.CachedAt.IsZero() {
		t.Fatal("应该是缓存命中")
	}
}

// 不同用户不能共用缓存：asset 归属是按用户校验的，共用等于跳过归属校验。
func TestSuggest_CacheIsPerUser(t *testing.T) {
	chat := &sgFakeChat{replies: append(
		sgCacheReplies("第一条"),
		sgCacheReplies("第二条")...,
	)}
	catalog := &sgFakeCatalog{categories: sgCategories(), prompts: sgPrompts(1, 50)}
	svc := sgFixture(t, chat, catalog, &sgFakeAssets{})

	if _, err := svc.Suggest(context.Background(), sgInput()); err != nil {
		t.Fatalf("第一次 Suggest 失败: %v", err)
	}
	callsAfterFirst := chat.calls

	in := sgInput()
	in.UserID = 43
	if _, err := svc.Suggest(context.Background(), in); err != nil {
		t.Fatalf("第二个用户 Suggest 失败: %v", err)
	}
	if chat.calls == callsAfterFirst {
		t.Fatal("另一个用户命中了别人的缓存 —— 归属校验被绕过了")
	}
}

// ---------------------------------------------------------------------------
// 过期
// ---------------------------------------------------------------------------

// 过了 TTL 的缓存视同不存在：重新调模型，结果是新鲜的。
func TestSuggest_CacheExpires(t *testing.T) {
	chat := &sgFakeChat{replies: append(
		sgCacheReplies("第一条"),
		sgCacheReplies("第二条")...,
	)}
	catalog := &sgFakeCatalog{categories: sgCategories(), prompts: sgPrompts(1, 50)}
	svc := sgFixture(t, chat, catalog, &sgFakeAssets{})

	if _, err := svc.Suggest(context.Background(), sgInput()); err != nil {
		t.Fatalf("第一次 Suggest 失败: %v", err)
	}
	callsAfterFirst := chat.calls

	// 把缓存的时钟拨快到 TTL 之后（只动测试注入的时钟，不 sleep）。
	base := time.Now()
	svc.cache.now = func() time.Time { return base.Add(suggestCacheTTL + time.Minute) }

	res, err := svc.Suggest(context.Background(), sgInput())
	if err != nil {
		t.Fatalf("过期后的 Suggest 失败: %v", err)
	}
	if chat.calls == callsAfterFirst {
		t.Fatal("过期的缓存还在命中 —— 灵感库每天同步，隔天必须按新库重挑")
	}
	if !res.CachedAt.IsZero() {
		t.Fatal("过期后重算的结果不该带 CachedAt")
	}
	if res.Prompt != "第二条" {
		t.Fatalf("过期后应拿到新答案，实际 %q", res.Prompt)
	}
}

// ---------------------------------------------------------------------------
// force（「重新推荐」按钮）
// ---------------------------------------------------------------------------

// Force 跳过缓存拿新答案，且新答案回写缓存：下一次不带 force 命中的是它。
func TestSuggest_ForceBypassesCacheAndRefreshesIt(t *testing.T) {
	chat := &sgFakeChat{replies: append(
		sgCacheReplies("旧答案"),
		sgCacheReplies("新答案")...,
	)}
	catalog := &sgFakeCatalog{categories: sgCategories(), prompts: sgPrompts(1, 50)}
	svc := sgFixture(t, chat, catalog, &sgFakeAssets{})

	if _, err := svc.Suggest(context.Background(), sgInput()); err != nil {
		t.Fatalf("第一次 Suggest 失败: %v", err)
	}
	callsAfterFirst := chat.calls

	in := sgInput()
	in.Force = true
	forced, err := svc.Suggest(context.Background(), in)
	if err != nil {
		t.Fatalf("force 的 Suggest 失败: %v", err)
	}
	if chat.calls == callsAfterFirst {
		t.Fatal("force 没有跳过缓存 —— 运营点「重新推荐」是明确要一个新答案")
	}
	if forced.Prompt != "新答案" {
		t.Fatalf("force 应拿到新答案，实际 %q", forced.Prompt)
	}
	if !forced.CachedAt.IsZero() {
		t.Fatal("force 现算的结果不该带 CachedAt")
	}
	callsAfterForce := chat.calls

	// 不带 force 再来一次：命中的必须是 force 刚写回去的那份新答案。
	third, err := svc.Suggest(context.Background(), sgInput())
	if err != nil {
		t.Fatalf("第三次 Suggest 失败: %v", err)
	}
	if chat.calls != callsAfterForce {
		t.Fatal("force 的新结果没回写缓存 —— 下一次相同输入又要重新花钱")
	}
	if third.Prompt != "新答案" {
		t.Fatalf("命中的应是 force 回写的新答案，实际 %q", third.Prompt)
	}
	if third.CachedAt.IsZero() {
		t.Fatal("命中缓存必须带 CachedAt")
	}
}

// ---------------------------------------------------------------------------
// 缓存本体：LRU 淘汰、键的构成
// ---------------------------------------------------------------------------

// 容量满了淘汰**最久没用过**的那条；刚用过的（Get 过的）活下来。
func TestSuggestCache_LRUEviction(t *testing.T) {
	c := newSuggestCache(2, time.Hour)
	c.Put("a", SuggestResult{Prompt: "A"})
	c.Put("b", SuggestResult{Prompt: "B"})

	// 用一下 a：它变成「最近用过」，b 变成最老的。
	if _, ok := c.Get("a"); !ok {
		t.Fatal("a 刚放进去就查不到")
	}

	c.Put("c", SuggestResult{Prompt: "C"})

	if _, ok := c.Get("b"); ok {
		t.Fatal("容量 2 放第 3 条时该淘汰最久没用的 b")
	}
	if _, ok := c.Get("a"); !ok {
		t.Fatal("a 刚被用过，不该被淘汰")
	}
	if _, ok := c.Get("c"); !ok {
		t.Fatal("刚放进去的 c 不该被淘汰")
	}
}

// 命中返回的是副本：调用方改它不能脏了缓存里那份。
func TestSuggestCache_GetReturnsCopy(t *testing.T) {
	c := newSuggestCache(2, time.Hour)
	c.Put("k", SuggestResult{
		Prompt:     "原文",
		Candidates: []SuggestCandidate{{UID: "p001", Title: "标题"}},
	})

	first, ok := c.Get("k")
	if !ok {
		t.Fatal("放进去的查不到")
	}
	first.Prompt = "被调用方改了"
	first.Candidates[0].Title = "也被改了"

	second, ok := c.Get("k")
	if !ok {
		t.Fatal("第二次查不到")
	}
	if second.Prompt != "原文" || second.Candidates[0].Title != "标题" {
		t.Fatalf("缓存被调用方的修改污染了：%+v", second)
	}
}

// 键的构成：每个输入都参与；同一批图换顺序、商品特点带首尾空白不影响。
func TestSuggestCacheKey_Components(t *testing.T) {
	base := suggestCacheKey(1, []string{"u1", "u2"}, "cat", "feat", "model")

	if got := suggestCacheKey(1, []string{"u2", "u1"}, "cat", "feat", "model"); got != base {
		t.Fatal("图的顺序不该影响键")
	}
	if got := suggestCacheKey(1, []string{"u1", "u2"}, "cat", "  feat  ", "model"); got != base {
		t.Fatal("商品特点的首尾空白不该影响键")
	}
	if got := suggestCacheKey(2, []string{"u1", "u2"}, "cat", "feat", "model"); got == base {
		t.Fatal("不同用户必须不同键")
	}
	if got := suggestCacheKey(1, []string{"u1"}, "cat", "feat", "model"); got == base {
		t.Fatal("少一张图必须不同键")
	}
	if got := suggestCacheKey(1, []string{"u1", "u2"}, "cat2", "feat", "model"); got == base {
		t.Fatal("不同分类必须不同键")
	}
	if got := suggestCacheKey(1, []string{"u1", "u2"}, "cat", "feat2", "model"); got == base {
		t.Fatal("不同商品特点必须不同键")
	}
	if got := suggestCacheKey(1, []string{"u1", "u2"}, "cat", "feat", "model2"); got == base {
		t.Fatal("不同模型必须不同键（换模型时旧缓存要整体失效）")
	}
	// 变长的 uid 列表不能和后面的字段串出歧义：
	// [a,b] + 分类 c 和 [a] + 分类 b …… 只要有一处这么串就会互相污染。
	left := suggestCacheKey(1, []string{"a", "b"}, "c", "f", "m")
	right := suggestCacheKey(1, []string{"a"}, "b", "c", "f")
	if left == right {
		t.Fatal("uid 列表和分类/特点串出了同一个键 —— 键的编码有歧义")
	}
}
