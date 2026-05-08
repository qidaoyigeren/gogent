package mq

// nats_client.go 封装 NATS 消息发布与订阅能力。
// 核心概念：
//   - subjectKey 是业务侧别名（如 "document.indexed"），通过配置 map 映射到真实 NATS subject
//   - Publish 同步发布（JSON 序列化），PublishAsync 保留异步接口形态但底层仍同步
//   - Subscribe 订阅 subject 并通过回调处理消息
//   - 连接配置：5 秒超时 + RetryOnFailedConnect，适合服务启动时 NATS 未就绪的场景

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
		return fmt.Errorf("subject key not found: %s", subjectKey)
	}

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

// PublishRaw publishes raw bytes directly to a NATS subject (bypasses subject map lookup).
func (p *NATSPublisher) PublishRaw(subject string, data []byte) error {
	return p.conn.Publish(subject, data)
}

// Subscribe subscribes to a NATS subject and invokes the handler for each message.
// Returns the subscription for later unsubscribing.
func (p *NATSPublisher) Subscribe(subject string, handler func(data []byte)) (*nats.Subscription, error) {
	return p.conn.Subscribe(subject, func(msg *nats.Msg) {
		handler(msg.Data)
	})
}

// QueueSubscribe subscribes to a NATS subject with a queue group.
// In a queue group, only one subscriber receives each message (load balancing).
func (p *NATSPublisher) QueueSubscribe(subject, queue string, handler func(data []byte)) (*nats.Subscription, error) {
	return p.conn.QueueSubscribe(subject, queue, func(msg *nats.Msg) {
		handler(msg.Data)
	})
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
