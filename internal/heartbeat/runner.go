package heartbeat

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type RunnerConfig struct {
	AgentID      string
	WorkspaceDir string
	Target       Target
	Interval     time.Duration
	ActiveHours  *ActiveHours
	TickInterval time.Duration
	Now          func() time.Time
	AgentTurn    AgentTurnFunc
	Send         SendFunc
}

type Runner struct {
	cfg RunnerConfig

	mu         sync.Mutex
	running    bool
	lastRunAt  time.Time
	lastOutput string
	lastStatus string
	lastReason string
	cancel     context.CancelFunc
	done       chan struct{}
	started    bool
}

type RunnerStatus struct {
	AgentID     string
	Enabled     bool
	Running     bool
	Interval    time.Duration
	LastRunAt   time.Time
	LastStatus  string
	LastReason  string
	HeartbeatMD string
	Target      Target
	ActiveHours *ActiveHours
}

type RunResult struct {
	AgentID string
	Status  string
	Reason  string
	Output  string
	Error   string
}

func NewRunner(cfg RunnerConfig) (*Runner, error) {
	if strings.TrimSpace(cfg.AgentID) == "" {
		return nil, errors.New("heartbeat agent_id is required")
	}
	if strings.TrimSpace(cfg.WorkspaceDir) == "" {
		return nil, errors.New("heartbeat workspace is required")
	}
	if err := cfg.Target.Validate(); err != nil {
		return nil, err
	}
	if cfg.Interval <= 0 {
		cfg.Interval = 15 * time.Minute
	}
	if cfg.TickInterval <= 0 {
		cfg.TickInterval = time.Second
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.AgentTurn == nil {
		return nil, errors.New("heartbeat agent turn function is required")
	}
	if cfg.Send == nil {
		return nil, errors.New("heartbeat send function is required")
	}
	if cfg.ActiveHours != nil {
		if err := cfg.ActiveHours.Validate(); err != nil {
			return nil, err
		}
	}
	return &Runner{cfg: cfg, lastStatus: "idle"}, nil
}

func (r *Runner) Start(ctx context.Context) {
	r.mu.Lock()
	if r.started {
		r.mu.Unlock()
		return
	}
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	r.cancel = cancel
	r.done = done
	r.started = true
	tickInterval := r.cfg.TickInterval
	r.mu.Unlock()

	go func() {
		defer close(done)
		ticker := time.NewTicker(tickInterval)
		defer ticker.Stop()
		for {
			select {
			case <-runCtx.Done():
				return
			case <-ticker.C:
				_, _ = r.Tick(runCtx)
			}
		}
	}()
}

func (r *Runner) Stop() {
	r.mu.Lock()
	cancel := r.cancel
	done := r.done
	r.cancel = nil
	r.done = nil
	r.started = false
	r.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	if done != nil {
		<-done
	}
}

func (r *Runner) Tick(ctx context.Context) (RunResult, error) {
	return r.run(ctx, false)
}

func (r *Runner) Trigger(ctx context.Context) (RunResult, error) {
	return r.run(ctx, true)
}

func (r *Runner) Status() RunnerStatus {
	r.mu.Lock()
	defer r.mu.Unlock()
	return RunnerStatus{
		AgentID:     r.cfg.AgentID,
		Enabled:     true,
		Running:     r.running,
		Interval:    r.cfg.Interval,
		LastRunAt:   r.lastRunAt,
		LastStatus:  r.lastStatus,
		LastReason:  r.lastReason,
		HeartbeatMD: r.heartbeatPath(),
		Target:      r.cfg.Target,
		ActiveHours: copyActiveHours(r.cfg.ActiveHours),
	}
}

func (r *Runner) run(ctx context.Context, force bool) (RunResult, error) {
	now := r.cfg.Now()
	instructions, ok, reason := r.shouldRun(now, force)
	if !ok {
		r.setStatus("skipped", reason)
		return RunResult{AgentID: r.cfg.AgentID, Status: "skipped", Reason: reason}, nil
	}

	r.mu.Lock()
	if r.running {
		r.mu.Unlock()
		return RunResult{AgentID: r.cfg.AgentID, Status: "busy", Reason: "already running"}, ErrBusy
	}
	r.running = true
	r.mu.Unlock()

	result := RunResult{AgentID: r.cfg.AgentID}
	ran := false
	defer func() {
		r.mu.Lock()
		r.running = false
		if ran {
			r.lastRunAt = now
		}
		r.mu.Unlock()
	}()

	output, err := r.cfg.AgentTurn(ctx, Task{
		ID:      "heartbeat:" + r.cfg.AgentID,
		Name:    "Heartbeat",
		Source:  "heartbeat",
		AgentID: r.cfg.AgentID,
		Target:  r.cfg.Target,
		Message: buildHeartbeatMessage(instructions),
	})
	if err != nil {
		if errors.Is(err, ErrBusy) {
			r.setStatus("busy", err.Error())
			return RunResult{AgentID: r.cfg.AgentID, Status: "busy", Reason: err.Error(), Error: err.Error()}, err
		}
		ran = true
		r.setStatus("error", err.Error())
		return RunResult{AgentID: r.cfg.AgentID, Status: "error", Error: err.Error()}, err
	}
	ran = true

	meaningful := parseHeartbeatOutput(output)
	if meaningful == "" {
		r.setStatus("suppressed", "HEARTBEAT_OK or empty output")
		return RunResult{AgentID: r.cfg.AgentID, Status: "suppressed", Reason: "HEARTBEAT_OK or empty output"}, nil
	}

	r.mu.Lock()
	duplicate := meaningful == r.lastOutput
	r.mu.Unlock()
	if duplicate {
		r.setStatus("suppressed", "duplicate output")
		return RunResult{AgentID: r.cfg.AgentID, Status: "suppressed", Reason: "duplicate output", Output: meaningful}, nil
	}

	if err := r.cfg.Send(ctx, r.cfg.Target, meaningful); err != nil {
		r.setStatus("error", err.Error())
		return RunResult{AgentID: r.cfg.AgentID, Status: "error", Output: meaningful, Error: err.Error()}, err
	}

	r.mu.Lock()
	r.lastOutput = meaningful
	r.mu.Unlock()
	result.Status = "delivered"
	result.Output = meaningful
	r.setStatus("delivered", "")
	return result, nil
}

func (r *Runner) shouldRun(now time.Time, force bool) (string, bool, string) {
	raw, err := os.ReadFile(r.heartbeatPath())
	if err != nil {
		if os.IsNotExist(err) {
			return "", false, "HEARTBEAT.md not found"
		}
		return "", false, fmt.Sprintf("read HEARTBEAT.md: %v", err)
	}
	instructions := strings.TrimSpace(string(raw))
	if instructions == "" {
		return "", false, "HEARTBEAT.md is empty"
	}
	if !force {
		r.mu.Lock()
		lastRunAt := r.lastRunAt
		r.mu.Unlock()
		if !lastRunAt.IsZero() && now.Sub(lastRunAt) < r.cfg.Interval {
			remaining := r.cfg.Interval - now.Sub(lastRunAt)
			return "", false, fmt.Sprintf("interval not elapsed (%s remaining)", remaining.Round(time.Second))
		}
		if r.cfg.ActiveHours != nil && !r.cfg.ActiveHours.Contains(now) {
			return "", false, fmt.Sprintf("outside active hours (%02d:00-%02d:00)", r.cfg.ActiveHours.Start, r.cfg.ActiveHours.End)
		}
	}
	return instructions, true, "all checks passed"
}

func (r *Runner) heartbeatPath() string {
	return filepath.Join(r.cfg.WorkspaceDir, "HEARTBEAT.md")
}

func (r *Runner) setStatus(status, reason string) {
	r.mu.Lock()
	r.lastStatus = status
	r.lastReason = reason
	r.mu.Unlock()
}

func buildHeartbeatMessage(instructions string) string {
	return "Scheduled heartbeat instructions:\n\n" +
		strings.TrimSpace(instructions) +
		"\n\nIf there is nothing useful to report, reply exactly HEARTBEAT_OK."
}

func parseHeartbeatOutput(output string) string {
	trimmed := normalizeText(output)
	if trimmed == "" {
		return ""
	}
	if strings.EqualFold(trimmed, "HEARTBEAT_OK") {
		return ""
	}
	return trimmed
}

func copyActiveHours(hours *ActiveHours) *ActiveHours {
	if hours == nil {
		return nil
	}
	copied := *hours
	return &copied
}
