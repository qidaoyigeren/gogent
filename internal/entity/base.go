package entity

import "time"

// BaseModel provides common fields for all entities, matching the PostgreSQL schema.
// Note: Not all tables have created_by/updated_by fields in Java version.
// Entities that need these fields should define them explicitly.
type BaseModel struct {
	ID         string    `gorm:"column:id;primaryKey" json:"id"`
	CreateTime time.Time `gorm:"column:create_time;autoCreateTime" json:"createTime"`
	UpdateTime time.Time `gorm:"column:update_time;autoUpdateTime" json:"updateTime"`
	Deleted    int       `gorm:"column:deleted;default:0" json:"-"`
}
