# Go libraries and layout

This document lists what a survey on 2026-11-04 found. Go has everything that noctiluca needs. Only the part that starts the model has no library, and noctiluca implements that part itself.

## Libraries

| Purpose | Choice | Reason |
|---|---|---|
| Slack | [slack-go/slack](https://github.com/slack-go/slack) (v0.34.0, 2026-11-03) | It supports Socket Mode, v2 file uploads, and `search.messages`. To read with a user token, create a second client with `slack.New(userToken)`. |
| MCP | [modelcontextprotocol/go-sdk](https://github.com/modelcontextprotocol/go-sdk) (v1.8.0) | The official SDK. It has both a server (stdio, streamable HTTP) and a client. It also has OAuth dynamic client registration and token refresh, built on `golang.org/x/oauth2`. |
| SQLite | [ncruces/go-sqlite3](https://github.com/ncruces/go-sqlite3) (v0.36.0) | It needs no cgo. It includes FTS5, and vector search can come later. |
| Scheduling | [robfig/cron](https://github.com/robfig/cron) v3 | Parses cron expressions and timezones. SQLite stores next-run times, and the reconciler controls dispatch. |

## What noctiluca implements itself

- Starting the `claude` CLI and reading and writing stream-json. No official Go Agent SDK exists. The message types are in the [TypeScript type definitions](https://code.claude.com/docs/en/agent-sdk/typescript.md) and the [headless documentation](https://code.claude.com/docs/en/headless).
- A future adapter can use the Codex [app-server](https://learn.chatgpt.com/docs/app-server) (JSON-RPC 2.0, one object per line). `turn/steer` adds input to a running turn, so the Codex adapter also needs no queue. `codex app-server generate-json-schema` generates the types.
- The tool hub. This is the part that re-exports the tools of external MCP servers from noctiluca's own MCP server.
- The OAuth login (a callback endpoint, and tokens stored in SQLite).
- Conversion between Slack mrkdwn and the model's text.

## Caution

The Claude adapter retains explicit settings sources and never passes `--bare`. It accepts CLI versions 2.1.316 and newer and warns on newer major/minor versions. [claude-cli.md](claude-cli.md) records the measurements.

## Layout

The layout follows traPtitech/NeoShowcase (HEAD `06fa2e6`).

- There are three layers: `pkg/domain` (all types and interfaces live here), `pkg/usecase/<service>`, and `pkg/infrastructure/<technology>`.
- Implementations are unexported structs, and each constructor returns a domain interface. For example, `pkg/infrastructure/repository/user.go` has `NewUserRepository(db) domain.UserRepository`.
- Dependency injection uses a hand-written `cmd/noctiluca/di.go`. The application owns startup and shutdown. Services expose `Start` and `Shutdown`. The core runs them under an `errgroup`.
- viper reads the config. Environment variables with a prefix override the file (`NS_` in NeoShowcase, `NOCTILUCA_` in noctiluca).
- The schema is declared in `migrations/schema.sql`, embedded in the binary, and applied idempotently at startup.
- Mocks use moq through `go tool`. Tests use testify and `go test -shuffle=on`. `mise.toml` holds the tasks and tool versions. `.golangci.yml` holds the lint config (govet, ineffassign, staticcheck, unused, sloglint). Logging uses `log/slog`.

noctiluca does not take these from NeoShowcase: buf (noctiluca does not use protobuf), a subcommand per component (noctiluca is one binary), testcontainers (`t.TempDir()` is enough for SQLite), and sqlboiler (the repositories use plain SQL).

```
cmd/noctiluca/
  main.go            # cobra: run, mcp-login <server>
  config.go          # collects each package's Config. Prefix NOCTILUCA_
  di.go             # application assembly and shutdown
pkg/domain/
  event.go           # ConversationRef, InboundEvent, Sender, Attachment
  messaging.go       # MessagingAdapter
  model.go           # ModelAdapter, ModelSession, ModelEvent
  tool.go            # Tool, Actor
  repository.go      # repository interfaces
  memory.go          # MemoryStore
pkg/usecase/
  core/              # acceptance rules, routing, inbox ingress, reply
  toolhub/           # one MCP server for core, adapter, and external tools
  reconcile/         # durable dispatch, retries, deadlines, and schedules
  consolidate/       # episodes and default procedures
  watch/             # watch and wake_at
pkg/infrastructure/
  slack/             # Socket Mode, bot and user clients, tools
  claude/            # stream-json driver
  mcpclient/         # external MCP clients and OAuth
  repository/        # SQLite
  memory/            # git memory repository
pkg/test/mocks/gen.go
migrations/schema.sql
mise.toml  .golangci.yml  go.mod
```

The Codex adapter adds `pkg/infrastructure/codex/` when it is built. `pkg/util/` appears when a second place uses the same code.
