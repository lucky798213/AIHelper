package delivery

import (
	"context"
	"errors"
	mrand "math/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

func TestFileOutboxEnqueueAndReload(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	now := time.Date(2026, 5, 23, 10, 0, 0, 0, time.UTC)
	outbox, err := NewFileOutbox(FileOutboxConfig{
		Dir: dir,
		Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatalf("new outbox: %v", err)
	}

	id, err := outbox.Enqueue(ctx, Item{Channel: "feishu", To: "ou-user", ToType: "open_id", Text: "hello"})
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, id+".json")); err != nil {
		t.Fatalf("stat queued file: %v", err)
	}

	reloaded, err := NewFileOutbox(FileOutboxConfig{Dir: dir})
	if err != nil {
		t.Fatalf("new reloaded outbox: %v", err)
	}
	pending, err := reloaded.Pending(ctx)
	if err != nil {
		t.Fatalf("pending: %v", err)
	}
	if len(pending) != 1 || pending[0].ID != id || pending[0].Text != "hello" || !pending[0].EnqueuedAt.Equal(now) {
		t.Fatalf("pending = %#v", pending)
	}
}

func TestFileOutboxAckDeletesQueuedFile(t *testing.T) {
	ctx := context.Background()
	outbox, err := NewFileOutbox(FileOutboxConfig{Dir: t.TempDir()})
	if err != nil {
		t.Fatalf("new outbox: %v", err)
	}
	id, err := outbox.Enqueue(ctx, Item{Channel: "cli", To: "cli-user", Text: "done"})
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if err := outbox.Ack(ctx, id); err != nil {
		t.Fatalf("ack: %v", err)
	}
	pending, err := outbox.Pending(ctx)
	if err != nil {
		t.Fatalf("pending: %v", err)
	}
	if len(pending) != 0 {
		t.Fatalf("pending after ack = %#v", pending)
	}
}

func TestFileOutboxFailBackoffAndMoveToFailed(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 5, 23, 10, 0, 0, 0, time.UTC)
	outbox, err := NewFileOutbox(FileOutboxConfig{
		Dir:        t.TempDir(),
		MaxRetries: 2,
		Now:        func() time.Time { return now },
		Rand:       mrand.New(mrand.NewSource(1)),
	})
	if err != nil {
		t.Fatalf("new outbox: %v", err)
	}
	id, err := outbox.Enqueue(ctx, Item{Channel: "feishu", To: "ou-user", Text: "retry me"})
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	if err := outbox.Fail(ctx, id, errors.New("temporary outage")); err != nil {
		t.Fatalf("fail once: %v", err)
	}
	pending, err := outbox.Pending(ctx)
	if err != nil {
		t.Fatalf("pending: %v", err)
	}
	if len(pending) != 1 {
		t.Fatalf("pending = %#v", pending)
	}
	got := pending[0]
	if got.RetryCount != 1 || got.LastError != "temporary outage" {
		t.Fatalf("retry state = %#v", got)
	}
	delay := got.NextRetryAt.Sub(now)
	if delay < 4*time.Second || delay > 6*time.Second {
		t.Fatalf("delay = %s, want within 5s +/-20%%", delay)
	}

	if err := outbox.Fail(ctx, id, errors.New("still down")); err != nil {
		t.Fatalf("fail twice: %v", err)
	}
	pending, err = outbox.Pending(ctx)
	if err != nil {
		t.Fatalf("pending after max retry: %v", err)
	}
	if len(pending) != 0 {
		t.Fatalf("pending after max retry = %#v", pending)
	}
	failed, err := outbox.Failed(ctx)
	if err != nil {
		t.Fatalf("failed: %v", err)
	}
	if len(failed) != 1 || failed[0].RetryCount != 2 || failed[0].LastError != "still down" {
		t.Fatalf("failed = %#v", failed)
	}
}

func TestFileOutboxRetryFailed(t *testing.T) {
	ctx := context.Background()
	outbox, err := NewFileOutbox(FileOutboxConfig{Dir: t.TempDir(), MaxRetries: 1})
	if err != nil {
		t.Fatalf("new outbox: %v", err)
	}
	id, err := outbox.Enqueue(ctx, Item{Channel: "feishu", To: "ou-user", Text: "recover"})
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if err := outbox.Fail(ctx, id, errors.New("fatal")); err != nil {
		t.Fatalf("fail: %v", err)
	}

	count, err := outbox.RetryFailed(ctx)
	if err != nil {
		t.Fatalf("retry failed: %v", err)
	}
	if count != 1 {
		t.Fatalf("retry count = %d, want 1", count)
	}
	failed, err := outbox.Failed(ctx)
	if err != nil {
		t.Fatalf("failed: %v", err)
	}
	if len(failed) != 0 {
		t.Fatalf("failed after retry = %#v", failed)
	}
	pending, err := outbox.Pending(ctx)
	if err != nil {
		t.Fatalf("pending: %v", err)
	}
	if len(pending) != 1 || pending[0].RetryCount != 0 || pending[0].LastError != "" || !pending[0].NextRetryAt.IsZero() {
		t.Fatalf("pending after retry = %#v", pending)
	}
}

func TestFileOutboxCleanupRemovesOrphanTemps(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".tmp.orphan.json"), []byte("{}"), 0o644); err != nil {
		t.Fatalf("write temp: %v", err)
	}
	outbox, err := NewFileOutbox(FileOutboxConfig{Dir: dir})
	if err != nil {
		t.Fatalf("new outbox: %v", err)
	}
	if err := outbox.Cleanup(ctx); err != nil {
		t.Fatalf("cleanup: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, ".tmp.orphan.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("orphan temp still exists or stat failed unexpectedly: %v", err)
	}
}

func TestChunkMessage(t *testing.T) {
	long := strings.Repeat("a", defaultChannelLimit+10)
	cliChunks := ChunkMessage(long, "cli")
	if len(cliChunks) != 1 || cliChunks[0] != long {
		t.Fatalf("cli chunks = %#v", cliChunks)
	}

	feishuChunks := ChunkMessage(long, "feishu")
	if len(feishuChunks) != 2 {
		t.Fatalf("feishu chunk count = %d, want 2", len(feishuChunks))
	}
	for _, chunk := range feishuChunks {
		if utf8.RuneCountInString(chunk) > defaultChannelLimit {
			t.Fatalf("chunk too long: %d", utf8.RuneCountInString(chunk))
		}
	}

	text := strings.Repeat("b", 2000) + "\n\n" + strings.Repeat("c", 2000)
	chunks := ChunkMessage(text, "unknown")
	if len(chunks) != 1 || chunks[0] != text {
		t.Fatalf("paragraph chunking = %#v", chunks)
	}
}
