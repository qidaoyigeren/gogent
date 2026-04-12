package entity

type QueryTermMappingDO struct {
	BaseModel
	Domain     string `gorm:"column:domain;size:64;index" json:"domain,omitempty"`
	SourceTerm string `gorm:"column:source_term;size:128;index" json:"sourceTerm"`
	TargetTerm string `gorm:"column:target_term;size:128" json:"targetTerm"`
	MatchType  int    `gorm:"column:match_type;type:smallint;default:1" json:"matchType"`
	Priority   int    `gorm:"column:priority;default:100" json:"priority"`
	Enabled    int    `gorm:"column:enabled;type:smallint;default:1" json:"enabled"`
	Remark     string `gorm:"column:remark;size:255" json:"remark,omitempty"`
	CreatedBy  string `gorm:"column:create_by;size:20" json:"createdBy,omitempty"`
	UpdatedBy  string `gorm:"column:update_by;size:20" json:"updatedBy,omitempty"`
}

func (QueryTermMappingDO) TableName() string { return "t_query_term_mapping" }
