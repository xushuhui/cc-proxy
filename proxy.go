package main

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/klauspost/compress/zstd"
)

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
		config: config,
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

		// Success
		log.Printf("[成功 #%d] %s - %s - HTTP %d", attemptCount, state.backend.Name, targetURL, resp.StatusCode)

		// Convert OpenAI response to Claude format if needed
		apiType := state.backend.APIType
		if apiType == "" {
			apiType = "claude"
		}

		if apiType == "openai" {
			ps.copyAndConvertResponse(w, resp, state.backend)
		} else {
			ps.copyResponse(w, resp)
		}
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

	targetURL.Path = originalReq.URL.Path
	targetURL.RawQuery = originalReq.URL.RawQuery

	// Determine API type (default to claude if not specified)
	apiType := backend.APIType
	if apiType == "" {
		apiType = "claude"
	}

	// Convert request format if backend is OpenAI
	if apiType == "openai" && len(bodyBytes) > 0 {
		convertedBody, err := convertClaudeToOpenAI(bodyBytes)
		if err != nil {
			log.Printf("[转换失败] %s - Claude->OpenAI 请求转换失败: %v", backend.Name, err)
			ps.circuitBreaker.RecordFailure(state, 0)
			return nil, true, err
		}
		bodyBytes = convertedBody
		log.Printf("[格式转换] %s - Claude 请求已转换为 OpenAI 格式", backend.Name)
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
	} else {
		log.Printf("[超时设置] %s - 流式请求,不设置超时限制", backend.Name)
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

	_, err := io.Copy(w, resp.Body)
	if err != nil {
		log.Printf("[复制响应] 写入失败: %v", err)
	}
}

// copyAndConvertResponse copies and converts OpenAI response to Claude format
func (ps *ProxyServer) copyAndConvertResponse(w http.ResponseWriter, resp *http.Response, backend Backend) {
	// Don't defer close here, we'll close after reading

	// Check if this is a streaming response
	isStreaming := strings.Contains(resp.Header.Get("Content-Type"), "text/event-stream")

	if isStreaming {
		defer resp.Body.Close()
		// Handle streaming response
		log.Printf("[流式转换] %s - 开始转换 OpenAI 流式响应为 Claude 格式", backend.Name)

		// Copy headers
		for key, values := range resp.Header {
			for _, value := range values {
				w.Header().Add(key, value)
			}
		}
		w.WriteHeader(resp.StatusCode)

		// Convert SSE chunks
		ps.convertStreamingResponse(w, resp.Body, backend)
	} else {
		// Handle non-streaming response - read body with proper gzip handling
		log.Printf("[响应头] %s - Content-Encoding: %s, Content-Type: %s",
			backend.Name,
			resp.Header.Get("Content-Encoding"),
			resp.Header.Get("Content-Type"))

		bodyBytes, err := readResponseBody(resp)
		resp.Body.Close() // Close after reading

		if err != nil {
			log.Printf("[转换失败] %s - 读取 OpenAI 响应失败: %v", backend.Name, err)
			http.Error(w, "Failed to read response", http.StatusInternalServerError)
			return
		}

		// Log the raw response for debugging (already decompressed by readResponseBody)
		bodyPreview := string(bodyBytes)
		if len(bodyPreview) > 1000 {
			bodyPreview = bodyPreview[:1000] + "..."
		}

		// Check if content is printable
		isPrintable := true
		for i := 0; i < len(bodyPreview) && i < 100; i++ {
			if bodyPreview[i] < 32 && bodyPreview[i] != '\n' && bodyPreview[i] != '\r' && bodyPreview[i] != '\t' {
				isPrintable = false
				break
			}
		}

		if isPrintable {
			log.Printf("[OpenAI 原始响应] %s - %s", backend.Name, bodyPreview)
		} else {
			log.Printf("[OpenAI 原始响应] %s - [二进制数据,长度: %d 字节]", backend.Name, len(bodyBytes))
		}

		convertedBody, err := convertOpenAIToClaude(bodyBytes)
		if err != nil {
			log.Printf("[转换失败] %s - OpenAI->Claude 响应转换失败: %v", backend.Name, err)
			if isPrintable {
				log.Printf("[响应内容] %s", bodyPreview)
			}
			http.Error(w, "Failed to convert response", http.StatusInternalServerError)
			return
		}

		// Check if conversion actually happened (convertOpenAIToClaude returns original if already Claude format)
		if bytes.Equal(convertedBody, bodyBytes) {
			log.Printf("[格式检测] %s - 响应已经是 Claude 格式,直接透传", backend.Name)
		} else {
			log.Printf("[格式转换] %s - OpenAI 响应已转换为 Claude 格式", backend.Name)
		}

		// Copy headers (except Content-Length which will change)
		for key, values := range resp.Header {
			if key != "Content-Length" {
				for _, value := range values {
					w.Header().Add(key, value)
				}
			}
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(resp.StatusCode)

		_, err = w.Write(convertedBody)
		if err != nil {
			log.Printf("[写入失败] %s - 写入响应失败: %v", backend.Name, err)
		}
	}
}

// convertStreamingResponse converts OpenAI SSE stream to Claude SSE stream
func (ps *ProxyServer) convertStreamingResponse(w http.ResponseWriter, body io.Reader, backend Backend) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		log.Printf("[流式转换] %s - ResponseWriter 不支持 Flusher", backend.Name)
		return
	}

	scanner := bufio.NewScanner(body)
	lineCount := 0
	dataCount := 0
	var fullText strings.Builder // 累积完整文本

	log.Printf("[流式开始] %s - 开始接收流式响应", backend.Name)

	for scanner.Scan() {
		line := scanner.Text()
		lineCount++

		// Skip empty lines and comments
		if line == "" || strings.HasPrefix(line, ":") {
			fmt.Fprintf(w, "%s\n", line)
			flusher.Flush()
			continue
		}

		// Parse SSE event line
		if strings.HasPrefix(line, "event: ") {
			// This is a Claude-format event, pass through directly
			eventType := strings.TrimPrefix(line, "event: ")
			log.Printf("[流式事件] %s - %s", backend.Name, eventType)
			fmt.Fprintf(w, "%s\n", line)
			flusher.Flush()
			continue
		}

		// Parse SSE data line
		if strings.HasPrefix(line, "data: ") {
			data := strings.TrimPrefix(line, "data: ")
			dataCount++

			// Handle [DONE] marker (OpenAI format)
			if data == "[DONE]" {
				log.Printf("[流式结束] %s - 收到 [DONE] 标记", backend.Name)
				log.Printf("[完整内容] %s - %s", backend.Name, fullText.String())
				fmt.Fprintf(w, "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
				flusher.Flush()
				break
			}

			// Try to detect if this is already Claude format
			var dataMap map[string]any
			if err := json.Unmarshal([]byte(data), &dataMap); err == nil {
				// Check if it has Claude-specific fields
				if dataType, ok := dataMap["type"].(string); ok {
					// This looks like Claude format already, pass through

					// Extract and display text content
					textContent := ""
					if dataType == "content_block_delta" {
						if delta, ok := dataMap["delta"].(map[string]any); ok {
							if text, ok := delta["text"].(string); ok {
								textContent = text
								fullText.WriteString(text) // 累积文本
							}
						}
					}

					if textContent != "" {
						// 显示文本内容,用引号包裹以便看清空格
						displayText := textContent
						if len(displayText) > 200 {
							displayText = displayText[:200] + "..."
						}
						log.Printf("[流式内容 #%d] %s - \"%s\"", dataCount, backend.Name, displayText)
					} else {
						// 非文本内容,显示类型
						log.Printf("[流式事件 #%d] %s - type: %s", dataCount, backend.Name, dataType)
					}

					fmt.Fprintf(w, "data: %s\n\n", data)
					flusher.Flush()
					continue
				}
			}

			// Convert OpenAI chunk to Claude format
			log.Printf("[流式数据 #%d] %s - OpenAI 格式,尝试转换", dataCount, backend.Name)
			convertedData, err := convertOpenAIStreamToClaude([]byte(data))
			if err != nil {
				log.Printf("[流式转换] %s - 转换失败: %v, 原始数据: %s", backend.Name, err, data)
				continue
			}

			log.Printf("[流式数据 #%d] %s - 转换成功,写入客户端", dataCount, backend.Name)
			fmt.Fprintf(w, "data: %s\n\n", convertedData)
			flusher.Flush()
		}
	}

	if err := scanner.Err(); err != nil {
		log.Printf("[流式错误] %s - 读取流失败: %v (共处理 %d 行, %d 个数据块)", backend.Name, err, lineCount, dataCount)
		if fullText.Len() > 0 {
			log.Printf("[部分内容] %s - %s", backend.Name, fullText.String())
		}
	} else {
		log.Printf("[流式完成] %s - 处理完成 (共 %d 行, %d 个数据块, 总字符数: %d)", backend.Name, lineCount, dataCount, fullText.Len())
		if fullText.Len() > 0 {
			log.Printf("[完整内容] %s - %s", backend.Name, fullText.String())
		}
	}
}
