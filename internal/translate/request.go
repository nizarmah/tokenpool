package translate

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

// AnthropicToOpenAI converts a Messages API request into a Chat Completions
// request. Anthropic-only features (thinking, cache_control, server tools,
// PDF documents) are dropped.
func AnthropicToOpenAI(body []byte) ([]byte, error) {
	var req anthropicRequest
	if err := json.Unmarshal(body, &req); err != nil {
		return nil, fmt.Errorf("invalid Messages API request: %w", err)
	}
	out := openaiOutRequest{
		Model:       req.Model,
		Messages:    anthropicMessagesToOpenAI(req.System, req.Messages),
		Temperature: req.Temperature,
		TopP:        req.TopP,
		Stop:        req.StopSequences,
		Stream:      req.Stream,
	}
	if req.MaxTokens > 0 {
		out.MaxTokens = &req.MaxTokens
	}
	if req.Stream {
		out.StreamOptions = &openaiStreamOption{IncludeUsage: true}
	}
	if req.Metadata != nil {
		out.User = req.Metadata.UserID
	}
	for _, t := range req.Tools {
		if len(t.InputSchema) == 0 {
			continue // server tools (web search, code execution) have no equivalent
		}
		out.Tools = append(out.Tools, openaiTool{Type: "function", Function: openaiFunction{
			Name: t.Name, Description: t.Description, Parameters: t.InputSchema,
		}})
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
			out.ToolChoice = obj{"type": "function", "function": obj{"name": tc.Name}}
		}
		if tc.DisableParallelToolUse {
			no := false
			out.ParallelToolCalls = &no
		}
	}
	return json.Marshal(out)
}

func anthropicMessagesToOpenAI(system blocks, msgs []anthropicMessage) []openaiOutMessage {
	var out []openaiOutMessage
	if s := system.text(); s != "" {
		out = append(out, openaiOutMessage{Role: "system", Content: s})
	}
	for _, m := range msgs {
		if m.Role == "assistant" {
			out = append(out, assistantToOpenAI(m.Content))
			continue
		}
		// User turn. Tool results become "tool" messages, which must come
		// right after the assistant's tool calls, so they go first.
		var parts, toolImages []openaiPart
		for _, b := range m.Content {
			switch b.Type {
			case "text":
				if b.Text != "" {
					parts = append(parts, openaiPart{Type: "text", Text: b.Text})
				}
			case "image":
				if p, ok := imagePart(b.Source); ok {
					parts = append(parts, p)
				}
			case "document":
				if text := documentText(b.Source); text != "" {
					parts = append(parts, openaiPart{Type: "text", Text: text})
				}
			case "tool_result":
				text, images := toolResultToOpenAI(b)
				out = append(out, openaiOutMessage{Role: "tool", ToolCallID: b.ToolUseID, Content: text})
				toolImages = append(toolImages, images...)
			}
		}
		// Tool messages carry text only; images they returned follow as user content.
		if parts = append(toolImages, parts...); len(parts) > 0 {
			out = append(out, openaiOutMessage{Role: "user", Content: userContent(parts)})
		}
	}
	return out
}

func assistantToOpenAI(content blocks) openaiOutMessage {
	msg := openaiOutMessage{Role: "assistant"}
	var text []string
	for _, b := range content {
		switch b.Type {
		case "text":
			if b.Text != "" {
				text = append(text, b.Text)
			}
		case "tool_use":
			msg.ToolCalls = append(msg.ToolCalls, openaiToolCall{
				ID: b.ID, Type: "function",
				Function: openaiFunctionCall{Name: b.Name, Arguments: argumentsString(b.Input)},
			})
		}
	}
	if len(text) > 0 || len(msg.ToolCalls) == 0 {
		msg.Content = strings.Join(text, "\n\n")
	}
	return msg
}

func toolResultToOpenAI(b block) (string, []openaiPart) {
	var texts []string
	var images []openaiPart
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

func imagePart(src *source) (openaiPart, bool) {
	if src == nil {
		return openaiPart{}, false
	}
	switch src.Type {
	case "base64":
		return openaiPart{Type: "image_url", ImageURL: &imageURL{
			URL: "data:" + src.MediaType + ";base64," + src.Data,
		}}, true
	case "url":
		return openaiPart{Type: "image_url", ImageURL: &imageURL{URL: src.URL}}, true
	}
	return openaiPart{}, false
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
// OpenAI-compatible server accepts, and content parts otherwise.
func userContent(parts []openaiPart) any {
	texts := make([]string, 0, len(parts))
	for _, p := range parts {
		if p.Type != "text" {
			return parts
		}
		texts = append(texts, p.Text)
	}
	return strings.Join(texts, "\n\n")
}

// OpenAIToAnthropic converts a Chat Completions request into a Messages API
// request. defaultMaxTokens fills max_tokens, which Anthropic requires.
func OpenAIToAnthropic(body []byte, defaultMaxTokens int) ([]byte, error) {
	var req openaiRequest
	if err := json.Unmarshal(body, &req); err != nil {
		return nil, fmt.Errorf("invalid Chat Completions request: %w", err)
	}
	system, msgs := openaiMessagesToAnthropic(req.Messages)
	out := anthropicOutRequest{
		Model:     req.Model,
		System:    system,
		Messages:  msgs,
		MaxTokens: defaultMaxTokens,
		TopP:      req.TopP,
		Stream:    req.Stream,
	}
	switch {
	case req.MaxCompletionTokens != nil:
		out.MaxTokens = *req.MaxCompletionTokens
	case req.MaxTokens != nil:
		out.MaxTokens = *req.MaxTokens
	}
	if t := req.Temperature; t != nil {
		clamped := min(*t, 1) // OpenAI allows up to 2, Anthropic up to 1
		out.Temperature = &clamped
	}
	if len(req.Stop) > 0 {
		var one string
		if err := json.Unmarshal(req.Stop, &one); err == nil {
			out.StopSequences = []string{one}
		} else {
			_ = json.Unmarshal(req.Stop, &out.StopSequences)
		}
	}
	for _, t := range req.Tools {
		if t.Type != "function" {
			continue
		}
		schema := t.Function.Parameters
		if len(schema) == 0 || string(schema) == "null" {
			schema = json.RawMessage(`{"type":"object","properties":{}}`)
		}
		out.Tools = append(out.Tools, anthropicTool{
			Name: t.Function.Name, Description: t.Function.Description, InputSchema: schema,
		})
	}
	if len(out.Tools) > 0 {
		out.ToolChoice = openaiToolChoiceToAnthropic(req.ToolChoice)
		if req.ParallelToolCalls != nil && !*req.ParallelToolCalls {
			if out.ToolChoice == nil {
				out.ToolChoice = &anthropicToolChoice{Type: "auto"}
			}
			if out.ToolChoice.Type != "none" {
				out.ToolChoice.DisableParallelToolUse = true
			}
		}
	}
	if req.User != "" {
		out.Metadata = &anthropicMetadata{UserID: req.User}
	}
	return json.Marshal(out)
}

func openaiToolChoiceToAnthropic(raw json.RawMessage) *anthropicToolChoice {
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
		Function struct {
			Name string `json:"name"`
		} `json:"function"`
	}
	if err := json.Unmarshal(raw, &named); err == nil && named.Function.Name != "" {
		return &anthropicToolChoice{Type: "tool", Name: named.Function.Name}
	}
	return nil
}

func openaiMessagesToAnthropic(msgs []openaiMessage) (string, []anthropicOutMessage) {
	var system []string
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
	for _, m := range msgs {
		switch m.Role {
		case "system", "developer":
			if text := openaiText(m.Content); text != "" {
				system = append(system, text)
			}
		case "user":
			add("user", openaiContentToBlocks(m.Content)...)
		case "assistant":
			var content []obj
			if text := openaiText(m.Content); text != "" {
				content = append(content, obj{"type": "text", "text": text})
			}
			for _, tc := range m.ToolCalls {
				content = append(content, obj{
					"type": "tool_use", "id": ToolID(tc.ID), "name": tc.Function.Name,
					"input": argumentsObject(tc.Function.Arguments),
				})
			}
			add("assistant", content...)
		case "tool":
			result := obj{"type": "tool_result", "tool_use_id": ToolID(m.ToolCallID)}
			if text := openaiText(m.Content); text != "" {
				result["content"] = text
			}
			add("user", result)
		}
	}
	return strings.Join(system, "\n\n"), out
}

func openaiContentToBlocks(raw json.RawMessage) []obj {
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		if s == "" {
			return nil
		}
		return []obj{{"type": "text", "text": s}}
	}
	var parts []openaiPart
	if err := json.Unmarshal(raw, &parts); err != nil {
		return nil
	}
	var out []obj
	for _, p := range parts {
		switch {
		case p.Type == "text" && p.Text != "":
			out = append(out, obj{"type": "text", "text": p.Text})
		case p.Type == "image_url" && p.ImageURL != nil:
			out = append(out, imageBlock(p.ImageURL.URL))
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

// argumentsObject turns OpenAI's JSON-string arguments into an object.
func argumentsObject(args string) json.RawMessage {
	trimmed := strings.TrimSpace(args)
	if strings.HasPrefix(trimmed, "{") && json.Valid([]byte(trimmed)) {
		return json.RawMessage(trimmed)
	}
	return json.RawMessage(`{}`)
}

// argumentsString turns an Anthropic tool input into OpenAI's JSON string.
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
		for _, key := range []string{"max_tokens", "max_completion_tokens"} {
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
