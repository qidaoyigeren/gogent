package rewrite

import (
	"sort"
	"strings"
	"sync"
)

// MatchTypeExact is the only match type currently implemented (plain substring).
// Other values (e.g. regex, prefix) are reserved for future extension, matching
// the Java QueryTermMappingService behaviour.
const MatchTypeExact = 1

// TermMapping represents a term normalisation rule persisted in the DB.
type TermMapping struct {
	Original   string
	Normalized string
	MatchType  int // 1 = exact substring (default); other values are skipped
	Priority   int // higher value = higher priority (applied first)
}

// TermMapper normalises domain-specific terms in queries.
type TermMapper struct {
	mappings []TermMapping // sorted: priority DESC, then source-length DESC
	mu       sync.RWMutex
}

// NewTermMapper creates a TermMapper from a list of mappings.
func NewTermMapper(mappings []TermMapping) *TermMapper {
	sorted := sortMappings(mappings)
	return &TermMapper{mappings: sorted}
}

// NewEmptyTermMapper creates a TermMapper with no mappings.
func NewEmptyTermMapper() *TermMapper {
	return &TermMapper{}
}

// Normalize applies all enabled exact-match term mappings to the input text.
// Rules are applied in priority-DESC / source-length-DESC order to ensure
// higher-priority and longer rules take precedence.
func (m *TermMapper) Normalize(text string) string {
	m.mu.RLock()
	defer m.mu.RUnlock()

	if len(m.mappings) == 0 {
		return text
	}

	result := text
	for _, mapping := range m.mappings {
		// Only exact substring match is implemented; skip other types.
		if mapping.MatchType != 0 && mapping.MatchType != MatchTypeExact {
			continue
		}
		result = applyMapping(result, mapping.Original, mapping.Normalized)
	}
	return result
}

// ReloadMappings replaces the current mappings with new ones (thread-safe).
// ReloadMappings 用于重新加载术语映射关系
// 它接收一个 TermMapping 类型的切片作为参数，对映射关系进行排序并更新到 TermMapper 实例中
func (m *TermMapper) ReloadMappings(mappings []TermMapping) {
	// 对传入的映射关系进行排序
	sorted := sortMappings(mappings)
	// 使用互斥锁保护共享资源的并发访问
	m.mu.Lock()
	// 更新映射关系为排序后的结果
	m.mappings = sorted
	// 解锁互斥锁
	m.mu.Unlock()
}

// sortMappings sorts rules by priority DESC then source-term rune-length DESC.
// This mirrors the Java QueryTermMappingService ordering:
//
//	Comparator.comparing(priority).reversed().thenComparing(sourceLen).reversed()
func sortMappings(mappings []TermMapping) []TermMapping {
	sorted := make([]TermMapping, len(mappings))
	copy(sorted, mappings)
	sort.SliceStable(sorted, func(i, j int) bool {
		if sorted[i].Priority != sorted[j].Priority {
			return sorted[i].Priority > sorted[j].Priority // higher priority first
		}
		return len([]rune(sorted[i].Original)) > len([]rune(sorted[j].Original)) // longer source first
	})
	return sorted
}

// applyMapping performs a safe substring replacement that avoids re-replacing
// positions that already contain the target term — mirroring Java's
// QueryTermMappingUtil.applyMapping logic.
//
// Example: source="平安保险" target="平安保司"
// If the text already contains "平安保司" at some position the replacement is
// skipped for that occurrence so it is not double-replaced.
func applyMapping(text, source, target string) string {
	if text == "" || source == "" {
		return text
	}

	var sb strings.Builder
	sb.Grow(len(text))

	idx := 0
	sourceLen := len(source)
	targetLen := len(target)

	for idx < len(text) {
		hit := strings.Index(text[idx:], source)
		if hit < 0 {
			sb.WriteString(text[idx:])
			break
		}
		hit += idx // absolute position

		// Copy everything before the match.
		sb.WriteString(text[idx:hit])

		// If the position already starts with the target term, keep it as-is.
		if target != "" && hit+targetLen <= len(text) && text[hit:hit+targetLen] == target {
			sb.WriteString(text[hit : hit+targetLen])
			idx = hit + targetLen
		} else {
			sb.WriteString(target)
			idx = hit + sourceLen
		}
	}

	return sb.String()
}
