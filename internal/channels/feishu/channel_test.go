package feishu

import (
	"context"
	"errors"
	"testing"

	"AIHelper/internal/channels"
)

type fakeClient struct {
	event MessageEvent
	err   error
}

func (f *fakeClient) Start(ctx context.Context, handler EventHandler) error {
	if f.err != nil {
		return f.err
	}
	return handler.OnMessage(ctx, f.event)
}

func (f *fakeClient) Close(ctx context.Context) error {
	return nil
}

type fakeSender struct {
	to            string
	receiveIDType string
	text          string
}

func (f *fakeSender) SendText(ctx context.Context, to string, receiveIDType string, text string) error {
	f.to = to
	f.receiveIDType = receiveIDType
	f.text = text
	return nil
}

func TestFeishuParsesP2PTextMessage(t *testing.T) {
	client := &fakeClient{event: MessageEvent{
		MessageID:    "msg-1",
		MessageType:  "text",
		Content:      `{"text":"hello"}`,
		ChatID:       "chat-id",
		ChatType:     "p2p",
		SenderOpenID: "ou-user",
	}}
	ch, err := New(Config{AccountID: "feishu-primary"}, client, &fakeSender{})
	if err != nil {
		t.Fatalf("new channel: %v", err)
	}

	out := make(chan channels.InboundMessage, 1)
	errs := make(chan error, 1)
	ch.Start(context.Background(), out, errs)

	msg := <-out
	if msg.Channel != "feishu" || msg.PeerID != "ou-user" || msg.ReplyToType != "open_id" || msg.Text != "hello" || msg.IsGroup {
		t.Fatalf("unexpected message: %#v", msg)
	}
}

func TestFeishuRequiresMentionInGroup(t *testing.T) {
	event := MessageEvent{
		MessageType:  "text",
		Content:      `{"text":"hello group"}`,
		ChatID:       "oc-group",
		ChatType:     "group",
		SenderOpenID: "ou-user",
	}
	ch, err := New(Config{BotOpenID: "ou-bot", RequireMention: true}, &fakeClient{event: event}, &fakeSender{})
	if err != nil {
		t.Fatalf("new channel: %v", err)
	}
	out := make(chan channels.InboundMessage, 1)
	errs := make(chan error, 1)
	ch.Start(context.Background(), out, errs)
	select {
	case msg := <-out:
		t.Fatalf("expected no message, got %#v", msg)
	default:
	}

	event.Mentions = []Mention{{OpenID: "ou-bot"}}
	ch, err = New(Config{BotOpenID: "ou-bot", RequireMention: true}, &fakeClient{event: event}, &fakeSender{})
	if err != nil {
		t.Fatalf("new mentioned channel: %v", err)
	}
	ch.Start(context.Background(), out, errs)
	msg := <-out
	if msg.PeerID != "oc-group" || msg.ReplyToType != "chat_id" || !msg.IsGroup {
		t.Fatalf("unexpected mentioned group message: %#v", msg)
	}
}

func TestFeishuSendUsesOutboundReceiveIDType(t *testing.T) {
	sender := &fakeSender{}
	ch, err := New(Config{}, &fakeClient{}, sender)
	if err != nil {
		t.Fatalf("new channel: %v", err)
	}
	if err := ch.Send(context.Background(), channels.OutboundMessage{
		To:     "ou-user",
		ToType: "open_id",
		Text:   "hello",
	}); err != nil {
		t.Fatalf("send: %v", err)
	}
	if sender.to != "ou-user" || sender.receiveIDType != "open_id" || sender.text != "hello" {
		t.Fatalf("unexpected send args: %#v", sender)
	}
}

func TestFeishuParsesImageMessage(t *testing.T) {
	ch, err := New(Config{}, &fakeClient{event: MessageEvent{
		MessageType:  "image",
		Content:      `{"image_key":"img-key"}`,
		ChatID:       "oc-group",
		ChatType:     "group",
		SenderOpenID: "ou-user",
	}}, &fakeSender{})
	if err != nil {
		t.Fatalf("new channel: %v", err)
	}
	out := make(chan channels.InboundMessage, 1)
	errs := make(chan error, 1)
	ch.Start(context.Background(), out, errs)
	msg := <-out
	if msg.Text != "[image]" || len(msg.Media) != 1 || msg.Media[0].Key != "img-key" {
		t.Fatalf("unexpected image message: %#v", msg)
	}
}

func TestFeishuReportsClientError(t *testing.T) {
	ch, err := New(Config{}, &fakeClient{err: errors.New("connect failed")}, &fakeSender{})
	if err != nil {
		t.Fatalf("new channel: %v", err)
	}
	out := make(chan channels.InboundMessage, 1)
	errs := make(chan error, 1)
	ch.Start(context.Background(), out, errs)
	if got := <-errs; got == nil {
		t.Fatal("expected error")
	}
}
