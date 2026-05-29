package intelligence

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"AIHelper/internal/agent"
	"AIHelper/internal/llm"
)

func TestBootstrapLoaderLoadsAndTruncates(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "IDENTITY.md"), []byte(strings.Repeat("a", 30)), 0o644); err != nil {
		t.Fatalf("write identity: %v", err)
	}

	loader := NewBootstrapLoader(dir)
	loader.MaxFileChars = 10
	loader.MaxTotalChars = 100
	loaded, err := loader.LoadAll(context.Background(), "full")
	if err != nil {
		t.Fatalf("load all: %v", err)
	}
	if _, ok := loaded["SOUL.md"]; ok {
		t.Fatal("missing file should not be loaded")
	}
	if got := loaded["IDENTITY.md"]; !strings.Contains(got, "truncated") {
		t.Fatalf("expected truncated marker, got %q", got)
	}
}

func TestBootstrapLoaderLoadsOnlyCorePromptFiles(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{
		"SOUL.md",
		"IDENTITY.md",
		"TOOLS.md",
		"MEMORY.md",
		"USER.md",
		"HEARTBEAT.md",
		"BOOTSTRAP.md",
		"AGENTS.md",
	} {
		mustWrite(t, filepath.Join(dir, name), name+" content")
	}

	loaded, err := NewBootstrapLoader(dir).LoadAll(context.Background(), "full")
	if err != nil {
		t.Fatalf("load full: %v", err)
	}
	for _, name := range []string{"SOUL.md", "IDENTITY.md", "TOOLS.md", "MEMORY.md"} {
		if _, ok := loaded[name]; !ok {
			t.Fatalf("expected core file %s to load: %#v", name, loaded)
		}
	}
	for _, name := range []string{"USER.md", "HEARTBEAT.md", "BOOTSTRAP.md", "AGENTS.md"} {
		if _, ok := loaded[name]; ok {
			t.Fatalf("expected non-core file %s to stay out of prompt bootstrap: %#v", name, loaded)
		}
	}

	minimal, err := NewBootstrapLoader(dir).LoadAll(context.Background(), "minimal")
	if err != nil {
		t.Fatalf("load minimal: %v", err)
	}
	if len(minimal) != 1 || minimal["TOOLS.md"] == "" {
		t.Fatalf("minimal mode should load only TOOLS.md, got %#v", minimal)
	}
}

func TestSkillsManagerDiscoversAndOverrides(t *testing.T) {
	root := t.TempDir()
	agentWorkspace := filepath.Join(root, "workspace", "agents", "local-master")
	writeSkill(t, filepath.Join(agentWorkspace, "skills", "shared"), "shared", "agent version")
	writeSkill(t, filepath.Join(root, "skills", "shared"), "shared", "project version")
	writeSkill(t, filepath.Join(root, "skills", "project"), "project", "project only")

	manager := NewSkillsManager(agentWorkspace, root)
	skills, err := manager.Discover(context.Background())
	if err != nil {
		t.Fatalf("discover: %v", err)
	}
	if len(skills) != 2 {
		t.Fatalf("skills len = %d, want 2: %#v", len(skills), skills)
	}
	byName := map[string]Skill{}
	for _, skill := range skills {
		byName[skill.Name] = skill
	}
	if !strings.Contains(byName["shared"].Path, filepath.Join("skills", "shared")) {
		t.Fatalf("expected later project skill to override shared, got %#v", byName["shared"])
	}
	block := manager.FormatPromptBlock(skills)
	if !strings.Contains(block, "## Available Skills") || !strings.Contains(block, "Skill: project") {
		t.Fatalf("unexpected skills block: %s", block)
	}
	if strings.Contains(block, "project only") {
		t.Fatalf("metadata prompt should not include skill body: %s", block)
	}
}

func TestMemoryStoreWritesAndSearchesEvergreenAndDaily(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "MEMORY.md"), []byte("User prefers blue color and Go examples."), 0o644); err != nil {
		t.Fatalf("write evergreen: %v", err)
	}

	store := NewMemoryStore(dir)
	store.Now = func() time.Time { return time.Date(2026, 5, 21, 10, 0, 0, 0, time.UTC) }
	if _, err := store.WriteMemory(context.Background(), "The project uses Feishu long connection events.", "project"); err != nil {
		t.Fatalf("write memory: %v", err)
	}

	blueHits, err := store.HybridSearch(context.Background(), "blue preference", 3)
	if err != nil {
		t.Fatalf("search blue: %v", err)
	}
	if !containsHitPath(blueHits, "MEMORY.md") {
		t.Fatalf("expected evergreen hit, got %#v", blueHits)
	}

	feishuHits, err := store.HybridSearch(context.Background(), "Feishu connection", 3)
	if err != nil {
		t.Fatalf("search feishu: %v", err)
	}
	if !containsHitPath(feishuHits, "2026-05-21.jsonl [project]") {
		t.Fatalf("expected daily hit, got %#v", feishuHits)
	}
	if len(feishuHits) > 3 {
		t.Fatalf("top_k ignored: %#v", feishuHits)
	}
}

func TestPromptBuilderAssemblesS06Layers(t *testing.T) {
	root := t.TempDir()
	workspace := filepath.Join(root, "workspace", "agents", "local-master")
	mustWrite(t, filepath.Join(workspace, "IDENTITY.md"), "# Identity\n\nTest identity.")
	mustWrite(t, filepath.Join(workspace, "SOUL.md"), "Curious and precise.")
	mustWrite(t, filepath.Join(workspace, "TOOLS.md"), "Use tools when actions are needed.")
	mustWrite(t, filepath.Join(workspace, "MEMORY.md"), "User likes blue dashboards.")
	mustWrite(t, filepath.Join(workspace, "USER.md"), "legacy user context should stay out")
	mustWrite(t, filepath.Join(workspace, "BOOTSTRAP.md"), "legacy bootstrap should stay out")
	mustWrite(t, filepath.Join(workspace, "AGENTS.md"), "legacy agents should stay out")
	mustWrite(t, filepath.Join(workspace, "HEARTBEAT.md"), "heartbeat instructions should stay out")
	writeSkill(t, filepath.Join(workspace, "skills", "note"), "note", "Use notes well.")

	service, err := NewService(ServiceConfig{
		ProjectRoot: root,
		Agents: []agent.AgentConfig{{
			ID:          "local-master",
			Name:        "Local Master",
			Role:        agent.AgentRoleMaster,
			Description: "Routes messages.",
		}},
		Now: func() time.Time { return time.Date(2026, 5, 21, 10, 0, 0, 0, time.UTC) },
	})
	if err != nil {
		t.Fatalf("new service: %v", err)
	}

	prompt, err := service.BuildSystemPrompt(context.Background(), agent.PromptBuildRequest{
		Agent: agent.AgentConfig{
			ID:          "local-master",
			Name:        "Local Master",
			Role:        agent.AgentRoleMaster,
			Description: "Routes messages.",
		},
		UserInput:  "blue dashboard",
		Channel:    "feishu",
		Model:      "test-model",
		BasePrompt: "base prompt",
	})
	if err != nil {
		t.Fatalf("build prompt: %v", err)
	}

	assertOrder(t, prompt,
		"# Identity",
		"## Soul",
		"## Tools",
		"## Available Skills",
		"## Memory",
		"## Runtime Context",
		"## Channel",
	)
	for _, want := range []string{
		"Test identity.",
		"Curious and precise.",
		"Skill: note",
		"Recalled Memories",
		"User likes blue dashboards.",
		"Channel: feishu",
		"responding via Feishu",
	} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("prompt missing %q:\n%s", want, prompt)
		}
	}
	for _, unwanted := range []string{
		"legacy user context should stay out",
		"legacy bootstrap should stay out",
		"legacy agents should stay out",
		"heartbeat instructions should stay out",
	} {
		if strings.Contains(prompt, unwanted) {
			t.Fatalf("prompt should not contain %q:\n%s", unwanted, prompt)
		}
	}

	debug, err := service.DebugPrompt(context.Background(), "local-master", "blue dashboard", "feishu")
	if err != nil {
		t.Fatalf("debug prompt: %v", err)
	}
	if debug.TotalChars <= 0 || len(debug.Sections) < 7 {
		t.Fatalf("unexpected debug summary: %#v", debug)
	}
	assertSectionOrder(t, debug.Sections, "Identity", "Soul", "Tools", "Skills", "Memory", "Runtime", "Channel")
}

func TestServiceCachesBootstrapUntilReload(t *testing.T) {
	root := t.TempDir()
	workspace := filepath.Join(root, "workspace", "agents", "local-master")
	mustWrite(t, filepath.Join(workspace, "IDENTITY.md"), "# Identity\n\nCached agent.")
	mustWrite(t, filepath.Join(workspace, "SOUL.md"), "old soul")

	service, err := NewService(ServiceConfig{
		ProjectRoot: root,
		Agents: []agent.AgentConfig{{
			ID:   "local-master",
			Name: "Local Master",
			Role: agent.AgentRoleMaster,
		}},
	})
	if err != nil {
		t.Fatalf("new service: %v", err)
	}

	first, err := service.DebugPrompt(context.Background(), "local-master", "", "cli")
	if err != nil {
		t.Fatalf("first prompt: %v", err)
	}
	mustWrite(t, filepath.Join(workspace, "SOUL.md"), "new soul")
	second, err := service.DebugPrompt(context.Background(), "local-master", "", "cli")
	if err != nil {
		t.Fatalf("second prompt: %v", err)
	}
	if !strings.Contains(first.Prompt, "old soul") || strings.Contains(second.Prompt, "new soul") {
		t.Fatalf("expected cached old soul before reload, first=%q second=%q", first.Prompt, second.Prompt)
	}

	if err := service.Reload(context.Background()); err != nil {
		t.Fatalf("reload: %v", err)
	}
	third, err := service.DebugPrompt(context.Background(), "local-master", "", "cli")
	if err != nil {
		t.Fatalf("third prompt: %v", err)
	}
	if !strings.Contains(third.Prompt, "new soul") {
		t.Fatalf("expected reload to pick up new soul:\n%s", third.Prompt)
	}
}

func TestServiceDebugSurfacesBootstrapSkillsAndMemory(t *testing.T) {
	root := t.TempDir()
	workspace := filepath.Join(root, "workspace", "agents", "local-master")
	mustWrite(t, filepath.Join(workspace, "IDENTITY.md"), "# Identity\n\nDebug agent.")
	mustWrite(t, filepath.Join(workspace, "MEMORY.md"), "User likes terminal-first workflows.")
	writeSkill(t, filepath.Join(workspace, "skills", "debug"), "debug", "Inspect debug state.")

	service, err := NewService(ServiceConfig{
		ProjectRoot: root,
		Agents: []agent.AgentConfig{{
			ID:   "local-master",
			Name: "Local Master",
			Role: agent.AgentRoleMaster,
		}},
		Now: func() time.Time { return time.Date(2026, 5, 21, 10, 0, 0, 0, time.UTC) },
	})
	if err != nil {
		t.Fatalf("new service: %v", err)
	}
	if _, err := service.WriteMemory(context.Background(), "local-master", "The CLI has internal-only debug commands.", "project"); err != nil {
		t.Fatalf("write memory: %v", err)
	}

	bootstrap, err := service.BootstrapDebug("local-master")
	if err != nil {
		t.Fatalf("bootstrap debug: %v", err)
	}
	if bootstrap.TotalChars == 0 || !debugFileLoaded(bootstrap.Files, "IDENTITY.md") {
		t.Fatalf("unexpected bootstrap debug: %#v", bootstrap)
	}

	skills, err := service.SkillsDebug("local-master")
	if err != nil {
		t.Fatalf("skills debug: %v", err)
	}
	if len(skills.Skills) != 1 || skills.Skills[0].Name != "debug" {
		t.Fatalf("unexpected skills debug: %#v", skills.Skills)
	}

	stats, err := service.MemoryStats(context.Background(), "local-master")
	if err != nil {
		t.Fatalf("memory stats: %v", err)
	}
	if stats.EvergreenChars == 0 || stats.DailyEntries != 1 {
		t.Fatalf("unexpected stats: %#v", stats)
	}

	search, err := service.SearchMemory(context.Background(), "local-master", "debug commands", 2)
	if err != nil {
		t.Fatalf("search memory: %v", err)
	}
	if !strings.Contains(search, "debug commands") {
		t.Fatalf("unexpected search result: %s", search)
	}
}

func TestSkillsManagerAppliesPluginConfigAndOverrides(t *testing.T) {
	root := t.TempDir()
	agentWorkspace := filepath.Join(root, "workspace", "agents", "local-master")
	writeSkill(t, filepath.Join(agentWorkspace, "skills", "shared"), "shared", "agent version")
	writeSkill(t, filepath.Join(root, "skills", "shared"), "shared", "project version")
	writeSkill(t, filepath.Join(root, "skills", "blocked"), "blocked", "blocked by name")
	writeSkill(t, filepath.Join(root, "skills", "disabled-plugin"), "disabled-plugin", "blocked by plugin")
	writeSkill(t, filepath.Join(root, "extra-skills", "shared"), "shared", "extra version")

	manager := NewSkillsManager(agentWorkspace, root)
	manager.Plugins = PluginConfig{
		SkillRoots:      []string{"extra-skills"},
		DisabledSkills:  []string{"blocked"},
		DisabledPlugins: []string{"disabled-plugin"},
	}
	debug, err := manager.DiscoverDebug(context.Background())
	if err != nil {
		t.Fatalf("discover debug: %v", err)
	}
	skills := enabledSkills(debug)
	if len(skills) != 1 || skills[0].Name != "shared" || !strings.Contains(skills[0].Path, "extra-skills") {
		t.Fatalf("unexpected enabled skills: %#v", skills)
	}
	if !hasSkillStatus(debug, "shared", "overridden by later skill") {
		t.Fatalf("expected shared override in debug: %#v", debug)
	}
	if !hasSkillStatus(debug, "blocked", "disabled skill") {
		t.Fatalf("expected blocked skill debug: %#v", debug)
	}
	if !hasSkillStatus(debug, "disabled-plugin", "disabled plugin") {
		t.Fatalf("expected disabled plugin debug: %#v", debug)
	}
}

func TestSkillsManagerDisablesNameMismatch(t *testing.T) {
	root := t.TempDir()
	agentWorkspace := filepath.Join(root, "workspace", "agents", "local-master")
	writeSkill(t, filepath.Join(agentWorkspace, "skills", "actual-name"), "wrong-name", "mismatch body")

	manager := NewSkillsManager(agentWorkspace, root)
	debug, err := manager.DiscoverDebug(context.Background())
	if err != nil {
		t.Fatalf("discover debug: %v", err)
	}
	if len(enabledSkills(debug)) != 0 {
		t.Fatalf("expected mismatched skill to be disabled: %#v", debug)
	}
	if !hasSkillStatus(debug, "wrong-name", "name mismatch") {
		t.Fatalf("expected name mismatch status: %#v", debug)
	}
}

func TestPromptBuilderInjectsSelectedActiveSkill(t *testing.T) {
	root := t.TempDir()
	workspace := filepath.Join(root, "workspace", "agents", "local-master")
	mustWrite(t, filepath.Join(workspace, "IDENTITY.md"), "# Identity\n\nSelector agent.")
	writeSkill(t, filepath.Join(workspace, "skills", "note"), "note", "Use notes well.")

	selector := &selectorClient{text: `{"skills":["note"]}`}
	service, err := NewService(ServiceConfig{
		ProjectRoot: root,
		Agents: []agent.AgentConfig{{
			ID:    "local-master",
			Name:  "Local Master",
			Role:  agent.AgentRoleMaster,
			Model: "selector-model",
		}},
		LLMClient: selector,
	})
	if err != nil {
		t.Fatalf("new service: %v", err)
	}

	debug, err := service.DebugPrompt(context.Background(), "local-master", "please use notes", "cli")
	if err != nil {
		t.Fatalf("debug prompt: %v", err)
	}
	if !strings.Contains(debug.Prompt, "## Available Skills") || strings.Contains(sectionContent(debug, "Skills"), "Use notes well.") {
		t.Fatalf("metadata skills section should not contain body:\n%s", debug.Prompt)
	}
	if !strings.Contains(debug.Prompt, "## Active Skills") || !strings.Contains(debug.Prompt, "Use notes well.") {
		t.Fatalf("expected active skill body in prompt:\n%s", debug.Prompt)
	}
	if len(selector.requests) != 1 || selector.requests[0].Purpose != "skill_select" {
		t.Fatalf("expected one skill_select request, got %#v", selector.requests)
	}
}

func TestPromptBuilderAllowsNoSelectedSkill(t *testing.T) {
	root := t.TempDir()
	workspace := filepath.Join(root, "workspace", "agents", "local-master")
	mustWrite(t, filepath.Join(workspace, "IDENTITY.md"), "# Identity\n\nSelector agent.")
	writeSkill(t, filepath.Join(workspace, "skills", "note"), "note", "Use notes well.")

	service, err := NewService(ServiceConfig{
		ProjectRoot: root,
		Agents: []agent.AgentConfig{{
			ID:   "local-master",
			Name: "Local Master",
			Role: agent.AgentRoleMaster,
		}},
		LLMClient: &selectorClient{text: `{"skills":[]}`},
	})
	if err != nil {
		t.Fatalf("new service: %v", err)
	}
	debug, err := service.DebugPrompt(context.Background(), "local-master", "plain chat", "cli")
	if err != nil {
		t.Fatalf("debug prompt: %v", err)
	}
	if strings.Contains(debug.Prompt, "## Active Skills") || strings.Contains(debug.Prompt, "Use notes well.") {
		t.Fatalf("expected no active skill injection:\n%s", debug.Prompt)
	}
}

func TestPromptBuilderWarnsWhenSelectorFails(t *testing.T) {
	root := t.TempDir()
	workspace := filepath.Join(root, "workspace", "agents", "local-master")
	writeSkill(t, filepath.Join(workspace, "skills", "note"), "note", "Use notes well.")

	service, err := NewService(ServiceConfig{
		ProjectRoot: root,
		Agents: []agent.AgentConfig{{
			ID:   "local-master",
			Name: "Local Master",
			Role: agent.AgentRoleMaster,
		}},
		LLMClient: &selectorClient{err: fmt.Errorf("selector down")},
	})
	if err != nil {
		t.Fatalf("new service: %v", err)
	}
	debug, err := service.DebugPrompt(context.Background(), "local-master", "please use notes", "cli")
	if err != nil {
		t.Fatalf("debug prompt: %v", err)
	}
	if len(debug.Warnings) == 0 || !strings.Contains(debug.Warnings[0], "selector down") {
		t.Fatalf("expected selector warning, got %#v", debug.Warnings)
	}
}

func TestSkillReferenceAndCommandRuntime(t *testing.T) {
	root := t.TempDir()
	workspace := filepath.Join(root, "workspace", "agents", "local-master")
	skillDir := filepath.Join(workspace, "skills", "runner")
	writeSkill(t, skillDir, "runner", "Read guide.md when extra context is needed.\nRun `sh hello.sh` when a greeting is needed.")
	mustWrite(t, filepath.Join(skillDir, "guide.md"), "reference content")
	mustWrite(t, filepath.Join(skillDir, "hello.sh"), "echo hello from skill")

	service, err := NewService(ServiceConfig{
		ProjectRoot: root,
		Agents: []agent.AgentConfig{{
			ID:   "local-master",
			Name: "Local Master",
			Role: agent.AgentRoleMaster,
		}},
	})
	if err != nil {
		t.Fatalf("new service: %v", err)
	}

	ref, err := service.ReadSkillReference(context.Background(), "local-master", "runner", "guide.md")
	if err != nil {
		t.Fatalf("read reference: %v", err)
	}
	if ref != "reference content" {
		t.Fatalf("reference = %q", ref)
	}
	for _, path := range []string{"SKILL.md", "hello.sh", "../guide.md"} {
		if _, err := service.ReadSkillReference(context.Background(), "local-master", "runner", path); err == nil {
			t.Fatalf("expected reference path %q to be rejected", path)
		}
	}

	out, err := service.RunSkillCommand(context.Background(), "local-master", "runner", "sh hello.sh")
	if err != nil {
		t.Fatalf("run command: %v", err)
	}
	if !strings.Contains(out, "exit_code: 0") || !strings.Contains(out, "hello from skill") {
		t.Fatalf("unexpected command result:\n%s", out)
	}
	if _, err := service.RunSkillCommand(context.Background(), "local-master", "runner", "sh missing.sh"); err == nil {
		t.Fatal("expected command not present in SKILL.md to be rejected")
	}
}

func TestOpenAICompatibleEmbedderCallsEmbeddingEndpoint(t *testing.T) {
	var called bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		if r.URL.Path != "/embeddings" {
			t.Fatalf("path = %s, want /embeddings", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer test-key" {
			t.Fatalf("authorization = %q", got)
		}
		var req openAIEmbeddingRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		if req.Model != "embed-test" || req.Input != "hello" {
			t.Fatalf("unexpected request: %#v", req)
		}
		fmt.Fprint(w, `{"data":[{"embedding":[0.1,0.2,0.3]}]}`)
	}))
	defer server.Close()

	embedder, err := NewOpenAICompatibleEmbedder(EmbeddingConfig{
		APIKey:  "test-key",
		BaseURL: server.URL,
		Model:   "embed-test",
	})
	if err != nil {
		t.Fatalf("new embedder: %v", err)
	}
	vector, err := embedder.Embed(context.Background(), "hello")
	if err != nil {
		t.Fatalf("embed: %v", err)
	}
	if !called || len(vector) != 3 || vector[1] != 0.2 {
		t.Fatalf("unexpected vector called=%v vector=%#v", called, vector)
	}
}

func TestMemorySearchFallsBackWhenEmbeddingFails(t *testing.T) {
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "MEMORY.md"), "Feishu long connection receives chat messages.")
	store := NewMemoryStore(dir)
	store.Embedder = failingEmbedder{}

	result, err := store.HybridSearchWithWarnings(context.Background(), "Feishu messages", 3)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(result.Hits) == 0 {
		t.Fatal("expected hash-vector fallback hits")
	}
	if len(result.Warnings) != 1 || !strings.Contains(result.Warnings[0], "fell back to hash-vector for all vector comparisons") {
		t.Fatalf("expected fallback warning, got %#v", result.Warnings)
	}
}

func TestVectorSearchFallsBackForAllComparisonsWhenChunkEmbeddingFails(t *testing.T) {
	chunks := []memoryChunk{
		{Path: "first", Text: "Feishu long connection receives chat messages."},
		{Path: "second", Text: "User prefers blue dashboards."},
	}
	query := "Feishu messages"
	embedder := &selectiveFailEmbedder{failText: "blue dashboards"}
	store := NewMemoryStore(t.TempDir())
	store.Embedder = embedder

	got, warnings := store.vectorSearch(context.Background(), query, chunks, 10)
	want := hashVectorSearch(query, chunks, 10)

	if len(warnings) != 1 || !strings.Contains(warnings[0], "fell back to hash-vector for all vector comparisons") {
		t.Fatalf("expected full fallback warning, got %#v", warnings)
	}
	if len(embedder.calls) != 3 {
		t.Fatalf("expected query and chunks to be attempted before fallback, got calls %#v", embedder.calls)
	}
	assertScoredMemoryEqual(t, got, want)
}

func writeSkill(t *testing.T, dir, name, body string) {
	t.Helper()
	content := "---\nname: " + name + "\ndescription: test skill\ninvocation: $" + name + "\n---\n" + body
	mustWrite(t, filepath.Join(dir, "SKILL.md"), content)
}

type selectorClient struct {
	text     string
	err      error
	requests []llm.Request
}

func (c *selectorClient) CreateMessage(ctx context.Context, req llm.Request) (llm.Response, error) {
	c.requests = append(c.requests, req)
	if c.err != nil {
		return llm.Response{}, c.err
	}
	text := c.text
	if text == "" {
		text = `{"skills":[]}`
	}
	return llm.Response{StopReason: llm.StopReasonEndTurn, Text: text}, nil
}

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func sectionContent(debug PromptDebug, name string) string {
	for _, section := range debug.Sections {
		if section.Name == name {
			return section.Content
		}
	}
	return ""
}

func containsHitPath(hits []MemoryHit, path string) bool {
	for _, hit := range hits {
		if hit.Path == path {
			return true
		}
	}
	return false
}

func assertOrder(t *testing.T, text string, parts ...string) {
	t.Helper()
	last := -1
	for _, part := range parts {
		idx := strings.Index(text, part)
		if idx < 0 {
			t.Fatalf("missing %q in text:\n%s", part, text)
		}
		if idx <= last {
			t.Fatalf("%q appears out of order in text:\n%s", part, text)
		}
		last = idx
	}
}

func assertSectionOrder(t *testing.T, sections []PromptSection, names ...string) {
	t.Helper()
	last := -1
	for _, name := range names {
		idx := -1
		for i, section := range sections {
			if section.Name == name {
				idx = i
				break
			}
		}
		if idx < 0 {
			t.Fatalf("missing section %q in %#v", name, sections)
		}
		if idx <= last {
			t.Fatalf("section %q appears out of order in %#v", name, sections)
		}
		last = idx
	}
}

func debugFileLoaded(files []BootstrapFileDebug, name string) bool {
	for _, file := range files {
		if file.Name == name {
			return file.Loaded
		}
	}
	return false
}

func hasSkillStatus(debug []SkillDebug, name, reason string) bool {
	for _, item := range debug {
		if item.Name == name && item.Reason == reason {
			return true
		}
	}
	return false
}

type failingEmbedder struct{}

func (failingEmbedder) Embed(ctx context.Context, text string) ([]float64, error) {
	return nil, fmt.Errorf("boom")
}

type selectiveFailEmbedder struct {
	failText string
	calls    []string
}

func (e *selectiveFailEmbedder) Embed(ctx context.Context, text string) ([]float64, error) {
	e.calls = append(e.calls, text)
	if strings.Contains(text, e.failText) {
		return nil, fmt.Errorf("boom")
	}
	return []float64{1, 0}, nil
}

func assertScoredMemoryEqual(t *testing.T, got, want []scoredMemory) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("scored len = %d, want %d; got=%#v want=%#v", len(got), len(want), got, want)
	}
	for i := range got {
		if got[i].Chunk != want[i].Chunk || got[i].Score != want[i].Score {
			t.Fatalf("scored[%d] = %#v, want %#v", i, got[i], want[i])
		}
	}
}
