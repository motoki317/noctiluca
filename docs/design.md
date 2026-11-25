# Design

## How a message becomes a reply

1. A messaging adapter receives a chat event from Slack, Discord, or traQ.
2. The core decides whether the sender can start a turn ([Trust](#trust)) and stores the event as a row in the SQLite inbox. The platform gets its acknowledgement only after the row is stored.
3. The reconciler sends the rows, in arrival order, to the one model session. The model adapter runs that session as a `claude` or `codex` CLI process.
4. The model works on the input: this is a turn. It uses the CLI's own tools and, over MCP, the tools that noctiluca provides.
5. The model replies by calling the send tool of a messaging adapter.

Scheduled tasks enter the same inbox as rows. So do the notes that the model sets for a later time with the `wake_at` tool, and the results of the background commands that it starts with the `watch` tool. So do the [events](events.md) that local programs post to noctiluca over HTTP.

## Decisions

- **One agent per instance.** To run several agents, start several instances, and they talk to each other in chat.
- **One session for everything.** The agent answers each message with every conversation and task in view.
- **A turn never waits.** One turn runs at a time, so a turn that waits holds every conversation. Long work runs in the background, as a subagent of the CLI or as a command under `watch`, and its result arrives as a new input.
- **Only tools post.** noctiluca never posts the model's final text. The model posts by calling a send tool, so a turn with nothing to say posts nothing.
- **Subagents draft, and only the main session posts.** Subagents have no send tools. The main session sees every conversation, so it decides which content can go to which conversation.
- **noctiluca never sends an input again once the model started on it.** A second run could post the same chat message twice. A row that fails before the model starts on it is tried up to three times, and then noctiluca reports it to you. A turn that fails after that is reported to you too. Only a crash before noctiluca records the start can repeat an input.
- **The model CLI does the agent's work.** noctiluca drives the official `claude` or `codex` CLI on your subscription. The CLI already provides a shell, subagents, and context compaction.
- **Memory is Markdown in a Git repository.** The model reads, edits, and commits it with the CLI's file tools, so noctiluca needs no memory API. Git gives every change a diff and a revert.
- **Memory has three tiers.** Short-term memory points to open work, such as requests and promised follow-ups. Long-term memory holds general rules, and its index `core.md` loads into every system prompt. External memory holds specific facts by theme, and the model reads it on demand: from other files, or from a knowledge system that you connected. The part that loads into every prompt stays small as memory grows.
- **The agent rests.** The optional nightly `rest` procedure saves what only the session knows, compresses your teachings and corrections into general rules, and restructures memory. After it, the session starts over with an empty context.
- **The model chooses when to compact.** The CLIs compact by themselves at a fixed context size, which usually comes in the middle of a task. So noctiluca shows the model its context size with each input once the size reaches 200k tokens, and the model calls the `compact` tool at a break in its work.

## Trust

- In a DM, every message starts a turn. In a channel that the bot is in, these start a turn: a top-level message, a message that mentions the bot, a post in a thread that the bot takes part in, and a reaction to one of its posts.
- Anyone who can reach the bot can send these, unless an optional `trust` allowlist in the config names the senders.
- Every turn has every tool, including a shell that runs as the Unix user of noctiluca. noctiluca asks for no approval of a tool call.
- An external MCP server on MCP 2026-07-28 or newer can ask you to approve one of its tool calls. The model then asks you in chat. It can answer instead of you, so this approval does not stop the model from making the call.
- Nothing isolates the model from noctiluca's config, tokens, and database. Isolation is out of scope for noctiluca.
