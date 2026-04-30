package chat

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"gogent/internal/model"
)

// Message represents a chat message (role + content).
type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// ChatRequest is the request to the LLM.
type ChatRequest struct {
	Messages    []Message `json:"messages"`
	Temperature float64   `json:"temperature,omitempty"`
	TopP        float64   `json:"top_p,omitempty"`
	MaxTokens   int       `json:"max_tokens,omitempty"`
	Stream      bool      `json:"stream"`
	Thinking    bool      `json:"thinking,omitempty"`
	ModelID     string    `json:"-"`
}

// ChatResponse is a non-streaming response.
type ChatResponse struct {
	Content          string
	ReasoningContent string // thinking / chain-of-thought (non-stream), when provider returns it
	FinishReason     string
	Usage            Usage
}

type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// StreamDelta is a chunk from streaming response.
type StreamDelta struct {
	Content      string
	FinishReason string
	IsThinking   bool
	Err          error
}

// LLMService is the interface for LLM chat.
type LLMService interface {
	Chat(ctx context.Context, messages []Message, opts ...ChatOption) (*ChatResponse, error)
	ChatStream(ctx context.Context, messages []Message, opts ...ChatOption) (<-chan StreamDelta, error)
}

type ModelSelectableLLMService interface {
	LLMService
	ChatWithModelID(ctx context.Context, modelID string, messages []Message, opts ...ChatOption) (*ChatResponse, error)
	ChatStreamWithModelID(ctx context.Context, modelID string, messages []Message, opts ...ChatOption) (<-chan StreamDelta, error)
}

// ChatOption configures a chat request.
type ChatOption func(*ChatRequest)

func WithTemperature(t float64) ChatOption {
	return func(r *ChatRequest) { r.Temperature = t }
}

func WithMaxTokens(n int) ChatOption {
	return func(r *ChatRequest) { r.MaxTokens = n }
}

func WithTopP(v float64) ChatOption {
	return func(r *ChatRequest) { r.TopP = v }
}

func WithThinking(v bool) ChatOption {
	return func(r *ChatRequest) { r.Thinking = v }
}

func WithModelID(modelID string) ChatOption {
	return func(r *ChatRequest) { r.ModelID = modelID }
}

// --- OpenAI-compatible client (works for BaiLian, SiliconFlow) ---

type openAIClient struct {
	httpClient *http.Client
}

// newOpenAIClient 创建并返回一个新的OpenAI客户端实例
// 该函数初始化一个带有120秒超时的HTTP客户端
func newOpenAIClient() *openAIClient {
	return &openAIClient{ // 返回一个openAIClient结构体的指针实例
		httpClient: &http.Client{Timeout: 120 * time.Second}, // 初始化HTTP客户端，设置请求超时时间为120秒
	}
}

// openAI request/response structures
type openAIRequest struct {
	Model          string    `json:"model"`
	Messages       []Message `json:"messages"`
	Temperature    float64   `json:"temperature,omitempty"`
	TopP           float64   `json:"top_p,omitempty"`
	MaxTokens      int       `json:"max_tokens,omitempty"`
	Stream         bool      `json:"stream"`
	EnableThinking *bool     `json:"enable_thinking,omitempty"` // Qwen / SiliconFlow style deep thinking
}

type openAIMessageBody struct {
	Role             string `json:"role,omitempty"`
	Content          string `json:"content,omitempty"`
	ReasoningContent string `json:"reasoning_content,omitempty"`
	Reasoning        string `json:"reasoning,omitempty"` // some gateways use shorthand
}

type openAIResponse struct {
	Choices []struct {
		Message      openAIMessageBody `json:"message"`
		Delta        openAIMessageBody `json:"delta"`
		FinishReason string            `json:"finish_reason"`
	} `json:"choices"`
	Usage Usage `json:"usage"`
}

func reasoningFromBody(b openAIMessageBody) string {
	if b.ReasoningContent != "" {
		return b.ReasoningContent
	}
	return b.Reasoning
}

func (c *openAIClient) buildAPIRequest(target model.ModelTarget, req ChatRequest, stream bool) openAIRequest {
	apiReq := openAIRequest{
		Model:       target.Model,
		Messages:    req.Messages,
		Temperature: req.Temperature,
		TopP:        req.TopP,
		MaxTokens:   req.MaxTokens,
		Stream:      stream,
	}
	if req.Thinking {
		t := true
		apiReq.EnableThinking = &t
	}
	return apiReq
}

func (c *openAIClient) call(ctx context.Context, target model.ModelTarget, req ChatRequest) (*ChatResponse, error) {
	apiReq := c.buildAPIRequest(target, req, false)

	body, _ := json.Marshal(apiReq)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, target.URL, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if target.APIKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+target.APIKey)
	}

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("http call: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("LLM returned %d: %s", resp.StatusCode, string(respBody))
	}

	var apiResp openAIResponse
	if err := json.NewDecoder(resp.Body).Decode(&apiResp); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}

	if len(apiResp.Choices) == 0 {
		return nil, fmt.Errorf("empty choices in LLM response")
	}

	msg := apiResp.Choices[0].Message
	return &ChatResponse{
		Content:          msg.Content,
		ReasoningContent: reasoningFromBody(msg),
		FinishReason:     apiResp.Choices[0].FinishReason,
		Usage:            apiResp.Usage,
	}, nil
}

func (c *openAIClient) callStream(ctx context.Context, target model.ModelTarget, req ChatRequest) (<-chan StreamDelta, error) {
	apiReq := c.buildAPIRequest(target, req, true)

	body, _ := json.Marshal(apiReq)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, target.URL, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "text/event-stream")
	if target.APIKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+target.APIKey)
	}

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("http call: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return nil, fmt.Errorf("LLM returned %d: %s", resp.StatusCode, string(respBody))
	}

	// Reader + consumer so we can exit promptly when ctx is canceled (stop button / client disconnect).
	type rawLine struct {
		s   string
		err error
	}
	lineCh := make(chan rawLine, 128)
	go func() {
		defer close(lineCh)
		scanner := bufio.NewScanner(resp.Body)
		for scanner.Scan() {
			select {
			case lineCh <- rawLine{s: scanner.Text()}:
			case <-ctx.Done():
				return
			}
		}
		if err := scanner.Err(); err != nil {
			select {
			case lineCh <- rawLine{err: err}:
			case <-ctx.Done():
			}
		}
	}()

	ch := make(chan StreamDelta, 64)
	go func() {
		defer close(ch)
		var once sync.Once
		closeBody := func() { once.Do(func() { resp.Body.Close() }) }
		defer closeBody()
		for {
			select {
			case <-ctx.Done():
				closeBody()
				return
			case item, ok := <-lineCh:
				if !ok {
					return
				}
				if item.err != nil {
					ch <- StreamDelta{Err: item.err}
					return
				}
				line := item.s
				if !strings.HasPrefix(line, "data: ") {
					continue
				}
				data := strings.TrimPrefix(line, "data: ")
				if data == "[DONE]" {
					return
				}

				var chunk openAIResponse
				if err := json.Unmarshal([]byte(data), &chunk); err != nil {
					ch <- StreamDelta{Err: err}
					return
				}
				if len(chunk.Choices) > 0 {
					choice := chunk.Choices[0]
					d := choice.Delta
					reason := reasoningFromBody(d)
					if reason != "" {
						ch <- StreamDelta{
							Content:      reason,
							FinishReason: choice.FinishReason,
							IsThinking:   true,
						}
					}
					if d.Content != "" {
						ch <- StreamDelta{
							Content:      d.Content,
							FinishReason: choice.FinishReason,
						}
					}
				}
			}
		}
	}()

	return ch, nil
}

// --- Ollama client ---

type ollamaRequest struct {
	Model    string         `json:"model"`
	Messages []Message      `json:"messages"`
	Stream   bool           `json:"stream"`
	Options  *ollamaOptions `json:"options,omitempty"`
}

type ollamaOptions struct {
	Temperature float64 `json:"temperature,omitempty"`
	TopP        float64 `json:"top_p,omitempty"`
}

type ollamaResponse struct {
	Message Message `json:"message"`
	Done    bool    `json:"done"`
}

type ollamaClient struct {
	httpClient *http.Client
}

func newOllamaClient() *ollamaClient {
	return &ollamaClient{
		httpClient: &http.Client{Timeout: 120 * time.Second},
	}
}

func (c *ollamaClient) call(ctx context.Context, target model.ModelTarget, req ChatRequest) (*ChatResponse, error) {
	apiReq := ollamaRequest{
		Model:    target.Model,
		Messages: req.Messages,
		Stream:   false,
	}
	if req.Temperature > 0 {
		apiReq.Options = &ollamaOptions{Temperature: req.Temperature, TopP: req.TopP}
	} else if req.TopP > 0 {
		apiReq.Options = &ollamaOptions{TopP: req.TopP}
	}

	body, _ := json.Marshal(apiReq)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, target.URL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("ollama returned %d: %s", resp.StatusCode, string(respBody))
	}

	var apiResp ollamaResponse
	if err := json.NewDecoder(resp.Body).Decode(&apiResp); err != nil {
		return nil, err
	}

	return &ChatResponse{
		Content: apiResp.Message.Content,
	}, nil
}

func (c *ollamaClient) callStream(ctx context.Context, target model.ModelTarget, req ChatRequest) (<-chan StreamDelta, error) {
	apiReq := ollamaRequest{
		Model:    target.Model,
		Messages: req.Messages,
		Stream:   true,
	}
	if req.Temperature > 0 {
		apiReq.Options = &ollamaOptions{Temperature: req.Temperature, TopP: req.TopP}
	} else if req.TopP > 0 {
		apiReq.Options = &ollamaOptions{TopP: req.TopP}
	}

	body, _ := json.Marshal(apiReq)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, target.URL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return nil, err
	}

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return nil, fmt.Errorf("ollama returned %d: %s", resp.StatusCode, string(respBody))
	}

	type ollChunk struct {
		content string
		done    bool
		err     error
	}
	chunkCh := make(chan ollChunk, 64)
	go func() {
		defer close(chunkCh)
		decoder := json.NewDecoder(resp.Body)
		for {
			var chunk ollamaResponse
			if err := decoder.Decode(&chunk); err != nil {
				if err != io.EOF {
					select {
					case chunkCh <- ollChunk{err: err}:
					case <-ctx.Done():
					}
				}
				return
			}
			select {
			case chunkCh <- ollChunk{content: chunk.Message.Content, done: chunk.Done}:
			case <-ctx.Done():
				return
			}
			if chunk.Done {
				return
			}
		}
	}()

	ch := make(chan StreamDelta, 64)
	go func() {
		defer close(ch)
		var once sync.Once
		closeBody := func() { once.Do(func() { resp.Body.Close() }) }
		defer closeBody()
		for {
			select {
			case <-ctx.Done():
				closeBody()
				return
			case c, ok := <-chunkCh:
				if !ok {
					return
				}
				if c.err != nil {
					ch <- StreamDelta{Err: c.err}
					return
				}
				if c.content != "" {
					ch <- StreamDelta{Content: c.content}
				}
				if c.done {
					return
				}
			}
		}
	}()

	return ch, nil
}
