package sessions

import (
	"context"
	"time"

	"AIHelper/internal/llm"
)

type CachedStore struct {
	cache *MemoryStore
	disk  Store
}

func NewCachedStore(cache *MemoryStore, disk Store) *CachedStore {
	if cache == nil {
		cache = NewMemoryStore()
	}
	return &CachedStore{cache: cache, disk: disk}
}

func (s *CachedStore) Load(ctx context.Context, sessionKey string) ([]llm.Message, error) {
	cached, err := s.cache.Load(ctx, sessionKey)
	if err != nil {
		return nil, err
	}
	if len(cached) > 0 || s.disk == nil {
		return cached, nil
	}

	messages, err := s.disk.Load(ctx, sessionKey)
	if err != nil {
		return nil, err
	}
	if len(messages) > 0 {
		if err := s.cache.Replace(ctx, sessionKey, messages); err != nil {
			return nil, err
		}
	}
	return messages, nil
}

func (s *CachedStore) Append(ctx context.Context, sessionKey string, message llm.Message) error {
	if s.disk != nil {
		if err := s.disk.Append(ctx, sessionKey, message); err != nil {
			return err
		}
	}
	return s.cache.Append(ctx, sessionKey, message)
}

func (s *CachedStore) Replace(ctx context.Context, sessionKey string, messages []llm.Message) error {
	if s.disk != nil {
		if err := s.disk.Replace(ctx, sessionKey, messages); err != nil {
			return err
		}
	}
	return s.cache.Replace(ctx, sessionKey, messages)
}

func (s *CachedStore) List(ctx context.Context) ([]SessionMeta, error) {
	if s.disk != nil {
		return s.disk.List(ctx)
	}
	return s.cache.List(ctx)
}

func (s *CachedStore) Delete(ctx context.Context, sessionKey string) error {
	if s.disk != nil {
		if err := s.disk.Delete(ctx, sessionKey); err != nil {
			return err
		}
	}
	return s.cache.Delete(ctx, sessionKey)
}

func (s *CachedStore) Touch(ctx context.Context, sessionKey string) error {
	if s.disk != nil {
		if err := s.disk.Touch(ctx, sessionKey); err != nil {
			return err
		}
	}
	return s.cache.Touch(ctx, sessionKey)
}

func (s *CachedStore) ClaimInboundMessage(ctx context.Context, receipt InboundReceipt, staleAfter time.Duration) (ClaimResult, error) {
	if s.disk != nil {
		return s.disk.ClaimInboundMessage(ctx, receipt, staleAfter)
	}
	return s.cache.ClaimInboundMessage(ctx, receipt, staleAfter)
}

func (s *CachedStore) CompleteInboundMessage(ctx context.Context, channel, accountID, messageID string) error {
	if s.disk != nil {
		return s.disk.CompleteInboundMessage(ctx, channel, accountID, messageID)
	}
	return s.cache.CompleteInboundMessage(ctx, channel, accountID, messageID)
}

func (s *CachedStore) FailInboundMessage(ctx context.Context, channel, accountID, messageID string, errText string) error {
	if s.disk != nil {
		return s.disk.FailInboundMessage(ctx, channel, accountID, messageID, errText)
	}
	return s.cache.FailInboundMessage(ctx, channel, accountID, messageID, errText)
}
