package main

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"
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
			Timeout: time.Duration(config.Retry.Timeout) * time.Second,
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

	targetURL.Path = originalReq.URL.Path
	targetURL.RawQuery = originalReq.URL.RawQuery

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

// readResponseBody reads response body, automatically handles gzip compression
func readResponseBody(resp *http.Response) ([]byte, error) {
	var reader io.Reader = resp.Body

	if strings.EqualFold(resp.Header.Get("Content-Encoding"), "gzip") {
		gzipReader, err := gzip.NewReader(resp.Body)
		if err != nil {
			return nil, fmt.Errorf("创建 gzip reader 失败: %v", err)
		}
		defer gzipReader.Close()
		reader = gzipReader
	}

	return io.ReadAll(reader)
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

	io.Copy(w, resp.Body)
}
