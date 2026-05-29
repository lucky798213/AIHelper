package resilience

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"AIHelper/internal/llm"
)

func TestClassifyFailure(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want FailoverReason
	}{
		{name: "auth status", err: &llm.HTTPError{StatusCode: http.StatusUnauthorized, Body: "bad key"}, want: ReasonAuth},
		{name: "billing status", err: &llm.HTTPError{StatusCode: http.StatusPaymentRequired, Body: "quota"}, want: ReasonBilling},
		{name: "rate status", err: &llm.HTTPError{StatusCode: http.StatusTooManyRequests, Body: "slow down"}, want: ReasonRateLimit},
		{name: "timeout text", err: errors.New("request timed out after 30s"), want: ReasonTimeout},
		{name: "overflow text", err: errors.New("maximum context length exceeded: too many tokens"), want: ReasonOverflow},
		{name: "unknown", err: errors.New("unexpected internal error"), want: ReasonUnknown},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ClassifyFailure(tt.err); got != tt.want {
				t.Fatalf("ClassifyFailure() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestProfileManagerSelectsAroundCooldowns(t *testing.T) {
	now := time.Date(2026, 5, 23, 12, 0, 0, 0, time.UTC)
	manager, err := NewProfileManager([]AuthProfile{
		{Name: "main", Provider: "openai_compatible", APIKey: "main-key"},
		{Name: "backup", Provider: "openai_compatible", APIKey: "backup-key"},
	})
	if err != nil {
		t.Fatalf("new manager: %v", err)
	}
	manager.setNowForTest(func() time.Time { return now })

	profile, ok := manager.SelectAvailable(nil)
	if !ok || profile.Name != "main" {
		t.Fatalf("first profile = %#v, %v", profile, ok)
	}

	manager.MarkFailure("main", ReasonRateLimit, 2*time.Minute)
	profile, ok = manager.SelectAvailable(nil)
	if !ok || profile.Name != "backup" {
		t.Fatalf("profile after cooldown = %#v, %v", profile, ok)
	}

	snapshot := manager.Snapshot()
	if snapshot[0].Available || snapshot[0].FailureReason != ReasonRateLimit {
		t.Fatalf("main snapshot = %#v", snapshot[0])
	}

	manager.MarkSuccess("main")
	snapshot = manager.Snapshot()
	if !snapshot[0].Available || snapshot[0].FailureReason != "" || snapshot[0].LastGoodAt.IsZero() {
		t.Fatalf("main after success = %#v", snapshot[0])
	}
}

func TestResilientClientRotatesAfterRateLimit(t *testing.T) {
	factory := &scriptedFactory{scripts: map[string][]scriptedResult{
		"main": {
			{err: &llm.HTTPError{StatusCode: http.StatusTooManyRequests, Body: "rate limit"}},
		},
		"backup": {
			{resp: llm.Response{StopReason: llm.StopReasonEndTurn, Text: "ok"}},
		},
	}}
	client := newTestClient(t, []AuthProfile{
		{Name: "main", Provider: "openai_compatible", APIKey: "main-key"},
		{Name: "backup", Provider: "openai_compatible", APIKey: "backup-key"},
	}, factory, nil)

	resp, err := client.CreateMessage(context.Background(), llm.Request{
		Model:    "primary",
		Messages: []llm.Message{{Role: "user", Content: "hello"}},
	})
	if err != nil {
		t.Fatalf("create message: %v", err)
	}
	if resp.Text != "ok" {
		t.Fatalf("response = %#v", resp)
	}
	if got := factory.profileCalls(); strings.Join(got, ",") != "main,backup" {
		t.Fatalf("profile calls = %#v", got)
	}
	if stats := client.Stats(); stats.TotalRotations != 1 || stats.TotalFailures != 1 || stats.TotalSuccesses != 1 {
		t.Fatalf("stats = %#v", stats)
	}
	if snapshot := client.profiles.Snapshot(); snapshot[0].FailureReason != ReasonRateLimit {
		t.Fatalf("main snapshot = %#v", snapshot[0])
	}
}

func TestResilientClientAppliesTimeoutCooldown(t *testing.T) {
	now := time.Date(2026, 5, 23, 12, 0, 0, 0, time.UTC)
	factory := &scriptedFactory{scripts: map[string][]scriptedResult{
		"main": {{err: errors.New("request timeout")}},
	}}
	client := newTestClient(t, []AuthProfile{
		{Name: "main", Provider: "openai_compatible", APIKey: "main-key"},
	}, factory, nil)
	client.profiles.setNowForTest(func() time.Time { return now })

	_, err := client.CreateMessage(context.Background(), llm.Request{
		Model:    "primary",
		Messages: []llm.Message{{Role: "user", Content: "hello"}},
	})
	if err == nil {
		t.Fatal("expected exhausted error")
	}

	snapshot := client.profiles.Snapshot()
	if snapshot[0].FailureReason != ReasonTimeout || snapshot[0].CooldownRemaining != time.Minute {
		t.Fatalf("snapshot = %#v", snapshot[0])
	}
}

func TestResilientClientUsesFallbackModelAfterPrimaryExhausted(t *testing.T) {
	factory := &scriptedFactory{scripts: map[string][]scriptedResult{
		"main": {
			{err: &llm.HTTPError{StatusCode: http.StatusTooManyRequests, Body: "rate limit"}},
			{resp: llm.Response{StopReason: llm.StopReasonEndTurn, Text: "fallback ok"}},
		},
	}}
	client := newTestClient(t, []AuthProfile{
		{Name: "main", Provider: "openai_compatible", APIKey: "main-key"},
	}, factory, []string{"fallback-model"})

	resp, err := client.CreateMessage(context.Background(), llm.Request{
		Model:    "primary-model",
		Messages: []llm.Message{{Role: "user", Content: "hello"}},
	})
	if err != nil {
		t.Fatalf("create message: %v", err)
	}
	if resp.Text != "fallback ok" {
		t.Fatalf("response = %#v", resp)
	}
	if len(factory.calls) != 2 || factory.calls[1].model != "fallback-model" {
		t.Fatalf("calls = %#v", factory.calls)
	}
}

func TestResilientClientReturnsClearErrorWhenExhausted(t *testing.T) {
	factory := &scriptedFactory{scripts: map[string][]scriptedResult{
		"main": {{err: &llm.HTTPError{StatusCode: http.StatusUnauthorized, Body: "bad key"}}},
	}}
	client := newTestClient(t, []AuthProfile{
		{Name: "main", Provider: "openai_compatible", APIKey: "main-key"},
	}, factory, nil)

	_, err := client.CreateMessage(context.Background(), llm.Request{
		Model:    "primary",
		Messages: []llm.Message{{Role: "user", Content: "hello"}},
	})
	if err == nil || !strings.Contains(err.Error(), "resilience exhausted profiles and fallback models") {
		t.Fatalf("err = %v", err)
	}
}

func TestResilientClientCompactsOverflowOnlyForRetryRequest(t *testing.T) {
	longToolResult := strings.Repeat("x", 80)
	messages := []llm.Message{
		{Role: "user", Content: "old user"},
		{Role: "assistant", Content: "old assistant"},
		{Role: "tool", Name: "read_file", Content: longToolResult, ToolCallID: "call_1"},
		{Role: "user", Content: "middle user"},
		{Role: "assistant", Content: "recent assistant"},
		{Role: "user", Content: "recent user"},
	}
	factory := &scriptedFactory{scripts: map[string][]scriptedResult{
		"main": {
			{err: errors.New("context window token overflow")},
			{resp: llm.Response{StopReason: llm.StopReasonEndTurn, Text: "summary text"}},
			{resp: llm.Response{StopReason: llm.StopReasonEndTurn, Text: "final ok"}},
		},
	}}
	client := newTestClient(t, []AuthProfile{
		{Name: "main", Provider: "openai_compatible", APIKey: "main-key"},
	}, factory, nil)
	client.maxOverflowCompactions = 2
	client.guard = NewContextGuard(1000, 10)

	resp, err := client.CreateMessage(context.Background(), llm.Request{
		Model:    "primary",
		Messages: messages,
	})
	if err != nil {
		t.Fatalf("create message: %v", err)
	}
	if resp.Text != "final ok" {
		t.Fatalf("response = %#v", resp)
	}
	if len(factory.calls) != 3 {
		t.Fatalf("calls = %#v", factory.calls)
	}
	if factory.calls[1].purpose != "resilience_compact" {
		t.Fatalf("summary call = %#v", factory.calls[1])
	}
	finalMessages := factory.calls[2].messages
	if !strings.Contains(finalMessages[0].Content, "[Previous conversation summary]\nsummary text") {
		t.Fatalf("final compacted messages = %#v", finalMessages)
	}
	if messages[2].Content != longToolResult {
		t.Fatalf("original messages were mutated: %#v", messages[2])
	}
	if stats := client.Stats(); stats.TotalCompactions != 1 {
		t.Fatalf("stats = %#v", stats)
	}
}

func newTestClient(t *testing.T, profiles []AuthProfile, factory *scriptedFactory, fallbackModels []string) *ResilientClient {
	t.Helper()
	manager, err := NewProfileManager(profiles)
	if err != nil {
		t.Fatalf("new manager: %v", err)
	}
	client, err := NewClient(ClientConfig{
		Profiles:               manager,
		ClientFactory:          factory.Factory,
		FallbackModels:         fallbackModels,
		ContextGuard:           NewContextGuard(1000, 100),
		MaxOverflowCompactions: 3,
	})
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	return client
}

type scriptedResult struct {
	resp llm.Response
	err  error
}

type scriptedCall struct {
	profile  string
	model    string
	purpose  string
	messages []llm.Message
}

type scriptedFactory struct {
	scripts map[string][]scriptedResult
	calls   []scriptedCall
}

func (f *scriptedFactory) Factory(profile AuthProfile) (llm.Client, error) {
	return &scriptedClient{profile: profile.Name, factory: f}, nil
}

func (f *scriptedFactory) profileCalls() []string {
	result := make([]string, 0, len(f.calls))
	for _, call := range f.calls {
		result = append(result, call.profile)
	}
	return result
}

type scriptedClient struct {
	profile string
	factory *scriptedFactory
}

func (c *scriptedClient) CreateMessage(ctx context.Context, req llm.Request) (llm.Response, error) {
	c.factory.calls = append(c.factory.calls, scriptedCall{
		profile:  c.profile,
		model:    req.Model,
		purpose:  req.Purpose,
		messages: cloneMessages(req.Messages),
	})
	script := c.factory.scripts[c.profile]
	if len(script) == 0 {
		return llm.Response{}, fmt.Errorf("no scripted response for profile %q", c.profile)
	}
	next := script[0]
	c.factory.scripts[c.profile] = script[1:]
	return next.resp, next.err
}
