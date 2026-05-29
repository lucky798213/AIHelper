# AIHelper Interview Q&A

说明：每个答案默认折叠，点击问题前的小箭头即可展开。回答基于当前代码实现，面试时可以直接按这里的表达回答；其中没有实现的能力也如实说明了边界。

## 多 Agent 调度

<details>
<summary><strong>master-specialist 架构中，专家 Agent 的返回结果是直接透传给用户还是经过主控 Agent 二次加工？</strong></summary>

当前实现是直接透传给用户，没有再让 master 做二次润色。

流程是：master 先做一次 dispatch，返回 JSON `{mode, agent_id, input}`；如果是 `delegate`，就调用 `runChildAgent` 跑专家 Agent；专家 Agent 的 `outputMessage.Content` 会被包装成 master session 里的 assistant message，然后直接作为 `OutboundMessage.Text` 返回给用户。

代码位置：

- `internal/agent/runner.go:90`：`dispatch` 调用 master 做路由决策。
- `internal/agent/runner.go:134`：`runDispatchDecision` 判断 direct 还是 delegate。
- `internal/agent/runner.go:145`：`runChildAgent` 执行专家 Agent。
- `internal/agent/runner.go:186`：专家输出被包装成 assistant message。
- `internal/agent/runner.go:81`：最终写入 `OutboundMessage.Text`。

</details>

<details>
<summary><strong>整个调度链路是同步等待还是流式输出？</strong></summary>

当前是同步等待，不是流式输出。

`App.runTurn` 会把任务放进命名 Lane，然后通过 `future.Result(ctx)` 等待 Agent 完整跑完。OpenAI-compatible client 也是普通 `/chat/completions` 请求，没有设置 stream，也没有边生成边投递的机制。

代码位置：

- `internal/app/app.go:237`：`runTurn` 入队并等待 `future.Result(ctx)`。
- `internal/concurrency/queue.go:360`：`Future.Result` 阻塞等待任务完成。
- `internal/llm/openai.go:80`：`CreateMessage` 使用普通 HTTP 请求。
- `internal/llm/openai.go:170`：请求体没有 stream 字段。

</details>

<details>
<summary><strong>如果代码专家 Agent 在执行过程中需要再次调用工具，工具调用结果如何回传给专家 Agent 形成多轮？</strong></summary>

专家 Agent 和 master 走同一个 `runAgent` 工具循环。

每一轮模型请求会携带当前 messages 和当前 Agent 可用工具 schema。模型返回 `tool_calls` 后，程序执行对应工具，把工具结果追加成一条 `role=tool` 的消息，再带着更新后的上下文继续调用模型。这样工具调用结果就会回传给同一个专家 Agent，形成多轮 tool-use loop。

代码位置：

- `internal/agent/runner.go:190`：`runAgent` 是统一的 Agent 执行循环。
- `internal/agent/runner.go:208`：调用 LLM 时传入 `Messages` 和 `Tools`。
- `internal/agent/runner.go:221`：根据 `StopReason` 判断是否工具调用。
- `internal/agent/runner.go:236`：遍历模型返回的 `ToolCalls`。
- `internal/agent/runner.go:241`：执行工具。
- `internal/agent/runner.go:248`：构造 `role=tool` 的消息。
- `internal/agent/runner.go:254`：把工具结果追加回当前上下文。

</details>

<details>
<summary><strong>最大调用深度怎么控制，防止循环调用？</strong></summary>

当前通过固定循环次数控制。`runAgent` 最多执行 5 次 LLM-tool 往返，如果 5 次后模型还没有返回 `end_turn`，就报错 `agent exceeded tool loop limit`。

这控制的是单个 Agent 单轮请求里的工具循环深度，不支持专家再递归委派其他专家。当前只有 master 可以 dispatch 给 declared children，specialist 不会再做子 Agent 调度。

代码位置：

- `internal/agent/runner.go:190`：进入 `runAgent`。
- `internal/agent/runner.go:192`：`for i := 0; i < 5; i++` 限制工具循环。
- `internal/agent/runner.go:263`：超过 5 次后返回错误。
- `internal/agent/types.go:86`：`CanDelegate` 只允许 master 委派给已声明 child。

</details>

<details>
<summary><strong>工具白名单是在 Agent 初始化时注入进 system prompt 的，还是在运行时拦截 function calling 的 name 字段做校验？两种方式各有什么问题？</strong></summary>

当前做了两层，不是只靠 system prompt。

第一层：运行时只把当前 Agent 白名单内的工具 schema 传给模型，减少模型看到不该看到的工具。  
第二层：模型真的返回工具调用后，再通过 `GetForAgent(agentID, toolName)` 做执行前校验，防止模型调用未授权工具名。

只靠 system prompt 的问题是：模型可能无视约束，也可能幻觉工具名；prompt 不是安全边界。  
只靠运行时拦截的问题是：如果把所有工具 schema 都暴露给模型，会浪费 token，也会让模型知道不该暴露的能力，增加误调用和安全风险。  
所以当前实现是 schema 最小暴露 + 执行时强校验。

代码位置：

- `internal/app/app.go:89`：注册所有内置工具。
- `internal/app/app.go:97`：配置每个 Agent 的工具白名单。
- `internal/app/app.go:1182`：`configureAgentTools`。
- `internal/tools/registry.go:79`：`SetAllowedTools` 写入白名单。
- `internal/tools/registry.go:109`：`SchemasForAgent` 只返回该 Agent 可见工具。
- `internal/tools/registry.go:94`：`GetForAgent` 执行前校验。
- `internal/agent/runner.go:215`：调用 LLM 时只传入当前 Agent 的工具 schema。
- `internal/agent/runner.go:237`：执行工具前按 Agent 校验工具名。

</details>

## 双通道接入与 Session 管理

<details>
<summary><strong>Agent / Channel / Peer 三维 session key 具体是怎么组合的？</strong></summary>

默认 `dm_scope` 是 `per-channel-peer`，格式是：

```text
agent:{agentID}:{channel}:direct:{peerID}
```

例如 CLI 默认就是：

```text
agent:local-master:cli:direct:cli-user
```

还支持其他隔离策略：

- `main`：`agent:{agentID}:main`
- `per-peer`：`agent:{agentID}:peer:{peerID}`
- `per-account-channel-peer`：`agent:{agentID}:account:{accountID}:{channel}:peer:{peerID}`

专家 Agent 被委派时使用独立 session：

```text
agent:{childAgentID}:parent:{masterAgentID}:{channel}:{peerID}
```

代码位置：

- `internal/gateway/router.go:66`：`buildSessionKey`。
- `internal/gateway/router.go:72`：按 `DMScope` 生成不同 session key。
- `internal/agent/runner.go:157`：专家 Agent 的 child session key。
- `internal/sessions/sqlite.go:403`：`parseSessionKey` 解析 key 并写入 session metadata。

</details>

<details>
<summary><strong>同一个用户在飞书群聊里 @ 不同 Agent，session 是共享还是隔离？</strong></summary>

当前默认配置下，飞书群聊里的 session 是按 `Agent + Channel + Peer` 隔离，而 `PeerID` 在群聊中是 `ChatID`，不是 `SenderID`。所以同一个群里的多个用户会共享同一个 master session。

当前系统也不是按“@ 不同 Agent 名字”直接路由到不同 master，而是先进入绑定的 master Agent，再由 master dispatch 决定是否委派给 `coder-agent` 或 `writer-agent`。如果希望同一群内按用户隔离，需要把 `SenderID` 纳入 session key；如果希望按不同 Agent mention 隔离，需要扩展飞书 mention 解析和 gateway binding。

代码位置：

- `internal/channels/feishu/channel.go:73`：群聊 `peerID` 使用 `ChatID`。
- `internal/channels/feishu/channel.go:75`：私聊 `peerID` 使用 `SenderOpenID`。
- `internal/gateway/router.go:66`：session key 生成不包含 `SenderID`。
- `configs/dev.yaml`：当前默认 binding 绑定到 `local-master`，`dm_scope: per-channel-peer`。

</details>

<details>
<summary><strong>飞书 SDK 的消息接收是 webhook 推送还是长轮询？</strong></summary>

当前不是传统 HTTP webhook，也不是长轮询，而是飞书/Lark SDK 的 WebSocket 事件客户端。

代码里使用 `larkws.NewClient(appID, appSecret, opts...).Start(ctx)` 建立 WebSocket 连接，并通过 event dispatcher 接收 `OnP2MessageReceiveV1` 消息事件。

代码位置：

- `internal/channels/feishu/sdk.go:9`：引入 `github.com/larksuite/oapi-sdk-go/v3/ws`。
- `internal/channels/feishu/sdk.go:26`：`SDKClient.Start`。
- `internal/channels/feishu/sdk.go:27`：创建事件 dispatcher。
- `internal/channels/feishu/sdk.go:36`：创建 WebSocket client。
- `internal/channels/feishu/sdk.go:37`：启动 WebSocket client。

</details>

<details>
<summary><strong>怎么保证飞书消息的幂等处理，消息重复推送了怎么办？</strong></summary>

当前版本没有完整实现幂等去重。

系统已经把飞书 `MessageID` 映射到了内部 `InboundMessage.ID`，这为幂等处理预留了 key；但现在没有把已处理消息 ID 落库，也没有在入口处做去重判断。所以飞书如果重复推送同一个 message，当前会重复处理。

面试时可以这样回答：当前版本保留了幂等字段，后续会在 SQLite 中增加 `processed_messages(channel, message_id)` 表，并对 `(channel, message_id)` 建唯一索引；处理前先插入，插入冲突说明重复消息，直接跳过。

代码位置：

- `internal/channels/message.go:5`：内部消息结构包含 `ID`。
- `internal/channels/feishu/channel.go:83`：飞书 `MessageID` 映射为 `InboundMessage.ID`。
- 当前没有 `processed_messages` 表或去重逻辑，这是明确的待增强点。

</details>

<details>
<summary><strong>CLI 通道和飞书通道的消息格式不同，是在哪一层做的格式抽象？抽象成什么内部结构？</strong></summary>

格式抽象在 `internal/channels` 层完成。

所有通道都实现统一 `Channel` 接口，输入统一成 `InboundMessage`，输出统一成 `OutboundMessage`。Agent、Gateway、Delivery 都不直接依赖 CLI 或飞书的原始消息格式。

`InboundMessage` 主要字段包括：

- `ID`
- `Text`
- `Channel`
- `AccountID`
- `PeerID`
- `SenderID`
- `ReplyToType`
- `IsGroup`
- `Media`
- `Raw`

代码位置：

- `internal/channels/channel.go:5`：`Channel` 接口。
- `internal/channels/message.go:5`：`InboundMessage`。
- `internal/channels/message.go:23`：`OutboundMessage`。
- `internal/channels/cli/channel.go:58`：CLI 输入转 `InboundMessage`。
- `internal/channels/feishu/channel.go:61`：飞书事件转 `InboundMessage`。
- `internal/channels/manager.go:40`：启动所有 Channel 并统一合并 inbound。

</details>

## Outbox 可靠投递

<details>
<summary><strong>文件型 Outbox 每条消息是一个独立文件还是 append 到一个文件？文件命名策略是什么？</strong></summary>

每条消息是一个独立 JSON 文件，不是 append 到一个大文件。

消息 ID 默认由 6 字节随机数生成并转成 hex 字符串；正式文件名是：

```text
{id}.json
```

写文件时先写临时文件：

```text
.tmp.*.json
```

然后 `Sync + Close + Rename`，保证写入原子性。

代码位置：

- `internal/delivery/outbox.go:28`：`Item` 结构。
- `internal/delivery/outbox.go:82`：`Enqueue`。
- `internal/delivery/outbox.go:92`：没有 ID 时生成新 ID。
- `internal/delivery/outbox.go:261`：`writeEntryLocked` 原子写入。
- `internal/delivery/outbox.go:265`：写 `.tmp.*.json`。
- `internal/delivery/outbox.go:283`：`tmp.Sync()`。
- `internal/delivery/outbox.go:290`：`os.Rename`。
- `internal/delivery/outbox.go:351`：正式文件路径 `{id}.json`。
- `internal/delivery/outbox.go:359`：`newDeliveryID`。

</details>

<details>
<summary><strong>消息状态（待发送 / 发送中 / 已发送 / 死信）是怎么表示的？用文件名还是文件内容里的字段？</strong></summary>

当前状态主要靠目录位置和文件存在性表示：

- 待发送：queue 根目录中的 `{id}.json`
- 已发送：`Ack` 后删除 `{id}.json`
- 死信：移动到 `failed/{id}.json`
- 发送中：当前没有显式 sending 状态或 claim 文件

重试相关状态放在文件内容字段里：

- `retry_count`
- `last_error`
- `next_retry_at`

代码位置：

- `internal/delivery/outbox.go:28`：`Item` 字段定义。
- `internal/delivery/outbox.go:110`：`Ack` 删除已发送文件。
- `internal/delivery/outbox.go:123`：`Fail` 更新重试状态。
- `internal/delivery/outbox.go:142`：达到最大重试次数后进入 failed。
- `internal/delivery/outbox.go:297`：`moveToFailedLocked`。
- `internal/delivery/outbox.go:355`：failed 文件路径。

</details>

<details>
<summary><strong>重试的退避策略是固定间隔还是指数退避？最大重试次数是多少，超过后进死信的处理流程是什么？</strong></summary>

当前是固定阶梯退避加 jitter，不是严格指数退避。

默认退避序列是：

```text
5s, 25s, 2min, 10min
```

每次会加正负 20% 左右的随机抖动。最大重试次数默认是 5，也可以通过配置里的 `delivery.max_retries` 改。超过最大次数后，先更新文件中的 `retry_count` 和 `last_error`，再把文件移动到 `failed/` 目录。后续可以用 CLI `/retry` 把 failed 消息重新放回队列。

代码位置：

- `internal/delivery/outbox.go:19`：`DefaultMaxRetries = 5`。
- `internal/delivery/outbox.go:21`：默认退避序列。
- `internal/delivery/outbox.go:56`：初始化最大重试次数。
- `internal/delivery/outbox.go:123`：失败处理。
- `internal/delivery/outbox.go:142`：判断是否达到最大重试次数。
- `internal/delivery/outbox.go:152`：设置 `NextRetryAt`。
- `internal/delivery/outbox.go:334`：计算退避和 jitter。
- `internal/delivery/outbox.go:174`：`RetryFailed`。
- `internal/app/app.go:335`：CLI `/queue`。
- `internal/app/app.go:337`：CLI `/failed`。
- `internal/app/app.go:339`：CLI `/retry`。

</details>

<details>
<summary><strong>投递协程和业务协程如何协作？投递协程是定时扫描文件还是通过 channel 通知？</strong></summary>

业务协程只负责把 `OutboundMessage` 入队到 Outbox；投递服务后台协程负责真正发送。

`Service.Start` 启动后台 goroutine。这个 goroutine 同时使用两种触发方式：

1. 定时 ticker 扫描 pending 文件。
2. 每次业务入队后通过 `wake` channel 立刻通知。

所以它不是纯定时扫描，也不是纯 channel 驱动，而是二者结合。

代码位置：

- `internal/app/app.go:230`：`deliver` 决定直接发送还是进入 Outbox。
- `internal/delivery/service.go:78`：启动投递 goroutine。
- `internal/delivery/service.go:99`：定时 ticker。
- `internal/delivery/service.go:105`：ticker 触发 `ProcessPending`。
- `internal/delivery/service.go:107`：`wake` channel 触发 `ProcessPending`。
- `internal/delivery/service.go:133`：业务入队。
- `internal/delivery/service.go:147`：入队后 `notify`。
- `internal/delivery/service.go:151`：扫描 pending 并发送。

</details>

<details>
<summary><strong>高并发下多个消息同时投递会不会有文件竞争？</strong></summary>

同一个进程内，`FileOutbox` 用 `sync.Mutex` 保护 `Enqueue / Ack / Fail / Pending / Failed / RetryFailed / Cleanup` 等文件操作，所以不会出现同一进程内的并发写竞争。

但当前没有跨进程文件锁，也没有显式 `sending` claim 文件。所以如果多个 AIHelper 进程共享同一个 Outbox 目录，可能出现重复读取和重复发送风险。这是当前实现边界。

代码位置：

- `internal/delivery/outbox.go:47`：`FileOutbox` 持有 `mu sync.Mutex`。
- `internal/delivery/outbox.go:99`：`Enqueue` 加锁。
- `internal/delivery/outbox.go:114`：`Ack` 加锁。
- `internal/delivery/outbox.go:127`：`Fail` 加锁。
- `internal/delivery/outbox.go:160`：`Pending` 加锁。

</details>

## 并发调度（Lane 模型）

<details>
<summary><strong>命名 Lane 的底层是 map[string]chan Message 吗？</strong></summary>

不是。

底层是：

```go
map[string]*LaneQueue
```

每个 `LaneQueue` 内部用 slice 保存队列：

```go
queue []queuedTask
```

并用 `sync.Mutex + sync.Cond` 管理状态。任务入队后，`pumpLocked` 会在 `active < maxConcurrency` 时从队首取任务并启动 goroutine 执行。

代码位置：

- `internal/concurrency/queue.go:34`：`CommandQueue`。
- `internal/concurrency/queue.go:36`：`lanes map[string]*LaneQueue`。
- `internal/concurrency/queue.go:145`：`LaneQueue`。
- `internal/concurrency/queue.go:148`：`queue []queuedTask`。
- `internal/concurrency/queue.go:151`：`sync.Mutex`。
- `internal/concurrency/queue.go:152`：`sync.Cond`。
- `internal/concurrency/queue.go:283`：`pumpLocked`。

</details>

<details>
<summary><strong>Lane 在没有消息时怎么回收，用了什么机制（idle timeout / 引用计数）？</strong></summary>

当前没有 Lane 回收机制。

Lane 一旦通过 `GetOrCreateLane` 创建，就会保留在 `CommandQueue.lanes` map 中。当前启动时预创建了 `main / heartbeat / cron` 三个 Lane，后续如果创建自定义 Lane，也不会自动根据 idle timeout 或引用计数回收。

代码位置：

- `internal/concurrency/queue.go:43`：`GetOrCreateLane` 只创建或返回 Lane。
- `internal/concurrency/queue.go:53`：如果不存在则创建。
- `internal/concurrency/queue.go:56`：写入 map。
- 当前没有 delete lane 或 idle cleanup 逻辑。

</details>

<details>
<summary><strong>同一 Lane 内串行处理，如果某条消息处理耗时很长，后续消息会积压，有没有设置 channel buffer 大小或者背压机制？</strong></summary>

同一 Lane 默认并发度是 1，所以某条消息处理很慢，后续同 Lane 任务会积压在 `LaneQueue.queue` 里。

当前 Lane 队列是 slice，没有最大长度限制，也没有显式背压。可以通过 CLI `/concurrency <lane> <N>` 动态调高某个 Lane 的并发度，但这会影响同 Lane 内的串行语义。

通道入口层有 buffer，`channels.NewManager(128)` 初始化 inbound/errors channel 缓冲区为 128；但进入 Lane 后没有队列上限。

代码位置：

- `internal/app/app.go:132`：初始化 `CommandQueue`。
- `internal/app/app.go:133`：`main` Lane 默认并发 1。
- `internal/app/app.go:134`：`heartbeat` Lane 默认并发 1。
- `internal/app/app.go:135`：`cron` Lane 默认并发 1。
- `internal/concurrency/queue.go:174`：任务入队到 slice。
- `internal/concurrency/queue.go:205`：`SetConcurrency`。
- `internal/app/app.go:323`：CLI `/concurrency`。
- `internal/channels/manager.go:16`：Channel manager buffer 默认逻辑。
- `internal/app/app.go:1152`：当前 manager buffer 设置为 128。

</details>

## 模型调用层

<details>
<summary><strong>API Key 轮换是轮询（round-robin）还是按错误率动态选择？</strong></summary>

当前既不是 round-robin，也不是按错误率动态选择。

`ProfileManager.SelectAvailable` 会按配置顺序选择第一个不在 cooldown、且本轮还没有试过的 profile。失败后根据错误类型给这个 profile 打 cooldown；本轮继续尝试下一个可用 profile。

代码位置：

- `internal/resilience/profile.go:66`：`SelectAvailable`。
- `internal/resilience/profile.go:70`：按当前时间判断 cooldown。
- `internal/resilience/profile.go:71`：按 profiles 顺序遍历。
- `internal/resilience/client.go:113`：`runWithProfiles`。
- `internal/resilience/client.go:118`：循环尝试可用 profile。
- `internal/resilience/client.go:171`：失败后 `MarkFailure`。

</details>

<details>
<summary><strong>Key 被限流和 Key 过期怎么区分处理？</strong></summary>

通过错误分类区分。

HTTP 429 被归类为 `rate_limit`，会给当前 profile 设置较短 cooldown，默认 120 秒。  
HTTP 401/403 被归类为 `auth`，通常代表 key 失效、权限问题或认证失败，会给当前 profile 设置更长 cooldown，默认 300 秒。  
HTTP 402 被归类为 `billing`，同样使用 300 秒 cooldown。

代码位置：

- `internal/llm/openai.go:35`：`HTTPError` 保存 HTTP status。
- `internal/llm/openai.go:43`：`HTTPStatusCode`。
- `internal/resilience/reason.go:25`：`ClassifyFailure`。
- `internal/resilience/reason.go:30`：识别 HTTP status。
- `internal/resilience/reason.go:33`：401/403 => auth。
- `internal/resilience/reason.go:35`：402 => billing。
- `internal/resilience/reason.go:39`：429 => rate_limit。
- `internal/resilience/reason.go:84`：不同 reason 的 cooldown。

</details>

<details>
<summary><strong>fallback 模型降级的触发条件是什么？</strong></summary>

fallback 模型在主模型通过所有可用 profile 都失败后触发。

流程是：先用原始 request 跑 `runWithProfiles`；如果失败，再遍历 `fallbackModels`，把 request 的 `Model` 改成 fallback model，再重新跑 profile 尝试。

代码位置：

- `internal/resilience/client.go:68`：`CreateMessage`。
- `internal/resilience/client.go:73`：先跑原始请求。
- `internal/resilience/client.go:82`：开始遍历 fallback models。
- `internal/resilience/client.go:89`：复制 request。
- `internal/resilience/client.go:90`：替换模型为 fallback。
- `internal/app/app.go:1008`：创建 `ResilientClient` 时注入 fallback models。
- `internal/config/config.go:33`：配置字段 `FallbackModels`。

</details>

<details>
<summary><strong>降级到备用模型后，原模型恢复了怎么切回来，有没有熔断恢复机制？</strong></summary>

当前没有持久化降级状态，也没有半开探测之类的完整熔断恢复机制。

每次新的 `CreateMessage` 都会先从原始模型开始尝试；只有这次请求原始模型失败后，才进入 fallback 模型。所以“切回来”是天然的：下一次请求会重新先试原模型。

profile 的恢复靠 cooldown 到期。`SelectAvailable` 只检查 `CooldownUntil`，时间到了就重新可用。fallback 前会对 `rate_limit` 和 `timeout` 类型的 cooldown 做一次 reset，让备用模型有机会复用这些 key。

代码位置：

- `internal/resilience/client.go:73`：每次先用原始 request。
- `internal/resilience/client.go:87`：fallback 前 reset rate_limit/timeout cooldown。
- `internal/resilience/profile.go:75`：cooldown 到期即可被选中。
- `internal/resilience/profile.go:113`：`ResetCooldownsFor`。

</details>

<details>
<summary><strong>上下文压缩是用摘要还是直接截断？触发压缩的条件是什么？</strong></summary>

当前是二段式：先截断过长工具结果，再用 LLM 摘要旧历史。

触发条件主要是模型调用失败后被 `ClassifyFailure` 归类为 `overflow`，也就是 context length、too many tokens、context window 等错误。当前不是请求前主动根据 token 阈值压缩；`EstimateTokens` 和 `ContextSafeTokens` 已经有，但没有在发送前做主动阈值判断。

压缩过程：

1. `TruncateToolResults` 截断超长 `role=tool` 消息。
2. `CompactHistory` 把较旧消息交给 LLM 总结。
3. 保留最近消息，并在最前面插入一条 `[Previous conversation summary]`。

代码位置：

- `internal/resilience/client.go:158`：识别 overflow。
- `internal/resilience/client.go:161`：截断工具结果。
- `internal/resilience/client.go:164`：压缩历史。
- `internal/resilience/context_guard.go:52`：`TruncateToolResults`。
- `internal/resilience/context_guard.go:66`：`CompactHistory`。
- `internal/resilience/context_guard.go:84`：摘要 prompt。
- `internal/resilience/context_guard.go:102`：插入 previous conversation summary。
- `internal/resilience/context_guard.go:35`：token 估算函数，目前不是主动触发逻辑的一部分。

</details>

<details>
<summary><strong>压缩后怎么保证重要信息不丢失？</strong></summary>

当前不能 100% 保证不丢，只做了工程上的降低风险。

具体策略是：摘要 prompt 要求保留关键事实和决策；同时不是压缩全部历史，而是保留最近消息，把旧消息摘要成一段 summary。对于长期重要信息，系统还提供 `memory_write`，可以把稳定事实写入 Agent workspace 下的长期记忆。

面试时可以如实说：当前是“摘要压缩 + 近期上下文保留 + 长期记忆工具”的组合，但摘要本身仍有信息损失风险。后续可以做结构化摘要、重要事实抽取、摘要 diff 校验，或者把用户偏好/项目决策强制写入 memory。

代码位置：

- `internal/resilience/context_guard.go:73`：保留一部分 recent messages。
- `internal/resilience/context_guard.go:84`：摘要 prompt 要求保留 key facts and decisions。
- `internal/resilience/context_guard.go:99`：摘要失败则只保留近期消息。
- `internal/intelligence/memory.go:56`：`WriteMemory`。
- `internal/tools/memory.go:61`：`memory_write` tool 调用记忆服务。

</details>

## Prompt 与记忆

<details>
<summary><strong>Markdown Bootstrap 提示词层是什么意思？和普通 system prompt 有什么区别？</strong></summary>

Markdown Bootstrap 提示词层就是把每个 Agent workspace 里的 Markdown 文件加载出来，按层组装成最终 system prompt。

加载的文件包括：

- `SOUL.md`
- `IDENTITY.md`
- `TOOLS.md`
- `MEMORY.md`

和普通 hardcoded system prompt 的区别是：普通 system prompt 通常写死在代码里；这里把人格、边界、工具说明、长期记忆等拆成文件层，不同 Agent 可以拥有不同 workspace，不改代码就能调整行为。`HEARTBEAT.md` 不进入普通 system prompt，只由 heartbeat runner 单独读取。

代码位置：

- `internal/intelligence/bootstrap.go:15`：`BootstrapFiles`。
- `internal/intelligence/bootstrap.go:40`：`LoadAll`。
- `internal/intelligence/service.go:157`：`Reload` 预加载每个 Agent 的 prompt 资料。
- `internal/intelligence/service.go:634`：`buildPromptDebug` 组装最终 prompt。
- `internal/intelligence/service.go:648`：Identity 层。
- `internal/intelligence/service.go:654`：Soul 层。
- `internal/intelligence/service.go:659`：Tools 层。
- `internal/intelligence/service.go:664`：Skills metadata catalog 和 Active Skills 层。
- `internal/intelligence/service.go:673`：Memory 层。
- `internal/intelligence/service.go:690`：Runtime 层。
- `internal/intelligence/service.go:698`：Channel 层。

</details>

<details>
<summary><strong>Skill 是启动时全部塞进 system prompt 吗？现在怎么避免 skill prompt 过大？</strong></summary>

不是。当前实现已经从“全量 skill 正文注入”改成了“metadata 常驻 + 按需正文注入”。

启动或 `/intelligence/reload` 时，`Reload` 会扫描每个 Agent 的 skill roots，但只缓存 metadata：

- `name`
- `description`
- `invocation`
- `path`
- `sourceRoot`
- `plugin`
- debug status

`SKILL.md` 正文不会在启动时进入 `agentCache`，也不会出现在常驻 `Skills` prompt 里。常驻 prompt 只生成一个 `## Available Skills` catalog，告诉模型有哪些 skill、每个 skill 适合什么场景。

每轮用户输入后，如果是 full mode，程序会先调用一次 LLM skill selector。selector 只看到用户输入和 skill metadata，返回 JSON：

```json
{"skills":["skill-name"]}
```

也可以返回：

```json
{"skills":[]}
```

返回空数组表示本轮不需要 skill。程序只会读取 selector 选中的 `SKILL.md` 正文，并把正文注入 `## Active Skills`。所以没命中的 skill 正文不会进入上下文。

此外，selector 的输出不是安全边界：不存在、禁用、重复、name mismatch 的 skill 会被忽略；默认最多注入 3 个 skill；selector 失败时不会中断回答，只会在 `/prompt` warning 里体现。

代码位置：

- `internal/intelligence/service.go:157`：`Reload` 对每个 Agent 预加载 bootstrap 和 skill metadata。
- `internal/intelligence/service.go:192`：缓存启用 skill metadata，不缓存正文。
- `internal/intelligence/service.go:199`：生成 metadata-only skill catalog。
- `internal/intelligence/skills.go:83`：扫描 skill roots 并生成 debug 状态。
- `internal/intelligence/skills.go:137`：校验 `SKILL.md` 的 `name` 必须和目录名一致。
- `internal/intelligence/skills.go:199`：`FormatPromptBlock` 只输出 metadata catalog。
- `internal/intelligence/service.go:390`：`SelectSkills` 调用 LLM selector。
- `internal/intelligence/service.go:458`：selector prompt 只暴露 metadata。
- `internal/intelligence/service.go:476`：命中后读取正文并生成 `## Active Skills`。

</details>

<details>
<summary><strong>Skill 目录里的 reference 和脚本是怎么用的？安全边界在哪里？</strong></summary>

Skill 目录格式是：

```text
skills/<skill-name>/
  SKILL.md
  guide.md          # 可选 reference
  run.sh            # 可选脚本
  scripts/xxx       # 可选脚本或资源
```

`SKILL.md` 是唯一会被 Active Skills 自动读取的文件。reference 和脚本不会自动读取，也不会在 selector 阶段暴露。

如果 `SKILL.md` 正文提示“需要更多背景时读取 guide.md”，模型可以在回答阶段调用 `read_skill_reference`。这个工具只允许读取当前已启用 skill 目录内的额外 `.md` 文件，拒绝：

- `SKILL.md`
- 非 `.md` 文件
- 绝对路径
- `../` 路径穿越
- symlink

如果 `SKILL.md` 正文提示“运行 `sh run.sh`”，模型可以调用 `run_skill_command`。脚本不限语言，关键约束是：传入的 `command` 必须在对应 `SKILL.md` 中逐字出现。命令会在 skill 目录作为 cwd 执行，有超时和 stdout/stderr 输出上限，工具只返回 exit code、timeout、stdout、stderr，不提供脚本源码读取能力。

代码位置：

- `internal/tools/skills.go:11`：`SkillService` 暴露 reference 和 command 能力。
- `internal/tools/skills.go:38`：`read_skill_reference` tool。
- `internal/tools/skills.go:98`：`run_skill_command` tool。
- `internal/intelligence/skill_runtime.go:21`：`ReadSkillReference` 的路径和文件类型校验。
- `internal/intelligence/skill_runtime.go:71`：`RunSkillCommand` 校验命令必须出现在 `SKILL.md`。
- `internal/intelligence/skill_runtime.go:90`：命令在 skill 目录下执行。
- `internal/intelligence/skill_runtime.go:160`：symlink 和真实路径校验。
- `internal/app/app.go:98`：注册 skill 工具。
- `internal/app/app.go:1251`：full-mode intelligence Agent 自动获得 skill 工具。

</details>

<details>
<summary><strong>混合记忆检索具体是什么和什么混合？是短期上下文 + 长期向量检索吗？</strong></summary>

当前混合检索不是“短期上下文 + 长期向量检索”的混合，而是长期记忆内部的两种检索方式混合。

数据来源是 Agent workspace 下的：

- `MEMORY.md`：evergreen memory
- `memory/daily/*.jsonl`：每日追加的长期记忆

检索方式混合：

1. TF-IDF 关键词检索。
2. 向量相似度检索。如果配置了 OpenAI-compatible embedding，就调用 embedding 服务；否则使用 hash-vector 回退。

然后用 0.7 / 0.3 加权合并，再做时间衰减和 MMR rerank。

代码位置：

- `internal/intelligence/memory.go:99`：读取 `MEMORY.md`。
- `internal/intelligence/memory.go:177`：读取 `memory/daily/*.jsonl`。
- `internal/intelligence/memory.go:124`：`HybridSearchWithWarnings`。
- `internal/intelligence/memory.go:142`：关键词检索。
- `internal/intelligence/memory.go:143`：向量检索。
- `internal/intelligence/memory.go:144`：0.7 / 0.3 合并。
- `internal/intelligence/memory.go:145`：时间衰减。
- `internal/intelligence/memory.go:146`：MMR rerank。
- `internal/intelligence/embedding.go:22`：`Embedder` 接口。

</details>

<details>
<summary><strong>向量存储用了什么？还是用 SQLite 做了简单关键词检索？</strong></summary>

当前没有专门的向量数据库，也没有把向量存到 SQLite。

记忆是文件型存储：`MEMORY.md` 和每日 JSONL。每次检索时加载 memory chunks，然后现场计算关键词分数和向量相似度。向量部分如果配置了 embedder，会调用 embedding API；如果没有配置或者调用失败，就用 hash-vector 作为本地回退。

代码位置：

- `internal/intelligence/memory.go:163`：加载所有 memory chunks。
- `internal/intelligence/memory.go:229`：TF-IDF 检索。
- `internal/intelligence/memory.go:264`：向量检索。
- `internal/intelligence/memory.go:289`：查询向量生成。
- `internal/intelligence/memory.go:470`：本地 hash-vector。
- `internal/intelligence/embedding.go:52`：OpenAI-compatible embedding 请求。

</details>

<details>
<summary><strong>Cron 心跳任务具体触发什么行为？是主动给用户发消息还是做内部状态维护？</strong></summary>

两者都有，但主要是主动任务触发后可能给用户发消息。

Heartbeat 会读取 Agent workspace 下的 `HEARTBEAT.md`，按 interval 和 active hours 判断是否运行，然后构造一条定时任务消息交给 Agent 执行。如果 Agent 返回空或 `HEARTBEAT_OK`，就抑制发送；如果返回有意义内容，就通过配置的 target 主动发送给用户或群。

Cron 会从 `CRON.json` 加载任务，支持 `at / every / cron` 三种调度。任务 payload 可以是 `agent_turn`，也可以是 `system_event`。执行后如果有输出，会发送到 target，并追加运行日志。

代码位置：

- `internal/heartbeat/runner.go:164`：Heartbeat 主运行逻辑。
- `internal/heartbeat/runner.go:238`：读取 `HEARTBEAT.md` 并判断是否 should run。
- `internal/heartbeat/runner.go:191`：调用 AgentTurn。
- `internal/heartbeat/runner.go:210`：解析 Agent 输出。
- `internal/heartbeat/runner.go:224`：发送有意义输出。
- `internal/heartbeat/cron.go:165`：Cron tick。
- `internal/heartbeat/cron.go:298`：执行 Cron job。
- `internal/heartbeat/cron.go:316`：`agent_turn` 调用 Agent。
- `internal/heartbeat/cron.go:328`：`system_event`。
- `internal/heartbeat/cron.go:342`：发送 Cron 输出。
- `internal/heartbeat/schedule.go:20`：计算下一次执行时间。

</details>

## 测试

<details>
<summary><strong>103 个单元测试，测试覆盖率大概是多少？有没有跑过 go test -race 检查竞态条件？</strong></summary>

当前我实际验证过：

```bash
go test ./...
go test -race ./...
go test -cover ./...
go test -coverpkg=./internal/... -coverprofile=/private/tmp/aihelper-cover.out ./internal/...
go tool cover -func=/private/tmp/aihelper-cover.out
```

结果：

- `go test ./...` 全部通过。
- `go test -race ./...` 全部通过，没有发现 data race。
- `go tool cover -func` 统计总覆盖率是 `70.8% statements`。

普通包级覆盖率中，几个核心包大致如下：

- `internal/agent`：75.4%
- `internal/app`：60.2%
- `internal/concurrency`：66.1%
- `internal/delivery`：72.6%
- `internal/intelligence`：72.9%
- `internal/resilience`：76.8%
- `internal/sessions`：70.1%
- `internal/tools`：71.6%
- `internal/config`：88.0%

</details>

<details>
<summary><strong>飞书 SDK 的调用在测试里怎么 mock 的？是用 interface 抽象还是直接 monkey patch？</strong></summary>

用 interface 抽象，不是 monkey patch。

飞书通道定义了两个接口：

- `Client`：负责接收消息事件。
- `Sender`：负责发送消息。

生产环境注入 SDK 实现；测试里注入 `fakeClient` 和 `fakeSender`。这样测试可以直接模拟飞书事件输入，也可以验证发送时的参数，不需要真实连接飞书。

代码位置：

- `internal/channels/feishu/types.go:18`：`Client` 接口。
- `internal/channels/feishu/types.go:27`：`Sender` 接口。
- `internal/channels/feishu/channel.go:18`：`New` 注入 client 和 sender。
- `internal/channels/feishu/sdk.go:15`：生产 SDK client。
- `internal/channels/feishu/sdk.go:47`：生产 SDK sender。
- `internal/channels/feishu/channel_test.go:11`：`fakeClient`。
- `internal/channels/feishu/channel_test.go:27`：`fakeSender`。

</details>

<details>
<summary><strong>LLM 调用在测试里是真实调用还是 mock 返回？mock 的话是怎么构造 fixture 数据的？</strong></summary>

测试里不调用真实 LLM。

模型层定义了 `llm.Client` 接口，生产实现是 OpenAI-compatible client，测试用 `MockClient` 或测试内自定义 fake client。mock 数据直接构造 `llm.Response`，包括：

- dispatch JSON：`{"mode":"direct|delegate","agent_id":"...","input":"..."}`
- skill selector JSON：`{"skills":[]}` 或 `{"skills":["skill-name"]}`
- tool call：`StopReasonToolUse` + `ToolCalls`
- final answer：`StopReasonEndTurn` + `Text`

代码位置：

- `internal/llm/client.go:5`：`Client` 接口。
- `internal/llm/mock.go:15`：`MockClient.CreateMessage`。
- `internal/llm/mock.go:33`：mock 的 `skill_select` 默认返回空 skill。
- `internal/agent/runner_test.go:36`：测试用 fake LLM client。
- `internal/agent/runner_test.go:40`：按 `req.Purpose` 返回 dispatch 或 answer。
- `internal/agent/runner_test.go:50`：构造 tool call 返回。
- `internal/agent/runner_test.go:60`：构造 final answer。
- `internal/intelligence/intelligence_test.go:326`：测试 selector 命中后注入 Active Skill。
- `internal/llm/openai_test.go`：OpenAI 请求构造和响应解析测试使用 HTTP test server。

</details>

<details>
<summary><strong>这些测试主要覆盖了哪些关键链路？</strong></summary>

测试覆盖的重点是核心行为而不是只测 happy path。

主要包括：

- Agent dispatch 到 coder/writer specialist。
- direct answer 路径。
- 非法 child delegate 拒绝。
- 工具调用后的多轮 LLM loop。
- PromptBuilder 只在 answer 阶段使用。
- SQLite session 持久化、tool calls 保存、session metadata。
- CachedStore 先写 disk 再写 cache。
- Feishu 私聊/群聊 mention/image 解析。
- Channel manager inbound 合并和 outbound 路由。
- Router 默认绑定和 dm scope session key。
- FileOutbox 入队、ack、fail、dead letter、retry failed、清理临时文件。
- Delivery service 成功 ack、失败重试、唤醒处理、长消息分块。
- Resilience failure classify、profile cooldown、rate limit 轮换、timeout cooldown、fallback model、overflow compact。
- Lane FIFO、跨 Lane 并发、同 Lane 并发度调整、future timeout、reset generation。
- Heartbeat active hours、重复输出抑制、busy 状态。
- Cron at/every/cron 调度、agent_turn/system_event、连续错误自动 disable。
- Intelligence bootstrap、skill metadata catalog、LLM skill selector、Active Skills 注入、skill reference/command 工具、memory search、prompt layers、embedding fallback。

可以通过下面命令查看测试列表：

```bash
rg "^func Test" internal -n
```

</details>
