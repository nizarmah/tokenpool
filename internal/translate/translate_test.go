package translate

import (
	"bytes"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
)

// sameJSON fails unless got and want are equal JSON values.
func sameJSON(t *testing.T, got []byte, want string) {
	t.Helper()
	var g, w any
	if err := json.Unmarshal(got, &g); err != nil {
		t.Fatalf("got invalid JSON %s: %v", got, err)
	}
	if err := json.Unmarshal([]byte(want), &w); err != nil {
		t.Fatalf("bad test JSON: %v", err)
	}
	if !reflect.DeepEqual(g, w) {
		pretty, _ := json.MarshalIndent(g, "", "  ")
		t.Errorf("got:\n%s\nwant:\n%s", pretty, want)
	}
}

func TestAnthropicToResponses(t *testing.T) {
	in := `{
	  "model": "claude-sonnet-5", "max_tokens": 1024, "stream": true, "temperature": 0.5,
	  "stop_sequences": ["END"], "metadata": {"user_id": "u1"},
	  "system": [{"type": "text", "text": "Be brief.", "cache_control": {"type": "ephemeral"}}],
	  "thinking": {"type": "enabled", "budget_tokens": 2000},
	  "tools": [
	    {"name": "read", "description": "Read a file", "input_schema": {"type": "object"}},
	    {"type": "web_search_20250305", "name": "web_search"}
	  ],
	  "tool_choice": {"type": "any", "disable_parallel_tool_use": true},
	  "messages": [
	    {"role": "user", "content": "Read a.txt"},
	    {"role": "assistant", "content": [
	      {"type": "thinking", "thinking": "hmm", "signature": "sig"},
	      {"type": "text", "text": "Reading."},
	      {"type": "tool_use", "id": "toolu_1", "name": "read", "input": {"path": "a.txt"}}
	    ]},
	    {"role": "user", "content": [
	      {"type": "tool_result", "tool_use_id": "toolu_1", "content": [
	        {"type": "text", "text": "hello"},
	        {"type": "image", "source": {"type": "base64", "media_type": "image/png", "data": "AAA"}}
	      ]},
	      {"type": "text", "text": "What does it say?"}
	    ]},
	    {"role": "assistant", "content": [{"type": "tool_use", "id": "toolu_2", "name": "read", "input": {}}]},
	    {"role": "user", "content": [{"type": "tool_result", "tool_use_id": "toolu_2", "content": "boom", "is_error": true}]}
	  ]
	}`
	got, err := AnthropicToResponses([]byte(in))
	if err != nil {
		t.Fatal(err)
	}
	sameJSON(t, got, `{
	  "model": "claude-sonnet-5", "max_output_tokens": 1024, "stream": true, "temperature": 0.5,
	  "store": false, "user": "u1", "instructions": "Be brief.",
	  "tools": [{"type": "function", "name": "read", "description": "Read a file", "parameters": {"type": "object"}}],
	  "tool_choice": "required", "parallel_tool_calls": false,
	  "input": [
	    {"role": "user", "content": "Read a.txt"},
	    {"role": "assistant", "content": "Reading."},
	    {"type": "function_call", "call_id": "toolu_1", "name": "read", "arguments": "{\"path\":\"a.txt\"}"},
	    {"type": "function_call_output", "call_id": "toolu_1", "output": "hello"},
	    {"role": "user", "content": [
	      {"type": "input_image", "image_url": "data:image/png;base64,AAA"},
	      {"type": "input_text", "text": "What does it say?"}
	    ]},
	    {"type": "function_call", "call_id": "toolu_2", "name": "read", "arguments": "{}"},
	    {"type": "function_call_output", "call_id": "toolu_2", "output": "Error: boom"}
	  ]
	}`)
}

func TestResponsesToAnthropic(t *testing.T) {
	in := `{
	  "model": "gpt-5", "max_output_tokens": 500, "temperature": 1.5, "store": true,
	  "parallel_tool_calls": false, "safety_identifier": "u1", "reasoning": {"effort": "low"},
	  "instructions": "Be brief.",
	  "tools": [{"type": "function", "name": "read", "parameters": {"type": "object"}},
	            {"type": "function", "name": "now"},
	            {"type": "web_search"}],
	  "input": [
	    {"role": "developer", "content": [{"type": "input_text", "text": "Use tools."}]},
	    {"role": "user", "content": [
	      {"type": "input_text", "text": "Look:"},
	      {"type": "input_image", "image_url": "data:image/jpeg;base64,BBB"},
	      {"type": "input_image", "image_url": "https://example.com/x.png"},
	      {"type": "input_file", "filename": "a.pdf", "file_data": "data:application/pdf;base64,CCC"}
	    ]},
	    {"type": "reasoning", "id": "rs_1", "summary": []},
	    {"type": "message", "id": "msg_1", "role": "assistant", "status": "completed",
	     "content": [{"type": "output_text", "text": "Checking.", "annotations": []}]},
	    {"type": "function_call", "id": "fc_1", "call_id": "call.1", "name": "read", "arguments": "{\"path\":\"a\"}"},
	    {"type": "function_call", "call_id": "call_2", "name": "now", "arguments": ""},
	    {"type": "function_call_output", "call_id": "call.1", "output": "file body"},
	    {"type": "function_call_output", "call_id": "call_2", "output": [{"type": "input_text", "text": "noon"}]},
	    {"role": "user", "content": "Thanks"}
	  ]
	}`
	got, err := ResponsesToAnthropic([]byte(in), 8192)
	if err != nil {
		t.Fatal(err)
	}
	sameJSON(t, got, `{
	  "model": "gpt-5", "max_tokens": 500, "temperature": 1,
	  "system": "Be brief.\n\nUse tools.", "metadata": {"user_id": "u1"},
	  "tools": [{"name": "read", "input_schema": {"type": "object"}},
	            {"name": "now", "input_schema": {"type": "object", "properties": {}}}],
	  "tool_choice": {"type": "auto", "disable_parallel_tool_use": true},
	  "messages": [
	    {"role": "user", "content": [
	      {"type": "text", "text": "Look:"},
	      {"type": "image", "source": {"type": "base64", "media_type": "image/jpeg", "data": "BBB"}},
	      {"type": "image", "source": {"type": "url", "url": "https://example.com/x.png"}},
	      {"type": "document", "source": {"type": "base64", "media_type": "application/pdf", "data": "CCC"}}
	    ]},
	    {"role": "assistant", "content": [
	      {"type": "text", "text": "Checking."},
	      {"type": "tool_use", "id": "call_1", "name": "read", "input": {"path": "a"}},
	      {"type": "tool_use", "id": "call_2", "name": "now", "input": {}}
	    ]},
	    {"role": "user", "content": [
	      {"type": "tool_result", "tool_use_id": "call_1", "content": "file body"},
	      {"type": "tool_result", "tool_use_id": "call_2", "content": [{"type": "text", "text": "noon"}]},
	      {"type": "text", "text": "Thanks"}
	    ]}
	  ]
	}`)

	minimal, err := ResponsesToAnthropic([]byte(`{"model":"m","input":"hi","tool_choice":"required"}`), 4096)
	if err != nil {
		t.Fatal(err)
	}
	sameJSON(t, minimal, `{"model":"m","max_tokens":4096,"messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}]}`)

	for _, stateful := range []string{
		`{"model":"m","previous_response_id":"resp_1","input":"more"}`,
		`{"model":"m","conversation":"conv_1","input":"more"}`,
		`{"model":"m","input":[{"type":"item_reference","id":"msg_1"}]}`,
	} {
		if _, err := ResponsesToAnthropic([]byte(stateful), 4096); !errors.Is(err, ErrStateful) {
			t.Errorf("ResponsesToAnthropic(%s) err = %v, want ErrStateful", stateful, err)
		}
	}
}

func TestResponses(t *testing.T) {
	upstream := `{
	  "id": "resp_1", "object": "response", "model": "grok-4", "status": "completed",
	  "output": [
	    {"type": "reasoning", "id": "rs_1", "summary": [{"type": "summary_text", "text": "hmm"}]},
	    {"type": "message", "id": "msg_a", "role": "assistant", "status": "completed",
	     "content": [{"type": "output_text", "text": "Let me check.", "annotations": []}]},
	    {"type": "function_call", "id": "fc_1", "call_id": "call_1", "name": "read",
	     "arguments": "{\"path\":\"a\"}", "status": "completed"}
	  ],
	  "usage": {"input_tokens": 100, "input_tokens_details": {"cached_tokens": 40},
	            "output_tokens": 20, "output_tokens_details": {"reasoning_tokens": 5}, "total_tokens": 120}
	}`
	got, err := ResponsesResponseToAnthropic([]byte(upstream))
	if err != nil {
		t.Fatal(err)
	}
	sameJSON(t, got, `{
	  "id": "msg_resp_1", "type": "message", "role": "assistant", "model": "grok-4",
	  "content": [
	    {"type": "text", "text": "Let me check."},
	    {"type": "tool_use", "id": "call_1", "name": "read", "input": {"path": "a"}}
	  ],
	  "stop_reason": "tool_use", "stop_sequence": null,
	  "usage": {"input_tokens": 60, "output_tokens": 20, "cache_read_input_tokens": 40}
	}`)

	cut, err := ResponsesResponseToAnthropic([]byte(`{"id":"r2","model":"m","status":"incomplete",
	  "incomplete_details":{"reason":"max_output_tokens"},"output":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	sameJSON(t, cut, `{"id":"msg_r2","type":"message","role":"assistant","model":"m","content":[],
	  "stop_reason":"max_tokens","stop_sequence":null,"usage":{"input_tokens":0,"output_tokens":0}}`)

	if _, err := ResponsesResponseToAnthropic([]byte(`{"status":"failed","error":{"code":"server_error","message":"boom"}}`)); err == nil || err.Error() != "boom" {
		t.Errorf("failed response err = %v", err)
	}

	anthropic := `{
	  "id": "msg_1", "type": "message", "role": "assistant", "model": "claude-sonnet-5",
	  "content": [
	    {"type": "thinking", "thinking": "plan", "signature": "s"},
	    {"type": "text", "text": "Done"},
	    {"type": "tool_use", "id": "toolu_1", "name": "read", "input": {"path": "a"}}
	  ],
	  "stop_reason": "tool_use",
	  "usage": {"input_tokens": 10, "output_tokens": 5, "cache_read_input_tokens": 90}
	}`
	got, err = AnthropicResponseToResponses([]byte(anthropic))
	if err != nil {
		t.Fatal(err)
	}
	var r map[string]any
	_ = json.Unmarshal(got, &r)
	if created, ok := r["created_at"].(float64); !ok || created <= 0 {
		t.Errorf("created_at = %v", r["created_at"])
	}
	delete(r, "created_at")
	got, _ = json.Marshal(r)
	sameJSON(t, got, `{
	  "id": "resp_1", "object": "response", "status": "completed", "model": "claude-sonnet-5",
	  "incomplete_details": null, "error": null,
	  "output": [
	    {"type": "message", "id": "msg_1_1", "status": "completed", "role": "assistant",
	     "content": [{"type": "output_text", "text": "Done", "annotations": []}]},
	    {"type": "function_call", "id": "fc_toolu_1", "call_id": "toolu_1", "name": "read",
	     "arguments": "{\"path\":\"a\"}", "status": "completed"}
	  ],
	  "usage": {"input_tokens": 100, "input_tokens_details": {"cached_tokens": 90}, "output_tokens": 5,
	            "output_tokens_details": {"reasoning_tokens": 0}, "total_tokens": 105}
	}`)

	got, err = AnthropicResponseToResponses([]byte(`{"id":"msg_2","model":"m","content":[{"type":"text","text":"cut"}],
	  "stop_reason":"max_tokens","usage":{"input_tokens":1,"output_tokens":2}}`))
	if err != nil {
		t.Fatal(err)
	}
	_ = json.Unmarshal(got, &r)
	if r["status"] != "incomplete" || r["incomplete_details"].(map[string]any)["reason"] != "max_output_tokens" {
		t.Errorf("max_tokens response = %s", got)
	}
}

type event struct {
	name string
	data map[string]any
}

func parseEvents(t *testing.T, stream string) []event {
	t.Helper()
	var out []event
	err := readSSE(strings.NewReader(stream), func(ev sseEvent) error {
		e := event{name: ev.Event}
		if ev.Data != "[DONE]" {
			if err := json.Unmarshal([]byte(ev.Data), &e.data); err != nil {
				t.Fatalf("bad event data %q: %v", ev.Data, err)
			}
		} else {
			e.name = "[DONE]"
		}
		out = append(out, e)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// sse joins events into a stream: each entry is "name" + "\n" + data, or
// data alone for an unnamed event.
func sse(events ...string) string {
	var b strings.Builder
	for _, e := range events {
		if name, data, ok := strings.Cut(e, "\n"); ok {
			b.WriteString("event: " + name + "\ndata: " + data + "\n\n")
		} else {
			b.WriteString("data: " + e + "\n\n")
		}
	}
	return b.String()
}

func TestResponsesStreamToAnthropic(t *testing.T) {
	upstream := sse(
		"response.created\n"+`{"type":"response.created","sequence_number":0,"response":{"id":"resp_1","model":"grok-4","status":"in_progress","output":[]}}`,
		"response.output_item.added\n"+`{"type":"response.output_item.added","output_index":0,"item":{"type":"reasoning","id":"rs_1","summary":[]}}`,
		"response.reasoning_summary_text.delta\n"+`{"type":"response.reasoning_summary_text.delta","item_id":"rs_1","output_index":0,"summary_index":0,"delta":"hmm"}`,
		"response.output_item.added\n"+`{"type":"response.output_item.added","output_index":1,"item":{"type":"message","id":"msg_a","role":"assistant","status":"in_progress","content":[]}}`,
		"response.output_text.delta\n"+`{"type":"response.output_text.delta","item_id":"msg_a","output_index":1,"content_index":0,"delta":"Hel"}`,
		"response.output_text.delta\n"+`{"type":"response.output_text.delta","item_id":"msg_a","output_index":1,"content_index":0,"delta":"lo"}`,
		"response.output_item.added\n"+`{"type":"response.output_item.added","output_index":2,"item":{"type":"function_call","id":"fc_a","call_id":"call_a","name":"read","arguments":"","status":"in_progress"}}`,
		"response.output_item.added\n"+`{"type":"response.output_item.added","output_index":3,"item":{"type":"function_call","id":"fc_b","call_id":"call_b","name":"now","arguments":"","status":"in_progress"}}`,
		"response.function_call_arguments.delta\n"+`{"type":"response.function_call_arguments.delta","item_id":"fc_a","output_index":2,"delta":"{\"pa"}`,
		"response.function_call_arguments.delta\n"+`{"type":"response.function_call_arguments.delta","item_id":"fc_a","output_index":2,"delta":"th\":\"a\"}"}`,
		"response.output_item.done\n"+`{"type":"response.output_item.done","output_index":2,"item":{"type":"function_call","id":"fc_a","call_id":"call_a","name":"read","arguments":"{\"path\":\"a\"}","status":"completed"}}`,
		"response.completed\n"+`{"type":"response.completed","response":{"id":"resp_1","model":"grok-4","status":"completed","output":[],"usage":{"input_tokens":11,"output_tokens":7,"total_tokens":18}}}`,
	)
	var out bytes.Buffer
	if err := ResponsesStreamToAnthropic(&out, nil, strings.NewReader(upstream), "fallback"); err != nil {
		t.Fatal(err)
	}
	events := parseEvents(t, out.String())
	var seq []string
	for _, e := range events {
		if e.name != e.data["type"] {
			t.Errorf("event %q carries type %v", e.name, e.data["type"])
		}
		seq = append(seq, e.name)
	}
	want := "message_start content_block_start content_block_delta content_block_delta content_block_stop " +
		"content_block_start content_block_delta content_block_stop content_block_start content_block_stop " +
		"message_delta message_stop"
	if got := strings.Join(seq, " "); got != want {
		t.Fatalf("events:\n%s\nwant:\n%s", got, want)
	}
	msg := events[0].data["message"].(map[string]any)
	if msg["id"] != "msg_resp_1" || msg["model"] != "grok-4" {
		t.Errorf("message_start = %v", msg)
	}
	tool := events[5].data["content_block"].(map[string]any)
	if tool["id"] != "call_a" || tool["name"] != "read" || events[5].data["index"] != 1.0 {
		t.Errorf("first tool block = %v", events[5].data)
	}
	if args := events[6].data["delta"].(map[string]any)["partial_json"]; args != `{"path":"a"}` {
		t.Errorf("tool args = %v", args)
	}
	if events[8].data["content_block"].(map[string]any)["name"] != "now" {
		t.Errorf("second tool block = %v", events[8].data)
	}
	delta := events[10].data
	if delta["delta"].(map[string]any)["stop_reason"] != "tool_use" {
		t.Errorf("message_delta = %v", delta)
	}
	if u := delta["usage"].(map[string]any); u["input_tokens"] != 11.0 || u["output_tokens"] != 7.0 {
		t.Errorf("usage = %v", u)
	}
}

func TestResponsesStreamCutShort(t *testing.T) {
	// Unnamed events, as some servers send them, ending at max_output_tokens.
	upstream := sse(
		`{"type":"response.output_text.delta","output_index":0,"content_index":0,"delta":"Hi"}`,
		`{"type":"response.incomplete","response":{"id":"r1","model":"m","status":"incomplete","incomplete_details":{"reason":"max_output_tokens"}}}`,
		`[DONE]`,
	)
	var out bytes.Buffer
	if err := ResponsesStreamToAnthropic(&out, nil, strings.NewReader(upstream), "m"); err != nil {
		t.Fatal(err)
	}
	events := parseEvents(t, out.String())
	delta := events[len(events)-2].data["delta"].(map[string]any)
	if delta["stop_reason"] != "max_tokens" {
		t.Errorf("stop_reason = %v", delta["stop_reason"])
	}
}

func TestResponsesStreamIncomplete(t *testing.T) {
	upstream := sse(`{"type":"response.output_text.delta","output_index":0,"delta":"Hi"}`, `[DONE]`)
	var out bytes.Buffer
	err := ResponsesStreamToAnthropic(&out, nil, strings.NewReader(upstream), "m")
	if !errors.Is(err, ErrIncomplete) {
		t.Fatalf("err = %v", err)
	}
	events := parseEvents(t, out.String())
	if last := events[len(events)-1]; last.name != "error" {
		t.Errorf("last event = %s, want error", last.name)
	}
}

func TestResponsesStreamUpstreamError(t *testing.T) {
	for _, upstream := range []string{
		sse("error\n" + `{"type":"error","code":"server_error","message":"model overloaded","param":null}`),
		sse("response.failed\n" + `{"type":"response.failed","response":{"status":"failed","error":{"code":"server_error","message":"model overloaded"}}}`),
		sse(`{"error":{"message":"model overloaded","type":"server_error"}}`),
	} {
		var out bytes.Buffer
		if err := ResponsesStreamToAnthropic(&out, nil, strings.NewReader(upstream), "m"); err == nil {
			t.Fatal("want error")
		}
		if !strings.Contains(out.String(), "model overloaded") {
			t.Errorf("error not relayed: %s", out.String())
		}
	}
}

func TestAnthropicStreamToResponses(t *testing.T) {
	upstream := `event: message_start
data: {"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","model":"claude-sonnet-5","content":[],"usage":{"input_tokens":12,"output_tokens":1}}}

event: ping
data: {"type":"ping"}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"plan"}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

event: content_block_start
data: {"type":"content_block_start","index":1,"content_block":{"type":"text","text":""}}

event: content_block_delta
data: {"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"Hi"}}

event: content_block_stop
data: {"type":"content_block_stop","index":1}

event: content_block_start
data: {"type":"content_block_start","index":2,"content_block":{"type":"tool_use","id":"toolu_1","name":"read","input":{}}}

event: content_block_delta
data: {"type":"content_block_delta","index":2,"delta":{"type":"input_json_delta","partial_json":"{\"path\":"}}

event: content_block_delta
data: {"type":"content_block_delta","index":2,"delta":{"type":"input_json_delta","partial_json":"\"a\"}"}}

event: content_block_stop
data: {"type":"content_block_stop","index":2}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"tool_use","stop_sequence":null},"usage":{"output_tokens":9}}

event: message_stop
data: {"type":"message_stop"}

`
	var out bytes.Buffer
	if err := AnthropicStreamToResponses(&out, nil, strings.NewReader(upstream)); err != nil {
		t.Fatal(err)
	}
	events := parseEvents(t, out.String())
	var seq []string
	for i, e := range events {
		if e.name != e.data["type"] {
			t.Errorf("event %q carries type %v", e.name, e.data["type"])
		}
		if e.data["sequence_number"] != float64(i) {
			t.Errorf("event %d has sequence_number %v", i, e.data["sequence_number"])
		}
		seq = append(seq, strings.TrimPrefix(e.name, "response."))
	}
	want := "created in_progress " +
		"output_item.added content_part.added output_text.delta output_text.done content_part.done output_item.done " +
		"output_item.added function_call_arguments.delta function_call_arguments.delta function_call_arguments.done output_item.done " +
		"completed"
	if got := strings.Join(seq, " "); got != want {
		t.Fatalf("events:\n%s\nwant:\n%s", got, want)
	}
	if r := events[0].data["response"].(map[string]any); r["id"] != "resp_1" || r["status"] != "in_progress" {
		t.Errorf("created = %v", r)
	}
	text := events[2].data
	if text["output_index"] != 0.0 || text["item"].(map[string]any)["type"] != "message" {
		t.Errorf("text item = %v", text)
	}
	if events[4].data["delta"] != "Hi" || events[5].data["text"] != "Hi" {
		t.Errorf("text events = %v, %v", events[4].data, events[5].data)
	}
	call := events[8].data["item"].(map[string]any)
	if events[8].data["output_index"] != 1.0 || call["call_id"] != "toolu_1" || call["name"] != "read" {
		t.Errorf("call item = %v", events[8].data)
	}
	if args := events[11].data["arguments"]; args != `{"path":"a"}` {
		t.Errorf("args = %v", args)
	}
	final := events[13].data["response"].(map[string]any)
	if final["status"] != "completed" || len(final["output"].([]any)) != 2 {
		t.Errorf("completed = %v", final)
	}
	usage := final["usage"].(map[string]any)
	if usage["input_tokens"] != 12.0 || usage["output_tokens"] != 9.0 {
		t.Errorf("usage = %v", usage)
	}
}

func TestAnthropicStreamError(t *testing.T) {
	upstream := "event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"overloaded_error\",\"message\":\"Overloaded\"}}\n\n"
	var out bytes.Buffer
	if err := AnthropicStreamToResponses(&out, nil, strings.NewReader(upstream)); err == nil {
		t.Fatal("want error")
	}
	events := parseEvents(t, out.String())
	if len(events) != 1 || events[0].name != "error" || events[0].data["message"] != "Overloaded" {
		t.Errorf("out = %s", out.String())
	}
}

func TestRewrite(t *testing.T) {
	body := []byte(`{"model":"a","max_tokens":64000,"extra":{"keep":true}}`)
	got, err := Rewrite(body, "b", 8000)
	if err != nil {
		t.Fatal(err)
	}
	sameJSON(t, got, `{"model":"b","max_tokens":8000,"extra":{"keep":true}}`)

	same, _ := Rewrite(body, "a", 0)
	if !bytes.Equal(same, body) {
		t.Error("unchanged body was re-encoded")
	}
	capped, _ := Rewrite([]byte(`{"model":"a","max_output_tokens":64000}`), "", 8000)
	sameJSON(t, capped, `{"model":"a","max_output_tokens":8000}`)
	under, _ := Rewrite([]byte(`{"model":"a","max_output_tokens":10}`), "", 8000)
	sameJSON(t, under, `{"model":"a","max_output_tokens":10}`)
}

func TestErrorMessage(t *testing.T) {
	tests := map[string]string{
		`{"type":"error","error":{"type":"rate_limit_error","message":"slow down"}}`: "slow down",
		`{"error":{"message":"quota","type":"insufficient_quota"}}`:                  "quota",
		`{"error":"plain"}`: "plain",
		`{"detail":"nope"}`: "nope",
		`upstream exploded`: "upstream exploded",
	}
	for body, want := range tests {
		if got := ErrorMessage([]byte(body)); got != want {
			t.Errorf("ErrorMessage(%s) = %q, want %q", body, got, want)
		}
	}
}

func TestToolID(t *testing.T) {
	if got := ToolID("call.abc:1"); got != "call_abc_1" {
		t.Errorf("ToolID = %q", got)
	}
}
