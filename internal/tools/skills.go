package tools

import (
	"context"
	"encoding/json"
	"fmt"

	"AIHelper/internal/llm"
)

type SkillService interface {
	ReadSkillReference(ctx context.Context, agentID, skillName, path string) (string, error)
	RunSkillCommand(ctx context.Context, agentID, skillName, command string) (string, error)
}

type ReadSkillReferenceTool struct {
	service SkillService
}

type readSkillReferenceInput struct {
	SkillName string `json:"skill_name"`
	Path      string `json:"path"`
}

func NewReadSkillReferenceTool(service SkillService) *ReadSkillReferenceTool {
	return &ReadSkillReferenceTool{service: service}
}

func NewReadSkillReferenceToolFactory(deps Dependencies) (Tool, error) {
	//SkillService 由 intelligence.Service 提供。
	//测试或轻量运行场景没有 SkillService 时，registry 会跳过 skill 工具。
	if deps.SkillService == nil {
		return nil, ErrToolUnavailable
	}
	return NewReadSkillReferenceTool(deps.SkillService), nil
}

func (t *ReadSkillReferenceTool) Name() string {
	return "read_skill_reference"
}

func (t *ReadSkillReferenceTool) Schema() llm.ToolSchema {
	return llm.ToolSchema{
		Name:        t.Name(),
		Description: "Read a Markdown reference file from an active skill directory. Use only when the active SKILL.md tells you to read that reference.",
		InputSchema: map[string]any{
			"type":     "object",
			"required": []string{"skill_name", "path"},
			"properties": map[string]any{
				"skill_name": map[string]any{
					"type":        "string",
					"description": "Name of the enabled skill.",
				},
				"path": map[string]any{
					"type":        "string",
					"description": "Relative Markdown reference path inside the skill directory.",
				},
			},
		},
	}
}

func (t *ReadSkillReferenceTool) Call(ctx context.Context, call CallContext, raw json.RawMessage) (ToolResult, error) {
	//工具层只负责 JSON 参数解析和 agentID 透传；
	//路径、扩展名、symlink 等安全校验集中放在 intelligence.Service。
	var input readSkillReferenceInput
	if err := json.Unmarshal(raw, &input); err != nil {
		return ToolResult{}, fmt.Errorf("decode read_skill_reference input: %w", err)
	}
	result, err := t.service.ReadSkillReference(ctx, call.AgentID, input.SkillName, input.Path)
	if err != nil {
		return ToolResult{}, err
	}
	return ToolResult{Content: result}, nil
}

type RunSkillCommandTool struct {
	service SkillService
}

type runSkillCommandInput struct {
	SkillName string `json:"skill_name"`
	Command   string `json:"command"`
}

func NewRunSkillCommandTool(service SkillService) *RunSkillCommandTool {
	return &RunSkillCommandTool{service: service}
}

func NewRunSkillCommandToolFactory(deps Dependencies) (Tool, error) {
	//命令执行能力依赖 SkillService；没有服务时不注册，避免暴露空工具。
	if deps.SkillService == nil {
		return nil, ErrToolUnavailable
	}
	return NewRunSkillCommandTool(deps.SkillService), nil
}

func (t *RunSkillCommandTool) Name() string {
	return "run_skill_command"
}

func (t *RunSkillCommandTool) Schema() llm.ToolSchema {
	return llm.ToolSchema{
		Name:        t.Name(),
		Description: "Run a shell command that appears verbatim in an active skill's SKILL.md. The command runs with the skill directory as the working directory.",
		InputSchema: map[string]any{
			"type":     "object",
			"required": []string{"skill_name", "command"},
			"properties": map[string]any{
				"skill_name": map[string]any{
					"type":        "string",
					"description": "Name of the enabled skill.",
				},
				"command": map[string]any{
					"type":        "string",
					"description": "Shell command copied exactly from the active SKILL.md.",
				},
			},
		},
	}
}

func (t *RunSkillCommandTool) Call(ctx context.Context, call CallContext, raw json.RawMessage) (ToolResult, error) {
	//这里不直接执行 shell。
	//真正的命令白名单、工作目录、超时和输出截断都由 intelligence.Service 统一处理。
	var input runSkillCommandInput
	if err := json.Unmarshal(raw, &input); err != nil {
		return ToolResult{}, fmt.Errorf("decode run_skill_command input: %w", err)
	}
	result, err := t.service.RunSkillCommand(ctx, call.AgentID, input.SkillName, input.Command)
	if err != nil {
		return ToolResult{}, err
	}
	return ToolResult{Content: result}, nil
}
