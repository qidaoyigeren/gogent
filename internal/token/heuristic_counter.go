package token

import (
	"unicode"
	"unicode/utf8"
)

// heuristic_counter.go 实现不依赖 tokenizer 模型的 token 近似计数器。
// 核心算法：
//   - CJK 字符（汉字/假名/韩文）按 1.5 token/字 估算（CJK 词素粒度更细）
//   - 英文连续非空白字符按 0.75 token/字符估算（大约 1.3 字符/token）
//   - 标点单独计 1 token
//
// 使用场景：
//   - knowledge_chunk_handler 在写 DB chunk 行时调用 countTokens 估算 token_count
//   - 供管理端展示 chunk 大小，辅助调度分块策略参数
//
// 注意：这是近似值，不应用于精确计费；精确 token 计数需要对应模型的 tokenizer。

// HeuristicCounter estimates token count without a tokenizer model.
// Uses the heuristic: ~0.75 tokens per English word, ~1.5 tokens per CJK character.
type HeuristicCounter struct{}

func NewHeuristicCounter() *HeuristicCounter {
	return &HeuristicCounter{}
}

// Count estimates the number of tokens in the text.
// 这是轻量估算，不依赖具体模型 tokenizer；用于 chunk 统计和管理端展示，
// 不应该作为精确计费依据。
func (c *HeuristicCounter) Count(text string) int {
	if text == "" {
		return 0
	}

	total := 0.0
	wordLen := 0

	for i := 0; i < len(text); {
		// 按 UTF-8 rune 遍历，保证中文、日文、韩文等多字节字符不会被拆坏。
		r, size := utf8.DecodeRuneInString(text[i:])
		i += size

		if isCJK(r) {
			// Flush current word
			// CJK 字符通常不像英文用空格分词，单字按更高 token 估算。
			if wordLen > 0 {
				total += 0.75 * float64(wordLen)
				wordLen = 0
			}
			total += 1.5
		} else if unicode.IsSpace(r) || unicode.IsPunct(r) {
			if wordLen > 0 {
				total += float64(wordLen) * 0.75
				wordLen = 0
			}
			if unicode.IsPunct(r) {
				total += 1
			}
		} else {
			wordLen++
		}
	}
	if wordLen > 0 {
		total += float64(wordLen) * 0.75
	}
	result := int(total)
	if result < 1 && len(text) > 0 {
		return 1
	}
	return result
}

// isCJK 判断 rune 是否属于中日韩主要文字范围。
func isCJK(r rune) bool {
	return unicode.Is(unicode.Han, r) ||
		unicode.Is(unicode.Hiragana, r) ||
		unicode.Is(unicode.Katakana, r) ||
		unicode.Is(unicode.Hangul, r)
}
