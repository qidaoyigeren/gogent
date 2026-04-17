package intent

// IntentNode represents a node in the intent tree (3-level: system -> topic -> detail).
type IntentNode struct {
	ID                  string        `json:"id"`
	Name                string        `json:"name"`
	Description         string        `json:"description,omitempty"`
	Level               int           `json:"level"` // 1=system, 2=topic, 3=detail
	ParentID            string        `json:"parentId,omitempty"`
	Children            []*IntentNode `json:"children,omitempty"`
	KBIDs               []string      `json:"kbIds,omitempty"`    // associated knowledge base IDs
	MCPTools            []string      `json:"mcpTools,omitempty"` // associated MCP tool IDs
	Keywords            []string      `json:"keywords,omitempty"` // hint keywords for classification
	Enabled             bool          `json:"enabled"`
	Kind                string        `json:"kind,omitempty"`                // KB, MCP, SYSTEM
	TopK                *int          `json:"topK,omitempty"`                // node-level topK override
	PromptTemplate      string        `json:"promptTemplate,omitempty"`      // custom prompt for system-only
	ParamPromptTemplate string        `json:"paramPromptTemplate,omitempty"` // custom MCP param extraction prompt
}
