package main

import (
	"context"
	"fmt"
	"gogent/internal/config"
	"gogent/internal/handler"
	"gogent/pkg/idgen"
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
)

func main() {
	// ========== Day 1: 配置加载 ==========
	cfgPath := "config/config.yaml"
	if p := os.Getenv("CONFIG_PATH"); p != "" {
		cfgPath = p
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		slog.Error("failed to load config", "err", err)
		os.Exit(1)
	}

	// ========== Day 3: ID 生成器 ==========
	if err := idgen.Init(0, 0); err != nil {
		slog.Error("failed to initialize id generator", "err", err)
		os.Exit(1)
	}

	// ========== Day 3: 基础设施初始化 ==========

	// Redis
	rdb := redis.NewClient(&redis.Options{
		Addr:     cfg.Redis.Addr,
		Password: cfg.Redis.Password,
		DB:       cfg.Redis.DB,
	})
	if err := rdb.Ping(context.Background()).Err(); err != nil {
		slog.Warn("redis not available", "err", err)
	}

	// PostgreSQL 连接池
	poolConfig, err := pgxpool.ParseConfig(cfg.Database.DSN)
	if err != nil {
		slog.Error("failed to parse pool config", "err", err)
		os.Exit(1)
	}

	poolConfig.MaxConns = int32(cfg.Database.MaxOpenConns)
	poolConfig.MinConns = int32(cfg.Database.MaxIdleConns)

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

	// GORM（Day 3 初始化，后续 Days 使用）
	_, err = gorm.Open(postgres.Open(cfg.Database.DSN), &gorm.Config{})
	if err != nil {
		slog.Warn("database not available, some features disabled", "err", err)
	}

	// ========== Day 2: 路由与中间件 ==========
	gin.SetMode(gin.ReleaseMode)
	r := gin.New()
	r.Use(handler.RecoveryMiddleware())
	r.Use(handler.CORSMiddleware())
	r.Use(handler.RequestLogMiddleware())

	// 健康检查
	r.GET("/health", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"status": "ok"})
	})

	// ========== 启动服务 ==========
	addr := fmt.Sprintf(":%d", cfg.Server.Port)
	slog.Info("server starting", "addr", addr)

	if err := r.Run(addr); err != nil {
		slog.Error("server failed", "err", err)
		os.Exit(1)
	}

}
