package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"unicode"

	"AIHelper/internal/channels"
	"AIHelper/internal/gateway"
	"AIHelper/internal/llm"
	"AIHelper/internal/sessions"
	"AIHelper/internal/tools"
)

type Runner struct {
	manager       *Manager
	client        llm.Client
	registry      *tools.Registry
	sessions      sessions.Store
	promptBuilder PromptBuilder
}

const maxDispatchPlanSteps = 6

type DispatchDecision struct {
	Mode             string         `json:"mode"`
	AgentID          string         `json:"agent_id"`
	Input            string         `json:"input"`
	Steps            []DispatchStep `json:"steps"`
	FinalInstruction string         `json:"final_instruction"`
}

type DispatchStep struct {
	ID      string `json:"id"`
	AgentID string `json:"agent_id"`
	Input   string `json:"input"`
}

type planStepResult struct {
	ID      string
	AgentID string
	Input   string
	Output  string
}

func NewRunner(manager *Manager, client llm.Client, registry *tools.Registry, store sessions.Store) *Runner {
	return &Runner{
		manager:  manager,
		client:   client,
		registry: registry,
		sessions: store,
	}
}

func (r *Runner) SetPromptBuilder(builder PromptBuilder) {
	r.promptBuilder = builder
}

func (r *Runner) RunTurn(ctx context.Context, msg channels.InboundMessage, route gateway.Route) (channels.OutboundMessage, error) {
	//先得到 专门处理这条信息的 agentconfig
	cfg, ok := r.manager.Get(route.AgentID)
	if !ok {
		return channels.OutboundMessage{}, fmt.Errorf("agent %q not found", route.AgentID)
	}
	if cfg.Role != AgentRoleMaster {
		return channels.OutboundMessage{}, fmt.Errorf("route agent %q must be a master agent", cfg.ID)
	}

	//加载当轮对话的历史信息
	history, err := r.sessions.Load(ctx, route.SessionKey)
	if err != nil {
		return channels.OutboundMessage{}, err
	}

	//将用户的 msg 转化成 llm.Message格式，然后保存进到 sessions
	userMessage := llm.Message{Role: "user", Content: msg.Text}
	if err := r.sessions.Append(ctx, route.SessionKey, userMessage); err != nil {
		return channels.OutboundMessage{}, err
	}
	history = append(history, userMessage)

	//这是主 agent 专门用来决定将这个任务讲给哪个 agent 执行
	decision, err := r.dispatch(ctx, cfg, msg.Text)
	if err != nil {
		return channels.OutboundMessage{}, err
	}

	//执行主 agent 的决定
	assistantMessage, err := r.runDispatchDecision(ctx, cfg, route, history, decision)
	if err != nil {
		return channels.OutboundMessage{}, err
	}
	if err := r.sessions.Append(ctx, route.SessionKey, assistantMessage); err != nil {
		return channels.OutboundMessage{}, err
	}

	//返回结果
	return channels.OutboundMessage{
		Channel: route.Channel,
		To:      route.PeerID,
		ToType:  msg.ReplyToType,
		Text:    assistantMessage.Content,
	}, nil
}

func (r *Runner) dispatch(ctx context.Context, cfg AgentConfig, input string) (DispatchDecision, error) {
	//调用模型的 api，得到结果
	resp, err := r.client.CreateMessage(ctx, llm.Request{
		AgentID:   cfg.ID,
		AgentRole: string(cfg.Role),
		Purpose:   "dispatch",
		Model:     cfg.Model,
		System:    cfg.DispatchPrompt(),
		Messages: []llm.Message{{
			Role:    "user",
			Content: input,
		}},
	})
	if err != nil {
		return DispatchDecision{}, err
	}
	if resp.StopReason != llm.StopReasonEndTurn {
		return DispatchDecision{}, fmt.Errorf("dispatch agent %q returned unsupported stop reason %q", cfg.ID, resp.StopReason)
	}

	var decision DispatchDecision
	if err := json.Unmarshal([]byte(resp.Text), &decision); err != nil {
		return DispatchDecision{}, fmt.Errorf("decode dispatch decision from agent %q: %w; raw=%q", cfg.ID, err, resp.Text)
	}
	decision.Mode = strings.ToLower(strings.TrimSpace(decision.Mode))
	decision.AgentID = strings.TrimSpace(decision.AgentID)
	decision.Input = strings.TrimSpace(decision.Input)
	decision.FinalInstruction = strings.TrimSpace(decision.FinalInstruction)
	for i := range decision.Steps {
		decision.Steps[i].ID = strings.TrimSpace(decision.Steps[i].ID)
		decision.Steps[i].AgentID = strings.TrimSpace(decision.Steps[i].AgentID)
		decision.Steps[i].Input = strings.TrimSpace(decision.Steps[i].Input)
	}
	if decision.Mode != "plan" && decision.Input == "" {
		decision.Input = input
	}
	switch decision.Mode {
	case "direct":
		decision.AgentID = ""
	case "delegate":
		if decision.AgentID == "" {
			return DispatchDecision{}, fmt.Errorf("dispatch decision from agent %q missing agent_id", cfg.ID)
		}
	case "plan":
		if err := r.validatePlanDecision(cfg, decision); err != nil {
			return DispatchDecision{}, err
		}
	default:
		return DispatchDecision{}, fmt.Errorf("dispatch decision from agent %q has unsupported mode %q", cfg.ID, decision.Mode)
	}
	return decision, nil
}

// direct 是主 agent 自己回答用户问题，delegate 是交个一个具体的子 agent 进行处理
func (r *Runner) runDispatchDecision(ctx context.Context, master AgentConfig, route gateway.Route, masterHistory []llm.Message, decision DispatchDecision) (llm.Message, error) {
	switch decision.Mode {
	case "direct":
		return r.runAgent(ctx, master, route.SessionKey, masterHistory, route.Channel)
	case "delegate":
		return r.runChildAgent(ctx, master, route, decision)
	case "plan":
		return r.runPlan(ctx, master, route, masterHistory, decision)
	default:
		return llm.Message{}, fmt.Errorf("unsupported dispatch mode %q", decision.Mode)
	}
}

func (r *Runner) runChildAgent(ctx context.Context, master AgentConfig, route gateway.Route, decision DispatchDecision) (llm.Message, error) {
	sessionKey := fmt.Sprintf("agent:%s:parent:%s:%s:%s", decision.AgentID, master.ID, route.Channel, route.PeerID)
	return r.runChildAgentWithInput(ctx, master, route, decision.AgentID, decision.Input, sessionKey)
}

func (r *Runner) runChildAgentWithInput(ctx context.Context, master AgentConfig, route gateway.Route, agentID string, input string, sessionKey string) (llm.Message, error) {
	//先检查一下这个decision.AgentID是不是 master 的子 agent
	if !r.manager.CanDelegate(master.ID, agentID) {
		return llm.Message{}, fmt.Errorf("agent %q cannot dispatch to %q", master.ID, agentID)
	}

	//拿到 child agent 配置
	child, ok := r.manager.Get(agentID)
	if !ok {
		return llm.Message{}, fmt.Errorf("child agent %q not found", agentID)
	}

	//加载 child agent 的历史
	history, err := r.sessions.Load(ctx, sessionKey)
	if err != nil {
		return llm.Message{}, err
	}

	//把 master 转交的 input 当成 child 的 user message
	userMessage := llm.Message{Role: "user", Content: input}

	//保存 child 的 user message，并追加到本轮 history
	if err := r.sessions.Append(ctx, sessionKey, userMessage); err != nil {
		return llm.Message{}, err
	}
	history = append(history, userMessage)

	//真正运行 child agent
	outputMessage, err := r.runAgent(ctx, child, sessionKey, history, route.Channel)
	if err != nil {
		return llm.Message{}, err
	}

	//保存 child 的输出
	if err := r.sessions.Append(ctx, sessionKey, outputMessage); err != nil {
		return llm.Message{}, err
	}

	//把 child 输出包装成 assistant message 返回给 master 流程
	return llm.Message{Role: "assistant", Content: outputMessage.Content}, nil
}

func (r *Runner) runPlan(ctx context.Context, master AgentConfig, route gateway.Route, masterHistory []llm.Message, decision DispatchDecision) (llm.Message, error) {
	if err := r.validatePlanDecision(master, decision); err != nil {
		return llm.Message{}, err
	}

	results := make([]planStepResult, 0, len(decision.Steps))
	for _, step := range decision.Steps {
		sessionKey := fmt.Sprintf(
			"agent:%s:parent:%s:%s:%s:step:%s",
			step.AgentID,
			master.ID,
			route.Channel,
			route.PeerID,
			safeSessionComponent(step.ID),
		)
		outputMessage, err := r.runChildAgentWithInput(ctx, master, route, step.AgentID, step.Input, sessionKey)
		if err != nil {
			return llm.Message{}, fmt.Errorf("run plan step %q with agent %q: %w", step.ID, step.AgentID, err)
		}
		results = append(results, planStepResult{
			ID:      step.ID,
			AgentID: step.AgentID,
			Input:   step.Input,
			Output:  outputMessage.Content,
		})
	}

	return r.synthesizePlan(ctx, master, route.Channel, masterHistory, decision, results)
}

func (r *Runner) validatePlanDecision(master AgentConfig, decision DispatchDecision) error {
	if len(decision.Steps) == 0 {
		return fmt.Errorf("dispatch plan from agent %q must include at least one step", master.ID)
	}
	if len(decision.Steps) > maxDispatchPlanSteps {
		return fmt.Errorf("dispatch plan from agent %q has %d steps, max %d", master.ID, len(decision.Steps), maxDispatchPlanSteps)
	}
	for i, step := range decision.Steps {
		if step.ID == "" {
			return fmt.Errorf("dispatch plan from agent %q step %d missing id", master.ID, i+1)
		}
		if step.AgentID == "" {
			return fmt.Errorf("dispatch plan from agent %q step %q missing agent_id", master.ID, step.ID)
		}
		if step.Input == "" {
			return fmt.Errorf("dispatch plan from agent %q step %q missing input", master.ID, step.ID)
		}
		if !r.manager.CanDelegate(master.ID, step.AgentID) {
			return fmt.Errorf("agent %q cannot dispatch plan step %q to %q", master.ID, step.ID, step.AgentID)
		}
	}
	return nil
}

func (r *Runner) synthesizePlan(ctx context.Context, master AgentConfig, channel string, masterHistory []llm.Message, decision DispatchDecision, results []planStepResult) (llm.Message, error) {
	userInput := latestUserInput(masterHistory)
	systemPrompt, err := r.buildSystemPrompt(ctx, master, userInput, channel)
	if err != nil {
		return llm.Message{}, err
	}
	resp, err := r.client.CreateMessage(ctx, llm.Request{
		AgentID:   master.ID,
		AgentRole: string(master.Role),
		Purpose:   "synthesize",
		Model:     master.Model,
		System:    systemPrompt,
		Messages: []llm.Message{{
			Role:    "user",
			Content: formatPlanSynthesisInput(userInput, decision, results),
		}},
	})
	if err != nil {
		return llm.Message{}, err
	}
	if resp.StopReason != llm.StopReasonEndTurn {
		return llm.Message{}, fmt.Errorf("synthesize agent %q returned unsupported stop reason %q", master.ID, resp.StopReason)
	}
	return llm.Message{Role: "assistant", Content: resp.Text, ReasoningContent: resp.ReasoningContent}, nil
}

func (r *Runner) runAgent(ctx context.Context, cfg AgentConfig, sessionKey string, messages []llm.Message, channel string) (llm.Message, error) {
	current := append([]llm.Message(nil), messages...)
	for i := 0; i < 5; i++ {
		//构建最基础的SystemPrompt，把该 agent 配置文件中的 description 和有关子 agent 的描述拼接起来
		systemPrompt, err := r.buildSystemPrompt(ctx, cfg, latestUserInput(current), channel)
		if err != nil {
			return llm.Message{}, err
		}
		resp, err := r.client.CreateMessage(ctx, llm.Request{
			AgentID:   cfg.ID,
			AgentRole: string(cfg.Role),
			Purpose:   "answer",
			Model:     cfg.Model,
			System:    systemPrompt,
			Messages:  current,
			Tools:     r.registry.SchemasForAgent(cfg.ID),
		})
		if err != nil {
			return llm.Message{}, err
		}

		switch resp.StopReason {
		case llm.StopReasonEndTurn:
			return llm.Message{Role: "assistant", Content: resp.Text, ReasoningContent: resp.ReasoningContent}, nil
		case llm.StopReasonToolUse:
			assistantToolMessage := llm.Message{
				Role:             "assistant",
				Content:          resp.Text,
				ReasoningContent: resp.ReasoningContent,
				ToolCalls:        resp.ToolCalls,
			}
			current = append(current, assistantToolMessage)
			if err := r.sessions.Append(ctx, sessionKey, assistantToolMessage); err != nil {
				return llm.Message{}, err
			}

			for _, toolCall := range resp.ToolCalls {
				tool, ok := r.registry.GetForAgent(cfg.ID, toolCall.Name)
				if !ok {
					return llm.Message{}, fmt.Errorf("tool %q is not available for agent %q", toolCall.Name, cfg.ID)
				}
				result, err := tool.Call(ctx, tools.CallContext{
					AgentID:    cfg.ID,
					SessionKey: sessionKey,
				}, toolCall.Input)
				if err != nil {
					return llm.Message{}, err
				}
				toolMessage := llm.Message{
					Role:       "tool",
					Name:       toolCall.Name,
					Content:    result.Content,
					ToolCallID: toolCall.ID,
				}
				current = append(current, toolMessage)
				if err := r.sessions.Append(ctx, sessionKey, toolMessage); err != nil {
					return llm.Message{}, err
				}
			}
		default:
			return llm.Message{}, fmt.Errorf("unsupported stop reason %q", resp.StopReason)
		}
	}
	return llm.Message{}, fmt.Errorf("agent %q exceeded tool loop limit", cfg.ID)
}

func (r *Runner) buildSystemPrompt(ctx context.Context, cfg AgentConfig, userInput string, channel string) (string, error) {
	systemPrompt := cfg.SystemPrompt()
	if r.promptBuilder == nil {
		return systemPrompt, nil
	}
	return r.promptBuilder.BuildSystemPrompt(ctx, PromptBuildRequest{
		Agent:      cfg,
		UserInput:  userInput,
		Channel:    channel,
		Model:      cfg.Model,
		BasePrompt: systemPrompt,
	})
}

func formatPlanSynthesisInput(userInput string, decision DispatchDecision, results []planStepResult) string {
	finalInstruction := decision.FinalInstruction
	if finalInstruction == "" {
		finalInstruction = "Synthesize the specialist results into one final answer for the user."
	}

	var b strings.Builder
	b.WriteString("Original user request:\n")
	b.WriteString(userInput)
	b.WriteString("\n\nFinal instruction:\n")
	b.WriteString(finalInstruction)
	b.WriteString("\n\nCompleted specialist steps:")
	for i, result := range results {
		fmt.Fprintf(&b, "\n\n%d. Step %s assigned to %s\nInput:\n%s\nOutput:\n%s", i+1, result.ID, result.AgentID, result.Input, result.Output)
	}
	return b.String()
}

func safeSessionComponent(value string) string {
	var b strings.Builder
	for _, r := range strings.TrimSpace(value) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) || r == '-' || r == '_' || r == '.' {
			b.WriteRune(r)
			continue
		}
		b.WriteRune('-')
	}
	result := strings.Trim(b.String(), "-_.")
	if result == "" {
		return "step"
	}
	return result
}

func latestUserInput(messages []llm.Message) string {
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role == "user" {
			return messages[i].Content
		}
	}
	return ""
}
