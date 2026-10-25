package translate

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
)

// chatGPTUnsupported are Responses fields the ChatGPT Codex backend
// rejects or ignores. The Codex CLI never sends them.
var chatGPTUnsupported = []string{
	"max_output_tokens", "max_completion_tokens", "max_tokens", "temperature", "top_p",
	"user", "safety_identifier", "prompt_cache_retention", "background", "metadata",
}

// ForChatGPT shapes a Responses request for a ChatGPT login's Codex
// backend, the way the Codex CLI sends it: never stored, always streamed,
// input always a list, without the sampling and token-limit fields that
// backend rejects.
func ForChatGPT(body []byte) ([]byte, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil {
		return nil, fmt.Errorf("request body must be a JSON object: %w", err)
	}
	for _, name := range chatGPTUnsupported {
		delete(fields, name)
	}
	var text string
	if json.Unmarshal(fields["input"], &text) == nil {
		// "Input must be a list": a bare string is one user message.
		fields["input"], _ = json.Marshal([]obj{{"type": "message", "role": "user",
			"content": []obj{{"type": "input_text", "text": text}}}})
	}
	fields["store"] = json.RawMessage("false")
	fields["stream"] = json.RawMessage("true")
	var instructions string
	if raw, ok := fields["instructions"]; ok && (json.Unmarshal(raw, &instructions) != nil || strings.TrimSpace(instructions) == "") {
		delete(fields, "instructions")
	}
	return json.Marshal(fields)
}

// CollectResponsesStream reads a Responses API event stream to its end and
// returns the final response object, for a client that did not ask to
// stream from an upstream that only streams. Output items announced by
// response.output_item.done fill in a final response that lists none.
func CollectResponsesStream(r io.Reader) ([]byte, error) {
	var final map[string]json.RawMessage
	items := map[int]json.RawMessage{}
	err := readSSE(r, func(ev sseEvent) error {
		if strings.TrimSpace(ev.Data) == "[DONE]" {
			return nil
		}
		var e struct {
			Type        string                     `json:"type"`
			OutputIndex int                        `json:"output_index"`
			Item        json.RawMessage            `json:"item"`
			Response    map[string]json.RawMessage `json:"response"`
			Message     string                     `json:"message"`
			Error       *apiError                  `json:"error"`
		}
		if err := json.Unmarshal([]byte(ev.Data), &e); err != nil {
			return fmt.Errorf("bad upstream event: %w", err)
		}
		switch e.Type {
		case "response.output_item.done":
			if len(e.Item) > 0 {
				items[e.OutputIndex] = e.Item
			}
		case "response.completed", "response.incomplete":
			final = e.Response
			return errDone
		case "response.failed":
			var failed struct {
				Error *apiError `json:"error"`
			}
			if raw, ok := e.Response["error"]; ok {
				_ = json.Unmarshal(raw, &failed.Error)
			}
			if failed.Error != nil && failed.Error.Message != "" {
				return errors.New(failed.Error.Message)
			}
			return errors.New("upstream response failed")
		case "error":
			msg := e.Message
			if msg == "" && e.Error != nil {
				msg = e.Error.Message
			}
			if msg == "" {
				msg = "upstream error"
			}
			return errors.New(msg)
		}
		return nil
	})
	switch {
	case errors.Is(err, errDone):
	case err == nil:
		return nil, ErrIncomplete
	default:
		return nil, err
	}
	if final == nil {
		return nil, errors.New("upstream sent an empty final response")
	}
	var output []json.RawMessage
	_ = json.Unmarshal(final["output"], &output)
	if len(output) == 0 && len(items) > 0 {
		indexes := make([]int, 0, len(items))
		for i := range items {
			indexes = append(indexes, i)
		}
		sort.Ints(indexes)
		for _, i := range indexes {
			output = append(output, items[i])
		}
		final["output"], _ = json.Marshal(output)
	}
	return json.Marshal(final)
}
