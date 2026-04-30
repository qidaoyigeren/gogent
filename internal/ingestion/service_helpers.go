package ingestion

import (
	"encoding/json"
	"gogent/pkg/idgen"
)

// nextID 统一生成 ingestion 相关表的字符串 ID，避免服务层直接依赖 idgen 细节。
func nextID() string {
	return idgen.NextIDStr()
}

// parseJSONText 将数据库中的 JSON 字符串转换为 RawMessage。
// 空字符串表示未配置，返回 nil 让节点走默认设置。
func parseJSONText(raw string) json.RawMessage {
	if raw == "" {
		return nil
	}
	return json.RawMessage(raw)
}

// marshalJSONText 将 RawMessage 保存回数据库文本字段。
// null 和空值都折叠为空字符串，减少无意义配置噪声。
func marshalJSONText(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	return string(raw)
}

// normalizeNodeType 标准化节点类型；未知值原样返回，方便错误信息保留用户输入。
func normalizeNodeType(value string) string {
	nodeType, err := ParseIngestionNodeType(value)
	if err != nil {
		return value
	}
	return string(nodeType)
}

// stringValue 读取可选字符串指针，nil 时返回空字符串。
func stringValue(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}
