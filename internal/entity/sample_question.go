package entity

type SampleQuestionDO struct {
	BaseModel
	Title       string `gorm:"column:title;size:64" json:"title,omitempty"`
	Description string `gorm:"column:description;size:255" json:"description,omitempty"`
	Question    string `gorm:"column:question;size:255" json:"question"`
}

func (SampleQuestionDO) TableName() string { return "t_sample_question" }
