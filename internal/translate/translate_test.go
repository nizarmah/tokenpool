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

func TestAnthropicToOpenAI(t *testing.T) {
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
	got, err := AnthropicToOpenAI([]byte(in))
	if err != nil {
		t.Fatal(err)
	}
	sameJSON(t, got, `{
	  "model": "claude-sonnet-5", "max_tokens": 1024, "stream": true, "temperature": 0.5,
	  "stream_options": {"include_usage": true}, "stop": ["END"], "user": "u1",
	  "tools": [{"type": "function", "function": {"name": "read", "description": "Read a file", "parameters": {"type": "object"}}}],
	  "tool_choice": "required", "parallel_tool_calls": false,
	  "messages": [
	    {"role": "system", "content": "Be brief."},
	    {"role": "user", "content": "Read a.txt"},
	    {"role": "assistant", "content": "Reading.", "tool_calls": [
	      {"id": "toolu_1", "type": "function", "function": {"name": "read", "arguments": "{\"path\":\"a.txt\"}"}}
	    ]},
	    {"role": "tool", "tool_call_id": "toolu_1", "content": "hello"},
	    {"role": "user", "content": [
	      {"type": "image_url", "image_url": {"url": "data:image/png;base64,AAA"}},
	      {"type": "text", "text": "What does it say?"}
	    ]},
	    {"role": "assistant", "content": null, "tool_calls": [
	      {"id": "toolu_2", "type": "function", "function": {"name": "read", "arguments": "{}"}}
	    ]},
	    {"role": "tool", "tool_call_id": "toolu_2", "content": "Error: boom"}
	  ]
	}`)
}

func TestOpenAIToAnthropic(t *testing.T) {
	in := `{
	  "model": "gpt-4o", "max_completion_tokens": 500, "temperature": 1.5, "stop": "END",
	  "parallel_tool_calls": false, "user": "u1", "n": 1, "response_format": {"type": "text"},
	  "tools": [{"type": "function", "function": {"name": "read", "parameters": {"type": "object"}}},
	            {"type": "function", "function": {"name": "now"}}],
	  "messages": [
	    {"role": "system", "content": "Be brief."},
	    {"role": "developer", "content": [{"type": "text", "text": "Use tools."}]},
	    {"role": "user", "content": [
	      {"type": "text", "text": "Look:"},
	      {"type": "image_url", "image_url": {"url": "data:image/jpeg;base64,BBB"}},
	      {"type": "image_url", "image_url": {"url": "https://example.com/x.png"}}
	    ]},
	    {"role": "assistant", "content": null, "tool_calls": [
	      {"id": "call.1", "type": "function", "function": {"name": "read", "arguments": "{\"path\":\"a\"}"}},
	      {"id": "call_2", "type": "function", "function": {"name": "now", "arguments": ""}}
	    ]},
	    {"role": "tool", "tool_call_id": "call.1", "content": "file body"},
	    {"role": "tool", "tool_call_id": "call_2", "content": "noon"},
	    {"role": "user", "content": "Thanks"}
	  ]
	}`
	got, err := OpenAIToAnthropic([]byte(in), 8192)
	if err != nil {
		t.Fatal(err)
	}
	sameJSON(t, got, `{
	  "model": "gpt-4o", "max_tokens": 500, "temperature": 1, "stop_sequences": ["END"],
	  "system": "Be brief.\n\nUse tools.", "metadata": {"user_id": "u1"},
	  "tools": [{"name": "read", "input_schema": {"type": "object"}},
	            {"name": "now", "input_schema": {"type": "object", "properties": {}}}],
	  "tool_choice": {"type": "auto", "disable_parallel_tool_use": true},
	  "messages": [
	    {"role": "user", "content": [
	      {"type": "text", "text": "Look:"},
	      {"type": "image", "source": {"type": "base64", "media_type": "image/jpeg", "data": "BBB"}},
	      {"type": "image", "source": {"type": "url", "url": "https://example.com/x.png"}}
	    ]},
	    {"role": "assistant", "content": [
	      {"type": "tool_use", "id": "call_1", "name": "read", "input": {"path": "a"}},
	      {"type": "tool_use", "id": "call_2", "name": "now", "input": {}}
	    ]},
	    {"role": "user", "content": [
	      {"type": "tool_result", "tool_use_id": "call_1", "content": "file body"},
	      {"type": "tool_result", "tool_use_id": "call_2", "content": "noon"},
	      {"type": "text", "text": "Thanks"}
	    ]}
	  ]
	}`)

	minimal, err := OpenAIToAnthropic([]byte(`{"model":"m","messages":[{"role":"user","content":"hi"}],"tool_choice":"required"}`), 4096)
	if err != nil {
		t.Fatal(err)
	}
	sameJSON(t, minimal, `{"model":"m","max_tokens":4096,"messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}]}`)
}

func TestResponses(t *testing.T) {
	openai := `{
	  "id": "chatcmpl-1", "model": "grok-4",
	  "choices": [{"index": 0, "finish_reason": "tool_calls", "message": {
	    "role": "assistant", "content": "Let me check.",
	    "tool_calls": [{"id": "call_1", "type": "function", "function": {"name": "read", "arguments": "{\"path\":\"a\"}"}}]
	  }}],
	  "usage": {"prompt_tokens": 100, "completion_tokens": 20, "total_tokens": 120, "prompt_tokens_details": {"cached_tokens": 40}}
	}`
	got, err := OpenAIResponseToAnthropic([]byte(openai))
	if err != nil {
		t.Fatal(err)
	}
	sameJSON(t, got, `{
	  "id": "msg_chatcmpl-1", "type": "message", "role": "assistant", "model": "grok-4",
	  "content": [
	    {"type": "text", "text": "Let me check."},
	    {"type": "tool_use", "id": "call_1", "name": "read", "input": {"path": "a"}}
	  ],
	  "stop_reason": "tool_use", "stop_sequence": null,
	  "usage": {"input_tokens": 60, "output_tokens": 20, "cache_read_input_tokens": 40}
	}`)

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
	got, err = AnthropicResponseToOpenAI([]byte(anthropic))
	if err != nil {
		t.Fatal(err)
	}
	var r map[string]any
	_ = json.Unmarshal(got, &r)
	delete(r, "created")
	got, _ = json.Marshal(r)
	sameJSON(t, got, `{
	  "id": "msg_1", "object": "chat.completion", "model": "claude-sonnet-5",
	  "choices": [{"index": 0, "finish_reason": "tool_calls", "message": {
	    "role": "assistant", "content": "Done", "reasoning_content": "plan",
	    "tool_calls": [{"id": "toolu_1", "type": "function", "function": {"name": "read", "arguments": "{\"path\":\"a\"}"}}]
	  }}],
	  "usage": {"prompt_tokens": 100, "completion_tokens": 5, "total_tokens": 105, "prompt_tokens_details": {"cached_tokens": 90}}
	}`)
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

func TestOpenAIStreamToAnthropic(t *testing.T) {
	upstream := strings.Join([]string{
		`data: {"id":"c1","model":"grok-4","choices":[{"index":0,"delta":{"role":"assistant","content":""}}]}`,
		`data: {"id":"c1","model":"grok-4","choices":[{"index":0,"delta":{"content":"Hel"}}]}`,
		`data: {"id":"c1","model":"grok-4","choices":[{"index":0,"delta":{"content":"lo"}}]}`,
		`data: {"id":"c1","model":"grok-4","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_a","type":"function","function":{"name":"read","arguments":"{\"pa"}}]}}]}`,
		`data: {"id":"c1","model":"grok-4","choices":[{"index":0,"delta":{"tool_calls":[{"index":1,"id":"call_b","type":"function","function":{"name":"now","arguments":""}}]}}]}`,
		`data: {"id":"c1","model":"grok-4","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"th\":\"a\"}"}}]}}]}`,
		`data: {"id":"c1","model":"grok-4","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
		`data: {"id":"c1","model":"grok-4","choices":[],"usage":{"prompt_tokens":11,"completion_tokens":7,"total_tokens":18}}`,
		`data: [DONE]`,
	}, "\n\n") + "\n\n"
	var out bytes.Buffer
	if err := OpenAIStreamToAnthropic(&out, nil, strings.NewReader(upstream), "fallback"); err != nil {
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
	if msg["id"] != "msg_c1" || msg["model"] != "grok-4" {
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

func TestOpenAIStreamIncomplete(t *testing.T) {
	upstream := `data: {"id":"c1","choices":[{"index":0,"delta":{"content":"Hi"}}]}` + "\n\n"
	var out bytes.Buffer
	err := OpenAIStreamToAnthropic(&out, nil, strings.NewReader(upstream), "m")
	if !errors.Is(err, ErrIncomplete) {
		t.Fatalf("err = %v", err)
	}
	events := parseEvents(t, out.String())
	if last := events[len(events)-1]; last.name != "error" {
		t.Errorf("last event = %s, want error", last.name)
	}
}

func TestOpenAIStreamUpstreamError(t *testing.T) {
	upstream := `data: {"error":{"message":"model overloaded","type":"server_error"}}` + "\n\n"
	var out bytes.Buffer
	if err := OpenAIStreamToAnthropic(&out, nil, strings.NewReader(upstream), "m"); err == nil {
		t.Fatal("want error")
	}
	if !strings.Contains(out.String(), "model overloaded") {
		t.Errorf("error not relayed: %s", out.String())
	}
}

func TestAnthropicStreamToOpenAI(t *testing.T) {
	upstream := `event: message_start
data: {"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","model":"claude-sonnet-5","content":[],"usage":{"input_tokens":12,"output_tokens":1}}}

event: ping
data: {"type":"ping"}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hi"}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

event: content_block_start
data: {"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"toolu_1","name":"read","input":{}}}

event: content_block_delta
data: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"path\":"}}

event: content_block_delta
data: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"\"a\"}"}}

event: content_block_stop
data: {"type":"content_block_stop","index":1}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"tool_use","stop_sequence":null},"usage":{"output_tokens":9}}

event: message_stop
data: {"type":"message_stop"}

`
	var out bytes.Buffer
	if err := AnthropicStreamToOpenAI(&out, nil, strings.NewReader(upstream), true); err != nil {
		t.Fatal(err)
	}
	events := parseEvents(t, out.String())
	if len(events) != 8 {
		t.Fatalf("got %d events:\n%s", len(events), out.String())
	}
	delta := func(i int) map[string]any {
		return events[i].data["choices"].([]any)[0].(map[string]any)["delta"].(map[string]any)
	}
	if delta(0)["role"] != "assistant" || events[0].data["id"] != "msg_1" {
		t.Errorf("first chunk = %v", events[0].data)
	}
	if delta(1)["content"] != "Hi" {
		t.Errorf("text chunk = %v", delta(1))
	}
	call := delta(2)["tool_calls"].([]any)[0].(map[string]any)
	if call["id"] != "toolu_1" || call["index"] != 0.0 || call["function"].(map[string]any)["name"] != "read" {
		t.Errorf("tool start = %v", call)
	}
	args := delta(3)["tool_calls"].([]any)[0].(map[string]any)["function"].(map[string]any)["arguments"].(string) +
		delta(4)["tool_calls"].([]any)[0].(map[string]any)["function"].(map[string]any)["arguments"].(string)
	if args != `{"path":"a"}` {
		t.Errorf("args = %s", args)
	}
	final := events[5].data["choices"].([]any)[0].(map[string]any)
	if final["finish_reason"] != "tool_calls" {
		t.Errorf("finish = %v", final)
	}
	usage := events[6].data["usage"].(map[string]any)
	if usage["prompt_tokens"] != 12.0 || usage["completion_tokens"] != 9.0 {
		t.Errorf("usage = %v", usage)
	}
	if events[7].name != "[DONE]" {
		t.Errorf("last = %v", events[7])
	}
}

func TestAnthropicStreamError(t *testing.T) {
	upstream := "event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"overloaded_error\",\"message\":\"Overloaded\"}}\n\n"
	var out bytes.Buffer
	if err := AnthropicStreamToOpenAI(&out, nil, strings.NewReader(upstream), false); err == nil {
		t.Fatal("want error")
	}
	if !strings.Contains(out.String(), "Overloaded") || !strings.HasSuffix(out.String(), "data: [DONE]\n\n") {
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
	under, _ := Rewrite([]byte(`{"model":"a","max_completion_tokens":10}`), "", 8000)
	sameJSON(t, under, `{"model":"a","max_completion_tokens":10}`)
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
