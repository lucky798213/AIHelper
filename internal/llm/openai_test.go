package llm

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestOpenAIClientBuildsChatCompletionRequest(t *testing.T) {
	var captured map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			t.Fatalf("path = %q", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer test-key" {
			t.Fatalf("Authorization = %q", got)
		}
		if err := json.NewDecoder(r.Body).Decode(&captured); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"choices": [{
				"finish_reason": "stop",
				"message": {"role": "assistant", "content": "hello"}
			}]
		}`))
	}))
	defer server.Close()

	client, err := NewOpenAIClient(OpenAIConfig{
		BaseURL:      server.URL,
		APIKey:       "test-key",
		DefaultModel: "default-model",
		Temperature:  0.3,
		MaxTokens:    123,
	})
	if err != nil {
		t.Fatalf("new client: %v", err)
	}

	resp, err := client.CreateMessage(context.Background(), Request{
		Model:  "agent-model",
		System: "system prompt",
		Messages: []Message{{
			Role:    "user",
			Content: "hi",
		}, {
			Role:             "assistant",
			Content:          "",
			ReasoningContent: "thinking before a tool call",
			ToolCalls: []ToolCall{{
				ID:    "call_123",
				Name:  "fake_tool",
				Input: json.RawMessage(`{"input":"build it"}`),
			}},
		}, {
			Role:       "tool",
			Name:       "fake_tool",
			Content:    "done",
			ToolCallID: "call_123",
		}},
		Tools: []ToolSchema{{
			Name:        "fake_tool",
			Description: "fake",
			InputSchema: map[string]any{
				"type": "object",
			},
		}},
	})
	if err != nil {
		t.Fatalf("create message: %v", err)
	}
	if resp.StopReason != StopReasonEndTurn || resp.Text != "hello" {
		t.Fatalf("unexpected response: %#v", resp)
	}
	if captured["model"] != "agent-model" {
		t.Fatalf("model = %#v", captured["model"])
	}
	if captured["tool_choice"] != "auto" {
		t.Fatalf("tool_choice = %#v", captured["tool_choice"])
	}
	if captured["max_tokens"].(float64) != 123 {
		t.Fatalf("max_tokens = %#v", captured["max_tokens"])
	}
	messages := captured["messages"].([]any)
	if len(messages) != 4 {
		t.Fatalf("messages len = %d", len(messages))
	}
	if messages[0].(map[string]any)["role"] != "system" {
		t.Fatalf("first message = %#v", messages[0])
	}
	assistantMessage := messages[2].(map[string]any)
	if assistantMessage["reasoning_content"] != "thinking before a tool call" {
		t.Fatalf("assistant reasoning_content = %#v", assistantMessage["reasoning_content"])
	}
	tools := captured["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("tools len = %d", len(tools))
	}
}

func TestOpenAIClientParsesToolCalls(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"choices": [{
				"finish_reason": "tool_calls",
				"message": {
					"role": "assistant",
					"reasoning_content": "I should call a tool.",
					"content": "",
					"tool_calls": [{
						"id": "call_123",
						"type": "function",
						"function": {
							"name": "fake_tool",
							"arguments": "{\"input\":\"build it\"}"
						}
					}]
				}
			}]
		}`))
	}))
	defer server.Close()

	client, err := NewOpenAIClient(OpenAIConfig{
		BaseURL:      server.URL,
		APIKey:       "test-key",
		DefaultModel: "default-model",
	})
	if err != nil {
		t.Fatalf("new client: %v", err)
	}

	resp, err := client.CreateMessage(context.Background(), Request{
		Messages: []Message{{Role: "user", Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("create message: %v", err)
	}
	if resp.StopReason != StopReasonToolUse {
		t.Fatalf("StopReason = %q", resp.StopReason)
	}
	if resp.ReasoningContent != "I should call a tool." {
		t.Fatalf("ReasoningContent = %q", resp.ReasoningContent)
	}
	if len(resp.ToolCalls) != 1 {
		t.Fatalf("ToolCalls len = %d", len(resp.ToolCalls))
	}
	call := resp.ToolCalls[0]
	if call.ID != "call_123" || call.Name != "fake_tool" {
		t.Fatalf("unexpected tool call: %#v", call)
	}
	if string(call.Input) != `{"input":"build it"}` {
		t.Fatalf("input = %s", call.Input)
	}
}

func TestOpenAIClientRequestsJSONForDispatch(t *testing.T) {
	var captured map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&captured); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"choices": [{
				"finish_reason": "stop",
				"message": {"role": "assistant", "content": "{\"mode\":\"direct\",\"input\":\"hi\"}"}
			}]
		}`))
	}))
	defer server.Close()

	client, err := NewOpenAIClient(OpenAIConfig{
		BaseURL:      server.URL,
		APIKey:       "test-key",
		DefaultModel: "default-model",
	})
	if err != nil {
		t.Fatalf("new client: %v", err)
	}

	if _, err := client.CreateMessage(context.Background(), Request{
		Purpose:  "dispatch",
		Messages: []Message{{Role: "user", Content: "hi"}},
	}); err != nil {
		t.Fatalf("create message: %v", err)
	}

	responseFormat := captured["response_format"].(map[string]any)
	if responseFormat["type"] != "json_object" {
		t.Fatalf("response_format = %#v", responseFormat)
	}
}

func TestOpenAIClientReturnsHTTPErrorBody(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "bad key", http.StatusUnauthorized)
	}))
	defer server.Close()

	client, err := NewOpenAIClient(OpenAIConfig{
		BaseURL:      server.URL,
		APIKey:       "test-key",
		DefaultModel: "default-model",
	})
	if err != nil {
		t.Fatalf("new client: %v", err)
	}

	_, err = client.CreateMessage(context.Background(), Request{
		Messages: []Message{{Role: "user", Content: "hi"}},
	})
	if err == nil {
		t.Fatal("expected error")
	}
	var httpErr *HTTPError
	if !errors.As(err, &httpErr) {
		t.Fatalf("expected HTTPError, got %T", err)
	}
	if httpErr.StatusCode != http.StatusUnauthorized || httpErr.Body == "" {
		t.Fatalf("unexpected HTTPError: %#v", httpErr)
	}
}
