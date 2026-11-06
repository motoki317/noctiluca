# Design decisions

noctiluca replaces [OpenClaw](https://github.com/openclaw/openclaw), a general agent gateway that previously ran this Slack assistant. Several decisions below answer problems seen there. [architecture.md](architecture.md) shows how the parts fit together.

## Scope

- One agent per instance. Several agents run as several instances that talk on the messaging platform.
- Only what a current workload needs (see Workloads). noctiluca replaces OpenClaw mainly to drop features and configuration that got in the way.

## Runtime

- Durable inbox plus one reconciler. With every unit of work as a SQLite row and one loop that dispatches rows, a failure stays with its row instead of stopping the process.
- Done once delivered. Platform posts are not idempotent, so a row handed to the model is never replayed. A crash between `Send` and the commit that marks the row done can still repeat one delivery.
- No waiting inside a turn. A turn that waits holds its session. Long work runs in Claude's background tools or `watch`. Timed follow-ups use `wake_at`.
- Tool access lasts as long as the session, not the turn. OpenClaw tied tool access to a turn, so a background subagent lost its tools when the turn ended.

## Sessions and replies

- One session per channel. Threads in a channel share context. The cost is that one channel's turns run in order. Background tools keep turns short.
- Explicit reply targets. With several threads in one session, an implicit "current conversation" picks the wrong thread.
- Only tools post. OpenClaw posted the final text unless it was a magic `NO_REPLY` string, which caused stray posts.

## Model adapter

- The official `claude` CLI on the owner's subscription, one process per session, over stream-json. Claude Code already provides shell, subagents, and context compaction.
- Planned: Codex through `codex app-server`, whose `turn/steer` fits `ModelSession.Send`.

Accepted sessions get full native tools under the same Unix user. Owner-only hub gates prevent mistakes, not a determined member. Real isolation needs a separate user or sandbox and is out of scope.

## Memory

- Markdown in a Git repository. Claude reads and edits files natively, and Git gives every change a diff and a revert. Mem0, Graphiti, cognee, and Letta each add a service for the same job.
- A small core loaded every turn, the rest on demand. Forgotten instructions in a large prompt were the most common OpenClaw failure.
- Search is `git grep`. An index is not needed at the current size.
- Consolidation learns and tidies. It commits facts the owner taught, clear corrections, and lessons that several episodes share, and it keeps its own memory (everything outside `owner/`) short: it deletes finished items and logs, merges and generalizes notes, and moves facts where they belong. Changes to `owner/`, tools, and the harness go to the owner as proposals. OpenClaw's nightly memory job promoted only memories that were searched often, so forgotten knowledge never qualified. Small item-level edits follow ACE (arXiv 2510.04618), which found that whole-document rewrites shrink memory and lose accuracy.
- Measured improvement on request only. The shipped `procedures/hillclimb.md` guides a train/held-out experiment with the model's own tools, following [Automating eval design and hillclimbing with Claude](https://claude.dev/blog/automating-eval-design-and-hillclimbing/). The harness has no evaluation code.

## Workloads

| Workload | What it needs |
|---|---|
| Owner DMs, team channels open to a user group, with or without mentions | `conversations.rules`, group refresh |
| Alert channels where bot posts start an investigation, a reaction marks it done | bot senders, reaction events |
| Hourly heartbeat over a todo list | heartbeat schedule, `procedures/heartbeat.md` |
| Daily photo: morning DM preview, evening thread post sent to the channel too | cron with timezone, upload and broadcast tools |
| Long PR reviews and delegated coding runs | native background tools, `watch` |
| Issue tracker, wiki, error tracker, read-only metrics CLI | external MCP servers with a shared OAuth login, native shell |
| Reading any thread, searching the workspace, downloading files | Slack read tools on an optional user token |
| Fixing memory from mistakes and keeping it short | consolidation |
