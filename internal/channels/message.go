package channels

import "encoding/json"

type InboundMessage struct {
	ID          string
	Text        string
	Channel     string
	AccountID   string
	PeerID      string
	SenderID    string
	ReplyToType string
	IsGroup     bool
	Media       []Media
	Raw         json.RawMessage
}

type Media struct {
	Type string
	Key  string
}

type OutboundMessage struct {
	Channel string
	To      string
	ToType  string
	Text    string
}
