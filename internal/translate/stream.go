package translate

import (
	"bufio"
	"cmp"
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

// event writes a named event: "event: name" plus JSON data.
func (s *sseWriter) event(name string, v any) {
	b, _ := json.Marshal(v)
	s.write("event: " + name + "\ndata: " + string(b) + "\n\n")
}

// ResponsesStreamToAnthropic reads a Responses API stream from r and writes
// the equivalent Messages API stream to w. model names the message if the
// upstream events do not.
func ResponsesStreamToAnthropic(w io.Writer, flush func(), r io.Reader, model string) error {
	c := &responsesToAnthropic{out: sseWriter{w: w, flush: flush}, model: model, byIndex: map[int]*pendingTool{}}
	err := readSSE(r, c.handle)
	switch {
	case errors.Is(err, errDone):
		err = nil
	case err == nil:
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

type responsesToAnthropic struct {
	out        sseWriter
	model      string
	started    bool
	index      int // next content block index
	textOpen   bool
	tools      []*pendingTool
	byIndex    map[int]*pendingTool // output_index -> tool
	status     string
	incomplete string
	usage      *responsesUsage
}

func (c *responsesToAnthropic) handle(ev sseEvent) error {
	if strings.TrimSpace(ev.Data) == "[DONE]" {
		return nil // only response.completed or response.incomplete ends it
	}
	var e responsesEvent
	if err := json.Unmarshal([]byte(ev.Data), &e); err != nil {
		return fmt.Errorf("bad upstream event: %w", err)
	}
	switch e.Type {
	case "response.created", "response.in_progress":
		if r := e.Response; r != nil {
			c.start(r.ID, r.Model)
		}
	case "response.output_text.delta", "response.refusal.delta":
		if d := e.delta(); d != "" {
			c.start("", "")
			c.text(d)
		}
	case "response.output_item.added", "response.output_item.done":
		if it := e.Item; it != nil && it.Type == "function_call" {
			c.tool(e.OutputIndex, it)
		}
	case "response.function_call_arguments.delta":
		if t := c.byIndex[e.OutputIndex]; t != nil {
			t.args.WriteString(e.delta())
		}
	case "response.function_call_arguments.done":
		if t := c.byIndex[e.OutputIndex]; t != nil && e.Arguments != "" {
			t.args.Reset()
			t.args.WriteString(e.Arguments)
		}
	case "response.completed", "response.incomplete":
		c.status = strings.TrimPrefix(e.Type, "response.")
		if r := e.Response; r != nil {
			c.start(r.ID, r.Model)
			c.status = cmp.Or(r.Status, c.status)
			c.incomplete, c.usage = r.incompleteReason(), r.Usage
		}
		return errDone
	case "response.failed":
		if r := e.Response; r != nil && r.Error != nil && r.Error.Message != "" {
			return errors.New(r.Error.Message)
		}
		return errors.New("upstream response failed")
	case "error":
		return errors.New(cmp.Or(e.Message, errorText(e.Error), "upstream error"))
	case "":
		if e.Error != nil {
			return errors.New(cmp.Or(e.Error.Message, "upstream error"))
		}
	}
	return c.out.err
}

func errorText(e *apiError) string {
	if e == nil {
		return ""
	}
	return e.Message
}

func (c *responsesToAnthropic) start(id, model string) {
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

func (c *responsesToAnthropic) text(s string) {
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

func (c *responsesToAnthropic) closeText() {
	if c.textOpen {
		c.out.event("content_block_stop", obj{"type": "content_block_stop", "index": c.index})
		c.index++
		c.textOpen = false
	}
}

// tool records a function call item. Argument deltas add to it, and the
// final arguments replace whatever the deltas built. Parallel calls may
// interleave, which Anthropic blocks cannot express, so each call is
// emitted whole at the end.
func (c *responsesToAnthropic) tool(index int, it *responsesItem) {
	t := c.byIndex[index]
	if t == nil {
		t = &pendingTool{}
		c.byIndex[index] = t
		c.tools = append(c.tools, t)
	}
	t.id = cmp.Or(t.id, it.CallID, it.ID)
	t.name = cmp.Or(t.name, it.Name)
	if it.Arguments != "" {
		t.args.Reset()
		t.args.WriteString(it.Arguments)
	}
}

func (c *responsesToAnthropic) end() {
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
	c.out.event("message_delta", obj{
		"type":  "message_delta",
		"delta": obj{"stop_reason": StopReason(c.status, c.incomplete, len(c.tools) > 0), "stop_sequence": nil},
		"usage": usageToAnthropic(c.usage),
	})
	c.out.event("message_stop", obj{"type": "message_stop"})
}

func (c *responsesToAnthropic) fail(err error) error {
	c.out.event("error", obj{"type": "error", "error": obj{"type": "api_error", "message": err.Error()}})
	return err
}

// AnthropicStreamToResponses reads a Messages API stream from r and writes
// the equivalent Responses API stream to w: each text block becomes a
// message item and each tool use a function_call item. Thinking is
// dropped, as in AnthropicResponseToResponses.
func AnthropicStreamToResponses(w io.Writer, flush func(), r io.Reader) error {
	c := &anthropicToResponses{
		out: sseWriter{w: w, flush: flush}, created: time.Now().Unix(), open: map[int]*streamItem{},
	}
	err := readSSE(r, c.handle)
	switch {
	case errors.Is(err, errDone):
		return c.out.err
	case err == nil:
		err = ErrIncomplete
	}
	c.emit("error", obj{"code": "server_error", "message": err.Error(), "param": nil})
	return err
}

type anthropicToResponses struct {
	out       sseWriter
	seq       int
	id, model string
	created   int64
	open      map[int]*streamItem // content block index -> item being streamed
	output    []obj               // finished items
	next      int                 // next output_index
	usage     anthropicUsage
	stop      string
}

// streamItem is an output item being streamed: a message's text, or a
// function call's arguments.
type streamItem struct {
	index        int
	id           string
	call         bool
	callID, name string
	buf          strings.Builder
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
		StopReason  string `json:"stop_reason"`
	} `json:"delta"`
	Usage *anthropicUsage `json:"usage"`
	Error *apiError       `json:"error"`
}

func (c *anthropicToResponses) handle(ev sseEvent) error {
	var e anthropicEvent
	if err := json.Unmarshal([]byte(ev.Data), &e); err != nil {
		return fmt.Errorf("bad upstream event: %w", err)
	}
	switch e.Type {
	case "message_start":
		if m := e.Message; m != nil {
			c.id, c.model, c.usage = m.ID, m.Model, m.Usage
		}
		inProgress := responseObject(c.id, c.model, c.created, "in_progress", "", []obj{}, nil)
		c.emit("response.created", obj{"response": inProgress})
		c.emit("response.in_progress", obj{"response": inProgress})
	case "content_block_start":
		b := e.ContentBlock
		if b == nil || (b.Type != "text" && b.Type != "tool_use") {
			break
		}
		it := &streamItem{index: c.next, call: b.Type == "tool_use", callID: b.ID, name: b.Name}
		c.next++
		c.open[e.Index] = it
		if it.call {
			it.id = "fc_" + b.ID
			c.emit("response.output_item.added", obj{
				"output_index": it.index, "item": functionCallItem(b.ID, b.Name, "", "in_progress"),
			})
			break
		}
		it.id = itemID(c.id, e.Index)
		c.emit("response.output_item.added", obj{"output_index": it.index, "item": messageItem(it.id, "in_progress")})
		c.emit("response.content_part.added", obj{
			"item_id": it.id, "output_index": it.index, "content_index": 0, "part": outputText(""),
		})
		if b.Text != "" {
			c.textDelta(it, b.Text)
		}
	case "content_block_delta":
		it, d := c.open[e.Index], e.Delta
		switch {
		case it == nil || d == nil:
		case d.Type == "text_delta" && !it.call:
			c.textDelta(it, d.Text)
		case d.Type == "input_json_delta" && it.call:
			it.buf.WriteString(d.PartialJSON)
			c.emit("response.function_call_arguments.delta", obj{
				"item_id": it.id, "output_index": it.index, "delta": d.PartialJSON,
			})
		}
	case "content_block_stop":
		if it := c.open[e.Index]; it != nil {
			delete(c.open, e.Index)
			c.finish(it)
		}
	case "message_delta":
		if e.Delta != nil && e.Delta.StopReason != "" {
			c.stop = e.Delta.StopReason
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
		status, incomplete := responseStatus(c.stop)
		output := append([]obj{}, c.output...)
		c.emit("response."+status, obj{
			"response": responseObject(c.id, c.model, c.created, status, incomplete, output, &c.usage),
		})
		return errDone
	case "error":
		return errors.New(cmp.Or(errorText(e.Error), "upstream error"))
	}
	return c.out.err
}

func (c *anthropicToResponses) textDelta(it *streamItem, text string) {
	it.buf.WriteString(text)
	c.emit("response.output_text.delta", obj{
		"item_id": it.id, "output_index": it.index, "content_index": 0, "delta": text,
	})
}

// finish closes an item with its done events and keeps it for the final
// response.
func (c *anthropicToResponses) finish(it *streamItem) {
	var done obj
	if it.call {
		args := argumentsString(json.RawMessage(it.buf.String()))
		c.emit("response.function_call_arguments.done", obj{
			"item_id": it.id, "output_index": it.index, "arguments": args,
		})
		done = functionCallItem(it.callID, it.name, args, "completed")
	} else {
		text := it.buf.String()
		c.emit("response.output_text.done", obj{
			"item_id": it.id, "output_index": it.index, "content_index": 0, "text": text,
		})
		c.emit("response.content_part.done", obj{
			"item_id": it.id, "output_index": it.index, "content_index": 0, "part": outputText(text),
		})
		done = messageItem(it.id, "completed", text)
	}
	c.emit("response.output_item.done", obj{"output_index": it.index, "item": done})
	c.output = append(c.output, done)
}

// emit writes one Responses event, numbered in order.
func (c *anthropicToResponses) emit(typ string, fields obj) {
	fields["type"] = typ
	fields["sequence_number"] = c.seq
	c.seq++
	c.out.event(typ, fields)
}
