package sessions

import (
	"errors"
	"strings"
	"time"
)

const (
	InboundStatusProcessing = "processing"
	InboundStatusCompleted  = "completed"
	InboundStatusFailed     = "failed"
)

type InboundReceipt struct {
	Channel    string
	AccountID  string
	MessageID  string
	SessionKey string
}

type ClaimResult struct {
	Claimed   bool
	Duplicate bool
	Status    string
	Attempts  int
}

type inboundReceiptKey struct {
	channel   string
	accountID string
	messageID string
}

type inboundReceiptRecord struct {
	InboundReceipt
	Status      string
	Attempts    int
	LastError   string
	CreatedAt   time.Time
	UpdatedAt   time.Time
	CompletedAt time.Time
}

func normalizeInboundReceipt(receipt InboundReceipt) (InboundReceipt, inboundReceiptKey, error) {
	normalized := InboundReceipt{
		Channel:    strings.TrimSpace(receipt.Channel),
		AccountID:  strings.TrimSpace(receipt.AccountID),
		MessageID:  strings.TrimSpace(receipt.MessageID),
		SessionKey: strings.TrimSpace(receipt.SessionKey),
	}
	if normalized.MessageID == "" {
		return InboundReceipt{}, inboundReceiptKey{}, errors.New("inbound message_id is required")
	}
	key := inboundReceiptKey{
		channel:   normalized.Channel,
		accountID: normalized.AccountID,
		messageID: normalized.MessageID,
	}
	return normalized, key, nil
}
