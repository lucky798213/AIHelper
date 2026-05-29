package feishu

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	lark "github.com/larksuite/oapi-sdk-go/v3"
	larkdispatcher "github.com/larksuite/oapi-sdk-go/v3/event/dispatcher"
	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"
	larkws "github.com/larksuite/oapi-sdk-go/v3/ws"
)

type SDKClient struct {
	appID     string
	appSecret string
	isLark    bool
	client    *larkws.Client
}

func NewSDKClient(cfg Config) *SDKClient {
	return &SDKClient{appID: cfg.AppID, appSecret: cfg.AppSecret, isLark: cfg.IsLark}
}

func (c *SDKClient) Start(ctx context.Context, handler EventHandler) error {
	dispatcher := larkdispatcher.NewEventDispatcher("", "").
		OnP2MessageReceiveV1(func(eventCtx context.Context, event *larkim.P2MessageReceiveV1) error {
			return handler.OnMessage(eventCtx, mapSDKMessageEvent(event))
		})

	opts := []larkws.ClientOption{larkws.WithEventHandler(dispatcher)}
	if c.isLark {
		opts = append(opts, larkws.WithDomain(lark.LarkBaseUrl))
	}
	c.client = larkws.NewClient(c.appID, c.appSecret, opts...)
	return c.client.Start(ctx)
}

func (c *SDKClient) Close(ctx context.Context) error {
	if c.client != nil {
		c.client.Close()
	}
	return nil
}

type SDKSender struct {
	client *lark.Client
}

func NewSDKSender(cfg Config) *SDKSender {
	opts := []lark.ClientOptionFunc{}
	if cfg.IsLark {
		opts = append(opts, lark.WithOpenBaseUrl(lark.LarkBaseUrl))
	}
	return &SDKSender{client: lark.NewClient(cfg.AppID, cfg.AppSecret, opts...)}
}

func (s *SDKSender) SendText(ctx context.Context, to string, receiveIDType string, text string) error {
	if strings.TrimSpace(receiveIDType) == "" {
		receiveIDType = "chat_id"
	}
	content, err := json.Marshal(map[string]string{"text": text})
	if err != nil {
		return err
	}
	req := larkim.NewCreateMessageReqBuilder().
		ReceiveIdType(receiveIDType).
		Body(larkim.NewCreateMessageReqBodyBuilder().
			ReceiveId(to).
			MsgType("text").
			Content(string(content)).
			Build()).
		Build()

	resp, err := s.client.Im.Message.Create(ctx, req)
	if err != nil {
		return err
	}
	if !resp.Success() {
		return fmt.Errorf("feishu send message failed: code=%d msg=%s", resp.Code, resp.Msg)
	}
	return nil
}

func mapSDKMessageEvent(event *larkim.P2MessageReceiveV1) MessageEvent {
	if event == nil || event.Event == nil || event.Event.Message == nil {
		return MessageEvent{}
	}
	msg := event.Event.Message
	var raw json.RawMessage
	if data, err := json.Marshal(event.Event); err == nil {
		raw = data
	}

	mapped := MessageEvent{
		MessageID:   deref(msg.MessageId),
		MessageType: deref(msg.MessageType),
		Content:     deref(msg.Content),
		ChatID:      deref(msg.ChatId),
		ChatType:    deref(msg.ChatType),
		Raw:         raw,
	}
	if event.Event.Sender != nil && event.Event.Sender.SenderId != nil {
		mapped.SenderOpenID = deref(event.Event.Sender.SenderId.OpenId)
	}
	for _, mention := range msg.Mentions {
		mapped.Mentions = append(mapped.Mentions, Mention{
			OpenID: deref(mention.Id.OpenId),
			UserID: deref(mention.Id.UserId),
			Key:    deref(mention.Key),
		})
	}
	return mapped
}

func deref(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}
