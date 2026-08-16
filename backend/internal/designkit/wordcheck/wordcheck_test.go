//go:build unit

package wordcheck

import (
	"testing"
)

// TestWordCount 词库被吞（embed 出空文件、路径改错）时功能会静默变成
// 「永远不命中」，这里钉死一个下限。
func TestWordCount(t *testing.T) {
	if WordCount() < 250 {
		t.Fatalf("词库只剩 %d 个词，data/adlaw.txt 多半没被 embed 进来", WordCount())
	}
}

// TestCheckHits 基本命中：词、rune 下标都要对。
func TestCheckHits(t *testing.T) {
	// 只含一个违禁词的句子，位置手算（「销量领先」不在词表里，裸「领先」刻意不收）。
	text := "本店销量领先，顶级品质"
	// 「顶级」在 rune 下标 7、8（本0店1销2量3领4先5，6顶7级8）。
	hits := Check(text)
	if len(hits) != 1 {
		t.Fatalf("期望 1 个命中，得到 %d 个：%+v", len(hits), hits)
	}
	if hits[0].Word != "顶级" || hits[0].Start != 7 || hits[0].End != 9 {
		t.Fatalf("命中不对：%+v，期望 顶级 [7,9)", hits[0])
	}
}

// TestCheckMultipleAndSorted 多个命中按出现位置排序；同一个词出现两次报两次。
func TestCheckMultipleAndSorted(t *testing.T) {
	text := "第一名就是第一"
	hits := Check(text)
	if len(hits) != 2 {
		t.Fatalf("期望 2 个命中，得到 %d 个：%+v", len(hits), hits)
	}
	if hits[0].Word != "第一" || hits[0].Start != 0 || hits[0].End != 2 {
		t.Fatalf("第 1 个命中不对：%+v", hits[0])
	}
	if hits[1].Word != "第一" || hits[1].Start != 5 || hits[1].End != 7 {
		t.Fatalf("第 2 个命中不对：%+v", hits[1])
	}
}

// TestCheckNoHits 干净文案不命中；空串不命中且不 panic。
func TestCheckNoHits(t *testing.T) {
	for _, text := range []string{
		"",
		"纯棉白色圆领短袖T恤 夏季薄款",
		"不锈钢保温杯 500ml 便携车载",
	} {
		if hits := Check(text); len(hits) != 0 {
			t.Fatalf("%q 不该命中，得到：%+v", text, hits)
		}
	}
}

// TestCheckOverlap 重叠词的去重规则：
// 被完全包住的丢掉，部分重叠但互不包含的都保留。
// 「全网最低价」：词表里「最低」被「全网最低」包住 → 丢；
// 「最低价」[2,5) 与「全网最低」[0,4) 部分重叠 → 两个都留。
func TestCheckOverlap(t *testing.T) {
	text := "全网最低价"
	hits := Check(text)
	if len(hits) != 2 {
		t.Fatalf("期望 2 个命中（全网最低 + 最低价），得到 %d 个：%+v", len(hits), hits)
	}
	if hits[0].Word != "全网最低" || hits[0].Start != 0 || hits[0].End != 4 {
		t.Fatalf("第 1 个命中不对：%+v，期望 全网最低 [0,4)", hits[0])
	}
	if hits[1].Word != "最低价" || hits[1].Start != 2 || hits[1].End != 5 {
		t.Fatalf("第 2 个命中不对：%+v，期望 最低价 [2,5)", hits[1])
	}
	// 「最低」不许单独再报一次。
	for _, h := range hits {
		if h.Word == "最低" {
			t.Fatalf("被包住的「最低」不该单独出现：%+v", hits)
		}
	}
}

// TestCheckASCIICaseInsensitive 英文词大小写不敏感，报的是词表里的小写形式。
func TestCheckASCIICaseInsensitive(t *testing.T) {
	text := "全店NO.1好物"
	hits := Check(text)
	if len(hits) != 1 {
		t.Fatalf("期望 1 个命中，得到 %d 个：%+v", len(hits), hits)
	}
	// 全0店1N2O3.4 1|5 好6物7 → no.1 在 [2,6)。
	if hits[0].Word != "no.1" || hits[0].Start != 2 || hits[0].End != 6 {
		t.Fatalf("命中不对：%+v，期望 no.1 [2,6)", hits[0])
	}
}

// TestTitleLen 标题字数按 rune 计：汉字、字母、数字、emoji 都是 1 个字；
// 首尾空白不算。
func TestTitleLen(t *testing.T) {
	// 期望值手算：
	//   款式2026New   → 2 汉字 + 4 数字 + 3 字母 = 9
	//   夏季🔥新款     → emoji 算 1 个字（rune），不是 2 个 UTF-16 单元 = 5
	//   两边有空格     → 首尾空白不算 = 5
	//   连衣裙 夏 新款 → 中间的空格算字数（平台也这么算）= 8
	cases := []struct {
		text string
		want int
	}{
		{"", 0},
		{"   ", 0},
		{"夏季新款", 4},
		{"款式2026New", 9},
		{"夏季🔥新款", 5},
		{"  两边有空格  ", 5},
		{"连衣裙 夏 新款", 8},
	}
	for _, c := range cases {
		if got := TitleLen(c.text); got != c.want {
			t.Fatalf("TitleLen(%q) = %d，期望 %d", c.text, got, c.want)
		}
	}
}

// TestPlatformByKey 平台表：拿得到、大小写不敏感、拿不到时 ok=false。
func TestPlatformByKey(t *testing.T) {
	p, ok := PlatformByKey("taobao")
	if !ok || p.Name != "淘宝" || p.TitleMaxRunes != 30 {
		t.Fatalf("taobao 规则不对：%+v ok=%v", p, ok)
	}
	if p, ok = PlatformByKey(" JD "); !ok || p.Key != "jd" || p.TitleMaxRunes != 45 {
		t.Fatalf("大小写/空白归一失败：%+v ok=%v", p, ok)
	}
	if _, ok = PlatformByKey("ebay"); ok {
		t.Fatal("不存在的平台不该返回 ok=true")
	}
	if len(Platforms()) != len(PlatformKeys()) || len(Platforms()) == 0 {
		t.Fatal("Platforms 与 PlatformKeys 长度不一致或为空")
	}
}
