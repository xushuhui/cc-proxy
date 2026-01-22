package main

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/klauspost/compress/zstd"
)

// OpenAI related types
type openaiChatCompletionRequest struct {
	Model       string `json:"model"`
	Messages    []any  `json:"messages"`
	MaxTokens   int    `json:"max_tokens,omitempty"`
	Temperature any    `json:"temperature,omitempty"`
	Stream      bool   `json:"stream,omitempty"`
	Tools       []any  `json:"tools,omitempty"`
	ToolChoice  any    `json:"tool_choice,omitempty"`
}

type openaiChatCompletionResponse struct {
	ID      string `json:"id"`
	Model   string `json:"model"`
	Choices []struct {
		Message struct {
			Role      string  `json:"role"`
			Content   *string `json:"content"`
			ToolCalls []struct {
				ID       string `json:"id"`
				Type     string `json:"type"`
				Function struct {
					Name      string `json:"name"`
					Arguments any    `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls,omitempty"`
		} `json:"message"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage *struct {
		PromptTokens        int `json:"prompt_tokens"`
		CompletionTokens    int `json:"completion_tokens"`
		PromptTokensDetails *struct {
			CachedTokens int `json:"cached_tokens"`
		} `json:"prompt_tokens_details,omitempty"`
	} `json:"usage,omitempty"`
}

type anthropicMessageRequest struct {
	Model       string          `json:"model"`
	MaxTokens   int             `json:"max_tokens"`
	Temperature *float64        `json:"temperature,omitempty"`
	Stream      bool            `json:"stream,omitempty"`
	System      json.RawMessage `json:"system,omitempty"`
	Messages    []anthropicMsg  `json:"messages"`
	Tools       []anthropicTool `json:"tools,omitempty"`
	ToolChoice  any             `json:"tool_choice,omitempty"`
}

type anthropicMsg struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

type anthropicTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"input_schema,omitempty"`
}

type anthropicContentBlock struct {
	Type string `json:"type"`

	// text
	Text string `json:"text,omitempty"`

	// image
	Source *anthropicImageSource `json:"source,omitempty"`

	// tool_use
	ID    string          `json:"id,omitempty"`
	Name  string          `json:"name,omitempty"`
	Input json.RawMessage `json:"input,omitempty"`

	// tool_result
	ToolUseID string          `json:"tool_use_id,omitempty"`
	Content   json.RawMessage `json:"content,omitempty"`
}

type anthropicImageSource struct {
	Type      string `json:"type"`
	MediaType string `json:"media_type"`
	Data      string `json:"data,omitempty"`
	URL       string `json:"url,omitempty"`
}

// ProxyServer is the proxy server
type ProxyServer struct {
	config         *Config
	client         *http.Client
	circuitBreaker *CircuitBreaker
}

// NewProxyServer creates proxy server instance
func NewProxyServer(configPath string) (*ProxyServer, error) {
	config, err := loadConfig(configPath)
	if err != nil {
		return nil, err
	}

	server := &ProxyServer{
		config:         config,
		client: &http.Client{
			// Don't set Timeout here - it would kill streaming responses
			// We'll use context with timeout for non-streaming requests only
			Timeout: 0,
		},
		circuitBreaker: NewCircuitBreaker(config),
	}

	return server, nil
}

// ServeHTTP handles HTTP requests
func (ps *ProxyServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	bodyBytes, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "Failed to read request body", http.StatusBadRequest)
		return
	}
	r.Body.Close()

	backendCount := len(ps.config.Backends)
	log.Printf("[请求开始] %s %s - 配置了 %d 个后端", r.Method, r.URL.Path, backendCount)

	var lastErr error
	attemptCount := 0
	skippedCount := 0

	// Get backends sorted by priority (non-rate-limited first)
	sortedStates := ps.circuitBreaker.SortBackendsByPriority()

	for _, state := range sortedStates {
		// Check if backend should be skipped
		if skip, reason := ps.circuitBreaker.ShouldSkipBackend(state); skip {
			skippedCount++
			log.Printf("[跳过] %s - %s", state.backend.Name, reason)
			continue
		}

		attemptCount++

		targetURL := state.backend.BaseURL + r.URL.Path
		if r.URL.RawQuery != "" {
			targetURL += "?" + r.URL.RawQuery
		}

		tokenPreview := state.backend.Token
		if len(tokenPreview) > 12 {
			tokenPreview = tokenPreview[:4] + "..." + tokenPreview[len(tokenPreview)-4:]
		}

		// Check if this is a half-open test request
		isHalfOpen := ps.circuitBreaker.IsHalfOpen(state)
		if isHalfOpen {
			ps.circuitBreaker.IncrementHalfOpenTries(state)
			log.Printf("[尝试 #%d] %s - %s %s (token: %s) [熔断测试 %d/%d]",
				attemptCount, state.backend.Name, r.Method, targetURL, tokenPreview,
				state.halfOpenTries, ps.config.Failover.CircuitBreaker.HalfOpenRequests)
		} else {
			log.Printf("[尝试 #%d] %s - %s %s (token: %s)", attemptCount, state.backend.Name, r.Method, targetURL, tokenPreview)
		}

		resp, shouldRetry, err := ps.forwardRequest(state, r, bodyBytes)
		if err != nil {
			lastErr = err
			log.Printf("[失败 #%d] %s - %s - %v", attemptCount, state.backend.Name, targetURL, err)
			continue
		}

		// Check if we should retry with next backend
		if shouldRetry {
			lastErr = fmt.Errorf("HTTP %d", resp.StatusCode)
			resp.Body.Close()
			continue
		}

		// Response will be returned to client (2xx success or 4xx client error)
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			log.Printf("[成功 #%d] %s - %s - HTTP %d", attemptCount, state.backend.Name, targetURL, resp.StatusCode)
		} else {
			log.Printf("[返回客户端] %d - %s - HTTP %s - %s (客户端错误,不重试)", attemptCount, state.backend.Name, targetURL, resp.Status)
		}

		ps.copyResponse(w, resp)
		return
	}

	log.Printf("[全部失败] 所有后端不可用 (尝试 %d 个,跳过 %d 个)", attemptCount, skippedCount)
	errMsg := "所有 API 后端不可用"
	if lastErr != nil {
		errMsg = fmt.Sprintf("%s: %v", errMsg, lastErr)
	}
	http.Error(w, errMsg, http.StatusBadGateway)
}

// forwardRequest forwards request to specified backend
// Returns: (response, shouldRetry, error)
// - shouldRetry=true: should try next backend (5xx, 429, timeout)
// - shouldRetry=false: return response to client (2xx, 3xx, 4xx except 429)
func (ps *ProxyServer) forwardRequest(state *BackendState, originalReq *http.Request, bodyBytes []byte) (*http.Response, bool, error) {
	backend := state.backend
	targetURL, err := url.Parse(backend.BaseURL)
	if err != nil {
		ps.circuitBreaker.RecordFailure(state, 0)
		return nil, true, err
	}

	// Determine platform type
	platform := backend.Platform
	if platform == "" {
		platform = "anthropic" // Default to anthropic
	}

	// Build target URL path - append client path to base URL path
	targetURL.Path = targetURL.Path + originalReq.URL.Path
	targetURL.RawQuery = originalReq.URL.RawQuery

	// For OpenAI backends, we need to handle path forwarding specially
	// Client requests /v1/messages but OpenAI expects /v1/chat/completions
	if platform == "openai" {
		// If the target URL doesn't already end with the correct OpenAI endpoint,
		// and the request is for /v1/messages, replace it with /v1/chat/completions
		if strings.HasSuffix(originalReq.URL.Path, "/v1/messages") {
			// Replace the path with OpenAI's chat completions endpoint
			targetURL.Path = strings.TrimSuffix(targetURL.Path, "/v1/messages") + "/v1/chat/completions"
			log.Printf("[路径转发] %s - /v1/messages → /v1/chat/completions", backend.Name)
		}
	}

	// Apply model override if specified
	if backend.Model != "" && len(bodyBytes) > 0 {
		var bodyMap map[string]any
		if err := json.Unmarshal(bodyBytes, &bodyMap); err == nil {
			bodyMap["model"] = backend.Model
			if modifiedBody, err := json.Marshal(bodyMap); err == nil {
				bodyBytes = modifiedBody
				log.Printf("[模型覆盖] %s - 使用配置的模型: %s", backend.Name, backend.Model)
			}
		}
	}

	// Convert request format if needed
	if platform == "openai" {
		convertedBody, err := ps.convertAnthropicToOpenAI(bodyBytes)
		if err != nil {
			log.Printf("[格式转换失败] %s - %v", backend.Name, err)
			// Continue with original body if conversion fails
		} else {
			bodyBytes = convertedBody
			log.Printf("[格式转换] %s - Anthropic 格式已转换为 OpenAI 格式", backend.Name)
		}
	}

	req, err := http.NewRequest(originalReq.Method, targetURL.String(), bytes.NewReader(bodyBytes))
	if err != nil {
		ps.circuitBreaker.RecordFailure(state, 0)
		return nil, true, err
	}

	// Check if this is a streaming request
	isStreamingRequest := false
	if len(bodyBytes) > 0 {
		var bodyMap map[string]any
		if json.Unmarshal(bodyBytes, &bodyMap) == nil {
			if stream, ok := bodyMap["stream"].(bool); ok && stream {
				isStreamingRequest = true
			}
		}
	}

	// Add timeout context only for non-streaming requests
	if !isStreamingRequest {
		ctx, cancel := context.WithTimeout(originalReq.Context(), time.Duration(ps.config.Retry.Timeout)*time.Second)
		defer cancel()
		req = req.WithContext(ctx)
		log.Printf("[超时设置] %s - 非流式请求,设置 %d 秒超时", backend.Name, ps.config.Retry.Timeout)
	}

	for key, values := range originalReq.Header {
		for _, value := range values {
			req.Header.Add(key, value)
		}
	}

	req.Header.Set("Authorization", "Bearer "+backend.Token)

	resp, err := ps.client.Do(req)
	if err != nil {
		// Network error or timeout
		ps.circuitBreaker.RecordFailure(state, 0)
		// Check if it's a timeout error
		if strings.Contains(err.Error(), "timeout") || strings.Contains(err.Error(), "deadline exceeded") {
			log.Printf("[超时] %s - 请求超时 (%d 秒)", backend.Name, ps.config.Retry.Timeout)
		}
		return nil, true, err
	}

	// Handle non-2xx responses
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		bodyBytes, readErr := readResponseBody(resp)
		resp.Body.Close()

		if readErr != nil {
			ps.circuitBreaker.RecordFailure(state, resp.StatusCode)
			return nil, true, fmt.Errorf("后端返回错误: HTTP %d (读取响应体失败: %v)", resp.StatusCode, readErr)
		}

		bodyStr := string(bodyBytes)
		if len(bodyStr) > 500 {
			bodyStr = bodyStr[:500] + "..."
		}

		// Log error response for debugging
		log.Printf("[错误详情] %s - HTTP %d - 响应: %s", backend.Name, resp.StatusCode, bodyStr)

		// Classify errors
		switch {
		case resp.StatusCode == 429:
			// Rate limit - record and retry
			retryAfter := resp.Header.Get("Retry-After")
			ps.circuitBreaker.Record429(state, retryAfter)
			return nil, true, fmt.Errorf("后端返回错误: HTTP %d", resp.StatusCode)

		case resp.StatusCode >= 500:
			// Server error - record failure and retry
			ps.circuitBreaker.RecordFailure(state, resp.StatusCode)
			return nil, true, fmt.Errorf("后端返回错误: HTTP %d", resp.StatusCode)

		case resp.StatusCode == 401 || resp.StatusCode == 403:
			// Auth error - don't retry, return immediately
			log.Printf("[认证错误] %s - HTTP %d,不重试", backend.Name, resp.StatusCode)
			resp.Body = io.NopCloser(bytes.NewReader(bodyBytes))
			return resp, false, nil

		default:
			// Other 3xx/4xx errors - don't retry, return immediately
			resp.Body = io.NopCloser(bytes.NewReader(bodyBytes))
			return resp, false, nil
		}
	}

	// Success - record and return
	ps.circuitBreaker.RecordSuccess(state)

	// Convert response format if needed
	if platform == "openai" {
		return ps.convertOpenAIResponse(resp)
	}

	return resp, false, nil
}

// readResponseBody reads response body, automatically handles gzip and zstd compression
func readResponseBody(resp *http.Response) ([]byte, error) {
	var reader io.Reader = resp.Body
	contentEncoding := resp.Header.Get("Content-Encoding")

	log.Printf("[readResponseBody] Content-Encoding 头: '%s'", contentEncoding)

	// Handle gzip compression
	if strings.EqualFold(contentEncoding, "gzip") {
		log.Printf("[readResponseBody] 检测到 gzip 压缩,尝试解压")
		gzipReader, err := gzip.NewReader(resp.Body)
		if err != nil {
			log.Printf("[readResponseBody] gzip 解压失败: %v", err)
			return io.ReadAll(resp.Body)
		}
		defer gzipReader.Close()
		reader = gzipReader
		log.Printf("[readResponseBody] gzip 解压器创建成功")
	}

	// Handle zstd compression
	if strings.EqualFold(contentEncoding, "zstd") {
		log.Printf("[readResponseBody] 检测到 zstd 压缩,尝试解压")
		zstdReader, err := zstd.NewReader(resp.Body)
		if err != nil {
			log.Printf("[readResponseBody] zstd 解压失败: %v", err)
			return io.ReadAll(resp.Body)
		}
		defer zstdReader.Close()
		reader = zstdReader
		log.Printf("[readResponseBody] zstd 解压器创建成功")
	}

	data, err := io.ReadAll(reader)
	if err != nil {
		// Check if we got any data before the error
		if len(data) > 0 {
			log.Printf("[readResponseBody] 读取时出错 %v,但已读取 %d 字节数据", err, len(data))
			return data, nil // Return partial data instead of error
		}
		return nil, err
	}

	log.Printf("[readResponseBody] 读取了 %d 字节数据", len(data))

	// Check if data looks like gzip even without header (magic bytes: 1f 8b)
	if len(data) > 2 && data[0] == 0x1f && data[1] == 0x8b && contentEncoding == "" {
		log.Printf("[readResponseBody] 检测到未声明的 gzip 数据,尝试解压")
		gzipReader, err := gzip.NewReader(bytes.NewReader(data))
		if err != nil {
			log.Printf("[readResponseBody] 未声明的 gzip 解压失败: %v", err)
			return data, nil
		}
		defer gzipReader.Close()
		decompressed, err := io.ReadAll(gzipReader)
		if err != nil {
			log.Printf("[readResponseBody] 读取解压数据失败: %v", err)
			return data, nil
		}
		log.Printf("[readResponseBody] 成功解压未声明的 gzip 数据: %d -> %d 字节", len(data), len(decompressed))
		return decompressed, nil
	}

	// Check if data looks like zstd even without header (magic bytes: 28 b5 2f fd)
	if len(data) > 4 && data[0] == 0x28 && data[1] == 0xb5 && data[2] == 0x2f && data[3] == 0xfd && contentEncoding == "" {
		log.Printf("[readResponseBody] 检测到未声明的 zstd 数据,尝试解压")
		zstdReader, err := zstd.NewReader(bytes.NewReader(data))
		if err != nil {
			log.Printf("[readResponseBody] 未声明的 zstd 解压失败: %v", err)
			return data, nil
		}
		defer zstdReader.Close()
		decompressed, err := io.ReadAll(zstdReader)
		if err != nil {
			log.Printf("[readResponseBody] 读取解压数据失败: %v", err)
			return data, nil
		}
		log.Printf("[readResponseBody] 成功解压未声明的 zstd 数据: %d -> %d 字节", len(data), len(decompressed))
		return decompressed, nil
	}

	return data, nil
}

// copyResponse copies response to client
func (ps *ProxyServer) copyResponse(w http.ResponseWriter, resp *http.Response) {
	defer resp.Body.Close()

	for key, values := range resp.Header {
		for _, value := range values {
			w.Header().Add(key, value)
		}
	}

	w.WriteHeader(resp.StatusCode)

	// Check if this is a streaming response
	contentType := resp.Header.Get("Content-Type")
	if strings.Contains(contentType, "text/event-stream") {
		// For streaming responses, use chunked copying with flushing
		flusher, ok := w.(http.Flusher)
		if !ok {
			log.Printf("[复制响应] 警告: ResponseWriter 不支持 Flusher 接口")
			// Fallback to regular copy
			_, err := io.Copy(w, resp.Body)
			if err != nil {
				log.Printf("[复制响应] 写入失败: %v", err)
			}
			return
		}

		// Copy with periodic flushing for streaming
		buf := make([]byte, 8192) // 8KB buffer
		for {
			n, err := resp.Body.Read(buf)
			if n > 0 {
				_, writeErr := w.Write(buf[:n])
				if writeErr != nil {
					log.Printf("[复制响应] 流式写入失败: %v", writeErr)
					return
				}
				flusher.Flush() // Immediately flush to client
			}
			if err != nil {
				if err != io.EOF {
					log.Printf("[复制响应] 流式读取失败: %v", err)
				}
				break
			}
		}
	} else {
		// For non-streaming responses, use regular copy
		_, err := io.Copy(w, resp.Body)
		if err != nil {
			log.Printf("[复制响应] 写入失败: %v", err)
		}
	}
}

// convertAnthropicToOpenAI converts Anthropic request format to OpenAI format
func (ps *ProxyServer) convertAnthropicToOpenAI(bodyBytes []byte) ([]byte, error) {
	var anthropicReq anthropicMessageRequest
	if err := json.Unmarshal(bodyBytes, &anthropicReq); err != nil {
		return nil, fmt.Errorf("解析 Anthropic 请求失败: %v", err)
	}

	openaiReq := openaiChatCompletionRequest{
		Model:     anthropicReq.Model,
		MaxTokens: anthropicReq.MaxTokens,
		Stream:    anthropicReq.Stream,
	}

	// Convert temperature if present
	if anthropicReq.Temperature != nil {
		openaiReq.Temperature = *anthropicReq.Temperature
	}

	// Convert messages
	var messages []any

	// Add instructions to mimic Claude's behavior
	systemPrompt := extractSystemText(anthropicReq.System)
	if systemPrompt == "" {
		systemPrompt = "You are a helpful AI assistant."
	}

	// Enhance system prompt based on model capabilities
	enhancedSystemPrompt := ps.getEnhancedSystemPrompt(systemPrompt, anthropicReq.Model)

	messages = append(messages, map[string]any{
		"role":    "system",
		"content": enhancedSystemPrompt,
	})

	// Convert conversation messages
	for _, msg := range anthropicReq.Messages {
		converted := convertAnthropicMessage(msg)
		messages = append(messages, converted...)
	}

	openaiReq.Messages = messages

	// Convert tools if present
	if len(anthropicReq.Tools) > 0 {
		openaiReq.Tools = convertAnthropicTools(anthropicReq.Tools)
	}

	// Convert tool choice if present
	if anthropicReq.ToolChoice != nil {
		openaiReq.ToolChoice = convertToolChoice(anthropicReq.ToolChoice)
	}

	return json.Marshal(openaiReq)
}

// getEnhancedSystemPrompt returns a model-specific enhanced system prompt
func (ps *ProxyServer) getEnhancedSystemPrompt(basePrompt, model string) string {
	// Model-specific optimizations
	modelLower := strings.ToLower(model)

	var enhancement string

	// Different models have different capabilities
	if strings.Contains(modelLower, "gpt-4") {
		enhancement = `

IMPORTANT: When working with code, follow this structured approach:

1. **Analysis**: Briefly analyze the request and current state
2. **Plan**: Describe your approach to solve the problem
3. **Implementation**: Show the actual code with clear explanations
4. **Verification**: Explain how your solution addresses the requirements

For file modifications:
- Use markdown headers like "### File: path/to/file.go"
- Show before/after when helpful
- Explain the reasoning behind changes
- Be thorough but concise

Always explain your reasoning step-by-step as you work.`
	} else if strings.Contains(modelLower, "gpt-3.5") {
		enhancement = `

When working with code:
1. First analyze what's needed
2. Plan your approach
3. Show the implementation
4. Explain how it works

Use markdown headers for files and be concise.`
	} else {
		// Default enhancement for other models
		enhancement = `

Please be detailed and structured in your responses, especially for code-related tasks.`
	}

	return basePrompt + enhancement
}

// extractSystemText extracts text from system message
func extractSystemText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var blocks []anthropicContentBlock
	if err := json.Unmarshal(raw, &blocks); err == nil {
		return joinTextBlocks(blocks)
	}
	return ""
}

// joinTextBlocks joins text blocks into a single string
func joinTextBlocks(blocks []anthropicContentBlock) string {
	var b strings.Builder
	for _, blk := range blocks {
		if blk.Type == "text" && blk.Text != "" {
			if b.Len() > 0 {
				b.WriteString("\n")
			}
			b.WriteString(blk.Text)
		}
	}
	return b.String()
}

// convertAnthropicMessage converts a single Anthropic message
func convertAnthropicMessage(msg anthropicMsg) []any {
	role := strings.TrimSpace(msg.Role)
	if role == "" {
		return nil
	}

	// Try to parse content as string first
	var asString string
	if err := json.Unmarshal(msg.Content, &asString); err == nil {
		return []any{map[string]any{
			"role":    role,
			"content": asString,
		}}
	}

	// Parse as content blocks
	var blocks []anthropicContentBlock
	if err := json.Unmarshal(msg.Content, &blocks); err != nil {
		return nil
	}

	switch role {
	case "user":
		return convertUserBlocks(blocks)
	case "assistant":
		return convertAssistantBlocks(blocks)
	default:
		// Fallback for unknown roles
		text := joinTextBlocks(blocks)
		return []any{map[string]any{
			"role":    role,
			"content": text,
		}}
	}
}

// convertUserBlocks converts user message blocks
func convertUserBlocks(blocks []anthropicContentBlock) []any {
	var messages []any

	// Handle tool_result blocks as separate tool messages
	for _, blk := range blocks {
		if blk.Type == "tool_result" && strings.TrimSpace(blk.ToolUseID) != "" {
			contentStr := ""
			if len(blk.Content) > 0 {
				if bytes, err := json.Marshal(blk.Content); err == nil {
					contentStr = string(bytes)
				}
			}
			messages = append(messages, map[string]any{
				"role":         "tool",
				"tool_call_id": blk.ToolUseID,
				"content":      contentStr,
			})
		}
	}

	// Handle text/image blocks as user message
	var parts []any
	for _, blk := range blocks {
		switch blk.Type {
		case "text":
			if blk.Text != "" {
				parts = append(parts, map[string]any{"type": "text", "text": blk.Text})
			}
		case "image":
			if blk.Source != nil {
				url := ""
				switch blk.Source.Type {
				case "base64":
					if blk.Source.MediaType != "" && blk.Source.Data != "" {
						url = "data:" + blk.Source.MediaType + ";base64," + blk.Source.Data
					}
				case "url":
					url = blk.Source.URL
				}
				if url != "" {
					parts = append(parts, map[string]any{
						"type":      "image_url",
						"image_url": map[string]any{"url": url},
					})
				}
			}
		}
	}

	if len(parts) > 0 {
		if len(parts) == 1 {
			if p, ok := parts[0].(map[string]any); ok && p["type"] == "text" {
				messages = append(messages, map[string]any{"role": "user", "content": p["text"]})
				return messages
			}
		}
		messages = append(messages, map[string]any{"role": "user", "content": parts})
	}

	return messages
}

// convertAssistantBlocks converts assistant message blocks
func convertAssistantBlocks(blocks []anthropicContentBlock) []any {
	text := joinTextBlocks(blocks)

	var toolCalls []any
	for _, blk := range blocks {
		if blk.Type == "tool_use" && strings.TrimSpace(blk.ID) != "" && strings.TrimSpace(blk.Name) != "" {
			args := "{}"
			if len(blk.Input) > 0 {
				args = string(blk.Input)
			}
			toolCalls = append(toolCalls, map[string]any{
				"id":       blk.ID,
				"type":     "function",
				"function": map[string]any{"name": blk.Name, "arguments": args},
			})
		}
	}

	msg := map[string]any{"role": "assistant"}
	if text != "" {
		msg["content"] = text
	}
	if len(toolCalls) > 0 {
		msg["tool_calls"] = toolCalls
	}

	return []any{msg}
}

// convertAnthropicTools converts Anthropic tools to OpenAI format
func convertAnthropicTools(tools []anthropicTool) []any {
	var openaiTools []any
	for _, tool := range tools {
		openaiTool := map[string]any{
			"type": "function",
			"function": map[string]any{
				"name":        tool.Name,
				"description": tool.Description,
			},
		}
		if len(tool.InputSchema) > 0 {
			var params any
			json.Unmarshal(tool.InputSchema, &params)
			openaiTool["function"].(map[string]any)["parameters"] = params
		}
		openaiTools = append(openaiTools, openaiTool)
	}
	return openaiTools
}

// convertToolChoice converts Anthropic tool choice to OpenAI format
func convertToolChoice(v any) any {
	m, ok := v.(map[string]any)
	if !ok {
		return v
	}
	typeVal, _ := m["type"].(string)
	switch typeVal {
	case "auto", "none", "required":
		return typeVal
	case "tool":
		name, _ := m["name"].(string)
		if name == "" {
			return "auto"
		}
		return map[string]any{
			"type":     "function",
			"function": map[string]any{"name": name},
		}
	default:
		return v
	}
}

// convertOpenAIResponse converts OpenAI response to Anthropic format
func (ps *ProxyServer) convertOpenAIResponse(resp *http.Response) (*http.Response, bool, error) {
	// Check if this is a streaming response
	contentType := resp.Header.Get("Content-Type")
	if strings.Contains(contentType, "text/event-stream") {
		return ps.convertOpenAIStreamResponse(resp)
	}

	// Handle non-streaming response
	bodyBytes, err := readResponseBody(resp)
	if err != nil {
		return resp, true, fmt.Errorf("读取响应体失败: %v", err)
	}

	var openaiResp openaiChatCompletionResponse
	if err := json.Unmarshal(bodyBytes, &openaiResp); err != nil {
		// Not a valid OpenAI response, return as-is
		resp.Body = io.NopCloser(bytes.NewReader(bodyBytes))
		return resp, false, nil
	}

	// Convert to Anthropic format
	anthropicResp := ps.convertOpenAIToAnthropic(openaiResp)
	convertedBody, err := json.Marshal(anthropicResp)
	if err != nil {
		return resp, true, fmt.Errorf("转换响应格式失败: %v", err)
	}

	// Create new response
	newResp := *resp
	newResp.Body = io.NopCloser(bytes.NewReader(convertedBody))
	newResp.Header.Set("Content-Type", "application/json")
	newResp.ContentLength = int64(len(convertedBody))

	log.Printf("[响应转换] OpenAI 格式已转换为 Anthropic 格式")
	return &newResp, false, nil
}

// convertOpenAIStreamResponse handles streaming response conversion
func (ps *ProxyServer) convertOpenAIStreamResponse(resp *http.Response) (*http.Response, bool, error) {
	log.Printf("[流式响应转换] 开始转换 OpenAI 流式响应为 Anthropic 格式")

	// Create a pipe to stream the converted response
	reader, writer := io.Pipe()
	newResp := *resp
	newResp.Body = reader
	newResp.Header.Set("Content-Type", "text/event-stream")
	newResp.Header.Set("Cache-Control", "no-cache")
	newResp.Header.Set("Connection", "keep-alive")
	newResp.Header.Set("X-Accel-Buffering", "no")
	newResp.ContentLength = -1 // Unknown length for streaming

	// Start conversion in a goroutine
	go func() {
		defer writer.Close()
		ps.streamOpenAIToAnthropic(resp.Body, writer)
	}()

	return &newResp, false, nil
}

// streamOpenAIToAnthropic converts OpenAI streaming format to Anthropic streaming format
func (ps *ProxyServer) streamOpenAIToAnthropic(upstreamBody io.ReadCloser, writer *io.PipeWriter) {
	defer upstreamBody.Close()

	// Create a buffered writer for flushing
	bufWriter := bufio.NewWriter(writer)
	defer bufWriter.Flush()

	// Minimal OpenAI SSE -> Anthropic SSE conversion (text deltas).
	encoder := func(event string, payload any) error {
		b, err := json.Marshal(payload)
		if err != nil {
			return err
		}
		if _, err := fmt.Fprintf(bufWriter, "event: %s\ndata: %s\n\n", event, string(b)); err != nil {
			return err
		}
		bufWriter.Flush()
		return nil
	}

	messageID := fmt.Sprintf("msg_%d", time.Now().UnixMilli())
	_ = encoder("message_start", map[string]any{
		"type": "message_start",
		"message": map[string]any{
			"id":            messageID,
			"type":          "message",
			"role":          "assistant",
			"model":         "gpt-4", // This should be extracted from the request
			"content":       []any{},
			"stop_reason":   nil,
			"stop_sequence": nil,
			"usage": map[string]any{
				"input_tokens":  0,
				"output_tokens": 0,
			},
		},
	})

	bufReader := bufio.NewReader(upstreamBody)
	chunkCount := 0
	textChars := 0
	var finishReason string
	sawDone := false
	nextContentBlockIndex := 0
	currentContentBlockIndex := -1
	currentBlockType := "" // "text" | "tool_use"
	hasTextBlock := false

	assignContentBlockIndex := func() int {
		idx := nextContentBlockIndex
		nextContentBlockIndex++
		return idx
	}

	closeCurrentBlock := func() {
		if currentContentBlockIndex >= 0 {
			_ = encoder("content_block_stop", map[string]any{
				"type":  "content_block_stop",
				"index": currentContentBlockIndex,
			})
			currentContentBlockIndex = -1
			currentBlockType = ""
		}
	}

	for {
		line, err := bufReader.ReadString('\n')
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			log.Printf("[流式转换错误] 读取上游响应失败: %v", err)
			return
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" || strings.HasPrefix(line, ":") {
			continue
		}
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "[DONE]" {
			sawDone = true
			break
		}

		var chunk struct {
			Choices []struct {
				Delta struct {
					Content *string `json:"content,omitempty"`
				} `json:"delta"`
				FinishReason *string `json:"finish_reason,omitempty"`
			} `json:"choices"`
		}
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			continue
		}
		if len(chunk.Choices) == 0 {
			continue
		}

		chunkCount++
		delta := chunk.Choices[0].Delta

		// Handle text content
		if delta.Content != nil && *delta.Content != "" {
			textChars += len([]rune(*delta.Content))
			// If we were in a tool block, close it before starting/continuing text.
			if currentBlockType != "" && currentBlockType != "text" {
				closeCurrentBlock()
			}
			if !hasTextBlock {
				hasTextBlock = true
				idx := assignContentBlockIndex()
				_ = encoder("content_block_start", map[string]any{
					"type":  "content_block_start",
					"index": idx,
					"content_block": map[string]any{
						"type": "text",
						"text": "",
					},
				})
				currentContentBlockIndex = idx
				currentBlockType = "text"
			}
			_ = encoder("content_block_delta", map[string]any{
				"type":  "content_block_delta",
				"index": currentContentBlockIndex,
				"delta": map[string]any{
					"type": "text_delta",
					"text": *delta.Content,
				},
			})
		}

		if chunk.Choices[0].FinishReason != nil {
			finishReason = *chunk.Choices[0].FinishReason
			stopReason := mapFinishReason(*chunk.Choices[0].FinishReason)
			_ = encoder("message_delta", map[string]any{
				"type": "message_delta",
				"delta": map[string]any{
					"stop_reason":   stopReason,
					"stop_sequence": nil,
				},
				"usage": map[string]any{
					"input_tokens":            0,
					"output_tokens":           0,
					"cache_read_input_tokens": 0,
				},
			})
		}
	}

	// Close any open content block (text or tool_use).
	closeCurrentBlock()

	// Ensure message_delta is always emitted before message_stop.
	if finishReason == "" {
		_ = encoder("message_delta", map[string]any{
			"type": "message_delta",
			"delta": map[string]any{
				"stop_reason":   "end_turn",
				"stop_sequence": nil,
			},
			"usage": map[string]any{
				"input_tokens":            0,
				"output_tokens":           0,
				"cache_read_input_tokens": 0,
			},
		})
	}

	_ = encoder("message_stop", map[string]any{
		"type": "message_stop",
	})

	log.Printf("[流式转换完成] chunks=%d text_chars=%d finish_reason=%q saw_done=%v", chunkCount, textChars, finishReason, sawDone)
}

// convertOpenAIToAnthropic converts OpenAI response to Anthropic format
func (ps *ProxyServer) convertOpenAIToAnthropic(resp openaiChatCompletionResponse) map[string]any {
	content := make([]any, 0, 4)

	var finishReason string
	if len(resp.Choices) > 0 {
		ch := resp.Choices[0]
		finishReason = ch.FinishReason

		// Convert text content
		if ch.Message.Content != nil && *ch.Message.Content != "" {
			content = append(content, map[string]any{
				"type": "text",
				"text": *ch.Message.Content,
			})
		}

		// Convert tool calls
		if len(ch.Message.ToolCalls) > 0 {
			for _, tc := range ch.Message.ToolCalls {
				input := map[string]any{}
				switch v := tc.Function.Arguments.(type) {
				case string:
					json.Unmarshal([]byte(v), &input)
				case map[string]any:
					input = v
				default:
					input = map[string]any{"value": fmt.Sprintf("%v", v)}
				}
				content = append(content, map[string]any{
					"type":  "tool_use",
					"id":    tc.ID,
					"name":  tc.Function.Name,
					"input": input,
				})
			}
		}
	}

	// Calculate token usage
	inputTokens := 0
	outputTokens := 0
	cacheRead := 0
	if resp.Usage != nil {
		if resp.Usage.PromptTokensDetails != nil {
			cacheRead = resp.Usage.PromptTokensDetails.CachedTokens
		}
		inputTokens = resp.Usage.PromptTokens - cacheRead
		outputTokens = resp.Usage.CompletionTokens
	}

	return map[string]any{
		"id":            resp.ID,
		"type":          "message",
		"role":          "assistant",
		"model":         resp.Model,
		"content":       content,
		"stop_reason":   mapFinishReason(finishReason),
		"stop_sequence": nil,
		"usage": map[string]any{
			"input_tokens":            inputTokens,
			"output_tokens":           outputTokens,
			"cache_read_input_tokens": cacheRead,
		},
	}
}

// mapFinishReason maps OpenAI finish reason to Anthropic format
func mapFinishReason(finish string) string {
	switch finish {
	case "stop":
		return "end_turn"
	case "length":
		return "max_tokens"
	case "tool_calls":
		return "tool_use"
	case "content_filter":
		return "stop_sequence"
	default:
		if finish == "" {
			return "end_turn"
		}
		return "end_turn"
	}
}
