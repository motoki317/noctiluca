# Design decisions

noctiluca replaces [OpenClaw](https://github.com/openclaw/openclaw), a general agent gateway that previously ran this Slack assistant. Several decisions below answer problems seen there. [architecture.md](architecture.md) shows how the parts fit together.

## Scope

- One agent per instance. Several agents run as several instances that talk on the messaging platform.
- One agent, several chat accounts, each a named `messaging` entry whose name prefixes its refs and tool names. The core needs a prefix anyway to route a ref to its adapter. A name instead of a platform type also lets two workspaces of one platform run side by side.
- Only what a current workload needs (see Workloads). noctiluca replaces OpenClaw mainly to drop features and configuration that got in the way.

## Runtime

- Durable inbox plus one reconciler. With every unit of work as a SQLite row and one loop that dispatches rows, a failure stays with its row instead of stopping the process.
- Done once delivered. Platform posts are not idempotent, so a row handed to the model is never replayed. A crash between `Send` and the commit that marks the row done can still repeat one delivery.
- No waiting inside a turn. A turn that waits holds its session. Long work runs in Claude's background tools or `watch`. Timed follow-ups use `wake_at`.
- Tool access lasts as long as the session, not the turn. OpenClaw tied tool access to a turn, so a background subagent lost its tools when the turn ended.

## Sessions and replies

- One session per channel. Threads in a channel share context. The cost is that one channel's turns run in order. Background tools keep turns short.
- Explicit post refs. With several threads in one session, an implicit "current conversation" picks the wrong thread, so every post names a ref from an input.
- Only tools post. OpenClaw posted the final text unless it was a magic `NO_REPLY` string, which caused stray posts.

## Model adapter

- The official `claude` CLI on the owner's subscription, one process per session, over stream-json. Claude Code already provides shell, subagents, and context compaction.
- Codex through `codex app-server`, one process per session. `turn/steer` adds input to the running turn. Codex keeps a thread's first developer instructions on resume, so a changed system prompt is added to the resumed thread as a developer message. Earlier instructions stay in the thread's history, including ones since deleted from memory.
- No per-session roles. The model judges what to change. Details: architecture.md, "Trust model".

## Memory

- Markdown in a Git repository. Claude reads and edits files natively, and Git gives every change a diff and a revert. Mem0, Graphiti, cognee, and Letta each add a service for the same job.
- The model edits and commits memory with its own tools, so the harness has no memory API.
- A small core loaded into the system prompt when each model process opens, the rest on demand. Changes to loaded files take effect on the next process, including on resume. Forgotten instructions in a large prompt were the most common OpenClaw failure.
- The model searches files with its native shell tools. An index is not needed at the current size.
- Memory has three tiers. Short-term memory holds the exact state of open work and loses each entry when the work ends. Long-term memory holds rules generalized from repeated experience: `core.md`, loaded every time, and leaf files it points to. External memory holds specific facts and procedures by theme: in a knowledge system the owner connected when they may be shared, so that people and other agents can use them, and in local files otherwise. Keeping specific facts out of the loaded core, and every file under 8 KB, bounds what a turn loads as memory grows.
- Consolidation restructures rather than appends. It gives each lesson and each open item one home in its tier, comparing it with what that home holds and choosing one of four edits (add, update, delete, or nothing), as Mem0 (arXiv 2504.19413) does. Changes to `owner/`, tools, and the harness go to the owner as proposals. OpenClaw's nightly memory job promoted only memories that were searched often, so forgotten knowledge never qualified. Small item-level edits follow ACE (arXiv 2510.04618), which found that whole-document rewrites shrink memory and lose accuracy.
- Measured improvement on request only. The shipped `procedures/hillclimb.md` guides a train/held-out experiment with the model's own tools, following [Automating eval design and hillclimbing with Claude](https://claude.dev/blog/automating-eval-design-and-hillclimbing/). The harness has no evaluation code.

## Workloads

| Workload | What it needs |
|---|---|
| DMs and team channels open to humans and bots, with or without mentions | any sender by default, optional `trust` allowlists, conversation overrides, group refresh |
| Alert channels where bot posts start an investigation, a reaction marks it done | top-level bot posts, replies in joined threads, read tools that show reactions on alert posts |
| Hourly heartbeat over a todo list | heartbeat schedule, `procedures/heartbeat.md` |
| Daily photo: morning DM preview, evening thread post sent to the channel too | cron with timezone, upload and broadcast tools |
| Long PR reviews and delegated coding runs | native background tools, `watch` |
| Issue tracker, wiki, error tracker, read-only metrics CLI | external MCP servers with a shared OAuth login, native shell |
| Reading any thread, searching the workspace, downloading files | Slack read tools on an optional user token |
| Fixing memory from mistakes and keeping it short | consolidation |
