# CC Proxy

A lightweight Claude API reverse proxy with automatic failover support for multiple API keys. When one backend fails, it automatically switches to the next available backend, completely transparent to clients.

[中文文档](README.zh.md)

## Features

- **Automatic Failover**: Automatically tries backup keys when primary API key fails (only 5xx/429 errors trigger failover)
- **Circuit Breaker**: Smart circuit breaker prevents repeated requests to failing backends
- **Rate Limit Handling**: Intelligent 429 error handling with cooldown and Retry-After header support
- **Timeout Handling**: Request timeouts trigger automatic failover to next backend
- **Detailed Error Logging**: All non-2xx responses are logged with detailed error information, supports automatic gzip decompression
- **Transparent Proxy**: Fully forwards all HTTP requests and responses
- **Flexible Configuration**: JSON configuration file, supports multiple backends, timeout, retry settings
- **Zero Dependencies**: Single binary after compilation, no additional dependencies required
- **Secure Logging**: Tokens only show first and last 4 characters to avoid leaking complete keys
- **Model Override**: Optional per-backend model override to force specific model versions

## Quick Start

### 1. Build

```bash
go mod tidy
go build -o cc-proxy
```

### 2. Configure Backends

Edit `config.json` file to configure your API tokens and backend addresses:

```json
{
  "port": 3456,  // Proxy server listening port
  "backends": [
    {
      "name": "Anthropic Official",      // Backend name (for logging)
      "base_url": "https://api.anthropic.com",  // API base URL
      "token": "sk-ant-api03-your-key-1",       // API token (not api_key!)
      "enabled": true,                          // Whether this backend is enabled
      "model": "claude-3-5-sonnet-20241022"    // (Optional) Override model in requests
    },
    {
      "name": "Backup Provider",
      "base_url": "https://api.backup.example.com",
      "token": "your-backup-key",
      "enabled": true
    }
  ],
  "retry": {
    "max_attempts": 3,        // Maximum retry attempts (unused in current version)
    "timeout_seconds": 30     // Request timeout in seconds
  },
  "failover": {
    "circuit_breaker": {
      "failure_threshold": 3,       // Number of consecutive failures to trigger circuit breaker
      "open_timeout_seconds": 30,   // How long circuit stays open (seconds)
      "half_open_requests": 1       // Number of test requests in half-open state
    },
    "rate_limit": {
      "cooldown_seconds": 60        // Cooldown time after 429 rate limit error (seconds)
    }
  }
}
```

**Important**:
- The configuration field is `token`, not `api_key`
- The `model` field is optional - if specified, the proxy will override the model in the request body
- The `failover` section is optional - defaults will be used if not specified

### 3. Start Proxy

```bash
./cc-proxy -config config.json
```

Example output:
```
2024/12/26 12:00:00 Claude API 故障转移代理启动中...
2024/12/26 12:00:00 监听端口: 3456
2024/12/26 12:00:00 配置的后端:
2024/12/26 12:00:00   1. Anthropic Official - https://api.anthropic.com [启用]
2024/12/26 12:00:00   2. Backup Provider - https://api.backup.example.com [启用]
2024/12/26 12:00:00 最大重试次数: 3
2024/12/26 12:00:00 请求超时: 30 秒
2024/12/26 12:00:00 熔断配置: 连续失败 3 次触发,熔断 30 秒
2024/12/26 12:00:00 限流配置: 429 错误后冷却 60 秒

2024/12/26 12:00:00 ✓ 代理服务器运行在 http://localhost:3456
2024/12/26 12:00:00 ✓ 配置 Claude Code: export ANTHROPIC_BASE_URL=http://localhost:3456
```

### 4. Configure Claude Code

```bash
# Set Claude Code to use local proxy
export ANTHROPIC_BASE_URL=http://localhost:3456
export ANTHROPIC_API_KEY=dummy  # Proxy handles real tokens

# Start Claude Code
claude
```

Or configure other Claude clients to use the proxy.

## Configuration

### Basic Configuration

| Config | Description | Default |
|--------|-------------|---------|
| `port` | Proxy server listening port | 3456 |

### Backend Configuration

Each backend supports the following configuration:

| Config | Description | Required |
|--------|-------------|----------|
| `name` | Backend name (for logging) | Yes |
| `base_url` | API base URL | Yes |
| `token` | API Token (note: token not api_key) | Yes |
| `enabled` | Whether enabled | Yes |
| `model` | Model override (optional) | No |

Backends are tried in order of priority. **Note**: Only 5xx/429 errors and network errors trigger failover. Other 4xx errors (like 403) are returned directly to the client.

When `model` is specified, the proxy will replace the "model" field in the request body with the configured value before forwarding to the backend. This is useful for forcing specific model versions.

### Retry Configuration

| Config | Description | Default |
|--------|-------------|---------|
| `retry.max_attempts` | Maximum retry attempts (unused in current version) | 3 |
| `retry.timeout_seconds` | Single request timeout (seconds) | 30 |

### Failover Configuration

The `failover` section configures circuit breaker and rate limit handling. All fields are optional with sensible defaults.

| Config | Description | Default |
|--------|-------------|---------|
| `failover.circuit_breaker.failure_threshold` | Consecutive failures to trigger circuit breaker | 3 |
| `failover.circuit_breaker.open_timeout_seconds` | How long circuit stays open before testing recovery (seconds) | 30 |
| `failover.circuit_breaker.half_open_requests` | Number of test requests allowed in half-open state | 1 |
| `failover.rate_limit.cooldown_seconds` | Cooldown time after 429 rate limit (seconds) | 60 |

**Circuit Breaker States**:
- **Closed (Normal)**: All requests go through normally
- **Open (Circuit Tripped)**: Backend is skipped after N consecutive failures
- **Half-Open (Testing)**: After timeout expires, allows limited test requests to check if backend recovered

## How It Works

1. **Request Reception**: Proxy receives Claude Code API requests
2. **Backend Selection**: Selects backends based on priority (normal > rate-limited > circuit-broken)
3. **Circuit Breaker Check**: Skips backends in circuit-open state
4. **Model Override**: If backend has `model` configured, replaces the model field in request body
5. **Request Forwarding**: Forwards request to selected backend, replaces Authorization header with backend's token
6. **Error Handling**:
   - **5xx errors**: Record failure, trigger circuit breaker if threshold reached, try next backend
   - **429 rate limit**: Record rate limit timestamp, lower priority for cooldown period, try next backend
   - **Timeout**: Record failure, try next backend
   - **401/403**: Return immediately without retry (authentication error)
   - **Other 4xx**: Return immediately without retry (client error)
7. **Circuit Breaker Recovery**: After timeout, backend enters half-open state for testing
8. **Response Return**: Returns backend response completely to client

**Important**: Only 5xx/429 errors, timeouts, and network errors trigger failover. Other 3xx/4xx errors (like 403 authentication failure) are returned directly to the client without trying other backends.

```
Request → Priority Sort (normal > rate-limited > circuit-broken)
       ↓
   Backend1 (circuit open - skip)
       ↓
   Backend2 (500 error → record failure → try next)
       ↓
   Backend3 (429 → record rate limit → try next)
       ↓
   Backend4 (success) → Response
```

## Logging

The proxy outputs detailed request logs to help you understand the request processing:

### Request Processing Logs

```
[请求开始] POST /v1/messages - 配置了 3 个后端
[跳过] Backend1 - 熔断中 (还需 25 秒)
[尝试 #1] Backend2 - POST https://api.anthropic.com/v1/messages (token: sk-a...xyz1)
[模型覆盖] Backend2 - 使用配置的模型: claude-3-5-sonnet-20241022
[错误详情] Backend2 - HTTP 500 - 响应: {"error":{"type":"internal_error","message":"Service temporarily unavailable"}}
[失败 #1] Backend2 - 后端返回错误: HTTP 500
[熔断触发] Backend2 - 连续失败 3 次,熔断 30 秒 (HTTP 500)
[尝试 #2] Backend3 - POST https://api.backup.com/v1/messages (token: sk-b...abc2)
[成功 #2] Backend3 - HTTP 200
```

**Circuit Breaker Logs**:
```
[熔断触发] Backend1 - 连续失败 3 次,熔断 30 秒 (HTTP 502)
[跳过] Backend1 - 熔断中 (还需 25 秒)
[尝试 #1] Backend1 - ... [熔断测试 1/1]
[熔断恢复] Backend1 - 后端已恢复正常
```

**Rate Limit Logs**:
```
[限流记录] Backend2 - 触发 429,Retry-After: 60 秒
[限流记录] Backend3 - 触发 429,冷却 60 秒
```

**Timeout Logs**:
```
[超时] Backend1 - 请求超时 (30 秒)
[失败 #1] Backend1 - ... context deadline exceeded
```

### Log Features

- **Token Security**: Only shows first 4 and last 4 characters of token (e.g., `sk-a...xyz1`) to avoid leaking complete keys
- **Attempt Numbering**: Shows `#1`, `#2`, etc., clearly see which backends were tried
- **Error Details**: All non-2xx responses show complete error information (automatically decompresses gzip)
- **Skip Reason**: Shows reason why backend was skipped (熔断中/半开测试中/禁用)
- **Model Override**: Shows when request model is overridden by backend configuration
- **Circuit Breaker Events**: Logs when circuit opens, tests recovery, and closes
- **Rate Limit Tracking**: Logs 429 errors with cooldown/Retry-After information
- **Timeout Detection**: Specifically marks timeout errors

## Advanced Usage

### Run as System Service

Create systemd service file `/etc/systemd/system/cc-proxy.service`:

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

Start service:

```bash
sudo systemctl daemon-reload
sudo systemctl enable cc-proxy
sudo systemctl start cc-proxy
```

### Run in Background

```bash
# Run in background with nohup
nohup ./cc-proxy -config config.json > proxy.log 2>&1 &

# View logs
tail -f proxy.log

# Stop service
pkill cc-proxy
```

## Troubleshooting

### Issue: Backend Keeps Getting Circuit Broken

**Symptoms**: Logs show `[熔断触发]` and `[跳过] - 熔断中`

**Possible Causes**:
1. Backend is actually down or unstable
2. Timeout too short for slow backend
3. Network issues causing failures

**Solutions**:
1. Check backend health independently: `curl -I https://backend-url/v1/messages`
2. Increase `retry.timeout_seconds` if backend is slow
3. Increase `failover.circuit_breaker.failure_threshold` to be more tolerant
4. Increase `failover.circuit_breaker.open_timeout_seconds` for faster recovery attempts
5. Temporarily disable the backend in config if it's known to be down

### Issue: Too Many 429 Rate Limit Errors

**Symptoms**: Logs show `[限流记录]` frequently

**Possible Causes**:
1. Request rate exceeds backend limits
2. Multiple clients sharing same token

**Solutions**:
1. Add more backends to distribute load
2. Increase `failover.rate_limit.cooldown_seconds` to avoid hammering rate-limited backends
3. Use separate tokens for different clients
4. Reduce request frequency

### Issue: Intermittent 403/200 Responses

**Symptoms**: Sometimes requests succeed (200), sometimes fail (403)

**Possible Causes**:
1. **Multiple backends rotating**: First backend token invalid returns 403, but 403 doesn't trigger failover, so returned directly to client
2. **Token rate limiting**: A token exceeds rate limit and returns 403, recovers after waiting

**Troubleshooting**:
1. Check token preview in `[尝试 #N]` logs (e.g., `sk-a...xyz1`)
2. Check specific error information in `[错误详情]`
3. If different requests use different tokens, indicates switching between backends

**Solutions**:
- Disable backends with invalid tokens in config.json and restart the proxy
- Check and update invalid tokens
- If rate limiting issue, consider adding more backends or reducing request frequency

### Issue: All Backends Failed

**Troubleshooting Steps**:
1. Check network connection: `curl -I https://api.anthropic.com`
2. Verify token validity (check `[错误详情]` logs)
3. Confirm configuration file uses `token` field not `api_key`
4. Confirm backend URL is correct
5. Check if all backends are circuit-broken (check `[跳过]` logs)

### Issue: Request Timeout

**Symptoms**: Logs show `[超时]` and timeout errors

**Solutions**:
1. Increase `retry.timeout_seconds` configuration
2. Check network latency to backend
3. Check if backend is responding slowly
4. Consider adding faster backends

## Performance

- **Memory Usage**: About 10-20MB
- **Concurrency Support**: Go native concurrency, supports large number of concurrent connections
- **Latency**: Proxy forwarding latency < 5ms

## Tech Stack

- **Language**: Go 1.21+
- **Dependencies**:
  - `encoding/json` - JSON configuration parsing (standard library)
  - Go standard library (`net/http`, `compress/gzip`, etc.)
- **Features**:
  - Zero external dependencies
  - Automatic gzip decompression
  - Graceful shutdown support
  - No API version header, compatible with various backends
  - Per-backend model override support

## License

MIT License

## Contributing

Issues and Pull Requests are welcome.

## Related Links

- [Claude Code Official Documentation](https://code.claude.com/docs)
- [Anthropic API Documentation](https://docs.anthropic.com)
