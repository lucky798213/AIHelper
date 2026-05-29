package heartbeat

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

var ErrBusy = errors.New("agent turn is busy")

type Target struct {
	Channel string `json:"channel"`
	PeerID  string `json:"peer_id"`
	ToType  string `json:"to_type"`
}

func (t Target) Validate() error {
	if strings.TrimSpace(t.Channel) == "" {
		return errors.New("target channel is required")
	}
	if strings.TrimSpace(t.PeerID) == "" {
		return errors.New("target peer_id is required")
	}
	return nil
}

type Task struct {
	ID      string
	Name    string
	Source  string
	AgentID string
	Target  Target
	Message string
}

type AgentTurnFunc func(ctx context.Context, task Task) (string, error)

type SendFunc func(ctx context.Context, target Target, text string) error

type ActiveHours struct {
	Start int
	End   int
}

func (h ActiveHours) Validate() error {
	if h.Start < 0 || h.Start > 23 {
		return fmt.Errorf("active hour start must be 0-23, got %d", h.Start)
	}
	if h.End < 0 || h.End > 24 {
		return fmt.Errorf("active hour end must be 0-24, got %d", h.End)
	}
	return nil
}

func (h ActiveHours) Contains(now time.Time) bool {
	hour := now.Hour()
	if h.Start == h.End {
		return true
	}
	if h.Start < h.End {
		return h.Start <= hour && hour < h.End
	}
	return hour >= h.Start || hour < h.End
}

func normalizeText(text string) string {
	return strings.TrimSpace(text)
}
