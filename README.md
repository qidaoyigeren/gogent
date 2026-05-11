# Gogent - RAG 智能体平台

Gogent 是一个工程级 **RAG（检索增强生成）智能体平台**，解决企业非结构化知识（产品手册、技术文档、FAQ、政策文件）如何被大语言模型准确、可控、可观测地检索和利用的问题。

## 核心特性

- **11 阶段 RAG 流水线** — 记忆加载 → 查询改写 → 意图识别 → 多路检索 → MCP 工具调用 → Prompt 组装 → LLM 生成，每阶段独立降级与全链路追踪
- **意图驱动路由** — 3 级意图树 + LLM 分类 + 歧义检测与引导澄清
- **多路检索引擎** — 意图定向通道 + 全局向量通道，支持去重与重排序
- **MCP 工具集成** — Model Context Protocol 协议的工具调用，支持 LLM 参数提取与并行执行
- **熔断降级** — 三态熔断器（CLOSED/OPEN/HALF_OPEN）实现模型自动切换
- **会话记忆** — N 轮保留 + LLM 摘要压缩
- **离线文档摄取** — 6 节点流水线（Fetcher → Parser → Chunker → Enhancer → Enricher → Indexer）+ 定时刷新
- **分布式限流** — Redis 信号量 + FIFO 队列 + NATS 通知
- **SSE 流式输出** — 支持思考链（think）与回答（response）双通道流式返回

## 技术栈

### 后端

| 技术 | 用途 |
|------|------|
| Go 1.26 | 主语言 |
| Gin | HTTP 框架 |
| GORM + PostgreSQL | ORM 与主数据库 |
| pgx/v5 | PostgreSQL 连接池 |
| go-redis/v9 | 缓存、分布式锁、限流、JWT 黑名单 |
| NATS | 分布式消息队列 |
| Milvus SDK / pgvector | 向量数据库 |
| AWS SDK Go v2 | S3 兼容对象存储（MinIO/RustFS） |
| Viper | 配置管理 |
| golang-jwt | JWT 认证 |
| robfig/cron | 定时任务 |
| Apache Tika | 二进制文档解析（PDF、Word、Excel） |

### 前端

| 技术 | 用途 |
|------|------|
| React 18 + TypeScript | UI 框架 |
| Vite | 构建工具 |
| Tailwind CSS | 样式 |
| Radix UI | 无障碍 UI 组件 |
| Zustand | 状态管理 |
| React Router | 路由 |
| Recharts | 图表（仪表盘） |
| react-markdown | Markdown 渲染 |

## 项目结构

```
gogent/
├── cmd/
│   ├── server/          # 主服务入口（端口 9090）
│   └── mcpserver/       # MCP 工具服务入口（端口 9099）
├── config/
│   └── config.yaml      # 中心化配置文件
├── docs/                # 深度技术文档
├── frontend/            # React/TypeScript 前端
├── internal/
│   ├── auth/            # JWT 认证
│   ├── chat/            # LLM 对话服务（OpenAI 兼容 / Ollama）
│   ├── config/          # 配置加载（Viper + 环境变量占位符）
│   ├── embedding/       # 向量嵌入服务（Ollama / SiliconFlow）
│   ├── entity/          # GORM 数据库实体
│   ├── guidance/        # 歧义检测与引导
│   ├── handler/         # Gin HTTP 处理器
│   ├── ingestion/       # 离线文档摄取流水线
│   ├── intent/          # 意图分类（3 级树 + LLM）
│   ├── mcp/             # MCP 客户端（注册、执行、参数提取）
│   ├── mcpserver/       # MCP JSON-RPC 服务端
│   ├── memory/          # 会话记忆（N 轮 + 摘要压缩）
│   ├── middleware/       # 幂等、演示模式中间件
│   ├── model/           # 熔断器、模型选择与路由
│   ├── mq/              # NATS 消息发布
│   ├── orchestrator/    # RAG 流水线编排器（核心）
│   ├── parser/          # Apache Tika 文档解析
│   ├── prompt/          # Prompt 构建器
│   ├── rerank/          # 重排序服务
│   ├── retrieve/        # 多路检索引擎
│   ├── rewrite/         # 查询改写（LLM + 规则回退）
│   ├── scheduler/       # 定时文档刷新
│   ├── service/         # 限流、追踪、会话服务
│   ├── storage/         # 文件存储（本地 / S3）
│   ├── token/           # Token 估算
│   └── vector/          # 向量存储（Milvus / pgvector）
└── pkg/
    ├── errcode/         # 结构化错误码
    ├── idgen/           # Snowflake ID 生成器
    ├── llmutil/         # LLM 输出工具函数
    └── response/        # 统一 HTTP 响应结构
```

## 快速开始

### 环境要求

- Go 1.26+
- PostgreSQL 15+
- Redis 7+
- NATS
- 向量数据库：Milvus 或 PostgreSQL pgvector 扩展
- （可选）Apache Tika — 用于解析 PDF/Word/Excel
- （可选）MinIO / RustFS — S3 兼容对象存储
- （可选）Ollama — 本地 LLM 推理

### 配置

编辑 `config/config.yaml`，支持环境变量占位符：

```yaml
database:
  dsn: "host=${DB_HOST:localhost} port=${DB_PORT:5432} user=${DB_USER:postgres} password=${DB_PASS:postgres} dbname=${DB_NAME:gogent} sslmode=disable"

redis:
  addr: "${REDIS_ADDR:localhost:6379}"

nats:
  url: "${NATS_URL:nats://localhost:4222}"
```

### 启动

```bash
# 启动主服务
go run cmd/server/main.go

# 启动 MCP 工具服务（可选）
go run cmd/mcpserver/main.go
```

主服务监听 `:9090`，上下文路径 `/api/ragent`。

### 前端开发

```bash
cd frontend
npm install
npm run dev
```

## RAG 流水线架构

```
用户输入
  │
  ▼
┌─────────────┐
│ 1. 记忆加载  │ ← 并行加载历史消息 + 摘要
├─────────────┤
│ 2. 记忆追加  │ ← 立即持久化当前轮
├─────────────┤
│ 3. 查询改写  │ ← LLM 子问题拆分 + 术语映射
├─────────────┤
│ 4. 意图识别  │ ← 3 级意图树 LLM 分类（并行）
├─────────────┤
│ 5. 引导判断  │ ← 歧义检测 → 引导澄清 / 系统短路
├─────────────┤
│ 6. 执行计划  │ ← 映射 KB ID + MCP 工具 + TopK
├─────────────┤
│ 7. 多路检索  │ ← 意图定向 + 全局向量（并行）→ 去重 → 重排
├─────────────┤
│ 8. MCP 执行  │ ← 并行工具调用 + LLM 参数提取
├─────────────┤
│ 9. Prompt 构建│ ← 系统提示 + 记忆 + 检索结果 + 工具结果
├─────────────┤
│ 10. 动态温度  │ ← 纯检索=0.0 / 含工具=0.3
├─────────────┤
│ 11. LLM 生成  │ ← 流式(SSE) / 非流式，熔断自动切换模型
└─────────────┘
  │
  ▼
SSE 事件流：meta → message → finish/cancel/reject → done
```

## SSE 事件协议

流式聊天接口 `GET /rag/v3/chat` 返回以下事件序列：

| 事件 | 说明 |
|------|------|
| `meta` | 首帧，包含 conversationId、taskId |
| `message` | 流式增量，type 为 `response`（回答）或 `think`（思考链） |
| `finish` / `cancel` / `reject` | 结束状态 |
| `error` | 错误（可选） |
| `done` | 终止帧 |

## 数据模型

| 表名 | 说明 |
|------|------|
| `t_conversation` | 对话 |
| `t_message` | 消息 |
| `t_conversation_summary` | LLM 生成的会话摘要 |
| `t_knowledge_base` | 知识库 |
| `t_knowledge_document` | 文档 |
| `t_knowledge_chunk` | 文档分块 |
| `t_knowledge_document_schedule` | 文档定时刷新计划 |
| `t_knowledge_document_schedule_exec` | 刷新执行历史 |
| `t_knowledge_document_chunk_log` | 分块处理日志 |
| `t_intent_node` | 意图树节点 |
| `t_user` | 用户 |
| `t_feedback` | 用户反馈 |
| `t_trace_run` / `t_trace_node` | 全链路追踪 |
| `t_query_term_mapping` | 查询术语映射 |
| `t_sample_question` | 意图训练样本问题 |

## 深度文档

- [熔断器与模型降级](docs/circuit-breaker-deep-dive.md)
- [多路检索策略](docs/retrieval-strategy-deep-dive.md)
- [LLM 流式探测与故障转移](docs/llm-streaming-probe-deep-dive.md)
- [性能优化与稳定性设计](docs/模块16-性能优化与稳定性设计.md)
- [架构全景（面试向）](go-wild-pebble.md)

## 许可证

MIT
