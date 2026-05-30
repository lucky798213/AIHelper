# AIHelper

> 一个用 Go 编写的 AI Agent Gateway：将 OpenAI-compatible 大模型接入 CLI 和飞书/Lark，并通过多 Agent 路由、工具调用、会话记忆、定时任务与可靠投递完成本地自动化协作。

![Go](https://img.shields.io/badge/Go-1.26+-00ADD8?logo=go&logoColor=white)
![LLM](https://img.shields.io/badge/LLM-OpenAI--compatible-412991)
![Status](https://img.shields.io/badge/status-learning_project-blue)

## 项目描述

AIHelper 是一个面向学习和本地实践的 AI Agent 网关项目。它的目标不是再封装一个聊天机器人，而是把一个可扩展 Agent 系统里最容易变乱的部分拆清楚：消息入口、路由、会话、工具、记忆、后台任务和失败恢复。

在接入大模型应用时，常见痛点包括：

- 不同平台的消息结构差异很大，业务逻辑容易被 CLI、飞书、Webhook 等平台细节污染。
- 工具调用如果缺少权限边界，模型可能看到或调用不该使用的能力。
- 多轮会话、上下文溢出、失败重试、消息投递这些工程问题容易散落在各处。
- 单 Agent 很快会承担过多职责，复杂任务需要更清晰的 master/specialist 分工。

AIHelper 通过 Go 的包边界把这些问题拆成独立模块。外部消息会先被标准化为统一的 `InboundMessage`，再由 `gateway.Router` 匹配到对应 Agent 和会话，最后进入 `agent.Runner` 完成 LLM 推理、工具调用和结果投递。

主要特性：

- **多 Agent 分发**：master Agent 负责判断直接回答还是委托给 specialist Agent。
- **OpenAI-compatible LLM 接入**：支持 OpenAI 兼容的 chat completions API，并提供 fallback 与失败分类。
- **多通道适配**：内置 CLI 和飞书/Lark channel，核心 Agent 不依赖平台字段。
- **工具注册表**：内置文件工具和记忆工具，支持按 Agent 白名单控制可用工具。
- **会话持久化**：支持 SQLite session store，并叠加内存缓存。
- **智能上下文层**：从 `workspace/agents/*` 加载身份、工具说明、记忆和技能。
- **可靠投递**：使用文件型 outbox 保存待投递消息，支持重试和失败队列。
- **后台任务**：支持 heartbeat 和 cron，让 Agent 可以被定时触发。

## 快速开始 / 安装指南

### 前置依赖

- Go 1.26 或更高版本
- Git
- 一个 OpenAI-compatible LLM API Key，例如 DeepSeek、OpenAI 或其他兼容服务
- 可选：飞书/Lark 应用凭证，用于启用飞书通道

### 1. 克隆仓库

```bash
git clone git@github.com:lucky798213/AIHelper.git
cd AIHelper
```

### 2. 准备配置文件

```bash
cp configs/dev.example.yaml configs/dev.yaml
```

`configs/dev.yaml` 是本地运行配置，已经被 `.gitignore` 排除，请不要提交真实密钥。

推荐使用环境变量注入密钥：

```bash
export AIHELPER_LLM_API_KEY="your-api-key"
export AIHELPER_FEISHU_APP_ID="your-feishu-app-id"
export AIHELPER_FEISHU_APP_SECRET="your-feishu-app-secret"
export AIHELPER_FEISHU_BOT_OPEN_ID="your-bot-open-id"
```

如果只使用 CLI，本地配置里可以保持飞书通道关闭。

### 3. 安装依赖并运行测试

```bash
go mod download
go test ./internal/...
```

### 4. 启动项目

```bash
go run ./cmd/aihelper -config configs/dev.yaml
```

也可以先构建二进制：

```bash
go build ./cmd/aihelper
./aihelper -config configs/dev.yaml
```

## 使用示例

### CLI 对话

启动后，CLI channel 会等待用户输入。你可以直接输入自然语言任务：

```txt
你能帮我总结一下这个项目的架构吗？
```

master Agent 会先判断任务应该自己回答，还是委托给子 Agent。例如代码相关任务可以路由给 `coder-agent`，文档表达类任务可以路由给 `writer-agent`。

### 常用 CLI 命令

项目内置了一组 slash commands，用于查看会话、队列、记忆和后台任务状态：

```txt
/help
/sessions
/new
/switch <session-key>
/export
/compact
/memory <agent-id>
/search <agent-id> <query>
/skills <agent-id>
/heartbeat
/cron
/delivery
/failed
```

### 配置 Agent

默认 Agent 工作区位于：

```txt
workspace/agents/local-master/
workspace/agents/coder-agent/
workspace/agents/writer-agent/
```

每个 Agent 可以通过 Markdown 文件描述自己的身份、边界和工具使用方式：

```txt
IDENTITY.md   Agent 身份和职责
SOUL.md       表达风格与行为倾向
TOOLS.md      工具使用说明
MEMORY.md     长期稳定记忆
HEARTBEAT.md  心跳任务提示
```

修改这些文件后，可以在 CLI 中执行：

```txt
/intelligence/reload
```

让运行中的服务重新加载 Agent 上下文。

### 飞书/Lark 通道

如果要启用飞书/Lark，需要在 `configs/dev.yaml` 中开启 `channels.feishu.enabled`，并配置对应凭证或环境变量：

```yaml
channels:
  feishu:
    enabled: true
    account_id: feishu-primary
    app_id: ""
    app_secret: ""
    bot_open_id: ""
    require_mention: true
    is_lark: false
```

建议把真实凭证放在环境变量中，而不是写入仓库文件。

## 项目目录

```txt
.
├── cmd/aihelper/              程序入口，负责加载配置并启动应用
├── configs/                   示例配置文件
├── docs/                      架构说明、学习笔记和面试问答
├── internal/agent/            Agent 定义、master/specialist 分发和工具循环
├── internal/app/              应用组装、消息主循环、CLI 命令和后台服务接线
├── internal/channels/         通道抽象与 CLI、飞书/Lark 实现
├── internal/config/           YAML 配置加载与环境变量覆盖
├── internal/concurrency/      基于 lane 的并发队列
├── internal/delivery/         可靠投递 outbox、重试和失败队列
├── internal/gateway/          消息路由、binding 匹配和会话 key 生成
├── internal/heartbeat/        heartbeat 和 cron 定时任务
├── internal/intelligence/     prompt 组装、技能发现、记忆写入和检索
├── internal/llm/              LLM 客户端抽象、mock 和 OpenAI-compatible 实现
├── internal/resilience/       失败原因分类、profile 轮换和上下文恢复
├── internal/sessions/         内存、SQLite 与缓存型 session store
├── internal/tools/            文件工具、记忆工具和工具注册表
└── workspace/agents/          默认 Agent 工作区模板
```

## 文档

- [Go 架构调研与设计建议](docs/claw0-go-architecture.md)
- [claw0 Go 学习文档](docs/claw0-go-learning-guide.md)
- [AIHelper 面试问答](docs/aihelper-interview-qa.md)

## 安全说明

- `configs/dev.yaml`、本地数据库、构建产物和运行时日志不会提交到 Git。
- 请优先使用环境变量保存 API Key 和飞书/Lark Secret。
- 如果误把真实密钥暴露到公开仓库，应立即在对应平台轮换密钥。

## 当前状态

AIHelper 目前是个人学习和工程化实践项目，核心包已经配套测试，适合用于理解 AI Agent Gateway 的架构拆分、工具调用边界和本地自动化工作流。
