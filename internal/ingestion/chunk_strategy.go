package ingestion

import (
	"regexp"
	"strings"
	"unicode"
)

//chunk_strategy.go 实现两种核心分块策略。
//
// 1. fixedSizeChunkingStrategy（fixed_size）
//    - 以字符为窗口，ChunkSize 控制块大小，OverlapSize 控制重叠
//    - adjustFixedBoundary 在目标切点附近向前查找换行/句末，减少语义断裂
//    - normalizeChunkText 做分块前文本规整：合并中文硬换行、还原被断开的 URL
//
// 2. structureAwareChunkingStrategy（structure_aware）
//    - segmentStructureAwareBlocks 识别 Markdown 标题、围栏代码块、图片链接和段落
//    - packStructureAwareBlocks 把结构块打包成接近 targetChars 的 chunk 范围
//    - materializeStructureAware 按范围切出文本，并可附加 overlapChars 重叠
//
// 两种策略通过 ChunkingStrategy 接口接入 chunker，可随时扩展新策略注册到 registry。

type ChunkingStrategy interface {
	Mode() ChunkingMode
	Split(text string, settings ChunkerSettings) []string
}

// newDefaultChunkStrategyRegistry 注册当前支持的分块策略。
// fixed_size 适合通用文本；structure_aware 尽量按标题、段落、代码块等结构切分。
func newDefaultChunkStrategyRegistry() map[ChunkingMode]ChunkingStrategy {
	return map[ChunkingMode]ChunkingStrategy{
		ChunkingModeFixedSize:      fixedSizeChunkingStrategy{},
		ChunkingModeStructureAware: structureAwareChunkingStrategy{},
	}
}

type fixedSizeChunkingStrategy struct{}

func (fixedSizeChunkingStrategy) Mode() ChunkingMode { return ChunkingModeFixedSize }

// Split 按固定字符窗口切分文本，并通过 overlap 保留上下文重叠。
// 它会尽量把边界向前调整到换行或句末，减少把句子从中间切开的概率。
func (fixedSizeChunkingStrategy) Split(text string, settings ChunkerSettings) []string {
	if strings.TrimSpace(text) == "" {
		return nil
	}
	normalized := normalizeChunkText(text)
	runes := []rune(normalized)
	chunkSize := settings.ChunkSize
	if chunkSize <= 0 {
		chunkSize = 512
	}
	overlap := settings.OverlapSize
	if overlap < 0 {
		overlap = 0
	}
	if len(runes) <= chunkSize {
		return []string{normalized}
	}
	step := chunkSize - overlap
	if step <= 0 {
		step = chunkSize
	}
	chunks := make([]string, 0, (len(runes)/step)+1)
	lastEnd := -1
	for start := 0; start < len(runes); {
		targetEnd := min(start+chunkSize, len(runes))
		end := adjustFixedBoundary(runes, start, targetEnd, overlap)
		if end <= start || end <= lastEnd {
			end = targetEnd
		}
		chunk := strings.TrimSpace(string(runes[start:end]))
		if chunk != "" {
			chunks = append(chunks, chunk)
		}
		lastEnd = end
		if end >= len(runes) {
			break
		}
		nextStart := max(0, end-overlap)
		if nextStart <= start {
			nextStart = end
		}
		start = nextStart
	}
	return chunks
}

type structureAwareChunkingStrategy struct{}

func (structureAwareChunkingStrategy) Mode() ChunkingMode { return ChunkingModeStructureAware }

// Split 先把文本识别为结构块，再按目标长度打包成 chunk。
// 相比 fixed_size，它更适合 Markdown/技术文档，能减少标题、代码块和段落被拆散。
func (structureAwareChunkingStrategy) Split(text string, settings ChunkerSettings) []string {
	if strings.TrimSpace(text) == "" {
		return nil
	}
	text = strings.ReplaceAll(text, "\r\n", "\n")
	text = strings.ReplaceAll(text, "\r", "\n")
	blocks := segmentStructureAwareBlocks(text)
	if len(blocks) == 0 {
		return []string{text}
	}
	ranges := packStructureAwareBlocks(blocks, settings.MinChars, settings.TargetChars, settings.MaxChars)
	if len(ranges) == 0 {
		return []string{text}
	}
	return materializeStructureAware(text, ranges, settings.OverlapChars)
}

// normalizeChunkText 做分块前的轻量文本规整。
// 重点处理两类问题：Windows 回车符、中文段落中被解析器插入的硬换行，以及 URL
// 被换行拆断的情况；这样 fixed_size 分块更贴近真实语义。
func normalizeChunkText(text string) string {
	if text == "" {
		return text
	}
	src := strings.ReplaceAll(text, "\r", "")
	var out strings.Builder
	inURL := false
	for i := 0; i < len(src); i++ {
		if !inURL && looksLikeURLStart(src, i) {
			inURL = true
		}
		ch := src[i]
		if inURL {
			// URL 内部遇到换行时，如果前后字符像同一个 URL，就跳过空白把 URL 接回去。
			if unicode.IsSpace(rune(ch)) {
				j := i
				sawNewLine := false
				for j < len(src) && unicode.IsSpace(rune(src[j])) {
					if src[j] == '\n' {
						sawNewLine = true
					}
					j++
				}
				var prev byte
				if i > 0 {
					prev = src[i-1]
				}
				var next byte
				if j < len(src) {
					next = src[j]
				}
				if sawNewLine && next != 0 && shouldJoinBrokenURL(prev, next, src, j) {
					i = j - 1
					continue
				}
				out.WriteString(src[i:j])
				inURL = false
				i = j - 1
				continue
			}
			out.WriteByte(ch)
			if !isURLChar(ch) && !isCommonURLPunct(ch) {
				inURL = false
			}
			continue
		}
		if ch == '\n' {
			var prev rune
			var next rune
			if i > 0 {
				prev = rune(src[i-1])
			}
			if i+1 < len(src) {
				next = rune(src[i+1])
			}
			if isCJKWordChar(prev) && isCJKWordChar(next) {
				// 中日韩文字之间的硬换行通常来自 PDF/网页抽取，不应成为语义断点。
				continue
			}
		}
		out.WriteByte(ch)
	}
	return out.String()
}

// adjustFixedBoundary 在固定窗口 targetEnd 附近寻找更自然的边界。
// 查找优先级是换行、中文句末、英文句末；找不到才使用原始 targetEnd。
func adjustFixedBoundary(runes []rune, start, targetEnd, overlap int) int {
	if targetEnd <= start {
		return targetEnd
	}
	maxLookback := min(overlap, targetEnd-start)
	if maxLookback <= 0 {
		return targetEnd
	}
	for i := 0; i <= maxLookback; i++ {
		pos := targetEnd - i - 1
		if pos <= start {
			break
		}
		if runes[pos] == '\n' {
			return pos + 1
		}
	}
	for i := 0; i <= maxLookback; i++ {
		pos := targetEnd - i - 1
		if pos <= start {
			break
		}
		switch runes[pos] {
		case '。', '！', '？':
			return pos + 1
		}
	}
	for i := 0; i <= maxLookback; i++ {
		pos := targetEnd - i - 1
		if pos <= start {
			break
		}
		switch runes[pos] {
		case '.', '!', '?':
			next := pos + 1
			if next >= len(runes) || unicode.IsSpace(runes[next]) {
				return pos + 1
			}
		}
	}
	return targetEnd
}

// looksLikeURLStart 判断当前位置是否像 URL 起点。
func looksLikeURLStart(s string, i int) bool {
	rest := strings.ToLower(s[i:])
	return strings.HasPrefix(rest, "http://") || strings.HasPrefix(rest, "https://") || strings.HasPrefix(rest, "www.")
}

func isASCIIAlpha(b byte) bool {
	return (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z')
}

func isURLChar(b byte) bool {
	return isASCIIAlpha(b) || (b >= '0' && b <= '9')
}

func isCommonURLPunct(b byte) bool {
	switch b {
	case '.', '/', ':', '?', '&', '=', '#', '%', '-', '_', '~':
		return true
	default:
		return false
	}
}

func isCJKWordChar(r rune) bool {
	return unicode.Is(unicode.Han, r) || unicode.Is(unicode.Hiragana, r) || unicode.Is(unicode.Katakana, r) || unicode.Is(unicode.Hangul, r)
}

type structureBlockKind int

const (
	blockHeading structureBlockKind = iota
	blockCode
	blockAtomic
	blockParagraph
)

type structureBlock struct {
	kind  structureBlockKind
	start int
	end   int
}

var (
	headingPattern     = regexp.MustCompile(`^#{1,6}\s+.*$`)
	codeFencePattern   = regexp.MustCompile("^```.*$")
	atomicImagePattern = regexp.MustCompile(`^!\[[^]]*]\([^)]+\)(?:\s*"[^"]*")?\s*$`)
	atomicLinkPattern  = regexp.MustCompile(`^\[[^]]+]\([^)]+\)\s*$`)
)

// segmentStructureAwareBlocks 扫描文本并产出结构块范围。
// 它识别标题、围栏代码块、图片/链接原子块和普通段落，返回的是原文 byte 区间，
// 这样后续 materialize 时可以保留原始内容格式。
func segmentStructureAwareBlocks(text string) []structureBlock {
	blocks := make([]structureBlock, 0)
	n := len(text)
	pos := 0
	inFence := false
	fenceStart := -1
	inParagraph := false
	paragraphStart := -1

	for pos < n {
		lineEnd := strings.IndexByte(text[pos:], '\n')
		if lineEnd >= 0 {
			lineEnd += pos
		} else {
			lineEnd = n
		}
		lineEndWithNL := lineEnd
		if lineEnd < n && text[lineEnd] == '\n' {
			lineEndWithNL++
		}
		line := text[pos:lineEnd]
		trimmed := trimRightKeepLeft(line)

		if !inFence && codeFencePattern.MatchString(trimmed) {
			// 进入代码块前先结束当前段落，避免正文和代码被合并进同一结构块。
			if inParagraph {
				blocks = append(blocks, structureBlock{kind: blockParagraph, start: paragraphStart, end: pos})
				inParagraph = false
			}
			inFence = true
			fenceStart = pos
			pos = lineEndWithNL
			continue
		}
		if inFence {
			if codeFencePattern.MatchString(trimmed) {
				// 围栏代码块整体作为 blockCode，避免分块时拆开代码上下文。
				blocks = append(blocks, structureBlock{kind: blockCode, start: fenceStart, end: lineEndWithNL})
				inFence = false
			}
			pos = lineEndWithNL
			continue
		}
		if strings.TrimSpace(trimmed) == "" {
			if inParagraph {
				// 空行是段落边界，段落块到此结束。
				blocks = append(blocks, structureBlock{kind: blockParagraph, start: paragraphStart, end: pos})
				inParagraph = false
			}
			pos = lineEndWithNL
			continue
		}
		if headingPattern.MatchString(trimmed) {
			if inParagraph {
				blocks = append(blocks, structureBlock{kind: blockParagraph, start: paragraphStart, end: pos})
				inParagraph = false
			}
			// 标题单独成块，后续打包时可以和紧随内容合并，但不会被误判为普通段落。
			blocks = append(blocks, structureBlock{kind: blockHeading, start: pos, end: lineEndWithNL})
			pos = lineEndWithNL
			continue
		}
		if atomicImagePattern.MatchString(trimmed) || atomicLinkPattern.MatchString(trimmed) {
			if inParagraph {
				blocks = append(blocks, structureBlock{kind: blockParagraph, start: paragraphStart, end: pos})
				inParagraph = false
			}
			// 图片/单独链接作为原子块，避免拆分 Markdown 语法。
			blocks = append(blocks, structureBlock{kind: blockAtomic, start: pos, end: lineEndWithNL})
			pos = lineEndWithNL
			continue
		}
		if !inParagraph {
			inParagraph = true
			paragraphStart = pos
		}
		pos = lineEndWithNL
	}
	if inFence {
		blocks = append(blocks, structureBlock{kind: blockCode, start: fenceStart, end: n})
	} else if inParagraph {
		blocks = append(blocks, structureBlock{kind: blockParagraph, start: paragraphStart, end: n})
	}
	return blocks
}

// shouldJoinBrokenURL 判断 URL 中间的换行是否应该被吞掉。
// 需要排除“1. xxx”这类列表项，避免把列表误拼到前一个 URL 后面。
func shouldJoinBrokenURL(prev, next byte, s string, nextIndex int) bool {
	if isListItemStart(s, nextIndex) {
		return false
	}
	if prev == '.' && isASCIIAlpha(next) {
		return true
	}
	switch prev {
	case '/', '?', '&', '=', '#', '%', '-', '_', ':':
		return true
	}
	switch next {
	case '/', '?', '&', '=', '#':
		return true
	}
	return false
}

// isListItemStart 判断当前位置是否是数字有序列表开头。
func isListItemStart(s string, i int) bool {
	p := i
	for p < len(s) && (s[p] == ' ' || s[p] == '\t') {
		p++
	}
	digits := 0
	for p < len(s) && s[p] >= '0' && s[p] <= '9' {
		p++
		digits++
	}
	return digits > 0 && p < len(s) && s[p] == '.'
}

// trimRightKeepLeft 去掉行尾空白但保留行首缩进，避免代码块/列表缩进丢失。
func trimRightKeepLeft(s string) string {
	return strings.TrimRightFunc(s, func(r rune) bool {
		return unicode.IsSpace(r) && r != '\n' && r != '\r'
	})
}

// packStructureAwareBlocks 把结构块合并为 chunk 范围。
// 合并目标是接近 targetChars，同时不超过 maxChars；如果当前 chunk 太短，会继续吸收
// 后续块，最后一个很短的 chunk 会尝试合并回前一个。
func packStructureAwareBlocks(blocks []structureBlock, minChars, targetChars, maxChars int) [][2]int {
	if targetChars <= 0 {
		targetChars = 1400
	}
	if maxChars <= 0 {
		maxChars = 1800
	}
	if minChars <= 0 {
		minChars = 600
	}
	ranges := make([][2]int, 0)
	for i := 0; i < len(blocks); {
		chunkStart := blocks[i].start
		chunkEnd := blocks[i].end
		size := chunkEnd - chunkStart
		j := i + 1
		for j < len(blocks) {
			next := blocks[j]
			afterAdd := next.end - chunkStart
			if afterAdd <= maxChars || size < minChars || size < targetChars {
				chunkEnd = next.end
				size = afterAdd
				j++
				if size >= targetChars && size >= minChars {
					break
				}
				continue
			}
			break
		}
		ranges = append(ranges, [2]int{chunkStart, chunkEnd})
		i = j
	}
	if len(ranges) >= 2 {
		last := ranges[len(ranges)-1]
		if last[1]-last[0] < min(minChars, targetChars/2) {
			prev := &ranges[len(ranges)-2]
			if last[1]-prev[0] <= maxChars*2 {
				prev[1] = last[1]
				ranges = ranges[:len(ranges)-1]
			}
		}
	}
	return ranges
}

// materializeStructureAware 根据范围切出最终文本，并可把上一块尾部作为 overlap。
func materializeStructureAware(text string, ranges [][2]int, overlapChars int) []string {
	if len(ranges) == 0 {
		return nil
	}
	chunks := make([]string, 0, len(ranges))
	prevTail := ""
	for _, rg := range ranges {
		body := text[rg[0]:rg[1]]
		if overlapChars > 0 && prevTail != "" {
			body = prevTail + body
		}
		body = strings.TrimSpace(body)
		if body != "" {
			chunks = append(chunks, body)
		}
		if overlapChars > 0 {
			prevTail = tailByRunes(text[rg[0]:rg[1]], overlapChars)
		}
	}
	return chunks
}

// tailByRunes 按 rune 截取尾部 overlap，避免中文等多字节字符被截断。
func tailByRunes(s string, n int) string {
	if n <= 0 {
		return ""
	}
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	return string(runes[len(runes)-n:])
}
