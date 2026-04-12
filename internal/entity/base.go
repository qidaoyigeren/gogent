package entity

import "time"

type BaseModel struct {
	ID         string    `gorm:"column:id;primaryKey" json:"id"`
	CreateTime time.Time `gorm:"column:create_time;autoCreateTime" json:"createTime"`
	UpdateTime time.Time `gorm:"column:update_time;autoUpdateTime" json:"updateTime"`
	Deleted    int       `gorm:"column:deleted;default:0" json:"-"`
}
