# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project Overview

This is a lightweight Claude API failover proxy written in Go. It provides automatic failover between multiple Claude API backends with health checking and transparent request forwarding. When one backend fails, it automatically tries the next available backend without client intervention.

## Build and Run Commands

```bash
# Build the proxy
go build -o cc-proxy

# Run with default config
./cc-proxy

# Run with custom config
./cc-proxy -config config.json

# Build and run in one step
go run main.go -config config.json

# Tidy dependencies
go mod tidy
```

## Configuration

The proxy is configured via `config.json` (JSON format). Key configuration sections:

- **port**: Proxy server listening port (default: 8080)
- **backends**: Array of API backends with name, base_url, token, enabled flag, and optional model field. Backends are tried in order of priority.
  - **model** (optional): If specified, the proxy will override the "model" field in the request body with this value before forwarding to the backend
- **retry**: max_attempts and timeout_seconds for request handling

**Important**: The `token` field in config.json should contain the actual API token (not api_key). The proxy sets `Authorization: Bearer <token>` header when forwarding requests.

## Architecture

### Core Components

**ProxyServer** (main.go:41-44): Main server struct that handles:
- Configuration management
- HTTP client with timeout
- Request forwarding and failover logic

**Request Flow**:
1. Client request arrives at ServeHTTP (main.go:86)
2. Request body is read and buffered
3. Log request start with backend count
4. Iterate through enabled backends in priority order
5. Skip disabled backends (logged with reason)
6. Forward request via forwardRequest (main.go:160) with backend's token
7. If backend has model configured, override the "model" field in request body (logged as [模型覆盖])
8. Log all non-2xx responses with decompressed body content
9. On 5xx errors, 429 errors, or network failures, try next backend (failover)
10. On 3xx/4xx errors (except 429), return to client immediately (no failover)
11. On success, copy response back to client via copyResponse (main.go:254)
12. If all backends fail, return 502 Bad Gateway

### Key Implementation Details

- **Request Buffering**: Request body is fully read into memory to enable retries across backends
- **Header Forwarding**: All original request headers are preserved, except Authorization is replaced with backend token
- **No API Version Header**: Does not set `anthropic-version` header, allowing backends to use their default version
- **Graceful Shutdown**: Listens for SIGINT/SIGTERM and allows 10 seconds for in-flight requests to complete
- **Error Handling**: 5xx and 429 status codes trigger failover; other 3xx/4xx errors are returned immediately
- **Error Logging**: All non-2xx responses log the full response body (truncated to 500 chars) for debugging
- **Gzip Support**: Automatically detects and decompresses gzip-encoded responses via readResponseBody helper (main.go:237-251)
- **Token Preview**: Logs show first 4 and last 4 chars of token for debugging without exposing full token
- **Attempt Tracking**: Each request logs attempt number and which backend is being tried
- **Model Override**: If a backend has `model` configured, the proxy will parse the JSON request body and replace the "model" field before forwarding

## Usage with Claude Code

To use this proxy with Claude Code:

```bash
# Start the proxy
./cc-proxy -config config.json

# Configure Claude Code to use the proxy
export ANTHROPIC_BASE_URL=http://localhost:3456
export ANTHROPIC_API_KEY=dummy  # Proxy handles real tokens

# Run Claude Code
claude
```

The proxy transparently handles all API requests and automatically fails over between configured backends.

## Backend Management

Backends are configured in `config.json` and loaded at startup. To enable/disable backends or change configuration:

1. Edit `config.json` to modify backend settings (enabled/disabled, tokens, model overrides, etc.)
2. Restart the proxy for changes to take effect

Example configuration:
```json
{
  "port": 3456,
  "backends": [
    {
      "name": "backend1",
      "base_url": "https://api.example.com",
      "token": "sk-ant-xxx",
      "enabled": true,
      "model": "claude-3-5-sonnet-20241022"
    },
    {
      "name": "backend2",
      "base_url": "https://api.another.com",
      "token": "sk-ant-yyy",
      "enabled": false
    }
  ],
  "retry": {
    "max_attempts": 3,
    "timeout_seconds": 30
  }
}
```

## Code Modification Guidelines

- When adding new configuration options, update both the Config struct and the validation logic in NewProxyServer
- All response body reading must use `readResponseBody()` helper to handle gzip compression automatically
- When backends return non-2xx responses, the response body is logged for debugging (see forwardRequest function)
- The proxy preserves all request/response headers and body content for transparency
- Logging uses Chinese characters - maintain this convention for consistency
- All backend tokens are stored in config.json and injected during request forwarding

## Debugging Tips

When investigating backend failures:
1. Check `[请求开始]` logs to see how many backends are configured
2. Check `[尝试 #N]` logs to see which backend is being tried and which token (preview)
3. Check `[模型覆盖]` logs to see if the request model is being overridden by backend config
4. Check `[跳过]` logs to see if backends are disabled
5. Check `[错误详情]` logs for all non-2xx responses with decompressed error messages
6. Check `[成功 #N]` or `[失败 #N]` to see final outcome
7. Gzip-compressed responses are automatically decompressed for readable logs
8. Token previews show first/last 4 chars (e.g., "sk-a...xyz1") to identify which token without exposing it

## Common Issues

**Intermittent 403/200 responses**:
- Check logs to see if different backends are being used (token preview will differ)
- 403 does NOT trigger failover - only 5xx and 429 errors do
- If one backend has invalid token (403), it will always return 403
- Disable failing backends in config.json and restart the proxy

**Gzip decompression errors**:
- All response reading uses `readResponseBody()` which handles gzip automatically
- If you see "创建 gzip reader 失败", the response claims to be gzip but isn't valid
