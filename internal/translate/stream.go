package translate

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
)

// maxSSELine bounds one server-sent event line (a single large chunk).
const maxSSELine = 32 << 20

var errDone = errors.New("stream done")

// ErrIncomplete means the upstream stream ended before its final event.
var ErrIncomplete = errors.New("upstream stream ended before completion")

type sseEvent struct {
	Event string
	Data  string
}

// readSSE calls fn for each server-sent event in r. fn returns errDone to
// stop early.
func readSSE(r io.Reader, fn func(sseEvent) error) error {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64<<10), maxSSELine)
	var ev sseEvent
	var data []string
	pending := false
	dispatch := func() error {
		if !pending {
			return nil
		}
		ev.Data = strings.Join(data, "\n")
		err := fn(ev)
		ev, data, pending = sseEvent{}, data[:0], false
		return err
	}
	for sc.Scan() {
		line := sc.Text()
		if line == "" {
			if err := dispatch(); err != nil {
				return err
			}
			continue
		}
		if strings.HasPrefix(line, ":") {
			continue
		}
		field, value, _ := strings.Cut(line, ":")
		value = strings.TrimPrefix(value, " ")
		switch field {
		case "event":
			ev.Event, pending = value, true
		case "data":
			data, pending = append(data, value), true
		}
	}
	if err := sc.Err(); err != nil {
		return err
	}
	return dispatch()
}

// sseWriter writes events and flushes after each one. The first write
// error sticks, so a vanished client stops the stream.
type sseWriter struct {
	w     io.Writer
	flush func()
	err   error
}

func (s *sseWriter) write(text string) {
	if s.err != nil {
		return
	}
	if _, s.err = io.WriteString(s.w, text); s.err == nil && s.flush != nil {
		s.flush()
	}
}

// event writes an Anthropic-style event: "event: name" plus JSON data.
func (s *sseWriter) event(name string, v any) {
	b, _ := json.Marshal(v)
	s.write("event: " + name + "\ndata: " + string(b) + "\n\n")
}

// data writes an OpenAI-style event: JSON data only.
func (s *sseWriter) data(v any) {
	b, _ := json.Marshal(v)
	s.write("data: " + string(b) + "\n\n")
}

// OpenAIStreamToAnthropic reads a Chat Completions stream from r and
// writes the equivalent Messages API stream to w. model names the message
// if the upstream chunks do not.
func OpenAIStreamToAnthropic(w io.Writer, flush func(), r io.Reader, model string) error {
	c := &openaiToAnthropic{out: sseWriter{w: w, flush: flush}, model: model, byIndex: map[int]*pendingTool{}}
	err := readSSE(r, c.handle)
	switch {
	case errors.Is(err, errDone):
		err = nil
	case err == nil && c.finish == "":
		err = ErrIncomplete
	}
	if err != nil {
		return c.fail(err)
	}
	c.end()
	return c.out.err
}

type pendingTool struct {
	id, name string
	args     strings.Builder
}

type openaiToAnthropic struct {
	out      sseWriter
	model    string
	started  bool
	index    int // next content block index
	textOpen bool
	tools    []*pendingTool
	byIndex  map[int]*pendingTool
	finish   string
	usage    *openaiUsage
}

func (c *openaiToAnthropic) handle(ev sseEvent) error {
	if strings.TrimSpace(ev.Data) == "[DONE]" {
		return errDone
	}
	var chunk openaiChunk
	if err := json.Unmarshal([]byte(ev.Data), &chunk); err != nil {
		return fmt.Errorf("bad upstream chunk: %w", err)
	}
	if chunk.Error != nil {
		return errors.New(chunk.Error.Message)
	}
	c.start(chunk.ID, chunk.Model)
	for _, choice := range chunk.Choices {
		if choice.Index != 0 {
			continue
		}
		if choice.Delta.Content != "" {
			c.text(choice.Delta.Content)
		}
		for _, tc := range choice.Delta.ToolCalls {
			c.tool(tc)
		}
		if choice.FinishReason != nil && *choice.FinishReason != "" {
			c.finish = *choice.FinishReason
		}
	}
	if chunk.Usage != nil {
		c.usage = chunk.Usage
	}
	return c.out.err
}

func (c *openaiToAnthropic) start(id, model string) {
	if c.started {
		return
	}
	c.started = true
	if model == "" {
		model = c.model
	}
	c.out.event("message_start", obj{"type": "message_start", "message": obj{
		"id": MessageID(id), "type": "message", "role": "assistant", "model": model,
		"content": []obj{}, "stop_reason": nil, "stop_sequence": nil,
		"usage": obj{"input_tokens": 0, "output_tokens": 0},
	}})
}

func (c *openaiToAnthropic) text(s string) {
	if !c.textOpen {
		c.out.event("content_block_start", obj{
			"type": "content_block_start", "index": c.index,
			"content_block": obj{"type": "text", "text": ""},
		})
		c.textOpen = true
	}
	c.out.event("content_block_delta", obj{
		"type": "content_block_delta", "index": c.index,
		"delta": obj{"type": "text_delta", "text": s},
	})
}

func (c *openaiToAnthropic) closeText() {
	if c.textOpen {
		c.out.event("content_block_stop", obj{"type": "content_block_stop", "index": c.index})
		c.index++
		c.textOpen = false
	}
}

// tool buffers tool call fragments. Providers may interleave fragments of
// parallel calls, which Anthropic blocks cannot express, so each call is
// emitted whole at the end.
func (c *openaiToAnthropic) tool(tc openaiToolCall) {
	var t *pendingTool
	switch {
	case tc.Index != nil:
		t = c.byIndex[*tc.Index]
		if t == nil {
			t = &pendingTool{}
			c.byIndex[*tc.Index] = t
			c.tools = append(c.tools, t)
		}
	case tc.ID != "":
		for _, existing := range c.tools {
			if existing.id == tc.ID {
				t = existing
			}
		}
	case len(c.tools) > 0:
		t = c.tools[len(c.tools)-1]
	}
	if t == nil {
		t = &pendingTool{}
		c.tools = append(c.tools, t)
	}
	if t.id == "" {
		t.id = tc.ID
	}
	if t.name == "" {
		t.name = tc.Function.Name
	}
	t.args.WriteString(tc.Function.Arguments)
}

func (c *openaiToAnthropic) end() {
	c.start("", "")
	c.closeText()
	for i, t := range c.tools {
		id := t.id
		if id == "" {
			id = fmt.Sprintf("toolu_tokenpool_%d_%d", time.Now().UnixNano(), i)
		}
		c.out.event("content_block_start", obj{
			"type": "content_block_start", "index": c.index,
			"content_block": obj{"type": "tool_use", "id": ToolID(id), "name": t.name, "input": obj{}},
		})
		if args := strings.TrimSpace(t.args.String()); args != "" && json.Valid([]byte(args)) {
			c.out.event("content_block_delta", obj{
				"type": "content_block_delta", "index": c.index,
				"delta": obj{"type": "input_json_delta", "partial_json": args},
			})
		}
		c.out.event("content_block_stop", obj{"type": "content_block_stop", "index": c.index})
		c.index++
	}
	usage := usageToAnthropic(c.usage)
	c.out.event("message_delta", obj{
		"type":  "message_delta",
		"delta": obj{"stop_reason": StopReason(c.finish, len(c.tools) > 0), "stop_sequence": nil},
		"usage": usage,
	})
	c.out.event("message_stop", obj{"type": "message_stop"})
}

func (c *openaiToAnthropic) fail(err error) error {
	c.out.event("error", obj{"type": "error", "error": obj{"type": "api_error", "message": err.Error()}})
	return err
}

// AnthropicStreamToOpenAI reads a Messages API stream from r and writes the
// equivalent Chat Completions stream to w. includeUsage adds the final
// usage chunk that stream_options.include_usage asks for.
func AnthropicStreamToOpenAI(w io.Writer, flush func(), r io.Reader, includeUsage bool) error {
	c := &anthropicToOpenAI{
		out: sseWriter{w: w, flush: flush}, includeUsage: includeUsage,
		created: time.Now().Unix(), tools: map[int]int{},
	}
	err := readSSE(r, c.handle)
	switch {
	case errors.Is(err, errDone):
		return c.out.err
	case err == nil:
		err = ErrIncomplete
	}
	c.out.data(obj{"error": obj{"type": "server_error", "message": err.Error()}})
	c.out.write("data: [DONE]\n\n")
	return err
}

type anthropicToOpenAI struct {
	out          sseWriter
	includeUsage bool
	id, model    string
	created      int64
	tools        map[int]int // content block index -> tool call index
	usage        anthropicUsage
	finish       string
}

type anthropicEvent struct {
	Type         string             `json:"type"`
	Message      *anthropicResponse `json:"message"`
	Index        int                `json:"index"`
	ContentBlock *block             `json:"content_block"`
	Delta        *struct {
		Type        string `json:"type"`
		Text        string `json:"text"`
		PartialJSON string `json:"partial_json"`
		Thinking    string `json:"thinking"`
		StopReason  string `json:"stop_reason"`
	} `json:"delta"`
	Usage *anthropicUsage `json:"usage"`
	Error *apiError       `json:"error"`
}

func (c *anthropicToOpenAI) handle(ev sseEvent) error {
	var e anthropicEvent
	if err := json.Unmarshal([]byte(ev.Data), &e); err != nil {
		return fmt.Errorf("bad upstream event: %w", err)
	}
	switch e.Type {
	case "message_start":
		if m := e.Message; m != nil {
			c.id, c.model, c.usage = m.ID, m.Model, m.Usage
		}
		c.chunk(obj{"role": "assistant", "content": ""}, nil)
	case "content_block_start":
		b := e.ContentBlock
		switch {
		case b == nil:
		case b.Type == "tool_use":
			i := len(c.tools)
			c.tools[e.Index] = i
			c.chunk(obj{"tool_calls": []obj{{
				"index": i, "id": b.ID, "type": "function",
				"function": obj{"name": b.Name, "arguments": ""},
			}}}, nil)
		case b.Type == "text" && b.Text != "":
			c.chunk(obj{"content": b.Text}, nil)
		}
	case "content_block_delta":
		d := e.Delta
		if d == nil {
			break
		}
		switch d.Type {
		case "text_delta":
			c.chunk(obj{"content": d.Text}, nil)
		case "thinking_delta":
			c.chunk(obj{"reasoning_content": d.Thinking}, nil)
		case "input_json_delta":
			if i, ok := c.tools[e.Index]; ok {
				c.chunk(obj{"tool_calls": []obj{{
					"index": i, "function": obj{"arguments": d.PartialJSON},
				}}}, nil)
			}
		}
	case "message_delta":
		if e.Delta != nil && e.Delta.StopReason != "" {
			c.finish = FinishReason(e.Delta.StopReason)
		}
		if u := e.Usage; u != nil {
			c.usage.OutputTokens = u.OutputTokens
			if u.InputTokens > 0 {
				c.usage.InputTokens = u.InputTokens
			}
			if u.CacheReadInputTokens > 0 {
				c.usage.CacheReadInputTokens = u.CacheReadInputTokens
			}
			if u.CacheCreationInputTokens > 0 {
				c.usage.CacheCreationInputTokens = u.CacheCreationInputTokens
			}
		}
	case "message_stop":
		finish := c.finish
		if finish == "" {
			finish = "stop"
		}
		c.chunk(obj{}, &finish)
		if c.includeUsage {
			c.out.data(obj{
				"id": c.id, "object": "chat.completion.chunk", "created": c.created, "model": c.model,
				"choices": []obj{}, "usage": usageToOpenAI(c.usage),
			})
		}
		c.out.write("data: [DONE]\n\n")
		return errDone
	case "error":
		msg := "upstream error"
		if e.Error != nil {
			msg = e.Error.Message
		}
		return errors.New(msg)
	}
	return c.out.err
}

func (c *anthropicToOpenAI) chunk(delta obj, finish *string) {
	c.out.data(obj{
		"id": c.id, "object": "chat.completion.chunk", "created": c.created, "model": c.model,
		"choices": []obj{{"index": 0, "delta": delta, "finish_reason": finish}},
	})
}
