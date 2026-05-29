package channels

import (
	"context"
	"fmt"
	"sync"
)

type Manager struct {
	mu       sync.RWMutex
	channels map[string]Channel
	inbound  chan InboundMessage
	errors   chan error
}

func NewManager(buffer int) *Manager {
	if buffer <= 0 {
		buffer = 64
	}
	return &Manager{
		channels: make(map[string]Channel),
		inbound:  make(chan InboundMessage, buffer),
		errors:   make(chan error, buffer),
	}
}

func (m *Manager) Register(ch Channel) error {
	if ch == nil {
		return fmt.Errorf("register nil channel")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, exists := m.channels[ch.Name()]; exists {
		return fmt.Errorf("channel %q already registered", ch.Name())
	}
	m.channels[ch.Name()] = ch
	return nil
}

func (m *Manager) Start(ctx context.Context) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, ch := range m.channels {
		channel := ch
		go channel.Start(ctx, m.inbound, m.errors)
	}
}

func (m *Manager) Inbound() <-chan InboundMessage {
	return m.inbound
}

func (m *Manager) Errors() <-chan error {
	return m.errors
}

func (m *Manager) Send(ctx context.Context, msg OutboundMessage) error {
	m.mu.RLock()
	ch, ok := m.channels[msg.Channel]
	m.mu.RUnlock()
	if !ok {
		return fmt.Errorf("channel %q not registered", msg.Channel)
	}
	return ch.Send(ctx, msg)
}

// 程序结束，关闭 channel
func (m *Manager) Close(ctx context.Context) error {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var firstErr error
	for _, ch := range m.channels {
		if err := ch.Close(ctx); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}
