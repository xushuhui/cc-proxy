# CC Proxy

A lightweight Claude API reverse proxy with automatic failover support for multiple API keys. When one backend fails, it automatically switches to the next available backend, completely transparent to clients.

[中文文档](README-cn.md)

## Features

- **Automatic Failover**: Automatically tries backup keys when primary API key fails (only 5xx/429 errors trigger failover)
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

**Important**:
- The configuration field is `token`, not `api_key`
- The `model` field is optional - if specified, the proxy will override the model in the request body

### 3. Start Proxy

```bash
./cc-proxy -config config.json
```

Example output:
```
2024/12/26 12:00:00 Claude API Failover Proxy Starting...
2024/12/26 12:00:00 Listening Port: 3456
2024/12/26 12:00:00 Configured Backends:
2024/12/26 12:00:00   1. Anthropic Official - https://api.anthropic.com [Enabled]
2024/12/26 12:00:00   2. Backup Provider - https://api.backup.example.com [Enabled]
2024/12/26 12:00:00 Max Retry Attempts: 3
2024/12/26 12:00:00 Request Timeout: 30 seconds

2024/12/26 12:00:00 ✓ Proxy server running at http://localhost:3456
2024/12/26 12:00:00 ✓ Configure Claude Code: export ANTHROPIC_BASE_URL=http://localhost:3456
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
| `retry.max_attempts` | Maximum retry attempts | 3 |
| `retry.timeout_seconds` | Single request timeout (seconds) | 30 |

## How It Works

1. **Request Reception**: Proxy receives Claude Code API requests
2. **Backend Selection**: Selects first enabled backend in order
3. **Model Override**: If backend has `model` configured, replaces the model field in request body
4. **Request Forwarding**: Forwards request to selected backend, replaces Authorization header with backend's token
5. **Failover**: If request fails (5xx/429 error or network error), automatically tries next backend
6. **Response Return**: Returns backend response completely to client

**Important**: Only 5xx/429 errors and network errors trigger failover. Other 3xx/4xx errors (like 403 authentication failure) are returned directly to the client without trying other backends.

```
Claude Code → Local Proxy → Backend1 (500 error)
                     ↓
                    Backend2 (success) → Response
```

## Logging

The proxy outputs detailed request logs to help you understand the request processing:

### Request Processing Logs

```
[Request Start] POST /v1/messages - Will try 3 backends
[Attempt #1] Anthropic Official - POST https://api.anthropic.com/v1/messages (token: sk-a...xyz1)
[Model Override] Anthropic Official - Using configured model: claude-3-5-sonnet-20241022
[Error Details] Anthropic Official - HTTP 500 - Response: {"error":{"type":"internal_error","message":"Service temporarily unavailable"}}
[Failed #1] Anthropic Official - Backend returned error: HTTP 500
[Attempt #2] Backup Provider - POST https://api.backup.com/v1/messages (token: sk-b...abc2)
[Success #2] Backup Provider - HTTP 200
```

### Log Features

- **Token Security**: Only shows first 4 and last 4 characters of token (e.g., `sk-a...xyz1`) to avoid leaking complete keys
- **Attempt Numbering**: Shows `#1`, `#2`, etc., clearly see which backends were tried
- **Error Details**: All non-2xx responses show complete error information (automatically decompresses gzip)
- **Skip Reason**: Shows reason why backend was skipped (disabled)
- **Model Override**: Shows when request model is overridden by backend configuration

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

### Issue: Intermittent 403/200 Responses

**Symptoms**: Sometimes requests succeed (200), sometimes fail (403)

**Possible Causes**:
1. **Multiple backends rotating**: First backend token invalid returns 403, but 403 doesn't trigger failover, so returned directly to client
2. **Token rate limiting**: A token exceeds rate limit and returns 403, recovers after waiting

**Troubleshooting**:
1. Check token preview in `[Attempt #N]` logs (e.g., `sk-a...xyz1`)
2. Check specific error information in `[Error Details]`
3. If different requests use different tokens, indicates switching between backends

**Solutions**:
- Disable backends with invalid tokens in config.json and restart the proxy
- Check and update invalid tokens
- If rate limiting issue, consider adding more backends or reducing request frequency

### Issue: All Backends Failed

**Troubleshooting Steps**:
1. Check network connection: `curl -I https://api.anthropic.com`
2. Verify token validity (check `[Error Details]` logs)
3. Confirm configuration file uses `token` field not `api_key`
4. Confirm backend URL is correct

### Issue: Request Timeout

**Solutions**:
1. Increase `retry.timeout_seconds` configuration
2. Check network latency
3. Check if backend responds slowly

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
