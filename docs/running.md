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
- For each Slack entry, a Slack app with Socket Mode, subscribed to channel and DM message events and `reaction_added`. It needs an app token (`connections:write`) and a bot token. An optional user token (`channels:history`, `channels:read`, `search:read`, `files:read`) lets the agent read any public channel that the token's user joined and search the workspace. Posts always use the bot token.
- For each Discord entry, a Discord application with a bot. In the Developer Portal, turn on the Message Content intent, and the Server Members intent if any rule or `direct_messages` lists `groups`. Invite the bot with the `bot` scope and these permissions: View Channels, Send Messages, Send Messages in Threads, Create Public Threads, Read Message History, Add Reactions, Attach Files, and Embed Links.
- For each traQ entry, a traQ bot in WebSocket mode and its access token. Subscribe it to `MESSAGE_CREATED`, `MENTION_MESSAGE_CREATED`, `MESSAGE_UPDATED`, `MESSAGE_DELETED`, `DIRECT_MESSAGE_CREATED`, `DIRECT_MESSAGE_UPDATED`, `DIRECT_MESSAGE_DELETED`, and `BOT_MESSAGE_STAMPS_UPDATED`. Add the bot to each rule channel unless the rule sets `require_mention: true`. Outside its channels, the bot receives only messages that mention it.
- Connect each Discord or traQ bot token once, from one entry in one process. Both platforms send each event to every connection of the token, so a second connection answers every message again.

## Configure

Start from [config.example.yaml](../config.example.yaml). Each token is `{env: NAME}` or `{file: path}`. Relative paths resolve against the config file.

- `messaging`: one entry per chat account, in a list. Each entry has a `name` (lowercase letters, digits, and `_`, starting with a letter), a `type` (`slack`, `discord`, or `traq`), the type's settings under a key named after the type (`slack`: tokens and `group_refresh_interval`; `discord`: `token`; `traq`: `origin` and `access_token`), the account's `owner` user ID (on traQ, the user's UUID), and `conversations`. Owner notices go to the owner of the first entry. The name prefixes the entry's tool names and the conversation refs stored in the database, so renaming an entry detaches its existing sessions and history.
- `conversations.rules` in an entry: one rule per channel ID, with allowed users and groups (Slack user groups, Discord role IDs, or traQ group IDs), `allow_bots`, and `require_mention` (both false by default). DMs accept only that entry's owner unless `direct_messages` says otherwise. Unlisted channels start no turns.
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

At startup, noctiluca writes and commits missing defaults for `procedures/consolidate.md` and `procedures/hillclimb.md`. To use newer shipped text, delete the corresponding procedure before restarting. You can also write `procedures/heartbeat.md` for the heartbeat to follow. It is outside `owner/`, so the agent may edit it later. Without that file, the heartbeat does nothing.

## Conversation events

Before channel rules apply, each adapter selects these events:

- Top-level messages, including edits and deletions.
- Thread messages that mention the agent or belong to a thread it takes part in. On Slack, that is a thread where it posted the root or a reply. On Discord, it is a thread it owns, joined, or started from its own message. Edits, deletions, and Slack thread broadcasts follow the same rule.
- Reactions on messages the agent posted.

In DMs, every message and reaction reaches the `direct_messages` rule, except on traQ: it reports stamps only on the bot's own messages.

The Slack and Discord adapters check thread participation through the platform, including posts from before noctiluca started. If that lookup fails, they log a warning and forward the event to the channel rules.

The agent posts quick answers where the request was made. It posts work, such as command results, reviews, drafts, and background completions, in reply to a channel request: on Slack, in the request's thread.

Platform differences:

- On Discord, work on a top-level channel request starts a thread from the request message.
- traQ has no threads, so work on a channel request quotes the request message.
- Slack redelivers events that noctiluca did not acknowledge. Discord and traQ do not. On each connect, their adapters read back the entry owner's DM and each channel or DM where a message has started a turn, to the last stored event, up to 200 messages or 24 hours: new and edited messages on Discord, and new messages on traQ. In a traQ channel that the bot has not joined, they read back only messages that mention it. Thread replies, deletions, reactions, and traQ edits made while noctiluca was down are not recovered.

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
