package main

import (
	"encoding/json"
	"fmt"
)

// convertClaudeToOpenAI converts Claude API request format to OpenAI format
func convertClaudeToOpenAI(claudeBody []byte) ([]byte, error) {
	var claudeReq map[string]any
	if err := json.Unmarshal(claudeBody, &claudeReq); err != nil {
		return nil, fmt.Errorf("解析 Claude 请求失败: %v", err)
	}

	openaiReq := make(map[string]any)

	// Convert model name
	if model, ok := claudeReq["model"].(string); ok {
		openaiReq["model"] = mapClaudeModelToOpenAI(model)
	}

	// Convert messages - Claude and OpenAI use similar format, but need to handle content array
	if messages, ok := claudeReq["messages"].([]any); ok {
		convertedMessages := make([]any, 0, len(messages))
		for _, msg := range messages {
			if msgMap, ok := msg.(map[string]any); ok {
				newMsg := make(map[string]any)
				newMsg["role"] = msgMap["role"]

				// Handle content field - Claude uses array, OpenAI uses string or array
				if content, ok := msgMap["content"].([]any); ok {
					// Multi-part content (text, images, etc.)
					if len(content) == 1 {
						// Single text block - convert to string for OpenAI
						if block, ok := content[0].(map[string]any); ok {
							if blockType, ok := block["type"].(string); ok && blockType == "text" {
								if text, ok := block["text"].(string); ok {
									newMsg["content"] = text
								}
							}
						}
					} else {
						// Multiple blocks - keep as array (OpenAI vision format)
						newMsg["content"] = content
					}
				} else if content, ok := msgMap["content"].(string); ok {
					// Already a string
					newMsg["content"] = content
				}

				convertedMessages = append(convertedMessages, newMsg)
			}
		}
		openaiReq["messages"] = convertedMessages
	}

	// Convert parameters
	if maxTokens, ok := claudeReq["max_tokens"].(float64); ok {
		openaiReq["max_tokens"] = int(maxTokens)
	}
	if temperature, ok := claudeReq["temperature"].(float64); ok {
		openaiReq["temperature"] = temperature
	}
	if topP, ok := claudeReq["top_p"].(float64); ok {
		openaiReq["top_p"] = topP
	}
	if stream, ok := claudeReq["stream"].(bool); ok {
		openaiReq["stream"] = stream
	}

	// Convert system prompt - Claude uses "system" field, OpenAI uses system message
	if system, ok := claudeReq["system"].(string); ok {
		messages := openaiReq["messages"].([]any)
		systemMsg := map[string]any{
			"role":    "system",
			"content": system,
		}
		// Prepend system message
		openaiReq["messages"] = append([]any{systemMsg}, messages...)
	}

	return json.Marshal(openaiReq)
}

// convertOpenAIToClaude converts OpenAI API response format to Claude format
func convertOpenAIToClaude(openaiBody []byte) ([]byte, error) {
	var openaiResp map[string]any
	if err := json.Unmarshal(openaiBody, &openaiResp); err != nil {
		return nil, fmt.Errorf("解析 OpenAI 响应失败: %v", err)
	}

	// Check if this is already a Claude response (has "type" and "content" fields)
	if respType, ok := openaiResp["type"].(string); ok && respType == "message" {
		// Already in Claude format, return as-is
		return openaiBody, nil
	}

	claudeResp := make(map[string]any)

	// Convert id
	if id, ok := openaiResp["id"].(string); ok {
		claudeResp["id"] = id
	}

	// Set type
	claudeResp["type"] = "message"

	// Convert role
	claudeResp["role"] = "assistant"

	// Convert content - OpenAI uses choices array, Claude uses content array
	if choices, ok := openaiResp["choices"].([]any); ok && len(choices) > 0 {
		if choice, ok := choices[0].(map[string]any); ok {
			if message, ok := choice["message"].(map[string]any); ok {
				if content, ok := message["content"].(string); ok {
					claudeResp["content"] = []map[string]any{
						{
							"type": "text",
							"text": content,
						},
					}
				}
			}

			// Convert finish_reason
			if finishReason, ok := choice["finish_reason"].(string); ok {
				claudeResp["stop_reason"] = mapOpenAIFinishReason(finishReason)
			}
		}
	}

	// Convert model
	if model, ok := openaiResp["model"].(string); ok {
		claudeResp["model"] = model
	}

	// Convert usage
	if usage, ok := openaiResp["usage"].(map[string]any); ok {
		claudeUsage := make(map[string]any)
		if inputTokens, ok := usage["prompt_tokens"].(float64); ok {
			claudeUsage["input_tokens"] = int(inputTokens)
		}
		if outputTokens, ok := usage["completion_tokens"].(float64); ok {
			claudeUsage["output_tokens"] = int(outputTokens)
		}
		claudeResp["usage"] = claudeUsage
	}

	return json.Marshal(claudeResp)
}

// convertOpenAIStreamToClaude converts OpenAI SSE chunk to Claude SSE chunk
func convertOpenAIStreamToClaude(openaiChunk []byte) ([]byte, error) {
	var openaiData map[string]any
	if err := json.Unmarshal(openaiChunk, &openaiData); err != nil {
		return nil, fmt.Errorf("解析 OpenAI 流式响应失败: %v", err)
	}

	claudeData := make(map[string]any)

	// Convert id
	if id, ok := openaiData["id"].(string); ok {
		claudeData["id"] = id
	}

	// Set type based on OpenAI chunk
	if choices, ok := openaiData["choices"].([]any); ok && len(choices) > 0 {
		if choice, ok := choices[0].(map[string]any); ok {
			if delta, ok := choice["delta"].(map[string]any); ok {
				// Check if this is content delta
				if content, ok := delta["content"].(string); ok {
					claudeData["type"] = "content_block_delta"
					claudeData["delta"] = map[string]any{
						"type": "text_delta",
						"text": content,
					}
					claudeData["index"] = 0
				}
			}

			// Check for finish_reason
			if finishReason, ok := choice["finish_reason"].(string); ok && finishReason != "" {
				claudeData["type"] = "message_delta"
				claudeData["delta"] = map[string]any{
					"stop_reason": mapOpenAIFinishReason(finishReason),
				}
			}
		}
	}

	return json.Marshal(claudeData)
}

// mapClaudeModelToOpenAI maps Claude model names to OpenAI model names
func mapClaudeModelToOpenAI(claudeModel string) string {
	// Map Claude models to OpenAI equivalents
	switch claudeModel {
	case "claude-3-5-sonnet-20241022", "claude-sonnet-4-5", "claude-sonnet-4-5-thinking":
		return "gpt-4o"
	case "claude-3-opus-20240229":
		return "gpt-4-turbo"
	case "claude-3-sonnet-20240229":
		return "gpt-4"
	case "claude-3-haiku-20240307":
		return "gpt-3.5-turbo"
	default:
		// Default to gpt-4o for unknown models
		return "gpt-4o"
	}
}

// mapOpenAIFinishReason maps OpenAI finish_reason to Claude stop_reason
func mapOpenAIFinishReason(openaiReason string) string {
	switch openaiReason {
	case "stop":
		return "end_turn"
	case "length":
		return "max_tokens"
	case "content_filter":
		return "stop_sequence"
	default:
		return "end_turn"
	}
}
