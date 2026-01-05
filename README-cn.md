# CC Proxy

一个轻量级的 Claude API 反向代理，支持多个 API key 的自动故障转移。当一个后端失败时，自动切换到下一个可用后端，对客户端完全透明。

[English Documentation](README.md)

## 功能特性

- **自动故障转移**：当主 API key 失败时，自动尝试备用 key（仅 5xx/429 错误触发）
- **详细错误日志**：所有非 2xx 响应都会记录详细的错误信息，支持 gzip 自动解压
- **透明代理**：完整转发所有 HTTP 请求和响应
- **配置灵活**：JSON 配置文件，支持多个后端、超时、重试等配置
- **零依赖**：编译后单个二进制文件，无需额外依赖
- **安全日志**：Token 仅显示前后 4 个字符，避免泄露完整密钥
- **模型覆盖**：可选的按后端模型覆盖，强制使用特定模型版本

## 快速开始

### 1. 编译程序

```bash
go mod tidy
go build -o cc-proxy
```

### 2. 配置后端

编辑 `config.json` 文件，配置你的 API token 和后端地址：

```json
{
  "port": 3456,
  "backends": [
    {
      "name": "Anthropic Official",
      "base_url": "https://api.anthropic.com",
      "token": "sk-ant-api03-your-key-1",
      "enabled": true,
      "model": "claude-3-5-sonnet-20241022"
    },
    {
      "name": "Backup Provider",
      "base_url": "https://api.backup.example.com",
      "token": "your-backup-key",
      "enabled": true
    }
  ],
  "retry": {
    "max_attempts": 3,
    "timeout_seconds": 30
  }
}
```

**重要**：
- 配置字段是 `token` 而不是 `api_key`
- `model` 字段是可选的 - 如果指定，代理会覆盖请求体中的 model 字段

### 3. 启动代理

```bash
./cc-proxy -config config.json
```

输出示例：
```
2024/12/26 12:00:00 Claude API 故障转移代理启动中...
2024/12/26 12:00:00 监听端口: 3456
2024/12/26 12:00:00 配置的后端:
2024/12/26 12:00:00   1. Anthropic Official - https://api.anthropic.com [启用]
2024/12/26 12:00:00   2. Backup Provider - https://api.backup.example.com [启用]
2024/12/26 12:00:00 最大重试次数: 3
2024/12/26 12:00:00 请求超时: 30 秒

2024/12/26 12:00:00 ✓ 代理服务器运行在 http://localhost:3456
2024/12/26 12:00:00 ✓ 配置 Claude Code: export ANTHROPIC_BASE_URL=http://localhost:3456
```

### 4. 配置 Claude Code

```bash
# 设置 Claude Code 使用本地代理
export ANTHROPIC_BASE_URL=http://localhost:3456
export ANTHROPIC_API_KEY=dummy  # 由代理处理真实 token

# 启动 Claude Code
claude
```

或者配置其他 Claude 客户端使用代理。

## 配置说明

### 基本配置

| 配置项 | 说明 | 默认值 |
|--------|------|--------|
| `port` | 代理服务器监听端口 | 3456 |

### 后端配置

每个后端支持以下配置：

| 配置项 | 说明 | 必填 |
|--------|------|------|
| `name` | 后端名称（用于日志） | 是 |
| `base_url` | API 基础 URL | 是 |
| `token` | API Token（注意是 token 不是 api_key） | 是 |
| `enabled` | 是否启用 | 是 |
| `model` | 模型覆盖（可选） | 否 |

后端按配置顺序优先使用，失败后自动尝试下一个。**注意**：只有 5xx/429 错误和网络错误才会触发故障转移，其他 4xx 错误（如 403）会直接返回给客户端。

当指定 `model` 时，代理会在转发到后端之前替换请求体中的 "model" 字段。这对于强制使用特定模型版本很有用。

### 重试配置

| 配置项 | 说明 | 默认值 |
|--------|------|--------|
| `retry.max_attempts` | 最大重试次数 | 3 |
| `retry.timeout_seconds` | 单次请求超时（秒） | 30 |

## 工作原理

1. **请求接收**：代理接收 Claude Code 的 API 请求
2. **后端选择**：按配置顺序选择第一个启用的后端
3. **模型覆盖**：如果后端配置了 `model`，替换请求体中的 model 字段
4. **请求转发**：将请求转发到选中的后端，替换 Authorization 头为后端的 token
5. **故障转移**：如果请求失败（5xx/429 错误或网络错误），自动尝试下一个后端
6. **响应返回**：将后端响应完整返回给客户端

**重要**：只有 5xx/429 错误和网络错误才触发故障转移。其他 3xx/4xx 错误（如 403 认证失败）会直接返回给客户端，不会尝试其他后端。

```
Claude Code → 本地代理 → 后端1 (500 错误)
                     ↓
                    后端2 (成功) → 响应
```

## 日志说明

代理会输出详细的请求日志，帮助你了解请求处理过程：

### 请求处理日志

```
[请求开始] POST /v1/messages - 将尝试 3 个后端
[尝试 #1] Anthropic Official - POST https://api.anthropic.com/v1/messages (token: sk-a...xyz1)
[模型覆盖] Anthropic Official - 使用配置的 model: claude-3-5-sonnet-20241022
[错误详情] Anthropic Official - HTTP 500 - 响应: {"error":{"type":"internal_error","message":"Service temporarily unavailable"}}
[失败 #1] Anthropic Official - 后端返回错误: HTTP 500
[尝试 #2] Backup Provider - POST https://api.backup.com/v1/messages (token: sk-b...abc2)
[成功 #2] Backup Provider - HTTP 200
```

### 日志特性

- **Token 安全**：只显示 token 的前 4 和后 4 个字符（如 `sk-a...xyz1`），避免泄露完整密钥
- **尝试编号**：显示 `#1`、`#2` 等，清楚看到尝试了哪些后端
- **错误详情**：所有非 2xx 响应都会显示完整的错误信息（自动解压 gzip）
- **跳过原因**：显示后端被跳过的原因（已禁用）
- **模型覆盖**：显示请求模型被后端配置覆盖的情况

## 高级用法

### 作为系统服务运行

创建 systemd 服务文件 `/etc/systemd/system/cc-proxy.service`：

```ini
[Unit]
Description=CC Proxy - Claude API Failover Service
After=network.target

[Service]
Type=simple
User=xsh
WorkingDirectory=/Users/xsh/gp/claude-proxy
ExecStart=/Users/xsh/gp/claude-proxy/cc-proxy -config /Users/xsh/gp/claude-proxy/config.json
Restart=on-failure

[Install]
WantedBy=multi-user.target
```

启动服务：

```bash
sudo systemctl daemon-reload
sudo systemctl enable cc-proxy
sudo systemctl start cc-proxy
```

### 后台运行

```bash
# 使用 nohup 后台运行
nohup ./cc-proxy -config config.json > proxy.log 2>&1 &

# 查看日志
tail -f proxy.log

# 停止服务
pkill cc-proxy
```

## 故障排查

### 问题：间歇性 403/200 响应

**症状**：有时候请求成功（200），有时候失败（403）

**可能原因**：
1. **多个后端轮换使用**：第一个后端 token 无效返回 403，但 403 不触发故障转移，所以直接返回给客户端
2. **Token 限流**：某个 token 超过限流后返回 403，等待一段时间后恢复

**排查方法**：
1. 查看日志中的 `[尝试 #N]` 行，检查 token 预览（如 `sk-a...xyz1`）
2. 查看 `[错误详情]` 中的具体错误信息
3. 如果不同请求使用了不同的 token，说明在多个后端间切换

**解决方案**：
- 在 config.json 中禁用无效 token 的后端并重启代理
- 检查并更新无效的 token
- 如果是限流问题，考虑增加更多后端或降低请求频率

### 问题：所有后端都失败

**排查步骤**：
1. 检查网络连接：`curl -I https://api.anthropic.com`
2. 验证 token 是否有效（查看 `[错误详情]` 日志）
3. 确认配置文件中使用的是 `token` 字段而不是 `api_key`
4. 确认后端 URL 是否正确

### 问题：请求超时

**解决方案**：
1. 增加 `retry.timeout_seconds` 配置
2. 检查网络延迟
3. 查看后端是否响应缓慢

## 性能优化

- **内存占用**：约 10-20MB
- **并发支持**：Go 原生并发，支持大量并发连接
- **延迟**：代理转发延迟 < 5ms

## 技术栈

- **语言**：Go 1.21+
- **依赖**：
  - `encoding/json` - JSON 配置解析（标准库）
  - Go 标准库（`net/http`、`compress/gzip` 等）
- **特性**：
  - 零外部依赖
  - 自动 gzip 解压
  - 优雅关闭支持
  - 不设置 API 版本头，兼容各种后端
  - 按后端模型覆盖支持

## 许可证

MIT License

## 贡献

欢迎提交 Issue 和 Pull Request。

## 相关链接

- [Claude Code 官方文档](https://code.claude.com/docs)
- [Anthropic API 文档](https://docs.anthropic.com)
