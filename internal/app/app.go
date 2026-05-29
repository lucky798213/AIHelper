package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"AIHelper/internal/agent"
	"AIHelper/internal/channels"
	"AIHelper/internal/channels/cli"
	"AIHelper/internal/channels/feishu"
	"AIHelper/internal/concurrency"
	"AIHelper/internal/config"
	"AIHelper/internal/delivery"
	"AIHelper/internal/gateway"
	"AIHelper/internal/heartbeat"
	"AIHelper/internal/intelligence"
	"AIHelper/internal/llm"
	"AIHelper/internal/resilience"
	"AIHelper/internal/sessions"
	"AIHelper/internal/tools"
)

type App struct {
	channels      *channels.Manager
	router        *gateway.Router
	runner        *agent.Runner
	out           io.Writer
	intelligence  *intelligence.Service
	debugAgentID  string
	agentIDs      map[string]struct{}
	commandQueue  *concurrency.CommandQueue
	sessionMu     sync.Mutex             //锁的是 sessionLocks 这个 map 本身。
	sessionLocks  map[string]*sync.Mutex //每一个 *sync.Mutex 锁的是 某一个具体 session 的完整 Agent 执行过程
	sessionStore  sessions.Store
	llmClient     llm.Client
	contextGuard  resilience.ContextGuard
	workspaceDir  string
	cliSessionKey string
	delivery      *delivery.Service
	heartbeat     *heartbeat.Manager
	cron          *heartbeat.CronService
}

const inboundMessageProcessingStaleAfter = 30 * time.Minute

func New(cfg config.Config, in io.Reader, out io.Writer) (*App, error) {
	sharedOut := newLockedWriter(out)

	manager, err := agent.NewManager(cfg.Agents)
	if err != nil {
		return nil, err
	}

	registry := tools.NewRegistry()
	workspaceDir, err := os.Getwd()
	if err != nil {
		return nil, err
	}

	client, err := newLLMClient(cfg.LLM)
	if err != nil {
		return nil, err
	}

	//创建 intelligenceService
	intelligenceService, err := intelligence.NewService(intelligence.ServiceConfig{
		ProjectRoot: workspaceDir, // 项目的根目录
		Agents:      cfg.Agents,   //完整 Agent 配置列表
		Embedding: intelligence.EmbeddingConfig{ //Memory 搜索时的向量化（embedding）能力
			Enabled:  cfg.Intelligence.Embedding.Enabled,
			Provider: cfg.Intelligence.Embedding.Provider,
			BaseURL:  cfg.Intelligence.Embedding.BaseURL,
			APIKey:   cfg.Intelligence.Embedding.APIKey,
			Model:    cfg.Intelligence.Embedding.Model,
		},
		Plugins: intelligence.PluginConfig{
			SkillRoots:      cfg.Intelligence.Plugins.SkillRoots,      //技能根目录
			DisabledSkills:  cfg.Intelligence.Plugins.DisabledSkills,  //被禁用的技能列表
			DisabledPlugins: cfg.Intelligence.Plugins.DisabledPlugins, //被禁用的插件列表
		},
		LLMClient: client,
	})
	if err != nil {
		return nil, err
	}

	//注册所有 tools
	if err := tools.RegisterAll(registry, tools.Dependencies{
		BaseDir:       workspaceDir,
		MemoryService: intelligenceService, //在创建memory_write, memory_search这两个 tool 时需要
		SkillService:  intelligenceService, //skill reference 和 skill command 工具需要
	}); err != nil {
		return nil, err
	}

	//将 agentID 对应上 agent 可以使用的 tools，agent 白名单
	if err := configureAgentTools(registry, cfg.Agents); err != nil {
		return nil, err
	}
	if err := validateBindings(manager, cfg.Bindings); err != nil {
		return nil, err
	}

	// 会话上下文：内存缓存 + 持久化存储。
	store, err := newSessionStore(cfg.Sessions, workspaceDir)
	if err != nil {
		return nil, err
	}

	//agent 编排层（解决如何处理信息，是先分发给子 agent 再处理，还是什么，他唯一的作用就是构建好 llm.Message，交给 client处理）
	runner := agent.NewRunner(manager, client, registry, store)

	//设置用来构建提示词的人
	runner.SetPromptBuilder(intelligenceService)

	//channel 管理
	channelManager, err := newChannelManager(cfg.Channels, in, sharedOut)
	if err != nil {
		return nil, err
	}
	deliveryService, err := newDeliveryService(cfg.Delivery, workspaceDir, channelManager.Send)
	if err != nil {
		return nil, err
	}

	commandQueue := concurrency.NewCommandQueue()
	commandQueue.GetOrCreateLane(concurrency.LaneMain, 1)
	commandQueue.GetOrCreateLane(concurrency.LaneHeartbeat, 1)
	commandQueue.GetOrCreateLane(concurrency.LaneCron, 1)

	application := &App{
		channels:     channelManager,
		router:       gateway.NewRouter(cfg.Bindings),
		runner:       runner,
		out:          sharedOut,
		intelligence: intelligenceService,
		debugAgentID: defaultDebugAgentID(cfg),
		agentIDs:     agentIDSet(cfg.Agents),
		commandQueue: commandQueue,
		sessionLocks: make(map[string]*sync.Mutex),
		sessionStore: store,
		llmClient:    client,
		contextGuard: resilience.NewContextGuard(cfg.LLM.Resilience.ContextSafeTokens, cfg.LLM.Resilience.MaxToolOutputChars),
		workspaceDir: workspaceDir,
		delivery:     deliveryService,
	}

	heartbeatManager, cronService, err := application.newBackgroundServices(cfg, workspaceDir)
	if err != nil {
		return nil, err
	}
	application.heartbeat = heartbeatManager
	application.cron = cronService

	return application, nil
}

func (a *App) Run(ctx context.Context) error {
	fmt.Fprintln(a.out, "AIHelper V1 started. Type 'exit' or 'quit' to stop.")

	//启动所有消息通道，比如 CLI、Feishu。
	//channels.Start(ctx) 会让各个 channel 开始监听外部输入，把用户消息送进 a.channels.Inbound()
	a.channels.Start(ctx)
	defer a.channels.Close(ctx)

	//启动可靠投递服务。
	//如果配置里启用了 delivery，那么 Agent 生成的回复不会直接发出去，而是先进入 delivery queue，再由后台 runner 扫描并发送。
	a.startDelivery(ctx)
	defer a.stopDelivery()

	//启动后台主动任务
	a.startBackground(ctx)
	defer a.stopBackground()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case err := <-a.channels.Errors():
			if err != nil {
				fmt.Fprintf(a.out, "Channel error: %v\n", err)
			}
		case msg := <-a.channels.Inbound():
			if msg.Text == "" {
				continue
			}
			//处理命令行来的消息
			if msg.Channel == "cli" && shouldExit(msg.Text) {
				return nil
			}
			if msg.Channel == "cli" && strings.HasPrefix(strings.TrimSpace(msg.Text), "/") {
				if err := a.handleCLICommand(ctx, msg.Text); err != nil {
					fmt.Fprintf(a.out, "Command error: %v\n", err)
				}
				continue
			}

			//处理其他平台来的消息，并且将信息返回
			if err := a.handleMessage(ctx, msg); err != nil {
				fmt.Fprintf(a.out, "Message error: %v\n", err)
			}
		}
	}
}

func (a *App) handleMessage(ctx context.Context, msg channels.InboundMessage) error {
	//将接收到的信息按照binding，给他分配一个 agent
	route, err := a.router.Resolve(ctx, msg)
	if err != nil {
		return err
	}
	if msg.Channel == "cli" && strings.TrimSpace(a.cliSessionKey) != "" {
		route.SessionKey = a.cliSessionKey
		route.PeerID = cliSessionPeerID(a.cliSessionKey)
	}

	claimed, err := a.claimInboundMessage(ctx, msg, route)
	if err != nil {
		return err
	}
	if !claimed {
		return nil
	}

	outbound, err := a.runTurn(ctx, concurrency.LaneMain, msg, route)
	if err != nil {
		if markErr := a.failInboundMessage(ctx, msg, err); markErr != nil {
			return errors.Join(err, markErr)
		}
		return err
	}
	if err := a.deliver(ctx, outbound); err != nil {
		if markErr := a.failInboundMessage(ctx, msg, err); markErr != nil {
			return errors.Join(err, markErr)
		}
		return err
	}
	if err := a.completeInboundMessage(ctx, msg); err != nil {
		return err
	}
	return nil
}

func (a *App) claimInboundMessage(ctx context.Context, msg channels.InboundMessage, route gateway.Route) (bool, error) {
	if strings.TrimSpace(msg.ID) == "" {
		return true, nil
	}
	result, err := a.sessionStore.ClaimInboundMessage(ctx, sessions.InboundReceipt{
		Channel:    msg.Channel,
		AccountID:  msg.AccountID,
		MessageID:  msg.ID,
		SessionKey: route.SessionKey,
	}, inboundMessageProcessingStaleAfter)
	if err != nil {
		return false, err
	}
	return result.Claimed, nil
}

func (a *App) completeInboundMessage(ctx context.Context, msg channels.InboundMessage) error {
	if strings.TrimSpace(msg.ID) == "" {
		return nil
	}
	return a.sessionStore.CompleteInboundMessage(ctx, msg.Channel, msg.AccountID, msg.ID)
}

func (a *App) failInboundMessage(ctx context.Context, msg channels.InboundMessage, cause error) error {
	if strings.TrimSpace(msg.ID) == "" {
		return nil
	}
	errText := ""
	if cause != nil {
		errText = cause.Error()
	}
	return a.sessionStore.FailInboundMessage(ctx, msg.Channel, msg.AccountID, msg.ID, errText)
}

func (a *App) deliver(ctx context.Context, msg channels.OutboundMessage) error {
	if a.delivery == nil {
		return a.channels.Send(ctx, msg)
	}
	return a.delivery.Enqueue(ctx, msg)
}

func (a *App) runTurn(ctx context.Context, lane string, msg channels.InboundMessage, route gateway.Route) (channels.OutboundMessage, error) {
	//把任务放进命名队列
	future := a.commandQueue.Enqueue(ctx, lane, func(taskCtx context.Context) (any, error) {
		return a.runTurnForSession(taskCtx, msg, route)
	})

	result, err := future.Result(ctx)
	if err != nil {
		return channels.OutboundMessage{}, err
	}

	outbound, ok := result.(channels.OutboundMessage)
	if !ok {
		return channels.OutboundMessage{}, fmt.Errorf("turn result has unexpected type %T", result)
	}
	return outbound, nil
}

func (a *App) runTurnForSession(ctx context.Context, msg channels.InboundMessage, route gateway.Route) (channels.OutboundMessage, error) {
	//第一，根据 route.SessionKey 获取会话锁：
	lock := a.sessionLock(route.SessionKey)

	//第二，锁住这个 session：
	lock.Lock()
	defer lock.Unlock()

	//然后调用：这才进入真正的 Agent 执行
	return a.runner.RunTurn(ctx, msg, route)
}

func (a *App) sessionLock(sessionKey string) *sync.Mutex {
	sessionKey = strings.TrimSpace(sessionKey)
	if sessionKey == "" {
		sessionKey = "default"
	}
	a.sessionMu.Lock()
	defer a.sessionMu.Unlock()
	if a.sessionLocks == nil {
		a.sessionLocks = make(map[string]*sync.Mutex)
	}
	lock, ok := a.sessionLocks[sessionKey]
	if !ok {
		lock = &sync.Mutex{}
		a.sessionLocks[sessionKey] = lock
	}
	return lock
}

func shouldExit(text string) bool {
	normalized := strings.ToLower(strings.TrimSpace(text))
	return normalized == "exit" || normalized == "quit"
}

func (a *App) handleCLICommand(ctx context.Context, text string) error {
	fields := strings.Fields(strings.TrimSpace(text))
	if len(fields) == 0 {
		return nil
	}
	command := strings.ToLower(fields[0])
	args := fields[1:]

	switch command {
	case "/help":
		a.printCLIHelp()
	case "/sessions":
		return a.printSessions(ctx)
	case "/new":
		return a.newCLISession(ctx, strings.Join(args, " "))
	case "/switch":
		if len(args) < 1 {
			fmt.Fprintln(a.out, "Usage: /switch <session>")
			return nil
		}
		return a.switchCLISession(ctx, args[0])
	case "/delete":
		if len(args) < 1 {
			fmt.Fprintln(a.out, "Usage: /delete <session>")
			return nil
		}
		return a.deleteSession(ctx, args[0])
	case "/export":
		return a.exportSession(ctx, args)
	case "/compact":
		return a.compactSession(ctx, args)
	case "/lanes":
		a.printLaneStatus()
	case "/concurrency":
		if len(args) < 2 {
			fmt.Fprintln(a.out, "Usage: /concurrency <lane> <N>")
			return nil
		}
		n, err := strconv.Atoi(args[1])
		if err != nil {
			fmt.Fprintln(a.out, "Usage: /concurrency <lane> <N>")
			return nil
		}
		a.commandQueue.SetConcurrency(args[0], n)
		fmt.Fprintf(a.out, "Lane %s max_concurrency=%d\n", strings.TrimSpace(args[0]), maxInt(1, n))
	case "/queue":
		return a.printDeliveryQueue(ctx)
	case "/failed":
		return a.printDeliveryFailed(ctx)
	case "/retry":
		return a.retryDeliveryFailed(ctx)
	case "/delivery":
		return a.printDeliveryStatus(ctx)
	case "/heartbeat":
		a.printHeartbeatStatus()
	case "/heartbeat-trigger":
		if len(args) < 1 {
			fmt.Fprintln(a.out, "Usage: /heartbeat-trigger <agent_id>")
			return nil
		}
		if a.heartbeat == nil {
			fmt.Fprintln(a.out, "Heartbeat disabled.")
			return nil
		}
		result, err := a.heartbeat.Trigger(ctx, args[0])
		if err != nil {
			if errors.Is(err, heartbeat.ErrBusy) {
				fmt.Fprintf(a.out, "Heartbeat agent=%s busy: %s\n", args[0], result.Reason)
				return nil
			}
			return err
		}
		fmt.Fprintf(a.out, "Heartbeat agent=%s status=%s reason=%s\n", result.AgentID, result.Status, result.Reason)
	case "/cron":
		a.printCronStatus()
	case "/cron-trigger":
		if len(args) < 1 {
			fmt.Fprintln(a.out, "Usage: /cron-trigger <job_id>")
			return nil
		}
		if a.cron == nil {
			fmt.Fprintln(a.out, "Cron disabled.")
			return nil
		}
		result, err := a.cron.Trigger(ctx, args[0])
		if err != nil {
			if errors.Is(err, heartbeat.ErrBusy) {
				fmt.Fprintf(a.out, "Cron job=%s busy\n", args[0])
				return nil
			}
			return err
		}
		fmt.Fprintf(a.out, "Cron job=%s status=%s error=%s\n", result.JobID, result.Status, result.Error)
	case "/prompt":
		agentID, query := a.parseOptionalAgent(args)
		debug, err := a.intelligence.DebugPrompt(ctx, agentID, query, "cli")
		if err != nil {
			return err
		}
		a.printPromptDebug(debug)
	case "/bootstrap":
		agentID, _ := a.parseOptionalAgent(args)
		debug, err := a.intelligence.BootstrapDebug(agentID)
		if err != nil {
			return err
		}
		a.printBootstrapDebug(debug)
	case "/skills":
		agentID, _ := a.parseOptionalAgent(args)
		debug, err := a.intelligence.SkillsDebug(agentID)
		if err != nil {
			return err
		}
		a.printSkillsDebug(debug)
	case "/memory":
		agentID, _ := a.parseOptionalAgent(args)
		stats, err := a.intelligence.MemoryStats(ctx, agentID)
		if err != nil {
			return err
		}
		fmt.Fprintf(a.out, "Memory agent=%s evergreen_chars=%d daily_files=%d daily_entries=%d\n",
			agentID, stats.EvergreenChars, stats.DailyFiles, stats.DailyEntries)
	case "/search":
		if len(args) < 2 || !a.knownAgent(args[0]) {
			fmt.Fprintln(a.out, "Usage: /search <agent_id> <query>")
			return nil
		}
		result, err := a.intelligence.SearchMemory(ctx, args[0], strings.Join(args[1:], " "), 5)
		if err != nil {
			return err
		}
		fmt.Fprintf(a.out, "Memory search agent=%s\n%s\n", args[0], result)
	case "/soul":
		agentID, _ := a.parseOptionalAgent(args)
		debug, err := a.intelligence.DebugPrompt(ctx, agentID, "", "cli")
		if err != nil {
			return err
		}
		for _, section := range debug.Sections {
			if section.Name == "Soul" {
				fmt.Fprintf(a.out, "%s\n", section.Content)
				return nil
			}
		}
		fmt.Fprintf(a.out, "No SOUL.md loaded for agent=%s\n", agentID)
	case "/intelligence/reload":
		if err := a.intelligence.Reload(ctx); err != nil {
			return err
		}
		fmt.Fprintln(a.out, "Intelligence cache reloaded.")
	default:
		fmt.Fprintf(a.out, "Unknown CLI command %q\n", command)
	}
	return nil
}

func (a *App) printCLIHelp() {
	fmt.Fprintln(a.out, "Commands:")
	fmt.Fprintln(a.out, "  /sessions                Show sessions")
	fmt.Fprintln(a.out, "  /new [name]              Create and switch to a CLI session")
	fmt.Fprintln(a.out, "  /switch <session>        Switch CLI session")
	fmt.Fprintln(a.out, "  /delete <session>        Delete a session")
	fmt.Fprintln(a.out, "  /export [session] [path] Export a session as JSONL")
	fmt.Fprintln(a.out, "  /compact [session]       Compact a session history")
	fmt.Fprintln(a.out, "  /lanes                    Show command lanes")
	fmt.Fprintln(a.out, "  /concurrency <lane> <N>   Set lane max concurrency")
	fmt.Fprintln(a.out, "  /queue                    Show delivery queue")
	fmt.Fprintln(a.out, "  /failed                   Show failed deliveries")
	fmt.Fprintln(a.out, "  /retry                    Retry failed deliveries")
	fmt.Fprintln(a.out, "  /delivery                 Show delivery status")
	fmt.Fprintln(a.out, "  /heartbeat                Show heartbeat status")
	fmt.Fprintln(a.out, "  /heartbeat-trigger <id>   Trigger heartbeat")
	fmt.Fprintln(a.out, "  /cron                     Show cron jobs")
	fmt.Fprintln(a.out, "  /cron-trigger <id>        Trigger cron job")
}

func (a *App) printSessions(ctx context.Context) error {
	metas, err := a.sessionStore.List(ctx)
	if err != nil {
		return err
	}
	if len(metas) == 0 {
		fmt.Fprintln(a.out, "No sessions.")
		return nil
	}
	active := a.currentCLISessionKey()
	for _, meta := range metas {
		marker := " "
		if meta.SessionKey == active {
			marker = "*"
		}
		updated := "-"
		if !meta.UpdatedAt.IsZero() {
			updated = meta.UpdatedAt.Format(time.RFC3339)
		}
		fmt.Fprintf(a.out, "%s session=%s agent=%s channel=%s peer=%s messages=%d updated=%s\n",
			marker,
			meta.SessionKey,
			valueOrDefault(meta.AgentID, "-"),
			valueOrDefault(meta.Channel, "-"),
			valueOrDefault(meta.PeerID, "-"),
			meta.MessageCount,
			updated,
		)
	}
	return nil
}

func (a *App) newCLISession(ctx context.Context, name string) error {
	sessionKey := a.newCLISessionKey(name)
	if err := a.sessionStore.Touch(ctx, sessionKey); err != nil {
		return err
	}
	a.cliSessionKey = sessionKey
	fmt.Fprintf(a.out, "Switched to session %s\n", sessionKey)
	return nil
}

func (a *App) switchCLISession(ctx context.Context, ref string) error {
	sessionKey, ok, err := a.resolveSessionRef(ctx, ref)
	if err != nil {
		return err
	}
	if !ok {
		fmt.Fprintf(a.out, "Session %q not found.\n", ref)
		return nil
	}
	a.cliSessionKey = sessionKey
	fmt.Fprintf(a.out, "Switched to session %s\n", sessionKey)
	return nil
}

func (a *App) deleteSession(ctx context.Context, ref string) error {
	sessionKey, ok, err := a.resolveSessionRef(ctx, ref)
	if err != nil {
		return err
	}
	if !ok {
		fmt.Fprintf(a.out, "Session %q not found.\n", ref)
		return nil
	}
	if err := a.sessionStore.Delete(ctx, sessionKey); err != nil {
		return err
	}
	if a.cliSessionKey == sessionKey {
		a.cliSessionKey = ""
	}
	fmt.Fprintf(a.out, "Deleted session %s\n", sessionKey)
	return nil
}

func (a *App) exportSession(ctx context.Context, args []string) error {
	sessionKey := a.currentCLISessionKey()
	path := ""
	if len(args) > 0 {
		if resolved, ok, err := a.resolveSessionRef(ctx, args[0]); err != nil {
			return err
		} else if ok {
			sessionKey = resolved
			if len(args) > 1 {
				path = args[1]
			}
		} else {
			path = args[0]
		}
	}
	if path == "" {
		path = filepath.Join("workspace", "session-exports", safeFileName(sessionKey)+".jsonl")
	}
	if !filepath.IsAbs(path) {
		path = filepath.Join(a.workspaceDir, path)
	}
	messages, err := a.sessionStore.Load(ctx, sessionKey)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	encoder := json.NewEncoder(file)
	for _, message := range messages {
		if err := encoder.Encode(message); err != nil {
			_ = file.Close()
			return err
		}
	}
	if err := file.Close(); err != nil {
		return err
	}
	fmt.Fprintf(a.out, "Exported session %s to %s (%d messages)\n", sessionKey, path, len(messages))
	return nil
}

func (a *App) compactSession(ctx context.Context, args []string) error {
	sessionKey := a.currentCLISessionKey()
	if len(args) > 0 {
		resolved, ok, err := a.resolveSessionRef(ctx, args[0])
		if err != nil {
			return err
		}
		if !ok {
			fmt.Fprintf(a.out, "Session %q not found.\n", args[0])
			return nil
		}
		sessionKey = resolved
	}
	messages, err := a.sessionStore.Load(ctx, sessionKey)
	if err != nil {
		return err
	}
	if len(messages) == 0 {
		fmt.Fprintf(a.out, "Session %s is empty.\n", sessionKey)
		return nil
	}
	compacted := a.contextGuard.CompactHistory(ctx, a.llmClient, llm.Request{
		AgentID:   a.debugAgentID,
		AgentRole: string(agent.AgentRoleMaster),
		Purpose:   "manual_compact",
		Messages:  messages,
	})
	if err := a.sessionStore.Replace(ctx, sessionKey, compacted); err != nil {
		return err
	}
	fmt.Fprintf(a.out, "Compacted session %s from %d to %d messages\n", sessionKey, len(messages), len(compacted))
	return nil
}

func (a *App) currentCLISessionKey() string {
	if strings.TrimSpace(a.cliSessionKey) != "" {
		return a.cliSessionKey
	}
	return defaultCLISessionKey(a.debugAgentID)
}

func (a *App) newCLISessionKey(name string) string {
	slug := safeSessionSlug(name)
	if slug == "" {
		slug = time.Now().UTC().Format("20060102-150405")
	}
	return fmt.Sprintf("agent:%s:cli:session:%s", valueOrDefault(a.debugAgentID, "local-master"), slug)
}

func defaultCLISessionKey(agentID string) string {
	return fmt.Sprintf("agent:%s:cli:direct:cli-user", valueOrDefault(agentID, "local-master"))
}

func (a *App) resolveSessionRef(ctx context.Context, ref string) (string, bool, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return "", false, nil
	}
	metas, err := a.sessionStore.List(ctx)
	if err != nil {
		return "", false, err
	}
	for _, meta := range metas {
		if meta.SessionKey == ref || sessionAlias(meta.SessionKey) == ref {
			return meta.SessionKey, true, nil
		}
	}
	return "", false, nil
}

func sessionAlias(sessionKey string) string {
	if idx := strings.LastIndex(sessionKey, ":session:"); idx >= 0 {
		return sessionKey[idx+len(":session:"):]
	}
	if idx := strings.LastIndex(sessionKey, ":"); idx >= 0 && idx < len(sessionKey)-1 {
		return sessionKey[idx+1:]
	}
	return sessionKey
}

func cliSessionPeerID(sessionKey string) string {
	if alias := sessionAlias(sessionKey); alias != "" && alias != "." {
		return "session:" + alias
	}
	return "cli-user"
}

func safeSessionSlug(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	var builder strings.Builder
	lastDash := false
	for _, r := range value {
		switch {
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			builder.WriteRune(unicode.ToLower(r))
			lastDash = false
		case r == '-' || r == '_' || r == '.':
			builder.WriteRune(r)
			lastDash = false
		case unicode.IsSpace(r):
			if !lastDash {
				builder.WriteRune('-')
				lastDash = true
			}
		}
	}
	return strings.Trim(builder.String(), "-_.")
}

func safeFileName(value string) string {
	name := safeSessionSlug(strings.ReplaceAll(value, ":", "-"))
	if name == "" {
		return "session"
	}
	return name
}

func (a *App) printLaneStatus() {
	for _, stat := range a.commandQueue.Stats() {
		fmt.Fprintf(a.out, "Lane %s active=%d queued=%d max=%d generation=%d\n",
			stat.Name,
			stat.Active,
			stat.QueueDepth,
			stat.MaxConcurrency,
			stat.Generation,
		)
	}
}

func (a *App) printHeartbeatStatus() {
	if a.heartbeat == nil {
		fmt.Fprintln(a.out, "Heartbeat disabled.")
		return
	}
	statuses := a.heartbeat.Statuses()
	if len(statuses) == 0 {
		fmt.Fprintln(a.out, "Heartbeat enabled, no runners configured.")
		return
	}
	for _, status := range statuses {
		activeHours := "all-day"
		if status.ActiveHours != nil {
			activeHours = fmt.Sprintf("%02d:00-%02d:00", status.ActiveHours.Start, status.ActiveHours.End)
		}
		lastRun := "-"
		if !status.LastRunAt.IsZero() {
			lastRun = status.LastRunAt.Format(time.RFC3339)
		}
		fmt.Fprintf(a.out,
			"Heartbeat agent=%s running=%t interval=%s active_hours=%s last_status=%s last_reason=%s last_run=%s target=%s:%s\n",
			status.AgentID,
			status.Running,
			status.Interval,
			activeHours,
			status.LastStatus,
			status.LastReason,
			lastRun,
			status.Target.Channel,
			status.Target.PeerID,
		)
	}
}

func (a *App) printCronStatus() {
	if a.cron == nil {
		fmt.Fprintln(a.out, "Cron disabled.")
		return
	}
	jobs := a.cron.ListJobs()
	if len(jobs) == 0 {
		fmt.Fprintln(a.out, "No cron jobs.")
		return
	}
	for _, job := range jobs {
		nextRun := "-"
		if !job.NextRunAt.IsZero() {
			nextRun = job.NextRunAt.Format(time.RFC3339)
		}
		lastRun := "-"
		if !job.LastRunAt.IsZero() {
			lastRun = job.LastRunAt.Format(time.RFC3339)
		}
		fmt.Fprintf(a.out,
			"Cron id=%s name=%s enabled=%t agent=%s kind=%s errors=%d next=%s last=%s target=%s:%s\n",
			job.ID,
			job.Name,
			job.Enabled,
			job.AgentID,
			job.ScheduleKind,
			job.ConsecutiveErrors,
			nextRun,
			lastRun,
			job.Target.Channel,
			job.Target.PeerID,
		)
	}
}

func (a *App) printDeliveryQueue(ctx context.Context) error {
	if a.delivery == nil {
		fmt.Fprintln(a.out, "Delivery disabled.")
		return nil
	}
	pending, err := a.delivery.Pending(ctx)
	if err != nil {
		return err
	}
	if len(pending) == 0 {
		fmt.Fprintln(a.out, "Delivery queue is empty.")
		return nil
	}
	now := time.Now()
	for _, item := range pending {
		wait := "-"
		if !item.NextRetryAt.IsZero() && item.NextRetryAt.After(now) {
			wait = item.NextRetryAt.Sub(now).Round(time.Second).String()
		}
		fmt.Fprintf(a.out, "Delivery pending id=%s channel=%s to=%s retry=%d wait=%s text=%q\n",
			item.ID,
			item.Channel,
			item.To,
			item.RetryCount,
			wait,
			previewText(item.Text, 60),
		)
	}
	return nil
}

func (a *App) printDeliveryFailed(ctx context.Context) error {
	if a.delivery == nil {
		fmt.Fprintln(a.out, "Delivery disabled.")
		return nil
	}
	failed, err := a.delivery.Failed(ctx)
	if err != nil {
		return err
	}
	if len(failed) == 0 {
		fmt.Fprintln(a.out, "No failed deliveries.")
		return nil
	}
	for _, item := range failed {
		fmt.Fprintf(a.out, "Delivery failed id=%s channel=%s to=%s retries=%d error=%q text=%q\n",
			item.ID,
			item.Channel,
			item.To,
			item.RetryCount,
			previewText(item.LastError, 40),
			previewText(item.Text, 60),
		)
	}
	return nil
}

func (a *App) retryDeliveryFailed(ctx context.Context) error {
	if a.delivery == nil {
		fmt.Fprintln(a.out, "Delivery disabled.")
		return nil
	}
	count, err := a.delivery.RetryFailed(ctx)
	if err != nil {
		return err
	}
	fmt.Fprintf(a.out, "Moved %d failed deliveries back to queue.\n", count)
	return nil
}

func (a *App) printDeliveryStatus(ctx context.Context) error {
	if a.delivery == nil {
		fmt.Fprintln(a.out, "Delivery disabled.")
		return nil
	}
	stats, err := a.delivery.Stats(ctx)
	if err != nil {
		return err
	}
	fmt.Fprintf(a.out, "Delivery pending=%d failed=%d attempted=%d succeeded=%d errors=%d\n",
		stats.Pending,
		stats.Failed,
		stats.TotalAttempted,
		stats.TotalSucceeded,
		stats.TotalFailed,
	)
	return nil
}

func (a *App) parseOptionalAgent(args []string) (string, string) {
	if len(args) == 0 {
		return a.debugAgentID, ""
	}
	if a.knownAgent(args[0]) {
		return args[0], strings.Join(args[1:], " ")
	}
	return a.debugAgentID, strings.Join(args, " ")
}

func (a *App) knownAgent(agentID string) bool {
	_, ok := a.agentIDs[agentID]
	return ok
}

func (a *App) printPromptDebug(debug intelligence.PromptDebug) {
	fmt.Fprintf(a.out, "Prompt agent=%s total_chars=%d\n", debug.AgentID, debug.TotalChars)
	for _, section := range debug.Sections {
		fmt.Fprintf(a.out, "- %s: %d chars\n", section.Name, section.Chars)
	}
	for _, warning := range debug.Warnings {
		fmt.Fprintf(a.out, "[warning] %s\n", warning)
	}
	fmt.Fprintf(a.out, "\n%s\n", debug.Prompt)
}

func (a *App) printBootstrapDebug(debug intelligence.BootstrapDebug) {
	fmt.Fprintf(a.out, "Bootstrap agent=%s mode=%s total_chars=%d workspace=%s loaded_at=%s\n",
		debug.AgentID,
		debug.Mode,
		debug.TotalChars,
		debug.Workspace,
		debug.LoadedAt.Format("2006-01-02 15:04:05 MST"),
	)
	for _, file := range debug.Files {
		status := "missing"
		if file.Loaded {
			status = "loaded"
		}
		fmt.Fprintf(a.out, "- %s: %s, %d chars, %s\n", file.Name, status, file.Chars, file.Path)
	}
}

func (a *App) printSkillsDebug(debug intelligence.SkillsDebug) {
	fmt.Fprintf(a.out, "Skills agent=%s workspace=%s loaded_at=%s\n",
		debug.AgentID,
		debug.Workspace,
		debug.LoadedAt.Format("2006-01-02 15:04:05 MST"),
	)
	if len(debug.Skills) == 0 {
		fmt.Fprintln(a.out, "No skills discovered.")
		return
	}
	for _, item := range debug.Skills {
		status := "active"
		if !item.Enabled {
			status = "disabled"
		} else if item.Overridden {
			status = "overridden"
		}
		reason := item.Reason
		if reason == "" {
			reason = "-"
		}
		fmt.Fprintf(a.out, "- %s: %s, plugin=%s, root=%s, reason=%s, path=%s\n",
			item.Name,
			status,
			item.Plugin,
			item.SourceRoot,
			reason,
			item.Path,
		)
	}
}

func newLLMClient(cfg config.LLMConfig) (llm.Client, error) {
	provider := strings.ToLower(strings.TrimSpace(cfg.Provider))
	if provider == "" {
		provider = "mock"
	}

	switch provider {
	case "mock":
		return llm.NewMockClient(), nil
	case "openai_compatible":
		if cfg.Resilience.Enabled {
			return newResilientOpenAIClient(cfg, provider)
		}
		return llm.NewOpenAIClient(llm.OpenAIConfig{
			BaseURL:      cfg.BaseURL,
			APIKey:       cfg.APIKey,
			DefaultModel: cfg.DefaultModel,
			Temperature:  cfg.Temperature,
			MaxTokens:    cfg.MaxTokens,
		})
	default:
		return nil, fmt.Errorf("unsupported llm provider %q", cfg.Provider)
	}
}

// 创建一个带韧性能力的 OpenAI 客户端，也就是普通 OpenAIClient 外面再包一层 ResilientClient。
// 多个 API Key 怎么轮换
// 失败时怎么切换 profile
// 上下文溢出时怎么压缩后重试
// 主模型失败时怎么 fallback 到备用模型
func newResilientOpenAIClient(cfg config.LLMConfig, provider string) (llm.Client, error) {
	//从配置里解析多个认证 profile
	profiles, err := resilienceProfilesFromConfig(cfg, provider)
	if err != nil {
		return nil, err
	}

	//创建 profile manager
	manager, err := resilience.NewProfileManager(profiles)
	if err != nil {
		return nil, err
	}

	//定义 factory，这个 factory 的作用是：给我一个 profile，我就创建一个真正的 OpenAIClient。
	factory := func(profile resilience.AuthProfile) (llm.Client, error) {
		profileProvider := strings.ToLower(strings.TrimSpace(profile.Provider))
		if profileProvider == "" {
			profileProvider = provider
		}
		if profileProvider != "openai_compatible" {
			return nil, fmt.Errorf("unsupported resilience profile provider %q", profile.Provider)
		}
		baseURL := strings.TrimSpace(profile.BaseURL)
		if baseURL == "" {
			baseURL = cfg.BaseURL
		}

		//创建 ResilientClient：
		return llm.NewOpenAIClient(llm.OpenAIConfig{
			BaseURL:      baseURL,
			APIKey:       profile.APIKey,
			DefaultModel: cfg.DefaultModel,
			Temperature:  cfg.Temperature,
			MaxTokens:    cfg.MaxTokens,
		})
	}

	return resilience.NewClient(resilience.ClientConfig{
		Profiles:               manager,
		ClientFactory:          factory,
		FallbackModels:         normalizedStrings(cfg.FallbackModels),
		ContextGuard:           resilience.NewContextGuard(cfg.Resilience.ContextSafeTokens, cfg.Resilience.MaxToolOutputChars),
		MaxOverflowCompactions: cfg.Resilience.MaxOverflowCompactions,
	})
}

func resilienceProfilesFromConfig(cfg config.LLMConfig, provider string) ([]resilience.AuthProfile, error) {
	if len(cfg.Profiles) == 0 {
		apiKey := strings.TrimSpace(cfg.APIKey)
		if apiKey == "" {
			return nil, fmt.Errorf("openai-compatible api_key is required")
		}
		return []resilience.AuthProfile{{
			Name:     "main-key",
			Provider: provider,
			BaseURL:  strings.TrimSpace(cfg.BaseURL),
			APIKey:   apiKey,
		}}, nil
	}

	profiles := make([]resilience.AuthProfile, 0, len(cfg.Profiles))
	for i, item := range cfg.Profiles {
		apiKey := strings.TrimSpace(item.APIKey)
		if apiKey == "" {
			return nil, fmt.Errorf("llm profile %d api_key is required", i+1)
		}
		profileProvider := strings.ToLower(strings.TrimSpace(item.Provider))
		if profileProvider == "" {
			profileProvider = provider
		}
		if profileProvider != provider {
			return nil, fmt.Errorf("llm profile %d provider %q does not match llm.provider %q", i+1, item.Provider, provider)
		}
		profiles = append(profiles, resilience.AuthProfile{
			Name:     strings.TrimSpace(item.Name),
			Provider: profileProvider,
			BaseURL:  strings.TrimSpace(item.BaseURL),
			APIKey:   apiKey,
		})
	}
	return profiles, nil
}

func normalizedStrings(values []string) []string {
	result := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value != "" {
			result = append(result, value)
		}
	}
	return result
}

func newSessionStore(cfg config.SessionsConfig, workspaceDir string) (sessions.Store, error) {
	//读取配置，决定用什么存储
	driver := strings.ToLower(strings.TrimSpace(cfg.Driver))
	if driver == "" {
		driver = "sqlite"
	}

	//根据 driver 分流
	switch driver {
	case "memory":
		return sessions.NewMemoryStore(), nil
	case "sqlite":
		//决定数据库文件放哪里
		path := strings.TrimSpace(cfg.Path)
		if path == "" {
			path = filepath.Join(workspaceDir, "workspace", "aihelper.db")
		}
		if !filepath.IsAbs(path) {
			path = filepath.Join(workspaceDir, path)
		}

		//创建 SQLite 持久化 store
		disk, err := sessions.NewSQLiteStore(path)
		if err != nil {
			return nil, err
		}

		//返回CachedStore，他包含MemoryStore和SQLiteStore
		return sessions.NewCachedStore(sessions.NewMemoryStore(), disk), nil
	default:
		return nil, fmt.Errorf("unsupported sessions driver %q", cfg.Driver)
	}
}

func newDeliveryService(cfg config.DeliveryConfig, workspaceDir string, sender delivery.Sender) (*delivery.Service, error) {
	if cfg.Enabled != nil && !*cfg.Enabled {
		return nil, nil
	}
	path := strings.TrimSpace(cfg.Path)
	if path == "" {
		path = filepath.Join(workspaceDir, "workspace", "delivery-queue")
	}
	if !filepath.IsAbs(path) {
		path = filepath.Join(workspaceDir, path)
	}
	outbox, err := delivery.NewFileOutbox(delivery.FileOutboxConfig{
		Dir:        filepath.Clean(path),
		MaxRetries: cfg.MaxRetries,
	})
	if err != nil {
		return nil, err
	}
	return delivery.NewService(delivery.ServiceConfig{
		Outbox:       outbox,
		Sender:       sender,
		ScanInterval: durationFromSeconds(cfg.ScanIntervalSeconds, delivery.DefaultScanInterval),
	})
}

func defaultDebugAgentID(cfg config.Config) string {
	for _, binding := range cfg.Bindings {
		if strings.TrimSpace(binding.AgentID) != "" {
			return binding.AgentID
		}
	}
	for _, agentCfg := range cfg.Agents {
		if agentCfg.Role == agent.AgentRoleMaster && strings.TrimSpace(agentCfg.ID) != "" {
			return agentCfg.ID
		}
	}
	if len(cfg.Agents) > 0 {
		return cfg.Agents[0].ID
	}
	return ""
}

func agentIDSet(agents []agent.AgentConfig) map[string]struct{} {
	set := make(map[string]struct{}, len(agents))
	for _, cfg := range agents {
		if strings.TrimSpace(cfg.ID) != "" {
			set[cfg.ID] = struct{}{}
		}
	}
	return set
}

func newChannelManager(cfg config.ChannelsConfig, in io.Reader, out io.Writer) (*channels.Manager, error) {
	manager := channels.NewManager(128)
	if cfg.CLI.Enabled {
		if err := manager.Register(cli.New(in, out)); err != nil {
			return nil, err
		}
	}
	if cfg.Feishu.Enabled {
		feishuCfg := feishu.Config{
			Enabled:        cfg.Feishu.Enabled,
			AccountID:      cfg.Feishu.AccountID,
			AppID:          cfg.Feishu.AppID,
			AppSecret:      cfg.Feishu.AppSecret,
			BotOpenID:      cfg.Feishu.BotOpenID,
			RequireMention: cfg.Feishu.RequireMention,
			IsLark:         cfg.Feishu.IsLark,
		}
		if strings.TrimSpace(feishuCfg.AppID) == "" || strings.TrimSpace(feishuCfg.AppSecret) == "" {
			return nil, fmt.Errorf("feishu channel requires app_id and app_secret when enabled")
		}
		ch, err := feishu.New(feishuCfg, feishu.NewSDKClient(feishuCfg), feishu.NewSDKSender(feishuCfg))
		if err != nil {
			return nil, err
		}
		if err := manager.Register(ch); err != nil {
			return nil, err
		}
	}
	return manager, nil
}

func configureAgentTools(registry *tools.Registry, agents []agent.AgentConfig) error {
	for _, cfg := range agents {
		toolNames := append([]string(nil), cfg.Tools...)
		if agentUsesFullIntelligence(cfg.Intelligence) {
			toolNames = appendUnique(toolNames, "read_skill_reference", "run_skill_command")
		}
		if err := registry.SetAllowedTools(cfg.ID, toolNames); err != nil {
			return err
		}
	}
	return nil
}

func agentUsesFullIntelligence(cfg agent.IntelligenceConfig) bool {
	if cfg.Enabled != nil && !*cfg.Enabled {
		return false
	}
	mode := strings.ToLower(strings.TrimSpace(cfg.PromptMode))
	return mode == "" || mode == "full"
}

func appendUnique(values []string, extras ...string) []string {
	seen := make(map[string]struct{}, len(values)+len(extras))
	for _, value := range values {
		seen[value] = struct{}{}
	}
	for _, extra := range extras {
		if _, ok := seen[extra]; ok {
			continue
		}
		values = append(values, extra)
		seen[extra] = struct{}{}
	}
	return values
}

// 判断每一条 binding 指向的 AgentID：
// 1. 必须存在
// 2. 必须是 master agent
func validateBindings(manager *agent.Manager, bindings []gateway.Binding) error {
	for _, binding := range bindings {
		cfg, ok := manager.Get(binding.AgentID)
		if !ok {
			return fmt.Errorf("binding references unknown agent %q", binding.AgentID)
		}
		if cfg.Role != agent.AgentRoleMaster {
			return fmt.Errorf("binding agent %q must be a master agent", binding.AgentID)
		}
	}
	return nil
}

func (a *App) startBackground(ctx context.Context) {
	if a.heartbeat != nil {
		a.heartbeat.Start(ctx)
	}
	if a.cron != nil {
		a.cron.Start(ctx)
	}
}

func (a *App) stopBackground() {
	if a.cron != nil {
		a.cron.Stop()
	}
	if a.heartbeat != nil {
		a.heartbeat.Stop()
	}
}

func (a *App) startDelivery(ctx context.Context) {
	if a.delivery != nil {
		a.delivery.Start(ctx)
	}
}

func (a *App) stopDelivery() {
	if a.delivery != nil {
		a.delivery.Stop()
	}
}

func (a *App) newBackgroundServices(cfg config.Config, workspaceDir string) (*heartbeat.Manager, *heartbeat.CronService, error) {
	heartbeatManager, err := a.newHeartbeatManager(cfg, workspaceDir)
	if err != nil {
		return nil, nil, err
	}
	cronService, err := a.newCronService(cfg, workspaceDir)
	if err != nil {
		return nil, nil, err
	}
	return heartbeatManager, cronService, nil
}

func (a *App) newHeartbeatManager(cfg config.Config, workspaceDir string) (*heartbeat.Manager, error) {
	if !cfg.Heartbeat.Enabled {
		return nil, nil
	}
	runners := make([]*heartbeat.Runner, 0, len(cfg.Heartbeat.Agents))
	for _, agentCfg := range cfg.Heartbeat.Agents {
		agentID := strings.TrimSpace(agentCfg.AgentID)
		if agentID == "" {
			return nil, fmt.Errorf("heartbeat agent_id is required")
		}
		if !a.knownAgent(agentID) {
			return nil, fmt.Errorf("heartbeat references unknown agent %q", agentID)
		}
		interval := durationFromSeconds(cfg.Heartbeat.IntervalSeconds, 15*time.Minute)
		if agentCfg.IntervalSeconds > 0 {
			interval = durationFromSeconds(agentCfg.IntervalSeconds, interval)
		}
		activeHours, err := activeHoursFromConfig(cfg.Heartbeat.ActiveHours, agentCfg.ActiveHours)
		if err != nil {
			return nil, fmt.Errorf("heartbeat agent %q active_hours: %w", agentID, err)
		}
		runner, err := heartbeat.NewRunner(heartbeat.RunnerConfig{
			AgentID:      agentID,
			WorkspaceDir: agentWorkspaceDir(workspaceDir, cfg.Agents, agentID),
			Target:       heartbeatTarget(agentCfg.Target),
			Interval:     interval,
			ActiveHours:  activeHours,
			AgentTurn:    a.runBackgroundAgentTurn,
			Send:         a.sendBackground,
		})
		if err != nil {
			return nil, fmt.Errorf("create heartbeat runner for agent %q: %w", agentID, err)
		}
		runners = append(runners, runner)
	}
	return heartbeat.NewManager(runners)
}

func (a *App) newCronService(cfg config.Config, workspaceDir string) (*heartbeat.CronService, error) {
	if !cfg.Cron.Enabled {
		return nil, nil
	}
	path := strings.TrimSpace(cfg.Cron.Path)
	if path == "" {
		path = filepath.Join(workspaceDir, "workspace", "CRON.json")
	}
	if !filepath.IsAbs(path) {
		path = filepath.Join(workspaceDir, path)
	}
	cronService, err := heartbeat.NewCronService(heartbeat.CronServiceConfig{
		Path:      filepath.Clean(path),
		LogPath:   filepath.Join(workspaceDir, "workspace", "logs", "cron-runs.jsonl"),
		AgentTurn: a.runBackgroundAgentTurn,
		Send:      a.sendBackground,
	})
	if err != nil {
		return nil, err
	}
	return cronService, nil
}

func (a *App) runBackgroundAgentTurn(ctx context.Context, task heartbeat.Task) (string, error) {
	if err := task.Target.Validate(); err != nil {
		return "", err
	}
	if !a.knownAgent(task.AgentID) {
		return "", fmt.Errorf("background task references unknown agent %q", task.AgentID)
	}
	msg := channels.InboundMessage{
		ID:          task.ID,
		Text:        task.Message,
		Channel:     task.Target.Channel,
		AccountID:   "background",
		PeerID:      task.Target.PeerID,
		SenderID:    task.Source,
		ReplyToType: task.Target.ToType,
	}
	route := gateway.Route{
		AgentID:    task.AgentID,
		SessionKey: backgroundSessionKey(task.AgentID, task.Target),
		Channel:    task.Target.Channel,
		PeerID:     task.Target.PeerID,
	}
	outbound, err := a.runTurn(ctx, laneForBackgroundSource(task.Source), msg, route)
	if err != nil {
		return "", err
	}
	return outbound.Text, nil
}

func laneForBackgroundSource(source string) string {
	switch strings.ToLower(strings.TrimSpace(source)) {
	case concurrency.LaneHeartbeat:
		return concurrency.LaneHeartbeat
	case concurrency.LaneCron:
		return concurrency.LaneCron
	default:
		return "background"
	}
}

func (a *App) sendBackground(ctx context.Context, target heartbeat.Target, text string) error {
	if err := target.Validate(); err != nil {
		return err
	}
	return a.deliver(ctx, channels.OutboundMessage{
		Channel: target.Channel,
		To:      target.PeerID,
		ToType:  target.ToType,
		Text:    text,
	})
}

func backgroundSessionKey(agentID string, target heartbeat.Target) string {
	channel := strings.TrimSpace(target.Channel)
	if channel == "" {
		channel = "background"
	}
	peerID := strings.TrimSpace(target.PeerID)
	if peerID == "" {
		peerID = "main"
	}
	return fmt.Sprintf("agent:%s:%s:direct:%s", strings.TrimSpace(agentID), channel, peerID)
}

func heartbeatTarget(cfg config.TargetConfig) heartbeat.Target {
	return heartbeat.Target{
		Channel: strings.TrimSpace(cfg.Channel),
		PeerID:  strings.TrimSpace(cfg.PeerID),
		ToType:  strings.TrimSpace(cfg.ToType),
	}
}

func durationFromSeconds(seconds int, fallback time.Duration) time.Duration {
	if seconds <= 0 {
		return fallback
	}
	return time.Duration(seconds) * time.Second
}

func previewText(text string, limit int) string {
	if limit <= 0 {
		return ""
	}
	runes := []rune(strings.ReplaceAll(text, "\n", " "))
	if len(runes) <= limit {
		return string(runes)
	}
	return string(runes[:limit])
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func valueOrDefault(value, fallback string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return fallback
	}
	return value
}

func activeHoursFromConfig(global, local config.ActiveHoursConfig) (*heartbeat.ActiveHours, error) {
	selected := global
	if local.Start != nil || local.End != nil {
		selected = local
	}
	if selected.Start == nil && selected.End == nil {
		return nil, nil
	}
	if selected.Start == nil || selected.End == nil {
		return nil, fmt.Errorf("both start and end are required")
	}
	activeHours := heartbeat.ActiveHours{
		Start: *selected.Start,
		End:   *selected.End,
	}
	if err := activeHours.Validate(); err != nil {
		return nil, err
	}
	return &activeHours, nil
}

func agentWorkspaceDir(projectRoot string, agents []agent.AgentConfig, agentID string) string {
	for _, cfg := range agents {
		if cfg.ID != agentID {
			continue
		}
		workspace := strings.TrimSpace(cfg.Intelligence.Workspace)
		if workspace == "" {
			return filepath.Join(projectRoot, "workspace", "agents", cfg.ID)
		}
		if filepath.IsAbs(workspace) {
			return filepath.Clean(workspace)
		}
		return filepath.Clean(filepath.Join(projectRoot, workspace))
	}
	return filepath.Join(projectRoot, "workspace", "agents", agentID)
}

type lockedWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func newLockedWriter(w io.Writer) io.Writer {
	if w == nil {
		w = io.Discard
	}
	return &lockedWriter{w: w}
}

func (w *lockedWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.w.Write(p)
}
