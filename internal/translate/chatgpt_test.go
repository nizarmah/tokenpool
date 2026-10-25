package translate

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestForChatGPT(t *testing.T) {
	in := `{"model":"gpt-5-codex","input":"hi","instructions":"be brief","max_output_tokens":100,
		"temperature":0.2,"top_p":1,"user":"u1","store":true,"stream":false,"tools":[]}`
	out, err := ForChatGPT([]byte(in))
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	if got["store"] != false || got["stream"] != true || got["instructions"] != "be brief" || got["model"] != "gpt-5-codex" {
		t.Errorf("shaped = %s", out)
	}
	for _, gone := range []string{"max_output_tokens", "temperature", "top_p", "user"} {
		if _, ok := got[gone]; ok {
			t.Errorf("%s was kept: %s", gone, out)
		}
	}
	out, _ = ForChatGPT([]byte(`{"model":"m","input":"hello"}`))
	if !strings.Contains(string(out), `"input":[{"content":[{"text":"hello","type":"input_text"}],"role":"user","type":"message"}]`) {
		t.Errorf("string input = %s", out)
	}
	out, _ = ForChatGPT([]byte(`{"model":"m","input":[],"instructions":""}`))
	if strings.Contains(string(out), "instructions") {
		t.Errorf("empty instructions kept: %s", out)
	}
	if _, err := ForChatGPT([]byte(`[1]`)); err == nil {
		t.Error("want an error for a non-object body")
	}
}

func chatSSE(events ...string) string {
	var b strings.Builder
	for _, e := range events {
		var head struct{ Type string }
		_ = json.Unmarshal([]byte(e), &head)
		b.WriteString("event: " + head.Type + "\ndata: " + e + "\n\n")
	}
	return b.String()
}

func TestCollectResponsesStream(t *testing.T) {
	stream := chatSSE(
		`{"type":"response.created","response":{"id":"r1","model":"gpt-5-codex","status":"in_progress","output":[]}}`,
		`{"type":"response.output_text.delta","output_index":0,"delta":"hel"}`,
		`{"type":"response.output_item.done","output_index":1,"item":{"type":"function_call","call_id":"c1","name":"ls","arguments":"{}"}}`,
		`{"type":"response.output_item.done","output_index":0,"item":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"hello"}]}}`,
		`{"type":"response.completed","response":{"id":"r1","model":"gpt-5-codex","status":"completed","output":[],"usage":{"input_tokens":3,"output_tokens":2,"total_tokens":5}}}`,
	)
	out, err := CollectResponsesStream(strings.NewReader(stream))
	if err != nil {
		t.Fatal(err)
	}
	var r struct {
		ID     string `json:"id"`
		Output []struct {
			Type string `json:"type"`
		} `json:"output"`
		Usage struct {
			OutputTokens int `json:"output_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(out, &r); err != nil {
		t.Fatal(err)
	}
	if r.ID != "r1" || len(r.Output) != 2 || r.Output[0].Type != "message" || r.Output[1].Type != "function_call" || r.Usage.OutputTokens != 2 {
		t.Errorf("collected = %s", out)
	}
	// And it converts for an Anthropic client.
	msg, err := ResponsesResponseToAnthropic(out)
	if err != nil || !strings.Contains(string(msg), `"text":"hello"`) || !strings.Contains(string(msg), `"tool_use"`) {
		t.Errorf("as a message = %s, %v", msg, err)
	}

	// A final response that lists its output keeps it.
	full := chatSSE(`{"type":"response.completed","response":{"id":"r2","status":"completed","output":[{"type":"message","content":[]}]}}`)
	if out, err := CollectResponsesStream(strings.NewReader(full)); err != nil || !strings.Contains(string(out), `"r2"`) {
		t.Errorf("full = %s, %v", out, err)
	}

	failed := chatSSE(`{"type":"response.failed","response":{"status":"failed","error":{"message":"The usage limit has been reached"}}}`)
	if _, err := CollectResponsesStream(strings.NewReader(failed)); err == nil || !strings.Contains(err.Error(), "usage limit") {
		t.Errorf("failed: %v", err)
	}
	cut := chatSSE(`{"type":"response.created","response":{"id":"r3"}}`)
	if _, err := CollectResponsesStream(strings.NewReader(cut)); !errors.Is(err, ErrIncomplete) {
		t.Errorf("cut stream: %v", err)
	}
}
