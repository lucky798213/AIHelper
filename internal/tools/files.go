package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"AIHelper/internal/llm"
)

const (
	defaultMaxReadBytes  = 64 * 1024
	defaultMaxWriteBytes = 256 * 1024
)

type fileToolBase struct {
	baseDir string
}

func newFileToolBase(baseDir string) (fileToolBase, error) {
	if strings.TrimSpace(baseDir) == "" {
		baseDir = "."
	}
	abs, err := filepath.Abs(baseDir)
	if err != nil {
		return fileToolBase{}, err
	}
	return fileToolBase{baseDir: filepath.Clean(abs)}, nil
}

func (b fileToolBase) resolve(relPath string) (string, string, error) {
	trimmed := strings.TrimSpace(relPath)
	if trimmed == "" {
		return "", "", fmt.Errorf("path is required")
	}
	if filepath.IsAbs(trimmed) {
		return "", "", fmt.Errorf("path must be relative to workspace")
	}

	cleaned := filepath.Clean(trimmed)
	if cleaned == ".." || strings.HasPrefix(cleaned, ".."+string(os.PathSeparator)) {
		return "", "", fmt.Errorf("path escapes workspace")
	}

	target := filepath.Clean(filepath.Join(b.baseDir, cleaned))
	if !isPathInside(b.baseDir, target) {
		return "", "", fmt.Errorf("path escapes workspace")
	}
	return target, cleaned, nil
}

func (b fileToolBase) ensureSafeParent(target string) error {
	parent := filepath.Dir(target)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return err
	}

	realBase, err := filepath.EvalSymlinks(b.baseDir)
	if err != nil {
		return err
	}
	realParent, err := filepath.EvalSymlinks(parent)
	if err != nil {
		return err
	}
	if !isPathInside(realBase, realParent) {
		return fmt.Errorf("path escapes workspace through symlink")
	}
	return nil
}

func rejectSymlink(path string) error {
	info, err := os.Lstat(path)
	if err == nil && info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("refusing to access symlink %q", path)
	}
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

func isPathInside(base, target string) bool {
	rel, err := filepath.Rel(base, target)
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(os.PathSeparator)))
}

type WriteFileTool struct {
	fileToolBase
	maxBytes int
}

type writeFileInput struct {
	Path    string `json:"path"`
	Content string `json:"content"`
	Append  bool   `json:"append"`
}

func NewWriteFileTool(baseDir string) (*WriteFileTool, error) {
	base, err := newFileToolBase(baseDir)
	if err != nil {
		return nil, err
	}
	return &WriteFileTool{fileToolBase: base, maxBytes: defaultMaxWriteBytes}, nil
}

func NewWriteFileToolFactory(deps Dependencies) (Tool, error) {
	return NewWriteFileTool(deps.BaseDir)
}

func (t *WriteFileTool) Name() string {
	return "write_file"
}

func (t *WriteFileTool) Schema() llm.ToolSchema {
	return llm.ToolSchema{
		Name:        t.Name(),
		Description: "Write UTF-8 text to a relative file path inside the workspace. Use append=true to append instead of replacing the file.",
		InputSchema: map[string]any{
			"type":     "object",
			"required": []string{"path", "content"},
			"properties": map[string]any{
				"path": map[string]any{
					"type":        "string",
					"description": "Relative path inside the workspace, for example tmp/ai-test.txt.",
				},
				"content": map[string]any{
					"type":        "string",
					"description": "Text content to write.",
				},
				"append": map[string]any{
					"type":        "boolean",
					"description": "Append to the file when true; replace it when false or omitted.",
				},
			},
		},
	}
}

func (t *WriteFileTool) Call(ctx context.Context, call CallContext, raw json.RawMessage) (ToolResult, error) {
	select {
	case <-ctx.Done():
		return ToolResult{}, ctx.Err()
	default:
	}

	var input writeFileInput
	if err := json.Unmarshal(raw, &input); err != nil {
		return ToolResult{}, fmt.Errorf("decode write_file input: %w", err)
	}
	target, cleaned, err := t.resolve(input.Path)
	if err != nil {
		return ToolResult{}, err
	}
	if len([]byte(input.Content)) > t.maxBytes {
		return ToolResult{}, fmt.Errorf("content is too large: %d bytes exceeds %d", len([]byte(input.Content)), t.maxBytes)
	}
	if err := t.ensureSafeParent(target); err != nil {
		return ToolResult{}, err
	}
	if err := rejectSymlink(target); err != nil {
		return ToolResult{}, err
	}

	flag := os.O_CREATE | os.O_WRONLY | os.O_TRUNC
	action := "wrote"
	if input.Append {
		flag = os.O_CREATE | os.O_WRONLY | os.O_APPEND
		action = "appended"
	}
	file, err := os.OpenFile(target, flag, 0o644)
	if err != nil {
		return ToolResult{}, err
	}
	defer file.Close()

	n, err := file.WriteString(input.Content)
	if err != nil {
		return ToolResult{}, err
	}
	return ToolResult{Content: fmt.Sprintf("%s %d bytes to %s", action, n, cleaned)}, nil
}

type EditFileTool struct {
	fileToolBase
	maxBytes int
}

type editFileInput struct {
	Path       string `json:"path"`
	OldText    string `json:"old_text"`
	NewText    string `json:"new_text"`
	ReplaceAll bool   `json:"replace_all"`
}

func NewEditFileTool(baseDir string) (*EditFileTool, error) {
	base, err := newFileToolBase(baseDir)
	if err != nil {
		return nil, err
	}
	return &EditFileTool{fileToolBase: base, maxBytes: defaultMaxWriteBytes}, nil
}

func NewEditFileToolFactory(deps Dependencies) (Tool, error) {
	return NewEditFileTool(deps.BaseDir)
}

func (t *EditFileTool) Name() string {
	return "edit_file"
}

func (t *EditFileTool) Schema() llm.ToolSchema {
	return llm.ToolSchema{
		Name:        t.Name(),
		Description: "Edit a UTF-8 text file inside the workspace by replacing exact text. Fails when old_text is missing or ambiguous unless replace_all=true.",
		InputSchema: map[string]any{
			"type":     "object",
			"required": []string{"path", "old_text", "new_text"},
			"properties": map[string]any{
				"path": map[string]any{
					"type":        "string",
					"description": "Relative path inside the workspace.",
				},
				"old_text": map[string]any{
					"type":        "string",
					"description": "Exact text to replace.",
				},
				"new_text": map[string]any{
					"type":        "string",
					"description": "Replacement text.",
				},
				"replace_all": map[string]any{
					"type":        "boolean",
					"description": "Replace every occurrence when true. Defaults to false.",
				},
			},
		},
	}
}

func (t *EditFileTool) Call(ctx context.Context, call CallContext, raw json.RawMessage) (ToolResult, error) {
	select {
	case <-ctx.Done():
		return ToolResult{}, ctx.Err()
	default:
	}

	var input editFileInput
	if err := json.Unmarshal(raw, &input); err != nil {
		return ToolResult{}, fmt.Errorf("decode edit_file input: %w", err)
	}
	if input.OldText == "" {
		return ToolResult{}, fmt.Errorf("old_text is required")
	}
	target, cleaned, err := t.resolve(input.Path)
	if err != nil {
		return ToolResult{}, err
	}
	if err := rejectSymlink(target); err != nil {
		return ToolResult{}, err
	}
	rawContent, err := os.ReadFile(target)
	if err != nil {
		return ToolResult{}, err
	}
	content := string(rawContent)
	count := strings.Count(content, input.OldText)
	if count == 0 {
		return ToolResult{}, fmt.Errorf("old_text not found in %s", cleaned)
	}
	if count > 1 && !input.ReplaceAll {
		return ToolResult{}, fmt.Errorf("old_text appears %d times in %s; set replace_all=true to replace all occurrences", count, cleaned)
	}
	limit := 1
	if input.ReplaceAll {
		limit = -1
	}
	updated := strings.Replace(content, input.OldText, input.NewText, limit)
	if len([]byte(updated)) > t.maxBytes {
		return ToolResult{}, fmt.Errorf("edited content is too large: %d bytes exceeds %d", len([]byte(updated)), t.maxBytes)
	}
	if err := os.WriteFile(target, []byte(updated), 0o644); err != nil {
		return ToolResult{}, err
	}
	replaced := 1
	if input.ReplaceAll {
		replaced = count
	}
	return ToolResult{Content: fmt.Sprintf("replaced %d occurrence(s) in %s", replaced, cleaned)}, nil
}

type ReadFileTool struct {
	fileToolBase
	maxBytes int
}

type readFileInput struct {
	Path     string `json:"path"`
	MaxBytes int    `json:"max_bytes"`
}

func NewReadFileTool(baseDir string) (*ReadFileTool, error) {
	base, err := newFileToolBase(baseDir)
	if err != nil {
		return nil, err
	}
	return &ReadFileTool{fileToolBase: base, maxBytes: defaultMaxReadBytes}, nil
}

func NewReadFileToolFactory(deps Dependencies) (Tool, error) {
	return NewReadFileTool(deps.BaseDir)
}

func (t *ReadFileTool) Name() string {
	return "read_file"
}

func (t *ReadFileTool) Schema() llm.ToolSchema {
	return llm.ToolSchema{
		Name:        t.Name(),
		Description: "Read a UTF-8 text file from a relative path inside the workspace.",
		InputSchema: map[string]any{
			"type":     "object",
			"required": []string{"path"},
			"properties": map[string]any{
				"path": map[string]any{
					"type":        "string",
					"description": "Relative path inside the workspace.",
				},
				"max_bytes": map[string]any{
					"type":        "integer",
					"description": "Maximum bytes to read. Defaults to 65536.",
				},
			},
		},
	}
}

func (t *ReadFileTool) Call(ctx context.Context, call CallContext, raw json.RawMessage) (ToolResult, error) {
	select {
	case <-ctx.Done():
		return ToolResult{}, ctx.Err()
	default:
	}

	var input readFileInput
	if err := json.Unmarshal(raw, &input); err != nil {
		return ToolResult{}, fmt.Errorf("decode read_file input: %w", err)
	}
	target, cleaned, err := t.resolve(input.Path)
	if err != nil {
		return ToolResult{}, err
	}
	if err := rejectSymlink(target); err != nil {
		return ToolResult{}, err
	}

	maxBytes := input.MaxBytes
	if maxBytes <= 0 || maxBytes > t.maxBytes {
		maxBytes = t.maxBytes
	}
	file, err := os.Open(target)
	if err != nil {
		return ToolResult{}, err
	}
	defer file.Close()

	data, err := io.ReadAll(io.LimitReader(file, int64(maxBytes)+1))
	if err != nil {
		return ToolResult{}, err
	}
	truncated := len(data) > maxBytes
	if truncated {
		data = data[:maxBytes]
	}
	content := string(data)
	if truncated {
		content += fmt.Sprintf("\n\n[truncated after %d bytes from %s]", maxBytes, cleaned)
	}
	return ToolResult{Content: content}, nil
}

type ListFilesTool struct {
	fileToolBase
	defaultLimit int
}

type listFilesInput struct {
	Path      string `json:"path"`
	Recursive bool   `json:"recursive"`
	Limit     int    `json:"limit"`
}

func NewListFilesTool(baseDir string) (*ListFilesTool, error) {
	base, err := newFileToolBase(baseDir)
	if err != nil {
		return nil, err
	}
	return &ListFilesTool{fileToolBase: base, defaultLimit: 100}, nil
}

func NewListFilesToolFactory(deps Dependencies) (Tool, error) {
	return NewListFilesTool(deps.BaseDir)
}

func (t *ListFilesTool) Name() string {
	return "list_files"
}

func (t *ListFilesTool) Schema() llm.ToolSchema {
	return llm.ToolSchema{
		Name:        t.Name(),
		Description: "List files under a relative workspace path.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"path": map[string]any{
					"type":        "string",
					"description": "Relative path inside the workspace. Defaults to the workspace root.",
				},
				"recursive": map[string]any{
					"type":        "boolean",
					"description": "Walk recursively when true. Defaults to false.",
				},
				"limit": map[string]any{
					"type":        "integer",
					"description": "Maximum number of entries to return. Defaults to 100.",
				},
			},
		},
	}
}

func (t *ListFilesTool) Call(ctx context.Context, call CallContext, raw json.RawMessage) (ToolResult, error) {
	select {
	case <-ctx.Done():
		return ToolResult{}, ctx.Err()
	default:
	}

	var input listFilesInput
	if err := json.Unmarshal(raw, &input); err != nil {
		return ToolResult{}, fmt.Errorf("decode list_files input: %w", err)
	}
	if strings.TrimSpace(input.Path) == "" {
		input.Path = "."
	}
	target, cleaned, err := t.resolve(input.Path)
	if err != nil {
		return ToolResult{}, err
	}
	if err := rejectSymlink(target); err != nil {
		return ToolResult{}, err
	}

	limit := input.Limit
	if limit <= 0 || limit > t.defaultLimit {
		limit = t.defaultLimit
	}

	var entries []string
	if !input.Recursive {
		children, err := os.ReadDir(target)
		if err != nil {
			return ToolResult{}, err
		}
		for _, child := range children {
			name := child.Name()
			if child.IsDir() {
				name += "/"
			}
			entries = append(entries, name)
			if len(entries) >= limit {
				break
			}
		}
		sort.Strings(entries)
		return ToolResult{Content: formatListResult(cleaned, entries, limit)}, nil
	}

	err = filepath.WalkDir(target, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if path == target {
			return nil
		}
		if len(entries) >= limit {
			return filepath.SkipAll
		}
		name, err := filepath.Rel(target, path)
		if err != nil {
			return err
		}
		if d.IsDir() {
			name += "/"
		}
		entries = append(entries, name)
		return nil
	})
	if err != nil {
		return ToolResult{}, err
	}
	sort.Strings(entries)
	return ToolResult{Content: formatListResult(cleaned, entries, limit)}, nil
}

func formatListResult(path string, entries []string, limit int) string {
	if len(entries) == 0 {
		return fmt.Sprintf("%s is empty", path)
	}
	content := strings.Join(entries, "\n")
	if len(entries) >= limit {
		content += fmt.Sprintf("\n[limited to %d entries]", limit)
	}
	return content
}
