package entity

// UserDO maps to t_user table.
type UserDO struct {
	BaseModel
	Username string `gorm:"column:username;uniqueIndex:uk_user_username;size:64" json:"username"`
	Password string `gorm:"column:password;size:128" json:"-"`
	Role     string `gorm:"column:role;size:32" json:"role"`
	Avatar   string `gorm:"column:avatar;size:128" json:"avatar,omitempty"`
}

func (UserDO) TableName() string { return "t_user" }
