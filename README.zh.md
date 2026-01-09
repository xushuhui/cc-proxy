# CC Proxy

一个轻量级的 Claude API 反向代理，支持多个 API key 的自动故障转移、混合后端（Claude + OpenAI）、智能熔断和限流处理。当一个后端失败时，自动切换到下一个可用后端，对客户端完全透明。

[English Documentation](README.md)

## 功能特性

### 核心功能
- **自动故障转移**：当主 API key 失败时,自动尝试备用 key(仅 5xx/429 错误触发)
- **熔断器机制**：智能熔断器防止对故障后端的重复请求
- **限流处理**：智能处理 429 错误,支持冷却时间和 Retry-After 响应头
- **超时处理**：非流式请求超时自动触发故障转移,流式请求无超时限制

### API 支持
- **Claude API 后端**：原生支持 Claude API 格式
- **OpenAI API 后端**：自动将 Claude API 请求转换为 OpenAI 格式
- **混合后端**：可同时配置 Claude 和 OpenAI 后端,实现真正的多云故障转移
- **格式自动检测**：智能识别响应格式,无需担心配置错误

### 压缩与传输
- **智能压缩处理**：自动检测并解压 gzip 和 zstd 压缩响应
- **流式响应支持**：完整支持流式和非流式响应
- **透明代理**：完整转发所有 HTTP 请求和响应头

### 开发与运维
- **配置灵活**：JSON 配置文件,支持多个后端、超时、重试等配置
- **详细日志**：所有请求和响应都有详细日志,包括流式内容实时显示
- **安全日志**：Token 仅显示前后 4 个字符,避免泄露完整密钥
- **模型覆盖**：可选的按后端模型覆盖,强制使用特定模型版本
- **零依赖**：编译后单个二进制文件,无需额外依赖

## 快速开始

### 1. 编译程序

```bash
go mod tidy
go build -o cc-proxy
```

### 2. 配置后端

编辑 `config.json` 文件,配置你的 API token 和后端地址：

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
    },
    {
      "name": "OpenAI Backend",
      "base_url": "https://api.openai.com/v1",
      "token": "sk-your-openai-key",
      "api_type": "openai",
      "model": "gpt-4o",
      "enabled": false
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

**配置说明**：
- `token`：API token（注意是 token 不是 api_key）
- `api_type`：API 类型，可选值 `"claude"`（默认）或 `"openai"`
- `model`：可选的模型覆盖，强制使用特定模型
- `failover`：可选的故障转移配置，有合理的默认值

### 3. 启动代理

```bash
./cc-proxy -config config.json
```

输出示例：
```
2026/01/07 14:00:00 Claude API 故障转移代理启动中...
2026/01/07 14:00:00 监听端口: 3456
2026/01/07 14:00:00 配置的后端:
2026/01/07 14:00:00   1. Anthropic Official - https://api.anthropic.com [启用] [API: claude]
2026/01/07 14:00:00   2. Backup Provider - https://api.backup.com [启用] [API: claude]
2026/01/07 14:00:00   3. OpenAI Backend - https://api.openai.com/v1 [禁用] [API: openai]
2026/01/07 14:00:00 最大重试次数: 3
2026/01/07 14:00:00 请求超时: 30 秒
2026/01/07 14:00:00 熔断配置: 连续失败 3 次触发,熔断 30 秒
2026/01/07 14:00:00 限流配置: 429 错误后冷却 60 秒

2026/01/07 14:00:00 ✓ 代理服务器运行在 http://localhost:3456
2026/01/07 14:00:00 ✓ 配置 Claude Code: export ANTHROPIC_BASE_URL=http://localhost:3456
```

### 4. 配置 Claude Code

```bash
# 设置 Claude Code 使用本地代理
export ANTHROPIC_BASE_URL=http://localhost:3456
export ANTHROPIC_API_KEY=dummy  # 由代理处理真实 token

# 启动 Claude Code
claude
```

## 配置说明

### 后端配置

每个后端支持以下配置：

| 配置项 | 说明 | 必填 | 默认值 |
|--------|------|------|--------|
| `name` | 后端名称（用于日志） | 是 | - |
| `base_url` | API 基础 URL | 是 | - |
| `token` | API Token | 是 | - |
| `enabled` | 是否启用 | 是 | - |
| `api_type` | API 类型：`"claude"` 或 `"openai"` | 否 | `"claude"` |
| `model` | 模型覆盖（可选） | 否 | - |

后端按配置顺序优先使用，失败后自动尝试下一个。

**API 类型说明**：
- `"claude"`（默认）：后端使用 Claude API 格式，直接透传
  - 适用于：Anthropic 官方 API、Claude API 兼容的第三方服务
  - 请求路径：`/v1/messages` → `/v1/messages`（透传）
  - 请求格式：Claude 格式（不转换）
  - 响应格式：Claude 格式（不转换）

- `"openai"`：代理会自动转换请求/响应格式和路径
  - 适用于：OpenAI 官方 API、OpenAI 兼容的第三方服务
  - 请求路径：`/v1/messages` → `/v1/chat/completions`（自动转换）
  - 请求格式：Claude 格式 → OpenAI 格式（自动转换）
  - 响应格式：OpenAI 格式 → Claude 格式（自动转换）
  - 支持流式和非流式响应
  - 自动检测：如果响应已是 Claude 格式，直接透传

**重要提示**：
- 只有真正的 OpenAI API 或 OpenAI 兼容接口才应该设置 `api_type: "openai"`
- 如果 backend 本身支持 Claude API 格式（即使它基于 OpenAI 模型），应该使用 `api_type: "claude"` 或不设置该字段
- 错误的 `api_type` 配置会导致 422 错误（格式不匹配）

### 重试与超时配置

| 配置项 | 说明 | 默认值 |
|--------|------|--------|
| `retry.max_attempts` | 最大重试次数(当前版本未使用) | 3 |
| `retry.timeout_seconds` | 非流式请求超时时间(秒) | 30 |

**重要**：超时配置仅对非流式请求生效。流式请求（`stream: true`）没有超时限制，避免长时间生成被中断。

### 故障转移配置

| 配置项 | 说明 | 默认值 |
|--------|------|--------|
| `failover.circuit_breaker.failure_threshold` | 触发熔断的连续失败次数 | 3 |
| `failover.circuit_breaker.open_timeout_seconds` | 熔断持续时间(秒) | 30 |
| `failover.circuit_breaker.half_open_requests` | 半开状态测试请求数 | 1 |
| `failover.rate_limit.cooldown_seconds` | 429 限流后冷却时间(秒) | 60 |

**熔断器状态**：
- **关闭(正常)**：所有请求正常通过
- **打开(熔断)**：后端连续失败 N 次后被跳过
- **半开(测试)**：超时到期后,允许有限的测试请求检查后端是否恢复

## 工作原理

### 请求处理流程

1. **请求接收**：代理接收客户端的 API 请求
2. **后端优先级排序**：
   - 正常状态的后端优先
   - 限流冷却中的后端次之
   - 熔断打开的后端最后
3. **逐个尝试后端**：
   - 检查后端是否应该跳过（禁用/熔断/限流）
   - 检测请求类型（流式/非流式）
   - **根据 `api_type` 处理请求路径**：
     - Claude 后端：`/v1/messages` → `base_url/v1/messages`
     - OpenAI 后端：`/v1/messages` → `base_url/v1/chat/completions`
   - 如果是 OpenAI 后端，转换请求格式
   - 添加适当的超时控制（仅非流式）
   - 转发请求到后端
4. **错误处理与故障转移**：
   - **5xx 错误**：记录失败,达到阈值触发熔断,尝试下一个后端
   - **429 限流**：记录限流时间戳,进入冷却,尝试下一个后端
   - **超时**：记录失败,尝试下一个后端
   - **401/403**：立即返回不重试(认证错误)
   - **其他 4xx**：立即返回不重试(客户端错误)
5. **响应处理**：
   - 如果是 OpenAI 后端，转换响应格式
   - 自动解压 gzip/zstd 压缩的响应
   - 返回给客户端

```
请求 → 后端优先级排序
     ↓
 后端1 (熔断打开 - 跳过)
     ↓
 后端2 (OpenAI 格式转换 → 500 错误 → 记录失败 → 尝试下一个)
     ↓
 后端3 (429 → 记录限流 → 尝试下一个)
     ↓
 后端4 (成功) → 响应格式转换 → 返回客户端
```

### 格式转换（OpenAI 后端）

当后端配置为 `"api_type": "openai"` 时，代理会自动处理以下转换：

**路径转换**：
- 客户端请求：`POST /v1/messages`
- OpenAI 后端：`POST /v1/chat/completions`
- 日志标记：`[路径转换] backend_name - Claude 路径 /v1/messages -> OpenAI 路径 /v1/chat/completions`

**请求转换（Claude → OpenAI）**：
- 转换 `messages` 数组格式（content array → string/array）
- 映射模型名称（如 `claude-3-5-sonnet` → `gpt-4o`）
- 将 `system` 字段转换为系统消息
- 日志标记：`[格式转换] backend_name - Claude 请求已转换为 OpenAI 格式`

**响应转换（OpenAI → Claude）**：
- 转换 `choices` → `content` 数组
- 映射 `finish_reason` → `stop_reason`
- 转换 `usage` 字段格式（prompt_tokens → input_tokens）
- 自动检测：如果响应已是 Claude 格式，直接透传
- 日志标记：`[格式转换] backend_name - OpenAI 响应已转换为 Claude 格式`

**支持的模型映射**：
- `claude-sonnet-4-5` / `claude-sonnet-4-5-thinking` → `gpt-4o`
- `claude-3-opus` → `gpt-4-turbo`
- `claude-3-sonnet` → `gpt-4`
- `claude-3-haiku` → `gpt-3.5-turbo`

### 压缩处理

代理自动处理以下压缩格式：

1. **gzip 压缩**
   - 通过 `Content-Encoding: gzip` 响应头检测
   - 自动解压并记录日志

2. **zstd 压缩**
   - 通过 `Content-Encoding: zstd` 响应头检测
   - 使用高效的 zstd 解压器

3. **魔术字节检测**
   - 即使响应头没有声明压缩
   - 通过检测魔术字节自动识别：
     - gzip: `1f 8b`
     - zstd: `28 b5 2f fd`

## 日志说明

### 请求处理日志

```
[请求开始] POST /v1/messages - 配置了 3 个后端
[跳过] Backend1 - 熔断中 (还需 25 秒)
[尝试 #1] Backend2 - POST https://api.anthropic.com/v1/messages (token: sk-a...xyz1)
[超时设置] Backend2 - 非流式请求,设置 30 秒超时
[模型覆盖] Backend2 - 使用配置的模型: claude-3-5-sonnet-20241022
[错误详情] Backend2 - HTTP 500 - 响应: {"error":{"type":"internal_error"}}
[失败 #1] Backend2 - 后端返回错误: HTTP 500
[熔断触发] Backend2 - 连续失败 3 次,熔断 30 秒
[尝试 #2] Backend3 - POST https://api.backup.com/v1/messages (token: sk-b...abc2)
[成功 #2] Backend3 - HTTP 200
[复制响应] 已写入 1523 字节到客户端
```

### 流式响应日志

```
[流式开始] Backend1 - 开始接收流式响应
[流式事件] Backend1 - message_start
[流式内容 #1] Backend1 - "Hello"
[流式内容 #2] Backend1 - "! How"
[流式内容 #3] Backend1 - " can I"
[流式事件 #4] Backend1 - type: content_block_stop
[流式结束] Backend1 - 收到 [DONE] 标记
[完整内容] Backend1 - Hello! How can I help you today?
[流式完成] Backend1 - 处理完成 (共 15 行, 8 个数据块, 总字符数: 31)
```

### 格式转换日志

```
[格式转换] Backend1 - Claude 请求已转换为 OpenAI 格式
[readResponseBody] Content-Encoding 头: 'zstd'
[readResponseBody] 检测到 zstd 压缩,尝试解压
[readResponseBody] zstd 解压器创建成功
[readResponseBody] 读取了 532 字节数据
[格式检测] Backend1 - 响应已经是 Claude 格式,直接透传
```

### 熔断器和限流日志

**熔断器**：
```
[熔断触发] Backend1 - 连续失败 3 次,熔断 30 秒 (HTTP 502)
[跳过] Backend1 - 熔断中 (还需 25 秒)
[尝试 #1] Backend1 - ... [熔断测试 1/1]
[熔断恢复] Backend1 - 后端已恢复正常
```

**限流**：
```
[限流记录] Backend2 - 触发 429,Retry-After: 60 秒
[限流记录] Backend3 - 触发 429,冷却 60 秒
```

### 日志特性

- **Token 安全**：只显示 token 的前 4 和后 4 个字符(如 `sk-a...xyz1`)
- **实时流式内容**：流式响应的每个内容块都会实时显示
- **完整内容累积**：流式传输结束后显示完整响应文本
- **压缩检测**：详细记录压缩格式和解压过程
- **格式转换**：清晰标注格式转换的每个步骤
- **错误详情**：所有非 2xx 响应都会显示完整错误信息

## 高级用法

### 作为系统服务运行

创建 systemd 服务文件 `/etc/systemd/system/cc-proxy.service`：

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

启动服务：

```bash
sudo systemctl daemon-reload
sudo systemctl enable cc-proxy
sudo systemctl start cc-proxy
sudo systemctl status cc-proxy
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

### 测试代理

**测试非流式请求**：
```bash
curl -X POST http://localhost:3456/v1/messages \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer dummy" \
  -d '{
    "model": "claude-sonnet",
    "messages": [{"role": "user", "content": "你好"}],
    "max_tokens": 1000
  }'
```

**测试流式请求**：
```bash
curl -X POST http://localhost:3456/v1/messages \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer dummy" \
  -d '{
    "model": "claude-sonnet",
    "messages": [{"role": "user", "content": "你好"}],
    "max_tokens": 1000,
    "stream": true
  }'
```

## 故障排查

### 问题：流式响应超时中断

**症状**：日志显示 `context deadline exceeded` 或流式传输中途停止

**原因**：旧版本的全局超时设置会杀死长时间流式响应

**解决方案**：已在新版本中修复。确保使用最新版本代码，流式请求不再受超时限制。

### 问题：后端持续熔断

**症状**：日志显示 `[熔断触发]` 和 `[跳过] - 熔断中`

**可能原因**：
1. 后端确实宕机或不稳定
2. 超时时间对慢速后端来说太短
3. 网络问题导致失败

**解决方案**：
1. 独立检查后端健康状态：`curl -I https://backend-url/v1/messages`
2. 如果后端较慢,增加 `retry.timeout_seconds`
3. 增加 `failover.circuit_breaker.failure_threshold` 以提高容忍度
4. 增加 `failover.circuit_breaker.open_timeout_seconds` 以更快尝试恢复
5. 如果已知后端宕机,在配置中临时禁用该后端

### 问题：压缩响应无法读取

**症状**：日志显示 `[二进制数据,长度: XXX 字节]`，响应内容乱码

**可能原因**：
1. 后端返回压缩响应但未声明 `Content-Encoding` 头
2. 代理未正确识别压缩格式

**排查方法**：
1. 查看 `[readResponseBody]` 日志中的 `Content-Encoding` 头
2. 检查是否有 `检测到 gzip/zstd 压缩` 的日志

**解决方案**：
- 新版本已支持魔术字节自动检测，无需声明也能正确解压
- 如果仍有问题，查看日志确认压缩格式

### 问题：OpenAI 后端返回错误

**症状**：配置了 `api_type: "openai"` 但请求失败，返回 422 错误

**可能原因**：
1. **最常见**：后端实际期望的是 Claude API 格式，但配置中设置了 `api_type: "openai"`
   - 错误信息示例：`"Input should be a valid string"` 或 `"loc":["body","messages",1,"content","str"]`
   - 原因：代理把请求转换为 OpenAI 格式，但后端期望 Claude 格式
2. 模型映射不正确
3. 后端 URL 配置错误

**排查方法**：
1. 查看 `[错误详情]` 日志中的完整错误信息
2. 查看 `[OpenAI 原始响应]` 日志
3. 检查是否有 `[格式检测] - 响应已经是 Claude 格式,直接透传`
4. 查看请求的 URL 是否正确

**解决方案**：
- **如果后端支持 Claude API 格式**（最常见情况）：
  - 将配置中的 `api_type` 改为 `"claude"` 或删除该字段（使用默认值）
  - 示例：很多第三方服务（如 iflow、OpenAI 代理等）虽然基于 OpenAI 模型，但提供 Claude API 兼容接口
- **如果后端确实是 OpenAI API**：
  - 确认 `base_url` 配置正确，通常应该包含 `/v1` 路径
  - 例如：`"base_url": "https://api.openai.com/v1"`
  - 代理会自动将 `/v1/messages` 转换为 `/v1/chat/completions`
- 代理会自动检测响应格式，如果响应已是 Claude 格式会直接透传

### 问题：频繁出现 429 限流错误

**症状**：日志频繁显示 `[限流记录]`

**解决方案**：
1. 添加更多后端来分散负载
2. 增加 `failover.rate_limit.cooldown_seconds` 避免对限流后端持续请求
3. 为不同客户端使用独立的 token
4. 降低请求频率

### 问题：所有后端都失败

**排查步骤**：
1. 检查网络连接：`curl -I https://api.anthropic.com`
2. 验证 token 是否有效(查看 `[错误详情]` 日志)
3. 确认配置文件中使用的是 `token` 字段而不是 `api_key`
4. 确认后端 URL 是否正确
5. 检查是否所有后端都被熔断(查看 `[跳过]` 日志)

## 性能优化

- **内存占用**：约 10-20MB
- **并发支持**：Go 原生并发，支持大量并发连接
- **延迟**：代理转发延迟 < 5ms（Claude 后端），< 10ms（OpenAI 后端含格式转换）
- **压缩性能**：zstd 解压速度比 gzip 快 2-5 倍

## 技术栈

- **语言**：Go 1.21+
- **核心依赖**：
  - `github.com/klauspost/compress/zstd` - zstd 压缩支持
  - Go 标准库（`net/http`、`compress/gzip`、`encoding/json` 等）
- **特性**：
  - 自动 gzip/zstd 解压
  - 优雅关闭支持（SIGINT/SIGTERM）
  - 智能格式转换（Claude ↔ OpenAI）
  - 熔断器和限流状态管理
  - 流式响应实时处理

## 许可证

MIT License

## 贡献

欢迎提交 Issue 和 Pull Request。

## 相关链接

- [Claude Code 官方文档](https://code.claude.com/docs)
- [Anthropic API 文档](https://docs.anthropic.com)
- [OpenAI API 文档](https://platform.openai.com/docs)
