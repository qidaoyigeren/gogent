package ingestion

// ConditionEvaluator 在 engine 每步**执行前**对 NodeConfig.Condition 求值，为 true 才跑该节点。
// 支持：JSON 布尔、expr 文本表达式、{all/any/not/field,operator,value} 对象；无法解析时**默认放行**。

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"github.com/expr-lang/expr"
)

type ConditionEvaluator struct{}

func NewConditionEvaluator() *ConditionEvaluator {
	return &ConditionEvaluator{}
}

// Evaluate 对给定条件进行求值，决定节点是否应该执行。
// 支持四种条件格式：
//  1. JSON 布尔值 true/false：直接返回
//  2. JSON 字符串：按 expr 表达式语言求值（如 "ChunkCount > 10"）
//  3. JSON 对象：支持 all/any/not/field 组合逻辑和字段比较
//  4. null 或空：视为条件满足，节点默认执行
//
// 应用场景：
//   - 跳过某些节点（如对小文件跳过 LLM 增强）
//   - 按文件类型选择不同分支（如 image/* 走不同处理链）
//   - 基于上游输出决策（如 chunk 数量超过阈值时索引）
//
// 容错设计：条件格式无法识别时选择放行，避免错误配置导致流程被静默跳过。
func (e *ConditionEvaluator) Evaluate(ctx *IngestionContext, condition json.RawMessage) bool {
	if len(condition) == 0 || string(condition) == "null" {
		// 未配置 condition 表示节点默认执行，这是 pipeline 最常见路径。
		return true
	}

	// Try boolean
	var boolVal bool
	if err := json.Unmarshal(condition, &boolVal); err == nil {
		return boolVal
	}

	// Try string (expression)
	var strVal string
	if err := json.Unmarshal(condition, &strVal); err == nil {
		return e.evalExpression(ctx, strVal)
	}

	// Try object
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(condition, &obj); err == nil {
		return e.evalObject(ctx, obj)
	}

	// 条件格式无法识别时选择放行，避免错误配置导致整条链路被静默跳过。
	return true
}

// evalObject 解析 JSON 对象：优先识别关键字 all/any/not，否则若含 field 则走单条比较规则。
func (e *ConditionEvaluator) evalObject(ctx *IngestionContext, obj map[string]json.RawMessage) bool {
	if all, ok := obj["all"]; ok {
		return e.evalAll(ctx, all)
	}

	if any, ok := obj["any"]; ok {
		return e.evalAny(ctx, any)
	}

	if not, ok := obj["not"]; ok {
		return !e.Evaluate(ctx, not)
	}

	if _, ok := obj["field"]; ok {
		return e.evalRule(ctx, obj)
	}

	return true
}

// evalAll 实现逻辑 AND，所有子条件都通过才执行节点。
func (e *ConditionEvaluator) evalAll(ctx *IngestionContext, node json.RawMessage) bool {
	var items []json.RawMessage
	if err := json.Unmarshal(node, &items); err != nil {
		return true // 解析失败不拦截：与 Evaluate 对未知格式默认放行一致
	}
	for _, item := range items {
		if !e.Evaluate(ctx, item) {
			return false // 短路：一项为假则整体为假
		}
	}
	return true
}

// evalAny 实现逻辑 OR，任一子条件通过即可执行节点。
func (e *ConditionEvaluator) evalAny(ctx *IngestionContext, node json.RawMessage) bool {
	var items []json.RawMessage
	if err := json.Unmarshal(node, &items); err != nil {
		return true
	}
	for _, item := range items {
		if e.Evaluate(ctx, item) {
			return true // 任一为真即可
		}
	}
	return false
}

// evalRule 执行字段比较规则：{"field": "FieldName", "operator": "op", "value": val}
// 支持的字段有限，只暴露必要的 IngestionContext 字段给条件表达式，避免过度耦合。
// 支持的 operator 包括：eq/ne/in/contains/regex/gt/gte/lt/lte/exists/not_exists
//
// 典型规则示例：
//
//	{"field": "MimeType", "operator": "eq", "value": "application/pdf"}
//	{"field": "Keywords", "operator": "contains", "value": "敏感词"}
//	{"field": "Metadata.source", "operator": "in", "value": ["web", "api"]}
func (e *ConditionEvaluator) evalRule(ctx *IngestionContext, obj map[string]json.RawMessage) bool {
	fieldBytes, ok := obj["field"]
	if !ok {
		return true
	}
	var field string
	json.Unmarshal(fieldBytes, &field)
	if field == "" {
		return true
	}

	operator := "eq"
	if opBytes, ok := obj["operator"]; ok {
		json.Unmarshal(opBytes, &operator)
	}

	left := e.readField(ctx, field)
	right := e.parseValue(obj["value"])

	return e.compare(left, right, operator)
}

// readField 支持从 IngestionContext 读取顶层字段或 Metadata.xxx 这样的嵌套字段。
func (e *ConditionEvaluator) readField(ctx *IngestionContext, path string) interface{} {
	// Support nested field access like "Metadata.key" or "RawText"
	parts := strings.Split(path, ".")
	var current interface{} = ctx

	for _, part := range parts {
		// 路径 "Metadata.xxx"：先到 ctx 取 Metadata 得到 map，再取下级 key
		switch v := current.(type) {
		case *IngestionContext:
			current = e.getFieldFromContext(v, part)
		case map[string]interface{}:
			current = v[part]
		default:
			return nil // 无法再向下钻取
		}
	}
	return current
}

// getFieldFromContext 明确暴露允许被条件表达式访问的上下文字段。
// 未列出的字段返回 nil，避免条件配置依赖过多内部实现细节。
func (e *ConditionEvaluator) getFieldFromContext(ctx *IngestionContext, field string) interface{} {
	switch field {
	case "TaskID":
		return ctx.TaskID
	case "PipelineID":
		return ctx.PipelineID
	case "RawText":
		return ctx.RawText
	case "EnhancedText":
		return ctx.EnhancedText
	case "MimeType":
		return ctx.MimeType
	case "Keywords":
		return ctx.Keywords
	case "Questions":
		return ctx.Questions
	case "Metadata":
		return ctx.Metadata
	default:
		return nil
	}
}

// parseValue 将 JSON 原始值解析成 Go 值，供 compare 做统一比较。
func (e *ConditionEvaluator) parseValue(val json.RawMessage) interface{} {
	if len(val) == 0 {
		return nil
	}

	var v interface{}
	if err := json.Unmarshal(val, &v); err != nil {
		return nil
	}
	return v
}

// compare 根据 operator 类型执行对应的比较操作。
// 支持的 operator 类别：
//
//	字符串：eq/ne/contains/regex - 对字符串进行精确/包含/正则匹配
//	集合：in - 判断左或右是否在对方列表中
//	数字：gt/gte/lt/lte - 数值大小比较
//	存在性：exists/not_exists - 判断字段是否存在（非 nil）
//
// 设计要点：
//   - 每个 operator 都带容错机制，格式不符时采用降级方案
//   - normalize 做轻量标准化（去首尾空白），避免空白差异导致比较失败
//   - 未知 operator 默认降级为 eq，确保条件不会导致节点被意外跳过
func (e *ConditionEvaluator) compare(left, right interface{}, operator string) bool {
	operator = strings.ToLower(operator)

	switch operator {
	case "eq":
		return e.normalize(left) == e.normalize(right)
	case "ne":
		return e.normalize(left) != e.normalize(right)
	case "in":
		return e.in(left, right)
	case "contains":
		return e.contains(left, right)
	case "regex":
		return e.regex(left, right)
	case "gt":
		return e.compareNumber(left, right) > 0
	case "gte":
		return e.compareNumber(left, right) >= 0
	case "lt":
		return e.compareNumber(left, right) < 0
	case "lte":
		return e.compareNumber(left, right) <= 0
	case "exists":
		return left != nil
	case "not_exists":
		return left == nil
	default:
		return e.normalize(left) == e.normalize(right)
	}
}

// normalize 做轻量标准化，目前主要去掉字符串首尾空白。
func (e *ConditionEvaluator) normalize(value interface{}) interface{} {
	if s, ok := value.(string); ok {
		return strings.TrimSpace(s)
	}
	return value
}

// in 支持“左值在右侧列表中”或“右值在左侧列表中”两种常见配置写法。
func (e *ConditionEvaluator) in(left, right interface{}) bool {
	// right is a list
	if list, ok := right.([]interface{}); ok {
		for _, item := range list {
			if e.normalize(left) == e.normalize(item) {
				return true
			}
		}
		return false
	}
	// left is a list
	if list, ok := left.([]interface{}); ok {
		for _, item := range list {
			if e.normalize(item) == e.normalize(right) {
				return true
			}
		}
		return false
	}
	return e.normalize(left) == e.normalize(right)
}

// contains 支持字符串包含和列表包含，用于按 MIME、关键词或 metadata 值筛选节点。
func (e *ConditionEvaluator) contains(left, right interface{}) bool {
	if left == nil || right == nil {
		return false
	}
	// string contains
	if ls, ok := left.(string); ok {
		return strings.Contains(ls, fmt.Sprintf("%v", right))
	}
	// list contains
	if list, ok := left.([]interface{}); ok {
		for _, item := range list {
			if e.normalize(item) == e.normalize(right) {
				return true
			}
		}
	}
	return false
}

// regex 使用正则表达式匹配字段值，适合复杂文件名或 MIME 规则。
func (e *ConditionEvaluator) regex(left, right interface{}) bool {
	if left == nil || right == nil {
		return false
	}
	pattern := fmt.Sprintf("%v", right)
	matched, err := regexp.MatchString(pattern, fmt.Sprintf("%v", left))
	if err != nil {
		return false
	}
	return matched
}

// compareNumber 将两侧值转成 float64 后比较，支持 gt/gte/lt/lte。
func (e *ConditionEvaluator) compareNumber(left, right interface{}) int {
	l := e.toFloat(left)
	r := e.toFloat(right)
	if l == nil || r == nil {
		return 0
	}
	if *l < *r {
		return -1
	} else if *l > *r {
		return 1
	}
	return 0
}

// toFloat 兼容 JSON 数字、Go 整型/浮点和可解析的数字字符串。
func (e *ConditionEvaluator) toFloat(value interface{}) *float64 {
	switch v := value.(type) {
	case float64:
		return &v
	case float32:
		f := float64(v)
		return &f
	case int:
		f := float64(v)
		return &f
	case int64:
		f := float64(v)
		return &f
	case int32:
		f := float64(v)
		return &f
	case string:
		var f float64
		if _, err := fmt.Sscanf(v, "%f", &f); err == nil {
			return &f
		}
	}
	return nil
}

// evalExpression 用 github.com/expr-lang/expr 编译布尔表达式，与 Java SpEL 角色类似，语法为 expr 方言。
func (e *ConditionEvaluator) evalExpression(ctx *IngestionContext, expression string) bool {
	env := map[string]interface{}{
		"ctx":        ctx,
		"TaskID":     ctx.TaskID,
		"PipelineID": ctx.PipelineID,
		"RawText":    ctx.RawText,
		"MimeType":   ctx.MimeType,
		"Metadata":   ctx.Metadata,
		"Keywords":   ctx.Keywords,
	}

	program, err := expr.Compile(expression, expr.Env(env), expr.AsBool())
	if err != nil {
		// 表达式编译失败视为条件不满足，避免执行不确定分支。
		return false
	}

	output, err := expr.Run(program, env)
	if err != nil {
		return false
	}

	if result, ok := output.(bool); ok {
		return result
	}
	return false
}
