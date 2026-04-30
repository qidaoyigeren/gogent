package mq

// nats_publisher.go 封装 NATS 消息发布能力。
// 它作为可选的事件通知扩展点存在，当前 go-ragent 主链路不强依赖 NATS。
// 核心概念：
//   - subjectKey 是业务侧别名（如 "document.indexed"），通过配置 map 映射到真实 NATS subject
//   - Publish 同步发布（JSON 序列化），PublishAsync 保留异步接口形态但底层仍同步
//   - 连接配置：5 秒超时 + RetryOnFailedConnect，适合服务启动时 NATS 未就绪的场景
//
// Day14 关注点：知道它在"文档入库完成"等事件时可以发布通知，用于异步通知下游消费者。
// 生产中可用于：入库完成 → 通知搜索服务预热缓存、触发评估任务等。

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/nats-io/nats.go"
)

// NATSPublisher publishes messages to NATS.
type NATSPublisher struct {
	conn     *nats.Conn
	subjects map[string]string
}

// NATSConfig holds NATS configuration.
type NATSConfig struct {
	URL      string
	Subjects map[string]string
}

// NewNATSPublisher creates a new NATS publisher.
func NewNATSPublisher(ctx context.Context, cfg NATSConfig) (*NATSPublisher, error) {
	// NATS 用于事件通知类扩展；Day14 只需要知道它负责把业务事件发布到配置的 subject。
	conn, err := nats.Connect(cfg.URL,
		nats.Timeout(5*time.Second),
		nats.RetryOnFailedConnect(true),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to NATS: %w", err)
	}

	return &NATSPublisher{
		conn:     conn,
		subjects: cfg.Subjects,
	}, nil
}

// Publish publishes a message to a subject.
func (p *NATSPublisher) Publish(ctx context.Context, subjectKey string, data interface{}) error {
	subject, ok := p.subjects[subjectKey]
	if !ok {
		// subjectKey 是业务侧别名，必须先在配置中映射到真实 NATS subject。
		return fmt.Errorf("subject key not found: %s", subjectKey)
	}

	// 发布前统一 JSON 序列化，消费者按统一消息格式解析。
	payload, err := json.Marshal(data)
	if err != nil {
		return fmt.Errorf("failed to marshal message: %w", err)
	}

	err = p.conn.Publish(subject, payload)
	if err != nil {
		return fmt.Errorf("failed to publish message: %w", err)
	}

	slog.Debug("message published", "subject", subject, "size", len(payload))
	return nil
}

// PublishAsync publishes a message asynchronously with callback.
func (p *NATSPublisher) PublishAsync(ctx context.Context, subjectKey string, data interface{}, callback func(error)) error {
	subject, ok := p.subjects[subjectKey]
	if !ok {
		return fmt.Errorf("subject key not found: %s", subjectKey)
	}

	payload, err := json.Marshal(data)
	if err != nil {
		return fmt.Errorf("failed to marshal message: %w", err)
	}

	// NATS v1.50 uses different async API
	// 当前实现仍使用同步 Publish，然后立即触发 callback，保留异步调用方的接口形态。
	err = p.conn.Publish(subject, payload)
	if err != nil {
		if callback != nil {
			callback(err)
		}
		return fmt.Errorf("failed to publish message: %w", err)
	}

	slog.Debug("message published", "subject", subject, "size", len(payload))
	if callback != nil {
		callback(nil)
	}

	return nil
}

// Close closes the NATS connection.
func (p *NATSPublisher) Close() {
	p.conn.Close()
}

// IsConnected checks if the connection is active.
func (p *NATSPublisher) IsConnected() bool {
	return p.conn.IsConnected()
}

// Status returns the connection status.
func (p *NATSPublisher) Status() nats.Status {
	return p.conn.Status()
}
