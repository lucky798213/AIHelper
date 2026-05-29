package sessions

import (
	"context"
	"sort"
	"strings"
	"sync"
	"time"

	"AIHelper/internal/llm"
)

type Store interface {
	Load(ctx context.Context, sessionKey string) ([]llm.Message, error)
	Append(ctx context.Context, sessionKey string, message llm.Message) error
	Replace(ctx context.Context, sessionKey string, messages []llm.Message) error
	List(ctx context.Context) ([]SessionMeta, error)
	Delete(ctx context.Context, sessionKey string) error
	Touch(ctx context.Context, sessionKey string) error
	ClaimInboundMessage(ctx context.Context, receipt InboundReceipt, staleAfter time.Duration) (ClaimResult, error)
	CompleteInboundMessage(ctx context.Context, channel, accountID, messageID string) error
	FailInboundMessage(ctx context.Context, channel, accountID, messageID string, errText string) error
}

type SessionMeta struct {
	SessionKey    string
	AgentID       string
	ParentAgentID string
	Channel       string
	PeerID        string
	CreatedAt     time.Time
	UpdatedAt     time.Time
	MessageCount  int
}

type MemoryStore struct {
	mu       sync.RWMutex
	messages map[string][]llm.Message
	metadata map[string]SessionMeta
	receipts map[inboundReceiptKey]inboundReceiptRecord
}

func NewMemoryStore() *MemoryStore {
	return &MemoryStore{
		messages: make(map[string][]llm.Message),
		metadata: make(map[string]SessionMeta),
		receipts: make(map[inboundReceiptKey]inboundReceiptRecord),
	}
}

// 将 memory 加载，然后返回
func (s *MemoryStore) Load(ctx context.Context, sessionKey string) ([]llm.Message, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	default:
	}

	s.mu.RLock()
	defer s.mu.RUnlock()
	return copyMessages(s.messages[sessionKey]), nil
}

func (s *MemoryStore) Append(ctx context.Context, sessionKey string, message llm.Message) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.messages == nil {
		s.messages = make(map[string][]llm.Message)
	}
	s.messages[sessionKey] = append(s.messages[sessionKey], copyMessage(message))
	s.touchLocked(sessionKey)
	return nil
}

func (s *MemoryStore) Replace(ctx context.Context, sessionKey string, messages []llm.Message) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.messages == nil {
		s.messages = make(map[string][]llm.Message)
	}
	s.messages[sessionKey] = copyMessages(messages)
	s.touchLocked(sessionKey)
	return nil
}

func (s *MemoryStore) List(ctx context.Context) ([]SessionMeta, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	default:
	}

	s.mu.RLock()
	defer s.mu.RUnlock()
	metas := make([]SessionMeta, 0, len(s.metadata))
	for key, meta := range s.metadata {
		meta.MessageCount = len(s.messages[key])
		metas = append(metas, meta)
	}
	sort.SliceStable(metas, func(i, j int) bool {
		if metas[i].UpdatedAt.Equal(metas[j].UpdatedAt) {
			return metas[i].SessionKey < metas[j].SessionKey
		}
		return metas[i].UpdatedAt.After(metas[j].UpdatedAt)
	})
	return metas, nil
}

func (s *MemoryStore) Delete(ctx context.Context, sessionKey string) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.messages, sessionKey)
	delete(s.metadata, sessionKey)
	return nil
}

func (s *MemoryStore) Touch(ctx context.Context, sessionKey string) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	s.touchLocked(sessionKey)
	return nil
}

func (s *MemoryStore) ClaimInboundMessage(ctx context.Context, receipt InboundReceipt, staleAfter time.Duration) (ClaimResult, error) {
	select {
	case <-ctx.Done():
		return ClaimResult{}, ctx.Err()
	default:
	}

	normalized, key, err := normalizeInboundReceipt(receipt)
	if err != nil {
		return ClaimResult{}, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.receipts == nil {
		s.receipts = make(map[inboundReceiptKey]inboundReceiptRecord)
	}

	now := time.Now().UTC()
	existing, ok := s.receipts[key]
	if !ok {
		s.receipts[key] = inboundReceiptRecord{
			InboundReceipt: normalized,
			Status:         InboundStatusProcessing,
			Attempts:       1,
			CreatedAt:      now,
			UpdatedAt:      now,
		}
		return ClaimResult{Claimed: true, Status: InboundStatusProcessing, Attempts: 1}, nil
	}

	switch existing.Status {
	case InboundStatusCompleted:
		return ClaimResult{Duplicate: true, Status: existing.Status, Attempts: existing.Attempts}, nil
	case InboundStatusProcessing:
		if staleAfter <= 0 || now.Sub(existing.UpdatedAt) < staleAfter {
			return ClaimResult{Duplicate: true, Status: existing.Status, Attempts: existing.Attempts}, nil
		}
	}

	existing.InboundReceipt = normalized
	existing.Status = InboundStatusProcessing
	existing.Attempts++
	existing.LastError = ""
	existing.UpdatedAt = now
	existing.CompletedAt = time.Time{}
	if existing.CreatedAt.IsZero() {
		existing.CreatedAt = now
	}
	s.receipts[key] = existing
	return ClaimResult{Claimed: true, Status: existing.Status, Attempts: existing.Attempts}, nil
}

func (s *MemoryStore) CompleteInboundMessage(ctx context.Context, channel, accountID, messageID string) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}

	receipt, key, err := normalizeInboundReceipt(InboundReceipt{Channel: channel, AccountID: accountID, MessageID: messageID})
	if err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.receipts == nil {
		s.receipts = make(map[inboundReceiptKey]inboundReceiptRecord)
	}

	now := time.Now().UTC()
	existing := s.receipts[key]
	if existing.MessageID == "" {
		existing.InboundReceipt = receipt
		existing.CreatedAt = now
	}
	existing.Status = InboundStatusCompleted
	existing.LastError = ""
	existing.UpdatedAt = now
	existing.CompletedAt = now
	s.receipts[key] = existing
	return nil
}

func (s *MemoryStore) FailInboundMessage(ctx context.Context, channel, accountID, messageID string, errText string) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}

	receipt, key, err := normalizeInboundReceipt(InboundReceipt{Channel: channel, AccountID: accountID, MessageID: messageID})
	if err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.receipts == nil {
		s.receipts = make(map[inboundReceiptKey]inboundReceiptRecord)
	}

	now := time.Now().UTC()
	existing := s.receipts[key]
	if existing.MessageID == "" {
		existing.InboundReceipt = receipt
		existing.CreatedAt = now
	}
	existing.Status = InboundStatusFailed
	existing.LastError = strings.TrimSpace(errText)
	existing.UpdatedAt = now
	existing.CompletedAt = time.Time{}
	s.receipts[key] = existing
	return nil
}

func (s *MemoryStore) touchLocked(sessionKey string) {
	sessionKey = strings.TrimSpace(sessionKey)
	if sessionKey == "" {
		sessionKey = "default"
	}
	if s.messages == nil {
		s.messages = make(map[string][]llm.Message)
	}
	if s.metadata == nil {
		s.metadata = make(map[string]SessionMeta)
	}
	now := time.Now().UTC()
	meta, ok := s.metadata[sessionKey]
	if !ok {
		parsed := parseSessionKey(sessionKey)
		meta = SessionMeta{
			SessionKey:    sessionKey,
			AgentID:       parsed.AgentID,
			ParentAgentID: parsed.ParentAgentID,
			Channel:       parsed.Channel,
			PeerID:        parsed.PeerID,
			CreatedAt:     now,
		}
	}
	if meta.CreatedAt.IsZero() {
		meta.CreatedAt = now
	}
	meta.UpdatedAt = now
	meta.MessageCount = len(s.messages[sessionKey])
	s.metadata[sessionKey] = meta
}

func copyMessages(messages []llm.Message) []llm.Message {
	if len(messages) == 0 {
		return nil
	}
	copied := make([]llm.Message, len(messages))
	for i, message := range messages {
		copied[i] = copyMessage(message)
	}
	return copied
}

func copyMessage(message llm.Message) llm.Message {
	message.ToolCalls = copyToolCalls(message.ToolCalls)
	return message
}

func copyToolCalls(calls []llm.ToolCall) []llm.ToolCall {
	if len(calls) == 0 {
		return nil
	}
	copied := make([]llm.ToolCall, len(calls))
	for i, call := range calls {
		copied[i] = call
		if len(call.Input) > 0 {
			copied[i].Input = append([]byte(nil), call.Input...)
		}
	}
	return copied
}
