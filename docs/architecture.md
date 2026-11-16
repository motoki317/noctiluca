# Architecture

One noctiluca process runs one agent. The core knows no platform or model names: each messaging adapter translates one chat account (Slack, Discord, or traQ), and a model adapter translates a model CLI (the Claude CLI or the Codex CLI).

```mermaid
flowchart LR
  Chat[chat accounts] <--> MA[messaging adapters<br/>one per messaging entry]
  MA <-->|InboundEvent, Send| Core[core<br/>pkg/usecase/core]
  Core -->|inbox row| DB[(SQLite)]
  Sched[schedules, wake_at, watch] -->|inbox row| DB
  DB --> Rec[reconciler<br/>pkg/usecase/reconcile]
  Rec -->|Open / Send| Model[model adapter]
  Model <--> CLI[model CLI]
  CLI -->|MCP, session token| Hub[tool hub<br/>pkg/usecase/toolhub]
  Hub -->|watch, wake_at, schedules| Core
  Hub -->|adapter tools| MA
  Hub --> Ext[external MCP servers]
  CLI -->|file tools and git| Mem[(memory Git repo)]
  MA -.- E1[e.g. Slack<br/>pkg/infrastructure/slack]:::example
  Model -.- E2[e.g. Claude Code<br/>pkg/infrastructure/claude]:::example
  classDef example font-size:11px,fill:none,stroke-dasharray:3 3
```

## A message, end to end

The diagram follows a Slack event that passes the filter described in [Conversation events](running.md#conversation-events). The adapter acknowledges dropped events without calling the core or writing to the database.

```mermaid
sequenceDiagram
  participant S as Slack
  participant A as messaging adapter
  participant C as core
  participant D as SQLite
  participant R as reconciler
  participant M as model CLI
  participant H as tool hub
  S->>A: message event
  A->>C: InboundEvent
  C->>D: event + inbox row (one transaction)
  C-->>A: committed
  A-->>S: ack (only after the commit)
  R->>D: claim ready row
  R->>M: Send (starts a turn, or joins or queues behind the running one)
  M-->>R: input_started (for this row's input ID)
  R->>D: row done
  M->>H: slack_send(channel, thread_ts, text)
  H->>A: send tool
  A->>S: chat.postMessage
  M-->>R: turn_finished
```

Only tools post. The model's final text is never sent, so a silent turn posts nothing.

A row counts as delivered when the model reports that it started on the row's input, not when `Send` returns. A conversation sends each input as it arrives, without waiting for earlier inputs to start.

## Sessions

- A channel is one session: its top-level posts and all its threads. A DM is one session per peer. Each scheduled task (heartbeat, cron job, rest) runs in its own session.
- Chat inputs name the place, sender, and local time, with ordering context. Each adapter formats a location line with its native IDs. For example, Slack supplies `channel`, `thread_ts`, and `timestamp`. Its send tool takes `channel` and an optional `thread_ts`. Watch results and wake notes arrive as plain inputs without posting targets.
- A session's turns run one at a time, so the shipped tool notes tell the model to move long work out of the turn: into Claude's own `run_in_background` Bash or Agent, or into the `watch` tool, which runs a command and sends its exit code and output to the session as a new input. Codex's background commands never wake the model, so the Codex adapter tells the model to use `watch`. `wake_at` sends a note to the session at a given time. The turn then ends in seconds.
- Model processes are disposable. An idle one closes after `model.idle_timeout`. The next input resumes it from the saved resume token.
- The nightly `rest` schedule sends `procedures/rest.md` as a normal input. The model can also follow it during the day when it judges that its session needs a fresh start.
- The core `rest` tool marks only its calling session to close at idle. It immediately clears the saved resume token. The process closes once it has no turn or unstarted input and the model CLI reports no background work of its own, such as `run_in_background` Bash or Agent.
- Inputs that arrive before that idle point still run in the old process. The wait for idle has no deadline. A restart after the call starts fresh even if the old process never reached idle.
- Only the tool closes a session for rest, not the end of the procedure. Other sessions keep their context until their own model calls it.

## Work and failure

Every unit of work is an inbox row: a chat input, a schedule firing, a `wake_at` note, or a `watch` outcome.

- A row that fails to send, or that the model refuses, is retried with backoff. A row whose model process closes before it starts, for example when noctiluca restarts, is sent again without counting an attempt. If noctiluca crashes before it records a start, that row is sent again.
- A schedule fire whose turn fails, heartbeat and rest included, runs again in the same task session 15 minutes later, and 30 minutes after a second failure. A failed turn on a chat, `watch`, or `wake_at` row does not run again.
- Delivery failures and turn failures count against one budget of three attempts per row, so an earlier delivery failure moves a fire's first turn retry to the 30-minute delay. After the third failed attempt the row is dead, and the owner gets one DM with the cause.
- Conversation turns are cut at `model.turn_timeout` (2 h), task turns at `tasks.timeout` (30 min).
- A turn cut short by a restart, crash, or model exit continues once with a note. In a conversation, the note asks the model to finish each unfinished input using its posting instructions. A turn cut at its timeout does not continue. Tasks notify the owner.
- Conversation sessions run concurrently. Scheduled tasks run one at a time.
- A failed row never stops the process. Only a failure of SQLite or of the tool-hub listener does.

## Interfaces

Core ↔ messaging adapter (`pkg/domain/messaging.go`):

```go
type MessagingAdapter interface {
	Start(ctx context.Context, consume func(context.Context, InboundEvent) error) error
	Send(ctx context.Context, to ConversationRef, msg OutboundMessage) (messageID string, err error)
	Tools() []Tool     // platform operations, offered to the model through the tool hub
	ToolNotes(context.Context) string // usage notes and the owner DM channel, added to the prompt
	Shutdown(context.Context) error
}
```

Each adapter is a named entry under `messaging` in the config. The core reads only the first segment of a `ConversationRef`, which is the entry's name, to select the adapter for input lookups and accept rules. The rest of the ref is opaque, and only that adapter interprets it. An adapter's tools carry the same name as a prefix (`slack_send` for an entry named `slack`).

`InboundEvent` carries the conversation and its parent, the sender (human or bot), text, attachments, and whether the agent is mentioned. An adapter can also implement `SourceInfoProvider` (readable names and a location line with native IDs for input headers), `UserGroupProvider` (group membership for accept rules), `DirectMessageProvider` (the owner's DM, for notices), and `ConversationFilter` (skips remote lookups in ignored conversations).

Core ↔ model adapter (`pkg/domain/model.go`):

```go
type ModelAdapter interface {
	Open(ctx context.Context, cfg ModelSessionConfig) (ModelSession, error) // session key, resume token, prompt, tool-hub URL + token, workdir
}

type ModelSession interface {
	Send(ctx context.Context, in UserInput) error // also during a turn: the adapter adds it to the running turn or queues it
	Events() <-chan ModelEvent                    // input_started, input_refused, turn_started, text, tool_call, tool_result, background_done, turn_finished, error
	ResumeToken() string
	Idle() bool
	Close() error
}
```

The model never receives tool definitions directly. It gets the tool hub's URL and a bearer token for its session. The token stays valid until the session closes, so background subagents keep their tools after a turn ends.

## State

| Where | What |
|---|---|
| SQLite | events and sends (with full-text search), inbox, sessions and resume tokens, schedules, watches, OAuth tokens |
| memory Git repo | `owner/` (the owner's handwritten files) and the agent's `core.md`, both loaded into the system prompt when a model process opens. Any other path is read on demand. |

## Trust model

Any human or bot that reaches the agent starts turns: in DMs, in every channel the bot is in, and on traQ in mentions elsewhere. Anyone who can add the bot to a channel widens that set. A `messaging` entry's optional `trust` narrows it to an allowlist, and the owner is always on it. A conversation override can replace that trust, require a mention, or ignore the conversation. A sender that the adapter reports as neither human nor bot never starts a turn. Configuration: [running.md](running.md#configure).

Every session, scheduled tasks included, gets the same tools: the hub's tools, external MCP tools, and the model CLI's native tools with every permission prompt allowed. Native tools run as the Unix user that runs noctiluca, because reviews and builds need a shell. The model judges what to change.

Only the prompt tells the model to leave `owner/` untouched, and its native tools can still write there.

Owner notices use `DirectMessageProvider` and `Send` on the first `messaging` entry. Model posts use each adapter's send tool. Each adapter's prompt notes name its owner's DM channel. Input headers append ` (owner)` to a sender whose ID matches the `owner` of the entry the message came from, so the model knows who the owner is. The marker grants no permission.

Real isolation needs a separate Unix user for the model, without access to noctiluca's config, tokens, and database. It is out of scope.
