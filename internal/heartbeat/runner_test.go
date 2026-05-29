package heartbeat

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestRunnerSkipsMissingAndEmptyHeartbeat(t *testing.T) {
	workspace := t.TempDir()
	runner := newTestHeartbeatRunner(t, workspace, nil, nil)

	result, err := runner.Tick(context.Background())
	if err != nil {
		t.Fatalf("tick missing heartbeat: %v", err)
	}
	if result.Status != "skipped" || result.Reason != "HEARTBEAT.md not found" {
		t.Fatalf("missing result = %#v", result)
	}

	writeHeartbeat(t, workspace, " \n")
	result, err = runner.Tick(context.Background())
	if err != nil {
		t.Fatalf("tick empty heartbeat: %v", err)
	}
	if result.Status != "skipped" || result.Reason != "HEARTBEAT.md is empty" {
		t.Fatalf("empty result = %#v", result)
	}
}

func TestRunnerRespectsActiveHours(t *testing.T) {
	workspace := t.TempDir()
	writeHeartbeat(t, workspace, "Check reminders.")
	now := mustTime(t, "2026-05-23T23:00:00+08:00")
	runner := newTestHeartbeatRunner(t, workspace, nil, nil)
	runner.cfg.Now = func() time.Time { return now }
	runner.cfg.ActiveHours = &ActiveHours{Start: 9, End: 18}

	result, err := runner.Tick(context.Background())
	if err != nil {
		t.Fatalf("tick outside active hours: %v", err)
	}
	if result.Status != "skipped" || result.Reason == "" {
		t.Fatalf("outside hours result = %#v", result)
	}
}

func TestRunnerSuppressesOKAndDuplicateOutput(t *testing.T) {
	workspace := t.TempDir()
	writeHeartbeat(t, workspace, "Report only meaningful updates.")
	responses := []string{"HEARTBEAT_OK", "Something changed.", "Something changed."}
	var sent []string
	runner := newTestHeartbeatRunner(t, workspace,
		func(ctx context.Context, task Task) (string, error) {
			if len(responses) == 0 {
				t.Fatal("agent turn called too many times")
			}
			next := responses[0]
			responses = responses[1:]
			return next, nil
		},
		func(ctx context.Context, target Target, text string) error {
			sent = append(sent, text)
			return nil
		},
	)

	result, err := runner.Trigger(context.Background())
	if err != nil {
		t.Fatalf("trigger ok: %v", err)
	}
	if result.Status != "suppressed" || len(sent) != 0 {
		t.Fatalf("ok result=%#v sent=%#v", result, sent)
	}

	result, err = runner.Trigger(context.Background())
	if err != nil {
		t.Fatalf("trigger meaningful: %v", err)
	}
	if result.Status != "delivered" || len(sent) != 1 || sent[0] != "Something changed." {
		t.Fatalf("meaningful result=%#v sent=%#v", result, sent)
	}

	result, err = runner.Trigger(context.Background())
	if err != nil {
		t.Fatalf("trigger duplicate: %v", err)
	}
	if result.Status != "suppressed" || len(sent) != 1 {
		t.Fatalf("duplicate result=%#v sent=%#v", result, sent)
	}
}

func TestRunnerReturnsBusyWithoutUpdatingLastRun(t *testing.T) {
	workspace := t.TempDir()
	writeHeartbeat(t, workspace, "Check reminders.")
	now := mustTime(t, "2026-05-23T10:00:00+08:00")
	runner := newTestHeartbeatRunner(t, workspace,
		func(ctx context.Context, task Task) (string, error) {
			return "", ErrBusy
		},
		nil,
	)
	runner.cfg.Now = func() time.Time { return now }

	result, err := runner.Trigger(context.Background())
	if !errors.Is(err, ErrBusy) {
		t.Fatalf("err = %v, want ErrBusy", err)
	}
	if result.Status != "busy" {
		t.Fatalf("result = %#v", result)
	}
	if !runner.Status().LastRunAt.IsZero() {
		t.Fatalf("last run should remain zero after busy")
	}
}

func newTestHeartbeatRunner(t *testing.T, workspace string, turn AgentTurnFunc, send SendFunc) *Runner {
	t.Helper()
	if turn == nil {
		turn = func(ctx context.Context, task Task) (string, error) {
			t.Fatalf("agent turn should not be called: %#v", task)
			return "", nil
		}
	}
	if send == nil {
		send = func(ctx context.Context, target Target, text string) error {
			t.Fatalf("send should not be called: target=%#v text=%q", target, text)
			return nil
		}
	}
	runner, err := NewRunner(RunnerConfig{
		AgentID:      "local-master",
		WorkspaceDir: workspace,
		Target:       Target{Channel: "cli", PeerID: "cli-user"},
		Interval:     time.Minute,
		Now:          func() time.Time { return mustTime(t, "2026-05-23T10:00:00+08:00") },
		AgentTurn:    turn,
		Send:         send,
	})
	if err != nil {
		t.Fatalf("new runner: %v", err)
	}
	return runner
}

func writeHeartbeat(t *testing.T, workspace, content string) {
	t.Helper()
	path := filepath.Join(workspace, "HEARTBEAT.md")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write heartbeat: %v", err)
	}
}
