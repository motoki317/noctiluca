# Design

noctiluca runs one agent with replaceable messaging and model adapters, a durable SQLite inbox, and a Git memory repository.
[requirements.md](requirements.md) records the workloads. [lessons-from-openclaw.md](lessons-from-openclaw.md) explains the operating constraints.

## Principles

- Carry only features that a current workload needs.
- Run one agent per instance. Separate instances communicate through the messaging platform.
- Return control when waiting for external work. Register `watch` or `wake_at`, then end the turn.
- Keep platform and provider names out of the core interfaces.

## Overall structure

```text
Messaging adapter ── event + inbox transaction ── SQLite
                                                     │
Schedules ── fire + advance transaction ── inbox ── reconciler
Watch process ── outcome + inbox transaction ──────── │
wake_at ── future inbox row ───────────────────────── │
                                                     ├─ session pumps ── model adapter
                                                     ├─ tool hub (MCP)
                                                     └─ memory repository (Git)
```

SQLite owns pending work. Model processes are disposable caches of session state.
One reconciler decides which rows to dispatch, retry, finish, or mark dead.
Session pumps perform model startup, prompt assembly, and input delivery outside that loop.

## Messaging adapter

`ConversationRef` is an opaque destination string. The core does not interpret its encoding.
An `InboundEvent` carries its source key, conversation, parent conversation, sender, text, attachments, mention flag, and receipt time.
Its kind is `message`, `message_edited`, or `reaction`.

The adapter passes each event to the core through a callback.
The core commits the event and inbox row in one transaction before the callback returns.
The adapter acknowledges delivery only after that return succeeds.
A unique source key makes repeated delivery a no-op. Rejected events remain in history, with a completed inbox row.

The Slack adapter uses Socket Mode and ignores its own messages.
An edit with unchanged text and files does not start a turn, including an unfurl update.
The adapter provides read, search, download, upload, reaction, and send tools, plus short usage notes for the prompt.
An optional user token supplies reads. Posts use the bot token.

## Core and reconciler

### Acceptance and routing

Conversation rules list accepted user IDs and groups, whether bots count, and whether a mention is required.
Threads inherit their parent conversation's rule. Explicit rules override the default DM rule.
Group membership refreshes periodically.

Each conversation or thread has one session key. Its first input includes the parent conversation's last 30 recorded events.
A model session opens with the saved resume token when one exists.

A session has actor `owner` only if its rule permits the owner alone. Other accepted sessions have actor `member`.
The actor stays fixed for that process. A permissions change that removes an owner grant retires the process and revokes its token.
A later process uses the current rule. Background tasks have actor `owner`.

### Durable work

Every input is an `inbox` row: inbound chat, a timed note, a watch outcome, or a schedule firing.
The row holds a unique source key, session key, actor, input and context, readiness time, lane, status, attempts, retry time, deadline, and last error.
Statuses are `pending`, `dispatched`, `done`, and `dead`.

The reconciler wakes on a notification or the next due time. Each pass:

1. Retire sessions past their turn deadline or idle timeout.
2. Fire due schedules and advance their next run in the same transaction. Missed runs coalesce into one firing.
3. Claim ready inbox rows and hand them to session pumps.

A successful `Send` marks its inbox row done. Turn state belongs to the session and lasts until `turn_finished`.
Conversation inputs reach the live process while it is busy. The model CLI queues them.
Independent conversation sessions run concurrently. The task lane permits one busy session across heartbeat, cron, and consolidation.
Conversation turns do not occupy the task worker.
The reconciler never waits for HTTP, process startup, or turn completion.

Delivery failures before a successful `Send` persist an attempt count and exponential backoff, starting at one second.
After three failed attempts, the item becomes dead and a separate durable inbox row requests one owner DM notice.
Notice delivery also retries, without creating recursive notices.
Missing accept rules and rejected senders mark their rows dead immediately, without owner notices.
A rejected sender does not interrupt another sender's running turn.
Model startup, prompt, and send failures remain local to that session.
Unusable shared persistence or the hub listener stops the process.

At startup, pending rows remain pending. Dispatched rows return to pending, including owner notices. Delivery deadlines also return unsent rows to pending.
The subprocess write and SQLite commit cannot form one atomic operation. A crash between them can repeat delivery.
A deadline, mid-turn process exit, or restart of a busy session records an interruption event.
A task turn deadline stops the task and queues one owner notice, without a continuation.
Other task interruptions allow one continuation with a short interruption note. Its count persists with the input and running turn.
An interrupted continuation stops without another continuation. Conversation interruptions create no continuation or owner notice.

### Session lifecycle

A pump owns the model process lifetime. The reconciler records its events and persists its resume token and running turn.
Events from a replaced process are ignored.
Task turn deadlines default to 30 minutes through `tasks.timeout`. Conversation turn deadlines default to two hours through `model.turn_timeout`.
Steering does not extend the current turn's deadline.
An idle process closes after `model.idle_timeout`, default one hour, only when no turn or background task remains.
Retirement revokes the hub token and cancels the process. The session key stays reserved until the pump closes the process and exits.
A later input resumes from the saved token.

### Replies

Only tools post messages. Final assistant text remains private, and a silent turn posts nothing.
`reply` copies the destination under the session lock, then sends synchronously outside the lock and records the send.
Task replies go to the owner's DM. Conversation replies go to that conversation.
There is no reply outbox because platform posts are not idempotent.

## Trust model

Accepted members get the model's full native tools under the same Unix user.
Hub checks restrict edits to `owner/`, `procedures/`, and `core.md`, plus access to owner-only tools.
These checks prevent mistakes, not a determined member.
Real isolation requires a separate Unix user or sandbox and is out of scope.

## Model adapter

`ModelAdapter.Open` accepts a session key, resume token, system prompt, tool-hub endpoint, and working directory.
It returns a `ModelSession` with `Send`, `Events`, `ResumeToken`, `Idle`, and `Close`.
Events include turn start, text, tool calls and results, turn finish, errors, and background completion.

`model.workdir` defaults to `work/` beside the database, created with mode `0700`.
Config validation keeps config, database, token files, and memory outside this directory. Schedules use it unless they specify a working directory.

The Claude adapter runs one subscription-authenticated CLI process per session with stream-json input and output.
It passes the hub through `--mcp-config`, retains user, project, and local settings, and never passes `--bare`.
Native tools remain available to accepted member sessions under the owner's host-access policy.
Claude handles shell execution, subagents, and compaction.

Measured CLI behavior queues mid-turn input for a subsequent turn. Conversation steering forwards each input as soon as its delivery slot is free.
CLI versions below 2.1.316 are refused. A newer major or minor version produces a warning.
[claude-cli.md](claude-cli.md) records the protocol measurements and commands.

## Tool hub

One loopback streamable-HTTP MCP server, named `noctiluca`, combines core tools, messaging adapter tools, and external MCP tools.
A bearer token identifies one model session and its actor. It lasts until that process closes, including between turns and during background subagent work.
Handlers receive the session identity through context. `OwnerOnly` is the hub's tool permission gate.
Memory writes also enforce permissions for their requested path.

Tool results contain the handler's JSON as text, or the external server's result text.
One client per external server shares the owner's OAuth login across sessions.
Servers without login are skipped with a warning. Reconnect refreshes the registry.
External tool-list changes apply to new model sessions only.
External servers require the owner by default. A server can opt members in with `members: true`.

### Waiting tools

`watch({command, cwd})` commits a watch row before spawning a shell process group.
On exit, one transaction stores the outcome and a new input with the exit code and last 4 KB of combined output.
A failed delivery uses the inbox retry policy. It does not stop the service.
Shutdown kills watch process groups. At startup, unfinished watches produce an interruption notice with an unknown exit status.
Commands are never rerun during recovery.

`wake_at({at, note})` inserts an inbox row with a future readiness time.
The originating session receives the note after that time. Task continuations use the task lane.
Neither continuation replaces its note with the scheduled procedure text.

## Memory

Memory lives in a Git repository, initialized when missing. Each tool write commits its message and preserves unrelated local edits.

| Path | Loaded | Tool write/delete permission |
|---|---|---|
| `owner/` | Every turn | Owner |
| `core.md` | Every turn | Owner |
| `topics/` | On demand | Owner or member |
| `procedures/` | On demand, or by a scheduled task | Owner |

The prompt combines owner files, `core.md`, and messaging adapter notes.
`core.md` contains the tool index and rules that must load every turn.
Its write cap defaults to 8 KB. Oversized files remain readable at startup and in prompts so the owner can repair them.
`memory_sizes` reports the revision, per-file sizes, core size, total size, file count, and configured caps.
File, total, and count caps reject growth while permitting reductions toward the limits.

Memory search uses literal Git grep over the managed files, without an index.
Shipped procedures also appear in memory reads and search when no local override exists.
They do not consume the repository's size caps. An empty override suppresses a scheduled procedure.

`memory_revert` reverts one commit without discarding later unrelated edits.
Temporary snapshots live under the repository's `.git/noctiluca-snapshots/` directory.
Startup sweeps that directory and prunes stale worktree registrations. It preserves unrelated owner worktrees.

SQLite separately records inbound events, sends, and model events.
Its event-text search uses FTS5 trigram matching, with `LIKE` for queries shorter than three characters.

## Background tasks

Schedules persist as rows with name, cron expression, timezone, prompt, working directory, actor, and next run.
The config and owner-only schedule tools create them. User cron expressions require exactly five fields, without descriptors.
A cron parser computes the next due time.
Heartbeat and consolidation are ordinary rows with reserved names.
Config-defined jobs return on restart if removed through a tool. Disabled built-in jobs are removed at startup.

Heartbeat reads `procedures/heartbeat.md` hourly by default. A missing or empty file skips its turn.
A task that needs to wait registers `watch` or `wake_at`, ends its turn, and releases the worker.
A hung turn loses its process at the deadline so another task can run.

### Memory consolidation (consolidate)

Consolidation is optional and defaults to 04:00 UTC when enabled. The config controls its cron expression and timezone.
It reads `procedures/consolidate.md`, with a shipped default if no override exists.

The procedure reads recent episodes, commits facts the owner taught and clear factual corrections, and sends behavioral proposals to the owner.
Tool limitations, harness changes, and memory restructuring also become proposals.
Mechanical signals include correction keywords, edits, deletions, negative reactions, tool errors, and repeated identical calls.
They guide inspection and do not prove a failure.

Consolidation excludes its own model events from episodes. After a successful procedure turn, the checkpoint advances to the turn-finish time.
Failed or skipped turns leave it unchanged. `reply` sends the summary to the owner's DM.

The separate `procedures/hillclimb.md` runs only at the owner's request.
It guides an experiment through the model's Bash, Git worktree, and subagent tools.
The harness contains no behavior-experiment runner or case store.
[Memory research](memory-research.md) and [third-party notices](../THIRD_PARTY_NOTICES.md) attribute the procedure's design ideas.

## Workload coverage

| Workload | Support |
|---|---|
| Owner DMs and allowed team channels | Acceptance rules, group refresh, per-conversation sessions |
| Mention-only and bot-alert channels | Mention and sender-kind rules |
| Thread investigations and reactions | Opaque thread destinations, parent history, reaction events |
| Scheduled previews and posts | Persistent cron rows, task replies, adapter send/upload/broadcast tools |
| Long builds, reviews, and delegated commands | Native model tools, durable watches, timed notes |
| External issue, wiki, and error tools | Shared MCP clients and persisted OAuth credentials |
| Files and images in chat | Adapter downloads and attachments |
| Factual memory maintenance | Scheduled consolidation and Git commits |
| Owner-requested behavior improvement | On-demand hillclimbing procedure |
