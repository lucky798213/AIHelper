package heartbeat

import (
	"context"
	"fmt"
	"sort"
	"sync"
)

type Manager struct {
	mu      sync.RWMutex
	runners map[string]*Runner
}

func NewManager(runners []*Runner) (*Manager, error) {
	byAgent := make(map[string]*Runner, len(runners))
	for _, runner := range runners {
		if runner == nil {
			continue
		}
		agentID := runner.Status().AgentID
		if _, exists := byAgent[agentID]; exists {
			return nil, fmt.Errorf("duplicate heartbeat runner for agent %q", agentID)
		}
		byAgent[agentID] = runner
	}
	return &Manager{runners: byAgent}, nil
}

func (m *Manager) Start(ctx context.Context) {
	if m == nil {
		return
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, runner := range m.runners {
		runner.Start(ctx)
	}
}

func (m *Manager) Stop() {
	if m == nil {
		return
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, runner := range m.runners {
		runner.Stop()
	}
}

func (m *Manager) Trigger(ctx context.Context, agentID string) (RunResult, error) {
	m.mu.RLock()
	runner, ok := m.runners[agentID]
	m.mu.RUnlock()
	if !ok {
		return RunResult{}, fmt.Errorf("heartbeat runner for agent %q not found", agentID)
	}
	return runner.Trigger(ctx)
}

func (m *Manager) Statuses() []RunnerStatus {
	if m == nil {
		return nil
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	ids := make([]string, 0, len(m.runners))
	for id := range m.runners {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	statuses := make([]RunnerStatus, 0, len(ids))
	for _, id := range ids {
		statuses = append(statuses, m.runners[id].Status())
	}
	return statuses
}
