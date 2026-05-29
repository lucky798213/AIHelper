package tools

import (
	"context"
	"encoding/json"

	"AIHelper/internal/llm"
)

type CallContext struct {
	AgentID    string
	SessionKey string
}

type ToolResult struct {
	Content string
}

type Tool interface {
	Name() string
	Schema() llm.ToolSchema
	Call(ctx context.Context, call CallContext, input json.RawMessage) (ToolResult, error)
}
