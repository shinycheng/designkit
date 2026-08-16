// Package wordcheck 文案检查：广告法违禁词命中 + 平台标题字数。
//
// 纯计算、无状态：词库在编译期 go:embed 进二进制，不查库、不发网络请求。
// 词库本体在 data/adlaw.txt（一行一词，# 注释），收词原则见该文件头。
//
// 实现刻意用「逐词 strings.Index 扫描」而不是 Aho-Corasick：
// 词量只有三百上下、待检文本是标题和短文案（几千字封顶），
// 逐词扫描在这个量级是微秒到毫秒级，引一个自动机库或自写一个都属过度设计。
// 哪天词库涨到上万词再换算法，接口不用动。
package wordcheck

import (
	_ "embed"
	"sort"
	"strings"
)

//go:embed data/adlaw.txt
var rawAdlawWords string

// Hit 一次违禁词命中。
//
// Start / End 是**按字（rune）数**的下标：第 1 个字是 0，End 不含。
// 选 rune 而不是字节，是因为调用方（前端 JS、ERP）数的都是「第几个字」，
// 字节下标在 UTF-8 中文里对它们毫无意义。
// ⚠ JS 侧要用 Array.from(text) 按码点切开再对下标，直接 text[i] 是 UTF-16 单元，
// 遇到 emoji 会错位。
type Hit struct {
	// Word 词表里的词（英文词是小写形式，如 no.1）。
	Word string
	// Start 命中的起点（rune 下标，从 0 起）。
	Start int
	// End 命中的终点（rune 下标，不含）。
	End int
}

// dictWords 词库。包加载时解析一次，之后只读。
var dictWords = loadWords(rawAdlawWords)

// loadWords 解析词库文本：去注释、去空行、去重；英文统一小写。
func loadWords(raw string) []string {
	seen := make(map[string]struct{}, 512)
	words := make([]string, 0, 512)
	for _, line := range strings.Split(raw, "\n") {
		w := strings.TrimSpace(line)
		if w == "" || strings.HasPrefix(w, "#") {
			continue
		}
		w = asciiLower(w)
		if _, ok := seen[w]; ok {
			continue
		}
		seen[w] = struct{}{}
		words = append(words, w)
	}
	return words
}

// WordCount 词库大小。给巡检 / 测试断言用：词库文件被吞（embed 出空文件）时
// 功能会静默变成「永远不命中」，必须有一个能断言的数。
func WordCount() int {
	return len(dictWords)
}

// Check 扫描 text，返回所有违禁词命中，按出现位置排序。
//
// 去重规则：**完全被另一个命中包住的丢掉**（词库同时收了「最低」和「全网最低」时，
// 「全网最低价」只报「全网最低」和「最低价」，不再单独报一次「最低」）。
// 部分重叠但互不包含的都保留，前端高亮时自行合并区间。
//
// 英文按 ASCII 大小写不敏感匹配（NO.1 / no.1 都命中「no.1」）。
// text 应是合法 UTF-8（handler 层已校验）；不是的话该段命中会被安全地丢弃。
func Check(text string) []Hit {
	if text == "" || len(dictWords) == 0 {
		return nil
	}
	lowered := asciiLower(text)

	// 先按字节下标收集所有命中。
	type span struct {
		word       string
		start, end int // 字节下标
	}
	var spans []span
	for _, w := range dictWords {
		from := 0
		for {
			idx := strings.Index(lowered[from:], w)
			if idx < 0 {
				break
			}
			start := from + idx
			spans = append(spans, span{word: w, start: start, end: start + len(w)})
			from = start + 1
		}
	}
	if len(spans) == 0 {
		return nil
	}

	// 起点升序；起点相同时长的在前，这样包含去重一趟就能做完。
	sort.Slice(spans, func(i, j int) bool {
		if spans[i].start != spans[j].start {
			return spans[i].start < spans[j].start
		}
		if spans[i].end != spans[j].end {
			return spans[i].end > spans[j].end
		}
		return spans[i].word < spans[j].word
	})

	// 丢掉被包住的（含完全相同的重复）。
	kept := make([]span, 0, len(spans))
	for _, s := range spans {
		contained := false
		for _, k := range kept {
			if k.start <= s.start && s.end <= k.end {
				contained = true
				break
			}
		}
		if !contained {
			kept = append(kept, s)
		}
	}

	// 字节下标 → rune 下标。只为用到的边界建映射，一趟扫完。
	needed := make(map[int]int, len(kept)*2)
	for _, s := range kept {
		needed[s.start] = -1
		needed[s.end] = -1
	}
	runeIdx := 0
	for byteIdx := range text {
		if _, ok := needed[byteIdx]; ok {
			needed[byteIdx] = runeIdx
		}
		runeIdx++
	}
	if _, ok := needed[len(text)]; ok {
		needed[len(text)] = runeIdx
	}

	hits := make([]Hit, 0, len(kept))
	for _, s := range kept {
		start, end := needed[s.start], needed[s.end]
		if start < 0 || end < 0 {
			// 边界没落在字符边界上：只有 text 不是合法 UTF-8 才会发生，丢弃保平安。
			continue
		}
		hits = append(hits, Hit{Word: s.word, Start: start, End: end})
	}
	return hits
}

// asciiLower 只把 A-Z 变小写，其余字节原样保留。
//
// 不用 strings.ToLower：Unicode 大小写折叠可能改变字节长度（如 İ），
// 那样匹配出的字节下标就对不回原文了。ASCII 逐字节替换长度恒等，下标恒有效。
func asciiLower(s string) string {
	hasUpper := false
	for i := 0; i < len(s); i++ {
		if s[i] >= 'A' && s[i] <= 'Z' {
			hasUpper = true
			break
		}
	}
	if !hasUpper {
		return s
	}
	b := []byte(s)
	for i := 0; i < len(b); i++ {
		if b[i] >= 'A' && b[i] <= 'Z' {
			b[i] += 'a' - 'A'
		}
	}
	return string(b)
}
