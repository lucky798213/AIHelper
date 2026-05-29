package resilience

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"AIHelper/internal/llm"
)

const (
	DefaultContextSafeTokens = 180000
	DefaultMaxToolOutput     = 50000
)

type ContextGuard struct {
	ContextSafeTokens  int
	MaxToolOutputChars int
}

func NewContextGuard(contextSafeTokens, maxToolOutputChars int) ContextGuard {
	if contextSafeTokens <= 0 {
		contextSafeTokens = DefaultContextSafeTokens
	}
	if maxToolOutputChars <= 0 {
		maxToolOutputChars = DefaultMaxToolOutput
	}
	return ContextGuard{
		ContextSafeTokens:  contextSafeTokens,
		MaxToolOutputChars: maxToolOutputChars,
	}
}

func (g ContextGuard) EstimateTokens(text string) int {
	return len(text) / 4
}

func (g ContextGuard) EstimateMessagesTokens(messages []llm.Message) int {
	total := 0
	for _, msg := range messages {
		total += g.EstimateTokens(msg.Content)
		total += g.EstimateTokens(msg.ReasoningContent)
		for _, call := range msg.ToolCalls {
			total += g.EstimateTokens(call.Name)
			total += g.EstimateTokens(string(call.Input))
		}
	}
	return total
}

// 工具返回的内容太长（超过设定的最大字数），就把内容截短；
// 截短后，末尾加一行提示，告诉别人：原文总共有多少字，只展示了前多少字；
func (g ContextGuard) TruncateToolResults(messages []llm.Message) []llm.Message {
	copied := cloneMessages(messages)
	for i := range copied {
		if copied[i].Role != "tool" || len(copied[i].Content) <= g.MaxToolOutputChars {
			continue
		}
		originalLen := len(copied[i].Content)
		copied[i].Content = copied[i].Content[:g.MaxToolOutputChars] +
			fmt.Sprintf("\n\n[... truncated (%d chars total, showing first %d) ...]",
				originalLen, g.MaxToolOutputChars)
	}
	return copied
}

func (g ContextGuard) CompactHistory(ctx context.Context, client llm.Client, req llm.Request) []llm.Message {
	messages := req.Messages
	total := len(messages)

	//信息太少就不用压缩，直接返回
	if total <= 4 {
		return cloneMessages(messages)
	}

	//keepCount 表示至少要保留多少条最近消息不动。
	keepCount := maxInt(4, total/5)

	//compressCount 表示准备拿前多少条旧消息去总结。
	compressCount := maxInt(2, total/2)

	//压缩掉的消息不能太多，要保证后面至少还剩 keepCount 条近期消息。
	if limit := total - keepCount; compressCount > limit {
		compressCount = limit
	}

	//如果能压缩的消息太少，就放弃
	if compressCount < 2 {
		return cloneMessages(messages)
	}

	// 切分旧消息和近期消息
	oldMessages := messages[:compressCount]
	recentMessages := cloneMessages(messages[compressCount:])

	// 构造总结 prompt，这里把旧消息通过 flattenMessages 转成普通文本，然后要求模型总结。
	summaryPrompt := "Summarize the following conversation concisely, preserving key facts and decisions. Output only the summary, no preamble.\n\n" +
		flattenMessages(oldMessages)

	//调用 LLM 总结旧消息
	summaryResp, err := client.CreateMessage(ctx, llm.Request{
		AgentID:   req.AgentID,
		AgentRole: req.AgentRole,
		Purpose:   "resilience_compact",
		Model:     req.Model,
		System:    "You are a conversation summarizer. Be concise and factual.",
		Messages: []llm.Message{{
			Role:    "user",
			Content: summaryPrompt,
		}},
	})

	//如果总结调用失败，或者模型返回空字符串，就直接丢掉旧消息，只返回近期消息。
	//这是一个重要细节：这里会丢失 oldMessages。
	if err != nil || strings.TrimSpace(summaryResp.Text) == "" {
		return recentMessages
	}

	//把摘要伪装成一轮对话
	//表示 assistant 已经理解这段摘要。
	//为什么要伪装成 user + assistant 两条？因为很多 chat 模型更适应交替的对话结构。这样后续模型看到的是：
	compacted := []llm.Message{{
		Role:    "user",
		Content: "[Previous conversation summary]\n" + strings.TrimSpace(summaryResp.Text),
	}, {
		Role:    "assistant",
		Content: "Understood, I have the context from our previous conversation.",
	}}
	return append(compacted, recentMessages...)
}

// 传入旧消息，将旧信息平展开，方便后续压缩
func flattenMessages(messages []llm.Message) string {
	var parts []string
	for _, msg := range messages {
		//把旧消息转化为
		//[user]: 帮我读一下 main.go
		//[assistant called read_file]: {"path":"main.go"}
		if strings.TrimSpace(msg.Content) != "" {
			parts = append(parts, fmt.Sprintf("[%s]: %s", msg.Role, msg.Content))
		}
		if strings.TrimSpace(msg.ReasoningContent) != "" {
			parts = append(parts, fmt.Sprintf("[%s reasoning]: %s", msg.Role, msg.ReasoningContent))
		}
		for _, call := range msg.ToolCalls {
			input := string(call.Input)
			if !json.Valid(call.Input) {
				input = fmt.Sprintf("%q", input)
			}
			//然后  想要调用tool的input 控制在 500字符以内
			if len(input) > 500 {
				input = input[:500] + "... [truncated]"
			}
			parts = append(parts, fmt.Sprintf("[%s called %s]: %s", msg.Role, call.Name, input))
		}
	}
	return strings.Join(parts, "\n")
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
