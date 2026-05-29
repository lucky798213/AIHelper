package heartbeat

import (
	"testing"
	"time"
)

func TestComputeNextAtPastAndFuture(t *testing.T) {
	now := mustTime(t, "2026-05-23T10:00:00+08:00")

	past, err := ComputeNext(Schedule{Kind: "at", At: "2026-05-23T09:00:00+08:00"}, now)
	if err != nil {
		t.Fatalf("compute past at: %v", err)
	}
	if !past.IsZero() {
		t.Fatalf("past at next = %s, want zero", past)
	}

	future, err := ComputeNext(Schedule{Kind: "at", At: "2026-05-23T11:00:00+08:00"}, now)
	if err != nil {
		t.Fatalf("compute future at: %v", err)
	}
	want := mustTime(t, "2026-05-23T11:00:00+08:00")
	if !future.Equal(want) {
		t.Fatalf("future at next = %s, want %s", future, want)
	}
}

func TestComputeNextEveryUsesAnchor(t *testing.T) {
	now := mustTime(t, "2026-05-23T10:30:00+08:00")
	next, err := ComputeNext(Schedule{
		Kind:         "every",
		EverySeconds: 3600,
		Anchor:       "2026-05-23T00:00:00+08:00",
	}, now)
	if err != nil {
		t.Fatalf("compute every: %v", err)
	}
	want := mustTime(t, "2026-05-23T11:00:00+08:00")
	if !next.Equal(want) {
		t.Fatalf("every next = %s, want %s", next, want)
	}
}

func TestComputeNextCronUsesTimezone(t *testing.T) {
	now := mustTime(t, "2026-05-23T00:30:00Z")
	next, err := ComputeNext(Schedule{
		Kind: "cron",
		Expr: "0 9 * * *",
		TZ:   "Asia/Shanghai",
	}, now)
	if err != nil {
		t.Fatalf("compute cron: %v", err)
	}
	loc, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		t.Fatalf("load location: %v", err)
	}
	want := time.Date(2026, 5, 23, 9, 0, 0, 0, loc)
	if !next.Equal(want) {
		t.Fatalf("cron next = %s, want %s", next, want)
	}
}

func mustTime(t *testing.T, value string) time.Time {
	t.Helper()
	parsed, err := time.Parse(time.RFC3339, value)
	if err != nil {
		t.Fatalf("parse time %q: %v", value, err)
	}
	return parsed
}
