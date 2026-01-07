# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project Overview

This is a lightweight Claude API failover proxy written in Go. It provides automatic failover between multiple Claude API backends with circuit breaker, rate limit handling, and transparent request forwarding. When one backend fails, it automatically tries the next available backend without client intervention.

**Key Features:**
- Automatic failover on 5xx/429 errors
- Circuit breaker to prevent repeated requests to failed backends
- Rate limit handling with cooldown periods
- Support for both Claude and OpenAI API backends with automatic format conversion
- Compression support (gzip and zstd) with automatic detection
- Streaming and non-streaming response handling

## Build and Run Commands

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
```

## Configuration

The proxy is configured via `config.json` (JSON format):

**Required fields:**
- `port`: Proxy server listening port
- `backends`: Array of API backend configurations

**Backend configuration:**
- `name`: Backend identifier (used in logs)
- `base_url`: API base URL
- `token`: Actual API token (proxy sets `Authorization: Bearer <token>`)
- `enabled`: Whether to use this backend
- `api_type` (optional): API type - "claude" (default) or "openai"
- `model` (optional): Override the "model" field in requests

**Optional configuration:**
- `retry.timeout_seconds`: Request timeout for non-streaming requests
- `failover.circuit_breaker`: Circuit breaker settings (failure_threshold, open_timeout_seconds, half_open_requests)
- `failover.rate_limit`: Rate limit handling (cooldown_seconds)

## Architecture

### File Structure

- **main.go**: Server initialization, startup, graceful shutdown
- **config.go**: Configuration structs (Backend, Config)
- **config_loader.go**: JSON config file loading
- **proxy.go**: Core request handling, response forwarding, compression handling
- **converter.go**: Claude ↔ OpenAI format conversion for requests/responses
- **circuit_breaker.go**: Circuit breaker and rate limit state management

### Request Flow

1. **ServeHTTP** (proxy.go): Entry point, reads request body
2. **CircuitBreaker.SortBackendsByPriority**: Sorts backends (non-rate-limited first, then by config order)
3. For each backend:
   - **CircuitBreaker.ShouldSkipBackend**: Check if backend should be skipped (disabled, circuit open, rate limit cooldown)
   - **forwardRequest**: Forward request to backend
     - Detects streaming requests (`"stream": true` in request body)
     - Converts request format if `api_type == "openai"`
     - Adds timeout context for non-streaming requests only
     - Replaces `Authorization` header with backend's token
   - On error/429/5xx: **CircuitBreaker.RecordFailure/Record429**, try next backend
   - On success: **CircuitBreaker.RecordSuccess**, return response to client
4. If all backends fail: Return 502 Bad Gateway

### Response Handling

Two paths based on `api_type`:

**Claude backends** (`api_type == "claude"` or unset):
- **copyResponse**: Direct passthrough with minimal overhead
- **convertStreamingResponse**: For streaming, passthrough with detailed logging

**OpenAI backends** (`api_type == "openai"`):
- **copyAndConvertResponse**:
  - Non-streaming: Read body, decompress (gzip/zstd), convert OpenAI → Claude format, send to client
  - Streaming: **convertStreamingResponse** with chunk-by-chunk conversion

### Format Conversion (converter.go)

- **convertClaudeToOpenAI**: Transforms Claude request format to OpenAI format
  - Converts `messages` array (Claude's content array → OpenAI's string/array)
  - Maps Claude model names to OpenAI equivalents
  - Moves `system` field to system message in `messages` array

- **convertOpenAIToClaude**: Transforms OpenAI response format to Claude format
  - Converts `choices` array → `content` array
  - Maps `finish_reason` → `stop_reason`
  - Converts `usage` fields
  - Auto-detects if response is already Claude format and passes through

- **convertOpenAIStreamToClaude**: Converts streaming SSE chunks

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
- `[格式转换]`: Request format conversion
- `[模型覆盖]`: Model override applied
- `[错误详情]`: Non-2xx response with body preview
- `[成功 #N]` / `[失败 #N]`: Final outcome
- `[流式开始/内容/完成]`: Streaming response details
- `[readResponseBody]`: Compression detection and decompression

## Important Implementation Details

### Request Buffering
Request body is fully read into memory to enable retries across backends. Trade-off: higher memory usage for reliable failover.

### Streaming Response Handling
For Claude API backends with streaming responses, the proxy:
- Passes through SSE events and data chunks
- Logs each content chunk with actual text content
- Accumulates full response text for final log
- Handles both standard Claude format and auto-detects OpenAI format

### OpenAI Backend Support
When `api_type: "openai"`:
- Requests are converted from Claude → OpenAI format before sending
- Responses are converted from OpenAI → Claude format before returning to client
- Conversion is bidirectional for both streaming and non-streaming
- Auto-detects if backend returns Claude format despite `api_type: "openai"` config

### Token Management
- `token` field in config contains actual API token
- Logs show token preview (first 4 + "..." + last 4 chars) for debugging
- Original `Authorization` header from client is replaced with backend's token

### Model Override
If backend has `model` configured:
- Proxy parses JSON request body
- Replaces `model` field with backend's configured value
- Logged as `[模型覆盖]`

## Common Issues

**Streaming responses timeout:**
- Symptom: `context deadline exceeded` during streaming
- Cause: Global http.Client.Timeout was killing streams
- Fix: Removed global timeout, only apply to non-streaming requests via context

**Compression not detected:**
- Proxy auto-detects gzip/zstd via header or magic bytes
- Check `[readResponseBody]` logs for `Content-Encoding` header value
- If backend returns compressed data without header, magic byte detection should handle it

**OpenAI backend returns Claude format:**
- Auto-detection in `convertOpenAIToClaude` checks for `type: "message"` field
- If detected, returns data as-is without conversion
- Logged as `[格式检测] - 响应已经是 Claude 格式,直接透传`

**Intermittent 403/200 responses:**
- 403 errors do NOT trigger failover (only 5xx and 429 do)
- Check token previews in logs to see if different backends are being used
- Disable failing backends in config.json

**Circuit breaker not opening:**
- Requires `failure_threshold` consecutive failures
- Check `[跳过]` logs to see if circuit is open
- Failures must be from actual backend errors (5xx), not client errors (4xx except 429)

## Code Modification Guidelines

- **All response body reading** must use `readResponseBody()` helper (handles compression)
- **New configuration options**: Update Config struct in config.go and validation in NewProxyServer
- **Logging**: Maintain Chinese convention for consistency with existing logs
- **Format conversion**: Add new conversion functions in converter.go if supporting new API types
- **Circuit breaker logic**: Modify circuit_breaker.go if changing failover behavior
- **Timeout handling**: Be aware of streaming vs non-streaming distinction
