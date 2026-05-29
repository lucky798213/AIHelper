# AIHelper

AIHelper is a Go implementation of an AI Agent Gateway. It connects OpenAI-compatible LLMs with local CLI and Feishu/Lark channels, then routes messages through a master/specialist multi-agent system with tool use, sessions, memory, heartbeat jobs, cron jobs, and reliable delivery.

The project is inspired by the `claw0` educational agent gateway, but organizes the ideas into production-style Go packages under `internal/`.

## Features

- Multi-agent dispatch with master and specialist roles.
- OpenAI-compatible chat completion client with fallback and resilience handling.
- CLI and Feishu/Lark channel adapters.
- Tool registry with per-agent tool allowlists.
- SQLite-backed session storage with in-memory caching.
- File-based reliable delivery outbox.
- Agent workspace loading for identity, tools, memory, and skills.
- Heartbeat and cron background tasks.

## Project Layout

```txt
cmd/aihelper/              CLI entrypoint
configs/                   Example YAML config
docs/                      Architecture and learning notes
internal/agent/            Agent runner, dispatch, and tool loop
internal/app/              Application wiring and runtime loop
internal/channels/         CLI and Feishu/Lark channel adapters
internal/config/           YAML config and environment overrides
internal/concurrency/      Lane-based command queue
internal/delivery/         Durable outbox and retry logic
internal/gateway/          Message routing and binding resolution
internal/heartbeat/        Heartbeat and cron services
internal/intelligence/     Prompt assembly, skills, and memory search
internal/llm/              LLM client abstraction and OpenAI-compatible client
internal/resilience/       Failure classification and context recovery
internal/sessions/         Memory, SQLite, and cached session stores
internal/tools/            Built-in file and memory tools
workspace/agents/          Default agent workspace templates
```

## Quick Start

```bash
cp configs/dev.example.yaml configs/dev.yaml
```

Edit `configs/dev.yaml` or provide secrets through environment variables:

```bash
export AIHELPER_LLM_API_KEY="your-api-key"
export AIHELPER_FEISHU_APP_ID="your-feishu-app-id"
export AIHELPER_FEISHU_APP_SECRET="your-feishu-app-secret"
export AIHELPER_FEISHU_BOT_OPEN_ID="your-bot-open-id"
```

Build and run:

```bash
go build ./cmd/aihelper
go run ./cmd/aihelper -config configs/dev.yaml
```

Run the test suite:

```bash
go test ./internal/...
```

## Configuration

`configs/dev.example.yaml` contains the default development configuration. The real `configs/dev.yaml` file is intentionally ignored because it may contain API keys, app secrets, local database paths, and other machine-specific settings.

Supported secret overrides:

- `AIHELPER_LLM_API_KEY`
- `AIHELPER_EMBEDDING_API_KEY`
- `AIHELPER_FEISHU_APP_ID`
- `AIHELPER_FEISHU_APP_SECRET`
- `AIHELPER_FEISHU_BOT_OPEN_ID`

## Documentation

- [Go architecture notes](docs/claw0-go-architecture.md)
- [Learning guide](docs/claw0-go-learning-guide.md)
- [Interview Q&A](docs/aihelper-interview-qa.md)

## Status

This repository is a personal learning and implementation project. The core packages are covered by tests, and the runtime is suitable for local experimentation with CLI-first agent workflows.
