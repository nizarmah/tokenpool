package translate

import (
	"bytes"
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// ErrStateful means a Responses request continues state that one provider
// stored (previous_response_id, conversation, item_reference). A Messages
// API upstream has none of it, so it cannot serve the request.
var ErrStateful = errors.New("the request continues a stored response, which a Messages API upstream cannot see")

// AnthropicToResponses converts a Messages API request into a Responses API
// request. Anthropic-only features (thinking, cache_control, server tools,
// PDF documents) are dropped, and so are stop_sequences, which the
// Responses API has no field for.
func AnthropicToResponses(body []byte) ([]byte, error) {
	var req anthropicRequest
	if err := json.Unmarshal(body, &req); err != nil {
		return nil, fmt.Errorf("invalid Messages API request: %w", err)
	}
	out := responsesOutRequest{
		Model:        req.Model,
		Instructions: req.System.text(),
		Input:        anthropicMessagesToResponses(req.Messages),
		Temperature:  req.Temperature,
		TopP:         req.TopP,
		Stream:       req.Stream,
	}
	if req.MaxTokens > 0 {
		out.MaxOutputTokens = &req.MaxTokens
	}
	if req.Metadata != nil {
		out.User = req.Metadata.UserID
	}
	for _, t := range req.Tools {
		if len(t.InputSchema) == 0 {
			continue // server tools (web search, code execution) have no equivalent
		}
		out.Tools = append(out.Tools, responsesTool{
			Type: "function", Name: t.Name, Description: t.Description, Parameters: t.InputSchema,
		})
	}
	if tc := req.ToolChoice; tc != nil && len(out.Tools) > 0 {
		switch tc.Type {
		case "auto":
			out.ToolChoice = "auto"
		case "any":
			out.ToolChoice = "required"
		case "none":
			out.ToolChoice = "none"
		case "tool":
			out.ToolChoice = obj{"type": "function", "name": tc.Name}
		}
		if tc.DisableParallelToolUse {
			no := false
			out.ParallelToolCalls = &no
		}
	}
	return json.Marshal(out)
}

func anthropicMessagesToResponses(msgs []anthropicMessage) []obj {
	out := []obj{}
	for _, m := range msgs {
		if m.Role == "assistant" {
			out = append(out, assistantToResponses(m.Content)...)
			continue
		}
		// User turn. Tool outputs go first, right after the calls they answer.
		var parts, toolImages []obj
		for _, b := range m.Content {
			switch b.Type {
			case "text":
				if b.Text != "" {
					parts = append(parts, obj{"type": "input_text", "text": b.Text})
				}
			case "image":
				if p, ok := imagePart(b.Source); ok {
					parts = append(parts, p)
				}
			case "document":
				if text := documentText(b.Source); text != "" {
					parts = append(parts, obj{"type": "input_text", "text": text})
				}
			case "tool_result":
				text, images := toolResultToResponses(b)
				out = append(out, obj{"type": "function_call_output", "call_id": b.ToolUseID, "output": text})
				toolImages = append(toolImages, images...)
			}
		}
		// Tool outputs carry text only; images they returned follow as user content.
		if parts = append(toolImages, parts...); len(parts) > 0 {
			out = append(out, obj{"role": "user", "content": userContent(parts)})
		}
	}
	return out
}

// assistantToResponses turns an assistant turn into items in block order:
// a message for each run of text and a function_call for each tool use.
func assistantToResponses(content blocks) []obj {
	var out []obj
	var text []string
	flush := func() {
		if len(text) > 0 {
			out = append(out, obj{"role": "assistant", "content": strings.Join(text, "\n\n")})
			text = nil
		}
	}
	for _, b := range content {
		switch b.Type {
		case "text":
			if b.Text != "" {
				text = append(text, b.Text)
			}
		case "tool_use":
			flush()
			out = append(out, obj{
				"type": "function_call", "call_id": b.ID, "name": b.Name,
				"arguments": argumentsString(b.Input),
			})
		}
	}
	flush()
	return out
}

func toolResultToResponses(b block) (string, []obj) {
	var texts []string
	var images []obj
	for _, c := range b.Content {
		switch c.Type {
		case "text":
			texts = append(texts, c.Text)
		case "image":
			if p, ok := imagePart(c.Source); ok {
				images = append(images, p)
			}
		}
	}
	text := strings.Join(texts, "\n\n")
	if text == "" && len(images) > 0 {
		text = "(the tool returned images; they follow in the next message)"
	}
	if b.IsError {
		text = "Error: " + text
	}
	return text, images
}

func imagePart(src *source) (obj, bool) {
	if src == nil {
		return nil, false
	}
	switch src.Type {
	case "base64":
		return obj{"type": "input_image", "image_url": "data:" + src.MediaType + ";base64," + src.Data}, true
	case "url":
		return obj{"type": "input_image", "image_url": src.URL}, true
	}
	return nil, false
}

func documentText(src *source) string {
	if src == nil {
		return ""
	}
	switch src.Type {
	case "text":
		return src.Data
	case "content":
		return src.Content.text()
	}
	return "[a document was attached here, but this model cannot read it]"
}

// userContent uses a plain string for text-only content, which every
// Responses server accepts, and content parts otherwise.
func userContent(parts []obj) any {
	texts := make([]string, 0, len(parts))
	for _, p := range parts {
		text, ok := p["text"].(string)
		if p["type"] != "input_text" || !ok {
			return parts
		}
		texts = append(texts, text)
	}
	return strings.Join(texts, "\n\n")
}

// ResponsesToAnthropic converts a Responses API request into a Messages API
// request. defaultMaxTokens fills max_tokens, which Anthropic requires.
func ResponsesToAnthropic(body []byte, defaultMaxTokens int) ([]byte, error) {
	var req responsesRequest
	if err := json.Unmarshal(body, &req); err != nil {
		return nil, fmt.Errorf("invalid Responses API request: %w", err)
	}
	if req.PreviousResponseID != "" || (len(req.Conversation) > 0 && string(req.Conversation) != "null") {
		return nil, ErrStateful
	}
	system, msgs, err := responsesInputToAnthropic(req.Instructions, req.Input)
	if err != nil {
		return nil, err
	}
	out := anthropicOutRequest{
		Model:     req.Model,
		System:    system,
		Messages:  msgs,
		MaxTokens: defaultMaxTokens,
		TopP:      req.TopP,
		Stream:    req.Stream,
	}
	if req.MaxOutputTokens != nil {
		out.MaxTokens = *req.MaxOutputTokens
	}
	if t := req.Temperature; t != nil {
		clamped := min(*t, 1) // OpenAI allows up to 2, Anthropic up to 1
		out.Temperature = &clamped
	}
	for _, t := range req.Tools {
		if t.Type != "function" {
			continue
		}
		schema := t.Parameters
		if len(schema) == 0 || string(schema) == "null" {
			schema = json.RawMessage(`{"type":"object","properties":{}}`)
		}
		out.Tools = append(out.Tools, anthropicTool{
			Name: t.Name, Description: t.Description, InputSchema: schema,
		})
	}
	if len(out.Tools) > 0 {
		out.ToolChoice = responsesToolChoiceToAnthropic(req.ToolChoice)
		if req.ParallelToolCalls != nil && !*req.ParallelToolCalls {
			if out.ToolChoice == nil {
				out.ToolChoice = &anthropicToolChoice{Type: "auto"}
			}
			if out.ToolChoice.Type != "none" {
				out.ToolChoice.DisableParallelToolUse = true
			}
		}
	}
	if user := cmp.Or(req.SafetyIdentifier, req.User); user != "" {
		out.Metadata = &anthropicMetadata{UserID: user}
	}
	return json.Marshal(out)
}

func responsesToolChoiceToAnthropic(raw json.RawMessage) *anthropicToolChoice {
	if len(raw) == 0 {
		return nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		switch s {
		case "auto":
			return &anthropicToolChoice{Type: "auto"}
		case "required":
			return &anthropicToolChoice{Type: "any"}
		case "none":
			return &anthropicToolChoice{Type: "none"}
		}
		return nil
	}
	var named struct {
		Type string `json:"type"`
		Name string `json:"name"`
	}
	if err := json.Unmarshal(raw, &named); err == nil && named.Type == "function" && named.Name != "" {
		return &anthropicToolChoice{Type: "tool", Name: named.Name}
	}
	return nil
}

// responsesInputToAnthropic turns instructions and input (a string or
// items) into a system prompt and messages. Reasoning items and
// server-side tool calls have no Messages API equivalent and are dropped.
func responsesInputToAnthropic(instructions string, input json.RawMessage) (string, []anthropicOutMessage, error) {
	var system []string
	if instructions != "" {
		system = append(system, instructions)
	}
	var out []anthropicOutMessage
	// Anthropic wants alternating roles, so consecutive turns of one role merge.
	add := func(role string, content ...obj) {
		if len(content) == 0 {
			return
		}
		if n := len(out); n > 0 && out[n-1].Role == role {
			out[n-1].Content = append(out[n-1].Content, content...)
			return
		}
		out = append(out, anthropicOutMessage{Role: role, Content: content})
	}
	var text string
	if err := json.Unmarshal(input, &text); err == nil {
		if text != "" {
			add("user", obj{"type": "text", "text": text})
		}
		return strings.Join(system, "\n\n"), out, nil
	}
	var items []responsesItem
	if len(input) > 0 && string(input) != "null" {
		if err := json.Unmarshal(input, &items); err != nil {
			return "", nil, fmt.Errorf("invalid Responses API input: %w", err)
		}
	}
	for _, it := range items {
		switch it.Type {
		case "", "message":
			switch it.Role {
			case "system", "developer":
				if text := responsesText(it.Content); text != "" {
					system = append(system, text)
				}
			case "user":
				add("user", responsesContentToBlocks(it.Content)...)
			case "assistant":
				if text := responsesText(it.Content); text != "" {
					add("assistant", obj{"type": "text", "text": text})
				}
			}
		case "function_call":
			add("assistant", obj{
				"type": "tool_use", "id": ToolID(it.CallID), "name": it.Name,
				"input": argumentsObject(it.Arguments),
			})
		case "function_call_output":
			result := obj{"type": "tool_result", "tool_use_id": ToolID(it.CallID)}
			var output string
			if err := json.Unmarshal(it.Output, &output); err == nil {
				if output != "" {
					result["content"] = output
				}
			} else if content := responsesContentToBlocks(it.Output); len(content) > 0 {
				result["content"] = content
			}
			add("user", result)
		case "item_reference":
			return "", nil, ErrStateful
		}
	}
	return strings.Join(system, "\n\n"), out, nil
}

func responsesContentToBlocks(raw json.RawMessage) []obj {
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		if s == "" {
			return nil
		}
		return []obj{{"type": "text", "text": s}}
	}
	var parts []responsesPart
	if err := json.Unmarshal(raw, &parts); err != nil {
		return nil
	}
	var out []obj
	for _, p := range parts {
		switch p.Type {
		case "input_text", "output_text":
			if p.Text != "" {
				out = append(out, obj{"type": "text", "text": p.Text})
			}
		case "input_image":
			if p.ImageURL != "" {
				out = append(out, imageBlock(p.ImageURL))
			}
		case "input_file":
			out = append(out, fileBlock(p))
		}
	}
	return out
}

func imageBlock(url string) obj {
	if rest, ok := strings.CutPrefix(url, "data:"); ok {
		if meta, data, ok := strings.Cut(rest, ","); ok {
			mediaType, _, _ := strings.Cut(meta, ";")
			return obj{"type": "image", "source": obj{"type": "base64", "media_type": mediaType, "data": data}}
		}
	}
	return obj{"type": "image", "source": obj{"type": "url", "url": url}}
}

// fileBlock turns an input_file into a document. Inline PDFs carry over;
// other files become a note that one was attached.
func fileBlock(p responsesPart) obj {
	if data, ok := strings.CutPrefix(p.FileData, "data:application/pdf;base64,"); ok {
		return obj{"type": "document", "source": obj{"type": "base64", "media_type": "application/pdf", "data": data}}
	}
	return obj{"type": "text", "text": "[a file was attached here, but this model cannot read it]"}
}

// argumentsObject turns JSON-string arguments into an object.
func argumentsObject(args string) json.RawMessage {
	trimmed := strings.TrimSpace(args)
	if strings.HasPrefix(trimmed, "{") && json.Valid([]byte(trimmed)) {
		return json.RawMessage(trimmed)
	}
	return json.RawMessage(`{}`)
}

// argumentsString turns an Anthropic tool input into a JSON string.
func argumentsString(input json.RawMessage) string {
	var buf bytes.Buffer
	if len(input) == 0 || string(input) == "null" || json.Compact(&buf, input) != nil {
		return "{}"
	}
	return buf.String()
}

var toolIDInvalid = regexp.MustCompile(`[^A-Za-z0-9_-]`)

// ToolID makes a tool call id acceptable to Anthropic (^[A-Za-z0-9_-]+$).
func ToolID(id string) string {
	if id == "" {
		return "toolu_missing_id"
	}
	return toolIDInvalid.ReplaceAllString(id, "_")
}

// Rewrite sets the model and caps max_tokens in a request body, leaving
// every other field as the client sent it.
func Rewrite(body []byte, model string, maxTokens int) ([]byte, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil {
		return nil, fmt.Errorf("request body must be a JSON object: %w", err)
	}
	changed := false
	if model != "" {
		var current string
		_ = json.Unmarshal(fields["model"], &current)
		if current != model {
			fields["model"], _ = json.Marshal(model)
			changed = true
		}
	}
	if maxTokens > 0 {
		for _, key := range []string{"max_tokens", "max_output_tokens"} {
			var n int
			if err := json.Unmarshal(fields[key], &n); err == nil && n > maxTokens {
				fields[key], _ = json.Marshal(maxTokens)
				changed = true
			}
		}
	}
	if !changed {
		return body, nil
	}
	return json.Marshal(fields)
}
