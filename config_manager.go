package main

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// ConfigManager handles thread-safe reading and writing of configuration
// with atomic operations and backup creation
// ConfigManager 处理配置的线程安全读写，支持原子操作和备份创建
type ConfigManager struct {
	configPath string
	mu         sync.RWMutex
	config     *Config
}

// NewConfigManager creates a new config manager instance
// NewConfigManager 创建新的配置管理器实例
func NewConfigManager(configPath string) (*ConfigManager, error) {
	cm := &ConfigManager{
		configPath: configPath,
	}

	// Load initial config
	// 加载初始配置
	config, err := loadConfig(configPath)
	if err != nil {
		return nil, fmt.Errorf("failed to load initial config: %w", err)
	}

	cm.config = config
	return cm, nil
}

// GetConfig returns a copy of the current configuration
// GetConfig 返回当前配置的副本
func (cm *ConfigManager) GetConfig() *Config {
	cm.mu.RLock()
	defer cm.mu.RUnlock()

	// Create a deep copy to avoid external modifications
	// 创建深拷贝以避免外部修改
	configBytes, err := json.Marshal(cm.config)
	if err != nil {
		return cm.config // Return original if copy fails
	}

	var copy Config
	if err := json.Unmarshal(configBytes, &copy); err != nil {
		return cm.config // Return original if copy fails
	}

	return &copy
}

// EnableBackend enables a backend by name and persists the change
// EnableBackend 通过名称启用后端并持久化更改
func (cm *ConfigManager) EnableBackend(name string) error {
	cm.mu.Lock()
	defer cm.mu.Unlock()

	// Find and enable the backend
	// 查找并启用后端
	for i := range cm.config.Backends {
		if cm.config.Backends[i].Name == name {
			if cm.config.Backends[i].Enabled {
				return fmt.Errorf("backend '%s' is already enabled", name)
			}

			cm.config.Backends[i].Enabled = true

			// Persist changes to file
			// 将更改持久化到文件
			if err := cm.persistConfig(); err != nil {
				// Revert change if persistence fails
				// 如果持久化失败则回滚更改
				cm.config.Backends[i].Enabled = false
				return fmt.Errorf("failed to persist config: %w", err)
			}

			return nil
		}
	}

	return fmt.Errorf("backend '%s' not found", name)
}

// DisableBackend disables a backend by name and persists the change
// DisableBackend 通过名称禁用后端并持久化更改
func (cm *ConfigManager) DisableBackend(name string) error {
	cm.mu.Lock()
	defer cm.mu.Unlock()

	// Find and disable the backend
	// 查找并禁用后端
	for i := range cm.config.Backends {
		if cm.config.Backends[i].Name == name {
			if !cm.config.Backends[i].Enabled {
				return fmt.Errorf("backend '%s' is already disabled", name)
			}

			cm.config.Backends[i].Enabled = false

			// Persist changes to file
			// 将更改持久化到文件
			if err := cm.persistConfig(); err != nil {
				// Revert change if persistence fails
				// 如果持久化失败则回滚更改
				cm.config.Backends[i].Enabled = true
				return fmt.Errorf("failed to persist config: %w", err)
			}

			return nil
		}
	}

	return fmt.Errorf("backend '%s' not found", name)
}

// GetBackendNames returns a list of all backend names
// GetBackendNames 返回所有后端名称的列表
func (cm *ConfigManager) GetBackendNames() []string {
	cm.mu.RLock()
	defer cm.mu.RUnlock()

	names := make([]string, len(cm.config.Backends))
	for i, backend := range cm.config.Backends {
		names[i] = backend.Name
	}

	return names
}

// IsBackendEnabled checks if a backend is enabled
// IsBackendEnabled 检查后端是否启用
func (cm *ConfigManager) IsBackendEnabled(name string) (bool, error) {
	cm.mu.RLock()
	defer cm.mu.RUnlock()

	for _, backend := range cm.config.Backends {
		if backend.Name == name {
			return backend.Enabled, nil
		}
	}

	return false, fmt.Errorf("backend '%s' not found", name)
}

// MaskToken masks a token for safe display (shows first 4 + ... + last 4 chars)
// MaskToken 对令牌进行掩码以便安全显示（显示前4个 + ... + 后4个字符）
func MaskToken(token string) string {
	if len(token) <= 8 {
		return "****" // Too short to mask meaningfully
	}

	return token[:4] + "..." + token[len(token)-4:]
}

// persistConfig atomically writes the config to disk with backup creation
// persistConfig 原子性地将配置写入磁盘并创建备份
func (cm *ConfigManager) persistConfig() error {
	// Create backup of current config file
	// 创建当前配置文件的备份
	if err := cm.createBackup(); err != nil {
		return fmt.Errorf("failed to create backup: %w", err)
	}

	// Marshal config to JSON with indentation for readability
	// 将配置序列化为带缩进的JSON以提高可读性
	configBytes, err := json.MarshalIndent(cm.config, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal config: %w", err)
	}

	// Write to temporary file first for atomic operation
	// 首先写入临时文件以实现原子操作
	tempPath := cm.configPath + ".tmp"
	if err := ioutil.WriteFile(tempPath, configBytes, 0644); err != nil {
		return fmt.Errorf("failed to write temporary config file: %w", err)
	}

	// Atomically replace the original file
	// 原子性地替换原始文件
	if err := os.Rename(tempPath, cm.configPath); err != nil {
		// Clean up temp file if rename fails
		// 如果重命名失败则清理临时文件
		os.Remove(tempPath)
		return fmt.Errorf("failed to replace config file: %w", err)
	}

	return nil
}

// createBackup creates a timestamped backup of the current config file
// createBackup 创建当前配置文件的时间戳备份
func (cm *ConfigManager) createBackup() error {
	// Check if original file exists
	// 检查原始文件是否存在
	if _, err := os.Stat(cm.configPath); os.IsNotExist(err) {
		return nil // No backup needed if file doesn't exist
	}

	// Create backup directory if it doesn't exist
	// 如果备份目录不存在则创建
	backupDir := filepath.Join(filepath.Dir(cm.configPath), "backups")
	if err := os.MkdirAll(backupDir, 0755); err != nil {
		return fmt.Errorf("failed to create backup directory: %w", err)
	}

	// Generate backup filename with timestamp
	// 生成带时间戳的备份文件名
	timestamp := time.Now().Format("20060102-150405")
	backupPath := filepath.Join(backupDir, fmt.Sprintf("config.%s.json", timestamp))

	// Read original file
	// 读取原始文件
	originalBytes, err := ioutil.ReadFile(cm.configPath)
	if err != nil {
		return fmt.Errorf("failed to read original config file: %w", err)
	}

	// Write backup file
	// 写入备份文件
	if err := ioutil.WriteFile(backupPath, originalBytes, 0644); err != nil {
		return fmt.Errorf("failed to write backup file: %w", err)
	}

	// Clean up old backups (keep only last 5)
	// 清理旧备份（只保留最近5个）
	if err := cm.cleanupOldBackups(backupDir); err != nil {
		// Log warning but don't fail the operation
		// 记录警告但不使操作失败
		fmt.Printf("Warning: failed to cleanup old backups: %v\n", err)
	}

	return nil
}

// cleanupOldBackups removes old backup files, keeping only the most recent ones
// cleanupOldBackups 删除旧备份文件，只保留最新的
func (cm *ConfigManager) cleanupOldBackups(backupDir string) error {
	// Read backup directory
	// 读取备份目录
	files, err := ioutil.ReadDir(backupDir)
	if err != nil {
		return err
	}

	// Filter backup files and sort by modification time
	// 过滤备份文件并按修改时间排序
	var backupFiles []os.FileInfo
	for _, file := range files {
		if !file.IsDir() && len(file.Name()) > 17 && file.Name()[0:6] == "config." && file.Name()[len(file.Name())-5:] == ".json" {
			backupFiles = append(backupFiles, file)
		}
	}

	// Sort by modification time (oldest first)
	// 按修改时间排序（最旧的在前面）
	for i := 0; i < len(backupFiles)-1; i++ {
		for j := i + 1; j < len(backupFiles); j++ {
			if backupFiles[i].ModTime().After(backupFiles[j].ModTime()) {
				backupFiles[i], backupFiles[j] = backupFiles[j], backupFiles[i]
			}
		}
	}

	// Remove old backups (keep only last 5)
	// 删除旧备份（只保留最近5个）
	const maxBackups = 5
	if len(backupFiles) > maxBackups {
		for i := 0; i < len(backupFiles)-maxBackups; i++ {
			oldBackup := filepath.Join(backupDir, backupFiles[i].Name())
			if err := os.Remove(oldBackup); err != nil {
				return fmt.Errorf("failed to remove old backup %s: %w", oldBackup, err)
			}
		}
	}

	return nil
}