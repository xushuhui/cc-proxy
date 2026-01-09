# CC Proxy

A lightweight Claude API reverse proxy with automatic failover support, intelligent circuit breaker, and rate limit handling. When one backend fails, it automatically switches to the next available backend, completely transparent to clients.

[中文文档](README.zh.md)

## Features

### Core Functionality
- **Automatic Failover**: Automatically tries backup keys when primary API key fails (only 5xx/429 errors trigger failover)
- **Circuit Breaker**: Smart circuit breaker prevents repeated requests to failing backends
- **Rate Limit Handling**: Intelligent 429 error handling with cooldown and Retry-After header support
- **Timeout Handling**: Non-streaming requests timeout triggers failover, streaming requests have no timeout limit

### API Support
- **Claude API Backends**: Native support for Claude API format and compatible endpoints

### Compression & Transmission
- **Smart Compression Handling**: Automatically detects and decompresses gzip and zstd compressed responses
- **Streaming Response Support**: Fully supports both streaming and non-streaming responses
- **Transparent Proxy**: Fully forwards all HTTP requests and response headers

### Development & Operations
- **Flexible Configuration**: JSON configuration file, supports multiple backends, timeout, retry settings
- **Detailed Logging**: All requests and responses have detailed logs, including real-time streaming content display
- **Secure Logging**: Tokens only show first and last 4 characters to avoid leaking complete keys
- **Model Override**: Optional per-backend model override to force specific model versions
- **Zero Dependencies**: Single binary after compilation, no additional dependencies required

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
      "name": "Backup Claude Provider",
      "base_url": "https://api.backup.example.com",
      "token": "your-backup-key",
      "enabled": true
    }
  ],
  "retry": {
    "max_attempts": 3,
    "timeout_seconds": 30
  },
  "failover": {
    "circuit_breaker": {
      "failure_threshold": 3,
      "open_timeout_seconds": 30,
      "half_open_requests": 1
    },
    "rate_limit": {
      "cooldown_seconds": 60
    }
  }
}
```

**Configuration Notes**:
- `token`: API token (note: token not api_key)
- `model`: Optional model override to force specific model
- `failover`: Optional failover configuration with sensible defaults

### 3. Start Proxy

```bash
./cc-proxy -config config.json
```

Example output:
```
2026/01/07 14:00:00 Claude API Failover Proxy Starting...
2026/01/07 14:00:00 Listening on port: 3456
2026/01/07 14:00:00 Configured backends:
2026/01/07 14:00:00   1. Anthropic Official - https://api.anthropic.com [Enabled]
2026/01/07 14:00:00   2. Backup Provider - https://api.backup.com [Enabled]
2026/01/07 14:00:00 Max retry attempts: 3
2026/01/07 14:00:00 Request timeout: 30 seconds
2026/01/07 14:00:00 Circuit breaker: 3 failures trigger, 30 second open timeout
2026/01/07 14:00:00 Rate limit: 60 second cooldown after 429 errors

2026/01/07 14:00:00 ✓ Proxy running at http://localhost:3456
2026/01/07 14:00:00 ✓ Configure Claude Code: export ANTHROPIC_BASE_URL=http://localhost:3456
```

### 4. Configure Claude Code

```bash
# Set Claude Code to use local proxy
export ANTHROPIC_BASE_URL=http://localhost:3456
export ANTHROPIC_API_KEY=dummy  # Proxy handles real tokens

# Start Claude Code
claude
```

## Configuration

### Backend Configuration

Each backend supports the following configuration:

| Config | Description | Required | Default |
|--------|-------------|----------|---------|
| `name` | Backend name (for logging) | Yes | - |
| `base_url` | API base URL | Yes | - |
| `token` | API Token | Yes | - |
| `enabled` | Whether enabled | Yes | - |
| `model` | Model override (optional) | No | - |

Backends are tried in order of priority. Failed backends automatically trigger the next backend.

### Retry & Timeout Configuration

| Config | Description | Default |
|--------|-------------|---------|
| `retry.max_attempts` | Maximum retry attempts (unused in current version) | 3 |
| `retry.timeout_seconds` | Non-streaming request timeout (seconds) | 30 |

**Important**: Timeout configuration only applies to non-streaming requests. Streaming requests (`stream: true`) have no timeout limit to avoid long-running generations being interrupted.

### Failover Configuration

| Config | Description | Default |
|--------|-------------|---------|
| `failover.circuit_breaker.failure_threshold` | Consecutive failures to trigger circuit breaker | 3 |
| `failover.circuit_breaker.open_timeout_seconds` | How long circuit stays open (seconds) | 30 |
| `failover.circuit_breaker.half_open_requests` | Number of test requests in half-open state | 1 |
| `failover.rate_limit.cooldown_seconds` | Cooldown time after 429 rate limit (seconds) | 60 |

**Circuit Breaker States**:
- **Closed (Normal)**: All requests go through normally
- **Open (Circuit Tripped)**: Backend is skipped after N consecutive failures
- **Half-Open (Testing)**: After timeout expires, allows limited test requests to check if backend recovered

## How It Works

### Request Processing Flow

1. **Request Reception**: Proxy receives client's API request
2. **Backend Priority Sorting**:
   - Normal state backends have priority
   - Rate-limited backends are secondary
   - Circuit-open backends are last
3. **Attempt Each Backend**:
   - Check if backend should be skipped (disabled/circuit-open/rate-limited)
   - Detect request type (streaming/non-streaming)
   - Add appropriate timeout control (non-streaming only)
   - Forward request to backend
4. **Error Handling & Failover**:
   - **5xx errors**: Record failure, trigger circuit breaker if threshold reached, try next backend
   - **429 rate limit**: Record rate limit timestamp, enter cooldown, try next backend
   - **Timeout**: Record failure, try next backend
   - **401/403**: Return immediately without retry (authentication error)
   - **Other 4xx**: Return immediately without retry (client error)
5. **Response Processing**:
   - Automatically decompress gzip/zstd compressed responses
   - Return to client

```
Request → Backend Priority Sorting
      ↓
  Backend1 (circuit open - skip)
      ↓
  Backend2 (500 error → record failure → try next)
      ↓
  Backend3 (429 → record rate limit → try next)
      ↓
  Backend4 (success) → Return to client
```

### Compression Handling

Proxy automatically handles the following compression formats:

1. **gzip Compression**
   - Detected via `Content-Encoding: gzip` response header
   - Automatically decompresses and logs

2. **zstd Compression**
   - Detected via `Content-Encoding: zstd` response header
   - Uses efficient zstd decompressor

3. **Magic Byte Detection**
   - Even when response doesn't declare compression
   - Automatically recognizes via magic bytes:
     - gzip: `1f 8b`
     - zstd: `28 b5 2f fd`

## Logging

### Request Processing Logs

```
[请求开始] POST /v1/messages - Configured 3 backends
[跳过] Backend1 - Circuit opened (25s remaining)
[尝试 #1] Backend2 - POST https://api.anthropic.com/v1/messages (token: sk-a...xyz1)
[超时设置] Backend2 - Non-streaming request, 30s timeout
[模型覆盖] Backend2 - Using configured model: claude-3-5-sonnet-20241022
[错误详情] Backend2 - HTTP 500 - Response: {"error":{"type":"internal_error"}}
[失败 #1] Backend2 - Backend error: HTTP 500
[熔断触发] Backend2 - 3 consecutive failures, circuit opened for 30s
[尝试 #2] Backend3 - POST https://api.backup.com/v1/messages (token: sk-b...abc2)
[成功 #2] Backend3 - HTTP 200
[复制响应] Written 1523 bytes to client
```

### Streaming Response Logs

```
[流式开始] Backend1 - Started receiving streaming response
[流式事件] Backend1 - message_start
[流式内容 #1] Backend1 - "Hello"
[流式内容 #2] Backend1 - "! How"
[流式内容 #3] Backend1 - " can I"
[流式事件 #4] Backend1 - type: content_block_stop
[流式结束] Backend1 - Received [DONE] marker
[完整内容] Backend1 - Hello! How can I help you today?
[流式完成] Backend1 - Processing complete (15 lines, 8 data blocks, 31 total chars)
```

### Format Conversion Logs

```
[readResponseBody] Content-Encoding header: 'zstd'
[readResponseBody] Detected zstd compression, attempting decompression
[readResponseBody] zstd decompressor created successfully
[readResponseBody] Read 532 bytes of data
```

### Circuit Breaker and Rate Limit Logs

**Circuit Breaker**:
```
[熔断触发] Backend1 - 3 consecutive failures, circuit opened for 30s (HTTP 502)
[跳过] Backend1 - Circuit opened (25s remaining)
[尝试 #1] Backend1 - ... [circuit test 1/1]
[熔断恢复] Backend1 - Backend recovered
```

**Rate Limit**:
```
[限流记录] Backend2 - 429 triggered, Retry-After: 60s
[限流记录] Backend3 - 429 triggered, 60s cooldown
```

### Log Features

- **Token Security**: Only shows first 4 and last 4 characters of token (e.g., `sk-a...xyz1`)
- **Real-time Streaming Content**: Each content block from streaming responses displays in real-time
- **Complete Content Accumulation**: Shows full response text after streaming completes
- **Compression Detection**: Detailed logging of compression format and decompression process
- **Error Details**: All non-2xx responses show complete error information

## Advanced Usage

### Run as System Service

Create systemd service file `/etc/systemd/system/cc-proxy.service`:

```ini
[Unit]
Description=CC Proxy - Claude API Failover Service
After=network.target

[Service]
Type=simple
User=your-username
WorkingDirectory=/path/to/claude-proxy
ExecStart=/path/to/claude-proxy/cc-proxy -config /path/to/claude-proxy/config.json
Restart=on-failure

[Install]
WantedBy=multi-user.target
```

Start service:

```bash
sudo systemctl daemon-reload
sudo systemctl enable cc-proxy
sudo systemctl start cc-proxy
sudo systemctl status cc-proxy
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

### Test Proxy

**Test non-streaming request**:
```bash
curl -X POST http://localhost:3456/v1/messages \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer dummy" \
  -d '{
    "model": "claude-sonnet",
    "messages": [{"role": "user", "content": "Hello"}],
    "max_tokens": 1000
  }'
```

**Test streaming request**:
```bash
curl -X POST http://localhost:3456/v1/messages \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer dummy" \
  -d '{
    "model": "claude-sonnet",
    "messages": [{"role": "user", "content": "Hello"}],
    "max_tokens": 1000,
    "stream": true
  }'
```

## Troubleshooting

### Issue: Streaming Response Timeout Interruption

**Symptoms**: Logs show `context deadline exceeded` or streaming stops midway

**Cause**: Global timeout in older versions kills long-running streaming responses

**Solution**: Fixed in newer version. Ensure using latest code - streaming requests are no longer subject to timeout limits.

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

### Issue: Compressed Response Unreadable

**Symptoms**: Logs show `[二进制数据,长度: XXX 字节]`, response content is gibberish

**Possible Causes**:
1. Backend returns compressed response without declaring `Content-Encoding` header
2. Proxy doesn't correctly recognize compression format

**Troubleshooting**:
1. Check `Content-Encoding` header in `[readResponseBody]` logs
2. Verify if there are logs mentioning `检测到 gzip/zstd 压缩`

**Solution**:
- New version supports magic byte auto-detection, no declaration needed
- If issues persist, check logs to confirm compression format

### Issue: Too Many 429 Rate Limit Errors

**Symptoms**: Logs show `[限流记录]` frequently

**Solutions**:
1. Add more backends to distribute load
2. Increase `failover.rate_limit.cooldown_seconds` to avoid hammering rate-limited backends
3. Use separate tokens for different clients
4. Reduce request frequency

### Issue: All Backends Failed

**Troubleshooting Steps**:
1. Check network connection: `curl -I https://api.anthropic.com`
2. Verify token validity (check `[错误详情]` logs)
3. Confirm configuration file uses `token` field not `api_key`
4. Confirm backend URL is correct
5. Check if all backends are circuit-broken (check `[跳过]` logs)

## Performance

- **Memory Usage**: About 10-20MB
- **Concurrency Support**: Go native concurrency, supports large number of concurrent connections
- **Latency**: Proxy forwarding latency < 5ms
- **Compression Performance**: zstd decompression is 2-5x faster than gzip

## Tech Stack

- **Language**: Go 1.21+
- **Core Dependencies**:
  - `github.com/klauspost/compress/zstd` - zstd compression support
  - Go standard library (`net/http`, `compress/gzip`, `encoding/json`, etc.)
- **Features**:
  - Automatic gzip/zstd decompression
  - Graceful shutdown support (SIGINT/SIGTERM)
  - Circuit breaker and rate limit state management
  - Real-time streaming response processing

## License

MIT License

## Contributing

Issues and Pull Requests are welcome.

## Related Links

- [Claude Code Official Documentation](https://code.claude.com/docs)
- [Anthropic API Documentation](https://docs.anthropic.com)