package main

import (
	"context"
	"database/sql"
	"fmt"
	"gogent/internal/auth"
	"gogent/internal/chat"
	"gogent/internal/config"
	"gogent/internal/embedding"
	"gogent/internal/entity"
	"gogent/internal/guidance"
	"gogent/internal/handler"
	"gogent/internal/ingestion"
	"gogent/internal/intent"
	"gogent/internal/mcp"
	"gogent/internal/memory"
	"gogent/internal/middleware"
	"gogent/internal/model"
	"gogent/internal/mq"
	"gogent/internal/orchestrator"
	internalparser "gogent/internal/parser"
	"gogent/internal/prompt"
	"gogent/internal/rerank"
	"gogent/internal/retrieve"
	"gogent/internal/rewrite"
	"gogent/internal/scheduler"
	"gogent/internal/service"
	"gogent/internal/storage"
	"gogent/internal/vector"
	"gogent/pkg/idgen"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
)

func main() {
	// 1. Load config
	cfgPath := "config/config.yaml"
	if p := os.Getenv("CONFIG_PATH"); p != "" {
		cfgPath = p
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		slog.Error("failed to load config", "err", err)
		os.Exit(1)
	}

	// Initialize JWT signing key from config
	auth.InitJWT(cfg.Server.JWTSecret)

	// Initialize Snowflake ID generator (workerID=0, datacenterID=0 for single instance)
	if err := idgen.Init(0, 0); err != nil {
		slog.Error("failed to initialize id generator", "err", err)
		os.Exit(1)
	}

	// 2. Initialize infrastructure
	// Redis
	rdb := redis.NewClient(&redis.Options{
		Addr:     cfg.Redis.Addr,
		Password: cfg.Redis.Password,
		DB:       cfg.Redis.DB,
	})
	if err := rdb.Ping(context.Background()).Err(); err != nil {
		slog.Warn("redis not available", "err", err)
	}

	// NATS
	var natsPub *mq.NATSPublisher
	if cfg.NATS.URL != "" {
		natsPub, err = mq.NewNATSPublisher(context.Background(), mq.NATSConfig{URL: cfg.NATS.URL})
		if err != nil {
			slog.Warn("nats not available, pub/sub disabled", "err", err)
		} else {
			slog.Info("nats connected", "url", cfg.NATS.URL)
		}
	}

	// PostgreSQL with advanced connection pool (pgxpool)
	var db *gorm.DB

	poolConfig, err := pgxpool.ParseConfig(cfg.Database.DSN)
	if err != nil {
		slog.Error("failed to parse pool config", "err", err)
		os.Exit(1)
	}

	// Configure connection pool
	poolConfig.MaxConns = int32(cfg.Database.MaxOpenConns)
	poolConfig.MinConns = int32(cfg.Database.MaxIdleConns)

	// Parse ConnMaxLifetime
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

	// Use GORM with standard driver
	db, err = gorm.Open(postgres.Open(cfg.Database.DSN), &gorm.Config{})
	if err != nil {
		slog.Warn("database not available, some features disabled", "err", err)
	}

	// 3. Build service graph (manual DI)
	// Model routing
	healthStore := model.NewHealthStore(
		cfg.AI.Selection.FailureThreshold,
		cfg.AI.Selection.OpenDurationMs,
	)
	selector := model.NewSelector(healthStore)

	// LLM Chat
	llmSvc := chat.NewRoutingLLMService(cfg.AI, healthStore, selector)

	// Embedding & Rerank
	embSvc := embedding.NewRoutingEmbeddingService(cfg.AI, healthStore, selector)
	rerankSvc := rerank.NewRoutingRerankService(cfg.AI, healthStore, selector)

	// Query Rewrite — load term mappings from DB at startup
	termMapper := rewrite.NewEmptyTermMapper()
	if db != nil {
		termMapper = rewrite.NewTermMapper(loadTermMappings(db))
	}
	rewriteSvc := rewrite.NewMultiQuestionRewriteService(llmSvc, termMapper, cfg.RAG.QueryRewrite)

	// Intent — load intent tree from DB at startup
	classifier := intent.NewDefaultClassifier(llmSvc)
	intentResolver := intent.NewResolver(classifier, intent.MaxIntentCount)
	intentCache := intent.NewTreeCache(rdb, func(ctx context.Context) ([]*intent.IntentNode, error) {
		if db == nil {
			return []*intent.IntentNode{}, nil
		}
		return loadIntentTree(ctx, db)
	})

	// Guidance
	guidanceSvc := guidance.NewService(guidance.Config{
		Enabled:            true,
		AmbiguityThreshold: 0.3,
		MaxOptions:         4,
	})

	// Vector Store - choose based on config
	var vectorSvc vector.VectorStoreService
	switch cfg.RAG.Vector.Type {
	case "milvus":
		vectorSvc = vector.NewMilvusService(cfg.Milvus.URI)
	case "pg":
		var sqlDB *sql.DB
		if db != nil {
			sqlDB, _ = db.DB()
		}
		vectorSvc = vector.NewPgVectorService(sqlDB)
		slog.Info("using pgvector store")
	default:
		slog.Info("using no-op vector store (vector disabled)")
		vectorSvc = vector.NewNoOpService()
	}

	// File Storage (Java-aligned interface injection)
	// Day14 离线入库从这里拿 FileStore：上传时保存文件，分块时再按 SourceLocation 读取。
	var fileStore storage.FileStore
	storageType := strings.ToLower(strings.TrimSpace(cfg.Storage.Type))
	if storageType == "" {
		storageType = "local"
	}
	var s3BucketAdmin storage.BucketAdmin // non-nil when S3 is active
	switch storageType {
	case "s3":
		s3Store, s3Err := storage.NewS3FileStorage(context.Background(), storage.S3Config{
			Endpoint:           cfg.Storage.S3.Endpoint,
			AccessKeyID:        cfg.Storage.S3.AccessKeyID,
			SecretAccessKey:    cfg.Storage.S3.SecretAccessKey,
			Region:             cfg.Storage.S3.Region,
			Bucket:             cfg.Storage.S3.Bucket,
			BasePath:           cfg.Storage.S3.BasePath,
			UsePathStyle:       cfg.Storage.S3.UsePathStyle,
			ReliableUpload:     cfg.Storage.S3.ReliableUpload,
			ReliableMaxRetries: cfg.Storage.S3.ReliableMaxRetries,
		})
		if s3Err != nil {
			slog.Warn("failed to initialize s3 storage, fallback to local", "err", s3Err)
			fileStore = storage.NewLocalFileStore(cfg.Storage.BasePath)
		} else {
			fileStore = s3Store
			s3BucketAdmin = s3Store // S3FileStorage implements BucketAdmin
			slog.Info("using s3 file storage", "bucket", cfg.Storage.S3.Bucket)
		}
	default:
		fileStore = storage.NewLocalFileStore(cfg.Storage.BasePath)
		slog.Info("using local file storage", "basePath", cfg.Storage.BasePath)
	}
	var tikaParser *internalparser.TikaParser
	if strings.TrimSpace(cfg.Ingestion.Tika.Endpoint) != "" {
		// Day14 离线入库节点：Tika 是可选的外部文档解析服务。
		// 主要用途：用 /tika 接口把 PDF/Word/Excel 等二进制文件转换为纯文本。
		// 未配置时 parser 节点只能使用内置文本/HTML 解析，不支持复杂二进制格式。
		timeout := 30 * time.Second
		if cfg.Ingestion.Tika.TimeoutMs > 0 {
			timeout = time.Duration(cfg.Ingestion.Tika.TimeoutMs) * time.Millisecond
		}
		tikaParser = internalparser.NewTikaParser(internalparser.TikaConfig{
			Endpoint: cfg.Ingestion.Tika.Endpoint,
			Timeout:  timeout,
		})
	}
	// Day14 离线入库的核心：IngestionRuntime 组装了 pipeline 全部节点所需的依赖。
	// 包含：embedding 服务（chunker 生成向量）、vector 库（indexer 写入）、
	// LLM 服务（enhancer/enricher 等节点优化）、Tika 解析器（parser 解析二进制）。
	// KnowledgeDocHandler（上传入库）和 IngestionHandler（管理 API）都复用这个 Runtime，
	// 保证手动上传和管理端创建任务的流水线执行逻辑一致。
	ingestionRuntime := ingestion.NewRuntime(
		embSvc,
		vectorSvc,
		llmSvc,
		tikaParser,
		cfg.RAG.Default.Dimension,
		cfg.RAG.Default.CollectionName,
	)

	// Retrieval Engine
	intentChRaw := retrieve.NewIntentDirectedChannel(
		embSvc, vectorSvc,
		cfg.RAG.Search.Channels.IntentDirected.MinIntentScore,
		cfg.RAG.Search.Channels.IntentDirected.TopKMultiplier,
	)
	vectorChRaw := retrieve.NewVectorGlobalChannel(
		embSvc, vectorSvc,
		cfg.RAG.Default.CollectionName,
		cfg.RAG.Search.Channels.VectorGlobal.ConfidenceThreshold,
		cfg.RAG.Search.Channels.VectorGlobal.TopKMultiplier,
		db,
	)
	dedupPP := retrieve.NewDeduplicationPostProcessor()
	rerankPP := retrieve.NewRerankPostProcessor(rerankSvc)
	intentPri := intentChRaw.Priority()
	if cfg.RAG.Search.Channels.IntentDirected.Priority != nil {
		intentPri = *cfg.RAG.Search.Channels.IntentDirected.Priority
	}
	vectorPri := vectorChRaw.Priority()
	if cfg.RAG.Search.Channels.VectorGlobal.Priority != nil {
		vectorPri = *cfg.RAG.Search.Channels.VectorGlobal.Priority
	}
	retrieveEngine := retrieve.NewMultiChannelEngine(
		[]retrieve.SearchChannel{
			retrieve.NewWrappedChannel(intentChRaw, intentPri, config.ChannelEnabled(cfg.RAG.Search.Channels.IntentDirected.Enabled)),
			retrieve.NewWrappedChannel(vectorChRaw, vectorPri, config.ChannelEnabled(cfg.RAG.Search.Channels.VectorGlobal.Enabled)),
		},
		[]retrieve.PostProcessor{dedupPP, rerankPP},
	)

	// MCP
	mcpRegistry := mcp.NewRegistry()
	mcpExtractor := mcp.NewParamExtractor(llmSvc)

	// Auto-configure MCP servers (connect and register remote tools)
	mcp.AutoConfigure(context.Background(), mcpRegistry, cfg.RAG.MCP)

	// Memory
	var memorySvc *memory.ConversationMemoryService
	if db != nil {
		memStore := memory.NewStore(db)
		summarySvc := memory.NewSummaryService(llmSvc, memStore, cfg.RAG.Memory)
		memorySvc = memory.NewConversationMemoryService(memStore, summarySvc, cfg.RAG.Memory.HistoryKeepTurns)
	} else {
		// Fallback: no-op memory (in-memory store would be needed)
		slog.Warn("database not available, memory service disabled")
	}

	// Prompt
	promptSvc := prompt.NewRAGPromptService()

	// RAG Orchestrator
	guidancePending := guidance.NewConversationPending()
	ragSvc := orchestrator.NewRAGChatService(
		llmSvc, memorySvc, rewriteSvc, intentResolver, intentCache,
		guidanceSvc, guidancePending, retrieveEngine, mcpRegistry, mcpExtractor, promptSvc, db,
	)

	// Rate limiter (distributed)
	var rateLimiter *service.RateLimiter
	if rdb != nil {
		rateLimiter = service.NewRateLimiter(rdb, natsPub, service.RateLimitConfig{
			Enabled:        cfg.RAG.RateLimit.Global.Enabled,
			MaxConcurrent:  cfg.RAG.RateLimit.Global.MaxConcurrent,
			MaxWaitSeconds: cfg.RAG.RateLimit.Global.MaxWaitSeconds,
			LeaseSeconds:   cfg.RAG.RateLimit.Global.LeaseSeconds,
			PollIntervalMs: cfg.RAG.RateLimit.Global.PollIntervalMs,
		})
	}

	// Distributed task cancellation
	if rdb != nil || natsPub != nil {
		handler.GetTaskManager().EnableDistributedCancel(rdb, natsPub, 30*time.Minute)
	}

	// 4. Database initialization (manual SQL migration - production best practice)
	if db != nil {
		// AutoMigrate disabled - use manual SQL migration for better control
		// Run this command to initialize database:
		//   psql -U postgres -d ragent -f resources/database/schema_pg.sql
		//   psql -U postgres -d ragent -f resources/database/init_data_pg.sql

		// Check if tables exist
		if !db.Migrator().HasTable("t_conversation") {
			slog.Warn("Database tables not found. Please initialize database manually:",
				"command", "psql -U postgres -d ragent -f resources/database/schema_pg.sql")
		} else {
			slog.Info("Database tables verified successfully")
		}

		// Ensure default admin user exists
		handler.EnsureDefaultAdmin(db)

	}

	// 5. Setup Gin router
	gin.SetMode(gin.ReleaseMode)
	r := gin.New()
	r.Use(handler.RecoveryMiddleware())
	r.Use(handler.CORSMiddleware())
	r.Use(handler.RequestLogMiddleware())

	// Health check
	r.GET("/health", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"status": "ok"})
	})

	// API routes
	api := r.Group(cfg.Server.ContextPath)

	// Auth routes (no auth middleware)
	handler.NewAuthHandler(db, rdb).RegisterRoutes(api)

	// Demo mode: intercept mutating requests before auth
	r.Use(middleware.DemoModeMiddleware(cfg.Server.DemoMode, cfg.Server.ContextPath))

	// Apply auth middleware to all other routes
	api.Use(auth.AuthMiddleware(rdb, cfg.Server.ContextPath))
	// Upload semaphore: limit concurrent uploads before multipart parsing
	api.Use(handler.UploadSemaphoreMiddleware(rdb, cfg.Server.UploadMaxConcurrent))
	// Apply idempotent middleware (prevents duplicate form submissions)
	api.Use(middleware.IdempotentMiddleware(rdb))
	{
		// Chat
		handler.NewChatHandler(ragSvc, db, rateLimiter, rdb).RegisterRoutes(api)

		// Conversations & Feedback
		handler.NewConversationHandler(db).RegisterRoutes(api)
		handler.NewFeedbackHandler(db).RegisterRoutes(api)

		// Users
		handler.NewUserHandler(db).RegisterRoutes(api)

		// Knowledge Base
		kbHandler := handler.NewKnowledgeBaseHandler(db, vectorSvc, cfg.RAG.Default.Dimension)
		if s3BucketAdmin != nil {
			// S3 存储启用时，知识库创建会同步准备每个 collection 对应的 bucket。
			kbHandler.WithBucketAdmin(s3BucketAdmin)
		}
		kbHandler.RegisterRoutes(api)

		// Day14 离线入库的 HTTP 控制中枢：KnowledgeDocHandler 管理文档的完整生命周期。
		// 职责包括：
		//   1. 上传 API：把 multipart 文件保存到 FileStore，创建文档行，初始化任务队列
		//   2. 分块 worker：异步轮询任务表中的 pending 任务，调用 runtime 执行 pipeline
		//   3. 全量替换：每次入库前删除旧 chunks 和旧向量，再写新结果，避免重复
		//   4. 调度集成：与 scheduler 协作，支持定时刷新（如每天更新飞书文档）
		// DocHandler 和 scheduler 一起构成了从"上传到可检索"到"定期维护"的完整离线链路。
		docHandler := handler.NewKnowledgeDocHandler(db, fileStore, vectorSvc, embSvc, tikaParser, ingestionRuntime)
		docHandler.RegisterRoutes(api)
		// 启动本地文档分块 worker：独立 goroutine 每 2 秒轮询一次任务表中的 pending 任务。
		// 一旦有待分块的文档，worker 会声明(claim)任务、执行 pipeline 并回写结果。
		// defer Stop 确保服务优雅关闭时停止轮询，避免任务中断。
		docHandler.StartChunkTaskWorker()
		defer docHandler.StopChunkTaskWorker()
		handler.NewKnowledgeChunkHandler(db, vectorSvc, embSvc).RegisterRoutes(api)

		// Ingestion pipeline & task services (wired to engine)
		// 管理端自定义 pipeline 和 task API 共享同一个 Runtime，与文档 pipeline 模式保持一致。
		pipelineSvc := ingestion.NewPipelineService(db)
		taskSvc := ingestion.NewTaskService(db, pipelineSvc, ingestionRuntime)
		handler.NewIngestionHandler(db, pipelineSvc, taskSvc).RegisterRoutes(api)

		// RAG Management
		handler.NewIntentTreeHandler(db).RegisterRoutes(api)
		handler.NewMappingHandler(db, termMapper).RegisterRoutes(api)
		handler.NewSampleQuestionHandler(db).RegisterRoutes(api)

		// Settings & Traces
		handler.NewSettingsHandler(cfg).RegisterRoutes(api)
		handler.NewTraceHandler(db).RegisterRoutes(api)

		// Dashboard (admin)
		handler.NewDashboardHandler(db).RegisterRoutes(api)

		if db != nil {
			// Day14 调度链路：离线维护、增量更新的关键组件。
			// 三层结构：
			//   1. ScheduleLockManager：分布式锁，防止多实例竞争同一个调度任务
			//   2. ScheduleRefreshProcessor：刷新处理器，调用 docHandler.RefreshDocument 执行入库
			//   3. KnowledgeDocumentScheduleJob：定时扫描、task 分配、失败恢复
			// 工作流程：
			//   - scanJob 每 10 秒扫描一次 t_knowledge_document_schedule 表，找出 next_run_time 已到期的任务
			//   - 通过 lockMgr 尝试获取分布式锁，成功者提交 executor 队列进行处理
			//   - refreshProc.Process 调用 docHandler.RefreshDocument 重新下载和入库，并更新 next_run_time
			//   - recoveryJob 每 60 秒检查一次 RUNNING 文档，若 30 分钟未更新则强制置为 FAILED，避免卡死
			lockMgr := scheduler.NewScheduleLockManager(rdb, db, 300)
			refreshProc := scheduler.NewScheduleRefreshProcessor(db, lockMgr, docHandler)
			scheduleJob := scheduler.NewKnowledgeDocumentScheduleJob(db, lockMgr, refreshProc, 10, 5)
			scheduleJob.Start(context.Background())
			// 停服时先停止 scheduleJob，避免退出过程中继续拉起新的文档刷新。
			// 后续 defer StopChunkTaskWorker 才能安全地关闭 docHandler 的 worker。
			defer scheduleJob.Stop()
		}
	}

	// 6. Start server with graceful shutdown
	addr := fmt.Sprintf(":%d", cfg.Server.Port)
	srv := &http.Server{
		Addr:    addr,
		Handler: r,
	}

	go func() {
		slog.Info("go-ragent server starting", "addr", addr, "contextPath", cfg.Server.ContextPath)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			slog.Error("server error", "err", err)
			os.Exit(1)
		}
	}()

	// Wait for interrupt
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit

	slog.Info("shutting down server...")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if natsPub != nil {
		natsPub.Close()
	}
	if rateLimiter != nil {
		rateLimiter.Stop()
	}
	if err := srv.Shutdown(ctx); err != nil {
		slog.Error("server shutdown error", "err", err)
	}
	slog.Info("server stopped")
}

// loadTermMappings loads all enabled term mappings from the database.
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

// loadIntentTree loads all enabled intent nodes from DB and builds the tree.
func loadIntentTree(ctx context.Context, db *gorm.DB) ([]*intent.IntentNode, error) {
	var dos []entity.IntentNodeDO
	if err := db.WithContext(ctx).
		Select(
			"id", "kb_id", "intent_code", "name", "level", "parent_code",
			"description", "collection_name", "mcp_tool_id", "top_k", "kind",
			"sort_order", "prompt_template", "param_prompt_template",
		).
		Where("deleted = 0 AND CAST(enabled AS TEXT) IN ('1','true','t')").
		Find(&dos).Error; err != nil {
		return nil, err
	}
	return buildIntentNodeTree(dos), nil
}

// buildIntentNodeTree converts IntentNodeDO list into a hierarchical IntentNode tree.
func buildIntentNodeTree(dos []entity.IntentNodeDO) []*intent.IntentNode {
	kindStr := map[int]string{
		0: entity.IntentKindKB,
		1: entity.IntentKindMCP,
		2: entity.IntentKindSYSTEM,
	}

	nodeMap := make(map[string]*intent.IntentNode, len(dos))
	for i := range dos {
		do := &dos[i]
		node := &intent.IntentNode{
			ID:                  do.ID,
			Name:                do.Name,
			Description:         do.Description,
			Level:               do.Level,
			ParentID:            do.ParentCode,
			Enabled:             true,
			Kind:                kindStr[do.Kind],
			TopK:                do.TopK,
			PromptTemplate:      do.PromptTemplate,
			ParamPromptTemplate: do.ParamPromptTemplate,
		}
		if do.KBID != "" {
			node.KBIDs = []string{do.KBID}
		}
		if do.McpToolID != "" {
			node.MCPTools = []string{do.McpToolID}
		}
		nodeMap[do.ID] = node
	}

	var roots []*intent.IntentNode
	for i := range dos {
		do := &dos[i]
		node := nodeMap[do.ID]
		if do.ParentCode == "" {
			roots = append(roots, node)
		} else if parent, ok := nodeMap[do.ParentCode]; ok {
			parent.Children = append(parent.Children, node)
		} else {
			// Parent not in result set (disabled/deleted), treat as root
			roots = append(roots, node)
		}
	}
	return roots
}
