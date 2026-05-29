package intelligence

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"AIHelper/internal/agent"
	"AIHelper/internal/llm"
)

type ServiceConfig struct {
	ProjectRoot string
	Agents      []agent.AgentConfig
	Now         func() time.Time
	Embedding   EmbeddingConfig
	Plugins     PluginConfig
	Embedder    Embedder
	LLMClient   llm.Client
}

type Service struct {
	mu          sync.RWMutex
	projectRoot string
	agents      map[string]agent.AgentConfig
	cache       map[string]agentCache
	now         func() time.Time
	plugins     PluginConfig
	embedder    Embedder
	llmClient   llm.Client
}

type agentCache struct {
	Agent               agent.AgentConfig
	Mode                string
	Workspace           string
	Bootstrap           map[string]string
	Skills              []Skill
	SkillsDebug         []SkillDebug
	SkillsCatalogPrompt string
	LoadedAt            time.Time
}

type PromptDebug struct {
	AgentID    string
	Prompt     string
	TotalChars int
	Sections   []PromptSection
	Warnings   []string
}

type PromptSection struct {
	Name    string
	Chars   int
	Content string
}

type BootstrapDebug struct {
	AgentID    string
	Workspace  string
	Mode       string
	LoadedAt   time.Time
	TotalChars int
	Files      []BootstrapFileDebug
}

type BootstrapFileDebug struct {
	Name   string
	Path   string
	Loaded bool
	Chars  int
}

type SkillsDebug struct {
	AgentID   string
	Workspace string
	LoadedAt  time.Time
	Skills    []SkillDebug
}

const (
	defaultMaxActiveSkills      = 3
	defaultMaxActiveSkillPrompt = 30000
)

func NewService(cfg ServiceConfig) (*Service, error) {
	//处理项目根目录
	projectRoot := strings.TrimSpace(cfg.ProjectRoot)
	if projectRoot == "" {
		projectRoot = "."
	}
	absRoot, err := filepath.Abs(projectRoot) //转化为绝对路径（absolutely）
	if err != nil {
		return nil, err
	}

	//把 agent 配置转成 map（跟 manager 结构一样）
	agents := make(map[string]agent.AgentConfig, len(cfg.Agents))
	for _, agentCfg := range cfg.Agents {
		if strings.TrimSpace(agentCfg.ID) == "" {
			continue
		}
		agents[agentCfg.ID] = agentCfg
	}

	//设置时间函数
	now := cfg.Now
	if now == nil {
		now = time.Now
	}

	//初始化 embedding
	embedder := cfg.Embedder
	if embedder == nil && cfg.Embedding.Enabled {
		embedder, err = newEmbedder(cfg.Embedding)
		if err != nil {
			return nil, err
		}
	}

	//创建 Service 对象
	service := &Service{
		projectRoot: filepath.Clean(absRoot),                  // 项目根目录
		agents:      agents,                                   // 所有 agent 配置
		cache:       make(map[string]agentCache, len(agents)), // 每个 agent 的 intelligence 缓存
		now:         now,                                      // 当前时间函数
		plugins:     cfg.Plugins,                              // skill 插件配置
		embedder:    embedder,                                 // memory embedding 搜索用
		llmClient:   cfg.LLMClient,                            // skill selector 用
	}

	//启动时预加载 intelligence 缓存，Reload 会去扫描每个 agent 的 workspace，然后加载，程序启动时缓存每个 agent 的人格、技能、bootstrap 文件。之后每轮对话不用重复扫文件，只动态做 memory recall
	if err := service.Reload(context.Background()); err != nil {
		return nil, err
	}
	return service, nil
}

func newEmbedder(cfg EmbeddingConfig) (Embedder, error) {
	provider := strings.ToLower(strings.TrimSpace(cfg.Provider))
	if provider == "" {
		provider = "openai_compatible"
	}
	switch provider {
	case "openai_compatible":
		return NewOpenAICompatibleEmbedder(cfg)
	default:
		return nil, fmt.Errorf("unsupported embedding provider %q", cfg.Provider)
	}
}

func (s *Service) Reload(ctx context.Context) error {
	//创建一个新的缓存容器，最终 s.cache = next
	next := make(map[string]agentCache, len(s.agents))
	ids := make([]string, 0, len(s.agents))

	//拿到所有 agentID，并排序
	for id := range s.agents {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	//每个 agent并处理
	for _, id := range ids {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		//拿 agent 配置、prompt 模式、workspace
		cfg := s.agents[id]

		//获取这个 agent 需要的 intelligence 的级别，有 full（完整），minimal（精简），none（极简）
		mode := promptMode(cfg.Intelligence)

		//找到存储这个 agent 的 intelligence 文件的地方
		workspace := s.agentWorkspace(cfg)

		//加载 bootstrap 文件
		//从 agent workspace 里加载这些文件：SOUL.md IDENTITY.md TOOLS.md MEMORY.md
		bootstrap, err := NewBootstrapLoader(workspace).LoadAll(ctx, mode) //格式bootstrap["IDENTITY.md"] = "..."
		if err != nil {
			return err
		}

		//存当前 agent 启用的 skill metadata。
		//这里不存 SKILL.md 正文；正文会在 selector 选中后按需读取。
		var skills []Skill

		//存所有扫描到的技能状态，包括 enabled、disabled、overridden，以及原因。这个主要给 /skills 调试命令看。
		var skillsDebug []SkillDebug

		//把有效 skill metadata 格式化成常驻 prompt catalog。
		//这个 catalog 只帮助模型/selector 知道有哪些 skill，不包含正文。
		var skillsCatalogPrompt string

		//如果是 full mode，加载 skills
		if mode == "full" {

			//这个 manager 是每个 agent 专属的 skill manager
			manager := NewSkillsManager(workspace, s.projectRoot)

			//这个plugins是在配置文件中写好的
			manager.Plugins = s.plugins

			//扫描 skill roots，得到所有 skill 的启用/禁用/覆盖调试信息。
			skillsDebug, err = manager.DiscoverDebug(ctx)
			if err != nil {
				return err
			}

			//从调试信息里提取最终启用的 skill metadata。
			skills = enabledSkills(skillsDebug)

			skillsCatalogPrompt = manager.FormatPromptBlock(skills)
		}

		next[id] = agentCache{
			Agent:               cfg,
			Mode:                mode,
			Workspace:           workspace,
			Bootstrap:           bootstrap,
			Skills:              skills,
			SkillsDebug:         skillsDebug,
			SkillsCatalogPrompt: skillsCatalogPrompt,
			LoadedAt:            s.now(),
		}
	}

	s.mu.Lock()
	s.cache = next
	s.mu.Unlock()
	return nil
}

func (s *Service) BuildSystemPrompt(ctx context.Context, req agent.PromptBuildRequest) (string, error) {
	//判断这个 agent 有没有启用 intelligence，如果没启用，那就直接返回刚才的基础 prompt
	if !intelligenceEnabled(req.Agent.Intelligence) {
		return req.BasePrompt, nil
	}
	debug, err := s.debugPromptForRequest(ctx, req)
	if err != nil {
		return "", err
	}
	return debug.Prompt, nil
}

func (s *Service) DebugPrompt(ctx context.Context, agentID, query, channel string) (PromptDebug, error) {
	cfg, err := s.agentConfig(agentID)
	if err != nil {
		return PromptDebug{}, err
	}
	basePrompt := cfg.SystemPrompt()
	if !intelligenceEnabled(cfg.Intelligence) {
		return basePromptDebug(cfg.ID, basePrompt), nil
	}
	return s.debugPromptForRequest(ctx, agent.PromptBuildRequest{
		Agent:      cfg,
		UserInput:  query,
		Channel:    channel,
		Model:      cfg.Model,
		BasePrompt: basePrompt,
	})
}

func (s *Service) debugPromptForRequest(ctx context.Context, req agent.PromptBuildRequest) (PromptDebug, error) {
	//根据 agentID 拿缓存，
	cache, err := s.cachedAgent(req.Agent.ID)
	if err != nil {
		return PromptDebug{}, err
	}

	//如果 prompt mode 是 none，就不使用 intelligence
	if cache.Mode == "none" {
		return basePromptDebug(req.Agent.ID, req.BasePrompt), nil
	}

	//准备 memory recall 变量，
	memoryContext := ""
	activeSkillsBlock := ""
	var warnings []string

	//full mode 下，根据当前用户输入搜索 memory
	if cache.Mode == "full" && strings.TrimSpace(req.UserInput) != "" {
		//执行 memory 搜索
		//memoryTopK(req.Agent.Intelligence)：如果 agent 配置了 memory_top_k，就用配置值；否则默认 3。
		//searchMemoryResult 运行后得到检索结果
		result, err := s.searchMemoryResult(ctx, req.Agent.ID, req.UserInput, memoryTopK(req.Agent.Intelligence))
		if err != nil {
			return PromptDebug{}, err
		}

		//把 memory hits 格式化成 prompt 文本
		memoryContext = formatRecallForPrompt(result.Hits)

		//收集 warning，正常发给大模型时，BuildSystemPrompt() 只返回 debug.Prompt，warning 不会进入 system prompt。 但 CLI 调试 /prompt 会打印 warning，方便你知道搜索是不是回退了。
		warnings = append(warnings, result.Warnings...)
	}

	if cache.Mode == "full" && strings.TrimSpace(req.UserInput) != "" {
		//每轮用户输入后，先让 selector 从 metadata catalog 里判断是否需要 skill。
		//selector 可以返回空数组；这表示本轮不需要任何 skill，后续不会注入 Active Skills。
		selectedSkills, skillWarnings := s.SelectSkills(ctx, req.Agent.ID, req.UserInput)
		warnings = append(warnings, skillWarnings...)

		//只有 selector 命中的 skill 才读取 SKILL.md 正文，并作为 Active Skills 注入最终提示词。
		block, blockWarnings := s.formatActiveSkillsBlock(selectedSkills)
		activeSkillsBlock = block
		warnings = append(warnings, blockWarnings...)
	}

	//调用 buildPromptDebug 拼完整 prompt
	return s.buildPromptDebug(buildPromptInput{
		Agent:         req.Agent,
		BasePrompt:    req.BasePrompt,
		Model:         req.Model,
		Channel:       req.Channel,
		Mode:          cache.Mode,
		Bootstrap:     cache.Bootstrap,
		SkillsCatalog: cache.SkillsCatalogPrompt,
		ActiveSkills:  activeSkillsBlock,
		MemoryContext: memoryContext,
		Warnings:      warnings,
	}), nil
}

func basePromptDebug(agentID, prompt string) PromptDebug {
	prompt = strings.TrimSpace(prompt)
	return PromptDebug{
		AgentID:    agentID,
		Prompt:     prompt,
		TotalChars: len([]rune(prompt)),
		Sections: []PromptSection{{
			Name:    "Runtime",
			Chars:   len([]rune(prompt)),
			Content: prompt,
		}},
	}
}

func (s *Service) BootstrapDebug(agentID string) (BootstrapDebug, error) {
	cache, err := s.cachedAgent(agentID)
	if err != nil {
		return BootstrapDebug{}, err
	}
	files := make([]BootstrapFileDebug, 0, len(BootstrapFiles))
	total := 0
	for _, name := range BootstrapFiles {
		content, loaded := cache.Bootstrap[name]
		chars := len([]rune(content))
		total += chars
		files = append(files, BootstrapFileDebug{
			Name:   name,
			Path:   filepath.Join(cache.Workspace, name),
			Loaded: loaded,
			Chars:  chars,
		})
	}
	return BootstrapDebug{
		AgentID:    cache.Agent.ID,
		Workspace:  cache.Workspace,
		Mode:       cache.Mode,
		LoadedAt:   cache.LoadedAt,
		TotalChars: total,
		Files:      files,
	}, nil
}

func (s *Service) SkillsDebug(agentID string) (SkillsDebug, error) {
	cache, err := s.cachedAgent(agentID)
	if err != nil {
		return SkillsDebug{}, err
	}
	return SkillsDebug{
		AgentID:   cache.Agent.ID,
		Workspace: cache.Workspace,
		LoadedAt:  cache.LoadedAt,
		Skills:    append([]SkillDebug(nil), cache.SkillsDebug...),
	}, nil
}

type skillSelectResponse struct {
	Skills []string `json:"skills"`
}

func (s *Service) SelectSkills(ctx context.Context, agentID, userInput string) ([]Skill, []string) {
	//SelectSkills 是每轮对话的 skill 选择器：
	//它把用户输入和已启用 skill metadata 交给 LLM，让模型返回需要激活的 skill 名称。
	//返回空数组是正常结果，表示本轮不需要 skill。
	cache, err := s.cachedAgent(agentID)
	if err != nil {
		return nil, []string{fmt.Sprintf("skill selector skipped: %v", err)}
	}
	if cache.Mode != "full" || strings.TrimSpace(userInput) == "" || len(cache.Skills) == 0 {
		return nil, nil
	}
	if s.llmClient == nil {
		//单元测试或 mock 场景里可能没有 selector client；没有就按不使用 skill 处理。
		return nil, nil
	}

	resp, err := s.llmClient.CreateMessage(ctx, llm.Request{
		AgentID:   agentID,
		AgentRole: string(cache.Agent.Role),
		Purpose:   "skill_select",
		Model:     cache.Agent.Model,
		System:    skillSelectorSystemPrompt(cache),
		Messages: []llm.Message{{
			Role:    "user",
			Content: userInput,
		}},
	})
	if err != nil {
		return nil, []string{"skill selector failed: " + err.Error()}
	}
	if resp.StopReason != llm.StopReasonEndTurn {
		return nil, []string{fmt.Sprintf("skill selector returned unsupported stop reason %q", resp.StopReason)}
	}

	var decoded skillSelectResponse
	if err := json.Unmarshal([]byte(resp.Text), &decoded); err != nil {
		return nil, []string{"skill selector returned invalid JSON: " + err.Error()}
	}

	byName := make(map[string]Skill, len(cache.Skills))
	for _, skill := range cache.Skills {
		byName[skill.Name] = skill
	}
	selected := make([]Skill, 0, minInt(len(decoded.Skills), defaultMaxActiveSkills))
	seen := make(map[string]struct{}, len(decoded.Skills))
	for _, name := range decoded.Skills {
		//selector 的输出只当作候选名单。
		//不存在、禁用、重复的 skill 会被静默忽略，防止模型编造 skill 名称。
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		if _, ok := seen[name]; ok {
			continue
		}
		skill, ok := byName[name]
		if !ok {
			continue
		}
		selected = append(selected, skill)
		seen[name] = struct{}{}
		if len(selected) >= defaultMaxActiveSkills {
			break
		}
	}
	return selected, nil
}

func skillSelectorSystemPrompt(cache agentCache) string {
	//给 selector 的 prompt 只暴露 metadata，不暴露正文。
	//这样 selector 的任务很窄：判断“要不要激活哪个 skill”，而不是直接执行 skill。
	var b strings.Builder
	b.WriteString("You are a skill selector. Return JSON only in this exact shape: {\"skills\":[\"skill-name\"]}.\n")
	b.WriteString("Select only skills that are clearly useful for the user's latest request. Return {\"skills\":[]} when no skill is needed. Do not invent skill names.\n\n")
	b.WriteString("Agent ID: ")
	b.WriteString(cache.Agent.ID)
	b.WriteString("\nAvailable skills:\n")
	for _, skill := range cache.Skills {
		fmt.Fprintf(&b, "- name: %s\n  description: %s\n", skill.Name, skill.Description)
		if strings.TrimSpace(skill.Invocation) != "" {
			fmt.Fprintf(&b, "  invocation: %s\n", skill.Invocation)
		}
	}
	return b.String()
}

func (s *Service) formatActiveSkillsBlock(skills []Skill) (string, []string) {
	//把 selector 命中的 skill 读取成 Active Skills prompt。
	//这里才会读取 SKILL.md 正文；未命中的 skill 不会进入上下文。
	if len(skills) == 0 {
		return "", nil
	}
	var b strings.Builder
	b.WriteString("## Active Skills\n\n")
	total := 0
	var warnings []string
	for _, skill := range skills {
		body, err := readSkillBody(skill)
		if err != nil {
			warnings = append(warnings, fmt.Sprintf("load skill %q failed: %v", skill.Name, err))
			continue
		}
		block := fmt.Sprintf(
			"### Skill: %s\nDescription: %s\nInvocation: %s\n\n%s\n\n",
			skill.Name,
			skill.Description,
			skill.Invocation,
			strings.TrimSpace(body),
		)
		blockChars := len([]rune(block))
		if total+blockChars > defaultMaxActiveSkillPrompt {
			b.WriteString("(... more active skills truncated)")
			break
		}
		b.WriteString(block)
		total += blockChars
	}
	content := strings.TrimSpace(b.String())
	if content == "## Active Skills" {
		return "", warnings
	}
	return content, warnings
}

func readSkillBody(skill Skill) (string, error) {
	//读取 SKILL.md 正文，并剥掉 frontmatter。
	//reference 和脚本文件不会在这里读取，必须走对应 tool。
	raw, err := osReadSkillFile(skill)
	if err != nil {
		return "", err
	}
	_, body := parseSkillFrontmatter(raw)
	return strings.TrimSpace(body), nil
}

func (s *Service) MemoryStats(ctx context.Context, agentID string) (MemoryStats, error) {
	store, err := s.memoryStore(agentID)
	if err != nil {
		return MemoryStats{}, err
	}
	return store.Stats(ctx)
}

func (s *Service) WriteMemory(ctx context.Context, agentID, content, category string) (string, error) {
	store, err := s.memoryStore(agentID)
	if err != nil {
		return "", err
	}
	return store.WriteMemory(ctx, content, category)
}

func (s *Service) SearchMemory(ctx context.Context, agentID, query string, topK int) (string, error) {
	result, err := s.searchMemoryResult(ctx, agentID, query, topK)
	if err != nil {
		return "", err
	}
	return FormatMemorySearchResult(result), nil
}

func (s *Service) searchMemoryHits(ctx context.Context, agentID, query string, topK int) ([]MemoryHit, error) {
	result, err := s.searchMemoryResult(ctx, agentID, query, topK)
	if err != nil {
		return nil, err
	}
	return result.Hits, nil
}

func (s *Service) searchMemoryResult(ctx context.Context, agentID, query string, topK int) (MemorySearchResult, error) {
	//确认 agent 存在
	cfg, ok := s.agents[agentID]
	if !ok {
		return MemorySearchResult{}, fmt.Errorf("unknown agent %q", agentID)
	}

	//确保 topk 为正
	if topK <= 0 {
		topK = memoryTopK(cfg.Intelligence)
	}

	// 创建一个 MemoryStore
	store := NewMemoryStore(s.agentWorkspace(cfg))

	//把 Now 和 Embedder 注入进去。
	store.Now = s.now
	store.Embedder = s.embedder
	return store.HybridSearchWithWarnings(ctx, query, topK)
}

func (s *Service) memoryStore(agentID string) (MemoryStore, error) {
	cfg, ok := s.agents[agentID]
	if !ok {
		return MemoryStore{}, fmt.Errorf("unknown agent %q", agentID)
	}
	store := NewMemoryStore(s.agentWorkspace(cfg))
	store.Now = s.now
	store.Embedder = s.embedder
	return store, nil
}

func (s *Service) cachedAgent(agentID string) (agentCache, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	cache, ok := s.cache[agentID]
	if !ok {
		return agentCache{}, fmt.Errorf("unknown agent %q", agentID)
	}
	return cache, nil
}

func (s *Service) agentConfig(agentID string) (agent.AgentConfig, error) {
	cfg, ok := s.agents[agentID]
	if !ok {
		return agent.AgentConfig{}, fmt.Errorf("unknown agent %q", agentID)
	}
	return cfg, nil
}

func (s *Service) agentWorkspace(cfg agent.AgentConfig) string {
	// 1. 读取配置里的 Workspace，并且去掉前后空格（容错）
	workspace := strings.TrimSpace(cfg.Intelligence.Workspace)

	// 2. 如果配置里没填 Workspace（空）
	//→ 自动生成一个默认路径：项目根目录 / workspace / agents /  agentID
	if workspace == "" {
		return filepath.Join(s.projectRoot, "workspace", "agents", cfg.ID)
	}

	//3. 如果配置里填的是【绝对路径】（比如 C:\xxx 或 /usr/xxx）
	//→ 直接用这个路径，清理一下格式
	if filepath.IsAbs(workspace) {
		return filepath.Clean(workspace)
	} //filepath.Clean，把乱七八糟、不规范、有风险的路径，清理成简洁、规范、安全的标准路径。

	// 4. 如果配置里填的是【相对路径】（比如 "mywork"）
	// → 拼接到项目根目录下，变成绝对路径
	return filepath.Clean(filepath.Join(s.projectRoot, workspace))
}

type buildPromptInput struct {
	Agent         agent.AgentConfig
	BasePrompt    string
	Model         string
	Channel       string
	Mode          string
	Bootstrap     map[string]string
	SkillsCatalog string
	ActiveSkills  string
	MemoryContext string
	Warnings      []string
}

func (s *Service) buildPromptDebug(input buildPromptInput) PromptDebug {
	var sections []PromptSection

	//创建一个添加段落的函数，帮你干净地拼接提示词，不乱、不空、不脏
	add := func(name, content string) {
		content = strings.TrimSpace(content)
		if content == "" {
			return
		}
		sections = append(sections, PromptSection{
			Name:    name,
			Chars:   len([]rune(content)),
			Content: content,
		})
	}

	//拼接「身份 Identity」
	identity := strings.TrimSpace(input.Bootstrap["IDENTITY.md"])
	if identity == "" {
		identity = defaultIdentity(input.Agent)
	}
	add("Identity", identity)

	//拼接「灵魂 Soul」（full 模式才加）
	soul := strings.TrimSpace(input.Bootstrap["SOUL.md"])
	if input.Mode == "full" && soul != "" {
		add("Soul", "## Soul\n\n"+soul)
	}

	//拼接「工具 Tools」
	toolsMD := strings.TrimSpace(input.Bootstrap["TOOLS.md"])
	if toolsMD != "" {
		add("Tools", "## Tools\n\n"+toolsMD)
	}

	//拼接「技能 Skills」（full 模式）
	if input.Mode == "full" {
		skillsBlock := strings.TrimSpace(input.SkillsCatalog)
		if skillsBlock == "" {
			skillsBlock = "## Skills\n\nNo skills are currently loaded."
		}
		add("Skills", skillsBlock)
		add("Active Skills", input.ActiveSkills)
	}

	// 拼接「记忆 Memory」（full 模式）
	if input.Mode == "full" {
		var memoryParts []string
		if memMD := strings.TrimSpace(input.Bootstrap["MEMORY.md"]); memMD != "" {
			memoryParts = append(memoryParts, "### Evergreen Memory\n\n"+memMD)
		}
		if strings.TrimSpace(input.MemoryContext) != "" {
			memoryParts = append(memoryParts, "### Recalled Memories (auto-searched)\n\n"+input.MemoryContext)
		}
		memoryParts = append(memoryParts,
			"### Memory Instructions\n\n"+
				"- Use memory_write to save important user facts, preferences, and stable project context.\n"+
				"- Use memory_search when the user asks about prior information or when recall would improve the answer.\n"+
				"- Reference remembered facts naturally and avoid over-recording transient details.",
		)
		add("Memory", "## Memory\n\n"+strings.Join(memoryParts, "\n\n"))
	}

	//拼接「运行时环境 Runtime」，当前时间、工作目录、项目信息等
	add("Runtime", s.runtimeContext(input))

	//拼接「频道提示 Channel」
	add("Channel", "## Channel\n\n"+channelHint(input.Channel))

	//把所有段落拼成最终提示词
	promptParts := make([]string, 0, len(sections))
	totalChars := 0
	for _, section := range sections {
		promptParts = append(promptParts, section.Content)
		totalChars += section.Chars
	}
	return PromptDebug{
		AgentID:    input.Agent.ID,
		Prompt:     strings.Join(promptParts, "\n\n"),
		TotalChars: totalChars,
		Sections:   sections,
		Warnings:   append([]string(nil), input.Warnings...),
	}
}

func (s *Service) runtimeContext(input buildPromptInput) string {
	model := strings.TrimSpace(input.Model)
	if model == "" {
		model = "default"
	}
	var children []string
	for _, child := range input.Agent.Children {
		children = append(children, fmt.Sprintf("- %s (%s): %s", child.AgentID, child.Name, child.Description))
	}
	childBlock := "(none)"
	if len(children) > 0 {
		childBlock = "\n" + strings.Join(children, "\n")
	}

	basePrompt := strings.TrimSpace(input.BasePrompt)
	if basePrompt == "" {
		basePrompt = defaultIdentity(input.Agent)
	}

	return fmt.Sprintf(
		"## Runtime Context\n\n"+
			"- Agent ID: %s\n"+
			"- Agent name: %s\n"+
			"- Agent role: %s\n"+
			"- Description: %s\n"+
			"- Model: %s\n"+
			"- Channel: %s\n"+
			"- Current time: %s\n"+
			"- Prompt mode: %s\n\n"+
			"### Child Agents\n%s\n\n"+
			"### Agent Runtime Instructions\n\n%s",
		input.Agent.ID,
		input.Agent.Name,
		input.Agent.Role,
		input.Agent.Description,
		model,
		valueOrDefault(input.Channel, "unknown"),
		s.now().UTC().Format("2006-01-02 15:04 UTC"),
		input.Mode,
		childBlock,
		basePrompt,
	)
}

func enabledSkills(debug []SkillDebug) []Skill {
	var skills []Skill
	for _, item := range debug {
		if item.Enabled && !item.Overridden {
			skills = append(skills, item.Skill)
		}
	}
	return skills
}

func defaultIdentity(cfg agent.AgentConfig) string {
	name := strings.TrimSpace(cfg.Name)
	if name == "" {
		name = cfg.ID
	}
	if name == "" {
		name = "AI assistant"
	}
	description := strings.TrimSpace(cfg.Description)
	if description == "" {
		description = "Help the user clearly and directly."
	}
	return fmt.Sprintf("You are %s (%s). Agent ID: %s. %s", name, cfg.Role, cfg.ID, description)
}

// 格式： - [记忆文件路径] 记忆摘要
func formatRecallForPrompt(hits []MemoryHit) string {
	if len(hits) == 0 {
		return ""
	}
	lines := make([]string, 0, len(hits))
	for _, hit := range hits {
		lines = append(lines, fmt.Sprintf("- [%s] %s", hit.Path, hit.Snippet))
	}
	return strings.Join(lines, "\n")
}

func intelligenceEnabled(cfg agent.IntelligenceConfig) bool {
	return cfg.Enabled == nil || *cfg.Enabled
}

func promptMode(cfg agent.IntelligenceConfig) string {
	switch strings.ToLower(strings.TrimSpace(cfg.PromptMode)) {
	case "minimal":
		return "minimal" //精简的 intelligence
	case "none":
		return "none" //极简 intelligence
	default:
		return "full" //全量的 intelligence
	}
}

func memoryTopK(cfg agent.IntelligenceConfig) int {
	if cfg.MemoryTopK <= 0 {
		return 3
	}
	return cfg.MemoryTopK
}

func channelHint(channel string) string {
	switch strings.ToLower(strings.TrimSpace(channel)) {
	case "cli", "terminal":
		return "You are responding via a terminal or CLI. Markdown is supported."
	case "feishu":
		return "You are responding via Feishu. Keep replies concise, useful, and easy to read in chat."
	case "telegram":
		return "You are responding via Telegram. Keep messages concise."
	case "discord":
		return "You are responding via Discord. Keep messages under 2000 characters."
	case "slack":
		return "You are responding via Slack. Use Slack mrkdwn formatting."
	default:
		if strings.TrimSpace(channel) == "" {
			return "You are responding in a chat interface."
		}
		return "You are responding via " + channel + "."
	}
}

func valueOrDefault(value, fallback string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return fallback
	}
	return value
}
