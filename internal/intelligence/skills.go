package intelligence

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

const (
	DefaultMaxSkills       = 150
	DefaultMaxSkillsPrompt = 30000
)

type Skill struct {
	Name        string
	Description string
	Invocation  string
	Path        string
	SourceRoot  string
	Plugin      string
	DirName     string
}

type PluginConfig struct {
	SkillRoots      []string
	DisabledSkills  []string
	DisabledPlugins []string
}

type SkillDebug struct {
	Skill
	Enabled    bool   // 这个 skill 最终是否可用
	Overridden bool   // 是否被后扫描到的同名 skill 覆盖
	Reason     string // 禁用/覆盖原因，主要给 /skills 调试命令看
}

type SkillsManager struct {
	AgentWorkspace string
	ProjectRoot    string
	Plugins        PluginConfig
	MaxSkills      int
	MaxPromptChars int
}

func NewSkillsManager(agentWorkspace, projectRoot string) SkillsManager {
	return SkillsManager{
		AgentWorkspace: agentWorkspace,
		ProjectRoot:    projectRoot,
		MaxSkills:      DefaultMaxSkills,
		MaxPromptChars: DefaultMaxSkillsPrompt,
	}
}

func (m SkillsManager) Discover(ctx context.Context) ([]Skill, error) {
	//Discover 是给业务流程用的精简视图：
	//只返回当前真正启用、没有被覆盖的 skill metadata。
	//注意这里不会读取/缓存 SKILL.md 正文，正文只在 selector 命中后按需读取。
	debug, err := m.DiscoverDebug(ctx)
	if err != nil {
		return nil, err
	}
	var result []Skill
	for _, item := range debug {
		if item.Enabled && !item.Overridden {
			result = append(result, item.Skill)
		}
	}
	maxSkills := m.MaxSkills
	if maxSkills <= 0 {
		maxSkills = DefaultMaxSkills
	}
	if len(result) > maxSkills {
		result = result[:maxSkills]
	}
	return result, nil
}

func (m SkillsManager) DiscoverDebug(ctx context.Context) ([]SkillDebug, error) {
	//配置最大技能数量
	maxSkills := m.MaxSkills
	if maxSkills <= 0 {
		maxSkills = DefaultMaxSkills
	}

	//构建要扫描的目录列表
	roots := []string{ //roots 是精准到指定 agent 的 Route
		filepath.Join(m.AgentWorkspace, "skills"),
		filepath.Join(m.AgentWorkspace, ".skills"),
		filepath.Join(m.AgentWorkspace, ".agents", "skills"),
		filepath.Join(m.ProjectRoot, ".agents", "skills"),
		filepath.Join(m.ProjectRoot, "skills"),
	}

	//再将 intelligence 中 plugins 的 skillsRoots添加进来
	for _, root := range m.Plugins.SkillRoots {
		if strings.TrimSpace(root) == "" {
			continue
		}
		if filepath.IsAbs(root) {
			roots = append(roots, filepath.Clean(root))
		} else {
			roots = append(roots, filepath.Clean(filepath.Join(m.ProjectRoot, root)))
		}
	}

	//自己实现的一个集合
	disabledSkills := stringSet(m.Plugins.DisabledSkills)
	disabledPlugins := stringSet(m.Plugins.DisabledPlugins)

	//当前最新的、可用的 skill 在 debug 数组里的下标
	latestIndex := make(map[string]int)

	//存储可用的 skill item
	debug := make([]SkillDebug, 0)
	for _, root := range roots {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}

		//提取出当前根目录下所有 skill metadata。
		//这里仅解析 frontmatter 和路径信息，不保留正文，避免启动时把所有 skill 内容塞进上下文。
		skills, err := scanSkillsDir(root)
		if err != nil {
			return nil, err
		}
		for _, skill := range skills {
			//默认假设这个技能是启用的，先创建一个调试信息项。
			item := SkillDebug{Skill: skill, Enabled: true}

			//SKILL.md 必须显式声明 name；目录名也必须和 name 一致。
			//不一致时只禁用这个 skill，不让整个 agent 启动失败。
			if strings.TrimSpace(skill.Name) == "" {
				item.Enabled = false
				item.Reason = "missing name"
			}

			if item.Enabled && skill.Name != skill.DirName {
				item.Enabled = false
				item.Reason = "name mismatch"
			}

			//检查：这个技能名字在禁用列表里吗
			if item.Enabled {
				if _, ok := disabledSkills[skill.Name]; ok {
					item.Enabled = false
					item.Reason = "disabled skill"
				}
			}

			//description 是 selector 选择 skill 的核心依据，所以缺失时禁用。
			if item.Enabled && strings.TrimSpace(skill.Description) == "" {
				item.Enabled = false
				item.Reason = "missing description"
			}

			//检查：这个技能所属的插件被禁用了吗
			if item.Enabled {
				if _, ok := disabledPlugins[skill.Plugin]; ok {
					item.Enabled = false
					item.Reason = "disabled plugin"
				}
			}

			//只有当前技能是启用状态，才走下面逻辑（处理覆盖）
			if item.Enabled {
				//检查之前有没有同名 skill。
				if prev, exists := latestIndex[skill.Name]; exists && debug[prev].Enabled {
					//标记已经覆盖了
					debug[prev].Overridden = true
					debug[prev].Reason = "overridden by later skill"
				}
				latestIndex[skill.Name] = len(debug)
			}
			debug = append(debug, item)
		}
	}

	enabledCount := 0
	//如果 skill 超过设置数量，就将超出的部分停用
	for i := range debug {
		if debug[i].Enabled && !debug[i].Overridden {
			enabledCount++
			if enabledCount > maxSkills {
				debug[i].Enabled = false
				debug[i].Reason = "max skills limit"
			}
		}
	}
	return debug, nil
}

func (m SkillsManager) FormatPromptBlock(skills []Skill) string {
	//如果没有技能，直接返回空
	if len(skills) == 0 {
		return ""
	}

	//设置最大长度限制
	maxPromptChars := m.MaxPromptChars
	if maxPromptChars <= 0 {
		maxPromptChars = DefaultMaxSkillsPrompt
	}

	//创建 metadata-only 的 skill catalog。
	//这个 block 会常驻 system prompt，但只包含 name/description/invocation，不包含 SKILL.md 正文。
	var builder strings.Builder
	builder.WriteString("## Available Skills\n\n")
	builder.WriteString("These skills are available as metadata only. Use a skill only when it is clearly relevant to the user's request; no skill is required when none fit. When a skill is activated, its full SKILL.md instructions will appear under Active Skills.\n\n")
	total := 0
	for _, skill := range skills {
		//将上下文拼接
		block := fmt.Sprintf(
			"### Skill: %s\nDescription: %s\nInvocation: %s\n",
			skill.Name,
			skill.Description,
			skill.Invocation,
		)
		block += "\n"

		//如果过长就直接直接舍弃剩下的所有 skill
		if total+len([]rune(block)) > maxPromptChars {
			builder.WriteString("(... more skills truncated)")
			break
		}
		builder.WriteString(block)
		total += len([]rune(block))
	}
	return builder.String()
}

func scanSkillsDir(root string) ([]Skill, error) {
	//读取目录里的所有内容
	entries, err := os.ReadDir(root)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		} // 目录不存在就直接返回空，不报错
		return nil, err
	}

	//把文件夹按字母顺序排序
	//保证每次扫描顺序一致，不会乱
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].Name() < entries[j].Name()
	})

	var found []Skill

	//遍历每个子文件夹（只看文件夹）
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}

		//它认定：
		//一个文件夹 + 里面有 SKILL.md = 一个技能
		skillPath := filepath.Join(root, entry.Name(), "SKILL.md")
		raw, err := os.ReadFile(skillPath)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, err
		}

		//解析 SKILL.md 的 frontmatter。
		//正文留在文件里，后续由 LLM selector 选中时再读取。
		meta, _ := parseSkillFrontmatter(string(raw))
		name := strings.TrimSpace(meta["name"])

		//提取技能信息，组装 Skill 对象
		found = append(found, Skill{
			Name:        name,
			Description: strings.TrimSpace(meta["description"]),
			Invocation:  strings.TrimSpace(meta["invocation"]),
			Path:        filepath.Dir(skillPath),
			SourceRoot:  root,
			Plugin:      entry.Name(),
			DirName:     entry.Name(),
		})
	}
	return found, nil
}

func parseSkillFrontmatter(content string) (map[string]string, string) {
	meta := map[string]string{}
	if !strings.HasPrefix(content, "---") {
		return meta, content
	}
	parts := strings.SplitN(content, "---", 3)
	if len(parts) < 3 {
		return meta, content
	}
	var parsed map[string]any
	if err := yaml.Unmarshal([]byte(parts[1]), &parsed); err != nil {
		return meta, parts[2]
	}
	for key, value := range parsed {
		switch v := value.(type) {
		case string:
			meta[strings.TrimSpace(key)] = strings.TrimSpace(v)
		default:
			meta[strings.TrimSpace(key)] = strings.TrimSpace(fmt.Sprint(v))
		}
	}
	return meta, parts[2]
}

func stringSet(values []string) map[string]struct{} {
	set := make(map[string]struct{}, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value != "" {
			set[value] = struct{}{}
		}
	}
	return set
}
