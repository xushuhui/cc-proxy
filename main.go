package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Backend 代表一个 API 后端配置
type Backend struct {
	Name    string `json:"name"`
	BaseURL string `json:"base_url"`
	Enabled bool   `json:"enabled"`
	Token   string `json:"token"`
}

// Config 代表配置文件结构
type Config struct {
	Port     int       `json:"port"`
	Backends []Backend `json:"backends"`
	Retry    struct {
		MaxAttempts int `json:"max_attempts"`
		Timeout     int `json:"timeout_seconds"`
	} `json:"retry"`
	HealthCheck struct {
		Enabled  bool `json:"enabled"`
		Interval int  `json:"interval_seconds"`
	} `json:"health_check"`
}

// ProxyServer 代理服务器
type ProxyServer struct {
	config      *Config
	client      *http.Client
	healthMutex sync.RWMutex
	healthMap   map[string]bool
}

// NewProxyServer 创建代理服务器实例
func NewProxyServer(configPath string) (*ProxyServer, error) {
	data, err := os.ReadFile(configPath)
	if err != nil {
		return nil, fmt.Errorf("读取配置文件失败: %w", err)
	}

	var config Config
	if err := json.Unmarshal(data, &config); err != nil {
		return nil, fmt.Errorf("解析配置文件失败: %w", err)
	}

	// 验证配置
	if len(config.Backends) == 0 {
		return nil, fmt.Errorf("配置文件中至少需要一个后端")
	}

	if config.Port == 0 {
		config.Port = 8080
	}

	if config.Retry.MaxAttempts == 0 {
		config.Retry.MaxAttempts = 3
	}

	if config.Retry.Timeout == 0 {
		config.Retry.Timeout = 30
	}

	server := &ProxyServer{
		config: &config,
		client: &http.Client{
			Timeout: time.Duration(config.Retry.Timeout) * time.Second,
		},
		healthMap: make(map[string]bool),
	}

	// 初始化所有后端为健康状态
	for _, backend := range config.Backends {
		if backend.Enabled {
			server.healthMap[backend.Name] = true
		}
	}

	// 启动健康检查
	if config.HealthCheck.Enabled {
		go server.startHealthCheck()
	}

	return server, nil
}

// isHealthy 检查后端是否健康
func (ps *ProxyServer) isHealthy(name string) bool {
	ps.healthMutex.RLock()
	defer ps.healthMutex.RUnlock()
	return ps.healthMap[name]
}

// setHealth 设置后端健康状态
func (ps *ProxyServer) setHealth(name string, healthy bool) {
	ps.healthMutex.Lock()
	defer ps.healthMutex.Unlock()
	ps.healthMap[name] = healthy
}

// startHealthCheck 定期健康检查
func (ps *ProxyServer) startHealthCheck() {
	interval := time.Duration(ps.config.HealthCheck.Interval) * time.Second
	if interval == 0 {
		interval = 60 * time.Second
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for range ticker.C {
		for _, backend := range ps.config.Backends {
			if !backend.Enabled {
				continue
			}

			go func(b Backend) {
				healthy := ps.checkBackend(b)
				ps.setHealth(b.Name, healthy)
				if healthy {
					log.Printf("[健康检查] %s ✓ 健康", b.Name)
				} else {
					log.Printf("[健康检查] %s ✗ 不可用", b.Name)
				}
			}(backend)
		}
	}
}

// checkBackend 检查单个后端是否可用
func (ps *ProxyServer) checkBackend(backend Backend) bool {
	// 使用 HTTP 请求检查（更准确地反映 API 可用性）
	// 首先尝试 /health 端点，如果失败则尝试实际的 API 端点

	healthURL := backend.BaseURL + "/v1/models"

	// 创建健康检查请求
	req, err := http.NewRequest("GET", healthURL, nil)
	if err != nil {
		log.Printf("[健康检查] %s - 创建请求失败: %v", backend.Name, err)
		return false
	}

	// 设置认证头
	req.Header.Set("Authorization", "Bearer "+backend.Token)

	// 创建专用的健康检查客户端（超时时间较短）
	client := &http.Client{
		Timeout: 5 * time.Second,
	}

	// 发送请求
	resp, err := client.Do(req)
	if err != nil {
		log.Printf("[健康检查] %s - 请求失败: %v", backend.Name, err)
		return false
	}
	defer resp.Body.Close()

	// 读取响应体（用于日志，自动处理 gzip）
	bodyBytes, readErr := readResponseBody(resp)
	if readErr != nil {
		log.Printf("[健康检查] %s - 读取响应失败: %v", backend.Name, readErr)
		return false
	}
	// 检查状态码
	if resp.StatusCode >= 200 && resp.StatusCode < 500 {
		// 2xx, 3xx, 4xx 都认为服务是可用的（4xx 表示服务在线但请求有问题）
		return true
	}

	// 5xx 错误表示服务不可用
	bodyStr := string(bodyBytes)
	if len(bodyStr) > 200 {
		bodyStr = bodyStr[:200] + "..."
	}
	log.Printf("[健康检查] %s - HTTP %d - 响应: %s", backend.Name, resp.StatusCode, bodyStr)
	return false
}

// ServeHTTP 处理 HTTP 请求
func (ps *ProxyServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// 读取请求体
	bodyBytes, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "读取请求体失败", http.StatusBadRequest)
		return
	}
	r.Body.Close()

	// 记录请求开始
	log.Printf("[请求开始] %s %s - 将尝试 %d 个后端", r.Method, r.URL.Path, len(ps.config.Backends))

	// 尝试所有启用的后端
	var lastErr error
	attemptCount := 0
	for _, backend := range ps.config.Backends {
		if !backend.Enabled {
			log.Printf("[跳过] %s (已禁用)", backend.Name)
			continue
		}

		// 跳过不健康的后端（如果启用了健康检查）
		if ps.config.HealthCheck.Enabled && !ps.isHealthy(backend.Name) {
			log.Printf("[跳过] %s (不健康)", backend.Name)
			continue
		}

		attemptCount++

		// 构建目标 URL 用于日志
		targetURL := backend.BaseURL + r.URL.Path
		if r.URL.RawQuery != "" {
			targetURL += "?" + r.URL.RawQuery
		}

		// 尝试请求后端
		// 显示 token 的前后各 4 个字符用于调试
		tokenPreview := backend.Token
		if len(tokenPreview) > 12 {
			tokenPreview = tokenPreview[:4] + "..." + tokenPreview[len(tokenPreview)-4:]
		}
		log.Printf("[尝试 #%d] %s - %s %s (token: %s)", attemptCount, backend.Name, r.Method, targetURL, tokenPreview)
		resp, err := ps.forwardRequest(backend, r, bodyBytes)
		if err != nil {
			lastErr = err
			log.Printf("[失败 #%d] %s - %s - %v", attemptCount, backend.Name, targetURL, err)
			// 标记为不健康
			if ps.config.HealthCheck.Enabled {
				ps.setHealth(backend.Name, false)
			}
			continue
		}

		// 成功，返回响应
		log.Printf("[成功 #%d] %s - %s - HTTP %d", attemptCount, backend.Name, targetURL, resp.StatusCode)
		ps.copyResponse(w, resp)
		return
	}

	// 所有后端都失败了
	log.Printf("[全部失败] 所有后端均不可用 (尝试了 %d 个后端)", attemptCount)
	errMsg := "所有 API 后端均不可用"
	if lastErr != nil {
		errMsg = fmt.Sprintf("%s: %v", errMsg, lastErr)
	}
	http.Error(w, errMsg, http.StatusBadGateway)
}

// forwardRequest 转发请求到指定后端
func (ps *ProxyServer) forwardRequest(backend Backend, originalReq *http.Request, bodyBytes []byte) (*http.Response, error) {
	// 构建目标 URL
	targetURL, err := url.Parse(backend.BaseURL)
	if err != nil {
		return nil, err
	}

	targetURL.Path = originalReq.URL.Path
	targetURL.RawQuery = originalReq.URL.RawQuery

	// 创建新请求
	req, err := http.NewRequest(originalReq.Method, targetURL.String(), bytes.NewReader(bodyBytes))
	if err != nil {
		return nil, err
	}

	// 复制原始请求头
	for key, values := range originalReq.Header {
		for _, value := range values {
			req.Header.Add(key, value)
		}
	}

	// 设置 Authorization Bearer Token（使用后端配置的 token）
	req.Header.Set("Authorization", "Bearer "+backend.Token)

	// 发送请求
	resp, err := ps.client.Do(req)
	if err != nil {
		return nil, err
	}

	// 检查响应状态码 - 记录所有非 2xx 响应的详细信息
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// 读取错误响应体用于日志（自动处理 gzip）
		bodyBytes, readErr := readResponseBody(resp)
		resp.Body.Close()

		if readErr != nil {
			return nil, fmt.Errorf("后端返回错误: HTTP %d (无法读取响应体: %v)", resp.StatusCode, readErr)
		}

		// 截断过长的响应（最多显示 500 字符）
		bodyStr := string(bodyBytes)
		if len(bodyStr) > 500 {
			bodyStr = bodyStr[:500] + "..."
		}

		log.Printf("[错误详情] %s - HTTP %d - 响应: %s", backend.Name, resp.StatusCode, bodyStr)

		// 只有 5xx 错误才触发故障转移
		if resp.StatusCode >= 500 {
			return nil, fmt.Errorf("后端返回错误: HTTP %d", resp.StatusCode)
		}

		// 3xx/4xx 错误不触发故障转移，但需要重新构造响应返回给客户端
		// 因为响应体已经被读取了，需要重新创建一个响应
		resp.Body = io.NopCloser(bytes.NewReader(bodyBytes))
		return resp, nil
	}

	return resp, nil
}

// readResponseBody 读取响应体，自动处理 gzip 压缩
func readResponseBody(resp *http.Response) ([]byte, error) {
	var reader io.Reader = resp.Body

	// 检查是否是 gzip 压缩
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

// copyResponse 复制响应到客户端
func (ps *ProxyServer) copyResponse(w http.ResponseWriter, resp *http.Response) {
	defer resp.Body.Close()

	// 复制响应头
	for key, values := range resp.Header {
		for _, value := range values {
			w.Header().Add(key, value)
		}
	}

	// 设置状态码
	w.WriteHeader(resp.StatusCode)

	// 复制响应体
	io.Copy(w, resp.Body)
}

func main() {
	configPath := flag.String("config", "config.json", "配置文件路径")
	flag.Parse()

	// 创建代理服务器
	server, err := NewProxyServer(*configPath)
	if err != nil {
		log.Fatalf("初始化失败: %v", err)
	}

	// 打印配置信息
	log.Printf("Claude API 故障转移代理启动中...")
	log.Printf("监听端口: %d", server.config.Port)
	log.Printf("配置的后端:")
	for i, backend := range server.config.Backends {
		status := "禁用"
		if backend.Enabled {
			status = "启用"
		}
		log.Printf("  %d. %s - %s [%s]", i+1, backend.Name, backend.BaseURL, status)
	}
	log.Printf("最大重试次数: %d", server.config.Retry.MaxAttempts)
	log.Printf("请求超时: %d 秒", server.config.Retry.Timeout)
	log.Printf("健康检查: %v", server.config.HealthCheck.Enabled)

	// 启动 HTTP 服务器
	addr := fmt.Sprintf(":%d", server.config.Port)
	log.Printf("\n✓ 代理服务器运行在 http://localhost%s", addr)
	log.Printf("✓ 配置 Claude Code: export ANTHROPIC_BASE_URL=http://localhost%s\n", addr)

	// 创建 HTTP 服务器实例（用于优雅关闭）
	srv := &http.Server{
		Addr:    addr,
		Handler: server,
	}

	// 在 goroutine 中启动服务器
	go func() {
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("服务器启动失败: %v", err)
		}
	}()

	// 监听系统信号（Ctrl+C 或 kill）
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit

	// 收到信号，开始优雅关闭
	log.Println("\n收到关闭信号，正在优雅关闭服务器...")

	// 设置 10 秒超时等待现有请求完成
	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Minute)
	defer cancel()

	if err := srv.Shutdown(ctx); err != nil {
		log.Printf("服务器强制关闭: %v", err)
	}

	log.Println("✓ 服务器已安全关闭")
}
