# claw0 实现调研与 Go 项目结构建议

调研对象：`git@github.com:shareAI-lab/claw0.git`

本地路径：`/Users/liangzhancheng/GolandProjects/AIHelper/claw0`

调研版本：`0090e86`

生成日期：2026-05-19

## 1. 总结

`claw0` 不是一个完整生产服务仓库，而是一个教学型 Python 项目。它把一个 AI Agent Gateway 拆成 10 个递进章节：每个章节是一份可运行 Python 文件，加一份对应说明文档。它的价值不在于目录工程化，而在于把生产级 Agent Gateway 的核心机制拆得很清楚：

- s01-s02：最小 Agent Loop 和工具分发表。
- s03-s05：会话持久化、通道抽象、网关路由。
- s06-s08：提示词智能层、心跳/Cron、可靠投递。
- s09-s10：认证轮换、上下文溢出恢复、命名 lane 并发。

如果你用 Go 开发，建议不要照搬 claw0 的“每节一个大文件”结构，而是把这些章节沉淀成 `internal/` 下的一组稳定包。核心原则是：**Agent Loop 保持很薄，外围能力通过接口注入**。

## 2. claw0 仓库观察

仓库结构很简单：

```txt
claw0/
├── README.md
├── README.zh.md
├── README.ja.md
├── requirements.txt
├── sessions/
│   ├── en/
│   ├── zh/
│   └── ja/
└── workspace/
    ├── AGENTS.md
    ├── BOOTSTRAP.md
    ├── CRON.json
    ├── HEARTBEAT.md
    ├── IDENTITY.md
    ├── MEMORY.md
    ├── SOUL.md
    ├── TOOLS.md
    └── skills/example-skill/SKILL.md
```

中文实现主要在 `sessions/zh/`：

```txt
s01_agent_loop.py          175 行
s02_tool_use.py            492 行
s03_sessions.py            893 行
s04_channels.py            780 行
s05_gateway_routing.py     625 行
s06_intelligence.py        950 行
s07_heartbeat_cron.py      659 行
s08_delivery.py            869 行
s09_resilience.py         1126 行
s10_concurrency.py         900 行
```

这些文件的实现逻辑是递进复制：后一节保留前一节的核心代码，再增加一个机制。因此它适合学习，不适合直接作为 Go 工程结构蓝本。

## 3. 关键设计拆解

### s01 Agent Loop

核心模型非常小：

```txt
user input -> messages append -> LLM call -> stop_reason 分支
```

`stop_reason` 是决策点：

- `end_turn`：提取文本并返回。
- `tool_use`：进入工具调用循环。
- `max_tokens` 或其他：按部分结果或异常处理。

Go 里应该把这个变成 `agent.Runner`，只负责“一轮推理”和“工具调用循环”，不要让它知道 Telegram、飞书、WebSocket、Cron 等外围细节。

### s02 Tools

claw0 的工具模型是两张表：

- `TOOLS`：给模型看的 JSON schema。
- `TOOL_HANDLERS`：给程序执行用的函数映射。

Go 中建议拆成：

```go
type Tool interface {
    Name() string
    Schema() ToolSchema
    Call(ctx context.Context, input json.RawMessage) (ToolResult, error)
}

type Registry interface {
    Register(tool Tool) error
    GetForAgent(agentID, name string) (Tool, bool)
    SchemasForAgent(agentID string) []ToolSchema
}
```

Agent Loop 只依赖 `Registry`，并通过 agent 工具白名单取 schema 和 handler。这样未来新增网页、日历、记忆、文件等工具时，不需要改循环本身，也不会绕过单个 agent 的能力边界。

### s03 Sessions

claw0 使用 JSONL 做会话持久化：

```txt
workspace/.sessions/agents/{agent_id}/sessions/{session_id}.jsonl
workspace/.sessions/agents/{agent_id}/sessions.json
```

每一行是一条事件，例如 user、assistant、tool_use、tool_result。读取时通过 replay 重建 LLM API 需要的 `messages[]`。

这个设计适合 Go 继续使用，因为：

- 追加写入简单。
- 崩溃恢复容易。
- 容易做审计、回放和压缩。
- 比数据库更适合作为第一版。

建议 Go 中定义：

```go
type SessionStore interface {
    Load(ctx context.Context, agentID, sessionID string) ([]llm.Message, error)
    Append(ctx context.Context, agentID, sessionID string, event SessionEvent) error
    List(ctx context.Context, agentID string) ([]SessionMeta, error)
    Compact(ctx context.Context, agentID, sessionID string) error
}
```

`ContextGuard` 则负责三阶段保护：

```txt
正常调用 -> 截断大型工具结果 -> LLM 摘要压缩历史 -> 失败
```

### s04 Channels

claw0 把所有平台消息统一成 `InboundMessage`：

```txt
text
sender_id
channel
account_id
peer_id
is_group
media
raw
```

这点非常重要。Go 项目里，Telegram、飞书、CLI、WebSocket 都应该只做适配，不要把平台字段泄漏到 Agent 核心。

建议接口：

```go
type InboundMessage struct {
    Text      string
    SenderID  string
    Channel   string
    AccountID string
    PeerID    string
    IsGroup   bool
    Media     []Media
    Raw       json.RawMessage
}

type Channel interface {
    Name() string
    Start(ctx context.Context, out chan<- InboundMessage) error
    Send(ctx context.Context, target OutboundTarget, msg OutboundMessage) error
    Close(ctx context.Context) error
}
```

### s05 Gateway & Routing

claw0 使用 5 级绑定表：

```txt
T1 peer_id      最具体
T2 guild_id
T3 account_id
T4 channel
T5 default      最宽泛
```

匹配时按 `(tier, -priority)` 排序，第一次命中的 binding 胜出。

Go 中可以做成：

```go
type Binding struct {
    AgentID    string
    Tier       int
    MatchKey   string
    MatchValue string
    Priority   int
}

type Router interface {
    Resolve(ctx context.Context, msg InboundMessage) (Route, error)
}
```

会话隔离由 `dm_scope` 控制：

```txt
main
per-peer
per-channel-peer
per-account-channel-peer
```

这个不要写死在 Channel 里，应该放在 Agent 配置或 Gateway 路由结果里。

### s06 Intelligence

claw0 的智能层由 7 层 prompt 组成：

```txt
1. Identity
2. Soul / Personality
3. Tools guidance
4. Skills
5. Memory
6. Runtime context
7. Channel hints
```

同时有三套能力：

- `BootstrapLoader`：读取 `IDENTITY.md`、`SOUL.md`、`TOOLS.md` 等工作区文件。
- `SkillsManager`：扫描 `SKILL.md`。
- `MemoryStore`：常驻 `MEMORY.md` + 每日 JSONL，支持 TF-IDF / hybrid recall。

Go 里建议把提示词组装做成纯函数式服务：

```go
type PromptBuilder interface {
    Build(ctx context.Context, req PromptRequest) (string, error)
}

type MemoryStore interface {
    Write(ctx context.Context, item MemoryItem) error
    Search(ctx context.Context, query string, topK int) ([]MemoryHit, error)
}

type SkillStore interface {
    Discover(ctx context.Context, roots []string) ([]Skill, error)
}
```

重点：PromptBuilder 不应该调用 LLM。它只负责把文件、记忆、技能、运行时信息拼成系统提示词。

### s07 Heartbeat & Cron

claw0 的心跳设计有两个关键点：

- 用户消息优先。
- 后台任务与用户消息走同一套 Agent 管线。

s07 用一个 `threading.Lock` 做 lane 互斥：用户消息阻塞拿锁，heartbeat 非阻塞拿锁，拿不到就跳过。

Go 里第一版可以用 `sync.Mutex` 或直接进入 s10 的 lane 系统。建议直接做 lane，避免后续重构。

Cron 支持三种调度：

```txt
at      指定时间一次性触发
every   固定间隔触发
cron    cron 表达式触发
```

任务定义来自 `workspace/CRON.json`。

### s08 Delivery

可靠投递是 claw0 里最值得直接学习的一层。

核心规则：

```txt
先写磁盘，再尝试发送
```

入队流程：

```txt
生成 id
写 .tmp.{pid}.{id}.json
fsync
os.replace 到 {id}.json
后台 runner 扫描并发送
成功 ack 删除
失败 fail 更新 retry_count / next_retry_at
超过最大次数 move failed/
```

Go 中建议保留“一个投递项一个 JSON 文件”的模型：

```go
type Outbox interface {
    Enqueue(ctx context.Context, item DeliveryItem) (string, error)
    Ack(ctx context.Context, id string) error
    Fail(ctx context.Context, id string, cause error) error
    Pending(ctx context.Context) ([]DeliveryItem, error)
}
```

原子写入用：

```txt
os.CreateTemp -> file.Sync -> file.Close -> os.Rename
```

退避策略：

```txt
5s, 25s, 2min, 10min，加 +/-20% jitter
```

### s09 Resilience

弹性层是三层重试洋葱：

```txt
Layer 1: Auth Rotation
Layer 2: Overflow Recovery
Layer 3: Tool-Use Loop
```

失败分类：

```txt
rate_limit
auth
timeout
billing
overflow
unknown
```

不同失败进入不同恢复路径：

- auth / billing：当前 profile 冷却较长时间，切下一个 key。
- rate_limit：短冷却，切下一个 key。
- timeout：更短冷却，切下一个 key。
- overflow：不切 key，压缩上下文后重试。
- unknown：冷却后换 profile。

Go 中建议：

```go
type ResilientRunner struct {
    Profiles ProfileManager
    Guard    ContextGuard
    ClientFn LLMClientFactory
    Tools    tools.Registry
}
```

这样 Agent Loop 本身仍然可以很干净，生产运行时用 `ResilientRunner` 包住普通 runner。

### s10 Concurrency

s10 把 s07 的单个锁升级为命名 lane：

```txt
main
cron
heartbeat
custom lanes...
```

每个 lane 是 FIFO 队列，有自己的 `max_concurrency`。默认 1，也就是同一 lane 内严格串行。任务返回 Future。

最关键的是 generation tracking：

```txt
lane.generation++
旧 generation 的任务完成后，不再 pump 后续队列
```

Go 中建议模型：

```go
type Task func(ctx context.Context) (any, error)

type Future interface {
    Result(ctx context.Context) (any, error)
}

type CommandQueue interface {
    Enqueue(ctx context.Context, lane string, task Task) Future
    SetConcurrency(lane string, n int)
    ResetAll() map[string]int
    Stats() []LaneStats
}
```

Go 实现上可以用：

- `chan queuedTask` 做队列。
- `sync.Mutex` + `sync.Cond` 管理状态。
- 每个 lane 一个 dispatcher goroutine。
- 或者每个任务启动 goroutine，但用 semaphore 限制并发。

第一版建议每个 lane 一个 dispatcher goroutine，更贴合 Go。

## 4. 推荐 Go 项目结构

当前你的 `AIHelper` 目录只有 `go.mod` 和 `main.go`，可以演进成下面结构：

```txt
AIHelper/
├── go.mod
├── go.sum
├── README.md
├── docs/
│   └── claw0-go-architecture.md
├── cmd/
│   └── aihelper/
│       └── main.go
├── configs/
│   ├── dev.yaml
│   └── prod.yaml
├── workspace/
│   ├── CRON.json
│   ├── agents/
│   │   └── local-master/
│   │       ├── HEARTBEAT.md
│   │       ├── IDENTITY.md
│   │       ├── MEMORY.md
│   │       ├── SOUL.md
│   │       └── TOOLS.md
│   └── skills/
│       └── example-skill/
│           └── SKILL.md
├── internal/
│   ├── app/
│   │   ├── app.go
│   │   └── lifecycle.go
│   ├── config/
│   │   └── config.go
│   ├── llm/
│   │   ├── client.go
│   │   ├── anthropic.go
│   │   ├── openai.go
│   │   └── message.go
│   ├── agent/
│   │   ├── runner.go
│   │   ├── loop.go
│   │   ├── turn.go
│   │   └── stop_reason.go
│   ├── tools/
│   │   ├── tool.go
│   │   ├── registry.go
│   │   ├── dispatcher.go
│   │   └── builtin/
│   │       ├── file.go
│   │       ├── shell.go
│   │       ├── memory.go
│   │       └── time.go
│   ├── sessions/
│   │   ├── event.go
│   │   ├── store.go
│   │   ├── jsonl_store.go
│   │   ├── rebuild.go
│   │   └── compact.go
│   ├── channels/
│   │   ├── message.go
│   │   ├── channel.go
│   │   ├── manager.go
│   │   ├── cli/
│   │   │   └── channel.go
│   │   ├── telegram/
│   │   │   ├── channel.go
│   │   │   ├── polling.go
│   │   │   └── offset.go
│   │   └── feishu/
│   │       ├── channel.go
│   │       └── webhook.go
│   ├── gateway/
│   │   ├── binding.go
│   │   ├── router.go
│   │   ├── session_key.go
│   │   ├── server.go
│   │   └── jsonrpc.go
│   ├── intelligence/
│   │   ├── bootstrap.go
│   │   ├── prompt.go
│   │   ├── memory.go
│   │   ├── recall.go
│   │   ├── skills.go
│   │   └── token_budget.go
│   ├── heartbeat/
│   │   ├── runner.go
│   │   ├── cron.go
│   │   ├── job.go
│   │   └── schedule.go
│   ├── delivery/
│   │   ├── item.go
│   │   ├── outbox.go
│   │   ├── file_outbox.go
│   │   ├── runner.go
│   │   ├── backoff.go
│   │   └── chunk.go
│   ├── resilience/
│   │   ├── reason.go
│   │   ├── profile.go
│   │   ├── manager.go
│   │   ├── context_guard.go
│   │   └── runner.go
│   └── concurrency/
│       ├── lane.go
│       ├── queue.go
│       ├── future.go
│       └── stats.go
├── pkg/
│   └── ids/
│       └── ids.go
├── var/
│   ├── sessions/
│   ├── delivery/
│   └── logs/
└── tests/
    ├── fixtures/
    └── integration/
```

## 5. 包职责边界

### `internal/agent`

只做 Agent 的核心循环：

- 接收用户消息。
- 加载历史上下文。
- 调 LLM。
- 处理 `stop_reason`。
- 遇到工具调用时交给 `tools.Dispatcher`。
- 最终返回 assistant response。

不应该依赖：

- Telegram / Feishu。
- WebSocket。
- Cron。
- 文件投递队列。

### `internal/channels`

负责平台接入：

- 将平台 payload 转成 `InboundMessage`。
- 将 `OutboundMessage` 发回对应平台。
- 做平台特有能力：Telegram offset、typing、Feishu token 等。

### `internal/gateway`

负责把消息送到正确 Agent：

- BindingTable。
- 5-tier route。
- session key 构造。
- WebSocket / JSON-RPC API。
- AgentManager。

### `internal/intelligence`

负责“脑子”的上下文：

- 读取 workspace markdown。
- 发现技能。
- 搜索记忆。
- 组装 7 层 prompt。

### `internal/delivery`

负责可靠输出：

- 预写 outbox。
- 原子落盘。
- 分片。
- 退避重试。
- failed 归档。

### `internal/resilience`

负责失败恢复：

- 失败分类。
- API profile 冷却和轮换。
- 上下文压缩。
- fallback model。
- 包装 agent runner。

### `internal/concurrency`

负责所有并发秩序：

- 命名 lane。
- FIFO。
- per-lane concurrency。
- Future。
- generation tracking。
- idle / graceful shutdown。

## 6. 推荐运行链路

### 用户消息链路

```txt
Channel.Start
  -> InboundMessage
  -> Gateway.Router.Resolve
  -> build session key
  -> CommandQueue.Enqueue("main")
  -> Agent.Runner.RunTurn
  -> Sessions.Load / Append
  -> PromptBuilder.Build
  -> ResilientRunner.Run
  -> Tools.Dispatcher
  -> Delivery.Outbox.Enqueue
  -> DeliveryRunner.Send
```

### Heartbeat / Cron 链路

```txt
CronService tick 或 HeartbeatRunner tick
  -> CommandQueue.Enqueue("heartbeat" 或 "cron")
  -> 构造系统事件 / agent prompt
  -> Agent.Runner.RunTurn
  -> 有意义输出才进入 Delivery.Outbox
```

### 工具调用链路

```txt
LLM stop_reason=tool_use
  -> ToolUse block
  -> Registry.GetForAgent(agentID, name)
  -> Tool.Call(ctx, input)
  -> tool_result append as user message
  -> 回到 LLM
```

### 失败恢复链路

```txt
LLM call failed
  -> classify failure
  -> overflow: truncate + compact
  -> auth/rate/timeout/billing: profile cooldown + rotate
  -> exhausted: fallback model
  -> still failed: return error
```

## 7. 建议数据目录

开发期可以使用本地文件：

```txt
var/
├── sessions/
│   └── agents/
│       └── {agent_id}/
│           ├── sessions.json
│           └── sessions/
│               └── {session_id}.jsonl
├── delivery/
│   ├── queue/
│   │   └── {delivery_id}.json
│   └── failed/
│       └── {delivery_id}.json
├── memory/
│   ├── MEMORY.md
│   └── daily/
│       └── 2026-05-19.jsonl
└── logs/
    ├── cron-runs.jsonl
    └── gateway.jsonl
```

生产期再考虑替换为 SQLite / Postgres / Redis。第一版不建议过早引入数据库，否则会把 Agent 的核心问题淹没在基础设施里。

## 8. Go 依赖建议

第一版尽量标准库优先：

- CLI：标准库 `flag` 即可，后面再换 `cobra`。
- 配置：标准库 `encoding/json` / `gopkg.in/yaml.v3`。
- WebSocket：`nhooyr.io/websocket` 或 `github.com/gorilla/websocket`。
- Cron：`github.com/robfig/cron/v3`。
- 日志：标准库 `log/slog`。
- HTTP：标准库 `net/http`。
- JSONL：标准库 `encoding/json` + `bufio.Scanner`。

LLM Provider 建议先做接口，再写 provider：

```go
type Client interface {
    CreateMessage(ctx context.Context, req MessageRequest) (MessageResponse, error)
}
```

这样 Anthropic / OpenAI / 兼容服务商都只是实现细节。

## 9. 里程碑建议

### M1：最小 Agent

- `cmd/aihelper/main.go`
- `internal/llm`
- `internal/agent`
- CLI 输入输出
- 无工具、无持久化

验收：可以多轮对话，能处理 `end_turn`。

### M2：工具系统

- `internal/tools`
- file read / write / edit
- shell 可先不开放，或做 allowlist

验收：模型可以连续调用工具，直到返回最终回答。

### M3：会话持久化

- `internal/sessions`
- JSONL append
- replay rebuild
- `/new`、`/switch`、`/context`

验收：重启后可以恢复上下文。

### M4：通道和路由

- `internal/channels`
- `internal/gateway`
- CLI channel
- WebSocket gateway
- BindingTable + dm_scope

验收：同一个 Agent 能处理不同 peer 的独立会话。

### M5：智能层

- `workspace/agents/{agent_id}/*.md`
- 7 层 prompt builder
- memory write/search
- skill discovery

验收：修改 `SOUL.md` 不改代码即可改变 agent 风格；记忆可被自动召回。

### M6：生产可靠性

- heartbeat / cron
- delivery outbox
- resilience runner
- named lanes

验收：后台任务不阻塞用户消息；发送失败可重试；API key 失败可轮换；同一 lane 串行。

## 10. 从 claw0 学到的工程原则

1. Agent 的核心循环要小。

   一旦 `agent` 包直接依赖各种平台、调度和投递，它就会很快变成“什么都知道”的大泥球。

2. 工具是 schema + handler。

   Schema 给模型看，handler 给程序执行。两者通过工具名连接。

3. 所有外部输入先归一化。

   Agent 不应该知道 Telegram update 或飞书 event 的原始形状。

4. 会话用事件流，而不是只存最终 messages。

   JSONL 事件流更容易审计、压缩、回放和调试。

5. Prompt 是运行时产物，不是硬编码字符串。

   `IDENTITY.md`、`SOUL.md`、`MEMORY.md`、`SKILL.md` 这些文件应该成为产品配置的一部分。

6. 主动任务必须尊重用户优先级。

   heartbeat 和 cron 不能抢占用户消息。Go 里用 lane 比单锁更清晰。

7. 输出要先落盘。

   对外发送消息时，先写 outbox，再发送。进程崩溃也不能丢消息。

8. 失败恢复要分类。

   上下文溢出、鉴权失败、限流、超时不是同一种错误，不应该用同一种 retry。

9. 并发要命名。

   `main`、`heartbeat`、`cron`、`research` 这些 lane 名称比散落的 goroutine 更容易理解和调试。

## 11. 对 AIHelper 的下一步建议

你的当前工程还是 GoLand 生成的默认 `main.go`。建议下一步直接做 M1-M2：

```txt
cmd/aihelper/main.go
internal/llm/
internal/agent/
internal/tools/
```

先不要上 Telegram、飞书、Cron 和 delivery。把 `while + stop_reason + tool dispatch` 做顺，再接 JSONL session。这个顺序和 claw0 的学习路径一致，也最不容易把系统做散。
