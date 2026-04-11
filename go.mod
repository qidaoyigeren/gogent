module gogent

go 1.26

// go.mod 核心依赖（实际项目）
require (
	github.com/gin-gonic/gin v1.12.0
	github.com/golang-jwt/jwt/v5 v5.3.1
	github.com/milvus-io/milvus-sdk-go/v2 v2.4.2
	github.com/redis/go-redis/v9 v9.18.0
	github.com/spf13/viper v1.21.0
	golang.org/x/crypto v0.49.0
	golang.org/x/sync v0.20.0
	gorm.io/driver/postgres v1.6.0
	gorm.io/gorm v1.31.1
)