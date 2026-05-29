package sessions

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"AIHelper/internal/llm"
)

func TestMemoryStoreRecordsMessages(t *testing.T) {
	store := NewMemoryStore()
	ctx := context.Background()
	key := "agent:local-master:cli:direct:cli-user"

	if err := store.Append(ctx, key, llm.Message{Role: "user", Content: "hello"}); err != nil {
		t.Fatalf("append user: %v", err)
	}
	if err := store.Append(ctx, key, llm.Message{Role: "assistant", Content: "hi"}); err != nil {
		t.Fatalf("append assistant: %v", err)
	}

	got, err := store.Load(ctx, key)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("len = %d, want 2", len(got))
	}
	if got[0].Role != "user" || got[1].Role != "assistant" {
		t.Fatalf("unexpected roles: %#v", got)
	}
}

func TestMemoryStoreReplaceCopiesMessages(t *testing.T) {
	store := NewMemoryStore()
	ctx := context.Background()
	key := "agent:local-master:cli:direct:cli-user"
	messages := []llm.Message{{
		Role:    "assistant",
		Content: "saved",
		ToolCalls: []llm.ToolCall{{
			ID:    "call-1",
			Name:  "read_file",
			Input: json.RawMessage(`{"path":"a.txt"}`),
		}},
	}}

	if err := store.Replace(ctx, key, messages); err != nil {
		t.Fatalf("replace: %v", err)
	}
	messages[0].Content = "mutated"
	messages[0].ToolCalls[0].Input[9] = 'b'

	got, err := store.Load(ctx, key)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got[0].Content != "saved" || string(got[0].ToolCalls[0].Input) != `{"path":"a.txt"}` {
		t.Fatalf("replace did not copy messages: %#v", got)
	}

	got[0].Content = "changed after load"
	got[0].ToolCalls[0].Input[9] = 'c'
	again, err := store.Load(ctx, key)
	if err != nil {
		t.Fatalf("load again: %v", err)
	}
	if again[0].Content != "saved" || string(again[0].ToolCalls[0].Input) != `{"path":"a.txt"}` {
		t.Fatalf("load exposed internal messages: %#v", again)
	}
}

func TestSQLiteStorePersistsMessagesAndToolCalls(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "sessions.db")
	key := "agent:local-master:cli:direct:cli-user"

	store := newTestSQLiteStore(t, path)
	if err := store.Append(ctx, key, llm.Message{Role: "user", Content: "hello"}); err != nil {
		t.Fatalf("append user: %v", err)
	}
	if err := store.Append(ctx, key, llm.Message{
		Role:             "assistant",
		Content:          "need tool",
		ReasoningContent: "thinking",
		ToolCalls: []llm.ToolCall{{
			ID:    "call-1",
			Name:  "read_file",
			Input: json.RawMessage(`{"path":"main.go"}`),
		}},
	}); err != nil {
		t.Fatalf("append assistant: %v", err)
	}
	if err := store.Append(ctx, key, llm.Message{
		Role:       "tool",
		Name:       "read_file",
		Content:    "package main",
		ToolCallID: "call-1",
	}); err != nil {
		t.Fatalf("append tool: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close first store: %v", err)
	}

	reopened := newTestSQLiteStore(t, path)
	got, err := reopened.Load(ctx, key)
	if err != nil {
		t.Fatalf("load reopened: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("len = %d, want 3: %#v", len(got), got)
	}
	if got[0].Role != "user" || got[0].Content != "hello" {
		t.Fatalf("unexpected user message: %#v", got[0])
	}
	if got[1].Role != "assistant" || got[1].ReasoningContent != "thinking" {
		t.Fatalf("unexpected assistant message: %#v", got[1])
	}
	if len(got[1].ToolCalls) != 1 || got[1].ToolCalls[0].ID != "call-1" || string(got[1].ToolCalls[0].Input) != `{"path":"main.go"}` {
		t.Fatalf("unexpected tool calls: %#v", got[1].ToolCalls)
	}
	if got[2].Role != "tool" || got[2].Name != "read_file" || got[2].ToolCallID != "call-1" {
		t.Fatalf("unexpected tool message: %#v", got[2])
	}
}

func TestSQLiteStoreRecordsSessionMetadata(t *testing.T) {
	ctx := context.Background()
	store := newTestSQLiteStore(t, filepath.Join(t.TempDir(), "sessions.db"))

	cases := []struct {
		key           string
		agentID       string
		parentAgentID string
		channel       string
		peerID        string
	}{
		{
			key:     "agent:local-master:cli:direct:cli-user",
			agentID: "local-master",
			channel: "cli",
			peerID:  "cli-user",
		},
		{
			key:           "agent:coder-agent:parent:local-master:feishu:ou-user",
			agentID:       "coder-agent",
			parentAgentID: "local-master",
			channel:       "feishu",
			peerID:        "ou-user",
		},
		{
			key:     "agent:local-master:main",
			agentID: "local-master",
			channel: "main",
			peerID:  "main",
		},
		{
			key:     "agent:local-master:peer:ou-user",
			agentID: "local-master",
			channel: "peer",
			peerID:  "ou-user",
		},
		{
			key:     "agent:local-master:account:feishu-primary:feishu:peer:ou-user",
			agentID: "local-master",
			channel: "feishu",
			peerID:  "ou-user",
		},
		{
			key:     "agent:local-master:cli:session:notes",
			agentID: "local-master",
			channel: "cli",
			peerID:  "session:notes",
		},
		{
			key: "not-a-known-shape",
		},
	}

	for _, tc := range cases {
		if err := store.Append(ctx, tc.key, llm.Message{Role: "user", Content: "hello"}); err != nil {
			t.Fatalf("append %s: %v", tc.key, err)
		}
		var agentID, parentAgentID, channel, peerID string
		if err := store.db.QueryRowContext(ctx, `
			SELECT agent_id, parent_agent_id, channel, peer_id
			FROM sessions
			WHERE session_key = ?
		`, tc.key).Scan(&agentID, &parentAgentID, &channel, &peerID); err != nil {
			t.Fatalf("query metadata %s: %v", tc.key, err)
		}
		if agentID != tc.agentID || parentAgentID != tc.parentAgentID || channel != tc.channel || peerID != tc.peerID {
			t.Fatalf("metadata for %s = (%q, %q, %q, %q), want (%q, %q, %q, %q)",
				tc.key,
				agentID, parentAgentID, channel, peerID,
				tc.agentID, tc.parentAgentID, tc.channel, tc.peerID,
			)
		}

		got, err := store.Load(ctx, tc.key)
		if err != nil {
			t.Fatalf("load %s: %v", tc.key, err)
		}
		if len(got) != 1 || got[0].Content != "hello" {
			t.Fatalf("messages for %s = %#v", tc.key, got)
		}
	}
}

func TestSessionStoreManagement(t *testing.T) {
	for _, tc := range []struct {
		name  string
		store Store
	}{
		{name: "memory", store: NewMemoryStore()},
		{name: "sqlite", store: newTestSQLiteStore(t, filepath.Join(t.TempDir(), "sessions.db"))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			key := "agent:local-master:cli:session:notes"
			if err := tc.store.Touch(ctx, key); err != nil {
				t.Fatalf("touch: %v", err)
			}
			metas, err := tc.store.List(ctx)
			if err != nil {
				t.Fatalf("list after touch: %v", err)
			}
			if len(metas) != 1 || metas[0].SessionKey != key || metas[0].MessageCount != 0 {
				t.Fatalf("metas after touch = %#v", metas)
			}

			if err := tc.store.Append(ctx, key, llm.Message{Role: "user", Content: "hello"}); err != nil {
				t.Fatalf("append: %v", err)
			}
			if err := tc.store.Replace(ctx, key, []llm.Message{{Role: "assistant", Content: "summary"}}); err != nil {
				t.Fatalf("replace: %v", err)
			}
			messages, err := tc.store.Load(ctx, key)
			if err != nil {
				t.Fatalf("load: %v", err)
			}
			if len(messages) != 1 || messages[0].Content != "summary" {
				t.Fatalf("messages after replace = %#v", messages)
			}
			metas, err = tc.store.List(ctx)
			if err != nil {
				t.Fatalf("list after replace: %v", err)
			}
			if len(metas) != 1 || metas[0].MessageCount != 1 {
				t.Fatalf("metas after replace = %#v", metas)
			}

			if err := tc.store.Delete(ctx, key); err != nil {
				t.Fatalf("delete: %v", err)
			}
			messages, err = tc.store.Load(ctx, key)
			if err != nil {
				t.Fatalf("load after delete: %v", err)
			}
			if len(messages) != 0 {
				t.Fatalf("messages after delete = %#v", messages)
			}
			metas, err = tc.store.List(ctx)
			if err != nil {
				t.Fatalf("list after delete: %v", err)
			}
			if len(metas) != 0 {
				t.Fatalf("metas after delete = %#v", metas)
			}
		})
	}
}

func TestCachedStoreLoadsFromDiskAndBackfillsMemory(t *testing.T) {
	ctx := context.Background()
	key := "agent:local-master:cli:direct:cli-user"
	disk := newTestSQLiteStore(t, filepath.Join(t.TempDir(), "sessions.db"))
	if err := disk.Append(ctx, key, llm.Message{Role: "user", Content: "from disk"}); err != nil {
		t.Fatalf("seed disk: %v", err)
	}

	cache := NewMemoryStore()
	store := NewCachedStore(cache, disk)
	got, err := store.Load(ctx, key)
	if err != nil {
		t.Fatalf("cached load: %v", err)
	}
	if len(got) != 1 || got[0].Content != "from disk" {
		t.Fatalf("load = %#v", got)
	}

	if err := disk.Close(); err != nil {
		t.Fatalf("close disk: %v", err)
	}
	got, err = store.Load(ctx, key)
	if err != nil {
		t.Fatalf("cached load after disk close: %v", err)
	}
	if len(got) != 1 || got[0].Content != "from disk" {
		t.Fatalf("cached load after disk close = %#v", got)
	}
}

func TestCachedStoreAppendWritesDiskBeforeCache(t *testing.T) {
	ctx := context.Background()
	key := "agent:local-master:cli:direct:cli-user"
	disk := newTestSQLiteStore(t, filepath.Join(t.TempDir(), "sessions.db"))
	cache := NewMemoryStore()
	store := NewCachedStore(cache, disk)

	if err := store.Append(ctx, key, llm.Message{Role: "user", Content: "hello"}); err != nil {
		t.Fatalf("append: %v", err)
	}
	for name, reader := range map[string]Store{"cache": cache, "disk": disk} {
		got, err := reader.Load(ctx, key)
		if err != nil {
			t.Fatalf("load %s: %v", name, err)
		}
		if len(got) != 1 || got[0].Content != "hello" {
			t.Fatalf("%s messages = %#v", name, got)
		}
	}

	failedCache := NewMemoryStore()
	failedStore := NewCachedStore(failedCache, failingStore{})
	if err := failedStore.Append(ctx, key, llm.Message{Role: "user", Content: "not cached"}); err == nil {
		t.Fatal("expected append to fail")
	}
	got, err := failedCache.Load(ctx, key)
	if err != nil {
		t.Fatalf("load failed cache: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("cache changed after disk failure: %#v", got)
	}
}

func newTestSQLiteStore(t *testing.T, path string) *SQLiteStore {
	t.Helper()
	store, err := NewSQLiteStore(path)
	if err != nil {
		t.Fatalf("new sqlite store: %v", err)
	}
	t.Cleanup(func() {
		_ = store.Close()
	})
	return store
}

type failingStore struct{}

func (failingStore) Load(context.Context, string) ([]llm.Message, error) {
	return nil, nil
}

func (failingStore) Append(context.Context, string, llm.Message) error {
	return errors.New("disk failed")
}

func (failingStore) Replace(context.Context, string, []llm.Message) error {
	return errors.New("disk failed")
}

func (failingStore) List(context.Context) ([]SessionMeta, error) {
	return nil, errors.New("disk failed")
}

func (failingStore) Delete(context.Context, string) error {
	return errors.New("disk failed")
}

func (failingStore) Touch(context.Context, string) error {
	return errors.New("disk failed")
}

func (failingStore) ClaimInboundMessage(context.Context, InboundReceipt, time.Duration) (ClaimResult, error) {
	return ClaimResult{}, errors.New("disk failed")
}

func (failingStore) CompleteInboundMessage(context.Context, string, string, string) error {
	return errors.New("disk failed")
}

func (failingStore) FailInboundMessage(context.Context, string, string, string, string) error {
	return errors.New("disk failed")
}
