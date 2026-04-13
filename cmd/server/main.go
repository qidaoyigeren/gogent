package main

import (
	"context"
	"gogent/internal/auth"
	"gogent/internal/config"
	"gogent/pkg/idgen"
	"log/slog"
	"os"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
)

func main() {
	// 1. 加载配置
	cfgPath := "config/config.yaml"
	if p := os.Getenv("CONFIG_PATH"); p != "" {
		cfgPath = p
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		slog.Error("failed to load config", "err", err)
		os.Exit(1)
	}

	// 2. 初始化 JWT
	auth.InitJWT("")

	// 3. 初始化 Snowflake ID 生成器
	if err := idgen.Init(0, 0); err != nil {
		slog.Error("failed to initialize id generator", "err", err)
		os.Exit(1)
	}

	// 4. 连接 Redis
	rdb := redis.NewClient(&redis.Options{
		Addr:     cfg.Redis.Addr,
		Password: cfg.Redis.Password,
		DB:       cfg.Redis.DB,
	})
	if err := rdb.Ping(context.Background()).Err(); err != nil {
		slog.Warn("redis not available", "err", err)
	}

	// 解析 pgxpool 配置
	poolConfig, err := pgxpool.ParseConfig(cfg.Database.DSN)
	if err != nil {
		slog.Error("failed to parse pool config", "err", err)
		os.Exit(1)
	}

	// 配置连接池参数
	poolConfig.MaxConns = int32(cfg.Database.MaxOpenConns)
	poolConfig.MinConns = int32(cfg.Database.MaxIdleConns)

	// 解析连接最大存活时间
	if duration, err := time.ParseDuration(cfg.Database.ConnMaxLifetime); err == nil {
		poolConfig.MaxConnLifetime = duration
	} else {
		poolConfig.MaxConnLifetime = 30 * time.Minute
	}

	poolConfig.MaxConnIdleTime = 30 * time.Minute
	poolConfig.HealthCheckPeriod = time.Minute

	_, err = pgxpool.NewWithConfig(context.Background(), poolConfig)
	if err != nil {
		slog.Warn("database pool init failed, fallback to default", "err", err)
	} else {
		slog.Info("database connection pool initialized",
			"maxConns", poolConfig.MaxConns,
			"minConns", poolConfig.MinConns)
	}

	// 使用 GORM 初始化数据库
	_, err = gorm.Open(postgres.Open(cfg.Database.DSN), &gorm.Config{})
	if err != nil {
		slog.Warn("database not available, some features disabled", "err", err)
	}
}
