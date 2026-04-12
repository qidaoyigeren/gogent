package entity

// IntentKind represents the type of intent node.
const (
	IntentKindKB     = "KB"     // Knowledge base intent
	IntentKindMCP    = "MCP"    // MCP tool intent
	IntentKindSYSTEM = "SYSTEM" // System-only intent (no retrieval needed)
)

// IntentNodeDO maps to t_intent_node table.
type IntentNodeDO struct {
	BaseModel
	KBID                string `gorm:"column:kb_id;size:20" json:"kbId,omitempty"`
	IntentCode          string `gorm:"column:intent_code;size:64" json:"intentCode"`
	Name                string `gorm:"column:name;size:64" json:"name"`
	Level               int    `gorm:"column:level;type:smallint" json:"level"`
	ParentCode          string `gorm:"column:parent_code;size:64" json:"parentCode,omitempty"`
	Description         string `gorm:"column:description;size:512" json:"description,omitempty"`
	Examples            string `gorm:"column:examples;type:text" json:"examples,omitempty"`
	CollectionName      string `gorm:"column:collection_name;size:128" json:"collectionName,omitempty"`
	McpToolID           string `gorm:"column:mcp_tool_id;size:128" json:"mcpToolId,omitempty"`
	TopK                *int   `gorm:"column:top_k" json:"topK,omitempty"`
	Kind                int    `gorm:"column:kind;type:smallint;default:0" json:"kind"`
	SortOrder           int    `gorm:"column:sort_order;default:0" json:"sortOrder"`
	PromptSnippet       string `gorm:"column:prompt_snippet;type:text" json:"promptSnippet,omitempty"`
	PromptTemplate      string `gorm:"column:prompt_template;type:text" json:"promptTemplate,omitempty"`
	ParamPromptTemplate string `gorm:"column:param_prompt_template;type:text" json:"paramPromptTemplate,omitempty"`
	Enabled             int    `gorm:"column:enabled;type:smallint;default:1" json:"enabled"`
	CreatedBy           string `gorm:"column:create_by;size:20" json:"createdBy,omitempty"`
	UpdatedBy           string `gorm:"column:update_by;size:20" json:"updatedBy,omitempty"`
}

func (IntentNodeDO) TableName() string { return "t_intent_node" }
