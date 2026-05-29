package delivery

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"AIHelper/internal/channels"
)

func TestServiceProcessPendingSuccessAcks(t *testing.T) {
	ctx := context.Background()
	outbox, err := NewFileOutbox(FileOutboxConfig{Dir: t.TempDir()})
	if err != nil {
		t.Fatalf("new outbox: %v", err)
	}
	var sent []channels.OutboundMessage
	service, err := NewService(ServiceConfig{
		Outbox: outbox,
		Sender: func(ctx context.Context, msg channels.OutboundMessage) error {
			sent = append(sent, msg)
			return nil
		},
	})
	if err != nil {
		t.Fatalf("new service: %v", err)
	}

	if err := service.Enqueue(ctx, channels.OutboundMessage{Channel: "cli", To: "cli-user", Text: "hello"}); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if err := service.ProcessPending(ctx); err != nil {
		t.Fatalf("process pending: %v", err)
	}

	if len(sent) != 1 || sent[0].Text != "hello" {
		t.Fatalf("sent = %#v", sent)
	}
	pending, err := service.Pending(ctx)
	if err != nil {
		t.Fatalf("pending: %v", err)
	}
	if len(pending) != 0 {
		t.Fatalf("pending after success = %#v", pending)
	}
	stats, err := service.Stats(ctx)
	if err != nil {
		t.Fatalf("stats: %v", err)
	}
	if stats.TotalAttempted != 1 || stats.TotalSucceeded != 1 || stats.TotalFailed != 0 {
		t.Fatalf("stats = %#v", stats)
	}
}

func TestServiceProcessPendingFailureSchedulesRetry(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 5, 23, 10, 0, 0, 0, time.UTC)
	outbox, err := NewFileOutbox(FileOutboxConfig{
		Dir: t.TempDir(),
		Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatalf("new outbox: %v", err)
	}
	service, err := NewService(ServiceConfig{
		Outbox: outbox,
		Now:    func() time.Time { return now },
		Sender: func(ctx context.Context, msg channels.OutboundMessage) error {
			return errors.New("send failed")
		},
	})
	if err != nil {
		t.Fatalf("new service: %v", err)
	}

	if err := service.Enqueue(ctx, channels.OutboundMessage{Channel: "feishu", To: "ou-user", Text: "hello"}); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if err := service.ProcessPending(ctx); err != nil {
		t.Fatalf("process pending: %v", err)
	}
	pending, err := service.Pending(ctx)
	if err != nil {
		t.Fatalf("pending: %v", err)
	}
	if len(pending) != 1 || pending[0].RetryCount != 1 || pending[0].LastError != "send failed" || !pending[0].NextRetryAt.After(now) {
		t.Fatalf("pending retry = %#v", pending)
	}
	stats, err := service.Stats(ctx)
	if err != nil {
		t.Fatalf("stats: %v", err)
	}
	if stats.TotalAttempted != 1 || stats.TotalSucceeded != 0 || stats.TotalFailed != 1 {
		t.Fatalf("stats = %#v", stats)
	}
}

func TestServiceStartProcessesWake(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	outbox, err := NewFileOutbox(FileOutboxConfig{Dir: t.TempDir()})
	if err != nil {
		t.Fatalf("new outbox: %v", err)
	}
	sent := make(chan channels.OutboundMessage, 1)
	service, err := NewService(ServiceConfig{
		Outbox:       outbox,
		ScanInterval: time.Hour,
		Sender: func(ctx context.Context, msg channels.OutboundMessage) error {
			sent <- msg
			return nil
		},
	})
	if err != nil {
		t.Fatalf("new service: %v", err)
	}
	service.Start(ctx)
	defer service.Stop()

	if err := service.Enqueue(ctx, channels.OutboundMessage{Channel: "cli", To: "cli-user", Text: "wake up"}); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	select {
	case msg := <-sent:
		if msg.Text != "wake up" {
			t.Fatalf("sent msg = %#v", msg)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for delivery wake")
	}
}

func TestServiceChunkedEnqueuePreservesOrder(t *testing.T) {
	ctx := context.Background()
	outbox, err := NewFileOutbox(FileOutboxConfig{Dir: t.TempDir()})
	if err != nil {
		t.Fatalf("new outbox: %v", err)
	}
	var mu sync.Mutex
	var sent []string
	service, err := NewService(ServiceConfig{
		Outbox: outbox,
		Sender: func(ctx context.Context, msg channels.OutboundMessage) error {
			mu.Lock()
			defer mu.Unlock()
			sent = append(sent, msg.Text)
			return nil
		},
	})
	if err != nil {
		t.Fatalf("new service: %v", err)
	}
	text := stringsOf("a", defaultChannelLimit) + stringsOf("b", 3)
	if err := service.Enqueue(ctx, channels.OutboundMessage{Channel: "feishu", To: "ou-user", Text: text}); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if err := service.ProcessPending(ctx); err != nil {
		t.Fatalf("process pending: %v", err)
	}
	if len(sent) != 2 || sent[0] != stringsOf("a", defaultChannelLimit) || sent[1] != "bbb" {
		t.Fatalf("sent chunks = %#v", sent)
	}
}

func stringsOf(s string, count int) string {
	out := ""
	for i := 0; i < count; i++ {
		out += s
	}
	return out
}
