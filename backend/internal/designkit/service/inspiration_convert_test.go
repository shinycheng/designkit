//go:build unit

package service

// 灵感库转换规则的单元测试。
//
// 这一份是 legacy-python 分支 tests/test_inspiration_convert.py 里
// ConvertPromptTests 的**逐条翻译**，一条都没少，另外补了几条老代码里
// 只有注释没有用例的边界（转义引号、超长标题、多分类归属）。
//
// 为什么要逐条翻：那些边界是在 1.4 万条真实语料上试出来的，
// 重写一遍必然漏。测试也不联网、不花钱（CLAUDE.md 第三节）。

import (
	"fmt"
	"strings"
	"testing"

	dkdomain "github.com/Wei-Shaw/sub2api/internal/designkit/domain"
)

// ----------------------------------------------------------------------------
// ConvertPrompt
// ----------------------------------------------------------------------------

func TestConvertPrompt(t *testing.T) {
	cases := []struct {
		name     string
		content  string
		wantBody string
		wantVars []dkdomain.PromptVariable
	}{
		{
			// 老用例 test_basic_argument
			name:     "最常见的一条：变量名带空格，有默认值",
			content:  `A {argument name="hair color" default="silver"} cat`,
			wantBody: "A {hair_color} cat",
			wantVars: []dkdomain.PromptVariable{{Name: "hair_color", Example: "silver"}},
		},
		{
			// 老用例 test_same_label_reused
			name:     "同一个变量出现两次，共用一个占位符，只产生一个变量定义",
			content:  `{argument name="tone" default="warm"} and {argument name="tone"}`,
			wantBody: "{tone} and {tone}",
			wantVars: []dkdomain.PromptVariable{{Name: "tone", Example: "warm"}},
		},
		{
			// 老用例 test_sanitized_collision_gets_suffix
			name:     "两个不同的变量清洗后撞名，第二个自动加序号",
			content:  `{argument name="Main-Color" default="a"} vs {argument name="main color" default="b"}`,
			wantBody: "{main_color} vs {main_color_2}",
			wantVars: []dkdomain.PromptVariable{
				{Name: "main_color", Example: "a"},
				{Name: "main_color_2", Example: "b"},
			},
		},
		{
			// 老用例 test_chinese_label_kept
			name:     "中文变量名原样保留",
			content:  `{argument name="背景色" default="米白"}`,
			wantBody: "{背景色}",
			wantVars: []dkdomain.PromptVariable{{Name: "背景色", Example: "米白"}},
		},
		{
			// 老用例 test_no_arguments_passthrough
			name:     "普通花括号不是变量，原样留着",
			content:  "Plain prompt with {curly} braces",
			wantBody: "Plain prompt with {curly} braces",
			wantVars: nil,
		},
		{
			// 老代码注释里写着「上游约 8% 的条目整体被转义过」，但老测试没有用例。
			// 认不出来的后果：上千条提示词的正文里留着一句 {argument name=...} 的乱码。
			name:     "转义引号形态 name=\\\"..\\\" 也要认出来",
			content:  `一只 {argument name=\"发色\" default=\"silver\"} 的猫`,
			wantBody: "一只 {发色} 的猫",
			wantVars: []dkdomain.PromptVariable{{Name: "发色", Example: "silver"}},
		},
		{
			name:     "值里面本身带转义引号",
			content:  `{argument name="say \"hi\"" default="x"}`,
			wantBody: "{say_hi}",
			wantVars: []dkdomain.PromptVariable{{Name: "say_hi", Example: "x"}},
		},
		{
			name:     "没有 default 的变量，示例值为空",
			content:  `{argument name="mood"} light`,
			wantBody: "{mood} light",
			wantVars: []dkdomain.PromptVariable{{Name: "mood", Example: ""}},
		},
		{
			name:     "default 是空串",
			content:  `{argument name="mood" default=""}`,
			wantBody: "{mood}",
			wantVars: []dkdomain.PromptVariable{{Name: "mood", Example: ""}},
		},
		{
			name:     "首尾空白会被去掉",
			content:  "  \n A {argument name=\"x\"} B \n ",
			wantBody: "A {x} B",
			wantVars: []dkdomain.PromptVariable{{Name: "x", Example: ""}},
		},
		{
			name:     "整条为空",
			content:  "",
			wantBody: "",
			wantVars: nil,
		},
		{
			name:     "变量名全是标点，退化成 var",
			content:  `{argument name="???" default="d"}`,
			wantBody: "{var}",
			wantVars: []dkdomain.PromptVariable{{Name: "var", Example: "d"}},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body, vars := ConvertPrompt(tc.content)
			if body != tc.wantBody {
				t.Fatalf("正文不对\n得到: %q\n期望: %q", body, tc.wantBody)
			}
			if len(vars) != len(tc.wantVars) {
				t.Fatalf("变量个数不对\n得到: %+v\n期望: %+v", vars, tc.wantVars)
			}
			for i := range vars {
				if vars[i] != tc.wantVars[i] {
					t.Fatalf("第 %d 个变量不对\n得到: %+v\n期望: %+v", i, vars[i], tc.wantVars[i])
				}
			}
		})
	}
}

func TestConvertPromptPlaceholdersMatchVariables(t *testing.T) {
	// 正文里的每个 {占位符} 都必须在变量定义里找得到，否则生成页会渲染出
	// 一个填不了的框（或者一个填了没用的框）。
	body, vars := ConvertPrompt(
		`{argument name="A B" default="1"} {argument name="a-b" default="2"} {argument name="A B"}`)
	for _, v := range vars {
		if !strings.Contains(body, "{"+v.Name+"}") {
			t.Fatalf("变量 %q 在正文里没有对应的占位符：%s", v.Name, body)
		}
	}
	if len(vars) != 2 {
		t.Fatalf("同名的应该合并、撞名的应该分开，得到 %d 个：%+v", len(vars), vars)
	}
}

// ----------------------------------------------------------------------------
// sanitizeVarName（老用例 test_sanitize_edge_cases）
// ----------------------------------------------------------------------------

func TestSanitizeVarName(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"  Hair Color!! ", "hair_color"}, // 老用例
		{"123abc", "v_123abc"},            // 老用例：数字开头要加前缀
		{"???", "var"},                    // 老用例：全是标点退化成 var
		{"背景色", "背景色"},
		{"Main-Color", "main_color"},
		{"main color", "main_color"},
		{"", "var"},
		{"   ", "var"},
		{"ＦＵＬＬ　ＷＩＤＴＨ", "full_width"}, // 全角要折成半角，不能整串变下划线
		{strings.Repeat("a", 60), strings.Repeat("a", maxVarNameRunes)},
		{strings.Repeat("中", 60), strings.Repeat("中", maxVarNameRunes)}, // 按字符截，不能把汉字截半个
	}
	for _, tc := range cases {
		if got := sanitizeVarName(tc.in); got != tc.want {
			t.Errorf("sanitizeVarName(%q) = %q，期望 %q", tc.in, got, tc.want)
		}
	}
}

func TestUnescapeArgument(t *testing.T) {
	if got := unescapeArgument(`say \"hi\"`); got != `say "hi"` {
		t.Fatalf("转义没还原：%q", got)
	}
	if got := unescapeArgument(`a\\b`); got != `a\b` {
		t.Fatalf("反斜杠没还原：%q", got)
	}
}

func TestCollapseSpaces(t *testing.T) {
	// 上游约 40 条 title 里带换行，不折的话列表页会被撑乱。
	if got := collapseSpaces(" 一行\n  两行\t三行 "); got != "一行 两行 三行" {
		t.Fatalf("空白没折叠：%q", got)
	}
	// 全角空格也要折（Go 的正则 \s 不认它，所以这里用的是 strings.Fields）。
	if got := collapseSpaces("中文　空格"); got != "中文 空格" {
		t.Fatalf("全角空格没折叠：%q", got)
	}
}

func TestTruncateRunes(t *testing.T) {
	if got := truncateRunes("中文中文", 2); got != "中文" {
		t.Fatalf("按字符截断出错：%q", got)
	}
	if got := truncateRunes("abc", 10); got != "abc" {
		t.Fatalf("没超长不该动：%q", got)
	}
	if got := truncateRunes("abc", 0); got != "" {
		t.Fatalf("上限为 0 时应返回空串：%q", got)
	}
}

// ----------------------------------------------------------------------------
// 分类归属
// ----------------------------------------------------------------------------

func TestPrimaryCategoryOrderCoversEveryCategory(t *testing.T) {
	// primaryCategoryOrder 必须是 inspirationCategories 的一个全排列。
	// 漏一个的后果：那个分类下的提示词全被归到「其他」，运营点开是空的。
	if len(primaryCategoryOrder) != len(inspirationCategories) {
		t.Fatalf("优先级清单 %d 条，分类 %d 个，对不上",
			len(primaryCategoryOrder), len(inspirationCategories))
	}
	inOrder := make(map[string]bool, len(primaryCategoryOrder))
	for _, slug := range primaryCategoryOrder {
		if inOrder[slug] {
			t.Fatalf("优先级清单里 %q 出现了两次", slug)
		}
		inOrder[slug] = true
	}
	for _, c := range inspirationCategories {
		if !inOrder[c.Slug] {
			t.Fatalf("分类 %q（%s）不在优先级清单里", c.Slug, c.NameZH)
		}
	}
}

func TestPrimaryCategoryPrefersEcommerce(t *testing.T) {
	// 老系统按「上游文件顺序取第一个」，结果「电商主图」416 条里有 398 条
	// 被排在前面的分类抢走，运营点开只剩 18 条。我们按贴合度取。
	cases := []struct {
		slugs []string
		want  string
	}{
		{[]string{"social-media-post", "ecommerce-main-image"}, "ecommerce-main-image"},
		{[]string{"infographic-edu-visual", "product-marketing"}, "product-marketing"},
		{[]string{"comic-storyboard"}, "comic-storyboard"},
		{[]string{"上游新加的分类"}, "others"}, // 不认识的一律归「其他」，不能整条丢掉
		{nil, "others"},
	}
	for _, tc := range cases {
		if got := primaryCategorySlug(tc.slugs); got != tc.want {
			t.Errorf("primaryCategorySlug(%v) = %q，期望 %q", tc.slugs, got, tc.want)
		}
	}
}

func TestCategorySlugsAreUnique(t *testing.T) {
	// slug 是 designkit_prompt_categories 的唯一键，重复会让两个分类互相覆盖。
	seen := make(map[string]bool, len(inspirationCategories))
	for _, c := range inspirationCategories {
		if seen[c.Slug] {
			t.Fatalf("分类标识 %q 重复了", c.Slug)
		}
		seen[c.Slug] = true
		if strings.TrimSpace(c.NameZH) == "" || strings.TrimSpace(c.NameEN) == "" {
			t.Fatalf("分类 %q 缺中文名或英文名（CLAUDE.md：每条文案都要有英文）", c.Slug)
		}
	}
}

// ----------------------------------------------------------------------------
// BuildSyncRows
// ----------------------------------------------------------------------------

// fixedUID 让测试里的 uid 可预期。
func fixedUID() func() string {
	n := 0
	return func() string {
		n++
		return fmt.Sprintf("%026d", n)
	}
}

func TestBuildSyncRows(t *testing.T) {
	longURL := "https://example.com/" + strings.Repeat("x", maxPreviewURLRunes)
	items := []UpstreamPrompt{
		{
			ID:          101,
			Title:       "  一张\n干净的白底图  ",
			Content:     `拍一张 {argument name="品类" default="连衣裙"} 的白底图`,
			SourceMedia: []string{"not-a-url", "https://img.example.com/a.png"},
			Slugs:       []string{"social-media-post", "ecommerce-main-image"},
		},
		{
			ID:      102,
			Title:   "",
			Content: "纯文字提示词",
			Slugs:   []string{"poster-flyer"},
		},
		{
			// 正文为空 → 整条丢掉（点进去是空白页，不如不进库）
			ID:      103,
			Title:   "空正文",
			Content: "   ",
			Slugs:   []string{"poster-flyer"},
		},
		{
			// 超长预览图地址 → 这条没有缩略图，但不能拖垮整轮同步
			ID:          104,
			Title:       "超长图片地址",
			Content:     "正文",
			SourceMedia: []string{longURL},
			Slugs:       []string{"game-asset"},
		},
	}

	rows := BuildSyncRows(items, fixedUID())
	if len(rows) != 3 {
		t.Fatalf("空正文那条应该被丢掉，得到 %d 行", len(rows))
	}

	first := rows[0]
	if first.SourceRef != "youmind:101" {
		t.Fatalf("幂等键不对：%q（改它等于下次同步把 1.4 万条重插一遍）", first.SourceRef)
	}
	if first.CategorySlug != "ecommerce-main-image" {
		t.Fatalf("多分类时该归电商主图，得到 %q", first.CategorySlug)
	}
	if first.Title != "一张 干净的白底图" {
		t.Fatalf("标题没折叠换行：%q", first.Title)
	}
	if first.Body != "拍一张 {品类} 的白底图" {
		t.Fatalf("正文没转换：%q", first.Body)
	}
	if len(first.Variables) != 1 || first.Variables[0].Name != "品类" ||
		first.Variables[0].Example != "连衣裙" {
		t.Fatalf("变量不对：%+v", first.Variables)
	}
	if first.PreviewURL == nil || *first.PreviewURL != "https://img.example.com/a.png" {
		t.Fatalf("预览图应该挑第一个 http 开头的：%v", first.PreviewURL)
	}
	if first.UID == "" {
		t.Fatal("新增用的 uid 不能为空")
	}

	if rows[1].Title != "提示词 #102" {
		t.Fatalf("没有标题时要给个兜底名：%q", rows[1].Title)
	}
	if rows[2].PreviewURL != nil {
		t.Fatalf("超长地址应该丢掉缩略图而不是写进库：%v", rows[2].PreviewURL)
	}
}

func TestBuildSyncRowsTitleFitsColumn(t *testing.T) {
	// title 列是 VARCHAR(255)，数的是字符不是字节。
	// 不截的后果：上游偶发的超长标题让**整轮**同步报错回滚，一条都进不去。
	rows := BuildSyncRows([]UpstreamPrompt{{
		ID:      1,
		Title:   strings.Repeat("中", 400),
		Content: "正文",
		Slugs:   []string{"others"},
	}}, fixedUID())
	if got := len([]rune(rows[0].Title)); got != maxPromptTitleRunes {
		t.Fatalf("标题应该截到 %d 个字符，得到 %d", maxPromptTitleRunes, got)
	}
}

func TestPromptSourceURL(t *testing.T) {
	// 上游是 CC BY 4.0，界面要能跳回原文。
	if got := PromptSourceURL("youmind:42"); got != "https://youmind.com/gpt-image-2-prompts?id=42" {
		t.Fatalf("原文链接不对：%q", got)
	}
	if got := PromptSourceURL(""); got != "" {
		t.Fatalf("运营自己存的提示词不该有原文链接：%q", got)
	}
	if got := PromptSourceURL("user:7"); got != "" {
		t.Fatalf("不是上游来的不该有原文链接：%q", got)
	}
}
