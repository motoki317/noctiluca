# Running noctiluca

## Build

```sh
nix build            # ./result/bin/noctiluca
# or, with Go 1.26.10 or newer:
go build -o bin/noctiluca ./cmd/noctiluca
```

Neither build needs cgo. Release archives for Linux and macOS are on the [Releases](https://github.com/motoki317/noctiluca/releases) page.

## Requirements

- Git, and one model CLI logged in with a subscription: Claude CLI 2.1.316 or newer, or Codex CLI 0.163.0 or newer after `codex login` with a ChatGPT account.
- For each Slack entry, a Slack app with Socket Mode, subscribed to these bot events: `message.channels`, `message.groups` (private channels, with the `groups:history` scope), `message.im`, and `reaction_added`. It needs an app token (`connections:write`) and a bot token. An optional user token (`channels:history`, `channels:read`, `search:read`, `files:read`) lets the agent read public channels that the token's user joined and search the workspace. Posts always use the bot token.
- For each Discord entry, a Discord application with a bot. In the Developer Portal, turn on the Message Content intent. Also turn on the Server Members intent if the entry's `trust` or any conversation override's `trust` lists `groups`. Invite the bot with the `bot` scope and these permissions: View Channels, Send Messages, Send Messages in Threads, Create Public Threads, Read Message History, Add Reactions, Attach Files, and Embed Links. Allow View Channels only in the channels where the bot should work, and turn off Public Bot so that others cannot add it to their servers.
- For each traQ entry, a traQ bot in WebSocket mode and its access token. Subscribe it to `MESSAGE_CREATED`, `MENTION_MESSAGE_CREATED`, `MESSAGE_UPDATED`, `MESSAGE_DELETED`, `DIRECT_MESSAGE_CREATED`, `DIRECT_MESSAGE_UPDATED`, `DIRECT_MESSAGE_DELETED`, and `BOT_MESSAGE_STAMPS_UPDATED`. The bot receives every message in the channels it has joined, and elsewhere only messages that mention it.
- Connect each Discord or traQ bot token once, from one entry in one process. Both platforms send each event to every connection of the token, so a second connection answers every message again.

## Configure

Start from [config.example.yaml](../config.example.yaml). Each token is `{env: NAME}` or `{file: path}`. Relative paths resolve against the config file.

- `messaging`: one entry per chat account, in a list. Each entry has a `name` (lowercase letters, digits, and `_`, starting with a letter), a `type` (`slack`, `discord`, or `traq`), the type's settings under a key named after the type (`slack`: tokens and `group_refresh_interval`; `discord`: `token`; `traq`: `origin` and `access_token`), the account's `owner` user ID (on traQ, the user's UUID), and optional `trust` and `conversations`. Owner notices go to the owner of the first entry. The name prefixes the entry's tool names and the conversation refs stored in the database, so renaming an entry detaches its existing sessions and history.
- `trust` in an entry: an optional allowlist of the senders whose messages start turns. Without `trust`, any human or bot starts turns: every member of a conversation the bot is in, anyone who can DM it, and on traQ anyone who mentions it in a channel it has not joined. Anyone who can add the bot to a channel widens that set. Every turn gets the same tools, a shell included ([Trust model](architecture.md#trust-model)). To restrict senders, set `trust`. Its `users` and `groups` (Slack user groups, Discord role IDs, or traQ group IDs) list people, and `bots` lists bot senders by user ID. On Slack, a post without a user ID, such as one from an incoming webhook, carries only its bot ID (`B…`); if you do not know which ID a bot posts with, list both. In the entry's `trust`, list a bot only if people you do not trust cannot make it post; otherwise, list it in the `trust` of overrides for the channels it serves. The owner is always trusted, so `trust: {}` permits only the owner.
- `conversations` in an entry: optional overrides by channel or DM ID, which also cover the channel's threads. `ignore: true` starts no turns there, not even the owner's, and allows no other key. `require_mention: true` starts turns only on messages that mention the agent, the owner's included, so reactions start none. An override's `trust` replaces the entry's `trust` there, and the owner stays trusted. An override without `trust` follows the entry, so under an entry without `trust` it accepts any sender.
- `model`: `type` is `claude` (default) or `codex`, and `executable` defaults to the type's name. Changing `type` starts each conversation in a fresh session, because neither CLI can resume the other's sessions. An empty `name` uses the CLI's default model. `reasoning_effort` is Codex only. `workdir` is where the model runs. Keep config, database, tokens, and memory outside it. Config validation rejects overlaps.
- `memory.dir`: the memory repository. noctiluca initializes it as a Git repository if missing.
- `heartbeat`, `cron`, `consolidate`: background tasks. Consolidation is off until `consolidate.enabled: true`.
- `mcp_servers`: external MCP servers. Every session can use the tools they expose; `tools` limits which ones load.

## Model CLI home

The model CLI reads its login, settings, and history from its usual home, `~/.claude` or `~/.codex`, and shares them with your own use of the CLI. To give the agent a separate home, set `CLAUDE_CONFIG_DIR` or `CODEX_HOME` in noctiluca's environment, for example in the systemd environment file (see [Run](#run)).

Each session's only MCP server is noctiluca's tool hub, which also serves the servers listed in noctiluca's `mcp_servers`. MCP servers in the CLI's own settings do not load. With Codex, plugins do not load either, and ChatGPT connectors (`features.apps`) are off.

Codex edits `config.toml` in its home when a session's workdir (`model.workdir`, or a cron entry's `workdir`) contains a `.codex/` directory: it adds a `trust_level = "trusted"` entry for that workdir. Keep `.codex/` out of those directories to leave `config.toml` unchanged.

## Memory

Write your own files in `owner/`, for example `soul.md` for the persona and `user.md` about you. The names are free. Every file in `owner/` and the root `core.md` go into the system prompt of each new model process.

Everything else belongs to the agent, which reads and edits memory with its native file tools and commits changes with git. The prompt tells it to leave `owner/` untouched, propose fixes to you, and keep `core.md` under 8 KB. These rules are prompt instructions, and the harness does not enforce them.

To back up memory, give the memory repository a Git remote: a private repository that the Unix user running noctiluca can push to without a prompt. Memory holds what the agent learns about your work. In the memory directory, run `git remote add origin <url>` and `git push -u origin HEAD`. The agent's pushes run in noctiluca's environment, not your shell, so an SSH key with a passphrase works only if the service can reach its agent. When a model process opens, noctiluca checks for a remote URL; with one, the prompt tells the agent to push after each commit, to answer a rejected push with `git push --force-with-lease`, and to tell you about any other push failure. The remote mirrors the local history rather than keeping every version, so nothing else may push to it: edit and commit `owner/` in the memory directory, not in another clone or a web editor. The agent never commits `owner/`; its next push carries your commits.

At startup, noctiluca writes and commits missing defaults for `procedures/consolidate.md` and `procedures/hillclimb.md`. To use newer shipped text, delete the corresponding procedure before restarting. You can also write `procedures/heartbeat.md` for the heartbeat to follow. It is outside `owner/`, so the agent may edit it later. Without that file, the heartbeat does nothing.

## Conversation events

Before trust and conversation overrides apply, each adapter selects these events:

- Top-level messages, including edits and deletions.
- Thread messages that mention the agent or belong to a thread it takes part in. On Slack, that is a thread where it posted the root or a reply. On Discord, it is a thread it owns, joined, or started from its own message. Edits, deletions, and Slack thread broadcasts follow the same rule.
- Reactions on messages the agent posted.

In DMs, every message and reaction reaches the trust check, except on traQ: it reports stamps only on the bot's own messages.

The Slack and Discord adapters check thread participation through the platform, including posts from before noctiluca started. If that lookup fails, they log a warning and forward the event to the trust check.

Each platform's notes tell the agent where to post:

- On Slack, quick answers stay where the request was made. Work on a top-level channel request, such as command results, reviews, drafts, and background completions, goes in the request's thread.
- On Discord, the agent talks in the channel, including quick investigations. It starts a thread from the request only for a project that spans days, or when asked.
- traQ has no threads. Quick answers stay in the channel, and a report on work requested in a channel quotes the request message.
- Before tool work, the agent shows that it started where it will answer: set_status on a Slack thread, otherwise a reaction on the input message.

On each connect, reconnects included, all three adapters read back the messages they missed while disconnected. A read-back message goes through the same trust check as a live one and can start a turn. The adapters read the entry owner's DM and each channel or DM where a top-level message started a turn. In each one they read the messages after the last stored message, at most 24 hours old and at most the newest 200. Discord also reads back edits. In a traQ channel that the bot has not joined, only messages that mention the bot are read back.

Slack also reads back replies in each thread where the agent posted in the last 24 hours, after the agent's last post there or the last stored reply, whichever is later, with the same limits. Replies in other threads are not read back, such as a thread whose parent was posted while disconnected. Discord does not read back thread replies.

Deletions and reactions are not read back. If reading a conversation fails, the adapter logs a warning and skips it. A later live message there moves the last stored message past the gap, so the skipped messages may never be read back.

## Run

```sh
noctiluca --config config.yaml mcp-login <server>   # once per MCP server: opens the browser (or open the printed URL) and waits for the login
noctiluca --config config.yaml run
```

SIGTERM stops the agent and kills the model CLI and `watch` process groups.

MCP logins request refresh tokens when the server supports them. A login without a refresh token logs its expiry date at startup.
Expired or revoked logins disable that server's tools and send one owner notice. Run `mcp-login <server>`, then restart noctiluca to load the replacement login.

As a systemd user service, install the binary to `~/.local/bin`, the config to `~/.config/noctiluca/config.yaml`, and [contrib/noctiluca.service](../contrib/noctiluca.service) to `~/.config/systemd/user/`. The unit reads `{env: …}` tokens from `~/.config/noctiluca/environment` (`NAME=value` lines, mode 0600). Then:

```sh
systemctl --user daemon-reload
systemctl --user enable --now noctiluca.service
journalctl --user -u noctiluca.service -f
```

## Test

```sh
go test -race -shuffle=on ./...
go test -tags live -run Live -v ./cmd/noctiluca   # real Claude CLI (Haiku), fake Slack. Spends subscription requests
NOCTILUCA_LIVE_CODEX_WORKDIR=<dir> go test -tags live -run Live -v ./pkg/infrastructure/codex   # real Codex CLI and tool hub. <dir> must exist and have no .codex/. Spends subscription requests
```

## Release

Push a `v*` tag. CI builds linux/amd64, linux/arm64, and darwin/arm64 archives with checksums and creates the GitHub Release.
