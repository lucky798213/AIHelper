# claw0 Go 学习文档：AIHelper 如何实现 10 个章节

生成日期：2026-05-23

本文按照 `claw0` 教程的 10 个章节，梳理 AIHelper 这个 Go 项目如何把教程里的教学型 Python 单文件实现，落成一个按包拆分的 Go 工程。阅读时建议一边打开 `claw0/sessions/zh/*.md`，一边对照本文列出的 Go 代码入口。

## 如何阅读本项目

`claw0` 的每一章都是一份可运行的 Python 文件：后一章保留前一章的核心代码，再添加一个新机制。AIHelper 没有照搬这种“每章一个大文件”的写法，而是把 10 个概念拆进稳定的 Go 包：

| claw0 章节 | 教程主题 | AIHelper 对应包 |
|---|---|---|
| s01 | Agent Loop | `internal/agent`, `internal/llm`, `internal/app` |
| s02 | Tool Use | `internal/tools`, `internal/agent` |
| s03 | Sessions & Context | `internal/sessions`, `internal/resilience`, `internal/app` |
| s04 | Channels | `internal/channels`, `internal/channels/cli`, `internal/channels/feishu` |
| s05 | Gateway & Routing | `internal/gateway`, `configs/dev.yaml` |
| s06 | Intelligence | `internal/intelligence`, `workspace/agents/*` |
| s07 | Heartbeat & Cron | `internal/heartbeat`, `internal/app` |
| s08 | Delivery | `internal/delivery`, `internal/app` |
| s09 | Resilience | `internal/resilience`, `internal/llm`, `internal/app` |
| s10 | Concurrency | `internal/concurrency`, `internal/app` |

真实运行链路可以先记住这一条：

```txt
cmd/aihelper/main.go
  -> config.Load
  -> app.New
  -> App.Run
  -> channels.Manager.Start
  -> gateway.Router.Resolve
  -> app.runTurn(lane=main)
  -> agent.Runner.RunTurn
  -> sessions.Store.Load/Append
  -> llm.Client.CreateMessage
  -> tools.Registry.GetForAgent
  -> delivery.Service.Enqueue
```

当前项目相对 claw0 的总差异：

- `claw0` 是教学仓库，重点展示每章的最小机制；AIHelper 是工程化拆包，重点是包边界和可测试性。
- `claw0` 主要使用 Anthropic 风格的 message/tool loop；AIHelper 当前走 OpenAI-compatible chat/completions，内部用 `llm.Client` 接口隔离。
- `claw0` 的 s03 用 JSONL 会话持久化；AIHelper 当前主存储是 SQLite，并包了一层内存缓存，同时 CLI 支持 JSONL 导出。
- AIHelper 增加了 master/specialist 多 Agent 分发，这不是 claw0 章节的必需内容，但它复用了同一个 Agent Loop、Session、Tool、Gateway 体系。

## s01 Agent Loop

### claw0 本章讲什么

s01 的重点是最小 Agent 循环：用户输入进入 `messages`，调用 LLM，然后根据 `stop_reason` 决定是结束回合还是继续。教程想让你看到：Agent 的核心不是复杂框架，而是一个不断把模型输出反馈进上下文的循环。

### AIHelper 如何实现

AIHelper 把这个循环拆成三层：

- `cmd/aihelper/main.go` 只负责加载配置并启动应用。
- `internal/app` 负责外部事件循环：接收 channel 消息、路由、排队、投递。
- `internal/agent` 负责真正的一轮 Agent 推理，包括历史加载、用户消息写入、LLM 调用、工具循环和最终 assistant 消息保存。

核心循环在 `Runner.RunTurn` 和 `Runner.runAgent` 里。`RunTurn` 是一轮用户消息入口；`runAgent` 是模型循环，最多执行 5 次工具往返。`llm.StopReasonEndTurn` 表示最终回答，`llm.StopReasonToolUse` 表示进入工具分发。

AIHelper 还在回答前加入了 master dispatch：主控 Agent 先用 `dispatch` 判断是自己回答，还是把任务委托给 specialist Agent。这个设计是在 claw0 的 loop 外面加了一层多 Agent 编排，但底层仍然是同一套 `runAgent` 循环。

### 代码入口

- `cmd/aihelper/main.go:13`：程序入口，加载 `configs/dev.yaml` 并调用 `app.New` / `App.Run`。
- `internal/app/app.go:164`：`App.Run` 的外层事件循环。
- `internal/app/app.go:204`：`handleMessage` 将 inbound message 路由到 Agent。
- `internal/agent/runner.go:43`：`Runner.RunTurn`，一轮 Agent 执行入口。
- `internal/agent/runner.go:190`：`Runner.runAgent`，处理 `end_turn` / `tool_use` 的核心循环。
- `internal/llm/types.go`：统一定义 `Message`、`Request`、`Response`、`StopReason`。

### 测试入口

- `internal/agent/runner_test.go:192`：`TestRunnerDirectAnswerUsesMasterAgent` 验证 direct 回答路径。
- `internal/agent/runner_test.go:249`：`TestRunnerPassesReasoningContentBackAfterToolCall` 覆盖工具调用后的返回。
- `internal/llm/openai_test.go:12`：`TestOpenAIClientBuildsChatCompletionRequest` 验证 OpenAI-compatible 请求构造。

### 与 claw0 的差异

`claw0` 的 s01 是纯 CLI 单文件循环；AIHelper 的 loop 被包在 `app`、`agent`、`llm` 三个层次里。这样做牺牲了一点入门直观性，但换来后续通道、路由、后台任务和可靠投递都能复用同一个核心 Agent Runner。

### 学习重点

先读 `Runner.RunTurn`，再读 `Runner.runAgent`。理解这两个函数后，再看外层 `App.Run`，你会发现整个项目只是不断把不同来源的消息统一成同一种 Agent Turn。

## s02 Tool Use

### claw0 本章讲什么

s02 引入工具调用：工具不是魔法，而是两张表。一张是给模型看的 schema，另一张是程序执行 handler 的分发表。模型只决定“调用哪个工具、传什么参数”，真正执行仍由程序完成。

### AIHelper 如何实现

AIHelper 用 Go 接口把“schema + handler”合成一个 `Tool`：

- `Tool.Schema()` 给 LLM 暴露工具描述。
- `Tool.Call()` 执行真实逻辑。
- `Registry` 保存全局工具表，同时维护每个 Agent 的工具白名单。

`Runner.runAgent` 每次调用模型时，会通过 `registry.SchemasForAgent(cfg.ID)` 只传入当前 Agent 允许使用的工具。模型返回 `tool_use` 后，再用 `registry.GetForAgent(cfg.ID, toolCall.Name)` 做二次检查，避免模型调用未授权工具。

当前内置工具分两类：

- 文件工具：`list_files`、`read_file`、`write_file`、`edit_file`。
- 记忆工具：`memory_write`、`memory_search`。

文件工具有路径保护：只允许相对路径，拒绝逃逸 workspace 和 symlink 目标。记忆工具通过 `MemoryService` 接口调用 `internal/intelligence`，所以工具层不直接知道记忆的存储细节。

### 代码入口

- `internal/tools/tool.go:19`：`Tool` 接口。
- `internal/tools/registry.go:20`：`Registry` 的全局工具表和 Agent 白名单。
- `internal/tools/registry.go:94`：`GetForAgent`，按 Agent 校验工具可用性。
- `internal/tools/registry.go:109`：`SchemasForAgent`，生成当前 Agent 可见的工具 schema。
- `internal/tools/files.go:148`：`WriteFileTool.Call`，文件写入工具入口。
- `internal/tools/memory.go:11`：`MemoryService`，记忆工具与智能层之间的接口。

### 测试入口

- `internal/tools/registry_test.go:31`：`TestRegistryAgentScopedTools` 验证工具白名单。
- `internal/tools/files_test.go:46`：`TestFileToolsRejectPathEscape` 验证路径逃逸保护。
- `internal/tools/files_test.go:61`：`TestFileToolsRejectSymlinkTarget` 验证 symlink 防护。
- `internal/tools/memory_test.go:22`：`TestMemoryToolsCallService` 验证记忆工具调用服务。
- `internal/agent/runner_test.go:249`：覆盖 Agent 工具调用循环。

### 与 claw0 的差异

`claw0` 用 Python dict 保存 schema 和 handler；AIHelper 用接口和注册表实现同一思想，并加了 Agent 级白名单。Go 版本更啰嗦，但边界更清楚：工具新增时只注册工具，不改 Agent Loop。

### 学习重点

工具系统的核心不是工具本身，而是权限边界：模型能看到什么 schema，就只能调用什么 handler。读代码时重点看 `SchemasForAgent` 和 `GetForAgent` 如何分别守住“展示”和“执行”两道门。

## s03 Sessions & Context

### claw0 本章讲什么

s03 引入会话持久化和上下文保护。教程中使用 JSONL：每条 user、assistant、tool_use、tool_result 都追加写入，重启时 replay 成 LLM messages。上下文太长时，再做截断或压缩。

### AIHelper 如何实现

AIHelper 把会话抽象成 `sessions.Store`，当前提供两种实现：

- `MemoryStore`：内存存储，适合测试。
- `SQLiteStore`：持久化存储，默认用于开发配置。

`NewCachedStore` 把 SQLite 和内存缓存组合起来：首次读取从 SQLite 加载，后续读取走内存；追加写入时先写 disk，再写 cache。这一点和 s08 的“先落盘再操作”思想一致。

CLI 侧提供了会话管理命令：

- `/sessions` 查看会话。
- `/new` 新建并切换 CLI 会话。
- `/switch` 切换会话。
- `/delete` 删除会话。
- `/export` 导出为 JSONL。
- `/compact` 调用 `ContextGuard.CompactHistory` 压缩历史。

### 代码入口

- `internal/sessions/memory.go:13`：`Store` 接口。
- `internal/sessions/sqlite.go:30`：`NewSQLiteStore` 初始化 SQLite 表。
- `internal/sessions/sqlite.go:61`：`SQLiteStore.Load` 读取并重建 messages。
- `internal/sessions/sqlite.go:109`：`SQLiteStore.Append` 追加消息。
- `internal/sessions/cached.go:14`：`NewCachedStore` 组合内存缓存和磁盘存储。
- `internal/app/app.go:525`：`exportSession` 将当前会话导出为 JSONL。
- `internal/app/app.go:571`：`compactSession` 手动压缩会话历史。

### 测试入口

- `internal/sessions/memory_test.go:76`：`TestSQLiteStorePersistsMessagesAndToolCalls` 验证 SQLite 持久化 tool calls。
- `internal/sessions/memory_test.go:131`：`TestSQLiteStoreRecordsSessionMetadata` 验证 session metadata。
- `internal/sessions/memory_test.go:278`：`TestCachedStoreLoadsFromDiskAndBackfillsMemory` 验证缓存回填。
- `internal/app/app_test.go:534`：`TestCLISessionCommands` 验证 CLI 会话命令。
- `internal/app/app_test.go:643`：`TestCLICompactSessionCommand` 验证手动 compact。

### 与 claw0 的差异

`claw0` 主存储是 JSONL；AIHelper 主存储是 SQLite。AIHelper 仍保留 `/export` 输出 JSONL，方便审计和学习，但运行期不是通过 JSONL replay。上下文压缩也被放进 `internal/resilience.ContextGuard`，既能被自动失败恢复使用，也能被 CLI 命令手动触发。

### 学习重点

读 `Store` 接口时要把它当成 Agent Loop 和存储实现之间的契约。`Runner.RunTurn` 不关心消息存在内存、SQLite 还是未来的 Postgres，它只依赖 `Load`、`Append`、`Replace`。

## s04 Channels

### claw0 本章讲什么

s04 解决多平台接入问题：Telegram、飞书、CLI 等平台 payload 都不一样，但进入 Agent 前应该统一成 `InboundMessage`。Agent 核心不应该知道平台原始字段。

### AIHelper 如何实现

AIHelper 定义了统一的 channel 模型：

- `InboundMessage`：所有平台消息的内部格式。
- `OutboundMessage`：所有平台回复的内部格式。
- `Channel` 接口：平台适配器需要实现 `Start`、`Send`、`Close`。
- `Manager`：统一启动多个 channel，合并 inbound 消息，按 channel 名称发送 outbound。

当前已经有两个 channel：

- CLI：从终端读取输入，输出 `Assistant > ...`。
- Feishu：通过 SDK 接收飞书事件，把 text/post/image 消息解析成统一结构。

### 代码入口

- `internal/channels/message.go:5`：`InboundMessage`。
- `internal/channels/channel.go:5`：`Channel` 接口。
- `internal/channels/manager.go:27`：`Manager.Register` 注册 channel。
- `internal/channels/manager.go:40`：`Manager.Start` 启动所有 channel。
- `internal/channels/manager.go:57`：`Manager.Send` 按 channel 名称发送。
- `internal/channels/cli/channel.go:22`：CLI channel 构造。
- `internal/channels/feishu/channel.go:61`：`parseEvent` 将飞书事件转为 `InboundMessage`。

### 测试入口

- `internal/channels/manager_test.go:36`：`TestManagerMergesInboundMessages` 验证 inbound 合并。
- `internal/channels/manager_test.go:56`：`TestManagerSendRoutesByChannel` 验证 outbound 路由。
- `internal/channels/feishu/channel_test.go:40`：`TestFeishuParsesP2PTextMessage` 验证私聊文本。
- `internal/channels/feishu/channel_test.go:64`：`TestFeishuRequiresMentionInGroup` 验证群聊 mention 过滤。
- `internal/channels/feishu/channel_test.go:115`：`TestFeishuParsesImageMessage` 验证图片消息解析。

### 与 claw0 的差异

`claw0` 的 s04 教学实现覆盖 Telegram 和飞书概念；AIHelper 当前实现了 CLI 和 Feishu，没有 Telegram。抽象层一致：平台适配器只负责把平台消息转成统一的 `InboundMessage`，Agent 和 Gateway 不依赖平台 SDK。

### 学习重点

看 `InboundMessage` 的字段就能理解后面路由为什么可行：`Channel`、`AccountID`、`PeerID`、`SenderID` 是 s05 routing 的输入，也是会话隔离的基础。

## s05 Gateway & Routing

### claw0 本章讲什么

s05 引入网关路由：同一个系统可能接入多个平台、多个账号、多个群或私聊，需要把 `(channel, account, peer)` 映射到正确 Agent，并生成隔离的 session key。

### AIHelper 如何实现

AIHelper 的 `gateway.Router` 接收一组 bindings，先按 `tier` 升序、同 tier 下按 `priority` 降序排序，然后从最具体规则开始匹配。匹配成功后返回 `Route`：

- `AgentID`：本轮消息交给哪个 master Agent。
- `SessionKey`：这一段对话的存储 key。
- `Channel` / `PeerID`：回复时需要的目标信息。

当前支持的 match key：

- `peer_id`
- `account_id`
- `channel`
- `default`

`DMScope` 控制会话隔离方式：

- `main`
- `per-peer`
- `per-channel-peer`
- `per-account-channel-peer`

开发配置里默认把所有消息路由到 `local-master`，会话隔离为 `per-channel-peer`。

### 代码入口

- `internal/gateway/types.go:3`：`Binding`。
- `internal/gateway/types.go:12`：`Route`。
- `internal/gateway/router.go:19`：`NewRouter` 对 bindings 排序。
- `internal/gateway/router.go:30`：`Router.Resolve` 匹配 inbound message。
- `internal/gateway/router.go:50`：`matches` 判断 binding 是否命中。
- `internal/gateway/router.go:66`：`buildSessionKey` 构造会话 key。
- `configs/dev.yaml:105`：默认 bindings 配置。

### 测试入口

- `internal/gateway/router_test.go:10`：`TestRouterDefaultBinding` 验证 default binding。
- `internal/gateway/router_test.go:35`：`TestRouterDMScopeSessionKeys` 验证不同 `dm_scope` 的 session key。
- `internal/app/app_test.go:25`：`TestNewRejectsBindingToSpecialistAgent` 验证 binding 不能指向 specialist。
- `internal/app/app_test.go:45`：`TestNewAcceptsBindingToMasterAgent` 验证合法 master binding。

### 与 claw0 的差异

`claw0` 教程讲的是 5 级绑定表；AIHelper 的结构保留了 `tier` 字段和优先级排序，但当前匹配键实现没有单独的 `guild_id`。如果后面接入 Discord、Slack 或 Telegram 群组，可以在 `matches` 中扩展新的 match key。

### 学习重点

s05 的关键不是具体平台，而是“路由结果必须同时决定 Agent 和 Session”。只决定 Agent 会导致上下文串话；只决定 Session 又无法支持多 Agent。

## s06 Intelligence

### claw0 本章讲什么

s06 引入智能层：系统提示词不再是硬编码字符串，而是由 identity、soul、tools、skills、memory、runtime context、channel hints 多层拼出来。换文件就能换人格和行为边界。

### AIHelper 如何实现

AIHelper 的 `intelligence.Service` 实现了 `agent.PromptBuilder` 接口，`Runner.runAgent` 在每次 answer 调用前会调用它构建 system prompt。

这个服务包含三块核心能力：

- Bootstrap：读取 agent workspace 下的 `SOUL.md`、`IDENTITY.md`、`TOOLS.md`、`MEMORY.md`。
- Skills：扫描 `skills/*/SKILL.md`、`.skills/*/SKILL.md`、插件 skill roots，并处理禁用和覆盖。
- Memory：读取常驻 `MEMORY.md` 和每日 JSONL，支持关键词、hash-vector、可选 embedding 的 hybrid search。

默认 agent workspace 在 `workspace/agents/{agent_id}`。例如 `local-master` 的 identity 在 `workspace/agents/local-master/IDENTITY.md`。

Prompt 组装顺序在 `buildPromptDebug` 里，核心层次是：

1. Identity
2. Soul
3. Tools
4. Skills
5. Memory
6. Runtime
7. Channel

### 代码入口

- `internal/intelligence/service.go:82`：`NewService` 初始化智能层。
- `internal/intelligence/service.go:147`：`Reload` 预加载 bootstrap 和 skills。
- `internal/intelligence/service.go:212`：`BuildSystemPrompt` 接入 Agent Runner。
- `internal/intelligence/service.go:440`：`buildPromptDebug` 组装 7 层 prompt。
- `internal/intelligence/bootstrap.go:40`：`BootstrapLoader.LoadAll` 读取 workspace 文件。
- `internal/intelligence/skills.go:78`：`SkillsManager.DiscoverDebug` 扫描技能。
- `internal/intelligence/memory.go:124`：`MemoryStore.HybridSearchWithWarnings` 执行记忆召回。

### 测试入口

- `internal/intelligence/intelligence_test.go:18`：`TestBootstrapLoaderLoadsAndTruncates` 验证 bootstrap 加载与截断。
- `internal/intelligence/intelligence_test.go:39`：`TestSkillsManagerDiscoversAndOverrides` 验证 skill 发现和覆盖。
- `internal/intelligence/intelligence_test.go:67`：`TestMemoryStoreWritesAndSearchesEvergreenAndDaily` 验证记忆写入和搜索。
- `internal/intelligence/intelligence_test.go:99`：`TestPromptBuilderAssemblesS06Layers` 验证 s06 prompt 分层。
- `internal/intelligence/intelligence_test.go:342`：`TestMemorySearchFallsBackWhenEmbeddingFails` 验证 embedding 失败时回退。
- `internal/app/app_test.go:187`：`TestRunHandlesCLIPromptCommandWithoutLLMReply` 验证 `/prompt` 调试命令。

### 与 claw0 的差异

`claw0` 的 s06 是教学单文件，重点展示 prompt 层次；AIHelper 把它拆成 `Service`、`BootstrapLoader`、`SkillsManager`、`MemoryStore`。AIHelper 还把 memory 工具接回 `tools.MemoryService`，所以模型可以主动写入和搜索记忆。

### 学习重点

读 s06 时不要从 `memory.go` 的算法开始。先看 `Service.BuildSystemPrompt` 如何接到 Agent，再看 `buildPromptDebug` 如何排列提示词层，最后再深入记忆搜索和 skill 扫描。

## s07 Heartbeat & Cron

### claw0 本章讲什么

s07 引入主动型 Agent：除了用户消息，系统也可以定时触发 Agent。关键约束是后台任务不能破坏用户消息优先级，并且最好与用户消息复用同一条 Agent 管线。

### AIHelper 如何实现

AIHelper 把 heartbeat 和 cron 都放在 `internal/heartbeat`：

- Heartbeat Runner 定期读取某个 Agent workspace 下的 `HEARTBEAT.md`，构造任务消息。
- Cron Service 从 `workspace/CRON.json` 读取 jobs，支持 `at`、`every`、`cron` 三种 schedule。
- 后台任务通过 `app.runBackgroundAgentTurn` 构造 `InboundMessage` 和 `Route`，再调用 `app.runTurn` 进入同一套 Agent Runner。

后台任务不会绕过 session、prompt、tools、delivery。它只是换了消息来源和 lane。

### 代码入口

- `internal/heartbeat/runner.go:61`：`NewRunner` 创建 heartbeat runner。
- `internal/heartbeat/runner.go:139`：`Runner.Tick` 定时检查。
- `internal/heartbeat/runner.go:143`：`Runner.Trigger` 手动触发。
- `internal/heartbeat/runner.go:164`：`Runner.run` 读取 `HEARTBEAT.md` 并执行 Agent Turn。
- `internal/heartbeat/schedule.go:20`：`ComputeNext` 计算 `at` / `every` / `cron` 下一次运行时间。
- `internal/heartbeat/cron.go:80`：`NewCronService` 加载 cron jobs。
- `internal/app/app.go:1294`：`runBackgroundAgentTurn` 把后台任务接回 Agent 管线。

### 测试入口

- `internal/heartbeat/runner_test.go:12`：`TestRunnerSkipsMissingAndEmptyHeartbeat` 验证空/缺失心跳文件跳过。
- `internal/heartbeat/runner_test.go:34`：`TestRunnerRespectsActiveHours` 验证活跃时间段。
- `internal/heartbeat/runner_test.go:51`：`TestRunnerSuppressesOKAndDuplicateOutput` 验证空输出和重复输出抑制。
- `internal/heartbeat/schedule_test.go:8`：`TestComputeNextAtPastAndFuture` 验证 at schedule。
- `internal/heartbeat/cron_test.go:69`：`TestCronServiceRunsAgentTurnAndSendsOutput` 验证 cron 调 Agent 并发送。
- `internal/app/app_test.go:353`：`TestCronTriggerRunsThroughAppAndSendsToTarget` 验证 cron 经过 App 管线。

### 与 claw0 的差异

`claw0` 的 s07 用单个 lock 演示用户消息和 heartbeat 的互斥；AIHelper 已经接入 s10 的 lane queue，同时又加了 per-session lock。后台任务可以和主 lane 并行，但同一个 session key 仍会串行，避免上下文写入冲突。

### 学习重点

看 `runBackgroundAgentTurn`：它说明“后台任务”本质上不是第二套 Agent 系统，而是构造一条特殊来源的消息，再进入同一套路由后的执行路径。

## s08 Delivery

### claw0 本章讲什么

s08 讲可靠投递：模型已经生成回复，不代表用户一定收到了。可靠系统应该先把待发送消息写入 outbox，再尝试发送；失败后按退避重试，超过次数进入 failed 队列。

### AIHelper 如何实现

AIHelper 的 delivery 分两层：

- `FileOutbox`：负责把待投递消息作为 JSON 文件写到队列目录，成功发送后删除，失败后更新 retry 信息或移动到 failed。
- `Service`：后台扫描 pending items，调用真实 sender 发送消息，并根据结果 ack/fail。

写入采用临时文件加 rename：

```txt
os.CreateTemp -> json.Encode -> file.Sync -> file.Close -> os.Rename
```

这保证了进程在写入中途崩溃时不会留下半个正式队列文件。`ChunkMessage` 会按平台限制切分长消息。`App.deliver` 在启用 delivery 时不直接调用 channel send，而是入队。

CLI 提供投递观测和修复命令：

- `/queue`
- `/failed`
- `/retry`
- `/delivery`

### 代码入口

- `internal/delivery/outbox.go:28`：`Item` 投递项结构。
- `internal/delivery/outbox.go:56`：`NewFileOutbox` 创建文件队列。
- `internal/delivery/outbox.go:82`：`FileOutbox.Enqueue` 入队。
- `internal/delivery/outbox.go:123`：`FileOutbox.Fail` 失败退避或移动 failed。
- `internal/delivery/outbox.go:261`：`writeEntryLocked` 原子写入实现。
- `internal/delivery/service.go:133`：`Service.Enqueue` 切片并入队。
- `internal/delivery/service.go:151`：`Service.ProcessPending` 扫描并发送。
- `internal/app/app.go:222`：`App.deliver` 选择直接发送或进入 delivery。

### 测试入口

- `internal/delivery/outbox_test.go:15`：`TestFileOutboxEnqueueAndReload` 验证入队和重载。
- `internal/delivery/outbox_test.go:70`：`TestFileOutboxFailBackoffAndMoveToFailed` 验证退避和 failed。
- `internal/delivery/outbox_test.go:162`：`TestFileOutboxCleanupRemovesOrphanTemps` 验证清理临时文件。
- `internal/delivery/outbox_test.go:180`：`TestChunkMessage` 验证消息切分。
- `internal/delivery/service_test.go:13`：`TestServiceProcessPendingSuccessAcks` 验证发送成功 ack。
- `internal/app/app_test.go:473`：`TestDeliveryCLICommands` 验证 CLI 观测命令。

### 与 claw0 的差异

两者思想一致：先落盘，再发送。AIHelper 使用一个 JSON 文件代表一个 delivery item，并增加了 CLI 运维命令。当前 outbox 目录由 `configs/dev.yaml` 的 `delivery.path` 配置，默认是 `workspace/delivery-queue`。

### 学习重点

不要把 delivery 理解成“发消息的工具类”。它是系统可靠性边界：Agent 生成结果之后，channel 发送之前，所有可能丢失的消息都应该先进入 outbox。

## s09 Resilience

### claw0 本章讲什么

s09 引入失败恢复：限流、鉴权失败、超时、余额不足、上下文溢出不是同一种错误，不能用同一种重试策略。教程把它拆成 auth rotation、overflow recovery、tool-use loop 三层洋葱。

### AIHelper 如何实现

AIHelper 把弹性层包成一个 `llm.Client` 实现：`resilience.ResilientClient`。这样 Agent Runner 不需要知道当前是否启用了 key 轮换、fallback model 或上下文压缩。

核心机制：

- `ClassifyFailure` 把错误分类为 `rate_limit`、`auth`、`timeout`、`billing`、`overflow`、`unknown`。
- `ProfileManager` 管理多个 auth profile，失败后按原因进入 cooldown。
- `ResilientClient.CreateMessage` 先尝试主模型和可用 profile，失败后尝试 fallback models。
- `ContextGuard.TruncateToolResults` 截断过长工具结果。
- `ContextGuard.CompactHistory` 调用 LLM 总结旧消息，保留近期上下文。

`app.newLLMClient` 在配置 `llm.resilience.enabled: true` 时创建 resilient client；否则创建普通 OpenAI-compatible client。

### 代码入口

- `internal/resilience/reason.go:9`：`FailoverReason` 分类枚举。
- `internal/resilience/reason.go:25`：`ClassifyFailure` 错误分类。
- `internal/resilience/profile.go:35`：`NewProfileManager` 初始化 profile 池。
- `internal/resilience/profile.go:66`：`SelectAvailable` 选择未冷却 profile。
- `internal/resilience/client.go:44`：`NewClient` 创建 resilient client。
- `internal/resilience/client.go:68`：`ResilientClient.CreateMessage` 对外实现 `llm.Client`。
- `internal/resilience/client.go:113`：`runWithProfiles` 执行 profile 轮换和 overflow retry。
- `internal/resilience/context_guard.go:66`：`CompactHistory` 压缩历史。
- `internal/app/app.go:961`：`newResilientOpenAIClient` 从配置组装弹性 LLM client。

### 测试入口

- `internal/resilience/resilience_test.go:15`：`TestClassifyFailure` 验证错误分类。
- `internal/resilience/resilience_test.go:38`：`TestProfileManagerSelectsAroundCooldowns` 验证 cooldown 选择。
- `internal/resilience/resilience_test.go:72`：`TestResilientClientRotatesAfterRateLimit` 验证限流后轮换。
- `internal/resilience/resilience_test.go:131`：`TestResilientClientUsesFallbackModelAfterPrimaryExhausted` 验证 fallback model。
- `internal/resilience/resilience_test.go:174`：`TestResilientClientCompactsOverflowOnlyForRetryRequest` 验证 overflow compact。
- `internal/app/app_test.go:110`：`TestNewLLMClientResilienceConfigCompatibility` 验证配置兼容性。

### 与 claw0 的差异

`claw0` 在教学代码里直接展示三层重试洋葱；AIHelper 把它封装为 `llm.Client` 装饰器。这样上层 `agent.Runner` 不需要改代码，就能从普通 client 切换到 resilient client。

### 学习重点

读 s09 时从 `newLLMClient` 开始，确认弹性层如何被接入。然后看 `ResilientClient.CreateMessage`，最后再看 `ContextGuard`。顺序反过来容易陷进压缩细节里。

## s10 Concurrency

### claw0 本章讲什么

s10 把单一锁升级成命名 lane：不同来源的任务可以放进不同队列，每个 lane 内 FIFO，可配置并发度，任务返回 future。generation tracking 用来处理 reset 后的旧任务，避免旧状态继续 pump 新队列。

### AIHelper 如何实现

AIHelper 的 `internal/concurrency` 提供 `CommandQueue` 和 `LaneQueue`：

- `CommandQueue.Enqueue(ctx, lane, task)` 把任务放进指定 lane。
- 每个 lane 默认 `maxConcurrency=1`，所以同 lane 严格串行。
- `SetConcurrency` 可以调整某个 lane 的并发度。
- `Future.Result` 等待任务结果。
- `Reset` 会增加 generation，并让排队中的任务返回 `ErrQueueReset`。

`App.New` 启动时创建三个 lane：

- `main`
- `heartbeat`
- `cron`

用户消息走 `main` lane，heartbeat 和 cron 通过 `laneForBackgroundSource` 进入各自 lane。AIHelper 还额外加了一层 `sessionLock(sessionKey)`：即使不同 lane 并发，同一个 session key 仍然不会并发写入历史。

CLI 提供并发观测和调整：

- `/lanes`
- `/concurrency <lane> <N>`

### 代码入口

- `internal/concurrency/queue.go:34`：`CommandQueue`。
- `internal/concurrency/queue.go:61`：`CommandQueue.Enqueue`。
- `internal/concurrency/queue.go:71`：`SetConcurrency`。
- `internal/concurrency/queue.go:78`：`ResetAll`。
- `internal/concurrency/queue.go:145`：`LaneQueue`。
- `internal/concurrency/queue.go:277`：`pumpLocked` 按并发度启动任务。
- `internal/concurrency/queue.go:294`：`taskDone` 用 generation 判断是否继续 pump。
- `internal/concurrency/queue.go:340`：`Future.Result`。
- `internal/app/app.go:229`：`runTurn` 把 Agent Turn 放入 lane。
- `internal/app/app.go:244`：`runTurnForSession` 加 per-session lock。
- `internal/app/app.go:691`：`printLaneStatus` 输出 lane 状态。

### 测试入口

- `internal/concurrency/queue_test.go:12`：`TestLaneRunsFIFOWithDefaultConcurrency` 验证默认串行。
- `internal/concurrency/queue_test.go:33`：`TestDifferentLanesRunConcurrently` 验证不同 lane 并行。
- `internal/concurrency/queue_test.go:57`：`TestSetConcurrencyAllowsParallelTasksInSameLane` 验证同 lane 提升并发。
- `internal/concurrency/queue_test.go:82`：`TestFutureReturnsTaskResultErrorAndContextTimeout` 验证 future 结果和超时。
- `internal/concurrency/queue_test.go:107`：`TestResetPreventsStaleTaskFromPumpingQueuedWork` 验证 generation reset。
- `internal/app/app_test.go:264`：`TestBackgroundAgentTurnRunsWhileMainLaneBusy` 验证后台 lane 不被 main lane 阻塞。
- `internal/app/app_test.go:307`：`TestBackgroundAgentTurnWaitsForSameSessionLock` 验证同 session 仍串行。
- `internal/app/app_test.go:684`：`TestConcurrencyCLICommands` 验证 CLI 命令。

### 与 claw0 的差异

`claw0` 的 s10 是教学版 lane queue；AIHelper 已经把 lane queue 接进真实 App。并且 AIHelper 同时保留 session-level mutex，这是对生产问题的补充：lane 解决来源并发，session lock 解决同一段上下文的写入顺序。

### 学习重点

并发层的目的不是让所有任务都并行，而是让并发“有名字”。看到 `main`、`heartbeat`、`cron` 这些 lane 名字时，你就能判断哪个来源拥堵、哪个来源可以独立运行。

## 综合学习路线

建议按下面顺序读代码：

1. `internal/agent/runner.go`：先理解 Agent Turn 和工具循环。
2. `internal/llm/types.go`、`internal/llm/openai.go`：理解模型请求和 stop reason 如何归一化。
3. `internal/tools`：理解 schema、handler、Agent 白名单。
4. `internal/sessions`：理解会话如何保存、加载、替换。
5. `internal/channels` 和 `internal/gateway`：理解外部消息如何进入正确 Agent 和 session。
6. `internal/intelligence/service.go`：理解 system prompt 如何由 workspace 文件、技能、记忆组装。
7. `internal/heartbeat`、`internal/delivery`、`internal/resilience`、`internal/concurrency`：理解生产可靠性能力如何接到主链路。

如果想从运行入口跟完整流程，可以从 `cmd/aihelper/main.go` 开始，一路跳到 `app.New`、`App.Run`、`handleMessage`、`runTurn`、`Runner.RunTurn`。

## 验证方式

文档对应的代码测试可以直接运行：

```sh
go test ./...
```

也可以用 CLI 启动项目做人工观察：

```sh
go run ./cmd/aihelper -config configs/dev.yaml
```

常用调试命令：

```txt
/help
/sessions
/prompt local-master 解释你自己
/bootstrap local-master
/skills local-master
/memory local-master
/lanes
/delivery
/heartbeat
/cron
```

## 结论

AIHelper 已经把 `claw0` 的 10 个章节完整映射成 Go 工程里的 10 组能力：Agent Loop、工具、会话、通道、路由、智能层、主动任务、可靠投递、失败恢复、命名并发。最重要的工程原则是：核心 Agent Loop 保持薄，外围能力通过接口和服务注入。这样每个章节都能单独学习、单独测试，也能在真实运行链路里组合起来。
