package main

import (
	"encoding/json"
	"fmt"
	"os"
)

// loadConfig loads and validates configuration from file
func loadConfig(configPath string) (*Config, error) {
	data, err := os.ReadFile(configPath)
	if err != nil {
		return nil, fmt.Errorf("读取配置文件失败: %w", err)
	}

	var config Config
	if err := json.Unmarshal(data, &config); err != nil {
		return nil, fmt.Errorf("解析配置文件失败: %w", err)
	}

	if len(config.Backends) == 0 {
		return nil, fmt.Errorf("配置文件中至少需要一个后端")
	}

	// Set default values
	if config.Port == 0 {
		config.Port = 8080
	}

	if config.Retry.MaxAttempts == 0 {
		config.Retry.MaxAttempts = 3
	}

	if config.Retry.Timeout == 0 {
		config.Retry.Timeout = 30
	}

	// Set default failover config
	if config.Failover.CircuitBreaker.FailureThreshold == 0 {
		config.Failover.CircuitBreaker.FailureThreshold = 3
	}
	if config.Failover.CircuitBreaker.OpenTimeoutSeconds == 0 {
		config.Failover.CircuitBreaker.OpenTimeoutSeconds = 30
	}
	if config.Failover.CircuitBreaker.HalfOpenRequests == 0 {
		config.Failover.CircuitBreaker.HalfOpenRequests = 1
	}
	if config.Failover.RateLimit.CooldownSeconds == 0 {
		config.Failover.RateLimit.CooldownSeconds = 60
	}

	return &config, nil
}
