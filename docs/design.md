# Design

This is the first design draft. It builds on the requirements in [requirements.md](requirements.md) and the lessons in [lessons-from-openclaw.md](lessons-from-openclaw.md). There is no code yet.

## Principles

- noctiluca carries no unneeded features. This is the main reason to build a harness instead of using OpenClaw. Before you add a feature, check that a row of the table "Can the current workloads run on this design" below needs it.
- One instance runs one agent (one soul). For several agents, the user starts several instances, and the agents talk to each other on the messaging platform.
- An agent never waits with `sleep` or polling. When it must wait, it hands control back to the core (`watch`, `wake_at`) and ends its turn.

## Overall structure

```
 Slack ─┐                                            ┌─ claude CLI (subscription)
Discord ┼─ messaging adapter ─ core ─ model adapter ─┤
  …    ─┘ (also provides tools) │                    └─ Codex, others
                                ├─ tool hub (MCP)
                                ├─ sessions and history (SQLite)
                                ├─ memory repository (git)
                                └─ scheduler and background queue
```

The core knows no platform name and no model name. It talks to the adapters on both sides only through the types in the next section.

## Messaging adapter

### Types that the core handles

- `ConversationRef`: an opaque string that identifies where a reply goes. The Slack adapter builds it as, for example, `slack:<channel>:<thread_ts>`, but the core does not interpret it.
- `InboundEvent`: an event that the adapter passes to the core. There are three kinds: `message`, `message_edited`, and `reaction`.
  - Common fields: `conversation` (`ConversationRef`), `parent` (the `ConversationRef` of the parent conversation for a thread, otherwise empty), `sender` (`{ id, displayName, kind: "human" | "bot" }`), `text`, `attachments` (file references), `mentionsMe`, `receivedAt`
- To send, the core makes one call to the adapter: `send(conversation, { text, attachments })`. It returns only the message ID.

### Tools that the adapter provides

The adapter registers platform-specific operations with the core as tool definitions (name, description, JSON Schema, implementation). The core shows them to the model through the tool hub and does not interpret their meaning. The Slack adapter provides these tools first.

| Tool | Purpose |
|---|---|
| `slack_read_thread` / `slack_read_history` | Read any channel or thread. The config can make them use a user token. |
| `slack_search` | `search.messages` |
| `slack_download_file` / `slack_upload_file` | Download and post files |
| `slack_react` | Reactions |
| `slack_send` | Post to another conversation, including "also send to channel" (broadcast) |

With a tool, the adapter can register a short note on its use (for example, which custom emoji to use). The core loads these notes next to the memory core.

## Core

### From an incoming event to a turn

1. **Acceptance check**: config rules decide whether the core accepts an `InboundEvent`. The rules live in one place. For each conversation, they state whose messages start a turn: a list of user IDs, a user group, whether bots count, and whether a mention is required. The history keeps rejected events too, as context.
2. **Session assignment**: one function, `routeSession(event) → SessionKey`, maps an event to a session key. By default, each thread gets its own session, the top level of a channel shares one session, and each DM partner gets one session. A new thread session starts by reading the recent history of its parent channel.
3. **Turn**: if no turn runs in the session, the model adapter starts a new turn. If a turn runs, the message goes into the session's inbox (next section).

### Replies

The model replies with the tool `reply`, which sends to the current conversation. The final text of a turn goes nowhere. A turn that sends nothing posts nothing. The history of the destination conversation also records what the agent posted there. For example, the DM session can later read a preview that a morning scheduled run sent to the DM.

### Permissions

The core compares `InboundEvent.sender` with the `owner` in the config, and tags the turn with `actor: owner | member`. The tool hub uses this value to gate tools that only the owner can use, such as registering a schedule or editing the memory core.

## Model adapter

### Interface

```ts
interface ModelAdapter {
  // Open a session. With a resumeToken, continue the previous conversation.
  open(session: { key: string; resumeToken?: string; systemPrompt: string; toolHub: McpEndpoint; workdir: string }): ModelSession
}

interface ModelSession {
  // Pass a user message. If no turn runs, start a new turn.
  // If a turn runs, pass the message to that turn (steer).
  send(input: UserInput): void
  events: AsyncIterable<ModelEvent>  // turn_started, text, tool_call, turn_finished, error
  resumeToken(): string | undefined
  close(): Promise<void>
}
```

To the core, an extra message for a running turn (R2) is one `send` call. If a model cannot take input in the middle of a turn, the adapter queues the message itself and passes it as the next turn right after `turn_finished`. The core does not know which method an adapter uses.

### Claude adapter

- It starts the official `claude` CLI with the subscription login as is. OpenClaw 2026.10.10 also starts it with `claude --print --input-format stream-json --output-format stream-json --verbose --replay-user-messages …` (`dist/cli-runtime-args-*.mjs`).
- It keeps one process per session and writes stream-json user messages to stdin. The CLI picks up a message written during a run at its next break point.
- It passes the tool hub with `--mcp-config`. It uses the user's `~/.claude` (skills, subagents, rules) as is.
- Claude Code itself handles shell execution, subagents, and context compaction.

`--bare` will become the default for `-p` in the future, and bare mode does not use the subscription login or `~/.claude`. The adapter either passes explicit flags that keep it out of bare mode, or pins the CLI version ([go-stack.md](go-stack.md)).

### Codex adapter (future)

It uses `codex app-server` (JSON-RPC 2.0). `turn/steer` adds input to a running turn, so `send` maps directly to steer.

## Tool hub

The core runs one MCP server and passes only that server to the model. The server combines these three sources:

- Core tools: `reply`, memory search, read, and write, todo, schedules, search over past sessions, and `watch` and `wake_at` (below)
- Tools that the messaging adapter registered
- External MCP servers (an issue tracker, a wiki, an error tracker). The core connects to them as a client and stores their OAuth tokens. With this, a different model adapter uses the same login.

Tool results are short (success or failure, ID, body). The processes of external MCP servers live as long as the core, and the core does not create them per session.

### `watch`: wait for a long job to finish

For a job that takes more than 10 minutes, such as a Codex run or starting a local k3d cluster, the core runs `watch({ command, cwd })` as a child process. When the process exits, the core sends a summary of the result to the original session as a message and starts a new turn (or steers the running turn). The model side's shell time limit does not apply. `wake_at({ at, note })` sends `note` to the original session at the given time. It covers waits with no process that signals the end, such as waiting for a PR review.

## Memory

### Location

Memory lives in the git-managed directory `memory/`. Every change is a commit and can be undone.

| Path | Loaded | Who can edit |
|---|---|---|
| `memory/owner/` (the persona, the user's preferences) | Every turn | The owner only |
| `memory/core.md` (the tool index, important rules) | Every turn | The agent. Limit: 8 KB |
| `memory/topics/*.md`, `memory/procedures/*.md` | When needed, from index links and search | The agent |
| `evals/` | Only during memory consolidation | The agent |

The tool index in `core.md` lists one line per entry, in the form "what to do → the tool to use" (for example, "read any Slack conversation → `slack_read_thread`"). The agent forgot the user token because no such line loaded every turn.

While there are a few dozen documents, the agent greps the memory repository, and noctiluca builds no search index. When grep is not enough, noctiluca adds an FTS5 trigram index and embeddings in SQLite ("Search" in [memory-research.md](memory-research.md)). Each item records the date it was last useful, and consolidate uses that date to pick candidates to forget.

### History

SQLite stores the incoming events, turns, and tool calls of all sessions. A tool searches past sessions, and memory consolidation reads this history as its input (episodes).

## Background tasks

### Queue and lock

Scheduled runs, cron tasks, and memory consolidation all go into one queue, and one worker runs them in order (R6). Conversation turns do not go into the queue and run in parallel. If the previous task still runs when a task's start time comes, the new task runs after it.

A task never waits inside itself. A task that waits for CI or a long build registers `watch` or `wake_at(time, note)` with the core, ends its turn, and releases the lock. When the wait ends, the core queues the continuation as a new task. As a result, no task holds the worker for long, and a task with a fixed time, such as the evening photo post, does not fall far behind.

### Scheduled run (heartbeat)

Once an hour, the agent runs the procedure in `memory/procedures/heartbeat.md` in one turn. The procedure lists items that the user added, such as "check the todo list" and "photo post". A cron task is `{ name, cron, tz, prompt, workdir }`, and the config or an owner tool registers it.

### Memory consolidation (consolidate)

The name "sleep" is easy to confuse with the shell `sleep`, so this task is called consolidate. It goes into the queue once a day (for example, at 4 a.m.) and when the number of new episodes reaches a set count.

By default, consolidate works on the agent's own memory. Its proposals have no limit on scope, though. If it judges that the harness code, its config, or the user's operations need a change, it sends that proposal to the owner.

The steps follow. The approach follows [Automating eval design and hillclimbing with Claude](https://claude.dev/blog/automating-eval-design-and-hillclimbing/).

1. **Collect**: read the episodes since the last run. Pick up failure signals mechanically: user corrections ("I didn't say stop"), negative reactions, tool errors, repeats of the same operation, and facts that the owner taught later.
2. **Classify causes**: put the cause of each failure into one class: the knowledge was missing, the knowledge existed but did not load, the instructions contradicted each other, the tool could not do it, or a harness bug. Memory cannot fix the last two, so they become proposals to the owner.
3. **Fix without measuring**: commit additions of facts that the owner taught and corrections of clear errors as they are. Measure only changes to behavior rules or to the structure of the memory.
4. **Pick cases**: measure each change to behavior on cases of the current run. Each run picks its cases again from recent episodes, both failures and successes. A set of only failures measures only the current model's weak points and drifts away from the real distribution of work. When the cases are picked, they split at random into a training set and a test set, and the consolidating agent never sees the test set. A pass condition is a checkable claim ("calls `slack_read_thread`", "does not say it cannot read").
5. **Fix**: make one change at a time, as a patch that fixes the cause. A change adds, merges, generalizes, or deletes one item, and never rewrites a whole document. ACE reports that a full rewrite shrinks the content and lowers accuracy. Do not tune wording. Do not paste the text of a failed conversation into memory. Write a generalized rule or a tool index line instead.
6. **Measure**: first, run the unchanged state twice to find how much results vary by chance (the noise). Run both the training set and the test set before and after the change. Adopt the change only if the test set improves by more than the noise. Reject a change that raises the average but fails a case that passed before. If only the training set improves, treat it as overfitting and revert. Also revert on a regression. During the runs, tools with side effects, such as sends and file writes, are replaced with stubs that only record the call. A different model grades the runs (for example, Codex grades Claude's cases).
7. **When stuck**: after two or three runs in a row with no adopted change, stop making changes, and only classify the remaining failures by cause. If a case fails every time, suspect an error in the case or in the grading.
8. **Shrink**: if memory exceeds the limit of `core.md`, the limit per document, or the limit on the total size of all documents, merge, generalize, or delete items to get back within the limits. The goal of this step is a smaller memory with the same score, and the test set must show no regression. At equal scores, the smaller memory wins, so forgetting and generalizing count as improvements, not as risks.
9. **Keep**: commit the adopted changes, and send a summary to the owner.

Cases do not pile up into a permanent regression suite. The case store has a cap on the number of cases and an expiry. Cases that pass every time and cases past their expiry are discarded. This keeps the memory from hardening around old failures.

## Can the current workloads run on this design

This table maps the work that OpenClaw handles today to the parts of the new design that cover it.

| Current work | What it needs | How the new design covers it |
|---|---|---|
| A DM with the owner | DM events, the owner check | Slack adapter, acceptance check, permissions |
| A team channel where any member of a user group can start a turn without a mention | Permission by user group, replies in threads | Acceptance check (group), `routeSession`, `reply` |
| A channel where the agent replies only when called | Read as context, and the model decides whether to reply | An accepted event posts nothing unless the model calls `reply` |
| Alert channels where bot posts start an investigation | Turns started by bot messages, one session per channel | Acceptance check (bots allowed), `routeSession` |
| A reaction marks an alert done | Reaction events | The `reaction` kind of `InboundEvent` |
| An hourly heartbeat over a todo list (check `memory/todo.md`, report `[urgent]` items) | Scheduled runs, todo, posts to a report channel | heartbeat, todo, `slack_send` |
| A daily photo post: a morning DM preview, and an evening post to a channel thread with "also send to channel" | cron with a time zone, image posts, a DM reply that replaces the preview | cron, `slack_upload_file`, `slack_send` (broadcast), history records of sends |
| Long PR-review runs on a local cluster (k3d starts in about 5 minutes) with browser automation | Long shell runs, browser automation | Claude Code's Bash, `watch` |
| Delegating implementation to Codex for more than 10 minutes | Waiting for a long job to finish | `watch` |
| External MCP servers (an issue tracker, a wiki, an error tracker) with a shared OAuth login, plus a read-only metrics/logs CLI | MCP (shared OAuth login), a CLI | External MCP in the tool hub, Claude Code's Bash |
| Reading files and images from chat | Downloads with a user token | `slack_download_file` |
| Searching past sessions (for example, a channel session looks up what a DM said) | History and search over all sessions | History in SQLite and a search tool |
| Dreaming (daily memory promotion at 3 a.m.) | Memory consolidation | consolidate |

## Decided

- The name is noctiluca.
- The implementation language is Go. The layout follows traPtitech/NeoShowcase. [go-stack.md](go-stack.md) lists the libraries and the layout.
- One instance runs one agent. The first migration moves only `main`.
- As today, the Claude subscription also serves turns that team members start.

## Open questions

1. With stream-json input to the `claude` CLI, what line format does a message written during a run take, and at which break point does it reach the turn? The docs do not say. The plan is to read the implementation in OpenClaw #159816 and test it by running the CLI.
2. If the core holds the OAuth tokens for external MCP servers, do the issue tracker, wiki, and error tracker servers in use actually accept dynamic client registration? The plan is to log in to each one and check.
