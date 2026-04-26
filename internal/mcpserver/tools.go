package mcpserver

import (
	"fmt"
	"math/rand"
	"strings"
	"time"
)

// BuiltinTools 返回所有内置工具
// 内置工具：
// 1. weather_query: 天气查询
// 2. sales_query: 销售数据查询
// 3. ticket_query: 工单查询
func BuiltinTools() []ToolDefinition {
	return []ToolDefinition{
		weatherTool(),
		salesTool(),
		ticketTool(),
	}
}

// weatherTool 天气查询工具
// 参数：
// - city: 城市名称（必填）
// - queryType: 查询类型（current/forecast）
// - days: 预报天数（默认 3，最大 7）
func weatherTool() ToolDefinition {
	return ToolDefinition{
		Name:        "weather_query",
		Description: "查询城市天气信息，支持当前天气和未来预报",
		InputSchema: schema(
			prop("city", "string", "城市名称，如北京、上海", true, nil),
			prop("queryType", "string", "查询类型：current/forecast", false, []string{"current", "forecast"}),
			prop("days", "integer", "预报天数，默认3，最大7", false, nil),
		),
		Handler: func(args map[string]interface{}) (string, bool, error) {
			city := strArg(args, "city")
			if city == "" {
				return "请提供城市名称", true, nil
			}
			queryType := strArg(args, "queryType")
			if queryType == "" {
				queryType = "current"
			}
			days := intArg(args, "days", 3)
			if days < 1 {
				days = 3
			}
			if days > 7 {
				days = 7
			}
			if queryType == "forecast" {
				return fmt.Sprintf("【%s 未来%d天天气预报】\n晴到多云，气温 16~27C，局地阵雨。", city, days), false, nil
			}
			return fmt.Sprintf("【%s 今日天气】\n多云，当前温度 23C，湿度 58%%，东南风 3-4级。", city), false, nil
		},
	}
}

// salesTool 销售数据查询工具
// 参数：
// - region: 地区（华东/华南/华北/西南/西北）
// - period: 时间段（本月/上月/本季度/上季度/本年）
// - product: 产品（企业版/专业版/基础版）
// - queryType: 查询类型（summary/ranking/detail/trend）
// - limit: 返回记录数限制
func salesTool() ToolDefinition {
	return ToolDefinition{
		Name:        "sales_query",
		Description: "查询销售数据，支持 summary/ranking/detail/trend",
		InputSchema: schema(
			prop("region", "string", "地区：华东/华南/华北/西南/西北", false, []string{"华东", "华南", "华北", "西南", "西北"}),
			prop("period", "string", "时间段：本月/上月/本季度/上季度/本年", false, []string{"本月", "上月", "本季度", "上季度", "本年"}),
			prop("product", "string", "产品：企业版/专业版/基础版", false, []string{"企业版", "专业版", "基础版"}),
			prop("queryType", "string", "查询类型：summary/ranking/detail/trend", false, []string{"summary", "ranking", "detail", "trend"}),
			prop("limit", "integer", "返回记录数限制", false, nil),
		),
		Handler: func(args map[string]interface{}) (string, bool, error) {
			region := strArg(args, "region")
			if region == "" {
				region = "全国"
			}
			period := strArg(args, "period")
			if period == "" {
				period = "本月"
			}
			queryType := strArg(args, "queryType")
			if queryType == "" {
				queryType = "summary"
			}
			seed := time.Now().YearDay() + len(region)*7 + len(period)*13
			r := rand.New(rand.NewSource(int64(seed)))
			total := 800 + r.Intn(1200)
			switch queryType {
			case "ranking":
				return fmt.Sprintf("【%s %s 销售排名】\n第1名 张三 %.2f万\n第2名 李四 %.2f万\n第3名 王五 %.2f万",
					period, region, float64(total)*0.18, float64(total)*0.15, float64(total)*0.13), false, nil
			case "detail":
				return fmt.Sprintf("【%s %s 销售明细】\n客户A 企业版 %.2f万\n客户B 专业版 %.2f万",
					period, region, float64(total)*0.09, float64(total)*0.06), false, nil
			case "trend":
				return fmt.Sprintf("【%s %s 销售趋势】\n第1周 %.2f万\n第2周 %.2f万\n第3周 %.2f万\n第4周 %.2f万",
					period, region, float64(total)*0.2, float64(total)*0.24, float64(total)*0.26, float64(total)*0.3), false, nil
			default:
				return fmt.Sprintf("【%s %s 销售汇总】\n总销售额 %.2f万，订单 %d 笔，平均客单 %.2f万。",
					period, region, float64(total), 30+r.Intn(80), float64(total)/float64(30+r.Intn(80))), false, nil
			}
		},
	}
}

// ticketTool 工单查询工具
// 参数：
// - region: 地区筛选
// - status: 工单状态筛选
// - priority: 优先级筛选
// - product: 产品筛选
// - queryType: 查询类型（summary/list/stats）
// - limit: 返回记录数限制
func ticketTool() ToolDefinition {
	return ToolDefinition{
		Name:        "ticket_query",
		Description: "查询客户工单，支持 summary/list/stats",
		InputSchema: schema(
			prop("region", "string", "地区筛选", false, []string{"华东", "华南", "华北", "西南", "西北"}),
			prop("status", "string", "工单状态筛选", false, []string{"待处理", "处理中", "已解决", "已关闭"}),
			prop("priority", "string", "优先级筛选", false, []string{"紧急", "高", "中", "低"}),
			prop("product", "string", "产品筛选", false, []string{"企业版", "专业版", "基础版"}),
			prop("queryType", "string", "查询类型：summary/list/stats", false, []string{"summary", "list", "stats"}),
			prop("limit", "integer", "返回记录数限制", false, nil),
		),
		Handler: func(args map[string]interface{}) (string, bool, error) {
			region := strArg(args, "region")
			if region == "" {
				region = "全国"
			}
			qt := strArg(args, "queryType")
			if qt == "" {
				qt = "summary"
			}
			switch qt {
			case "list":
				return fmt.Sprintf("【%s 工单列表】\nTK-202604-0001 [紧急][处理中] API调用500\nTK-202604-0002 [高][待处理] 报表导出超时", region), false, nil
			case "stats":
				return fmt.Sprintf("【%s 工单统计】\n功能异常 34%%\n性能问题 26%%\n安装部署 18%%\n其他 22%%", region), false, nil
			default:
				return fmt.Sprintf("【%s 工单汇总】\n总数 128，待处理 16，处理中 21，已解决 63，已关闭 28。", region), false, nil
			}
		},
	}
}

// schema 构建 JSON Schema
// 参数：
// - props: 属性列表（由 prop 函数生成）
// 返回值：
// - JSON Schema 对象（包含 type/properties/required）
func schema(props ...map[string]interface{}) map[string]interface{} {
	propertyMap := make(map[string]interface{}, len(props))
	required := make([]string, 0)
	for _, p := range props {
		name := p["name"].(string)
		propertyMap[name] = p["schema"]
		if p["required"].(bool) {
			required = append(required, name)
		}
	}
	out := map[string]interface{}{
		"type":       "object",
		"properties": propertyMap,
	}
	if len(required) > 0 {
		out["required"] = required
	}
	return out
}

// prop 构建单个属性定义
// 参数：
// - name: 属性名
// - typ: 属性类型（string/integer/boolean 等）
// - desc: 属性描述
// - required: 是否必填
// - enum: 枚举值（可选）
func prop(name, typ, desc string, required bool, enum []string) map[string]interface{} {
	s := map[string]interface{}{
		"type":        typ,
		"description": desc,
	}
	if len(enum) > 0 {
		s["enum"] = enum
	}
	return map[string]interface{}{
		"name":     name,
		"required": required,
		"schema":   s,
	}
}

// strArg 从参数 map 中获取字符串值
func strArg(args map[string]interface{}, key string) string {
	v, _ := args[key].(string)
	return strings.TrimSpace(v)
}

// intArg 从参数 map 中获取整数值
// 参数：
// - args: 参数 map
// - key: 键名
// - d: 默认值
func intArg(args map[string]interface{}, key string, d int) int {
	switch v := args[key].(type) {
	case float64:
		return int(v)
	case int:
		return v
	default:
		return d
	}
}
