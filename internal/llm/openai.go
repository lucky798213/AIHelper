package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const defaultOpenAIBaseURL = "https://api.openai.com/v1"

type OpenAIConfig struct {
	BaseURL      string
	APIKey       string
	DefaultModel string
	Temperature  float64
	MaxTokens    int
	HTTPClient   *http.Client
}

type OpenAIClient struct {
	baseURL      string
	apiKey       string
	defaultModel string
	temperature  float64
	maxTokens    int
	httpClient   *http.Client
}

type HTTPError struct {
	StatusCode int
	Body       string
}

func (e *HTTPError) Error() string {
	return fmt.Sprintf("openai-compatible request failed: status=%d body=%s", e.StatusCode, e.Body)
}

func (e *HTTPError) HTTPStatusCode() int {
	return e.StatusCode
}

func NewOpenAIClient(cfg OpenAIConfig) (*OpenAIClient, error) {
	if strings.TrimSpace(cfg.APIKey) == "" {
		return nil, fmt.Errorf("openai-compatible api_key is required")
	}
	if strings.TrimSpace(cfg.DefaultModel) == "" {
		return nil, fmt.Errorf("openai-compatible default_model is required")
	}

	baseURL := strings.TrimRight(strings.TrimSpace(cfg.BaseURL), "/")
	if baseURL == "" {
		baseURL = defaultOpenAIBaseURL
	}

	maxTokens := cfg.MaxTokens
	if maxTokens <= 0 {
		maxTokens = 2048
	}

	httpClient := cfg.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 60 * time.Second}
	}

	return &OpenAIClient{
		baseURL:      baseURL,
		apiKey:       cfg.APIKey,
		defaultModel: cfg.DefaultModel,
		temperature:  cfg.Temperature,
		maxTokens:    maxTokens,
		httpClient:   httpClient,
	}, nil
}

func (c *OpenAIClient) CreateMessage(ctx context.Context, req Request) (Response, error) {
	//1. 先确定使用哪个模型
	model := strings.TrimSpace(req.Model)
	if model == "" {
		model = c.defaultModel
	}

	//构造 OpenAI-compatible 请求体
	body := openAIChatRequest{
		Model:       model,
		Messages:    buildOpenAIMessages(req),
		Temperature: c.temperature,
		MaxTokens:   c.maxTokens,
	}

	//如果是dispatch，就规定 这个请求的返回的格式一定要是 合法的 JSON 对象
	if req.Purpose == "dispatch" || req.Purpose == "skill_select" {
		body.ResponseFormat = &openAIResponseFormat{Type: "json_object"}
	}
	if len(req.Tools) > 0 {
		body.Tools = buildOpenAITools(req.Tools)
		body.ToolChoice = "auto"
	}

	rawBody, err := json.Marshal(body)
	if err != nil {
		return Response{}, fmt.Errorf("marshal openai request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(
		ctx,
		http.MethodPost,
		c.baseURL+"/chat/completions",
		bytes.NewReader(rawBody),
	)
	if err != nil {
		return Response{}, err
	}
	httpReq.Header.Set("Authorization", "Bearer "+c.apiKey)
	httpReq.Header.Set("Content-Type", "application/json")

	httpResp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return Response{}, err
	}
	defer httpResp.Body.Close()

	respBody, err := io.ReadAll(io.LimitReader(httpResp.Body, 1<<20))
	if err != nil {
		return Response{}, err
	}
	if httpResp.StatusCode < http.StatusOK || httpResp.StatusCode >= http.StatusMultipleChoices {
		return Response{}, &HTTPError{
			StatusCode: httpResp.StatusCode,
			Body:       string(respBody),
		}
	}

	var decoded openAIChatResponse
	if err := json.Unmarshal(respBody, &decoded); err != nil {
		return Response{}, fmt.Errorf("decode openai response: %w", err)
	}
	if len(decoded.Choices) == 0 {
		return Response{}, fmt.Errorf("openai-compatible response has no choices")
	}

	choice := decoded.Choices[0]
	switch choice.FinishReason {
	case "tool_calls":
		return Response{
			StopReason:       StopReasonToolUse,
			Text:             choice.Message.Content,
			ReasoningContent: choice.Message.ReasoningContent,
			ToolCalls:        parseOpenAIToolCalls(choice.Message.ToolCalls),
		}, nil
	case "stop", "":
		return Response{
			StopReason:       StopReasonEndTurn,
			Text:             choice.Message.Content,
			ReasoningContent: choice.Message.ReasoningContent,
		}, nil
	default:
		return Response{
			StopReason:       StopReasonEndTurn,
			Text:             choice.Message.Content,
			ReasoningContent: choice.Message.ReasoningContent,
		}, nil
	}
}

type openAIChatRequest struct {
	Model          string                `json:"model"`
	Messages       []openAIMessage       `json:"messages"`
	Tools          []openAITool          `json:"tools,omitempty"`
	ToolChoice     string                `json:"tool_choice,omitempty"`
	ResponseFormat *openAIResponseFormat `json:"response_format,omitempty"`
	Temperature    float64               `json:"temperature,omitempty"`
	MaxTokens      int                   `json:"max_tokens,omitempty"`
}

type openAIResponseFormat struct {
	Type string `json:"type"`
}

type openAIMessage struct {
	Role             string           `json:"role"`
	Content          string           `json:"content,omitempty"`
	ReasoningContent string           `json:"reasoning_content,omitempty"`
	Name             string           `json:"name,omitempty"`
	ToolCallID       string           `json:"tool_call_id,omitempty"`
	ToolCalls        []openAIToolCall `json:"tool_calls,omitempty"`
}

type openAITool struct {
	Type     string         `json:"type"`
	Function openAIFunction `json:"function"`
}

type openAIFunction struct {
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	Parameters  map[string]any `json:"parameters"`
}

type openAIToolCall struct {
	ID       string             `json:"id,omitempty"`
	Type     string             `json:"type,omitempty"`
	Function openAIToolFunction `json:"function"`
}

type openAIToolFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type openAIChatResponse struct {
	Choices []openAIChoice `json:"choices"`
}

type openAIChoice struct {
	FinishReason string        `json:"finish_reason"`
	Message      openAIMessage `json:"message"`
}

func buildOpenAIMessages(req Request) []openAIMessage {
	messages := make([]openAIMessage, 0, len(req.Messages)+1)
	if strings.TrimSpace(req.System) != "" {
		messages = append(messages, openAIMessage{
			Role:    "system",
			Content: req.System,
		})
	}
	for _, msg := range req.Messages {
		messages = append(messages, openAIMessage{
			Role:             msg.Role,
			Content:          msg.Content,
			ReasoningContent: msg.ReasoningContent,
			Name:             msg.Name,
			ToolCallID:       msg.ToolCallID,
			ToolCalls:        buildOpenAIToolCalls(msg.ToolCalls),
		})
	}
	return messages
}

func buildOpenAITools(schemas []ToolSchema) []openAITool {
	tools := make([]openAITool, 0, len(schemas))
	for _, schema := range schemas {
		tools = append(tools, openAITool{
			Type: "function",
			Function: openAIFunction{
				Name:        schema.Name,
				Description: schema.Description,
				Parameters:  schema.InputSchema,
			},
		})
	}
	return tools
}

func buildOpenAIToolCalls(calls []ToolCall) []openAIToolCall {
	if len(calls) == 0 {
		return nil
	}
	converted := make([]openAIToolCall, 0, len(calls))
	for _, call := range calls {
		converted = append(converted, openAIToolCall{
			ID:   call.ID,
			Type: "function",
			Function: openAIToolFunction{
				Name:      call.Name,
				Arguments: string(call.Input),
			},
		})
	}
	return converted
}

func parseOpenAIToolCalls(calls []openAIToolCall) []ToolCall {
	parsed := make([]ToolCall, 0, len(calls))
	for _, call := range calls {
		input := json.RawMessage(call.Function.Arguments)
		if len(input) == 0 {
			input = json.RawMessage(`{}`)
		}
		parsed = append(parsed, ToolCall{
			ID:    call.ID,
			Name:  call.Function.Name,
			Input: input,
		})
	}
	return parsed
}
