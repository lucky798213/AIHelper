package feishu

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"AIHelper/internal/channels"
)

type Channel struct {
	cfg    Config
	client Client
	sender Sender
}

func New(cfg Config, client Client, sender Sender) (*Channel, error) {
	if strings.TrimSpace(cfg.AccountID) == "" {
		cfg.AccountID = "feishu-primary"
	}
	if client == nil {
		return nil, fmt.Errorf("feishu client is required")
	}
	if sender == nil {
		return nil, fmt.Errorf("feishu sender is required")
	}
	return &Channel{cfg: cfg, client: client, sender: sender}, nil
}

func (c *Channel) Name() string {
	return "feishu"
}

func (c *Channel) Start(ctx context.Context, out chan<- channels.InboundMessage, errs chan<- error) {
	err := c.client.Start(ctx, eventHandlerFunc(func(eventCtx context.Context, event MessageEvent) error {
		msg, ok := c.parseEvent(event)
		if !ok {
			return nil
		}
		select {
		case <-eventCtx.Done():
			return eventCtx.Err()
		case out <- msg:
			return nil
		}
	}))
	if err != nil && ctx.Err() == nil {
		sendError(ctx, errs, err)
	}
}

func (c *Channel) Send(ctx context.Context, msg channels.OutboundMessage) error {
	return c.sender.SendText(ctx, msg.To, msg.ToType, msg.Text)
}

func (c *Channel) Close(ctx context.Context) error {
	return c.client.Close(ctx)
}

func (c *Channel) parseEvent(event MessageEvent) (channels.InboundMessage, bool) {
	isGroup := event.ChatType == "group" || event.ChatType == "topic_group"
	if isGroup && c.cfg.RequireMention && !c.botMentioned(event.Mentions) {
		return channels.InboundMessage{}, false
	}

	text, media := parseContent(event.MessageType, event.Content)
	text = strings.TrimSpace(text)
	if text == "" {
		return channels.InboundMessage{}, false
	}

	peerID := event.ChatID
	replyToType := "chat_id"
	if event.ChatType == "p2p" {
		peerID = event.SenderOpenID
		replyToType = "open_id"
	}
	if peerID == "" {
		return channels.InboundMessage{}, false
	}

	return channels.InboundMessage{
		ID:          event.MessageID,
		Text:        text,
		Channel:     c.Name(),
		AccountID:   c.cfg.AccountID,
		PeerID:      peerID,
		SenderID:    event.SenderOpenID,
		ReplyToType: replyToType,
		IsGroup:     isGroup,
		Media:       media,
		Raw:         event.Raw,
	}, true
}

func (c *Channel) botMentioned(mentions []Mention) bool {
	if c.cfg.BotOpenID == "" {
		return false
	}
	for _, mention := range mentions {
		if mention.OpenID == c.cfg.BotOpenID || mention.UserID == c.cfg.BotOpenID || mention.Key == c.cfg.BotOpenID {
			return true
		}
	}
	return false
}

func parseContent(messageType, raw string) (string, []channels.Media) {
	switch messageType {
	case "text":
		var body struct {
			Text string `json:"text"`
		}
		if err := json.Unmarshal([]byte(raw), &body); err != nil {
			return "", nil
		}
		return body.Text, nil
	case "post":
		return parsePostContent(raw), nil
	case "image":
		var body struct {
			ImageKey string `json:"image_key"`
		}
		if err := json.Unmarshal([]byte(raw), &body); err != nil || body.ImageKey == "" {
			return "[image]", nil
		}
		return "[image]", []channels.Media{{Type: "image", Key: body.ImageKey}}
	default:
		return "", nil
	}
}

func parsePostContent(raw string) string {
	var payload map[string]struct {
		Title   string              `json:"title"`
		Content [][]postContentNode `json:"content"`
	}
	if err := json.Unmarshal([]byte(raw), &payload); err != nil {
		return ""
	}

	var parts []string
	for _, locale := range payload {
		if locale.Title != "" {
			parts = append(parts, locale.Title)
		}
		for _, paragraph := range locale.Content {
			for _, node := range paragraph {
				switch node.Tag {
				case "text":
					parts = append(parts, node.Text)
				case "a":
					if node.Href != "" {
						parts = append(parts, strings.TrimSpace(node.Text+" "+node.Href))
					} else {
						parts = append(parts, node.Text)
					}
				}
			}
		}
	}
	return strings.Join(parts, "\n")
}

type postContentNode struct {
	Tag  string `json:"tag"`
	Text string `json:"text"`
	Href string `json:"href"`
}

type eventHandlerFunc func(ctx context.Context, event MessageEvent) error

func (f eventHandlerFunc) OnMessage(ctx context.Context, event MessageEvent) error {
	return f(ctx, event)
}

func sendError(ctx context.Context, errs chan<- error, err error) {
	if err == nil {
		return
	}
	select {
	case <-ctx.Done():
	case errs <- err:
	default:
	}
}
