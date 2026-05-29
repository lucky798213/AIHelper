package agent

import (
	"context"
	"fmt"
)

type AgentRole string

const (
	AgentRoleMaster     AgentRole = "master"
	AgentRoleSpecialist AgentRole = "specialist"
)

type AgentConfig struct {
	ID           string             `yaml:"id"`
	Name         string             `yaml:"name"`
	Role         AgentRole          `yaml:"role"`
	Model        string             `yaml:"model"`
	Description  string             `yaml:"description"`
	Tools        []string           `yaml:"tools"`
	Intelligence IntelligenceConfig `yaml:"intelligence"`
	Children     []ChildAgentRef    `yaml:"children"`
}

type IntelligenceConfig struct {
	Enabled    *bool  `yaml:"enabled"`
	Workspace  string `yaml:"workspace"`
	PromptMode string `yaml:"prompt_mode"`
	MemoryTopK int    `yaml:"memory_top_k"`
}

type PromptBuildRequest struct {
	Agent      AgentConfig
	UserInput  string
	Channel    string
	Model      string
	BasePrompt string
}

type PromptBuilder interface {
	BuildSystemPrompt(ctx context.Context, req PromptBuildRequest) (string, error)
}

type ChildAgentRef struct {
	AgentID     string `yaml:"agent_id"`
	Name        string `yaml:"name"`
	Description string `yaml:"description"`
}

// map[agentID]AgentConfig
type Manager struct {
	agents map[string]AgentConfig
}

// 把所有 agent 加载出来，存到 Manager 里，并且做基础合法性校验
func NewManager(configs []AgentConfig) (*Manager, error) {
	agents := make(map[string]AgentConfig, len(configs))
	for _, cfg := range configs {
		if cfg.ID == "" {
			return nil, fmt.Errorf("agent id is required")
		}
		if cfg.Role == "" {
			cfg.Role = AgentRoleSpecialist
		}
		if _, exists := agents[cfg.ID]; exists {
			return nil, fmt.Errorf("duplicate agent id %q", cfg.ID)
		}
		agents[cfg.ID] = cfg
	}
	for _, cfg := range agents {
		for _, child := range cfg.Children {
			if _, ok := agents[child.AgentID]; !ok {
				return nil, fmt.Errorf("agent %q references unknown child agent %q", cfg.ID, child.AgentID)
			}
		}
	}
	return &Manager{agents: agents}, nil
}

func (m *Manager) Get(agentID string) (AgentConfig, bool) {
	cfg, ok := m.agents[agentID]
	return cfg, ok
}

func (m *Manager) CanDelegate(parentAgentID, childAgentID string) bool {
	parent, ok := m.Get(parentAgentID)
	if !ok || parent.Role != AgentRoleMaster {
		return false
	}
	for _, child := range parent.Children {
		if child.AgentID == childAgentID {
			return true
		}
	}
	return false
}

// You are [Name] ([Role]). Agent ID: [ID]. [Description]
// When tools are available and the user asks you to inspect or modify files, call the appropriate tool instead of only describing the action.
// You are the master agent. Answer directly when handling a direct task. Child-agent dispatch is handled before this answer step.
// Available child agents:
// - [AgentID1] ([Name1]): [Description1]
// - [AgentID2] ([Name2]): [Description2]
func (c AgentConfig) SystemPrompt() string {
	prompt := fmt.Sprintf(
		"You are %s (%s). Agent ID: %s. %s",
		c.Name,
		c.Role,
		c.ID,
		c.Description,
	)
	prompt += "\nWhen tools are available and the user asks you to inspect or modify files, call the appropriate tool instead of only describing the action."
	if c.Role == AgentRoleMaster && len(c.Children) > 0 {
		prompt += "\nYou are the master agent. Answer directly when handling a direct task. Child-agent dispatch is handled before this answer step.\nAvailable child agents:"
		for _, child := range c.Children {
			prompt += fmt.Sprintf("\n- %s (%s): %s", child.AgentID, child.Name, child.Description)
		}
	}
	if c.Role == AgentRoleSpecialist {
		prompt += "\nYou are a specialist child agent. Complete the delegated task and return only your result to the master agent."
	}
	return prompt
}

//You are [名字], a master dispatch agent. Return JSON only. Do not add markdown. Agent ID: [ID]. [描述]
//
//Available child agents:
//- [子AgentID] ([子名字]): [子描述]
//- [子AgentID] ([子名字]): [子描述]
//
//Use mode=direct for simple tasks. Use mode=delegate when one child agent clearly fits. Use mode=plan for multi-step tasks.

func (c AgentConfig) DispatchPrompt() string {
	prompt := fmt.Sprintf(
		"You are %s, a master dispatch agent. Return JSON only. Do not add markdown. Agent ID: %s. %s",
		c.Name,
		c.ID,
		c.Description,
	)
	prompt += "\nSupported JSON shapes:"
	prompt += "\n- Direct: {\"mode\":\"direct\",\"input\":\"the user request\"}"
	prompt += "\n- Single specialist: {\"mode\":\"delegate\",\"agent_id\":\"child-agent-id\",\"input\":\"task for that specialist\"}"
	prompt += "\n- Multi-step plan: {\"mode\":\"plan\",\"steps\":[{\"id\":\"step-1\",\"agent_id\":\"child-agent-id\",\"input\":\"task for that step\"}],\"final_instruction\":\"how the master should synthesize the results\"}"
	if len(c.Children) > 0 {
		prompt += "\nAvailable child agents:"
		for _, child := range c.Children {
			prompt += fmt.Sprintf("\n- %s (%s): %s", child.AgentID, child.Name, child.Description)
		}
	}
	prompt += "\nUse mode=direct for simple tasks the master can answer itself."
	prompt += "\nUse mode=delegate only when exactly one child agent clearly fits."
	prompt += "\nUse mode=plan for complex tasks that require multiple specialists or ordered dependent steps. Plans are synchronous and sequential, must use only available child agents, and must contain at most 6 steps."
	return prompt
}
