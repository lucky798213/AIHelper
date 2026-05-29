package heartbeat

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

const defaultMaxConsecutiveErrors = 5

type Payload struct {
	Kind    string `json:"kind"`
	Message string `json:"message"`
	Text    string `json:"text"`
}

type CronServiceConfig struct {
	Path                 string
	LogPath              string
	TickInterval         time.Duration
	MaxConsecutiveErrors int
	Now                  func() time.Time
	AgentTurn            AgentTurnFunc
	Send                 SendFunc
}

type CronService struct {
	cfg CronServiceConfig

	mu      sync.Mutex
	jobs    []*CronJob
	cancel  context.CancelFunc
	done    chan struct{}
	started bool
}

type CronJob struct {
	ID                string
	Name              string
	AgentID           string
	Enabled           bool
	Schedule          Schedule
	Target            Target
	Payload           Payload
	DeleteAfterRun    bool
	ConsecutiveErrors int
	LastRunAt         time.Time
	NextRunAt         time.Time
}

type CronJobStatus struct {
	ID                string
	Name              string
	AgentID           string
	Enabled           bool
	ScheduleKind      string
	ConsecutiveErrors int
	LastRunAt         time.Time
	NextRunAt         time.Time
	Target            Target
}

type CronRunResult struct {
	JobID     string    `json:"job_id"`
	JobName   string    `json:"job_name"`
	Status    string    `json:"status"`
	Error     string    `json:"error,omitempty"`
	Output    string    `json:"output,omitempty"`
	RunAt     time.Time `json:"run_at"`
	NextRunAt time.Time `json:"next_run_at,omitempty"`
}

func NewCronService(cfg CronServiceConfig) (*CronService, error) {
	if cfg.TickInterval <= 0 {
		cfg.TickInterval = time.Second
	}
	if cfg.MaxConsecutiveErrors <= 0 {
		cfg.MaxConsecutiveErrors = defaultMaxConsecutiveErrors
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.AgentTurn == nil {
		return nil, errors.New("cron agent turn function is required")
	}
	if cfg.Send == nil {
		return nil, errors.New("cron send function is required")
	}
	service := &CronService{cfg: cfg}
	if err := service.LoadJobs(); err != nil {
		return nil, err
	}
	return service, nil
}

func (s *CronService) LoadJobs() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	jobs, err := s.loadJobsLocked()
	if err != nil {
		return err
	}
	s.jobs = jobs
	return nil
}

func (s *CronService) Start(ctx context.Context) {
	if s == nil {
		return
	}
	s.mu.Lock()
	if s.started {
		s.mu.Unlock()
		return
	}
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	s.cancel = cancel
	s.done = done
	s.started = true
	interval := s.cfg.TickInterval
	s.mu.Unlock()

	go func() {
		defer close(done)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-runCtx.Done():
				return
			case <-ticker.C:
				_, _ = s.Tick(runCtx)
			}
		}
	}()
}

func (s *CronService) Stop() {
	if s == nil {
		return
	}
	s.mu.Lock()
	cancel := s.cancel
	done := s.done
	s.cancel = nil
	s.done = nil
	s.started = false
	s.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	if done != nil {
		<-done
	}
}

func (s *CronService) Tick(ctx context.Context) ([]CronRunResult, error) {
	if s == nil {
		return nil, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.cfg.Now()
	var results []CronRunResult
	remove := make(map[string]struct{})

	for _, job := range s.jobs {
		if !job.Enabled || job.NextRunAt.IsZero() || now.Before(job.NextRunAt) {
			continue
		}
		result, busy := s.runJobLocked(ctx, job, now)
		if busy {
			continue
		}
		results = append(results, result)
		if job.DeleteAfterRun && strings.EqualFold(job.Schedule.Kind, "at") {
			remove[job.ID] = struct{}{}
		}
	}
	if len(remove) > 0 {
		filtered := s.jobs[:0]
		for _, job := range s.jobs {
			if _, ok := remove[job.ID]; !ok {
				filtered = append(filtered, job)
			}
		}
		s.jobs = filtered
	}
	return results, nil
}

func (s *CronService) Trigger(ctx context.Context, jobID string) (CronRunResult, error) {
	if s == nil {
		return CronRunResult{}, errors.New("cron service is disabled")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, job := range s.jobs {
		if job.ID == jobID {
			if !job.Enabled {
				return CronRunResult{}, fmt.Errorf("cron job %q is disabled", jobID)
			}
			result, busy := s.runJobLocked(ctx, job, s.cfg.Now())
			if busy {
				return result, ErrBusy
			}
			return result, nil
		}
	}
	return CronRunResult{}, fmt.Errorf("cron job %q not found", jobID)
}

func (s *CronService) ListJobs() []CronJobStatus {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	statuses := make([]CronJobStatus, 0, len(s.jobs))
	for _, job := range s.jobs {
		statuses = append(statuses, CronJobStatus{
			ID:                job.ID,
			Name:              job.Name,
			AgentID:           job.AgentID,
			Enabled:           job.Enabled,
			ScheduleKind:      job.Schedule.Kind,
			ConsecutiveErrors: job.ConsecutiveErrors,
			LastRunAt:         job.LastRunAt,
			NextRunAt:         job.NextRunAt,
			Target:            job.Target,
		})
	}
	sort.Slice(statuses, func(i, j int) bool {
		return statuses[i].ID < statuses[j].ID
	})
	return statuses
}

func (s *CronService) runJobLocked(ctx context.Context, job *CronJob, now time.Time) (CronRunResult, bool) {
	output, err := s.executeJob(ctx, job)
	if errors.Is(err, ErrBusy) {
		return CronRunResult{
			JobID:   job.ID,
			JobName: job.Name,
			Status:  "busy",
			Error:   err.Error(),
			RunAt:   now,
		}, true
	}

	job.LastRunAt = now
	status := "ok"
	errorText := ""
	if err != nil {
		status = "error"
		errorText = err.Error()
		job.ConsecutiveErrors++
		if job.ConsecutiveErrors >= s.cfg.MaxConsecutiveErrors {
			job.Enabled = false
		}
	} else {
		job.ConsecutiveErrors = 0
	}
	next, nextErr := ComputeNext(job.Schedule, now)
	if nextErr != nil {
		status = "error"
		if errorText == "" {
			errorText = nextErr.Error()
		}
		job.ConsecutiveErrors++
		if job.ConsecutiveErrors >= s.cfg.MaxConsecutiveErrors {
			job.Enabled = false
		}
	}
	job.NextRunAt = next

	result := CronRunResult{
		JobID:     job.ID,
		JobName:   job.Name,
		Status:    status,
		Error:     errorText,
		Output:    output,
		RunAt:     now,
		NextRunAt: job.NextRunAt,
	}
	_ = s.appendRunLog(result)
	return result, false
}

func (s *CronService) executeJob(ctx context.Context, job *CronJob) (string, error) {
	if err := job.Target.Validate(); err != nil {
		return "", err
	}
	kind := strings.ToLower(strings.TrimSpace(job.Payload.Kind))
	if kind == "" {
		kind = "agent_turn"
	}
	var output string
	switch kind {
	case "agent_turn":
		if strings.TrimSpace(job.AgentID) == "" {
			return "", errors.New("agent_turn payload requires agent_id")
		}
		message := strings.TrimSpace(job.Payload.Message)
		if message == "" {
			return "", errors.New("agent_turn payload message is required")
		}
		agentOutput, err := s.cfg.AgentTurn(ctx, Task{
			ID:      "cron:" + job.ID,
			Name:    job.Name,
			Source:  "cron",
			AgentID: job.AgentID,
			Target:  job.Target,
			Message: message,
		})
		if err != nil {
			return "", err
		}
		output = normalizeText(agentOutput)
	case "system_event":
		output = normalizeText(job.Payload.Text)
		if output == "" {
			output = normalizeText(job.Payload.Message)
		}
		if output == "" {
			return "", errors.New("system_event payload text is required")
		}
	default:
		return "", fmt.Errorf("unsupported payload kind %q", job.Payload.Kind)
	}
	if output == "" {
		return "", nil
	}
	if err := s.cfg.Send(ctx, job.Target, output); err != nil {
		return output, err
	}
	return output, nil
}

func (s *CronService) loadJobsLocked() ([]*CronJob, error) {
	if strings.TrimSpace(s.cfg.Path) == "" {
		return nil, nil
	}
	raw, err := os.ReadFile(s.cfg.Path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read cron file: %w", err)
	}
	var file cronFile
	if err := json.Unmarshal(raw, &file); err != nil {
		return nil, fmt.Errorf("decode cron file: %w", err)
	}
	now := s.cfg.Now()
	jobs := make([]*CronJob, 0, len(file.Jobs))
	seen := make(map[string]struct{}, len(file.Jobs))
	for _, rawJob := range file.Jobs {
		id := strings.TrimSpace(rawJob.ID)
		if id == "" {
			return nil, errors.New("cron job id is required")
		}
		if _, exists := seen[id]; exists {
			return nil, fmt.Errorf("duplicate cron job id %q", id)
		}
		seen[id] = struct{}{}
		enabled := true
		if rawJob.Enabled != nil {
			enabled = *rawJob.Enabled
		}
		name := strings.TrimSpace(rawJob.Name)
		if name == "" {
			name = id
		}
		next, err := ComputeNext(rawJob.Schedule, now)
		if err != nil {
			return nil, fmt.Errorf("compute next run for cron job %q: %w", id, err)
		}
		jobs = append(jobs, &CronJob{
			ID:             id,
			Name:           name,
			AgentID:        strings.TrimSpace(rawJob.AgentID),
			Enabled:        enabled,
			Schedule:       rawJob.Schedule,
			Target:         rawJob.Target,
			Payload:        rawJob.Payload,
			DeleteAfterRun: rawJob.DeleteAfterRun,
			NextRunAt:      next,
		})
	}
	return jobs, nil
}

func (s *CronService) appendRunLog(result CronRunResult) error {
	if strings.TrimSpace(s.cfg.LogPath) == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(s.cfg.LogPath), 0o755); err != nil {
		return err
	}
	file, err := os.OpenFile(s.cfg.LogPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	defer file.Close()
	return json.NewEncoder(file).Encode(result)
}

type cronFile struct {
	Jobs []rawCronJob `json:"jobs"`
}

type rawCronJob struct {
	ID             string   `json:"id"`
	Name           string   `json:"name"`
	AgentID        string   `json:"agent_id"`
	Enabled        *bool    `json:"enabled"`
	Schedule       Schedule `json:"schedule"`
	Target         Target   `json:"target"`
	Payload        Payload  `json:"payload"`
	DeleteAfterRun bool     `json:"delete_after_run"`
}
