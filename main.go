package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)

func main() {
	configPath := flag.String("config", "config.json", "配置文件路径")
	flag.Parse()

	server, err := NewProxyServer(*configPath)
	if err != nil {
		log.Fatalf("初始化失败: %v", err)
	}

	log.Printf("Claude API 故障转移代理启动中...")
	log.Printf("监听端口: %d", server.config.Port)
	log.Printf("配置的后端:")
	for i, backend := range server.config.Backends {
		status := "禁用"
		if backend.Enabled {
			status = "启用"
		}
		modelInfo := ""
		if backend.Model != "" {
			modelInfo = fmt.Sprintf(" (模型覆盖: %s)", backend.Model)
		}
		log.Printf("  %d. %s - %s [%s]%s", i+1, backend.Name, backend.BaseURL, status, modelInfo)
	}
	log.Printf("最大重试次数: %d", server.config.Retry.MaxAttempts)
	log.Printf("请求超时: %d 秒", server.config.Retry.Timeout)
	log.Printf("熔断配置: 连续失败 %d 次触发,熔断 %d 秒",
		server.config.Failover.CircuitBreaker.FailureThreshold,
		server.config.Failover.CircuitBreaker.OpenTimeoutSeconds)
	log.Printf("限流配置: 429 错误后冷却 %d 秒",
		server.config.Failover.RateLimit.CooldownSeconds)

	addr := fmt.Sprintf(":%d", server.config.Port)
	log.Printf("\n✓ 代理服务器运行在 http://localhost%s", addr)
	log.Printf("✓ 配置 Claude Code: export ANTHROPIC_BASE_URL=http://localhost%s\n", addr)

	srv := &http.Server{
		Addr:    addr,
		Handler: server,
	}

	go func() {
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("服务器启动失败: %v", err)
		}
	}()

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit

	log.Println("\n收到关闭信号,正在优雅关闭服务器...")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := srv.Shutdown(ctx); err != nil {
		log.Printf("服务器强制关闭: %v", err)
	}

	log.Println("✓ 服务器已安全关闭")
}
