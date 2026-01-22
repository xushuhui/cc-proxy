# AGENTS.md

This file provides guidance for agentic coding agents working on this codebase.

## Build, Lint, and Test Commands

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

# No test suite exists - consider adding tests for:
# - Circuit breaker state transitions
# - Request/response handling edge cases
# - Format conversion logic
```

## Code Style Guidelines

### Package Structure
- All code resides in the `main` package (single-package architecture)
- All source files are in the root directory: main.go, config.go, config_loader.go, proxy.go, circuit_breaker.go

### Comments
- **DO NOT ADD COMMENTS** unless explicitly requested by the user
- Existing comments are minimal; maintain this style

### Imports
- Group imports: stdlib first, then third-party
- Third-party only: `github.com/klauspost/compress/zstd`
- Use `encoding/json`, `io`, `net/http`, `log`, `time` from stdlib

### Naming Conventions
- **Files**: lowercase with underscores (e.g., `config_loader.go`)
- **Types**: PascalCase (e.g., `ProxyServer`, `BackendState`, `CircuitBreaker`)
- **Variables**: camelCase (e.g., `backendCount`, `isStreamingRequest`)
- **Constants**: camelCase (e.g., `maxAttempts`, `openTimeoutSeconds`)
- **Private fields**: prefix with type name (e.g., `cb.config`, `state.backend`)

### Error Handling
- Use early returns for error cases
- Wrap errors with context: `fmt.Errorf("reading config: %w", err)`
- Log errors at appropriate level before returning
- Use named return values when helpful for documentation (e.g., `shouldRetry`)

### JSON Handling
- Use `json.Unmarshal` for parsing request/response bodies
- Use `json.Marshal` for modification (e.g., model override)
- Handle `omitempty` tags for optional fields in structs

### Logging
- **All logs use Chinese** with bracketed prefixes for consistency
- Key prefixes: `[请求开始]`, `[跳过]`, `[尝试 #N]`, `[成功 #N]`, `[失败 #N]`, `[错误详情]`, `[熔断触发]`, `[限流记录]`
- Log token previews: first 4 + "..." + last 4 characters

### Response Body Reading
- **ALWAYS use `readResponseBody()` helper** - handles gzip and zstd compression
- Never read response bodies directly without this helper
- Function defined in proxy.go:251

### Circuit Breaker Patterns
- Track state per backend with `BackendState` struct
- Use `stateMu sync.RWMutex` for thread safety
- States: Closed → Open → Half-Open → Closed
- Record success/failure/429 appropriately

### Timeout Handling
- **CRITICAL**: Never set http.Client.Timeout - kills streaming responses
- Only use `context.WithTimeout` for non-streaming requests
- Detect streaming: parse JSON body for `"stream": true`
- No timeout context for streaming requests

### URL Path Handling
- Use: `targetURL.Path = targetURL.Path + originalReq.URL.Path`
- Preserves base_url paths (e.g., `/anthropic` in `https://api.longcat.chat/anthropic`)

### Configuration
- Config struct in config.go with JSON tags
- Default values set in config_loader.go
- Backend struct: `Name`, `BaseURL`, `Enabled`, `Token`, `Model` (optional)

### Response Handling
- Use `copyResponse()` for forwarding responses
- Handle headers: copy all from backend response
- Use `io.Copy` for body streaming
