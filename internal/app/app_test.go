package app

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"AIHelper/internal/agent"
	"AIHelper/internal/channels"
	"AIHelper/internal/concurrency"
	"AIHelper/internal/config"
	"AIHelper/internal/gateway"
	"AIHelper/internal/heartbeat"
	"AIHelper/internal/llm"
	"AIHelper/internal/tools"
)

func TestNewRejectsBindingToSpecialistAgent(t *testing.T) {
	cfg := config.Config{
		Agents: []agent.AgentConfig{
			{ID: "local-master", Role: agent.AgentRoleMaster},
			{ID: "coder-agent", Role: agent.AgentRoleSpecialist},
		},
		Bindings: []gateway.Binding{{
			AgentID:    "coder-agent",
			Tier:       5,
			MatchKey:   "default",
			MatchValue: "*",
		}},
	}

	_, err := New(cfg, strings.NewReader(""), io.Discard)
	if err == nil {
		t.Fatal("expected binding to specialist agent to fail")
	}
}

func TestNewAcceptsBindingToMasterAgent(t *testing.T) {
	cfg := config.Config{
		Sessions: config.SessionsConfig{Driver: "memory"},
		Agents: []agent.AgentConfig{
			{ID: "local-master", Role: agent.AgentRoleMaster},
		},
		Bindings: []gateway.Binding{{
			AgentID:    "local-master",
			Tier:       5,
			MatchKey:   "default",
			MatchValue: "*",
		}},
	}

	if _, err := New(cfg, strings.NewReader(""), io.Discard); err != nil {
		t.Fatalf("new app: %v", err)
	}
}

func TestNewAcceptsSQLiteSessionsConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sessions.db")
	cfg := config.Config{
		Sessions: config.SessionsConfig{Driver: "sqlite", Path: path},
		Agents: []agent.AgentConfig{
			{ID: "local-master", Role: agent.AgentRoleMaster},
		},
		Bindings: []gateway.Binding{{
			AgentID:    "local-master",
			Tier:       5,
			MatchKey:   "default",
			MatchValue: "*",
		}},
	}

	if _, err := New(cfg, strings.NewReader(""), io.Discard); err != nil {
		t.Fatalf("new app: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("stat sqlite sessions db: %v", err)
	}
}

func TestNewUsesDefaultSQLiteSessionPath(t *testing.T) {
	cwd := t.TempDir()
	chdir(t, cwd)
	cfg := config.Config{
		Agents: []agent.AgentConfig{
			{ID: "local-master", Role: agent.AgentRoleMaster},
		},
		Bindings: []gateway.Binding{{
			AgentID:    "local-master",
			Tier:       5,
			MatchKey:   "default",
			MatchValue: "*",
		}},
	}

	if _, err := New(cfg, strings.NewReader(""), io.Discard); err != nil {
		t.Fatalf("new app: %v", err)
	}
	if _, err := os.Stat(filepath.Join(cwd, "workspace", "aihelper.db")); err != nil {
		t.Fatalf("stat default sqlite sessions db: %v", err)
	}
}

func TestNewLLMClientResilienceConfigCompatibility(t *testing.T) {
	tests := []struct {
		name     string
		cfg      config.LLMConfig
		wantAuth string
	}{
		{
			name: "single api key becomes main profile",
			cfg: config.LLMConfig{
				Provider:     "openai_compatible",
				APIKey:       "legacy-key",
				DefaultModel: "default-model",
				Resilience:   config.LLMResilienceConfig{Enabled: true},
			},
			wantAuth: "Bearer legacy-key",
		},
		{
			name: "profiles override legacy api key when resilience enabled",
			cfg: config.LLMConfig{
				Provider:     "openai_compatible",
				APIKey:       "legacy-key",
				DefaultModel: "default-model",
				Profiles: []config.LLMProfileConfig{{
					Name:   "profile-key",
					APIKey: "profile-secret",
				}},
				Resilience: config.LLMResilienceConfig{Enabled: true},
			},
			wantAuth: "Bearer profile-secret",
		},
		{
			name: "disabled resilience keeps legacy client behavior",
			cfg: config.LLMConfig{
				Provider:     "openai_compatible",
				APIKey:       "legacy-key",
				DefaultModel: "default-model",
				Profiles: []config.LLMProfileConfig{{
					Name:   "profile-key",
					APIKey: "profile-secret",
				}},
			},
			wantAuth: "Bearer legacy-key",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var gotAuth string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotAuth = r.Header.Get("Authorization")
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{
					"choices": [{
						"finish_reason": "stop",
						"message": {"role": "assistant", "content": "ok"}
					}]
				}`))
			}))
			defer server.Close()

			tt.cfg.BaseURL = server.URL
			client, err := newLLMClient(tt.cfg)
			if err != nil {
				t.Fatalf("new client: %v", err)
			}
			if _, err := client.CreateMessage(context.Background(), llm.Request{
				Messages: []llm.Message{{Role: "user", Content: "hi"}},
			}); err != nil {
				t.Fatalf("create message: %v", err)
			}
			if gotAuth != tt.wantAuth {
				t.Fatalf("Authorization = %q, want %q", gotAuth, tt.wantAuth)
			}
		})
	}
}

func TestConfigureAgentToolsAutoAddsSkillToolsForFullMode(t *testing.T) {
	registry := tools.NewRegistry()
	if err := tools.RegisterAll(registry, tools.Dependencies{
		BaseDir:      t.TempDir(),
		SkillService: appFakeSkillService{},
	}); err != nil {
		t.Fatalf("register all: %v", err)
	}
	if err := configureAgentTools(registry, []agent.AgentConfig{{
		ID: "local-master",
		Intelligence: agent.IntelligenceConfig{
			PromptMode: "full",
		},
	}}); err != nil {
		t.Fatalf("configure tools: %v", err)
	}
	for _, name := range []string{"read_skill_reference", "run_skill_command"} {
		if _, ok := registry.GetForAgent("local-master", name); !ok {
			t.Fatalf("expected auto-added tool %q", name)
		}
	}
}

func TestRunHandlesCLIPromptCommandWithoutLLMReply(t *testing.T) {
	cwd := t.TempDir()
	chdir(t, cwd)
	mustWriteApp(t, filepath.Join(cwd, "workspace", "agents", "local-master", "IDENTITY.md"), "# Identity\n\nCLI debug identity.")

	cfg := config.Config{
		Channels: config.ChannelsConfig{CLI: config.CLIConfig{Enabled: true}},
		LLM:      config.LLMConfig{Provider: "mock"},
		Agents: []agent.AgentConfig{{
			ID:   "local-master",
			Name: "Local Master",
			Role: agent.AgentRoleMaster,
		}},
		Bindings: []gateway.Binding{{
			AgentID:    "local-master",
			Tier:       5,
			MatchKey:   "default",
			MatchValue: "*",
		}},
	}
	var out bytes.Buffer
	app, err := New(cfg, strings.NewReader("/prompt local-master hello\nexit\n"), &out)
	if err != nil {
		t.Fatalf("new app: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := app.Run(ctx); err != nil {
		t.Fatalf("run: %v", err)
	}

	got := out.String()
	if !strings.Contains(got, "Prompt agent=local-master") || !strings.Contains(got, "CLI debug identity") {
		t.Fatalf("expected prompt debug output, got:\n%s", got)
	}
	if strings.Contains(got, "Assistant >") {
		t.Fatalf("CLI debug command should not send through LLM/channel, got:\n%s", got)
	}
}

func TestRunHandlesCLIBootstrapCommandShowsCoreFilesOnly(t *testing.T) {
	cwd := t.TempDir()
	chdir(t, cwd)
	workspace := filepath.Join(cwd, "workspace", "agents", "local-master")
	for _, name := range []string{"IDENTITY.md", "SOUL.md", "TOOLS.md", "MEMORY.md"} {
		mustWriteApp(t, filepath.Join(workspace, name), name+" core content")
	}
	for _, name := range []string{"USER.md", "BOOTSTRAP.md", "AGENTS.md", "HEARTBEAT.md"} {
		mustWriteApp(t, filepath.Join(workspace, name), name+" non-prompt content")
	}

	cfg := config.Config{
		Channels: config.ChannelsConfig{CLI: config.CLIConfig{Enabled: true}},
		LLM:      config.LLMConfig{Provider: "mock"},
		Agents: []agent.AgentConfig{{
			ID:   "local-master",
			Name: "Local Master",
			Role: agent.AgentRoleMaster,
		}},
		Bindings: []gateway.Binding{{
			AgentID:    "local-master",
			Tier:       5,
			MatchKey:   "default",
			MatchValue: "*",
		}},
	}
	var out bytes.Buffer
	app, err := New(cfg, strings.NewReader("/bootstrap local-master\nexit\n"), &out)
	if err != nil {
		t.Fatalf("new app: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := app.Run(ctx); err != nil {
		t.Fatalf("run: %v", err)
	}

	got := out.String()
	if !strings.Contains(got, "Bootstrap agent=local-master") {
		t.Fatalf("expected bootstrap debug output, got:\n%s", got)
	}
	for _, name := range []string{"IDENTITY.md", "SOUL.md", "TOOLS.md", "MEMORY.md"} {
		if !strings.Contains(got, name+": loaded") {
			t.Fatalf("expected %s to be reported loaded, got:\n%s", name, got)
		}
	}
	for _, name := range []string{"USER.md", "BOOTSTRAP.md", "AGENTS.md", "HEARTBEAT.md"} {
		if strings.Contains(got, name) {
			t.Fatalf("expected %s to stay out of bootstrap debug, got:\n%s", name, got)
		}
	}
}

func TestRunHandlesCLIIntelligenceReloadCommand(t *testing.T) {
	cwd := t.TempDir()
	chdir(t, cwd)
	mustWriteApp(t, filepath.Join(cwd, "workspace", "agents", "local-master", "IDENTITY.md"), "# Identity\n\nReload agent.")

	cfg := config.Config{
		Channels: config.ChannelsConfig{CLI: config.CLIConfig{Enabled: true}},
		LLM:      config.LLMConfig{Provider: "mock"},
		Agents: []agent.AgentConfig{{
			ID:   "local-master",
			Name: "Local Master",
			Role: agent.AgentRoleMaster,
		}},
		Bindings: []gateway.Binding{{
			AgentID:    "local-master",
			Tier:       5,
			MatchKey:   "default",
			MatchValue: "*",
		}},
	}
	var out bytes.Buffer
	app, err := New(cfg, strings.NewReader("/intelligence/reload\nexit\n"), &out)
	if err != nil {
		t.Fatalf("new app: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := app.Run(ctx); err != nil {
		t.Fatalf("run: %v", err)
	}
	if got := out.String(); !strings.Contains(got, "Intelligence cache reloaded.") {
		t.Fatalf("expected reload output, got:\n%s", got)
	}
}

func TestBackgroundAgentTurnRunsWhileMainLaneBusy(t *testing.T) {
	cfg := config.Config{
		Sessions: config.SessionsConfig{Driver: "memory"},
		LLM:      config.LLMConfig{Provider: "mock"},
		Agents: []agent.AgentConfig{{
			ID:   "local-master",
			Name: "Local Master",
			Role: agent.AgentRoleMaster,
		}},
	}
	app, err := New(cfg, strings.NewReader(""), io.Discard)
	if err != nil {
		t.Fatalf("new app: %v", err)
	}

	startedMain := make(chan struct{})
	releaseMain := make(chan struct{})
	mainFuture := app.commandQueue.Enqueue(context.Background(), concurrency.LaneMain, func(ctx context.Context) (any, error) {
		close(startedMain)
		<-releaseMain
		return nil, nil
	})
	waitAppClosed(t, startedMain)

	output, err := app.runBackgroundAgentTurn(context.Background(), heartbeat.Task{
		ID:      "heartbeat:local-master",
		Source:  "heartbeat",
		AgentID: "local-master",
		Target:  heartbeat.Target{Channel: "cli", PeerID: "cli-user"},
		Message: "background check",
	})
	if err != nil {
		t.Fatalf("background turn: %v", err)
	}
	if !strings.Contains(output, "background check") {
		t.Fatalf("output = %q", output)
	}
	close(releaseMain)
	if _, err := mainFuture.Result(context.Background()); err != nil {
		t.Fatalf("main future: %v", err)
	}
}

func TestBackgroundAgentTurnWaitsForSameSessionLock(t *testing.T) {
	cfg := config.Config{
		Sessions: config.SessionsConfig{Driver: "memory"},
		LLM:      config.LLMConfig{Provider: "mock"},
		Agents: []agent.AgentConfig{{
			ID:   "local-master",
			Name: "Local Master",
			Role: agent.AgentRoleMaster,
		}},
	}
	app, err := New(cfg, strings.NewReader(""), io.Discard)
	if err != nil {
		t.Fatalf("new app: %v", err)
	}

	target := heartbeat.Target{Channel: "cli", PeerID: "cli-user"}
	lock := app.sessionLock(backgroundSessionKey("local-master", target))
	lock.Lock()
	done := make(chan error, 1)
	go func() {
		_, err := app.runBackgroundAgentTurn(context.Background(), heartbeat.Task{
			ID:      "heartbeat:local-master",
			Source:  "heartbeat",
			AgentID: "local-master",
			Target:  target,
			Message: "background check",
		})
		done <- err
	}()

	select {
	case err := <-done:
		t.Fatalf("background completed before session lock was released: %v", err)
	case <-time.After(30 * time.Millisecond):
	}
	lock.Unlock()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("background turn: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("background turn did not finish after session lock release")
	}
}

func TestCronTriggerRunsThroughAppAndSendsToTarget(t *testing.T) {
	cwd := t.TempDir()
	chdir(t, cwd)
	cronPath := filepath.Join(cwd, "workspace", "CRON.json")
	mustWriteApp(t, cronPath, `{
	  "jobs": [{
	    "id": "daily",
	    "enabled": true,
	    "agent_id": "local-master",
	    "schedule": {"kind": "every", "every_seconds": 3600},
	    "target": {"channel": "cli", "peer_id": "cli-user"},
	    "payload": {"kind": "agent_turn", "message": "hello scheduled note"}
	  }]
	}`)
	cfg := config.Config{
		Channels: config.ChannelsConfig{CLI: config.CLIConfig{Enabled: true}},
		Sessions: config.SessionsConfig{Driver: "memory"},
		LLM:      config.LLMConfig{Provider: "mock"},
		Cron:     config.CronConfig{Enabled: true, Path: cronPath},
		Agents: []agent.AgentConfig{{
			ID:   "local-master",
			Name: "Local Master",
			Role: agent.AgentRoleMaster,
		}},
	}
	var out bytes.Buffer
	app, err := New(cfg, strings.NewReader(""), &out)
	if err != nil {
		t.Fatalf("new app: %v", err)
	}
	if app.cron == nil {
		t.Fatal("expected cron service")
	}

	ctx := context.Background()
	result, err := app.cron.Trigger(ctx, "daily")
	if err != nil {
		t.Fatalf("trigger cron: %v", err)
	}
	if result.Status != "ok" {
		t.Fatalf("cron result = %#v", result)
	}
	if err := app.delivery.ProcessPending(ctx); err != nil {
		t.Fatalf("process delivery: %v", err)
	}
	if got := out.String(); !strings.Contains(got, "Assistant > [local-master]") || !strings.Contains(got, "hello scheduled note") {
		t.Fatalf("expected cron output through CLI channel, got:\n%s", got)
	}
}

func TestBackgroundSendQueuesWhenDeliveryIsNotStarted(t *testing.T) {
	cwd := t.TempDir()
	chdir(t, cwd)
	cfg := config.Config{
		Channels: config.ChannelsConfig{CLI: config.CLIConfig{Enabled: true}},
		Sessions: config.SessionsConfig{Driver: "memory"},
		LLM:      config.LLMConfig{Provider: "mock"},
		Agents: []agent.AgentConfig{{
			ID:   "local-master",
			Name: "Local Master",
			Role: agent.AgentRoleMaster,
		}},
	}
	var out bytes.Buffer
	app, err := New(cfg, strings.NewReader(""), &out)
	if err != nil {
		t.Fatalf("new app: %v", err)
	}
	if app.delivery == nil {
		t.Fatal("expected delivery to be enabled by default")
	}

	err = app.sendBackground(context.Background(), heartbeat.Target{Channel: "cli", PeerID: "cli-user"}, "queued note")
	if err != nil {
		t.Fatalf("send background: %v", err)
	}
	if strings.Contains(out.String(), "queued note") {
		t.Fatalf("expected delivery to queue before runner sends, got:\n%s", out.String())
	}
	pending, err := app.delivery.Pending(context.Background())
	if err != nil {
		t.Fatalf("pending: %v", err)
	}
	if len(pending) != 1 || pending[0].Text != "queued note" {
		t.Fatalf("pending = %#v", pending)
	}
}

func TestDeliveryDisabledSendsDirectly(t *testing.T) {
	disabled := false
	cwd := t.TempDir()
	chdir(t, cwd)
	cfg := config.Config{
		Channels: config.ChannelsConfig{CLI: config.CLIConfig{Enabled: true}},
		Sessions: config.SessionsConfig{Driver: "memory"},
		Delivery: config.DeliveryConfig{Enabled: &disabled},
		Agents: []agent.AgentConfig{{
			ID:   "local-master",
			Name: "Local Master",
			Role: agent.AgentRoleMaster,
		}},
	}
	var out bytes.Buffer
	app, err := New(cfg, strings.NewReader(""), &out)
	if err != nil {
		t.Fatalf("new app: %v", err)
	}
	if app.delivery != nil {
		t.Fatal("expected delivery to be disabled")
	}

	err = app.sendBackground(context.Background(), heartbeat.Target{Channel: "cli", PeerID: "cli-user"}, "direct note")
	if err != nil {
		t.Fatalf("send background: %v", err)
	}
	if got := out.String(); !strings.Contains(got, "Assistant > direct note") {
		t.Fatalf("expected direct send, got:\n%s", got)
	}
}

func TestDeliveryCLICommands(t *testing.T) {
	cwd := t.TempDir()
	chdir(t, cwd)
	cfg := config.Config{
		Sessions: config.SessionsConfig{Driver: "memory"},
		Delivery: config.DeliveryConfig{
			MaxRetries: 1,
		},
		Agents: []agent.AgentConfig{{
			ID:   "local-master",
			Name: "Local Master",
			Role: agent.AgentRoleMaster,
		}},
	}
	var out bytes.Buffer
	app, err := New(cfg, strings.NewReader(""), &out)
	if err != nil {
		t.Fatalf("new app: %v", err)
	}

	ctx := context.Background()
	if err := app.deliver(ctx, channels.OutboundMessage{Channel: "missing", To: "peer", Text: "will fail"}); err != nil {
		t.Fatalf("deliver: %v", err)
	}
	if err := app.delivery.ProcessPending(ctx); err != nil {
		t.Fatalf("process pending: %v", err)
	}

	out.Reset()
	if err := app.handleCLICommand(ctx, "/failed"); err != nil {
		t.Fatalf("failed command: %v", err)
	}
	if got := out.String(); !strings.Contains(got, "Delivery failed") || !strings.Contains(got, "will fail") {
		t.Fatalf("expected failed delivery output, got:\n%s", got)
	}

	out.Reset()
	if err := app.handleCLICommand(ctx, "/retry"); err != nil {
		t.Fatalf("retry command: %v", err)
	}
	if got := out.String(); !strings.Contains(got, "Moved 1 failed deliveries back to queue.") {
		t.Fatalf("expected retry output, got:\n%s", got)
	}

	out.Reset()
	if err := app.handleCLICommand(ctx, "/queue"); err != nil {
		t.Fatalf("queue command: %v", err)
	}
	if got := out.String(); !strings.Contains(got, "Delivery pending") || !strings.Contains(got, "will fail") {
		t.Fatalf("expected queue output, got:\n%s", got)
	}

	out.Reset()
	if err := app.handleCLICommand(ctx, "/delivery"); err != nil {
		t.Fatalf("delivery command: %v", err)
	}
	if got := out.String(); !strings.Contains(got, "Delivery pending=1 failed=0 attempted=1 succeeded=0 errors=1") {
		t.Fatalf("expected delivery stats, got:\n%s", got)
	}
}

func TestCLISessionCommands(t *testing.T) {
	cwd := t.TempDir()
	chdir(t, cwd)
	cfg := config.Config{
		Sessions: config.SessionsConfig{Driver: "memory"},
		LLM:      config.LLMConfig{Provider: "mock"},
		Agents: []agent.AgentConfig{{
			ID:   "local-master",
			Name: "Local Master",
			Role: agent.AgentRoleMaster,
		}},
	}
	var out bytes.Buffer
	app, err := New(cfg, strings.NewReader(""), &out)
	if err != nil {
		t.Fatalf("new app: %v", err)
	}

	ctx := context.Background()
	if err := app.handleCLICommand(ctx, "/new notes"); err != nil {
		t.Fatalf("new session: %v", err)
	}
	sessionKey := "agent:local-master:cli:session:notes"
	if app.cliSessionKey != sessionKey {
		t.Fatalf("active session = %q", app.cliSessionKey)
	}
	if err := app.handleCLICommand(ctx, "/sessions"); err != nil {
		t.Fatalf("sessions command: %v", err)
	}
	if got := out.String(); !strings.Contains(got, "* session="+sessionKey) {
		t.Fatalf("expected active session output, got:\n%s", got)
	}

	out.Reset()
	if err := app.handleCLICommand(ctx, "/export notes exports/notes.jsonl"); err != nil {
		t.Fatalf("export command: %v", err)
	}
	if _, err := os.Stat(filepath.Join(cwd, "exports", "notes.jsonl")); err != nil {
		t.Fatalf("stat export: %v", err)
	}
	if got := out.String(); !strings.Contains(got, "Exported session "+sessionKey) {
		t.Fatalf("expected export output, got:\n%s", got)
	}

	out.Reset()
	if err := app.handleCLICommand(ctx, "/delete notes"); err != nil {
		t.Fatalf("delete command: %v", err)
	}
	if app.cliSessionKey != "" {
		t.Fatalf("active session after delete = %q", app.cliSessionKey)
	}
	metas, err := app.sessionStore.List(ctx)
	if err != nil {
		t.Fatalf("list sessions: %v", err)
	}
	if len(metas) != 0 {
		t.Fatalf("sessions after delete = %#v", metas)
	}
}

func TestCLIActiveSessionOverridesRoutedSession(t *testing.T) {
	cwd := t.TempDir()
	chdir(t, cwd)
	cfg := config.Config{
		Sessions: config.SessionsConfig{Driver: "memory"},
		LLM:      config.LLMConfig{Provider: "mock"},
		Agents: []agent.AgentConfig{{
			ID:   "local-master",
			Name: "Local Master",
			Role: agent.AgentRoleMaster,
		}},
		Bindings: []gateway.Binding{{
			AgentID:    "local-master",
			Tier:       5,
			MatchKey:   "default",
			MatchValue: "*",
		}},
	}
	app, err := New(cfg, strings.NewReader(""), io.Discard)
	if err != nil {
		t.Fatalf("new app: %v", err)
	}
	ctx := context.Background()
	if err := app.handleCLICommand(ctx, "/new notes"); err != nil {
		t.Fatalf("new session: %v", err)
	}
	if err := app.handleMessage(ctx, channels.InboundMessage{
		Text:    "hello active session",
		Channel: "cli",
		PeerID:  "cli-user",
	}); err != nil {
		t.Fatalf("handle message: %v", err)
	}
	activeMessages, err := app.sessionStore.Load(ctx, "agent:local-master:cli:session:notes")
	if err != nil {
		t.Fatalf("load active session: %v", err)
	}
	if len(activeMessages) == 0 || activeMessages[0].Content != "hello active session" {
		t.Fatalf("active messages = %#v", activeMessages)
	}
	defaultMessages, err := app.sessionStore.Load(ctx, "agent:local-master:cli:direct:cli-user")
	if err != nil {
		t.Fatalf("load default session: %v", err)
	}
	if len(defaultMessages) != 0 {
		t.Fatalf("default session should be untouched: %#v", defaultMessages)
	}
}

func TestHandleMessageSkipsDuplicateInboundMessage(t *testing.T) {
	cwd := t.TempDir()
	chdir(t, cwd)
	app := newInboundDedupeTestApp(t, config.SessionsConfig{Driver: "memory"}, nil)

	ctx := context.Background()
	msg := channels.InboundMessage{
		ID:          "msg-1",
		Text:        "hello from feishu",
		Channel:     "feishu",
		AccountID:   "feishu-primary",
		PeerID:      "chat-1",
		ReplyToType: "chat_id",
	}
	if err := app.handleMessage(ctx, msg); err != nil {
		t.Fatalf("handle first message: %v", err)
	}
	if err := app.handleMessage(ctx, msg); err != nil {
		t.Fatalf("handle duplicate message: %v", err)
	}

	messages, err := app.sessionStore.Load(ctx, "agent:local-master:feishu:direct:chat-1")
	if err != nil {
		t.Fatalf("load session: %v", err)
	}
	if len(messages) != 2 {
		t.Fatalf("session messages len = %d, want 2: %#v", len(messages), messages)
	}
	pending, err := app.delivery.Pending(ctx)
	if err != nil {
		t.Fatalf("pending delivery: %v", err)
	}
	if len(pending) != 1 {
		t.Fatalf("pending deliveries len = %d, want 1: %#v", len(pending), pending)
	}
}

func TestHandleMessageDedupeKeyIncludesAccountID(t *testing.T) {
	cwd := t.TempDir()
	chdir(t, cwd)
	app := newInboundDedupeTestApp(t, config.SessionsConfig{Driver: "memory"}, nil)

	ctx := context.Background()
	base := channels.InboundMessage{
		ID:          "same-message-id",
		Text:        "hello from feishu",
		Channel:     "feishu",
		PeerID:      "chat-1",
		ReplyToType: "chat_id",
	}
	first := base
	first.AccountID = "feishu-a"
	second := base
	second.AccountID = "feishu-b"
	if err := app.handleMessage(ctx, first); err != nil {
		t.Fatalf("handle first account: %v", err)
	}
	if err := app.handleMessage(ctx, second); err != nil {
		t.Fatalf("handle second account: %v", err)
	}

	messages, err := app.sessionStore.Load(ctx, "agent:local-master:feishu:direct:chat-1")
	if err != nil {
		t.Fatalf("load session: %v", err)
	}
	if len(messages) != 4 {
		t.Fatalf("session messages len = %d, want 4: %#v", len(messages), messages)
	}
	pending, err := app.delivery.Pending(ctx)
	if err != nil {
		t.Fatalf("pending delivery: %v", err)
	}
	if len(pending) != 2 {
		t.Fatalf("pending deliveries len = %d, want 2: %#v", len(pending), pending)
	}
}

func TestHandleMessageWithoutInboundIDKeepsExistingBehavior(t *testing.T) {
	cwd := t.TempDir()
	chdir(t, cwd)
	app := newInboundDedupeTestApp(t, config.SessionsConfig{Driver: "memory"}, nil)

	ctx := context.Background()
	msg := channels.InboundMessage{
		Text:        "hello without id",
		Channel:     "feishu",
		AccountID:   "feishu-primary",
		PeerID:      "chat-1",
		ReplyToType: "chat_id",
	}
	if err := app.handleMessage(ctx, msg); err != nil {
		t.Fatalf("handle first message: %v", err)
	}
	if err := app.handleMessage(ctx, msg); err != nil {
		t.Fatalf("handle second message: %v", err)
	}

	messages, err := app.sessionStore.Load(ctx, "agent:local-master:feishu:direct:chat-1")
	if err != nil {
		t.Fatalf("load session: %v", err)
	}
	if len(messages) != 4 {
		t.Fatalf("session messages len = %d, want 4: %#v", len(messages), messages)
	}
}

func TestHandleMessageFailedTurnCanRetrySameInboundMessage(t *testing.T) {
	cwd := t.TempDir()
	chdir(t, cwd)
	app := newInboundDedupeTestApp(t, config.SessionsConfig{Driver: "memory"}, []agent.AgentConfig{
		{
			ID:   "local-master",
			Name: "Local Master",
			Role: agent.AgentRoleMaster,
			Children: []agent.ChildAgentRef{{
				AgentID: "coder-agent",
				Name:    "Coder",
			}},
		},
		{ID: "coder-agent", Name: "Coder", Role: agent.AgentRoleSpecialist},
		{ID: "writer-agent", Name: "Writer", Role: agent.AgentRoleSpecialist},
	})

	ctx := context.Background()
	msg := channels.InboundMessage{
		ID:          "msg-retry-turn",
		Text:        "帮我润色一份文档",
		Channel:     "feishu",
		AccountID:   "feishu-primary",
		PeerID:      "chat-1",
		ReplyToType: "chat_id",
	}
	if err := app.handleMessage(ctx, msg); err == nil {
		t.Fatal("expected first turn to fail")
	}
	if err := app.handleMessage(ctx, msg); err == nil {
		t.Fatal("expected retry turn to fail")
	}

	messages, err := app.sessionStore.Load(ctx, "agent:local-master:feishu:direct:chat-1")
	if err != nil {
		t.Fatalf("load session: %v", err)
	}
	if len(messages) != 2 {
		t.Fatalf("session messages len = %d, want 2 retries recorded: %#v", len(messages), messages)
	}
	pending, err := app.delivery.Pending(ctx)
	if err != nil {
		t.Fatalf("pending delivery: %v", err)
	}
	if len(pending) != 0 {
		t.Fatalf("pending deliveries len = %d, want 0: %#v", len(pending), pending)
	}
}

func TestHandleMessageFailedDeliveryCanRetrySameInboundMessage(t *testing.T) {
	cwd := t.TempDir()
	chdir(t, cwd)
	app := newInboundDedupeTestApp(t, config.SessionsConfig{Driver: "memory"}, nil)
	app.delivery = nil
	app.channels = channels.NewManager(1)

	ctx := context.Background()
	msg := channels.InboundMessage{
		ID:          "msg-retry-delivery",
		Text:        "hello with delivery failure",
		Channel:     "feishu",
		AccountID:   "feishu-primary",
		PeerID:      "chat-1",
		ReplyToType: "chat_id",
	}
	if err := app.handleMessage(ctx, msg); err == nil {
		t.Fatal("expected first delivery to fail")
	}
	if err := app.handleMessage(ctx, msg); err == nil {
		t.Fatal("expected retry delivery to fail")
	}

	messages, err := app.sessionStore.Load(ctx, "agent:local-master:feishu:direct:chat-1")
	if err != nil {
		t.Fatalf("load session: %v", err)
	}
	if len(messages) != 4 {
		t.Fatalf("session messages len = %d, want 4 retries recorded: %#v", len(messages), messages)
	}
}

func TestHandleMessageSQLiteDedupeSurvivesRestart(t *testing.T) {
	cwd := t.TempDir()
	chdir(t, cwd)
	cfgSessions := config.SessionsConfig{Driver: "sqlite", Path: filepath.Join(cwd, "sessions.db")}
	app := newInboundDedupeTestApp(t, cfgSessions, nil)

	ctx := context.Background()
	msg := channels.InboundMessage{
		ID:          "msg-persisted",
		Text:        "hello persisted",
		Channel:     "feishu",
		AccountID:   "feishu-primary",
		PeerID:      "chat-1",
		ReplyToType: "chat_id",
	}
	if err := app.handleMessage(ctx, msg); err != nil {
		t.Fatalf("handle first app message: %v", err)
	}

	restarted := newInboundDedupeTestApp(t, cfgSessions, nil)
	if err := restarted.handleMessage(ctx, msg); err != nil {
		t.Fatalf("handle restarted duplicate message: %v", err)
	}

	messages, err := restarted.sessionStore.Load(ctx, "agent:local-master:feishu:direct:chat-1")
	if err != nil {
		t.Fatalf("load restarted session: %v", err)
	}
	if len(messages) != 2 {
		t.Fatalf("session messages len = %d, want 2 after restart duplicate: %#v", len(messages), messages)
	}
	pending, err := restarted.delivery.Pending(ctx)
	if err != nil {
		t.Fatalf("pending delivery: %v", err)
	}
	if len(pending) != 1 {
		t.Fatalf("pending deliveries len = %d, want 1 after restart duplicate: %#v", len(pending), pending)
	}
}

func TestCLICompactSessionCommand(t *testing.T) {
	cfg := config.Config{
		Sessions: config.SessionsConfig{Driver: "memory"},
		LLM:      config.LLMConfig{Provider: "mock"},
		Agents: []agent.AgentConfig{{
			ID:   "local-master",
			Name: "Local Master",
			Role: agent.AgentRoleMaster,
		}},
	}
	var out bytes.Buffer
	app, err := New(cfg, strings.NewReader(""), &out)
	if err != nil {
		t.Fatalf("new app: %v", err)
	}

	ctx := context.Background()
	sessionKey := "agent:local-master:cli:session:long"
	for i := 0; i < 10; i++ {
		if err := app.sessionStore.Append(ctx, sessionKey, llm.Message{
			Role:    "user",
			Content: "message " + strconv.Itoa(i),
		}); err != nil {
			t.Fatalf("append message %d: %v", i, err)
		}
	}
	if err := app.handleCLICommand(ctx, "/compact long"); err != nil {
		t.Fatalf("compact command: %v", err)
	}
	messages, err := app.sessionStore.Load(ctx, sessionKey)
	if err != nil {
		t.Fatalf("load compacted: %v", err)
	}
	if len(messages) >= 10 {
		t.Fatalf("expected compacted messages, got %d: %#v", len(messages), messages)
	}
	if got := out.String(); !strings.Contains(got, "Compacted session "+sessionKey) {
		t.Fatalf("expected compact output, got:\n%s", got)
	}
}

func TestConcurrencyCLICommands(t *testing.T) {
	cfg := config.Config{
		Sessions: config.SessionsConfig{Driver: "memory"},
		LLM:      config.LLMConfig{Provider: "mock"},
		Agents: []agent.AgentConfig{{
			ID:   "local-master",
			Name: "Local Master",
			Role: agent.AgentRoleMaster,
		}},
	}
	var out bytes.Buffer
	app, err := New(cfg, strings.NewReader(""), &out)
	if err != nil {
		t.Fatalf("new app: %v", err)
	}

	if err := app.handleCLICommand(context.Background(), "/lanes"); err != nil {
		t.Fatalf("lanes command: %v", err)
	}
	got := out.String()
	for _, lane := range []string{"Lane cron", "Lane heartbeat", "Lane main"} {
		if !strings.Contains(got, lane) {
			t.Fatalf("expected %s in lanes output, got:\n%s", lane, got)
		}
	}

	out.Reset()
	if err := app.handleCLICommand(context.Background(), "/concurrency heartbeat 2"); err != nil {
		t.Fatalf("concurrency command: %v", err)
	}
	if err := app.handleCLICommand(context.Background(), "/lanes"); err != nil {
		t.Fatalf("lanes command: %v", err)
	}
	got = out.String()
	if !strings.Contains(got, "Lane heartbeat max_concurrency=2") || !strings.Contains(got, "Lane heartbeat active=0 queued=0 max=2") {
		t.Fatalf("expected heartbeat max concurrency update, got:\n%s", got)
	}
}

func TestRunPrintsHeartbeatAndCronCommands(t *testing.T) {
	cwd := t.TempDir()
	chdir(t, cwd)
	cronPath := filepath.Join(cwd, "workspace", "CRON.json")
	mustWriteApp(t, cronPath, `{
	  "jobs": [{
	    "id": "daily",
	    "enabled": true,
	    "agent_id": "local-master",
	    "schedule": {"kind": "at", "at": "2099-01-01T09:00:00+08:00"},
	    "target": {"channel": "cli", "peer_id": "cli-user"},
	    "payload": {"kind": "agent_turn", "message": "future brief"}
	  }]
	}`)
	cfg := config.Config{
		Channels: config.ChannelsConfig{CLI: config.CLIConfig{Enabled: true}},
		Sessions: config.SessionsConfig{Driver: "memory"},
		LLM:      config.LLMConfig{Provider: "mock"},
		Heartbeat: config.HeartbeatConfig{
			Enabled: true,
			Agents: []config.HeartbeatAgentConfig{{
				AgentID: "local-master",
				Target:  config.TargetConfig{Channel: "cli", PeerID: "cli-user"},
			}},
		},
		Cron: config.CronConfig{Enabled: true, Path: cronPath},
		Agents: []agent.AgentConfig{{
			ID:   "local-master",
			Name: "Local Master",
			Role: agent.AgentRoleMaster,
		}},
	}
	var out bytes.Buffer
	app, err := New(cfg, strings.NewReader("/heartbeat\n/cron\nexit\n"), &out)
	if err != nil {
		t.Fatalf("new app: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := app.Run(ctx); err != nil {
		t.Fatalf("run: %v", err)
	}
	got := out.String()
	if !strings.Contains(got, "Heartbeat agent=local-master") {
		t.Fatalf("expected heartbeat status, got:\n%s", got)
	}
	if !strings.Contains(got, "Cron id=daily") {
		t.Fatalf("expected cron status, got:\n%s", got)
	}
}

func newInboundDedupeTestApp(t *testing.T, sessionsCfg config.SessionsConfig, agents []agent.AgentConfig) *App {
	t.Helper()
	if agents == nil {
		agents = []agent.AgentConfig{{
			ID:   "local-master",
			Name: "Local Master",
			Role: agent.AgentRoleMaster,
		}}
	}
	cfg := config.Config{
		Sessions: sessionsCfg,
		LLM:      config.LLMConfig{Provider: "mock"},
		Agents:   agents,
		Bindings: []gateway.Binding{{
			AgentID:    "local-master",
			Tier:       5,
			MatchKey:   "default",
			MatchValue: "*",
		}},
	}
	app, err := New(cfg, strings.NewReader(""), io.Discard)
	if err != nil {
		t.Fatalf("new app: %v", err)
	}
	return app
}

func chdir(t *testing.T, dir string) {
	t.Helper()
	old, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("chdir: %v", err)
	}
	t.Cleanup(func() {
		_ = os.Chdir(old)
	})
}

func mustWriteApp(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func waitAppClosed(t *testing.T, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(time.Second):
		t.Fatal("timeout waiting for channel")
	}
}

type appFakeSkillService struct{}

func (appFakeSkillService) ReadSkillReference(ctx context.Context, agentID, skillName, path string) (string, error) {
	return "reference", nil
}

func (appFakeSkillService) RunSkillCommand(ctx context.Context, agentID, skillName, command string) (string, error) {
	return "command", nil
}
