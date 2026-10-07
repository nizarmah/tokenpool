// Package translate converts requests, responses and streams between the
// Anthropic Messages API and the OpenAI Responses API, so one client can
// fail over between upstreams that speak different APIs.
package translate

import (
	"encoding/json"
	"strings"
)

// ---- Anthropic Messages API ----

type anthropicRequest struct {
	Model         string               `json:"model"`
	System        blocks               `json:"system,omitempty"`
	Messages      []anthropicMessage   `json:"messages"`
	MaxTokens     int                  `json:"max_tokens,omitempty"`
	Temperature   *float64             `json:"temperature,omitempty"`
	TopP          *float64             `json:"top_p,omitempty"`
	StopSequences []string             `json:"stop_sequences,omitempty"`
	Stream        bool                 `json:"stream,omitempty"`
	Tools         []anthropicTool      `json:"tools,omitempty"`
	ToolChoice    *anthropicToolChoice `json:"tool_choice,omitempty"`
	Metadata      *anthropicMetadata   `json:"metadata,omitempty"`
}

type anthropicMessage struct {
	Role    string `json:"role"`
	Content blocks `json:"content"`
}

type anthropicMetadata struct {
	UserID string `json:"user_id,omitempty"`
}

type anthropicTool struct {
	Type        string          `json:"type,omitempty"`
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"input_schema,omitempty"`
}

type anthropicToolChoice struct {
	Type                   string `json:"type"`
	Name                   string `json:"name,omitempty"`
	DisableParallelToolUse bool   `json:"disable_parallel_tool_use,omitempty"`
}

// blocks is Anthropic content: a plain string or an array of blocks.
type blocks []block

func (b *blocks) UnmarshalJSON(data []byte) error {
	if string(data) == "null" {
		*b = nil
		return nil
	}
	var s string
	if err := json.Unmarshal(data, &s); err == nil {
		*b = nil
		if s != "" {
			*b = blocks{{Type: "text", Text: s}}
		}
		return nil
	}
	var arr []block
	if err := json.Unmarshal(data, &arr); err != nil {
		return err
	}
	*b = arr
	return nil
}

// text joins the text blocks.
func (b blocks) text() string {
	var parts []string
	for _, blk := range b {
		if blk.Type == "text" && blk.Text != "" {
			parts = append(parts, blk.Text)
		}
	}
	return strings.Join(parts, "\n\n")
}

type block struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
	// image, document
	Source *source `json:"source,omitempty"`
	// tool_use
	ID    string          `json:"id,omitempty"`
	Name  string          `json:"name,omitempty"`
	Input json.RawMessage `json:"input,omitempty"`
	// tool_result
	ToolUseID string `json:"tool_use_id,omitempty"`
	Content   blocks `json:"content,omitempty"`
	IsError   bool   `json:"is_error,omitempty"`
	// thinking
	Thinking string `json:"thinking,omitempty"`
}

type source struct {
	Type      string `json:"type"`
	MediaType string `json:"media_type,omitempty"`
	Data      string `json:"data,omitempty"`
	URL       string `json:"url,omitempty"`
	Content   blocks `json:"content,omitempty"`
}

type anthropicResponse struct {
	ID         string         `json:"id"`
	Model      string         `json:"model"`
	Content    blocks         `json:"content"`
	StopReason string         `json:"stop_reason"`
	Usage      anthropicUsage `json:"usage"`
}

type anthropicUsage struct {
	InputTokens              int `json:"input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens,omitempty"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens,omitempty"`
}

// anthropicOutRequest is what we send to an Anthropic upstream.
type anthropicOutRequest struct {
	Model         string                `json:"model"`
	System        string                `json:"system,omitempty"`
	Messages      []anthropicOutMessage `json:"messages"`
	MaxTokens     int                   `json:"max_tokens"`
	Temperature   *float64              `json:"temperature,omitempty"`
	TopP          *float64              `json:"top_p,omitempty"`
	StopSequences []string              `json:"stop_sequences,omitempty"`
	Stream        bool                  `json:"stream,omitempty"`
	Tools         []anthropicTool       `json:"tools,omitempty"`
	ToolChoice    *anthropicToolChoice  `json:"tool_choice,omitempty"`
	Metadata      *anthropicMetadata    `json:"metadata,omitempty"`
}

type anthropicOutMessage struct {
	Role    string `json:"role"`
	Content []obj  `json:"content"`
}

// obj is a JSON object we build field by field, so empty strings survive.
type obj = map[string]any

// ---- OpenAI Responses API ----

type responsesRequest struct {
	Model              string          `json:"model"`
	Instructions       string          `json:"instructions,omitempty"`
	Input              json.RawMessage `json:"input"`
	MaxOutputTokens    *int            `json:"max_output_tokens,omitempty"`
	Temperature        *float64        `json:"temperature,omitempty"`
	TopP               *float64        `json:"top_p,omitempty"`
	Stream             bool            `json:"stream,omitempty"`
	Tools              []responsesTool `json:"tools,omitempty"`
	ToolChoice         json.RawMessage `json:"tool_choice,omitempty"`
	ParallelToolCalls  *bool           `json:"parallel_tool_calls,omitempty"`
	User               string          `json:"user,omitempty"`
	SafetyIdentifier   string          `json:"safety_identifier,omitempty"`
	PreviousResponseID string          `json:"previous_response_id,omitempty"`
	Conversation       json.RawMessage `json:"conversation,omitempty"`
}

// responsesItem is one input or output item: a message, a function call,
// a function call's output, or a kind tokenpool skips.
type responsesItem struct {
	Type      string          `json:"type"`
	ID        string          `json:"id,omitempty"`
	Role      string          `json:"role,omitempty"`
	Content   json.RawMessage `json:"content,omitempty"` // a string or parts
	CallID    string          `json:"call_id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Arguments string          `json:"arguments,omitempty"`
	Output    json.RawMessage `json:"output,omitempty"` // a string or parts
}

type responsesPart struct {
	Type     string `json:"type"`
	Text     string `json:"text,omitempty"`
	Refusal  string `json:"refusal,omitempty"`
	ImageURL string `json:"image_url,omitempty"`
	FileData string `json:"file_data,omitempty"`
	FileURL  string `json:"file_url,omitempty"`
}

type responsesTool struct {
	Type        string          `json:"type"`
	Name        string          `json:"name,omitempty"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
}

// responsesOutRequest is what we send to a Responses upstream. Store is
// always false: the next turn may go to another upstream, so nothing
// stored would be used.
type responsesOutRequest struct {
	Model             string          `json:"model"`
	Instructions      string          `json:"instructions,omitempty"`
	Input             []obj           `json:"input"`
	MaxOutputTokens   *int            `json:"max_output_tokens,omitempty"`
	Temperature       *float64        `json:"temperature,omitempty"`
	TopP              *float64        `json:"top_p,omitempty"`
	Stream            bool            `json:"stream,omitempty"`
	Store             bool            `json:"store"`
	Tools             []responsesTool `json:"tools,omitempty"`
	ToolChoice        any             `json:"tool_choice,omitempty"`
	ParallelToolCalls *bool           `json:"parallel_tool_calls,omitempty"`
	User              string          `json:"user,omitempty"`
}

type responsesResponse struct {
	ID                string `json:"id"`
	Model             string `json:"model"`
	Status            string `json:"status"`
	IncompleteDetails *struct {
		Reason string `json:"reason"`
	} `json:"incomplete_details"`
	Output []responsesItem  `json:"output"`
	Usage  *responsesUsage  `json:"usage"`
	Error  *responsesErrObj `json:"error"`
}

func (r *responsesResponse) incompleteReason() string {
	if r.IncompleteDetails == nil {
		return ""
	}
	return r.IncompleteDetails.Reason
}

type responsesErrObj struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type responsesUsage struct {
	InputTokens        int `json:"input_tokens"`
	InputTokensDetails struct {
		CachedTokens int `json:"cached_tokens"`
	} `json:"input_tokens_details"`
	OutputTokens int `json:"output_tokens"`
}

// responsesEvent is one Responses stream event. Delta stays raw: most
// events carry a string there, but not all.
type responsesEvent struct {
	Type        string             `json:"type"`
	Response    *responsesResponse `json:"response"`
	OutputIndex int                `json:"output_index"`
	Item        *responsesItem     `json:"item"`
	Delta       json.RawMessage    `json:"delta"`
	Arguments   string             `json:"arguments"`
	Message     string             `json:"message"`
	Error       *apiError          `json:"error"`
}

func (e responsesEvent) delta() string {
	var s string
	_ = json.Unmarshal(e.Delta, &s)
	return s
}

type apiError struct {
	Type    string `json:"type"`
	Message string `json:"message"`
}

// responsesText extracts the text of Responses content: a string or parts.
// A refusal counts as text.
func responsesText(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var parts []responsesPart
	if err := json.Unmarshal(raw, &parts); err != nil {
		return ""
	}
	var texts []string
	for _, p := range parts {
		switch {
		case p.Text != "" && (p.Type == "input_text" || p.Type == "output_text"):
			texts = append(texts, p.Text)
		case p.Type == "refusal" && p.Refusal != "":
			texts = append(texts, p.Refusal)
		}
	}
	return strings.Join(texts, "\n\n")
}
