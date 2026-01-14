package main

import (
	"net/http"
	"time"

	"github.com/xushuhui/ares"
)

// BackendStatus represents the current status of a backend including circuit breaker state
// BackendStatus 表示后端的当前状态，包括断路器状态
type BackendStatus struct {
	Name           string `json:"name"`
	Enabled        bool   `json:"enabled"`
	CircuitBreaker struct {
		State               string    `json:"state"`
		ConsecutiveFailures int       `json:"consecutive_failures"`
		LastFailureTime     time.Time `json:"last_failure_time"`
	} `json:"circuit_breaker"`
	RateLimit struct {
		CooldownUntil *time.Time `json:"cooldown_until"`
		RetryAfter    int        `json:"retry_after_seconds"`
	} `json:"rate_limit"`
	LastError string `json:"last_error,omitempty"`
}

// BackendInfo represents backend information for listing (without sensitive data)
// BackendInfo 表示用于列出后端的信息（不包含敏感数据）
type BackendInfo struct {
	Name        string `json:"name"`
	BaseURL     string `json:"base_url"`
	Enabled     bool   `json:"enabled"`
	Model       string `json:"model,omitempty"`
	TokenMasked string `json:"token_masked"`
}

// BackendsResponse represents the response for listing backends
// BackendsResponse 表示列出后端的响应
type BackendsResponse struct {
	Backends []BackendInfo `json:"backends"`
	Count    int           `json:"count"`
}

// BackendStatusResponse represents the response for backend status
// BackendStatusResponse 表示后端状态的响应
type BackendStatusResponse struct {
	Backends []BackendStatus `json:"backends"`
	Count    int             `json:"count"`
}

// ErrorResponse represents an error response
// ErrorResponse 表示错误响应
type ErrorResponse struct {
	Error   string `json:"error"`
	Message string `json:"message,omitempty"`
}

// SuccessResponse represents a success response
// SuccessResponse 表示成功响应
type SuccessResponse struct {
	Success bool   `json:"success"`
	Message string `json:"message"`
}

// AresManagementServer wraps the proxy functionality with Ares framework
// AresManagementServer 使用 Ares 框架包装代理功能
type AresManagementServer struct {
	app            *ares.Ares
	configManager  *ConfigManager
	circuitBreaker *CircuitBreaker
	proxyServer    *ProxyServer
}

// NewAresManagementServer creates a new Ares-based management server
// NewAresManagementServer 创建新的基于 Ares 的管理服务器
func NewAresManagementServer(configManager *ConfigManager, circuitBreaker *CircuitBreaker, proxyServer *ProxyServer) *AresManagementServer {
	// Create Ares app with default middleware (logger + recovery)
	app := ares.Default()

	server := &AresManagementServer{
		app:            app,
		configManager:  configManager,
		circuitBreaker: circuitBreaker,
		proxyServer:    proxyServer,
	}

	// Setup routes
	server.setupRoutes()

	return server
}

// setupRoutes configures all HTTP routes
// setupRoutes 配置所有 HTTP 路由
func (ams *AresManagementServer) setupRoutes() {
	// Management API group

	// Backend management endpoints
	backends := ams.app.Group("/backends")
	backends.GET("", ams.listBackends)
	backends.GET("/status", ams.getBackendStatus)
	backend := ams.app.Group("/backend")
	backend.GET("/{name}/enable", ams.enableBackend)
	backend.GET("/{name}/disable", ams.disableBackend)

	// Health check
	ams.app.GET("/health", ams.healthCheck)

	// Proxy all other requests to the original proxy server
	// Use chi's NotFound handler to catch all unmatched routes
	ams.app.NotFound(func(w http.ResponseWriter, r *http.Request) {
		// Forward to original proxy server
		ams.proxyServer.ServeHTTP(w, r)
	})
}

// listBackends handles GET /api/backends
// listBackends 处理 GET /api/backends
func (ams *AresManagementServer) listBackends(ctx *ares.Context) error {
	config := ams.configManager.GetConfig()

	backends := make([]BackendInfo, len(config.Backends))
	for i, backend := range config.Backends {
		backends[i] = BackendInfo{
			Name:        backend.Name,
			BaseURL:     backend.BaseURL,
			Enabled:     backend.Enabled,
			Model:       backend.Model,
			TokenMasked: MaskToken(backend.Token),
		}
	}

	response := BackendsResponse{
		Backends: backends,
		Count:    len(backends),
	}

	return ctx.JSON(http.StatusOK, response)
}

// getBackendStatus handles GET /api/backends/status
// getBackendStatus 处理 GET /api/backends/status
func (ams *AresManagementServer) getBackendStatus(ctx *ares.Context) error {
	config := ams.configManager.GetConfig()

	backends := make([]BackendStatus, len(config.Backends))
	for i, backend := range config.Backends {
		// Get circuit breaker state for this backend
		cbState := ams.circuitBreaker.GetBackendState(backend.Name)

		// Get rate limit state for this backend
		rlState := ams.circuitBreaker.GetRateLimitState(backend.Name)

		backends[i] = BackendStatus{
			Name:    backend.Name,
			Enabled: backend.Enabled,
			CircuitBreaker: struct {
				State               string    `json:"state"`
				ConsecutiveFailures int       `json:"consecutive_failures"`
				LastFailureTime     time.Time `json:"last_failure_time"`
			}{
				State:               cbState.State,
				ConsecutiveFailures: cbState.ConsecutiveFailures,
				LastFailureTime:     cbState.LastFailureTime,
			},
			RateLimit: struct {
				CooldownUntil *time.Time `json:"cooldown_until"`
				RetryAfter    int        `json:"retry_after_seconds"`
			}{
				CooldownUntil: rlState.CooldownUntil,
				RetryAfter:    rlState.RetryAfter,
			},
			LastError: cbState.LastError,
		}
	}

	response := BackendStatusResponse{
		Backends: backends,
		Count:    len(backends),
	}

	return ctx.JSON(http.StatusOK, response)
}

// enableBackend handles POST /api/backends/{name}/enable
// enableBackend 处理 POST /api/backends/{name}/enable
func (ams *AresManagementServer) enableBackend(ctx *ares.Context) error {
	backendName := ctx.Param("name")
	if backendName == "" {
		return ctx.JSON(http.StatusBadRequest, ErrorResponse{
			Error:   "Missing backend name",
			Message: "Backend name is required",
		})
	}

	// Enable the backend
	if err := ams.configManager.EnableBackend(backendName); err != nil {
		return ctx.JSON(http.StatusBadRequest, ErrorResponse{
			Error:   "Failed to enable backend",
			Message: err.Error(),
		})
	}

	// Notify circuit breaker about the change
	ams.circuitBreaker.OnBackendEnabled(backendName)

	response := SuccessResponse{
		Success: true,
		Message: "Backend '" + backendName + "' has been enabled",
	}

	return ctx.JSON(http.StatusOK, response)
}

// disableBackend handles POST /api/backends/{name}/disable
// disableBackend 处理 POST /api/backends/{name}/disable
func (ams *AresManagementServer) disableBackend(ctx *ares.Context) error {
	backendName := ctx.Param("name")
	if backendName == "" {
		return ctx.JSON(http.StatusBadRequest, ErrorResponse{
			Error:   "Missing backend name",
			Message: "Backend name is required",
		})
	}

	// Disable the backend
	if err := ams.configManager.DisableBackend(backendName); err != nil {
		return ctx.JSON(http.StatusBadRequest, ErrorResponse{
			Error:   "Failed to disable backend",
			Message: err.Error(),
		})
	}

	// Notify circuit breaker about the change
	ams.circuitBreaker.OnBackendDisabled(backendName)

	response := SuccessResponse{
		Success: true,
		Message: "Backend '" + backendName + "' has been disabled",
	}

	return ctx.JSON(http.StatusOK, response)
}

// healthCheck handles GET /health
// healthCheck 处理 GET /health
func (ams *AresManagementServer) healthCheck(ctx *ares.Context) error {
	config := ams.configManager.GetConfig()

	enabledCount := 0
	for _, backend := range config.Backends {
		if backend.Enabled {
			enabledCount++
		}
	}

	response := map[string]interface{}{
		"status":           "healthy",
		"total_backends":   len(config.Backends),
		"enabled_backends": enabledCount,
		"timestamp":        time.Now().UTC(),
	}

	return ctx.JSON(http.StatusOK, response)
}

// Run starts the Ares management server
// Run 启动 Ares 管理服务器
func (ams *AresManagementServer) Run(addr string, opts ...ares.Option) error {
	return ams.app.Run(addr, opts...)
}

// GetHandler returns the HTTP handler for use with custom servers
// GetHandler 返回 HTTP 处理器以供自定义服务器使用
func (ams *AresManagementServer) GetHandler() http.Handler {
	return ams.app
}
