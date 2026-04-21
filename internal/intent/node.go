package intent

// IntentNode 意图树节点（3 层结构：system -> topic -> detail）
// 核心职责：
// 1. 定义意图分类的层级结构
// 2. 关联知识库 ID 和 MCP 工具
// 3. 提供树形遍历方法（查找子节点、收集叶子节点等）
type IntentNode struct {
	ID                  string        `json:"id"`                            // 节点唯一 ID
	Name                string        `json:"name"`                          // 节点名称
	Description         string        `json:"description,omitempty"`         // 节点描述（用于 LLM 分类）
	Level               int           `json:"level"`                         // 层级：1=系统，2=主题，3=详情
	ParentID            string        `json:"parentId,omitempty"`            // 父节点 ID
	Children            []*IntentNode `json:"children,omitempty"`            // 子节点列表
	KBIDs               []string      `json:"kbIds,omitempty"`               // 关联的知识库 ID 列表
	MCPTools            []string      `json:"mcpTools,omitempty"`            // 关联的 MCP 工具 ID 列表
	Keywords            []string      `json:"keywords,omitempty"`            // 分类提示关键词
	Enabled             bool          `json:"enabled"`                       // 是否启用
	Kind                string        `json:"kind,omitempty"`                // 类型：KB（知识库）、MCP（工具）、SYSTEM（系统）
	TopK                *int          `json:"topK,omitempty"`                // 节点级 TopK 覆盖（可选）
	PromptTemplate      string        `json:"promptTemplate,omitempty"`      // 自定义提示词（system-only 场景）
	ParamPromptTemplate string        `json:"paramPromptTemplate,omitempty"` // MCP 参数提取提示词
}

func (n *IntentNode) IsLeaf() bool {
	return len(n.Children) == 0
}

// FindChild 方法用于在当前意图节点的子节点中查找指定ID的子节点
func (n *IntentNode) FindChild(id string) *IntentNode {
	for _, child := range n.Children {
		if child.ID == id {
			return child
		}
	}
	return nil
}

// AllLeaves 收集子树下的所有叶子节点（递归）
func (n *IntentNode) AllLeaves() []*IntentNode {
	if n.IsLeaf() {
		return []*IntentNode{n}
	}
	var leaves []*IntentNode
	for _, child := range n.Children {
		leaves = append(leaves, child.AllLeaves()...)
	}
	return leaves
}

// FlattenAll 返回子树中的所有节点（BFS 广度优先遍历）
func (n *IntentNode) FlattenAll() []*IntentNode {
	var result []*IntentNode
	queue := []*IntentNode{n}
	for len(queue) > 0 {
		curr := queue[0]
		queue = queue[1:]
		result = append(result, curr)
		queue = append(queue, curr.Children...)
	}
	return result
}
