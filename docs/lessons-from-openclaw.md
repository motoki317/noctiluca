# Lessons from running OpenClaw

For about a month, the owner ran OpenClaw (2026.10.3 to 2026.10.10) on WSL as a Slack bot. This document lists the problems from that period and how the new harness handles each one. The evidence is the commit history and README of the deployment config (a Nix home-manager module), the agent's `MEMORY.md`, and conversations on Slack.

## 1. The prompt is large, and important instructions get buried

- What happened: every turn, the system prompt includes descriptions of unused tools (canvas, nodes, mobile_ui, talk_voice, theme, and others) and OpenClaw's own delivery rules. `MEMORY.md` said that the agent could read Slack with a user token, but the agent forgot this many times. Once, it reported the wrong reason: "I cannot read it because of permissions." It also forgot to use a read-only metrics/logs CLI, because only the alert prompt mentioned that CLI.
- What happened: nothing detects instruction files that contradict each other. A global Claude Code rule file said "Output in English.", and `USER.md` said "Preferred language: Japanese". As a result, the agent answered Japanese questions in English.
- In noctiluca: every turn loads only a short core (the persona, the user, and the tool index). The index links to the details, and the agent reads them when needed. Tools that the agent cannot use appear in neither the prompt nor the tool list. Memory consolidation (R5) finds contradictory instructions and unused text, and fixes them.

## 2. The reply delivery rules are complex

- What happened: if the agent breaks rules such as `final=false`, `NO_REPLY`, and "omit final on the last send", OpenClaw posts a failure notice. Unless the final text of a heartbeat is `NO_REPLY`, OpenClaw posts it as an alert. Once, an explanation of why the agent sent nothing appeared in a team channel. The claude-cli runtime also cannot use `heartbeat_respond`.
- What happened: each send result is several KB of JSON and uses up context.
- In noctiluca: the agent replies only through a tool call (`reply`), and the final text goes nowhere. If the agent sends nothing, nothing is posted. A tool result returns only what the agent needs (success or failure, and the message ID).

## 3. The message tool reads only the current conversation

- What happened: the OpenClaw message tool rejects reads of another channel or thread with `Delegated slack:read requires the exact current conversation and account for this plugin.` As a workaround, the owner configured a separate user token, and the agent read through an HTTP client and the Slack API. The tool also has no search action.
- In noctiluca: the Slack adapter provides reads (threads, history, search, files) as tools. The adapter config decides whether reads use a user token.

## 4. Messages that arrive during a running turn

- What happened: on 2026.10.3, the config made a message that arrived during a running turn abort the turn. 2026.10.10 (openclaw/openclaw#159816) added steering into claude-cli turns.
- In noctiluca: the interface between the core and the model adapter has this from the start (the "Model adapter" section of [design.md](design.md)).

## 5. Limits on shell execution

- What happened: the exec tool rejected background runs, and exec calls made in parallel ran one at a time. To run several Codex tasks at once, the agent used `&` and `wait` inside one foreground command, and split the work into pieces of about 570 seconds that it resumed one after another.
- In noctiluca: shell execution stays with the model side (for example, Claude Code's own Bash tool). The harness does not wrap it in an exec tool of its own.

## 6. Resources leak over long runs

- What happened: each thread and each heartbeat run created an MCP runtime, and the runtime stayed until the gateway stopped. After a week, the runtimes reached the gateway's cap of 256, and every new turn failed. A config change then released runtimes that stayed unused for an hour.
- In noctiluca: processes such as MCP servers are tied to the lifetime of a turn or a session, and always stop when it ends.

## 7. The config cannot easily set the unit of a session

- What happened: with `replyToMode = "all"` and `requireMention = false`, each top-level message in a channel created a new session, and each session read the context again. A config change set `replyToMode` to `"off"`. In the alert channels, the opposite change made the whole channel share one session.
- In noctiluca: one small function in the core assigns each incoming message to a session. To change the assignment, replace that function, not a combination of config values.

## 8. The order of authorization checks

- What happened: the Slack plugin checked a bot sender against the channel's member allowlist before it checked `allowBots`. It dropped error-tracker alerts as `unauthorized-sender`. As a workaround, `botChannels` added the bots to the allowlist.
- In noctiluca: the check for whose messages start a turn is one rule in one place.

## 9. Nix packaging and updates

- What happened: on each OpenClaw update, the npm dist patcher of nix-openclaw failed. The owner kept up by adding patches to a fork of nix-openclaw.
- What happened: after a gateway rebuild, the Slack plugin installed from npm still referenced the old gateway and failed to load. OpenClaw warns on every start that it cannot confirm the origin of a plugin built with Nix.
- What happened: the database migration by `openclaw doctor --fix` is one-way, and it cannot run in Nix mode. Before each update, the owner copies all of `~/.openclaw`.
- What happened: `secrets.json` must be a regular file, so sops-nix and agenix do not work.
- In noctiluca: it has few dependencies and is one package that Nix builds in the usual way. The app migrates its own state schema at startup.

## 10. Where instruction files and memory live

- What happened: each deploy of the home-manager module copied `AGENTS.md` and other files into the working directory and overwrote them. The agent could not edit them, so to fix an instruction it had to ask the owner.
- What happened: `memory_search` needs an OpenAI embedding API key. A Japanese query against English notes scored about 0.16, below the threshold of 0.35. Dreaming creates a file per day in `memory/dreaming/`, but nothing measures whether those files improve behavior.
- What happened: the agent writes remaining work into `memory/todo.md` by hand, and the heartbeat reads it again.
- In noctiluca: memory lives in a git-managed directory, and the agent edits it itself. A rule per file decides how much the agent can edit. Every change stays as a commit, so any change can be undone. The todo list is core data, and the scheduled run reads it.

## 11. Side effects of the claude-cli integration

- What happened: when OpenClaw starts Claude Code, it replaces the user's MCP servers and effort setting with its own.
- In noctiluca: the harness config sets the values that go to the model adapter (MCP servers, effort, model) explicitly. The docs describe how these values combine with the user's `~/.claude`.

## 12. When a turn ends, background subagents stop

- What happened: the agent handed a logo redraw to a Claude Code background subagent and ended its turn. The subagent kept running, but every call to an OpenClaw tool such as `exec` failed with `PreToolUse:mcp__openclaw__exec hook error: The OpenClaw run is no longer active.` It ended without drawing anything. OpenClaw ties the period in which tools work to one turn (run), and the Claude Code process and its subagents live longer than that. The next turn, which the completion notice started, could not use the tools either, because of the same error.
- In noctiluca: the period in which tools work is tied to the session, not the turn. While the model process lives, tools work, also for subagents. The model adapter reports the end of background work to the core as a `ModelEvent`. The core treats it like an incoming message: it passes the event to the running turn, or starts a new turn.
