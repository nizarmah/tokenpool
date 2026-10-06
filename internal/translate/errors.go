package translate

import (
	"encoding/json"
	"strings"
)

// ErrorMessage pulls a readable message out of an upstream error body,
// whichever API shape it uses.
func ErrorMessage(body []byte) string {
	var shaped struct {
		Error   json.RawMessage `json:"error"`
		Message string          `json:"message"`
		Detail  string          `json:"detail"`
	}
	if err := json.Unmarshal(body, &shaped); err == nil {
		var inner apiError
		if json.Unmarshal(shaped.Error, &inner) == nil && inner.Message != "" {
			return inner.Message
		}
		var s string
		if json.Unmarshal(shaped.Error, &s) == nil && s != "" {
			return s
		}
		if shaped.Message != "" {
			return shaped.Message
		}
		if shaped.Detail != "" {
			return shaped.Detail
		}
	}
	msg := strings.TrimSpace(string(body))
	if len(msg) > 300 {
		msg = msg[:300] + "…"
	}
	return msg
}

// AnthropicError renders an error body the Anthropic SDK understands.
func AnthropicError(status int, message string) []byte {
	b, _ := json.Marshal(obj{
		"type":  "error",
		"error": obj{"type": anthropicErrorType(status), "message": message},
	})
	return b
}

// OpenAIError renders an error body the OpenAI SDK understands.
func OpenAIError(status int, message string) []byte {
	b, _ := json.Marshal(obj{
		"error": obj{"type": openaiErrorType(status), "message": message, "code": nil},
	})
	return b
}

func anthropicErrorType(status int) string {
	switch status {
	case 413:
		return "request_too_large"
	case 401:
		return "authentication_error"
	case 403:
		return "permission_error"
	case 404:
		return "not_found_error"
	case 429:
		return "rate_limit_error"
	case 503, 529:
		return "overloaded_error"
	}
	if status >= 500 {
		return "api_error"
	}
	return "invalid_request_error"
}

func openaiErrorType(status int) string {
	switch {
	case status == 401:
		return "authentication_error"
	case status == 403:
		return "permission_error"
	case status == 404:
		return "not_found_error"
	case status == 429:
		return "rate_limit_exceeded"
	case status >= 500:
		return "server_error"
	}
	return "invalid_request_error"
}
