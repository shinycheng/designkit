package service

// ============================================================================
// 灵感库：从 YouMind 开源库拉数据
// ============================================================================
//
// 上游是 11 个 JSON 文件 + 1 个 manifest.json，放在 GitHub 的 raw 地址上。
// 一轮下载大约 1.4 万条、十几秒。
//
// 【全部下完并解析成功，才算这一轮拿到了数据】
// 中途某个文件 404 或者返回了代理的错误页，就整轮失败、什么都不写库。
// 半截数据的后果是「某几个分类突然空了」，运营完全看不出发生了什么。
//
// 【HTTP 客户端是**传进来**的，这里不自己建】
// 因为要不要走代理是 designkit_settings 里的配置，由 inspiration_sync.go 决定。
// 更要紧的是：**绝不能动 http.DefaultTransport，也绝不能设 HTTP_PROXY 环境变量**。
// 老代码的注释原话是「不能用全局 urllib 代理或环境变量——那会把生图请求也带进代理」，
// 而生图网关是局域网地址，走代理必然连不上。Go 侧同理。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
)

const (
	// inspirationUserAgent 上游 GitHub 对没有 UA 的请求会返回 403。
	inspirationUserAgent = "DesignKit/1.0 (+https://github.com/YouMind-OpenLab/gpt-image-2-prompts-search)"

	// maxInspirationFileBytes 单个 JSON 文件的大小上限。
	// 最大的分类文件目前 20MB 左右，64MB 留足余量；
	// 有上限是为了防「代理返回了一个无限流」这种情况把内存吃光。
	maxInspirationFileBytes = 64 << 20

	// manifestName manifest 文件名（不带 .json）。
	manifestName = "manifest"
)

// UpstreamPrompt 是上游的一条提示词（已按 id 合并了多个分类文件）。
type UpstreamPrompt struct {
	// ID 上游的唯一编号，source_ref 由它拼出来。
	ID int64
	// Title 标题，上游 100% 覆盖，本身就是一句话概括。
	Title string
	// Content 提示词正文，里面可能含 {argument ...} 变量。
	Content string
	// SourceMedia 效果图外链，第一张能用的当预览图。
	SourceMedia []string
	// Slugs 这条命中的**全部**分类，按 inspirationCategories 的顺序。
	// 主分类由 primaryCategorySlug 从中挑一个。
	Slugs []string
}

// InspirationSnapshot 是一轮下载的完整结果。
type InspirationSnapshot struct {
	// UpdatedAt 上游 manifest 里写的快照时间，原样透出给界面看。
	UpdatedAt string
	// Prompts 去重合并后的全部条目，按 ID 升序。
	Prompts []UpstreamPrompt
}

// InspirationFetcher 拉取上游数据。抽成接口只为一件事：
// 单元测试塞一个假的进去，不联网、不花钱（CLAUDE.md 第三节）。
type InspirationFetcher interface {
	// Fetch 下载并解析全部分类文件。client 由调用方按「要不要走代理」建好。
	Fetch(ctx context.Context, client *http.Client) (*InspirationSnapshot, error)
	// Probe 只拉 manifest 验证上游可达，返回快照时间。
	// 给设置页的「测试连接」用：整轮同步要十几秒，让人等那么久才发现代理不通太浪费。
	Probe(ctx context.Context, client *http.Client) (string, error)
}

// youMindFetcher 是 InspirationFetcher 的正式实现。
type youMindFetcher struct {
	// baseURL 上游 references 目录。测试会把它指向 httptest 起的本地服务。
	baseURL string
}

// NewYouMindFetcher 建正式的数据源。baseURL 留空用 YouMindRawBase。
func NewYouMindFetcher(baseURL string) InspirationFetcher {
	base := strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if base == "" {
		base = YouMindRawBase
	}
	return &youMindFetcher{baseURL: base}
}

// upstreamID 是上游那个 id 字段。
//
// 单独造一个类型只为一件事：**认不出来的 id 变成 0（这一条被跳过），
// 而不是让整个文件解析失败**。上游偶尔把 id 写成字符串 "123"，
// 直接用 int64 接会让整轮同步报错、1.4 万条一条都进不去 ——
// 为了一条脏数据丢掉全部，不划算。
type upstreamID int64

// UnmarshalJSON 数字和字符串两种写法都吃，**永远不返回错误**。
func (u *upstreamID) UnmarshalJSON(raw []byte) error {
	text := strings.Trim(strings.TrimSpace(string(raw)), `"`)
	if text == "" || text == "null" {
		*u = 0
		return nil
	}
	n, err := strconv.ParseInt(text, 10, 64)
	if err != nil {
		*u = 0
		return nil
	}
	*u = upstreamID(n)
	return nil
}

// upstreamItem 是上游 JSON 里的一条。字段名照抄上游，不要改。
type upstreamItem struct {
	ID          upstreamID `json:"id"`
	Title       string     `json:"title"`
	Content     string     `json:"content"`
	SourceMedia []string   `json:"sourceMedia"`
}

// upstreamManifest manifest.json，目前只用得上快照时间。
type upstreamManifest struct {
	UpdatedAt string `json:"updatedAt"`
}

func (f *youMindFetcher) fileURL(name string) string {
	return f.baseURL + "/" + name + ".json"
}

// getJSON 下载一个文件并解析。任何一步失败都带上文件名，
// 这样日志里能直接看出是哪个分类坏了。
func (f *youMindFetcher) getJSON(ctx context.Context, client *http.Client, name string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, f.fileURL(name), nil)
	if err != nil {
		return fmt.Errorf("拼接 %s.json 的地址失败：%w", name, err)
	}
	req.Header.Set("User-Agent", inspirationUserAgent)
	req.Header.Set("Accept", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("下载 %s.json 失败：%w", name, err)
	}
	defer func() {
		_ = resp.Body.Close()
	}()

	if resp.StatusCode != http.StatusOK {
		// 读一点点正文帮助排障（代理挡在前面时这里往往是一段 HTML）。
		peek, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
		return fmt.Errorf("下载 %s.json 失败：上游返回 %d %s",
			name, resp.StatusCode, collapseSpaces(string(peek)))
	}

	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxInspirationFileBytes+1))
	if err != nil {
		return fmt.Errorf("读取 %s.json 失败：%w", name, err)
	}
	if len(raw) > maxInspirationFileBytes {
		return fmt.Errorf("%s.json 超过 %d MB，已放弃（上游格式可能变了）",
			name, maxInspirationFileBytes>>20)
	}
	if err := json.Unmarshal(raw, out); err != nil {
		// 最常见的原因是代理返回了一个登录页 / 错误页，而不是真的 JSON。
		return fmt.Errorf("%s.json 的内容不完整或者不是合法数据（如果配了代理，多半是代理返回了错误页）：%w", name, err)
	}
	return nil
}

// Probe 只拉 manifest。
func (f *youMindFetcher) Probe(ctx context.Context, client *http.Client) (string, error) {
	var manifest upstreamManifest
	if err := f.getJSON(ctx, client, manifestName, &manifest); err != nil {
		return "", err
	}
	if strings.TrimSpace(manifest.UpdatedAt) == "" {
		return "未知", nil
	}
	return manifest.UpdatedAt, nil
}

// Fetch 拉 manifest + 11 个分类文件。
//
// manifest 放在最前面：它最小，代理不通时几秒内就能报错，
// 不用等一个 20MB 的分类文件慢慢超时。
func (f *youMindFetcher) Fetch(ctx context.Context, client *http.Client) (*InspirationSnapshot, error) {
	updatedAt, err := f.Probe(ctx, client)
	if err != nil {
		return nil, err
	}

	// merged 按上游 id 合并：同一条常同时出现在好几个分类文件里。
	merged := make(map[int64]*UpstreamPrompt, 16384)
	for _, category := range inspirationCategories {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		var items []upstreamItem
		if err := f.getJSON(ctx, client, category.Slug, &items); err != nil {
			// 一个文件失败就整轮失败，绝不半截入库。
			return nil, err
		}
		for _, item := range items {
			id := int64(item.ID)
			if id <= 0 {
				// 没有编号就没有幂等键，下次同步会再插一条重复的，直接丢掉。
				continue
			}
			if existing, ok := merged[id]; ok {
				existing.Slugs = append(existing.Slugs, category.Slug)
				continue
			}
			merged[id] = &UpstreamPrompt{
				ID:          id,
				Title:       item.Title,
				Content:     item.Content,
				SourceMedia: item.SourceMedia,
				Slugs:       []string{category.Slug},
			}
		}
	}

	prompts := make([]UpstreamPrompt, 0, len(merged))
	for _, p := range merged {
		prompts = append(prompts, *p)
	}
	// 按 id 升序：map 遍历顺序是随机的，不排的话每轮同步的写入顺序都不一样，
	// 日志和进度完全没法比对。
	sort.Slice(prompts, func(i, j int) bool { return prompts[i].ID < prompts[j].ID })

	if len(prompts) == 0 {
		// 一条都没有多半是上游改了目录结构。宁可这一轮什么都不做，
		// 也不能让「库里还有上次同步的 1.4 万条」变成「界面上一条都没有」。
		return nil, errors.New("上游一条提示词都没返回，这一轮不写库（上游格式可能变了）")
	}
	return &InspirationSnapshot{UpdatedAt: updatedAt, Prompts: prompts}, nil
}
