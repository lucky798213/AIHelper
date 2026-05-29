package heartbeat

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestCronServiceRunsDueSystemEventAndDeletesAtJob(t *testing.T) {
	dir := t.TempDir()
	cronPath := filepath.Join(dir, "CRON.json")
	logPath := filepath.Join(dir, "logs", "cron-runs.jsonl")
	current := mustTime(t, "2026-05-23T10:00:00+08:00")
	writeCronFile(t, cronPath, `{
	  "jobs": [{
	    "id": "reminder",
	    "name": "Reminder",
	    "enabled": true,
	    "schedule": {"kind": "at", "at": "2026-05-23T10:01:00+08:00"},
	    "target": {"channel": "cli", "peer_id": "cli-user"},
	    "payload": {"kind": "system_event", "text": "stand up"},
	    "delete_after_run": true
	  }]
	}`)
	var sent []string
	service, err := NewCronService(CronServiceConfig{
		Path:    cronPath,
		LogPath: logPath,
		Now:     func() time.Time { return current },
		AgentTurn: func(ctx context.Context, task Task) (string, error) {
			t.Fatalf("agent turn should not run for system_event")
			return "", nil
		},
		Send: func(ctx context.Context, target Target, text string) error {
			sent = append(sent, text)
			return nil
		},
	})
	if err != nil {
		t.Fatalf("new cron service: %v", err)
	}

	current = mustTime(t, "2026-05-23T10:02:00+08:00")
	results, err := service.Tick(context.Background())
	if err != nil {
		t.Fatalf("tick cron: %v", err)
	}
	if len(results) != 1 || results[0].Status != "ok" {
		t.Fatalf("results = %#v", results)
	}
	if len(sent) != 1 || sent[0] != "stand up" {
		t.Fatalf("sent = %#v", sent)
	}
	if jobs := service.ListJobs(); len(jobs) != 0 {
		t.Fatalf("delete_after_run job remains: %#v", jobs)
	}
	rawLog, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read cron log: %v", err)
	}
	if !strings.Contains(string(rawLog), `"job_id":"reminder"`) {
		t.Fatalf("cron log missing job id: %s", string(rawLog))
	}
}

func TestCronServiceRunsAgentTurnAndSendsOutput(t *testing.T) {
	dir := t.TempDir()
	cronPath := filepath.Join(dir, "CRON.json")
	current := mustTime(t, "2026-05-23T10:00:00+08:00")
	writeCronFile(t, cronPath, `{
	  "jobs": [{
	    "id": "daily",
	    "enabled": true,
	    "agent_id": "local-master",
	    "schedule": {"kind": "at", "at": "2026-05-23T10:01:00+08:00"},
	    "target": {"channel": "cli", "peer_id": "cli-user"},
	    "payload": {"kind": "agent_turn", "message": "generate brief"}
	  }]
	}`)
	var turnTask Task
	var sent []string
	service, err := NewCronService(CronServiceConfig{
		Path: cronPath,
		Now:  func() time.Time { return current },
		AgentTurn: func(ctx context.Context, task Task) (string, error) {
			turnTask = task
			return "brief ready", nil
		},
		Send: func(ctx context.Context, target Target, text string) error {
			sent = append(sent, text)
			return nil
		},
	})
	if err != nil {
		t.Fatalf("new cron service: %v", err)
	}

	current = mustTime(t, "2026-05-23T10:02:00+08:00")
	results, err := service.Tick(context.Background())
	if err != nil {
		t.Fatalf("tick cron: %v", err)
	}
	if len(results) != 1 || results[0].Status != "ok" {
		t.Fatalf("results = %#v", results)
	}
	if turnTask.AgentID != "local-master" || turnTask.Message != "generate brief" {
		t.Fatalf("turn task = %#v", turnTask)
	}
	if len(sent) != 1 || sent[0] != "brief ready" {
		t.Fatalf("sent = %#v", sent)
	}
}

func TestCronServiceAutoDisablesAfterConsecutiveErrors(t *testing.T) {
	dir := t.TempDir()
	cronPath := filepath.Join(dir, "CRON.json")
	current := mustTime(t, "2026-05-23T10:00:00+08:00")
	writeCronFile(t, cronPath, `{
	  "jobs": [{
	    "id": "bad-target",
	    "enabled": true,
	    "schedule": {"kind": "every", "every_seconds": 60},
	    "target": {"channel": "", "peer_id": ""},
	    "payload": {"kind": "system_event", "text": "hello"}
	  }]
	}`)
	service, err := NewCronService(CronServiceConfig{
		Path:                 cronPath,
		MaxConsecutiveErrors: 2,
		Now:                  func() time.Time { return current },
		AgentTurn: func(ctx context.Context, task Task) (string, error) {
			return "", nil
		},
		Send: func(ctx context.Context, target Target, text string) error {
			t.Fatalf("send should not run with bad target")
			return nil
		},
	})
	if err != nil {
		t.Fatalf("new cron service: %v", err)
	}

	if _, err := service.Trigger(context.Background(), "bad-target"); err != nil {
		t.Fatalf("first trigger should record job error, got %v", err)
	}
	if _, err := service.Trigger(context.Background(), "bad-target"); err != nil {
		t.Fatalf("second trigger should record job error, got %v", err)
	}
	jobs := service.ListJobs()
	if len(jobs) != 1 || jobs[0].Enabled || jobs[0].ConsecutiveErrors != 2 {
		t.Fatalf("job status = %#v", jobs)
	}
}

func writeCronFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write cron file: %v", err)
	}
}
