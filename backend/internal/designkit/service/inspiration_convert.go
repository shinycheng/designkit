package service

// ============================================================================
// 灵感库：上游语法 → 本项目格式的转换
// ============================================================================
//
// 这一份是从 legacy-python 分支的 backend/app/services/inspiration.py **逐条移植**
// 过来的，不是重写。那些正则和边界是在 1.4 万条真实语料上试出来的，
// 凭感觉重写必然漏（老测试 tests/test_inspiration_convert.py 的用例
// 已经全部翻成了 inspiration_convert_test.go 的表驱动用例）。
//
// 移植时**故意保留**的三个细节：
//  1. 引号有两种形态 —— 裸引号 name="..." 和整体被转义的 name=\"...\"（上游约 8%
//     的条目是后者）。正则必须两种都吃，只认一种会让上千条提示词的变量原样留在正文里，
//     运营看到的是一句 {argument name=...} 的乱码。
//  2. 变量名要清洗成占位符能用的形式（空格、标点、大小写），中文原样保留。
//  3. 同一个变量名多次出现共用一个占位符；不同变量清洗后撞名的自动加序号。
//
// 移植时**改掉**的一个地方（这里说清楚，免得以后有人以为是漏了）：
// 老代码给每个变量存了 name / label / type / default / required 五个字段，
// 本项目的 designkit_prompts.variables 只有 name + example 两个
// （domain.PromptVariable，那个类型不归本轮改）。所以：
//
//	name    ← 清洗后的占位符键（正文里的 {xxx} 就是它，必须一致）
//	example ← 上游的 default，当输入框的提示文字
//
// 丢掉的是「原始标签」（"hair color" 清洗成 hair_color 之后拿不回来了）。
// 中文标签清洗后不变，所以对中文语料没有损失。

import (
	"fmt"
	"regexp"
	"strings"
	"unicode"

	dkdomain "github.com/Wei-Shaw/sub2api/internal/designkit/domain"
)

// ----------------------------------------------------------------------------
// 上游来源
// ----------------------------------------------------------------------------

const (
	// YouMindRawBase 上游开源提示词库的原始文件地址（CC BY 4.0，每天更新两次）。
	// 仓库：github.com/YouMind-OpenLab/gpt-image-2-prompts-search
	YouMindRawBase = "https://raw.githubusercontent.com/" +
		"YouMind-OpenLab/gpt-image-2-prompts-search/main/references"

	// PromptSourceRefPrefix 同步进来的提示词的 source_ref 前缀，
	// 完整形态是 youmind:<上游 id>。**这是同步的幂等键**，改了它下一次同步
	// 会把 1.4 万条全部当成新条目再插一遍。
	PromptSourceRefPrefix = "youmind:"

	// PromptAttributionText 署名。上游是 CC BY 4.0，必须注明来源。
	// 界面上在灵感库页面显示一次即可，不必逐条重复。
	PromptAttributionText = "提示词来自 YouMind 开源提示词库，按 CC BY 4.0 授权使用"

	// youMindWebURLFormat 单条提示词在 YouMind 官网上的页面，做「查看原文」链接。
	youMindWebURLFormat = "https://youmind.com/gpt-image-2-prompts?id=%s"
)

// PromptSourceURL 由 source_ref 反推出「查看原文」的链接。
// 不是 youmind: 开头的（运营自己存的）返回空串，界面据此不显示这个链接。
func PromptSourceURL(sourceRef string) string {
	ref := strings.TrimSpace(sourceRef)
	if !strings.HasPrefix(ref, PromptSourceRefPrefix) {
		return ""
	}
	id := strings.TrimSpace(strings.TrimPrefix(ref, PromptSourceRefPrefix))
	if id == "" {
		return ""
	}
	return fmt.Sprintf(youMindWebURLFormat, id)
}

// ----------------------------------------------------------------------------
// 分类
// ----------------------------------------------------------------------------

// inspirationCategory 一个灵感库分类。
type inspirationCategory struct {
	// Slug 稳定的英文标识，也是上游 references/<slug>.json 的文件名。
	Slug string
	// NameZH 界面默认显示这个。
	NameZH string
	// NameEN 英文名（CLAUDE.md：每条文案都要有英文，措辞不必推敲）。
	NameEN string
}

// inspirationCategories 上游的 11 个分类文件，**顺序即上游 manifest 的顺序**。
// 同步时会按这个顺序逐个下载，sort_order 也按这个顺序生成。
var inspirationCategories = []inspirationCategory{
	{Slug: "profile-avatar", NameZH: "头像与个人形象", NameEN: "Profile & Avatar"},
	{Slug: "social-media-post", NameZH: "社交媒体帖子", NameEN: "Social Media Post"},
	{Slug: "infographic-edu-visual", NameZH: "信息图与教育视觉", NameEN: "Infographic & Education"},
	{Slug: "youtube-thumbnail", NameZH: "视频封面", NameEN: "Video Thumbnail"},
	{Slug: "comic-storyboard", NameZH: "漫画与分镜", NameEN: "Comic & Storyboard"},
	{Slug: "product-marketing", NameZH: "产品营销", NameEN: "Product Marketing"},
	{Slug: "ecommerce-main-image", NameZH: "电商主图", NameEN: "E-commerce Main Image"},
	{Slug: "game-asset", NameZH: "游戏素材", NameEN: "Game Asset"},
	{Slug: "poster-flyer", NameZH: "海报与传单", NameEN: "Poster & Flyer"},
	{Slug: "app-web-design", NameZH: "应用与网页设计", NameEN: "App & Web Design"},
	{Slug: "others", NameZH: "其他", NameEN: "Others"},
}

// primaryCategoryOrder 决定「一条提示词同时出现在好几个分类文件里时，归到哪个分类」。
//
// **这是本次移植唯一一处刻意改掉老逻辑的地方，理由值得写长一点。**
//
// 老代码取「第一个命中的分类」（也就是上游 manifest 的顺序），结果是：
// 「电商主图」文件里的 416 条，有 398 条因为同时出现在排在它前面的
// 「社交媒体帖子」「信息图」里而被抢走，运营点开「电商主图」只能看到 18 条。
// 老代码的补救是加一列 source_slugs 存全部标签、按标签筛选；
// 我们的表（9001 迁移，永不可改）没有这一列，只有一个 category_id。
//
// 所以改成「按我们自己的贴合度排序取最靠前的那个」：
// 我们是电商出图工具，一条同时属于「电商主图」和「社交媒体帖子」的提示词，
// 归到「电商主图」比归到「社交媒体帖子」更对，也更有用。
//
// 代价：「社交媒体帖子」这类靠后的分类条数会比上游少。可以接受 ——
// 关键词搜索是跨分类的，不会因此搜不到。
var primaryCategoryOrder = []string{
	"ecommerce-main-image",
	"product-marketing",
	"poster-flyer",
	"social-media-post",
	"infographic-edu-visual",
	"app-web-design",
	"youtube-thumbnail",
	"profile-avatar",
	"comic-storyboard",
	"game-asset",
	"others",
}

// primaryCategorySlug 从一条提示词命中的全部分类里挑出主分类。
// 传进来的 slugs 一个都不认识时返回 "others"（上游新增分类时不至于整条丢掉）。
func primaryCategorySlug(slugs []string) string {
	have := make(map[string]bool, len(slugs))
	for _, s := range slugs {
		have[strings.TrimSpace(s)] = true
	}
	for _, want := range primaryCategoryOrder {
		if have[want] {
			return want
		}
	}
	return "others"
}

// ----------------------------------------------------------------------------
// {argument name=.. default=..} → {占位符}
// ----------------------------------------------------------------------------

// argumentPattern 逐字移植自老代码的 _ARG_RE。
//
//	\{argument\s+name=\\?"((?:[^"\\]|\\.)+?)\\?"(?:\s+default=\\?"((?:[^"\\]|\\.)*?)\\?")?\s*\}
//
// 两处 `\\?` 就是「兼容转义引号」：上游约 8% 的条目整体被转义过一遍，
// 长成 {argument name=\"发色\" default=\"silver\"}。值里面也允许出现 \" 之类的转义。
// default 整段是可选的（没有默认值的变量），所以第二个分组可能不存在。
var argumentPattern = regexp.MustCompile(
	`\{argument\s+name=\\?"((?:[^"\\]|\\.)+?)\\?"` +
		`(?:\s+default=\\?"((?:[^"\\]|\\.)*?)\\?")?\s*\}`)

// varNameStripPattern 占位符键里只留：小写字母、数字、下划线、中日韩汉字。
// 其余（空格、标点、全角符号…）一律折成下划线。
// \x{4e00}-\x{9fff} 就是老代码里那个 一-鿿 的区间。
var varNameStripPattern = regexp.MustCompile(`[^a-z0-9_\x{4e00}-\x{9fff}]+`)

// maxVarNameRunes 占位符键的长度上限（按字符数，不是字节数）。
// 跟老代码的 key[:40] 一致。
const maxVarNameRunes = 40

// unescapeArgument 把值里的 \" 和 \\ 还原。移植自老代码的 _unescape。
func unescapeArgument(value string) string {
	return strings.NewReplacer(`\"`, `"`, `\\`, `\`).Replace(value)
}

// sanitizeVarName 把任意变量名（可能含空格、大小写、标点、中文）
// 转成占位符允许的形式。移植自老代码的 _sanitize_var_name。
//
// 跟老代码的唯一差别：老代码先做 Unicode NFKC 规范化，我们用 foldFullWidth
// 只折全角 ASCII。理由是 NFKC 要引 golang.org/x/text，而它现在是 go.mod 里的
// 间接依赖 —— 为了一个只影响全角字符的细节把它提成直接依赖，
// 每次跟上游合版都要多处理一次 go.mod 冲突，不划算。
func sanitizeVarName(label string) string {
	normalized := strings.ToLower(strings.TrimSpace(foldFullWidth(label)))
	key := strings.Trim(varNameStripPattern.ReplaceAllString(normalized, "_"), "_")
	if key == "" {
		// 变量名整个是标点（上游真的有 "???" 这种），给个兜底名，
		// 不能返回空串 —— 正文里会出现一个 {} 的空占位符。
		return "var"
	}
	if runes := []rune(key); unicode.IsDigit(runes[0]) {
		// 数字开头的占位符在很多模板引擎里不合法，加个前缀躲开。
		key = "v_" + key
	}
	return truncateRunes(key, maxVarNameRunes)
}

// foldFullWidth 把全角 ASCII（！～）折成半角，全角空格折成普通空格。
// 上游语料里中文条目常带全角标点，不折的话它们会被当成「非法字符」全变成下划线。
func foldFullWidth(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch {
		case r >= 0xFF01 && r <= 0xFF5E:
			b.WriteRune(r - 0xFEE0)
		case r == 0x3000:
			b.WriteRune(' ')
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// uniqueVarKey 撞名时加序号：main_color、main_color_2、main_color_3…
// （"Main-Color" 和 "main color" 清洗后是同一个键，但它们是两个不同的变量，
// 共用一个占位符会让运营填一个框、两处都变。）
func uniqueVarKey(base string, used map[string]bool) string {
	if !used[base] {
		return base
	}
	for i := 2; ; i++ {
		candidate := fmt.Sprintf("%s_%d", base, i)
		if !used[candidate] {
			return candidate
		}
	}
}

// ConvertPrompt 把上游的 {argument name=".." default=".."} 转成
// 本项目的 {占位符} + 变量定义。
//
// 返回的正文首尾空白会被去掉；没有变量时第二个返回值是 nil（不是空切片，
// repository 那边会把 nil 写成 JSONB 的 []）。
func ConvertPrompt(content string) (string, []dkdomain.PromptVariable) {
	matches := argumentPattern.FindAllStringSubmatchIndex(content, -1)
	if len(matches) == 0 {
		return strings.TrimSpace(content), nil
	}

	var (
		body       strings.Builder
		variables  []dkdomain.PromptVariable
		keyByLabel = make(map[string]string, len(matches))
		usedKeys   = make(map[string]bool, len(matches))
		cursor     int
	)
	body.Grow(len(content))

	for _, m := range matches {
		// m[0]/m[1] 是整个 {argument ...} 的起止；先把它前面那段原样抄过去。
		body.WriteString(content[cursor:m[0]])
		cursor = m[1]

		label := unescapeArgument(content[m[2]:m[3]])
		// 第二个分组（default）可能整段不存在，此时下标是 -1。
		def := ""
		if m[4] >= 0 && m[5] >= 0 {
			def = unescapeArgument(content[m[4]:m[5]])
		}

		key, seen := keyByLabel[label]
		if !seen {
			key = uniqueVarKey(sanitizeVarName(label), usedKeys)
			usedKeys[key] = true
			keyByLabel[label] = key
			variables = append(variables, dkdomain.PromptVariable{
				Name: key,
				// 上游的 default 当输入框提示文字。
				Example: def,
			})
		}
		body.WriteString("{" + key + "}")
	}
	body.WriteString(content[cursor:])

	return strings.TrimSpace(body.String()), variables
}

// ----------------------------------------------------------------------------
// 一条上游条目 → 一行待入库的数据
// ----------------------------------------------------------------------------

const (
	// maxPromptTitleRunes 标题列是 VARCHAR(255)。PostgreSQL 的 VARCHAR(n) 数的是
	// **字符**不是字节，所以按 rune 截。
	// 不截的后果：上游偶发的超长标题会让**整轮同步**报错回滚，1.4 万条一条都进不去。
	maxPromptTitleRunes = 255

	// maxPreviewURLRunes 预览图列是 VARCHAR(1024)。超长的宁可不要缩略图，
	// 也不能拖垮整轮同步（老代码在这里栽过，注释里写着「宁可这条没有缩略图」）。
	maxPreviewURLRunes = 1024
)

// collapseSpaces 折叠换行和连续空白。移植自老代码的 _collapse。
// 上游约 40 条 title 里含换行，不折会把列表页撑乱。
// 用 strings.Fields 而不是正则 \s+：Go 的 \s 只认 ASCII 空白，
// 而上游中文条目里有全角空格。
func collapseSpaces(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// truncateRunes 按字符数截断（不会把一个汉字截成半个）。
func truncateRunes(s string, max int) string {
	if max <= 0 {
		return ""
	}
	runes := []rune(s)
	if len(runes) <= max {
		return s
	}
	return string(runes[:max])
}

// pickPreviewURL 从上游的 sourceMedia 里挑第一张能用的预览图。
// 挑不到返回 nil（列可空）。
func pickPreviewURL(media []string) *string {
	for _, raw := range media {
		url := strings.TrimSpace(raw)
		if !strings.HasPrefix(url, "http") {
			continue
		}
		if len([]rune(url)) > maxPreviewURLRunes {
			// 这一条没有缩略图，但整轮同步不会因此失败。
			continue
		}
		return &url
	}
	return nil
}

// BuildSyncRows 把拉回来的上游条目转成待入库的行。
//
// 顺序：按上游 id 升序（调用方保证），转换后原样保留 —— 入库是逐条 upsert，
// 顺序只影响写入顺序，但稳定的顺序让两次同步的日志可比对。
//
// 正文转换后为空的条目直接丢掉（老代码同样 continue）：
// 一条没有正文的提示词点进去是空白页，不如不进库。
func BuildSyncRows(items []UpstreamPrompt, newUID func() string) []dkdomain.SyncPromptRow {
	rows := make([]dkdomain.SyncPromptRow, 0, len(items))
	for _, item := range items {
		body, variables := ConvertPrompt(item.Content)
		if body == "" {
			continue
		}
		title := truncateRunes(collapseSpaces(item.Title), maxPromptTitleRunes)
		if title == "" {
			title = fmt.Sprintf("提示词 #%d", item.ID)
		}
		rows = append(rows, dkdomain.SyncPromptRow{
			UID:          newUID(),
			CategorySlug: primaryCategorySlug(item.Slugs),
			Title:        title,
			Body:         body,
			Variables:    variables,
			PreviewURL:   pickPreviewURL(item.SourceMedia),
			SourceRef:    fmt.Sprintf("%s%d", PromptSourceRefPrefix, item.ID),
		})
	}
	return rows
}
