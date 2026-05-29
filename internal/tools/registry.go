package tools

import (
	"errors"
	"fmt"
	"sync"

	"AIHelper/internal/llm"
)

var ErrToolUnavailable = errors.New("tool unavailable")

type Dependencies struct {
	BaseDir       string
	MemoryService MemoryService
	SkillService  SkillService
}

type Factory func(Dependencies) (Tool, error)

type Registry struct {
	mu sync.RWMutex
	//全局工具注册表
	tools map[string]Tool
	//每个 agent 的工具白名单
	allowed map[string]map[string]struct{}
}

func NewRegistry() *Registry {
	return &Registry{
		tools:   make(map[string]Tool),
		allowed: make(map[string]map[string]struct{}),
	}
}

func RegisterAll(registry *Registry, deps Dependencies) error {
	factories := []Factory{
		NewReadFileToolFactory,
		NewEditFileToolFactory,
		NewWriteFileToolFactory,
		NewListFilesToolFactory,
		NewMemoryWriteToolFactory,
		NewMemorySearchToolFactory,
		NewReadSkillReferenceToolFactory,
		NewRunSkillCommandToolFactory,
	}
	for _, factory := range factories {
		tool, err := factory(deps)
		if err != nil {
			if errors.Is(err, ErrToolUnavailable) {
				continue
			}
			return err
		}
		if err := registry.Register(tool); err != nil {
			return err
		}
	}
	return nil
}

func (r *Registry) Register(tool Tool) error {
	if tool == nil {
		return fmt.Errorf("register nil tool")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.tools[tool.Name()]; exists {
		return fmt.Errorf("tool %q already registered", tool.Name())
	}
	r.tools[tool.Name()] = tool
	return nil
}

func (r *Registry) Get(name string) (Tool, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	tool, ok := r.tools[name]
	return tool, ok
}

func (r *Registry) SetAllowedTools(agentID string, names []string) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	allowed := make(map[string]struct{}, len(names))
	for _, name := range names {
		if _, ok := r.tools[name]; !ok {
			return fmt.Errorf("tool %q configured for agent %q is not registered", name, agentID)
		}
		allowed[name] = struct{}{}
	}
	r.allowed[agentID] = allowed
	return nil
}

func (r *Registry) GetForAgent(agentID, name string) (Tool, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	allowed, ok := r.allowed[agentID]
	if !ok {
		return nil, false
	}
	if _, ok := allowed[name]; !ok {
		return nil, false
	}
	tool, ok := r.tools[name]
	return tool, ok
}

// 遍历 agent 的 Tool 白名单，然后找到对应的实例 tools，然后将各个 Tool.Schema塞进数组返回，用于调用 tools
func (r *Registry) SchemasForAgent(agentID string) []llm.ToolSchema {
	r.mu.RLock()
	defer r.mu.RUnlock()

	allowed := r.allowed[agentID]
	schemas := make([]llm.ToolSchema, 0, len(allowed))
	for name := range allowed {
		tool, ok := r.tools[name]
		if !ok {
			continue
		}
		schemas = append(schemas, tool.Schema())
	}
	return schemas
}
