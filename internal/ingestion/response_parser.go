package ingestion

import (
	"encoding/json"
	"strings"
)

//response_parser.go 提供**无状态的** LLM 响应解析工具函数。
//
// 设计意图：
//   - 为 enricher.applyResult 等场景提供统一的 JSON/文本解析能力
//   - 与 EnhancerNode 内的 parseStringList/parseObject 功能重复，但这里更通用、无依赖
//   - 新代码优先调用这里的函数，减少重复代码，保持解析逻辑一致

// parseStringListResponse 将 LLM 返回解析成字符串列表。
func parseStringListResponse(response string) []string {
	var arr []string
	if err := json.Unmarshal([]byte(response), &arr); err == nil {
		return arr
	}
	lines := strings.Split(response, "\n")
	result := make([]string, 0, len(lines))
	for _, line := range lines {
		line = strings.TrimSpace(line)
		// 去除 Markdown 列表符号和引号，避免 " - foo" 或 '"bar"' 这类格式污染结果。
		// 支持的符号：单引号、双引号、减号、圆点、星号、加号（常见于 LLM 输出）。
		line = strings.Trim(line, `"'\-•*`)
		if line != "" {
			result = append(result, line)
		}
	}
	return result
}

// parseObjectResponse 将 LLM 返回解析成对象（map[string]interface{}）。
//
// 解析策略（降级顺序）：
//  1. 优先按 JSON object 解析：{"key1": "value1", "key2": "value2"}
//  2. JSON 失败则按行切分，每行按首个冒号切分为 key 和 value
//
// 使用场景：
//   - EnhancerNode：元数据抽取（METADATA），如作者、日期、主题等
//   - EnricherNode：块级元数据抽取（metadata）
//
// 设计要点：
//   - strings.SplitN(line, ":", 2) 仅按首个冒号切分，允许值中包含冒号（如 URL）
//   - 空行或不含冒号的行会被忽略，容错性强
func parseObjectResponse(response string) map[string]interface{} {
	var obj map[string]interface{}
	if err := json.Unmarshal([]byte(response), &obj); err == nil {
		return obj
	}
	result := make(map[string]interface{})
	lines := strings.Split(response, "\n")
	for _, line := range lines {
		// 仅按首个冒号切分，值中可含冒号（如 URL: "http://example.com"）
		parts := strings.SplitN(line, ":", 2)
		if len(parts) == 2 {
			key := strings.TrimSpace(parts[0])
			val := strings.TrimSpace(parts[1])
			result[key] = val
		}
	}
	return result
}
