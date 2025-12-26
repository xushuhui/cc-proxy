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
- **backends**: Array of API backends with name, base_url, token, and enabled flag. Backends are tried in order of priority.
- **retry**: max_attempts and timeout_seconds for request handling
- **health_check**: enabled flag and interval_seconds for periodic backend health checks

**Important**: The `token` field in config.json should contain the actual API token (not api_key). The proxy sets `Authorization: Bearer <token>` header when forwarding requests.

## Architecture

### Core Components

**ProxyServer** (main.go:44-50): Main server struct that handles:
- Configuration management
- HTTP client with timeout
- Health status tracking with mutex-protected healthMap
- Request forwarding and failover logic

**Request Flow**:
1. Client request arrives at ServeHTTP (main.go:201)
2. Request body is read and buffered
3. Log request start with backend count
4. Iterate through enabled backends in priority order
5. Skip disabled or unhealthy backends (logged with reason)
6. Forward request via forwardRequest (main.go:261) with backend's token
7. Log all non-2xx responses with decompressed body content
8. On 5xx errors or network failures, try next backend (failover)
9. On 3xx/4xx errors, return to client immediately (no failover)
10. On success, copy response back to client via copyResponse (main.go:346)
11. If all backends fail, return 502 Bad Gateway

**Health Checking** (main.go:147-199):
- Runs in background goroutine if enabled
- Uses HTTP GET requests to `/v1/models` endpoint with actual authentication tokens
- Considers 2xx/3xx/4xx responses as healthy (4xx means service is online but request has issues)
- Only 5xx errors mark backend as unhealthy
- Automatically handles gzip-compressed responses for accurate logging
- Logs detailed error responses for debugging (truncated to 200 chars)
- Updates healthMap for each backend
- Automatically skips unhealthy backends during request handling
- Note: `/v1/models` is commonly supported by OpenAI-compatible Claude API proxies

### Key Implementation Details

- **Thread Safety**: healthMap is protected by RWMutex for concurrent access
- **Request Buffering**: Request body is fully read into memory to enable retries across backends
- **Header Forwarding**: All original request headers are preserved, except Authorization is replaced with backend token
- **No API Version Header**: Does not set `anthropic-version` header, allowing backends to use their default version
- **Graceful Shutdown**: Listens for SIGINT/SIGTERM and allows 60 seconds for in-flight requests to complete
- **Error Handling**: Only 5xx status codes trigger failover; 3xx/4xx errors are returned immediately
- **Error Logging**: All non-2xx responses log the full response body (truncated to 500 chars) for debugging
- **Gzip Support**: Automatically detects and decompresses gzip-encoded responses via readResponseBody helper (main.go:328-343)
- **Token Preview**: Logs show first 4 and last 4 chars of token for debugging without exposing full token
- **Attempt Tracking**: Each request logs attempt number and which backend is being tried

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

## Code Modification Guidelines

- When adding new configuration options, update both the Config struct and the validation logic in NewProxyServer
- Health check logic uses HTTP GET requests to `/health` endpoint with authentication - this accurately reflects API availability
- All response body reading must use `readResponseBody()` helper to handle gzip compression automatically
- When backends return non-2xx responses, the response body is logged for debugging (see forwardRequest function)
- The proxy preserves all request/response headers and body content for transparency
- Logging uses Chinese characters - maintain this convention for consistency
- All backend tokens are stored in config.json and injected during request forwarding

## Debugging Tips

When investigating backend failures:
1. Check `[请求开始]` logs to see how many backends are configured
2. Check `[尝试 #N]` logs to see which backend is being tried and which token (preview)
3. Check `[跳过]` logs to see if backends are disabled or marked unhealthy
4. Check `[错误详情]` logs for all non-2xx responses with decompressed error messages
5. Check `[成功 #N]` or `[失败 #N]` to see final outcome
6. Check `[健康检查]` logs for periodic health status updates
7. Gzip-compressed responses are automatically decompressed for readable logs
8. Token previews show first/last 4 chars (e.g., "sk-a...xyz1") to identify which token without exposing it

## Common Issues

**Intermittent 403/200 responses**:
- Check logs to see if different backends are being used (token preview will differ)
- 403 does NOT trigger failover - only 5xx errors do
- If one backend has invalid token (403), it will always return 403 unless marked unhealthy
- Enable health_check to automatically skip failing backends

**Gzip decompression errors**:
- All response reading uses `readResponseBody()` which handles gzip automatically
- If you see "创建 gzip reader 失败", the response claims to be gzip but isn't valid
