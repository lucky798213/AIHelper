package gateway

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"AIHelper/internal/channels"
)

var ErrNoRoute = errors.New("no route matched inbound message")

type Router struct {
	bindings []Binding
}

func NewRouter(bindings []Binding) *Router {
	copied := append([]Binding(nil), bindings...)
	sort.SliceStable(copied, func(i, j int) bool {
		if copied[i].Tier == copied[j].Tier {
			return copied[i].Priority > copied[j].Priority
		}
		return copied[i].Tier < copied[j].Tier
	})
	return &Router{bindings: copied}
}

func (r *Router) Resolve(ctx context.Context, msg channels.InboundMessage) (Route, error) {
	select {
	case <-ctx.Done():
		return Route{}, ctx.Err()
	default:
	}

	for _, binding := range r.bindings {
		if matches(binding, msg) {
			return Route{
				AgentID:    binding.AgentID,
				SessionKey: buildSessionKey(binding, msg),
				Channel:    msg.Channel,
				PeerID:     msg.PeerID,
			}, nil
		}
	}
	return Route{}, ErrNoRoute
}

func matches(binding Binding, msg channels.InboundMessage) bool {
	switch strings.ToLower(binding.MatchKey) {
	case "peer_id":
		return binding.MatchValue == msg.PeerID ||
			binding.MatchValue == fmt.Sprintf("%s:%s", msg.Channel, msg.PeerID)
	case "account_id":
		return binding.MatchValue == msg.AccountID
	case "channel":
		return binding.MatchValue == msg.Channel
	case "default":
		return binding.MatchValue == "*" || binding.MatchValue == ""
	default:
		return false
	}
}

func buildSessionKey(binding Binding, msg channels.InboundMessage) string {
	agentID := valueOrDefault(binding.AgentID, "unknown")
	channel := valueOrDefault(msg.Channel, "unknown")
	peerID := valueOrDefault(msg.PeerID, "main")
	accountID := valueOrDefault(msg.AccountID, "unknown")

	switch strings.ToLower(strings.TrimSpace(binding.DMScope)) {
	case "main":
		return fmt.Sprintf("agent:%s:main", agentID)
	case "per-peer":
		return fmt.Sprintf("agent:%s:peer:%s", agentID, peerID)
	case "per-account-channel-peer":
		return fmt.Sprintf("agent:%s:account:%s:%s:peer:%s", agentID, accountID, channel, peerID)
	case "", "per-channel-peer":
		return fmt.Sprintf("agent:%s:%s:direct:%s", agentID, channel, peerID)
	default:
		return fmt.Sprintf("agent:%s:%s:direct:%s", agentID, channel, peerID)
	}
}

func valueOrDefault(value, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return strings.TrimSpace(value)
}
