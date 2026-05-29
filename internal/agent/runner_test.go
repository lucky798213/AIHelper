package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"AIHelper/internal/channels"
	"AIHelper/internal/gateway"
	"AIHelper/internal/llm"
	"AIHelper/internal/sessions"
	"AIHelper/internal/tools"
)

type runnerFakeTool struct{}

func (runnerFakeTool) Name() string {
	return "record_tool"
}

func (runnerFakeTool) Schema() llm.ToolSchema {
	return llm.ToolSchema{
		Name:        "record_tool",
		Description: "record",
		InputSchema: map[string]any{
			"type": "object",
		},
	}
}

func (runnerFakeTool) Call(ctx context.Context, call tools.CallContext, input json.RawMessage) (tools.ToolResult, error) {
	return tools.ToolResult{Content: "tool ok"}, nil
}

type reasoningToolClient struct {
	answerRequests []llm.Request
}

func (c *reasoningToolClient) CreateMessage(ctx context.Context, req llm.Request) (llm.Response, error) {
	if req.Purpose == "dispatch" {
		return llm.Response{
			StopReason: llm.StopReasonEndTurn,
			Text:       `{"mode":"direct","input":"please use a tool"}`,
		}, nil
	}

	c.answerRequests = append(c.answerRequests, req)
	if len(c.answerRequests) == 1 {
		return llm.Response{
			StopReason:       llm.StopReasonToolUse,
			ReasoningContent: "tool reasoning",
			ToolCalls: []llm.ToolCall{{
				ID:    "call_1",
				Name:  "record_tool",
				Input: json.RawMessage(`{}`),
			}},
		}, nil
	}
	return llm.Response{
		StopReason: llm.StopReasonEndTurn,
		Text:       "final answer",
	}, nil
}

type planOrchestrationClient struct {
	dispatchText       string
	synthesizeText     string
	coderUsesTool      bool
	answerCounts       map[string]int
	answerRequests     []llm.Request
	synthesizeRequests []llm.Request
}

func (c *planOrchestrationClient) CreateMessage(ctx context.Context, req llm.Request) (llm.Response, error) {
	switch req.Purpose {
	case "dispatch":
		return llm.Response{StopReason: llm.StopReasonEndTurn, Text: c.dispatchText}, nil
	case "synthesize":
		c.synthesizeRequests = append(c.synthesizeRequests, req)
		text := c.synthesizeText
		if text == "" {
			text = "final synthesis"
		}
		return llm.Response{StopReason: llm.StopReasonEndTurn, Text: text}, nil
	case "answer":
		if c.answerCounts == nil {
			c.answerCounts = make(map[string]int)
		}
		count := c.answerCounts[req.AgentID]
		c.answerCounts[req.AgentID] = count + 1
		c.answerRequests = append(c.answerRequests, req)
		if c.coderUsesTool && req.AgentID == "coder-agent" && count == 0 {
			return llm.Response{
				StopReason: llm.StopReasonToolUse,
				ToolCalls: []llm.ToolCall{{
					ID:    "call_1",
					Name:  "record_tool",
					Input: json.RawMessage(`{}`),
				}},
			}, nil
		}
		switch req.AgentID {
		case "coder-agent":
			if c.coderUsesTool {
				return llm.Response{StopReason: llm.StopReasonEndTurn, Text: "coder used tool"}, nil
			}
			return llm.Response{StopReason: llm.StopReasonEndTurn, Text: "coder output"}, nil
		case "writer-agent":
			return llm.Response{StopReason: llm.StopReasonEndTurn, Text: "writer output"}, nil
		default:
			return llm.Response{StopReason: llm.StopReasonEndTurn, Text: "answer from " + req.AgentID}, nil
		}
	default:
		return llm.Response{}, fmt.Errorf("unexpected purpose %q", req.Purpose)
	}
}

func newTestRunner(t *testing.T, configs []AgentConfig) (*Runner, sessions.Store) {
	t.Helper()

	manager, err := NewManager(configs)
	if err != nil {
		t.Fatalf("new manager: %v", err)
	}
	registry := tools.NewRegistry()
	for _, cfg := range configs {
		if err := registry.SetAllowedTools(cfg.ID, cfg.Tools); err != nil {
			t.Fatalf("set allowed tools: %v", err)
		}
	}
	store := sessions.NewMemoryStore()
	return NewRunner(manager, llm.NewMockClient(), registry, store), store
}

func TestRunnerDispatchesToCoderAgent(t *testing.T) {
	runner, store := newTestRunner(t, []AgentConfig{
		{
			ID:   "local-master",
			Name: "本地主控",
			Role: AgentRoleMaster,
			Children: []ChildAgentRef{{
				AgentID: "coder-agent",
				Name:    "代码专家",
			}},
		},
		{ID: "coder-agent", Name: "代码专家", Role: AgentRoleSpecialist},
	})

	out, err := runner.RunTurn(context.Background(), channels.InboundMessage{
		Text:    "帮我设计 Go 项目结构",
		Channel: "cli",
		PeerID:  "cli-user",
	}, gateway.Route{
		AgentID:    "local-master",
		SessionKey: "agent:local-master:cli:direct:cli-user",
		Channel:    "cli",
		PeerID:     "cli-user",
	})
	if err != nil {
		t.Fatalf("run turn: %v", err)
	}
	if !strings.Contains(out.Text, "[coder-agent]") {
		t.Fatalf("output does not come from coder-agent: %s", out.Text)
	}

	masterHistory, err := store.Load(context.Background(), "agent:local-master:cli:direct:cli-user")
	if err != nil {
		t.Fatalf("load master session: %v", err)
	}
	if len(masterHistory) != 2 {
		t.Fatalf("master history len = %d, want 2: %#v", len(masterHistory), masterHistory)
	}
	if masterHistory[1].Content != out.Text {
		t.Fatalf("master assistant message = %q, want %q", masterHistory[1].Content, out.Text)
	}

	childHistory, err := store.Load(context.Background(), "agent:coder-agent:parent:local-master:cli:cli-user")
	if err != nil {
		t.Fatalf("load child session: %v", err)
	}
	if len(childHistory) != 2 {
		t.Fatalf("child history len = %d, want 2: %#v", len(childHistory), childHistory)
	}
}

type promptCaptureClient struct {
	dispatchText   string
	answerRequests []llm.Request
}

func (c *promptCaptureClient) CreateMessage(ctx context.Context, req llm.Request) (llm.Response, error) {
	if req.Purpose == "dispatch" {
		text := c.dispatchText
		if text == "" {
			text = `{"mode":"direct","input":"hello"}`
		}
		return llm.Response{StopReason: llm.StopReasonEndTurn, Text: text}, nil
	}
	c.answerRequests = append(c.answerRequests, req)
	return llm.Response{StopReason: llm.StopReasonEndTurn, Text: "final answer"}, nil
}

type capturePromptBuilder struct {
	requests []PromptBuildRequest
}

func (b *capturePromptBuilder) BuildSystemPrompt(ctx context.Context, req PromptBuildRequest) (string, error) {
	b.requests = append(b.requests, req)
	return "built prompt for " + req.Agent.ID, nil
}

func TestRunnerDispatchesToWriterAgent(t *testing.T) {
	runner, _ := newTestRunner(t, []AgentConfig{
		{
			ID:   "local-master",
			Name: "本地主控",
			Role: AgentRoleMaster,
			Children: []ChildAgentRef{{
				AgentID: "writer-agent",
				Name:    "文档专家",
			}},
		},
		{ID: "writer-agent", Name: "文档专家", Role: AgentRoleSpecialist},
	})

	out, err := runner.RunTurn(context.Background(), channels.InboundMessage{
		Text:    "帮我润色一份文档",
		Channel: "cli",
		PeerID:  "cli-user",
	}, gateway.Route{
		AgentID:    "local-master",
		SessionKey: "agent:local-master:cli:direct:cli-user",
		Channel:    "cli",
		PeerID:     "cli-user",
	})
	if err != nil {
		t.Fatalf("run turn: %v", err)
	}
	if !strings.Contains(out.Text, "[writer-agent]") {
		t.Fatalf("output does not come from writer-agent: %s", out.Text)
	}
}

func TestRunnerDirectAnswerUsesMasterAgent(t *testing.T) {
	runner, _ := newTestRunner(t, []AgentConfig{
		{
			ID:   "local-master",
			Name: "本地主控",
			Role: AgentRoleMaster,
		},
	})

	out, err := runner.RunTurn(context.Background(), channels.InboundMessage{
		Text:    "你好",
		Channel: "cli",
		PeerID:  "cli-user",
	}, gateway.Route{
		AgentID:    "local-master",
		SessionKey: "agent:local-master:cli:direct:cli-user",
		Channel:    "cli",
		PeerID:     "cli-user",
	})
	if err != nil {
		t.Fatalf("run turn: %v", err)
	}
	if !strings.Contains(out.Text, "[local-master]") {
		t.Fatalf("output does not come from master agent: %s", out.Text)
	}
}

func TestRunnerRejectsUnconfiguredDispatchedChild(t *testing.T) {
	runner, _ := newTestRunner(t, []AgentConfig{
		{
			ID:   "local-master",
			Name: "本地主控",
			Role: AgentRoleMaster,
			Children: []ChildAgentRef{{
				AgentID: "coder-agent",
				Name:    "代码专家",
			}},
		},
		{ID: "coder-agent", Name: "代码专家", Role: AgentRoleSpecialist},
		{ID: "writer-agent", Name: "文档专家", Role: AgentRoleSpecialist},
	})

	_, err := runner.RunTurn(context.Background(), channels.InboundMessage{
		Text:    "帮我润色一份文档",
		Channel: "cli",
		PeerID:  "cli-user",
	}, gateway.Route{
		AgentID:    "local-master",
		SessionKey: "agent:local-master:cli:direct:cli-user",
		Channel:    "cli",
		PeerID:     "cli-user",
	})
	if err == nil {
		t.Fatal("expected dispatch to unconfigured child to fail")
	}
}

func TestRunnerPassesReasoningContentBackAfterToolCall(t *testing.T) {
	manager, err := NewManager([]AgentConfig{{
		ID:    "local-master",
		Role:  AgentRoleMaster,
		Tools: []string{"record_tool"},
	}})
	if err != nil {
		t.Fatalf("new manager: %v", err)
	}
	registry := tools.NewRegistry()
	if err := registry.Register(runnerFakeTool{}); err != nil {
		t.Fatalf("register tool: %v", err)
	}
	if err := registry.SetAllowedTools("local-master", []string{"record_tool"}); err != nil {
		t.Fatalf("set allowed tools: %v", err)
	}
	client := &reasoningToolClient{}
	runner := NewRunner(manager, client, registry, sessions.NewMemoryStore())

	out, err := runner.RunTurn(context.Background(), channels.InboundMessage{
		Text:    "please use a tool",
		Channel: "cli",
		PeerID:  "cli-user",
	}, gateway.Route{
		AgentID:    "local-master",
		SessionKey: "agent:local-master:cli:direct:cli-user",
		Channel:    "cli",
		PeerID:     "cli-user",
	})
	if err != nil {
		t.Fatalf("run turn: %v", err)
	}
	if out.Text != "final answer" {
		t.Fatalf("out text = %q", out.Text)
	}
	if len(client.answerRequests) != 2 {
		t.Fatalf("answer requests len = %d", len(client.answerRequests))
	}
	second := client.answerRequests[1]
	var found bool
	for _, msg := range second.Messages {
		if msg.Role == "assistant" && len(msg.ToolCalls) == 1 {
			found = true
			if msg.ReasoningContent != "tool reasoning" {
				t.Fatalf("reasoning content = %q", msg.ReasoningContent)
			}
		}
	}
	if !found {
		t.Fatalf("tool-call assistant message not found: %#v", second.Messages)
	}
}

func TestRunnerUsesPromptBuilderForAnswerOnly(t *testing.T) {
	manager, err := NewManager([]AgentConfig{{
		ID:   "local-master",
		Name: "Local Master",
		Role: AgentRoleMaster,
	}})
	if err != nil {
		t.Fatalf("new manager: %v", err)
	}
	registry := tools.NewRegistry()
	if err := registry.SetAllowedTools("local-master", nil); err != nil {
		t.Fatalf("set allowed tools: %v", err)
	}
	client := &promptCaptureClient{dispatchText: `{"mode":"direct","input":"hello"}`}
	builder := &capturePromptBuilder{}
	runner := NewRunner(manager, client, registry, sessions.NewMemoryStore())
	runner.SetPromptBuilder(builder)

	_, err = runner.RunTurn(context.Background(), channels.InboundMessage{
		Text:    "hello",
		Channel: "cli",
		PeerID:  "cli-user",
	}, gateway.Route{
		AgentID:    "local-master",
		SessionKey: "agent:local-master:cli:direct:cli-user",
		Channel:    "cli",
		PeerID:     "cli-user",
	})
	if err != nil {
		t.Fatalf("run turn: %v", err)
	}
	if len(builder.requests) != 1 {
		t.Fatalf("prompt builder calls = %d", len(builder.requests))
	}
	req := builder.requests[0]
	if req.Agent.ID != "local-master" || req.UserInput != "hello" || req.Channel != "cli" {
		t.Fatalf("unexpected prompt request: %#v", req)
	}
	if len(client.answerRequests) != 1 || client.answerRequests[0].System != "built prompt for local-master" {
		t.Fatalf("unexpected answer request: %#v", client.answerRequests)
	}
}

func TestRunnerUsesChildPromptBuilderForDelegatedAgent(t *testing.T) {
	manager, err := NewManager([]AgentConfig{
		{
			ID:   "local-master",
			Role: AgentRoleMaster,
			Children: []ChildAgentRef{{
				AgentID: "coder-agent",
				Name:    "Coder",
			}},
		},
		{ID: "coder-agent", Role: AgentRoleSpecialist},
	})
	if err != nil {
		t.Fatalf("new manager: %v", err)
	}
	registry := tools.NewRegistry()
	if err := registry.SetAllowedTools("local-master", nil); err != nil {
		t.Fatalf("set master tools: %v", err)
	}
	if err := registry.SetAllowedTools("coder-agent", nil); err != nil {
		t.Fatalf("set coder tools: %v", err)
	}
	client := &promptCaptureClient{dispatchText: `{"mode":"delegate","agent_id":"coder-agent","input":"please code"}`}
	builder := &capturePromptBuilder{}
	runner := NewRunner(manager, client, registry, sessions.NewMemoryStore())
	runner.SetPromptBuilder(builder)

	_, err = runner.RunTurn(context.Background(), channels.InboundMessage{
		Text:    "please code",
		Channel: "feishu",
		PeerID:  "ou-user",
	}, gateway.Route{
		AgentID:    "local-master",
		SessionKey: "agent:local-master:feishu:direct:ou-user",
		Channel:    "feishu",
		PeerID:     "ou-user",
	})
	if err != nil {
		t.Fatalf("run turn: %v", err)
	}
	if len(builder.requests) != 1 {
		t.Fatalf("prompt builder calls = %d", len(builder.requests))
	}
	req := builder.requests[0]
	if req.Agent.ID != "coder-agent" || req.UserInput != "please code" || req.Channel != "feishu" {
		t.Fatalf("unexpected prompt request: %#v", req)
	}
	if len(client.answerRequests) != 1 || client.answerRequests[0].System != "built prompt for coder-agent" {
		t.Fatalf("unexpected answer request: %#v", client.answerRequests)
	}
}

func TestRunnerExecutesPlanStepsAndSynthesizes(t *testing.T) {
	manager, err := NewManager([]AgentConfig{
		{
			ID:   "local-master",
			Name: "本地主控",
			Role: AgentRoleMaster,
			Children: []ChildAgentRef{
				{AgentID: "coder-agent", Name: "代码专家"},
				{AgentID: "writer-agent", Name: "文档专家"},
			},
		},
		{ID: "coder-agent", Name: "代码专家", Role: AgentRoleSpecialist},
		{ID: "writer-agent", Name: "文档专家", Role: AgentRoleSpecialist},
	})
	if err != nil {
		t.Fatalf("new manager: %v", err)
	}
	registry := tools.NewRegistry()
	for _, agentID := range []string{"local-master", "coder-agent", "writer-agent"} {
		if err := registry.SetAllowedTools(agentID, nil); err != nil {
			t.Fatalf("set allowed tools: %v", err)
		}
	}
	client := &planOrchestrationClient{dispatchText: `{"mode":"plan","steps":[{"id":"code","agent_id":"coder-agent","input":"implement the change"},{"id":"write","agent_id":"writer-agent","input":"summarize the change"}],"final_instruction":"merge both specialist results"}`}
	store := sessions.NewMemoryStore()
	runner := NewRunner(manager, client, registry, store)

	out, err := runner.RunTurn(context.Background(), channels.InboundMessage{
		Text:    "复杂任务，需要代码和文档专家一起完成",
		Channel: "cli",
		PeerID:  "cli-user",
	}, gateway.Route{
		AgentID:    "local-master",
		SessionKey: "agent:local-master:cli:direct:cli-user",
		Channel:    "cli",
		PeerID:     "cli-user",
	})
	if err != nil {
		t.Fatalf("run turn: %v", err)
	}
	if out.Text != "final synthesis" {
		t.Fatalf("out text = %q", out.Text)
	}
	if len(client.answerRequests) != 2 {
		t.Fatalf("answer requests len = %d", len(client.answerRequests))
	}
	if client.answerRequests[0].AgentID != "coder-agent" || client.answerRequests[1].AgentID != "writer-agent" {
		t.Fatalf("unexpected answer order: %#v", client.answerRequests)
	}
	if len(client.synthesizeRequests) != 1 {
		t.Fatalf("synthesize requests len = %d", len(client.synthesizeRequests))
	}
	synthesisInput := client.synthesizeRequests[0].Messages[0].Content
	for _, want := range []string{"复杂任务，需要代码和文档专家一起完成", "merge both specialist results", "coder output", "writer output"} {
		if !strings.Contains(synthesisInput, want) {
			t.Fatalf("synthesis input missing %q: %s", want, synthesisInput)
		}
	}

	masterHistory, err := store.Load(context.Background(), "agent:local-master:cli:direct:cli-user")
	if err != nil {
		t.Fatalf("load master session: %v", err)
	}
	if len(masterHistory) != 2 || masterHistory[1].Content != "final synthesis" {
		t.Fatalf("unexpected master history: %#v", masterHistory)
	}
	for _, key := range []string{
		"agent:coder-agent:parent:local-master:cli:cli-user:step:code",
		"agent:writer-agent:parent:local-master:cli:cli-user:step:write",
	} {
		history, err := store.Load(context.Background(), key)
		if err != nil {
			t.Fatalf("load child step session %s: %v", key, err)
		}
		if len(history) != 2 {
			t.Fatalf("child step session %s len = %d, want 2: %#v", key, len(history), history)
		}
	}
}

func TestRunnerRejectsPlanWithUnconfiguredChild(t *testing.T) {
	runner, _ := newTestRunner(t, []AgentConfig{
		{
			ID:   "local-master",
			Name: "本地主控",
			Role: AgentRoleMaster,
			Children: []ChildAgentRef{{
				AgentID: "coder-agent",
				Name:    "代码专家",
			}},
		},
		{ID: "coder-agent", Name: "代码专家", Role: AgentRoleSpecialist},
		{ID: "writer-agent", Name: "文档专家", Role: AgentRoleSpecialist},
	})
	runner.client = &planOrchestrationClient{dispatchText: `{"mode":"plan","steps":[{"id":"write","agent_id":"writer-agent","input":"write docs"}]}`}

	_, err := runner.RunTurn(context.Background(), channels.InboundMessage{
		Text:    "需要文档专家",
		Channel: "cli",
		PeerID:  "cli-user",
	}, gateway.Route{
		AgentID:    "local-master",
		SessionKey: "agent:local-master:cli:direct:cli-user",
		Channel:    "cli",
		PeerID:     "cli-user",
	})
	if err == nil {
		t.Fatal("expected plan with unconfigured child to fail")
	}
	if !strings.Contains(err.Error(), `cannot dispatch plan step "write" to "writer-agent"`) {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestRunnerRejectsInvalidPlanSteps(t *testing.T) {
	longSteps := make([]string, 0, maxDispatchPlanSteps+1)
	for i := 0; i < maxDispatchPlanSteps+1; i++ {
		longSteps = append(longSteps, fmt.Sprintf(`{"id":"step-%d","agent_id":"coder-agent","input":"task"}`, i+1))
	}

	tests := []struct {
		name         string
		dispatchText string
		wantError    string
	}{
		{
			name:         "empty steps",
			dispatchText: `{"mode":"plan","steps":[]}`,
			wantError:    "must include at least one step",
		},
		{
			name:         "empty id",
			dispatchText: `{"mode":"plan","steps":[{"id":"","agent_id":"coder-agent","input":"task"}]}`,
			wantError:    "missing id",
		},
		{
			name:         "empty input",
			dispatchText: `{"mode":"plan","steps":[{"id":"code","agent_id":"coder-agent","input":""}]}`,
			wantError:    `step "code" missing input`,
		},
		{
			name:         "too many steps",
			dispatchText: `{"mode":"plan","steps":[` + strings.Join(longSteps, ",") + `]}`,
			wantError:    "max 6",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			runner, _ := newTestRunner(t, []AgentConfig{
				{
					ID:   "local-master",
					Name: "本地主控",
					Role: AgentRoleMaster,
					Children: []ChildAgentRef{{
						AgentID: "coder-agent",
						Name:    "代码专家",
					}},
				},
				{ID: "coder-agent", Name: "代码专家", Role: AgentRoleSpecialist},
			})
			runner.client = &planOrchestrationClient{dispatchText: tt.dispatchText}

			_, err := runner.RunTurn(context.Background(), channels.InboundMessage{
				Text:    "复杂任务",
				Channel: "cli",
				PeerID:  "cli-user",
			}, gateway.Route{
				AgentID:    "local-master",
				SessionKey: "agent:local-master:cli:direct:cli-user",
				Channel:    "cli",
				PeerID:     "cli-user",
			})
			if err == nil {
				t.Fatal("expected invalid plan to fail")
			}
			if !strings.Contains(err.Error(), tt.wantError) {
				t.Fatalf("error = %v, want to contain %q", err, tt.wantError)
			}
		})
	}
}

func TestRunnerPlanStepSupportsChildToolLoop(t *testing.T) {
	manager, err := NewManager([]AgentConfig{
		{
			ID:   "local-master",
			Name: "本地主控",
			Role: AgentRoleMaster,
			Children: []ChildAgentRef{{
				AgentID: "coder-agent",
				Name:    "代码专家",
			}},
		},
		{ID: "coder-agent", Name: "代码专家", Role: AgentRoleSpecialist, Tools: []string{"record_tool"}},
	})
	if err != nil {
		t.Fatalf("new manager: %v", err)
	}
	registry := tools.NewRegistry()
	if err := registry.Register(runnerFakeTool{}); err != nil {
		t.Fatalf("register tool: %v", err)
	}
	if err := registry.SetAllowedTools("local-master", nil); err != nil {
		t.Fatalf("set master tools: %v", err)
	}
	if err := registry.SetAllowedTools("coder-agent", []string{"record_tool"}); err != nil {
		t.Fatalf("set coder tools: %v", err)
	}
	client := &planOrchestrationClient{
		dispatchText:  `{"mode":"plan","steps":[{"id":"code","agent_id":"coder-agent","input":"inspect files"}],"final_instruction":"summarize the implementation"}`,
		coderUsesTool: true,
	}
	runner := NewRunner(manager, client, registry, sessions.NewMemoryStore())

	out, err := runner.RunTurn(context.Background(), channels.InboundMessage{
		Text:    "先调用工具检查，再总结",
		Channel: "cli",
		PeerID:  "cli-user",
	}, gateway.Route{
		AgentID:    "local-master",
		SessionKey: "agent:local-master:cli:direct:cli-user",
		Channel:    "cli",
		PeerID:     "cli-user",
	})
	if err != nil {
		t.Fatalf("run turn: %v", err)
	}
	if out.Text != "final synthesis" {
		t.Fatalf("out text = %q", out.Text)
	}
	if len(client.answerRequests) != 2 {
		t.Fatalf("answer requests len = %d", len(client.answerRequests))
	}
	var foundToolResult bool
	for _, msg := range client.answerRequests[1].Messages {
		if msg.Role == "tool" && msg.Name == "record_tool" && msg.Content == "tool ok" {
			foundToolResult = true
		}
	}
	if !foundToolResult {
		t.Fatalf("tool result was not passed back to child: %#v", client.answerRequests[1].Messages)
	}
	if len(client.synthesizeRequests) != 1 {
		t.Fatalf("synthesize requests len = %d", len(client.synthesizeRequests))
	}
	synthesisInput := client.synthesizeRequests[0].Messages[0].Content
	if !strings.Contains(synthesisInput, "coder used tool") {
		t.Fatalf("synthesis input missing child final output: %s", synthesisInput)
	}
}

func TestNewManagerRejectsUnknownChildAgent(t *testing.T) {
	_, err := NewManager([]AgentConfig{
		{
			ID:   "local-master",
			Role: AgentRoleMaster,
			Children: []ChildAgentRef{{
				AgentID: "missing-agent",
			}},
		},
	})
	if err == nil {
		t.Fatal("expected unknown child to fail")
	}
}
