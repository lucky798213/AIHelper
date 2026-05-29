package channels

import (
	"context"
	"testing"
)

type fakeChannel struct {
	name string
	msg  InboundMessage
	sent []OutboundMessage
	err  error
}

func (f *fakeChannel) Name() string {
	return f.name
}

func (f *fakeChannel) Start(ctx context.Context, out chan<- InboundMessage, errs chan<- error) {
	if f.err != nil {
		errs <- f.err
		return
	}
	out <- f.msg
}

func (f *fakeChannel) Send(ctx context.Context, msg OutboundMessage) error {
	f.sent = append(f.sent, msg)
	return nil
}

func (f *fakeChannel) Close(ctx context.Context) error {
	return nil
}

func TestManagerMergesInboundMessages(t *testing.T) {
	manager := NewManager(2)
	if err := manager.Register(&fakeChannel{name: "one", msg: InboundMessage{Channel: "one", Text: "hello"}}); err != nil {
		t.Fatalf("register one: %v", err)
	}
	if err := manager.Register(&fakeChannel{name: "two", msg: InboundMessage{Channel: "two", Text: "world"}}); err != nil {
		t.Fatalf("register two: %v", err)
	}

	manager.Start(context.Background())
	seen := map[string]bool{}
	for i := 0; i < 2; i++ {
		msg := <-manager.Inbound()
		seen[msg.Channel] = true
	}
	if !seen["one"] || !seen["two"] {
		t.Fatalf("expected both channel messages, got %#v", seen)
	}
}

func TestManagerSendRoutesByChannel(t *testing.T) {
	manager := NewManager(1)
	ch := &fakeChannel{name: "cli"}
	if err := manager.Register(ch); err != nil {
		t.Fatalf("register: %v", err)
	}
	if err := manager.Send(context.Background(), OutboundMessage{Channel: "cli", Text: "hi"}); err != nil {
		t.Fatalf("send: %v", err)
	}
	if len(ch.sent) != 1 || ch.sent[0].Text != "hi" {
		t.Fatalf("sent = %#v", ch.sent)
	}
}
