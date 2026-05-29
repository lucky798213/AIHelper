package intelligence

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

const (
	defaultSkillReferenceMaxBytes = 64 * 1024
	defaultSkillCommandTimeout    = 30 * time.Second
	defaultSkillCommandOutputMax  = 50 * 1024
)

func (s *Service) ReadSkillReference(ctx context.Context, agentID, skillName, relPath string) (string, error) {
	//读取某个已启用 skill 目录下的 reference markdown。
	//模型只有在 Active Skill 正文提示需要 reference 时，才应该通过 tool 调到这里。
	select {
	case <-ctx.Done():
		return "", ctx.Err()
	default:
	}

	skill, err := s.enabledSkill(agentID, skillName)
	if err != nil {
		return "", err
	}
	target, cleaned, err := resolveSkillRelativePath(skill.Path, relPath)
	if err != nil {
		return "", err
	}
	//reference 只能是额外的 .md 文件；SKILL.md 正文由 Active Skills 注入，不允许 tool 重读。
	if strings.EqualFold(filepath.Base(cleaned), "SKILL.md") {
		return "", fmt.Errorf("SKILL.md is loaded only through active skill selection")
	}
	if strings.ToLower(filepath.Ext(cleaned)) != ".md" {
		return "", fmt.Errorf("skill reference must be a .md file")
	}
	if err := ensureExistingPathInside(skill.Path, target); err != nil {
		return "", err
	}

	//限制单次 reference 读取大小，避免一个 reference 文件把上下文撑爆。
	file, err := os.Open(target)
	if err != nil {
		return "", err
	}
	defer file.Close()

	data, err := io.ReadAll(io.LimitReader(file, int64(defaultSkillReferenceMaxBytes)+1))
	if err != nil {
		return "", err
	}
	truncated := len(data) > defaultSkillReferenceMaxBytes
	if truncated {
		data = data[:defaultSkillReferenceMaxBytes]
	}
	content := string(data)
	if truncated {
		content += fmt.Sprintf("\n\n[truncated after %d bytes from %s]", defaultSkillReferenceMaxBytes, cleaned)
	}
	return content, nil
}

func (s *Service) RunSkillCommand(ctx context.Context, agentID, skillName, command string) (string, error) {
	//执行某个已启用 skill 允许的脚本/命令。
	//命令必须逐字出现在 SKILL.md 中，避免模型临场编造任意 shell 命令。
	skill, err := s.enabledSkill(agentID, skillName)
	if err != nil {
		return "", err
	}
	command = strings.TrimSpace(command)
	if command == "" {
		return "", fmt.Errorf("command is required")
	}
	raw, err := osReadSkillFile(skill)
	if err != nil {
		return "", err
	}
	if !strings.Contains(raw, command) {
		return "", fmt.Errorf("command must appear verbatim in %s/SKILL.md", skill.Name)
	}

	//命令在 skill 目录下执行，这样 SKILL.md 中可以写相对脚本路径，如 sh scripts/run.sh。
	runCtx, cancel := context.WithTimeout(ctx, defaultSkillCommandTimeout)
	defer cancel()

	var stdout, stderr limitedBuffer
	stdout.limit = defaultSkillCommandOutputMax
	stderr.limit = defaultSkillCommandOutputMax

	cmd := exec.CommandContext(runCtx, "sh", "-c", command)
	cmd.Dir = skill.Path
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err = cmd.Run()
	timedOut := runCtx.Err() == context.DeadlineExceeded
	exitCode := 0
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			exitCode = exitErr.ExitCode()
		} else if timedOut {
			exitCode = -1
		} else {
			return "", err
		}
	}

	return formatSkillCommandResult(exitCode, timedOut, stdout.String(), stderr.String()), nil
}

func (s *Service) enabledSkill(agentID, skillName string) (Skill, error) {
	//所有 reference 和 command 操作都只能作用在当前 agent 已启用的 skill 上。
	//被禁用、被覆盖、name mismatch 的 skill 不会出现在 cache.Skills 里。
	cache, err := s.cachedAgent(agentID)
	if err != nil {
		return Skill{}, err
	}
	skillName = strings.TrimSpace(skillName)
	if skillName == "" {
		return Skill{}, fmt.Errorf("skill_name is required")
	}
	for _, skill := range cache.Skills {
		if skill.Name == skillName {
			return skill, nil
		}
	}
	return Skill{}, fmt.Errorf("skill %q is not enabled for agent %q", skillName, agentID)
}

func osReadSkillFile(skill Skill) (string, error) {
	//读取 SKILL.md 前先做路径和 symlink 校验。
	//这样 Active Skills 注入和 command 白名单校验都不会跟随危险链接。
	target := filepath.Join(skill.Path, "SKILL.md")
	if err := ensureExistingPathInside(skill.Path, target); err != nil {
		return "", err
	}
	raw, err := os.ReadFile(target)
	if err != nil {
		return "", err
	}
	return string(raw), nil
}

func resolveSkillRelativePath(base, relPath string) (string, string, error) {
	//把模型传入的 reference 路径收敛到 skill 目录内部。
	//拒绝绝对路径和 ../，防止读取 workspace 里的其他文件。
	trimmed := strings.TrimSpace(relPath)
	if trimmed == "" {
		return "", "", fmt.Errorf("path is required")
	}
	if filepath.IsAbs(trimmed) {
		return "", "", fmt.Errorf("path must be relative to the skill directory")
	}
	cleaned := filepath.Clean(trimmed)
	if cleaned == "." || cleaned == ".." || strings.HasPrefix(cleaned, ".."+string(os.PathSeparator)) {
		return "", "", fmt.Errorf("path escapes skill directory")
	}
	target := filepath.Clean(filepath.Join(base, cleaned))
	if !pathInside(base, target) {
		return "", "", fmt.Errorf("path escapes skill directory")
	}
	return target, cleaned, nil
}

func ensureExistingPathInside(base, target string) error {
	//对实际存在的目标文件做二次校验：
	//1. 拒绝 symlink 本身；
	//2. EvalSymlinks 后确认真实路径仍在 skill 目录内。
	info, err := os.Lstat(target)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("refusing to access symlink %q", target)
	}
	realBase, err := filepath.EvalSymlinks(base)
	if err != nil {
		return err
	}
	realTarget, err := filepath.EvalSymlinks(target)
	if err != nil {
		return err
	}
	if !pathInside(realBase, realTarget) {
		return fmt.Errorf("path escapes skill directory through symlink")
	}
	return nil
}

func pathInside(base, target string) bool {
	rel, err := filepath.Rel(base, target)
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(os.PathSeparator)))
}

type limitedBuffer struct {
	limit     int
	buf       bytes.Buffer
	truncated bool
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	//给脚本 stdout/stderr 加硬上限。
	//返回 len(p) 可以避免子进程因为 pipe 写入短写而异常退出。
	if b.limit <= 0 {
		return len(p), nil
	}
	remaining := b.limit - b.buf.Len()
	if remaining > 0 {
		if len(p) <= remaining {
			_, _ = b.buf.Write(p)
		} else {
			_, _ = b.buf.Write(p[:remaining])
			b.truncated = true
		}
	} else {
		b.truncated = true
	}
	return len(p), nil
}

func (b *limitedBuffer) String() string {
	out := b.buf.String()
	if b.truncated {
		out += fmt.Sprintf("\n[truncated after %d bytes]", b.limit)
	}
	return out
}

func formatSkillCommandResult(exitCode int, timedOut bool, stdout, stderr string) string {
	return fmt.Sprintf(
		"exit_code: %d\ntimed_out: %t\nstdout:\n%s\nstderr:\n%s",
		exitCode,
		timedOut,
		strings.TrimRight(stdout, "\n"),
		strings.TrimRight(stderr, "\n"),
	)
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
