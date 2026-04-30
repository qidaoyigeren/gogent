package llmutil

import (
	"encoding/json"
	"strings"
)

// CleanJSONBlock strips ```json ... ``` wrappers from LLM output.
// LLM 经常把 JSON 包在 Markdown fenced code block 里，解析前先去掉包裹。
func CleanJSONBlock(raw string) string {
	s := strings.TrimSpace(raw)
	// Remove ```json or ``` prefix
	if strings.HasPrefix(s, "```json") {
		s = strings.TrimPrefix(s, "```json")
	} else if strings.HasPrefix(s, "```") {
		s = strings.TrimPrefix(s, "```")
	}
	// Remove trailing ```
	if strings.HasSuffix(s, "```") {
		s = strings.TrimSuffix(s, "```")
	}
	return strings.TrimSpace(s)
}

// ParseJSON attempts to parse LLM output as JSON after cleaning.
// 泛型返回值让调用方可以直接解析成目标结构体或 map。
func ParseJSON[T any](raw string) (T, error) {
	var result T
	cleaned := CleanJSONBlock(raw)
	err := json.Unmarshal([]byte(cleaned), &result)
	return result, err
}

// ExtractThinkContent extracts content after </think> tag if present.
// 推理模型可能输出 <think>...</think>，业务通常只需要标签后的最终答案。
func ExtractThinkContent(raw string) string {
	idx := strings.Index(raw, "</think>")
	if idx >= 0 {
		return strings.TrimSpace(raw[idx+len("</think>"):])
	}
	return raw
}

// TruncateString truncates a string to maxLen characters.
// 按 rune 截断，避免把中文或 emoji 这类多字节字符截成非法 UTF-8。
func TruncateString(s string, maxLen int) string {
	runes := []rune(s)
	if len(runes) <= maxLen {
		return s
	}
	return string(runes[:maxLen])
}
