package translate

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// OpenAIResponseToAnthropic converts a chat completion into a Messages API
// response.
func OpenAIResponseToAnthropic(body []byte) ([]byte, error) {
	var r openaiResponse
	if err := json.Unmarshal(body, &r); err != nil {
		return nil, fmt.Errorf("invalid chat completion: %w", err)
	}
	content := []obj{}
	stop := "end_turn"
	if len(r.Choices) > 0 {
		c := r.Choices[0]
		text := openaiText(c.Message.Content)
		if text == "" {
			text = c.Message.Refusal
		}
		if text != "" {
			content = append(content, obj{"type": "text", "text": text})
		}
		for _, tc := range c.Message.ToolCalls {
			content = append(content, obj{
				"type": "tool_use", "id": ToolID(tc.ID), "name": tc.Function.Name,
				"input": argumentsObject(tc.Function.Arguments),
			})
		}
		stop = StopReason(c.FinishReason, len(c.Message.ToolCalls) > 0)
	}
	return json.Marshal(obj{
		"id": MessageID(r.ID), "type": "message", "role": "assistant", "model": r.Model,
		"content": content, "stop_reason": stop, "stop_sequence": nil,
		"usage": usageToAnthropic(r.Usage),
	})
}

// AnthropicResponseToOpenAI converts a Messages API response into a chat
// completion.
func AnthropicResponseToOpenAI(body []byte) ([]byte, error) {
	var r anthropicResponse
	if err := json.Unmarshal(body, &r); err != nil {
		return nil, fmt.Errorf("invalid Messages API response: %w", err)
	}
	var text, reasoning []string
	var calls []openaiToolCall
	for _, b := range r.Content {
		switch b.Type {
		case "text":
			text = append(text, b.Text)
		case "thinking":
			reasoning = append(reasoning, b.Thinking)
		case "tool_use":
			calls = append(calls, openaiToolCall{
				ID: b.ID, Type: "function",
				Function: openaiFunctionCall{Name: b.Name, Arguments: argumentsString(b.Input)},
			})
		}
	}
	msg := obj{"role": "assistant", "content": nil}
	if len(text) > 0 || len(calls) == 0 {
		msg["content"] = strings.Join(text, "")
	}
	if len(calls) > 0 {
		msg["tool_calls"] = calls
	}
	if len(reasoning) > 0 {
		msg["reasoning_content"] = strings.Join(reasoning, "")
	}
	return json.Marshal(obj{
		"id": r.ID, "object": "chat.completion", "created": time.Now().Unix(), "model": r.Model,
		"choices": []obj{{"index": 0, "message": msg, "finish_reason": FinishReason(r.StopReason)}},
		"usage":   usageToOpenAI(r.Usage),
	})
}

// StopReason maps an OpenAI finish_reason to an Anthropic stop_reason.
func StopReason(finish string, hasToolCalls bool) string {
	switch {
	case finish == "length":
		return "max_tokens"
	case finish == "content_filter":
		return "refusal"
	case finish == "tool_calls" || finish == "function_call" || hasToolCalls:
		return "tool_use"
	}
	return "end_turn"
}

// FinishReason maps an Anthropic stop_reason to an OpenAI finish_reason.
func FinishReason(stop string) string {
	switch stop {
	case "max_tokens", "model_context_window_exceeded":
		return "length"
	case "tool_use":
		return "tool_calls"
	case "refusal":
		return "content_filter"
	}
	return "stop"
}

// MessageID turns an upstream id into an Anthropic-style message id.
func MessageID(id string) string {
	if id == "" {
		return fmt.Sprintf("msg_tokenpool_%d", time.Now().UnixNano())
	}
	if strings.HasPrefix(id, "msg_") {
		return id
	}
	return "msg_" + id
}

func usageToAnthropic(u *openaiUsage) anthropicUsage {
	if u == nil {
		return anthropicUsage{}
	}
	cached := 0
	if u.PromptTokensDetails != nil {
		cached = u.PromptTokensDetails.CachedTokens
	}
	return anthropicUsage{
		InputTokens:          max(u.PromptTokens-cached, 0),
		OutputTokens:         u.CompletionTokens,
		CacheReadInputTokens: cached,
	}
}

func usageToOpenAI(u anthropicUsage) openaiUsage {
	prompt := u.InputTokens + u.CacheReadInputTokens + u.CacheCreationInputTokens
	out := openaiUsage{
		PromptTokens:     prompt,
		CompletionTokens: u.OutputTokens,
		TotalTokens:      prompt + u.OutputTokens,
	}
	if u.CacheReadInputTokens > 0 {
		out.PromptTokensDetails = &openaiPromptDetails{CachedTokens: u.CacheReadInputTokens}
	}
	return out
}
