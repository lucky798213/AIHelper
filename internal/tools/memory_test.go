package tools

import (
	"context"
	"encoding/json"
	"testing"
)

type fakeMemoryService struct {
	writes []memoryWriteInput
}

func (f *fakeMemoryService) WriteMemory(ctx context.Context, agentID, content, category string) (string, error) {
	f.writes = append(f.writes, memoryWriteInput{Content: agentID + ":" + content, Category: category})
	return "saved", nil
}

func (f *fakeMemoryService) SearchMemory(ctx context.Context, agentID, query string, topK int) (string, error) {
	return agentID + ":" + query, nil
}

func TestMemoryToolsCallService(t *testing.T) {
	service := &fakeMemoryService{}
	writeTool := NewMemoryWriteTool(service)
	raw, _ := json.Marshal(memoryWriteInput{Content: "remember blue", Category: "preference"})
	result, err := writeTool.Call(context.Background(), CallContext{AgentID: "agent-a"}, raw)
	if err != nil {
		t.Fatalf("write call: %v", err)
	}
	if result.Content != "saved" || len(service.writes) != 1 || service.writes[0].Content != "agent-a:remember blue" {
		t.Fatalf("unexpected write result=%#v writes=%#v", result, service.writes)
	}

	searchTool := NewMemorySearchTool(service)
	raw, _ = json.Marshal(memorySearchInput{Query: "blue"})
	result, err = searchTool.Call(context.Background(), CallContext{AgentID: "agent-a"}, raw)
	if err != nil {
		t.Fatalf("search call: %v", err)
	}
	if result.Content != "agent-a:blue" {
		t.Fatalf("search result = %#v", result)
	}
}

func TestMemoryToolsRespectAgentAllowlist(t *testing.T) {
	registry := NewRegistry()
	if err := RegisterAll(registry, Dependencies{
		BaseDir:       t.TempDir(),
		MemoryService: &fakeMemoryService{},
	}); err != nil {
		t.Fatalf("register tools: %v", err)
	}
	if err := registry.SetAllowedTools("agent-a", []string{"memory_write"}); err != nil {
		t.Fatalf("set allowed agent-a: %v", err)
	}
	if err := registry.SetAllowedTools("agent-b", nil); err != nil {
		t.Fatalf("set allowed agent-b: %v", err)
	}
	if _, ok := registry.GetForAgent("agent-a", "memory_write"); !ok {
		t.Fatal("expected agent-a to access memory_write")
	}
	if _, ok := registry.GetForAgent("agent-b", "memory_write"); ok {
		t.Fatal("expected agent-b to be denied memory_write")
	}
}
