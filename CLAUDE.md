# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Build & Test

```bash
# Build
go build ./cmd/aihelper

# Run (requires configs/dev.yaml)
go run ./cmd/aihelper -config configs/dev.yaml

# Run all tests
go test ./internal/...

# Run tests for a single package
go test ./internal/agent/...
go test ./internal/channels/feishu/...

# Run a single test
go test ./internal/agent/ -run TestRunner -v
```

No linting or code-generation tools are configured. There is no Makefile.

## Architecture

This is a Go implementation of an AI Agent Gateway, inspired by the Python `claw0` educational project. It connects LLMs to messaging channels (CLI, Feishu/Lark) through a multi-agent dispatch system.

### Message Flow

```
Channel (CLI/Feishu) → channels.Manager → App.Run loop
  → gateway.Router (match binding → agent + session key)
  → concurrency.CommandQueue (lane-based concurrency)
  → agent.Runner.RunTurn
    → dispatch (master agent decides: direct answer or delegate to specialist)
    → runAgent (tool-use loop, up to 5 iterations)
  → delivery.Service (reliable outbox with retry) → channel.Send
```

### Package Roles

| Package | Responsibility |
|---------|---------------|
| `internal/agent` | Agent definitions (master/specialist), Manager, Runner with dispatch+tool loop |
| `internal/channels` | `Channel` interface + Manager. Impls: `cli/` (stdin/stdout), `feishu/` (Lark SDK) |
| `internal/gateway` | `Router` matches inbound messages to agents via tiered binding rules |
| `internal/intelligence` | System prompt builder: loads bootstrap files, discovers skills, memory recall |
| `internal/llm` | LLM client abstraction (`Client` interface), OpenAI-compatible impl, mock |
| `internal/resilience` | API key profile rotation, fallback models, context overflow compaction |
| `internal/sessions` | Session persistence: MemoryStore, SQLite, CachedStore (memory cache + SQLite) |
| `internal/tools` | Tool registry with per-agent tool whitelists. File ops + memory ops built-in |
| `internal/concurrency` | Named lane queue (`LaneQueue`) with configurable max concurrency |
| `internal/delivery` | Reliable outbox: file-based queue with retry, pending/failed management |
| `internal/heartbeat` | Background agents: interval-based heartbeat + cron-scheduled jobs |
| `internal/config` | YAML config loading with env var overrides for secrets |

### Agent Roles

- **master** — Entry point for messages. Performs a dispatch step (LLM call) to decide whether to answer directly or delegate to a child specialist. Only masters can be bound in gateway routes.
- **specialist** — Child agents that handle delegated tasks. Has its own session, tools, and workspace.

### Intelligence Layer

At startup, `intelligence.Service.Reload()` scans each agent's workspace directory (`workspace/agents/<agent-id>/`) and loads markdown files into an in-memory cache:

- `IDENTITY.md`, `SOUL.md` — Agent persona
- `BOOTSTRAP.md`, `AGENTS.md`, `TOOLS.md`, `HEARTBEAT.md`, `USER.md` — Context layers
- `MEMORY.md` — Evergreen memory (statically loaded)
- Skills (discovered from `skill_roots` config or agent workspace)

Dynamic memory recall happens per-turn via keyword + embedding hybrid search against daily memory files.

Three prompt modes per agent: `full` (all layers), `minimal` (bootstrap only, no skills/memory), `none` (base system prompt only).

### Config

- `configs/dev.yaml` — Main config. `configs/dev.example.yaml` for reference.
- Secrets can be set via env vars: `AIHELPER_LLM_API_KEY`, `AIHELPER_EMBEDDING_API_KEY`, `AIHELPER_FEISHU_APP_ID`, `AIHELPER_FEISHU_APP_SECRET`, `AIHELPER_FEISHU_BOT_OPEN_ID`.

### CLI Commands

Built-in slash commands available in CLI mode: `/help`, `/sessions`, `/new`, `/switch`, `/delete`, `/export`, `/compact`, `/lanes`, `/concurrency`, `/queue`, `/failed`, `/retry`, `/delivery`, `/heartbeat`, `/heartbeat-trigger`, `/cron`, `/cron-trigger`, `/prompt`, `/bootstrap`, `/skills`, `/memory`, `/search`, `/soul`, `/intelligence/reload`.

### Key Design Decisions

- Session isolation is per-agent+channel+peer, with per-session mutex locks to serialize turns on the same session.
- The dispatch step is a separate LLM call that returns JSON `{mode, agent_id, input}`. Only masters can dispatch; only to declared children.
- Tool calls loop up to 5 times per turn. Each tool result is appended as a `tool` role message before the next LLM call.
- The resilience client wraps the base LLM client with: profile rotation on failure, context overflow compaction (truncate tool results then compact history), and fallback model chain.
- Delivery uses a file-based outbox (`workspace/delivery-queue/`) with per-item retry counters and a background scanner.
