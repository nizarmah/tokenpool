package translate

import (
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// ResponsesResponseToAnthropic converts a Responses API response into a
// Messages API response. Reasoning items are dropped.
func ResponsesResponseToAnthropic(body []byte) ([]byte, error) {
	var r responsesResponse
	if err := json.Unmarshal(body, &r); err != nil {
		return nil, fmt.Errorf("invalid Responses API response: %w", err)
	}
	if r.Status == "failed" {
		msg := "upstream response failed"
		if r.Error != nil && r.Error.Message != "" {
			msg = r.Error.Message
		}
		return nil, errors.New(msg)
	}
	content := []obj{}
	var text []string
	var calls []obj
	for _, it := range r.Output {
		switch it.Type {
		case "message":
			if t := responsesText(it.Content); t != "" {
				text = append(text, t)
			}
		case "function_call":
			calls = append(calls, obj{
				"type": "tool_use", "id": ToolID(cmp.Or(it.CallID, it.ID)), "name": it.Name,
				"input": argumentsObject(it.Arguments),
			})
		}
	}
	if len(text) > 0 {
		content = append(content, obj{"type": "text", "text": strings.Join(text, "\n\n")})
	}
	content = append(content, calls...)
	return json.Marshal(obj{
		"id": MessageID(r.ID), "type": "message", "role": "assistant", "model": r.Model,
		"content": content, "stop_reason": StopReason(r.Status, r.incompleteReason(), len(calls) > 0),
		"stop_sequence": nil, "usage": usageToAnthropic(r.Usage),
	})
}

// AnthropicResponseToResponses converts a Messages API response into a
// Responses API response: a message item per text block and a
// function_call item per tool use. Thinking is dropped: a client would
// send it back as a reasoning item no Responses upstream has stored.
func AnthropicResponseToResponses(body []byte) ([]byte, error) {
	var r anthropicResponse
	if err := json.Unmarshal(body, &r); err != nil {
		return nil, fmt.Errorf("invalid Messages API response: %w", err)
	}
	output := []obj{}
	for i, b := range r.Content {
		switch b.Type {
		case "text":
			output = append(output, messageItem(itemID(r.ID, i), "completed", b.Text))
		case "tool_use":
			output = append(output, functionCallItem(b.ID, b.Name, argumentsString(b.Input), "completed"))
		}
	}
	status, incomplete := responseStatus(r.StopReason)
	return json.Marshal(responseObject(r.ID, r.Model, time.Now().Unix(), status, incomplete, output, &r.Usage))
}

// responseObject builds a Responses API response object.
func responseObject(id, model string, created int64, status, incomplete string, output []obj, usage *anthropicUsage) obj {
	r := obj{
		"id": responseID(id), "object": "response", "created_at": created, "status": status,
		"model": model, "output": output, "incomplete_details": nil, "error": nil, "usage": nil,
	}
	if incomplete != "" {
		r["incomplete_details"] = obj{"reason": incomplete}
	}
	if usage != nil {
		r["usage"] = usageToResponses(*usage)
	}
	return r
}

// messageItem is an assistant message output item; text is omitted while
// the item is still in progress.
func messageItem(id, status string, text ...string) obj {
	content := []obj{}
	for _, t := range text {
		content = append(content, outputText(t))
	}
	return obj{"type": "message", "id": id, "status": status, "role": "assistant", "content": content}
}

func outputText(text string) obj {
	return obj{"type": "output_text", "text": text, "annotations": []obj{}}
}

func functionCallItem(callID, name, arguments, status string) obj {
	return obj{
		"type": "function_call", "id": "fc_" + callID, "call_id": callID, "name": name,
		"arguments": arguments, "status": status,
	}
}

// StopReason maps a Responses status and incomplete reason to an Anthropic
// stop_reason.
func StopReason(status, incomplete string, hasToolCalls bool) string {
	switch {
	case status == "incomplete" && incomplete == "content_filter":
		return "refusal"
	case status == "incomplete":
		return "max_tokens"
	case hasToolCalls:
		return "tool_use"
	}
	return "end_turn"
}

// responseStatus maps an Anthropic stop_reason to a Responses status and
// incomplete reason.
func responseStatus(stop string) (status, incomplete string) {
	switch stop {
	case "max_tokens", "model_context_window_exceeded":
		return "incomplete", "max_output_tokens"
	case "refusal":
		return "incomplete", "content_filter"
	}
	return "completed", ""
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

// responseID turns an Anthropic message id into a Responses-style id.
func responseID(id string) string {
	if id == "" {
		return fmt.Sprintf("resp_tokenpool_%d", time.Now().UnixNano())
	}
	return "resp_" + strings.TrimPrefix(id, "msg_")
}

// itemID names the output item made from content block i of message id.
func itemID(id string, i int) string {
	return fmt.Sprintf("msg_%s_%d", strings.TrimPrefix(id, "msg_"), i)
}

func usageToAnthropic(u *responsesUsage) anthropicUsage {
	if u == nil {
		return anthropicUsage{}
	}
	cached := u.InputTokensDetails.CachedTokens
	return anthropicUsage{
		InputTokens:          max(u.InputTokens-cached, 0),
		OutputTokens:         u.OutputTokens,
		CacheReadInputTokens: cached,
	}
}

func usageToResponses(u anthropicUsage) obj {
	input := u.InputTokens + u.CacheReadInputTokens + u.CacheCreationInputTokens
	return obj{
		"input_tokens":          input,
		"input_tokens_details":  obj{"cached_tokens": u.CacheReadInputTokens},
		"output_tokens":         u.OutputTokens,
		"output_tokens_details": obj{"reasoning_tokens": 0},
		"total_tokens":          input + u.OutputTokens,
	}
}
