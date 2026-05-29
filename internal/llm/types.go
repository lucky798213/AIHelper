package llm

import "encoding/json"

type StopReason string

const (
	StopReasonEndTurn StopReason = "end_turn"
	StopReasonToolUse StopReason = "tool_use"
)

type Message struct {
	Role             string
	Name             string
	Content          string
	ReasoningContent string
	ToolCallID       string
	ToolCalls        []ToolCall
}

type ToolSchema struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"input_schema"`
}

type ToolCall struct {
	ID    string
	Name  string
	Input json.RawMessage
}

type Request struct {
	AgentID   string
	AgentRole string
	Purpose   string
	Model     string
	System    string
	Messages  []Message
	Tools     []ToolSchema
}

type Response struct {
	StopReason       StopReason
	Text             string
	ReasoningContent string
	ToolCalls        []ToolCall
}
