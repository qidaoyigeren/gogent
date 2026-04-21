package llmutil

import (
	"encoding/json"
	"strings"
)

func CleanJSONBlock(raw string) string {
	s := strings.TrimSpace(raw)
	if strings.HasPrefix(s, "```json") {
		s = strings.TrimPrefix(s, "```json")
	} else if strings.HasPrefix(s, "```") {
		s = strings.TrimPrefix(s, "```")
	}
	if strings.HasSuffix(s, "```") {
		s = strings.TrimSuffix(s, "```")
	}
	s = strings.TrimSpace(s)
	return s
}

func ParseJSON[T any](raw string) (T, error) {
	var result T
	cleaned := CleanJSONBlock(raw)
	err := json.Unmarshal([]byte(cleaned), &result)
	return result, err
}

func ExtractThinkContent(raw string) string {
	idx := strings.Index(raw, "</think>")
	if idx >= 0 {
		return strings.TrimSpace(raw[idx+len("</think>"):])
	}
	return raw
}

func TruncateString(s string, maxLen int) string {
	runes := []rune(s)
	if len(runes) <= maxLen {
		return s
	}
	return string(runes[:maxLen])
}
