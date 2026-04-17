package main

import (
	"context"
	"fmt"
	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"
	"gogent/internal/auth"
	"gogent/internal/config"
	"gogent/internal/entity"
	"gogent/internal/handler"
	"gogent/internal/middleware"
	"gogent/internal/model"
	"gogent/internal/rewrite"
	"gogent/pkg/idgen"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)

func main() {
	// 1) Load config
	cfgPath := "config/config.yaml"
	if p := os.Getenv("CONFIG_PATH"); p != "" {
		cfgPath = p
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		slog.Error("failed to load config", "err", err)
		os.Exit(1)
	}
	// 2) Bootstrap basics
	auth.InitJWT("")
	if err := idgen.Init(0, 0); err != nil {
		slog.Error("failed to initialize id generator", "err", err)
		os.Exit(1)
	}
	// 3) Redis
	rdb := redis.NewClient(&redis.Options{
		Addr:     cfg.Redis.Addr,
		Password: cfg.Redis.Password,
		DB:       cfg.Redis.DB,
	})
	if err := rdb.Ping(context.Background()).Err(); err != nil {
		slog.Error("redis not available (day5 needs redis for auth middleware)", "err", err)
		os.Exit(1)
	}
	// 4) DB pool + GORM
	poolConfig, err := pgxpool.ParseConfig(cfg.Database.DSN)
	if err != nil {
		slog.Error("failed to parse pool config", "err", err)
		os.Exit(1)
	}
	poolConfig.MaxConns = int32(cfg.Database.MaxOpenConns)
	poolConfig.MinConns = int32(cfg.Database.MaxIdleConns)
	if d, e := time.ParseDuration(cfg.Database.ConnMaxLifetime); e == nil {
		poolConfig.MaxConnLifetime = d
	} else {
		poolConfig.MaxConnLifetime = 30 * time.Minute
	}
	poolConfig.MaxConnIdleTime = 30 * time.Minute
	poolConfig.HealthCheckPeriod = time.Minute
	if _, err := pgxpool.NewWithConfig(context.Background(), poolConfig); err != nil {
		slog.Warn("database pool init failed, continue with gorm", "err", err)
	}
	db, err := gorm.Open(postgres.Open(cfg.Database.DSN), &gorm.Config{})
	if err != nil {
		slog.Error("database not available (day5 management apis require db)", "err", err)
		os.Exit(1)
	}
	// 5) Day5 focus: model routing base
	healthStore := model.NewHealthStore(
		cfg.AI.Selection.FailureThreshold,
		cfg.AI.Selection.OpenDurationMs,
	)
	_ = model.NewSelector(healthStore)
	// MappingHandler needs termMapper injection
	termMapper := rewrite.NewEmptyTermMapper()
	termMapper.ReloadMappings(loadTermMappings(db))
	// 6) Router (keep Day5 scope)
	gin.SetMode(gin.ReleaseMode)
	r := gin.New()
	r.Use(handler.RecoveryMiddleware())
	r.Use(handler.CORSMiddleware())
	r.Use(handler.RequestLogMiddleware())
	r.GET("/health", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"status": "ok"})
	})
	api := r.Group(cfg.Server.ContextPath)
	// Auth routes (no auth middleware)
	handler.NewAuthHandler(db, rdb).RegisterRoutes(api)
	// Protected routes
	api.Use(auth.AuthMiddleware(rdb, cfg.Server.ContextPath))
	api.Use(middleware.IdempotentMiddleware(rdb))
	{
		// Day5 target: RAG management APIs
		handler.NewIntentTreeHandler(db).RegisterRoutes(api)
		handler.NewMappingHandler(db, termMapper).RegisterRoutes(api)
		handler.NewSampleQuestionHandler(db).RegisterRoutes(api)
	}
	// 7) Start + graceful shutdown
	addr := fmt.Sprintf(":%d", cfg.Server.Port)
	srv := &http.Server{Addr: addr, Handler: r}
	go func() {
		slog.Info("day5 server starting", "addr", addr, "contextPath", cfg.Server.ContextPath)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			slog.Error("server error", "err", err)
			os.Exit(1)
		}
	}()
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = srv.Shutdown(ctx)
}
func loadTermMappings(db *gorm.DB) []rewrite.TermMapping {
	type termMappingRow struct {
		SourceTerm string
		TargetTerm string
	}
	var records []termMappingRow
	db.Model(&entity.QueryTermMappingDO{}).
		Select("source_term", "target_term").
		Where("deleted = 0 AND CAST(enabled AS TEXT) IN ('1','true','t')").
		Find(&records)
	mappings := make([]rewrite.TermMapping, 0, len(records))
	for _, r := range records {
		mappings = append(mappings, rewrite.TermMapping{
			Original:   r.SourceTerm,
			Normalized: r.TargetTerm,
		})
	}
	return mappings
}
