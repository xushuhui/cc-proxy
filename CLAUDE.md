# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project Overview

This is a lightweight Claude API failover proxy written in Go. It provides automatic failover between multiple API backends (Claude and OpenAI compatible) with circuit breaker, rate limit handling, and transparent request forwarding. When one backend fails, it automatically tries the next available backend without client intervention.

**Key Features:**
- Automatic failover on 5xx/429 errors
- Circuit breaker to prevent repeated requests to failed backends
- Rate limit handling with cooldown periods
- Support for both Claude API and OpenAI API backends
- Bidirectional format conversion between Anthropic and OpenAI APIs
- Compression support (gzip and zstd) with automatic detection
- Streaming and non-streaming response handling
- Path forwarding (e.g., `/v1/messages` → `/v1/chat/completions` for OpenAI)

## Build and Run Commands

**Requirements:**
- Go 1.23 or higher

```bash
# Build the proxy
go build -o cc-proxy

# Run with default config (config.json)
./cc-proxy

# Run with custom config
./cc-proxy -config config.json

# Build and run in one step
go run main.go -config config.json

# Add/update dependencies
go mod tidy
go get <package>

# Format code (all code in single package)
go fmt

# Verify dependencies
go mod verify
```

## Configuration

The proxy is configured via `config.json` (JSON format). See `config.example.json` for a template.

**Required fields:**
- `port`: Proxy server listening port
- `backends`: Array of API backend configurations

**Backend configuration:**
- `name`: Backend identifier (used in logs)
- `base_url`: API base URL (can include path, e.g., `https://api.example.com` or `https://api.openai.com/v1`)
- `token`: Actual API token (proxy sets `Authorization: Bearer <token>`)
- `enabled`: Whether to use this backend
- `platform`: Platform type - "anthropic" (default) or "openai"
- `model` (optional): Override the "model" field in requests

**Important**: `base_url` can include path components. The proxy will append the client request path to the base URL path. For example:
- `base_url: "https://api.example.com"` + request `/v1/messages` = `https://api.example.com/v1/messages`
- `base_url: "https://api.openai.com/v1"` + request `/v1/messages` = `https://api.openai.com/v1/v1/messages`

**For OpenAI backends**: Set `platform: "openai"` and the proxy will automatically handle path forwarding:
- Client requests `/v1/messages` → automatically forwarded to `/v1/chat/completions`
- Use base URL without the endpoint: `base_url: "https://api.openai.com"`
- The proxy will construct the final URL as: `https://api.openai.com/v1/chat/completions`

**Optional configuration:**
- `retry.timeout_seconds`: Request timeout for non-streaming requests
- `failover.circuit_breaker`: Circuit breaker settings (failure_threshold, open_timeout_seconds, half_open_requests)
- `failover.rate_limit`: Rate limit handling (cooldown_seconds)

## Architecture

### File Structure

**All code is in a single package (`main`):**
- **main.go**: Server initialization, startup, graceful shutdown
- **config.go**: Configuration structs (Backend, Config)
- **config_loader.go**: JSON config file loading
- **proxy.go**: Core request handling, response forwarding, compression handling, format conversion
- **circuit_breaker.go**: Circuit breaker and rate limit state management

**Key dependencies:**
- `github.com/klauspost/compress/zstd`: zstd compression support
- Go standard library for everything else (no external HTTP frameworks)

**Note:** This project currently has no test suite. When adding new features, consider writing tests to verify behavior, especially for:
- Circuit breaker state transitions (circuit_breaker.go)
- Request/response format conversion (proxy.go)
- Request/response handling edge cases

### Request Flow

1. **ServeHTTP** (proxy.go): Entry point, reads request body
2. **CircuitBreaker.SortBackendsByPriority**: Sorts backends (non-rate-limited first, then by config order)
3. For each backend:
   - **CircuitBreaker.ShouldSkipBackend**: Check if backend should be skipped (disabled, circuit open, rate limit cooldown)
   - **forwardRequest**: Forward request to backend
     - Detects platform type from `platform` field (default: "anthropic")
     - For OpenAI backends: converts Anthropic format → OpenAI format
     - Detects streaming requests (`"stream": true` in request body)
     - Adds timeout context for non-streaming requests only
     - Replaces `Authorization` header with backend's token
   - On error/429/5xx: **CircuitBreaker.RecordFailure/Record429**, try next backend
   - On success: **CircuitBreaker.RecordSuccess**, convert response if needed, return to client
4. If all backends fail: Return 502 Bad Gateway

### Format Conversion

**Anthropic ↔ OpenAI Conversion:**
- **Request conversion**: Anthropic `/v1/messages` format → OpenAI `/v1/chat/completions` format
- **Response conversion**: OpenAI response format → Anthropic response format
- **Streaming support**: Real-time conversion of streaming responses
- **Multi-modal support**: Text, images, and tool calls

**Key conversion functions:**
- `convertAnthropicToOpenAI()`: Converts Anthropic requests to OpenAI format
- `convertOpenAIToAnthropic()`: Converts OpenAI responses to Anthropic format
- Handles system messages, user/assistant messages, tool calls, and tool results

### Response Handling

The proxy handles response conversion based on platform type:
- **Anthropic backends**: Direct passthrough with minimal overhead
- **OpenAI backends**: Convert OpenAI format → Anthropic format before returning to client

### Circuit Breaker (circuit_breaker.go)

**States per backend:**
- **Closed**: Normal operation, requests allowed
- **Open**: After `failure_threshold` consecutive failures, blocks requests for `open_timeout_seconds`
- **Half-Open**: After open timeout, allows `half_open_requests` test requests to see if backend recovered

**Rate limiting:**
- Tracks 429 errors per backend
- Enforces `cooldown_seconds` before retrying rate-limited backends
- Supports `Retry-After` header from backend

### Compression Handling (proxy.go:readResponseBody)

**Automatic detection and decompression:**
- gzip via `Content-Encoding: gzip` header
- zstd via `Content-Encoding: zstd` header
- Fallback detection via magic bytes (gzip: `1f 8b`, zstd: `28 b5 2f fd`)

Applied to all response body reading paths.

### Timeout Handling

**Critical distinction:**
- **Non-streaming requests**: Use `context.WithTimeout` (configurable via `retry.timeout_seconds`)
- **Streaming requests**: No timeout (would kill long-lived streams)

Detected by parsing request body for `"stream": true` field.

### Logging

All logs use Chinese for consistency. Key log prefixes:
- `[请求开始]`: Request received, backend count
- `[跳过]`: Backend skipped (with reason)
- `[尝试 #N]`: Which backend being tried
- `[超时设置]`: Timeout configuration
- `[模型覆盖]`: Model override applied
- `[格式转换]`: Request/response format conversion
- `[格式转换失败]`: Format conversion error
- `[响应转换]`: Response format conversion
- `[错误详情]`: Non-2xx response with body preview
- `[成功 #N]`: Backend returned 2xx success
- `[返回客户端]`: Backend returned 4xx client error (not retried)
- `[失败 #N]`: Backend failed with error or 5xx/429 (will retry next backend)
- `[流式开始/内容/完成]`: Streaming response details
- `[readResponseBody]`: Compression detection and decompression

## Important Implementation Details

### Request Buffering
Request body is fully read into memory to enable retries across backends. Trade-off: higher memory usage for reliable failover.

### Streaming Response Handling
For streaming responses, the proxy:
- **Anthropic backends**: Passes through responses directly without modification
- **OpenAI backends**: Converts OpenAI streaming format to Anthropic streaming format in real-time

### Token Management
- `token` field in config contains actual API token
- Logs show token preview (first 4 + "..." + last 4 chars) for debugging
- Original `Authorization` header from client is replaced with backend's token

### Model Override
If backend has `model` configured:
- Proxy parses JSON request body
- Replaces `model` field with backend's configured value
- Logged as `[模型覆盖]`

### Path Forwarding for OpenAI
**Automatic path forwarding**: The proxy automatically handles path conversion for OpenAI backends:
- ✅ Client requests `/v1/messages` → automatically forwarded to `/v1/chat/completions`
- ✅ Use simple base URL: `base_url: "https://api.openai.com"`
- ✅ Final URL: `https://api.openai.com/v1/chat/completions`

The proxy handles this conversion automatically when `platform: "openai"` is set.

## Common Issues

**Streaming responses timeout:**
- Symptom: `context deadline exceeded` during streaming
- Cause: Global http.Client.Timeout was killing streams
- Fix: Removed global timeout, only apply to non-streaming requests via context

**Circuit breaker not opening:**
- Requires `failure_threshold` consecutive failures
- Check `[跳过]` logs to see if circuit is open
- Failures must be from actual backend errors (5xx), not client errors (4xx except 429)

**OpenAI format conversion errors:**
- Check `[格式转换失败]` logs for conversion errors
- Ensure OpenAI backend is properly configured with `platform: "openai"`
- Verify base_url includes the correct endpoint path

## Code Modification Guidelines

- **All response body reading** must use `readResponseBody()` helper (handles compression)
- **New configuration options**: Update Config struct in config.go and validation in NewProxyServer
- **Logging**: Maintain Chinese convention for consistency with existing logs
- **Circuit breaker logic**: Modify circuit_breaker.go if changing failover behavior
- **Timeout handling**: Be aware of streaming vs non-streaming distinction
- **URL path handling**: When modifying URL construction, use `targetURL.Path = targetURL.Path + originalReq.URL.Path` to preserve base_url paths
- **Format conversion**: Test conversion logic with both streaming and non-streaming requests

## Development Workflow

When making changes to the proxy:

1. **Edit code**: All source files are in the root directory
2. **Test locally**: Use `go run main.go -config config.json` for quick testing
3. **Build**: Use `go build -o cc-proxy` to create the binary
4. **Format**: Run `go fmt` to format code (all files in single package)
5. **Verify**: Run `go mod verify` to ensure dependency integrity

**Debugging tips:**
- Set `enabled: false` for failing backends to isolate issues
- Use the detailed Chinese logs to trace request flow through backends
- Check token previews in logs to verify correct backend is being used
- Monitor circuit breaker state transitions in logs
- For OpenAI backends, verify `[格式转换]` and `[响应转换]` logs
- The Authorization header in error logs is partially masked for security (shows first 15 and last 5 chars)
