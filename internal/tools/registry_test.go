package tools

import (
	"context"
	"encoding/json"
	"testing"

	"AIHelper/internal/llm"
)

type fakeTool struct{}

func (fakeTool) Name() string {
	return "fake_tool"
}

func (fakeTool) Schema() llm.ToolSchema {
	return llm.ToolSchema{
		Name:        "fake_tool",
		Description: "fake",
		InputSchema: map[string]any{
			"type": "object",
		},
	}
}

func (fakeTool) Call(ctx context.Context, call CallContext, input json.RawMessage) (ToolResult, error) {
	return ToolResult{Content: "ok"}, nil
}

func TestRegistryAgentScopedTools(t *testing.T) {
	registry := NewRegistry()
	if err := registry.Register(fakeTool{}); err != nil {
		t.Fatalf("register: %v", err)
	}
	if err := registry.SetAllowedTools("agent-a", []string{"fake_tool"}); err != nil {
		t.Fatalf("set allowed: %v", err)
	}
	if err := registry.SetAllowedTools("agent-b", nil); err != nil {
		t.Fatalf("set allowed empty: %v", err)
	}

	if _, ok := registry.GetForAgent("agent-a", "fake_tool"); !ok {
		t.Fatal("expected agent-a to access fake_tool")
	}
	if _, ok := registry.GetForAgent("agent-b", "fake_tool"); ok {
		t.Fatal("expected agent-b to be denied fake_tool")
	}
	if got := registry.SchemasForAgent("agent-a"); len(got) != 1 {
		t.Fatalf("agent-a schemas len = %d, want 1", len(got))
	}
	if got := registry.SchemasForAgent("agent-b"); len(got) != 0 {
		t.Fatalf("agent-b schemas len = %d, want 0", len(got))
	}
}

func TestRegistryRejectsUnknownAllowedTool(t *testing.T) {
	registry := NewRegistry()
	if err := registry.SetAllowedTools("agent-a", []string{"missing_tool"}); err == nil {
		t.Fatal("expected unknown tool to fail")
	}
}

func TestRegisterAllRegistersFileAndMemoryTools(t *testing.T) {
	registry := NewRegistry()
	if err := RegisterAll(registry, Dependencies{
		BaseDir:       t.TempDir(),
		MemoryService: &fakeMemoryService{},
	}); err != nil {
		t.Fatalf("register all: %v", err)
	}

	for _, name := range []string{"read_file", "edit_file", "write_file", "list_files", "memory_write", "memory_search"} {
		if _, ok := registry.Get(name); !ok {
			t.Fatalf("expected tool %q to be registered", name)
		}
	}
}

func TestRegisterAllRegistersSkillTools(t *testing.T) {
	registry := NewRegistry()
	if err := RegisterAll(registry, Dependencies{
		BaseDir:      t.TempDir(),
		SkillService: fakeSkillService{},
	}); err != nil {
		t.Fatalf("register all: %v", err)
	}
	for _, name := range []string{"read_skill_reference", "run_skill_command"} {
		if _, ok := registry.Get(name); !ok {
			t.Fatalf("expected tool %q to be registered", name)
		}
	}
}

func TestRegisterAllSkipsUnavailableMemoryTools(t *testing.T) {
	registry := NewRegistry()
	if err := RegisterAll(registry, Dependencies{BaseDir: t.TempDir()}); err != nil {
		t.Fatalf("register all: %v", err)
	}
	for _, name := range []string{"read_file", "edit_file", "write_file", "list_files"} {
		if _, ok := registry.Get(name); !ok {
			t.Fatalf("expected tool %q to be registered", name)
		}
	}
	if _, ok := registry.Get("memory_write"); ok {
		t.Fatal("memory_write should be skipped without memory service")
	}
	if _, ok := registry.Get("memory_search"); ok {
		t.Fatal("memory_search should be skipped without memory service")
	}
}

type fakeSkillService struct{}

func (fakeSkillService) ReadSkillReference(ctx context.Context, agentID, skillName, path string) (string, error) {
	return "reference", nil
}

func (fakeSkillService) RunSkillCommand(ctx context.Context, agentID, skillName, command string) (string, error) {
	return "command", nil
}
