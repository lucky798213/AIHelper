package feishu

import (
	"context"
	"encoding/json"
)

type Config struct {
	Enabled        bool
	AccountID      string
	AppID          string
	AppSecret      string
	BotOpenID      string
	RequireMention bool
	IsLark         bool
}

type Client interface {
	Start(ctx context.Context, handler EventHandler) error
	Close(ctx context.Context) error
}

type EventHandler interface {
	OnMessage(ctx context.Context, event MessageEvent) error
}

type Sender interface {
	SendText(ctx context.Context, to string, receiveIDType string, text string) error
}

type MessageEvent struct {
	MessageID    string
	MessageType  string
	Content      string
	ChatID       string
	ChatType     string
	SenderOpenID string
	Mentions     []Mention
	Raw          json.RawMessage
}

type Mention struct {
	OpenID string
	UserID string
	Key    string
}
