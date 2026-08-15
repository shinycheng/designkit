//go:build unit

package worker

// ratio_instruction_test.go —— 「靠提示词控比例」那一条的单测。
//
// 为什么这一条值得单独钉：2026-08-14 实测发现，输入一张 554×554 的正方图、
// 请求 1:1，网关回来的是 1087×1447（3:4）。原来指望的「补白边控制比例」
// 单靠自己不管用，而 size 参数发的是补边后的真实像素、不是接口认的标准尺寸，
// 等于没发。提示词是第三条路，也是唯一不依赖「OpenAI 支不支持 3:4 / 16:9
// 这类非标准比例」的一条。
//
// 最要紧的一条断言是**不改快照**：追加的那句话只能进发给模型的那一份，
// 落库的 prompt_text 必须还是运营写的原文。混同的话，历史记录里每条词后面
// 都挂着一句系统加的话，运营会以为是自己写的，重试时又会被追加第二遍。

import (
	"strings"
	"testing"

	dkdomain "github.com/Wei-Shaw/sub2api/internal/designkit/domain"
)

// 五个配置里允许的比例都必须有对应的说法，一个都不能漏。
// 漏掉的那个会静默退化成「不控制比例」——出图不报错，只是比例不对。
func TestWithRatioInstruction_CoversEveryAllowedRatio(t *testing.T) {
	// 跟 9001 迁移里 ratios 的默认值一致。
	for _, ratio := range []dkdomain.Ratio{"1:1", "3:4", "4:3", "16:9", "9:16"} {
		out := withRatioInstruction("纯白背景，产品居中", ratio)
		if !strings.Contains(out, string(ratio)) {
			t.Fatalf("比例 %s 的提示词里没有出现这个比例：%q", ratio, out)
		}
		if out == "纯白背景，产品居中" {
			t.Fatalf("比例 %s 没有追加任何要求，等于不控制比例", ratio)
		}
	}
}

// 原文必须原样保留在前面：追加的是一句要求，不是替换。
func TestWithRatioInstruction_KeepsOriginalPrompt(t *testing.T) {
	const original = "米白色针织开衫，柔和顶光"
	out := withRatioInstruction(original, "3:4")
	if !strings.HasPrefix(out, original) {
		t.Fatalf("原文没有被原样保留在开头：%q", out)
	}
}

// 不认识的比例原样返回。宁可这一张比例不受控，
// 也不要拼一句半通不通的话进去（那会直接影响出图内容）。
func TestWithRatioInstruction_UnknownRatioPassesThrough(t *testing.T) {
	const original = "纯白背景"
	if out := withRatioInstruction(original, "7:13"); out != original {
		t.Fatalf("不认识的比例应该原样返回，实际 %q", out)
	}
}

// 提示词是空的时候只返回那句要求，不要留一个开头的空行。
func TestWithRatioInstruction_EmptyPrompt(t *testing.T) {
	out := withRatioInstruction("   ", "1:1")
	if strings.HasPrefix(out, "\n") || strings.TrimSpace(out) == "" {
		t.Fatalf("空提示词的处理不对：%q", out)
	}
}

// ⚠ 这个函数**不能**改到落库的那一份：它是纯函数，只在调用点用于构造发给网关的请求。
//
// 2026-08-14 起它还多了一个更强的性质：**幂等**。
// 因为追加之前会先清掉原有的比例说法，而上一次追加的那句本身就是比例说法，
// 所以重复施加不会累积。
//
// 这不是可有可无的：万一哪天有人把追加后的内容误存回库再重试，
// 原来会越滚越长（十次重试就挂十句比例要求），现在最多是「清掉再加一遍」。
// **落库仍然必须存原文**，这条只是把误用的后果从「越来越长」降成「无害」。
func TestWithRatioInstruction_IsPureAndIdempotent(t *testing.T) {
	const original = "纯白背景，柔和顶光"
	first := withRatioInstruction(original, "16:9")
	if first != withRatioInstruction(original, "16:9") {
		t.Fatal("同样的输入应该得到同样的输出")
	}

	// 拿输出再施加一次 —— 结果必须跟第一次完全一样，不能变长。
	twice := withRatioInstruction(first, "16:9")
	if twice != first {
		t.Fatalf("重复施加不该累积：\n第一次: %q\n第二次: %q", first, twice)
	}
	if strings.Count(twice, "16:9") != 1 {
		t.Fatalf("比例要求出现了 %d 次，应该只有 1 次：%q", strings.Count(twice, "16:9"), twice)
	}

	// 换个比例再施加：旧的必须没了，新的必须在 —— 这正是运营「改了比例重新提交」的路径。
	switched := withRatioInstruction(first, "3:4")
	if strings.Contains(switched, "16:9") {
		t.Fatalf("换比例之后旧的 16:9 还在：%q", switched)
	}
	if !strings.Contains(switched, "3:4") {
		t.Fatalf("换比例之后没加上 3:4：%q", switched)
	}
}

// ---------------------------------------------------------------------------
// 清掉提示词里原有的比例说法（2026-08-14 monica 发现的冲突）
// ---------------------------------------------------------------------------

// 用她贴的那条真实提示词验：结尾有「方形1:1构图」，而她第三步选的是 16:9。
//
// 不清的话最终提示词里会同时出现「方形1:1构图」和「宽高比严格 16:9」，
// 谁赢纯粹碰运气。
func TestWithRatioInstruction_RemovesConflictingRatioFromRealPrompt(t *testing.T) {
	const real = "生成一张高质感电商主图：画面中央仅展示一只正面坐姿的米白色兔子毛绒玩偶，" +
		"严格还原商品外形与比例，包括两只竖直修长的圆角耳朵、圆润大头、黑色椭圆眼睛、" +
		"黑色“×”形小嘴、短小圆润的双臂、饱满躯干和向前伸出的两只圆脚；" +
		"整体风格简洁、温暖、可爱、写实，色彩准确，细节精致，商业产品摄影质感，高分辨率，" +
		"方形1:1构图，无文字、无水印、无边框、无多余元素。"

	out := withRatioInstruction(real, "16:9")

	// 冲突的那一句必须没了。
	if strings.Contains(out, "方形1:1构图") {
		t.Fatalf("原有的「方形1:1构图」没被清掉，会跟 16:9 打架：\n%s", out)
	}
	if strings.Contains(out, "1:1") {
		t.Fatalf("还残留 1:1：\n%s", out)
	}
	// 我们要的那句必须在。
	if !strings.Contains(out, "16:9") {
		t.Fatalf("没追加 16:9 的要求：\n%s", out)
	}
	// ⚠ 商品描述必须**一个字都不能少**。清比例误伤商品描述比比例不对更糟：
	// 比例不对是「图形状不对」，商品描述被删是「画出来根本不是这个东西」。
	for _, must := range []string{
		"米白色兔子毛绒玩偶", "两只竖直修长的圆角耳朵", "黑色椭圆眼睛",
		"向前伸出的两只圆脚", "商业产品摄影质感", "无文字、无水印",
	} {
		if !strings.Contains(out, must) {
			t.Fatalf("商品描述被误删了：%q 不见了\n%s", must, out)
		}
	}
	// 「严格还原商品外形与比例」这句里有「比例」但说的是**商品**的比例，不能清掉。
	if !strings.Contains(out, "严格还原商品外形与比例") {
		t.Fatalf("「严格还原商品外形与比例」被误删 —— 那说的是商品比例不是画面比例：\n%s", out)
	}
}

// **不能误伤商品本身的形状描述。**
//
// 「正方形包装盒」是在说商品长什么样，清掉就把商品说错了。
// 判据是「形状词 + 在谈画面的词」同时出现才算比例说法。
func TestStripRatioClauses_KeepsProductShapeWords(t *testing.T) {
	cases := []struct {
		in   string
		must string
	}{
		{"米白色正方形包装盒，纯白背景，柔和顶光", "正方形包装盒"},
		{"方形托盘上摆着三块饼干，自然光", "方形托盘"},
		{"横版长条形的木质餐盘，俯拍", "横版长条形的木质餐盘"},
	}
	for _, c := range cases {
		out := stripRatioClauses(c.in)
		if !strings.Contains(out, c.must) {
			t.Fatalf("商品形状被误删：%q 不见了\n输入: %s\n输出: %s", c.must, c.in, out)
		}
	}
}

// 各种比例写法都要清掉。
func TestStripRatioClauses_RemovesRatioPhrasings(t *testing.T) {
	cases := []string{
		"纯白背景，方形1:1构图，柔和顶光",
		"纯白背景，正方形构图，柔和顶光",
		"纯白背景，画面比例 16 : 9，柔和顶光",
		"纯白背景，3：4 竖版画幅，柔和顶光", // 全角冒号
		"clean background, square composition, soft light",
		"纯白背景，竖版构图，柔和顶光",
	}
	for _, in := range cases {
		out := stripRatioClauses(in)
		if ratioTokenPattern.MatchString(out) {
			t.Fatalf("还残留比例记号\n输入: %s\n输出: %s", in, out)
		}
		if strings.Contains(out, "构图") || strings.Contains(out, "画幅") ||
			strings.Contains(strings.ToLower(out), "composition") {
			t.Fatalf("讲构图的那一片没清掉\n输入: %s\n输出: %s", in, out)
		}
		// 其余内容要留着。
		if !strings.Contains(out, "纯白背景") && !strings.Contains(out, "clean background") {
			t.Fatalf("清过头了\n输入: %s\n输出: %s", in, out)
		}
	}
}

// 整条都是比例说法时不能返回一个只剩标点的字符串。
func TestStripRatioClauses_AllRatioBecomesEmpty(t *testing.T) {
	if out := stripRatioClauses("方形1:1构图。"); strings.TrimRight(out, "。， ") != "" {
		t.Fatalf("应该清成空，实际 %q", out)
	}
	// 这时候 withRatioInstruction 只返回那句要求，不能是「。\n\n要求」这种。
	out := withRatioInstruction("方形1:1构图。", "16:9")
	if strings.HasPrefix(out, "。") || strings.HasPrefix(out, "\n") {
		t.Fatalf("清空之后开头留了标点：%q", out)
	}
}
