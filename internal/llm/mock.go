package llm

import (
	"context"
	"fmt"
	"strings"
)

type MockClient struct{}

func NewMockClient() *MockClient {
	return &MockClient{}
}

func (c *MockClient) CreateMessage(ctx context.Context, req Request) (Response, error) {
	select {
	case <-ctx.Done():
		return Response{}, ctx.Err()
	default:
	}

	if len(req.Messages) == 0 {
		return Response{StopReason: StopReasonEndTurn, Text: "我还没有收到消息。"}, nil
	}

	last := req.Messages[len(req.Messages)-1]
	if req.Purpose == "dispatch" {
		return Response{
			StopReason: StopReasonEndTurn,
			Text:       mockDispatchDecision(last.Content),
		}, nil
	}
	if req.Purpose == "skill_select" {
		return Response{
			StopReason: StopReasonEndTurn,
			Text:       `{"skills":[]}`,
		}, nil
	}

	if req.AgentRole == "specialist" {
		return Response{
			StopReason: StopReasonEndTurn,
			Text:       fmt.Sprintf("[%s] 已处理任务：%s", req.AgentID, last.Content),
		}, nil
	}

	return Response{
		StopReason: StopReasonEndTurn,
		Text:       fmt.Sprintf("[%s] 我可以直接处理这条消息：%s", req.AgentID, last.Content),
	}, nil
}

func pickChildAgent(input string) string {
	lower := strings.ToLower(input)
	codeKeywords := []string{"go", "代码", "项目结构", "架构", "debug", "实现"}
	for _, keyword := range codeKeywords {
		if strings.Contains(lower, strings.ToLower(keyword)) {
			return "coder-agent"
		}
	}

	writerKeywords := []string{"文档", "总结", "润色", "表达", "写作"}
	for _, keyword := range writerKeywords {
		if strings.Contains(lower, strings.ToLower(keyword)) {
			return "writer-agent"
		}
	}
	return ""
}

func mockDispatchDecision(input string) string {
	agentID := pickChildAgent(input)
	if agentID == "" {
		return fmt.Sprintf(`{"mode":"direct","input":%q}`, input)
	}
	return fmt.Sprintf(`{"mode":"delegate","agent_id":%q,"input":%q}`, agentID, buildDelegateInput(input, agentID))
}

func buildDelegateInput(userInput, childAgentID string) string {
	switch childAgentID {
	case "coder-agent":
		return "请从 Go 工程实现角度处理这个任务：" + userInput
	case "writer-agent":
		return "请从文档和表达角度处理这个任务：" + userInput
	default:
		return userInput
	}
}
