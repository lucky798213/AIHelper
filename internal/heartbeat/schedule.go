package heartbeat

import (
	"fmt"
	"strings"
	"time"

	cronv3 "github.com/robfig/cron/v3"
)

type Schedule struct {
	Kind         string `json:"kind"`
	At           string `json:"at"`
	EverySeconds int    `json:"every_seconds"`
	Anchor       string `json:"anchor"`
	Expr         string `json:"expr"`
	TZ           string `json:"tz"`
}

func ComputeNext(schedule Schedule, now time.Time) (time.Time, error) {
	kind := strings.ToLower(strings.TrimSpace(schedule.Kind))
	switch kind {
	case "at":
		return computeNextAt(schedule, now)
	case "every":
		return computeNextEvery(schedule, now)
	case "cron":
		return computeNextCron(schedule, now)
	default:
		return time.Time{}, fmt.Errorf("unsupported schedule kind %q", schedule.Kind)
	}
}

func computeNextAt(schedule Schedule, now time.Time) (time.Time, error) {
	loc, err := scheduleLocation(schedule.TZ)
	if err != nil {
		return time.Time{}, err
	}
	at, err := parseScheduleTime(schedule.At, loc)
	if err != nil {
		return time.Time{}, fmt.Errorf("parse at schedule: %w", err)
	}
	if !at.After(now) {
		return time.Time{}, nil
	}
	return at, nil
}

func computeNextEvery(schedule Schedule, now time.Time) (time.Time, error) {
	everySeconds := schedule.EverySeconds
	if everySeconds <= 0 {
		everySeconds = 3600
	}
	every := time.Duration(everySeconds) * time.Second
	if strings.TrimSpace(schedule.Anchor) == "" {
		return now.Add(every), nil
	}

	loc, err := scheduleLocation(schedule.TZ)
	if err != nil {
		return time.Time{}, err
	}
	anchor, err := parseScheduleTime(schedule.Anchor, loc)
	if err != nil {
		return time.Time{}, fmt.Errorf("parse every anchor: %w", err)
	}
	if anchor.After(now) {
		return anchor, nil
	}
	elapsed := now.Sub(anchor)
	steps := int64(elapsed/every) + 1
	return anchor.Add(time.Duration(steps) * every), nil
}

func computeNextCron(schedule Schedule, now time.Time) (time.Time, error) {
	expr := strings.TrimSpace(schedule.Expr)
	if expr == "" {
		return time.Time{}, fmt.Errorf("cron expr is required")
	}
	loc, err := scheduleLocation(schedule.TZ)
	if err != nil {
		return time.Time{}, err
	}
	parser := cronv3.NewParser(cronv3.Minute | cronv3.Hour | cronv3.Dom | cronv3.Month | cronv3.Dow | cronv3.Descriptor)
	parsed, err := parser.Parse(expr)
	if err != nil {
		return time.Time{}, fmt.Errorf("parse cron expr: %w", err)
	}
	return parsed.Next(now.In(loc)), nil
}

func scheduleLocation(name string) (*time.Location, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return time.Local, nil
	}
	loc, err := time.LoadLocation(name)
	if err != nil {
		return nil, fmt.Errorf("load schedule timezone %q: %w", name, err)
	}
	return loc, nil
}

func parseScheduleTime(value string, loc *time.Location) (time.Time, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return time.Time{}, fmt.Errorf("time value is required")
	}
	if parsed, err := time.Parse(time.RFC3339, value); err == nil {
		return parsed, nil
	}
	layouts := []string{
		"2006-01-02T15:04:05",
		"2006-01-02 15:04:05",
		"2006-01-02T15:04",
		"2006-01-02 15:04",
	}
	for _, layout := range layouts {
		if parsed, err := time.ParseInLocation(layout, value, loc); err == nil {
			return parsed, nil
		}
	}
	return time.Time{}, fmt.Errorf("unsupported time format %q", value)
}
