package tools

import (
	"context"
	"encoding/json"
	"fmt"

	"AIHelper/internal/llm"
)

type MemoryService interface {
	WriteMemory(ctx context.Context, agentID, content, category string) (string, error)
	SearchMemory(ctx context.Context, agentID, query string, topK int) (string, error)
}

type MemoryWriteTool struct {
	service MemoryService
}

type memoryWriteInput struct {
	Content  string `json:"content"`
	Category string `json:"category"`
}

func NewMemoryWriteTool(service MemoryService) *MemoryWriteTool {
	return &MemoryWriteTool{service: service}
}

func NewMemoryWriteToolFactory(deps Dependencies) (Tool, error) {
	if deps.MemoryService == nil {
		return nil, ErrToolUnavailable
	}
	return NewMemoryWriteTool(deps.MemoryService), nil
}

func (t *MemoryWriteTool) Name() string {
	return "memory_write"
}

func (t *MemoryWriteTool) Schema() llm.ToolSchema {
	return llm.ToolSchema{
		Name:        t.Name(),
		Description: "Save an important stable fact, preference, or project observation to this agent's long-term memory.",
		InputSchema: map[string]any{
			"type":     "object",
			"required": []string{"content"},
			"properties": map[string]any{
				"content": map[string]any{
					"type":        "string",
					"description": "The fact or observation to remember.",
				},
				"category": map[string]any{
					"type":        "string",
					"description": "Optional category such as preference, fact, project, or decision.",
				},
			},
		},
	}
}

func (t *MemoryWriteTool) Call(ctx context.Context, call CallContext, raw json.RawMessage) (ToolResult, error) {
	var input memoryWriteInput
	if err := json.Unmarshal(raw, &input); err != nil {
		return ToolResult{}, fmt.Errorf("decode memory_write input: %w", err)
	}
	result, err := t.service.WriteMemory(ctx, call.AgentID, input.Content, input.Category)
	if err != nil {
		return ToolResult{}, err
	}
	return ToolResult{Content: result}, nil
}

type MemorySearchTool struct {
	service MemoryService
}

type memorySearchInput struct {
	Query string `json:"query"`
	TopK  int    `json:"top_k"`
}

func NewMemorySearchTool(service MemoryService) *MemorySearchTool {
	return &MemorySearchTool{service: service}
}

func NewMemorySearchToolFactory(deps Dependencies) (Tool, error) {
	if deps.MemoryService == nil {
		return nil, ErrToolUnavailable
	}
	return NewMemorySearchTool(deps.MemoryService), nil
}

func (t *MemorySearchTool) Name() string {
	return "memory_search"
}

func (t *MemorySearchTool) Schema() llm.ToolSchema {
	return llm.ToolSchema{
		Name:        t.Name(),
		Description: "Search this agent's long-term memory for relevant information.",
		InputSchema: map[string]any{
			"type":     "object",
			"required": []string{"query"},
			"properties": map[string]any{
				"query": map[string]any{
					"type":        "string",
					"description": "Search query.",
				},
				"top_k": map[string]any{
					"type":        "integer",
					"description": "Maximum results to return. Defaults to this agent's memory_top_k.",
				},
			},
		},
	}
}

func (t *MemorySearchTool) Call(ctx context.Context, call CallContext, raw json.RawMessage) (ToolResult, error) {
	var input memorySearchInput
	if err := json.Unmarshal(raw, &input); err != nil {
		return ToolResult{}, fmt.Errorf("decode memory_search input: %w", err)
	}
	result, err := t.service.SearchMemory(ctx, call.AgentID, input.Query, input.TopK)
	if err != nil {
		return ToolResult{}, err
	}
	return ToolResult{Content: result}, nil
}
