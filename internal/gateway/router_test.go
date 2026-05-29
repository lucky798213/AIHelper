package gateway

import (
	"context"
	"testing"

	"AIHelper/internal/channels"
)

func TestRouterDefaultBinding(t *testing.T) {
	router := NewRouter([]Binding{
		{
			AgentID:    "local-master",
			Tier:       5,
			MatchKey:   "default",
			MatchValue: "*",
		},
	})

	route, err := router.Resolve(context.Background(), channels.InboundMessage{
		Channel: "cli",
		PeerID:  "cli-user",
	})
	if err != nil {
		t.Fatalf("resolve route: %v", err)
	}
	if route.AgentID != "local-master" {
		t.Fatalf("AgentID = %q, want local-master", route.AgentID)
	}
	if route.SessionKey != "agent:local-master:cli:direct:cli-user" {
		t.Fatalf("SessionKey = %q", route.SessionKey)
	}
}

func TestRouterDMScopeSessionKeys(t *testing.T) {
	msg := channels.InboundMessage{
		Channel:   "feishu",
		AccountID: "account-a",
		PeerID:    "peer-a",
	}
	tests := []struct {
		name    string
		scope   string
		wantKey string
	}{
		{name: "main", scope: "main", wantKey: "agent:local-master:main"},
		{name: "per peer", scope: "per-peer", wantKey: "agent:local-master:peer:peer-a"},
		{name: "per channel peer", scope: "per-channel-peer", wantKey: "agent:local-master:feishu:direct:peer-a"},
		{name: "per account channel peer", scope: "per-account-channel-peer", wantKey: "agent:local-master:account:account-a:feishu:peer:peer-a"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			router := NewRouter([]Binding{{
				AgentID:    "local-master",
				Tier:       5,
				MatchKey:   "default",
				MatchValue: "*",
				DMScope:    tt.scope,
			}})
			route, err := router.Resolve(context.Background(), msg)
			if err != nil {
				t.Fatalf("resolve route: %v", err)
			}
			if route.SessionKey != tt.wantKey {
				t.Fatalf("SessionKey = %q, want %q", route.SessionKey, tt.wantKey)
			}
		})
	}
}
