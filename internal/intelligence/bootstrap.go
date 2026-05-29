package intelligence

import (
	"context"
	"os"
	"path/filepath"
	"strings"
)

const (
	DefaultMaxBootstrapFileChars  = 20000
	DefaultMaxBootstrapTotalChars = 150000
)

var BootstrapFiles = []string{
	"SOUL.md",
	"IDENTITY.md",
	"TOOLS.md",
	"MEMORY.md",
}

type BootstrapLoader struct {
	WorkspaceDir  string
	MaxFileChars  int
	MaxTotalChars int
}

func NewBootstrapLoader(workspaceDir string) BootstrapLoader {
	return BootstrapLoader{
		WorkspaceDir:  workspaceDir,
		MaxFileChars:  DefaultMaxBootstrapFileChars,
		MaxTotalChars: DefaultMaxBootstrapTotalChars,
	}
}

// 将 intelligence 相关文件加载出来，变成 text，然后存在loaded := make(map[string]string, len(names))这，加载完后直接返回
func (l BootstrapLoader) LoadAll(ctx context.Context, mode string) (map[string]string, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	default:
	}

	if mode == "none" {
		return map[string]string{}, nil
	}

	names := BootstrapFiles
	if mode == "minimal" {
		names = []string{"TOOLS.md"}
	}

	maxFileChars := l.MaxFileChars
	if maxFileChars <= 0 {
		maxFileChars = DefaultMaxBootstrapFileChars
	}
	maxTotalChars := l.MaxTotalChars
	if maxTotalChars <= 0 {
		maxTotalChars = DefaultMaxBootstrapTotalChars
	}

	loaded := make(map[string]string, len(names))
	total := 0
	for _, name := range names {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}

		raw, err := os.ReadFile(filepath.Join(l.WorkspaceDir, name))
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, err
		}
		content := truncateText(string(raw), maxFileChars)
		contentChars := len([]rune(content))
		if total+contentChars > maxTotalChars {
			remaining := maxTotalChars - total
			if remaining <= 0 {
				break
			}
			content = truncateText(string(raw), remaining)
			contentChars = len([]rune(content))
		}
		loaded[name] = content
		total += contentChars
	}
	return loaded, nil
}

func truncateText(content string, maxChars int) string {
	if maxChars <= 0 {
		return ""
	}
	runes := []rune(content)
	if len(runes) <= maxChars {
		return content
	}

	cut := maxChars
	prefix := string(runes[:maxChars])
	if idx := strings.LastIndex(prefix, "\n"); idx > 0 {
		cut = len([]rune(prefix[:idx]))
	}
	if cut <= 0 {
		cut = maxChars
	}
	return string(runes[:cut]) + "\n\n[... truncated ...]"
}
