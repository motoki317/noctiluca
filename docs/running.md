# Running noctiluca

## Build

```sh
nix build            # ./result/bin/noctiluca
# or, with Go 1.26.10 or newer:
go build -o bin/noctiluca ./cmd/noctiluca
```

Neither build needs cgo. Release archives for Linux and macOS are on the [Releases](https://github.com/motoki317/noctiluca/releases) page.

## Requirements

- Git, and Claude CLI 2.1.316 or newer logged in with a subscription.
- For each Slack entry, a Slack app with Socket Mode, subscribed to channel and DM message events and `reaction_added`. It needs an app token (`connections:write`) and a bot token. An optional user token (`channels:history`, `channels:read`, `search:read`, `files:read`) lets the agent read any public channel that the token's user joined and search the workspace. Posts always use the bot token.

## Configure

Start from [config.example.yaml](../config.example.yaml). Each token is `{env: NAME}` or `{file: path}`. Relative paths resolve against the config file.

- `messaging`: one entry per chat account, in a list. Each entry has a `name` (lowercase letters, digits, and `_`, starting with a letter), a `type` (`slack`), the type's settings (Slack: tokens and `group_refresh_interval`), the account's `owner` user ID, and `conversations`. Owner notices go to the owner of the first entry. The name prefixes the entry's tool names and the conversation refs stored in the database, so renaming an entry detaches its existing sessions and history.
- `conversations.rules` in an entry: one rule per channel ID, with allowed users and user groups, `allow_bots`, and `require_mention`. DMs accept only that entry's owner unless `direct_messages` says otherwise. Unlisted channels start no turns.
- `model.workdir`: where Claude runs. Keep config, database, tokens, and memory outside it. Config validation rejects overlaps.
- `memory.dir`: the memory repository. noctiluca initializes it as a Git repository if missing.
- `heartbeat`, `cron`, `consolidate`: background tasks. Consolidation is off until `consolidate.enabled: true`.
- `mcp_servers`: external MCP servers. Every session can use the tools they expose; `tools` limits which ones load.

## Memory

Write your own files in `owner/`, for example `soul.md` for the persona and `user.md` about you. The names are free. Every file in `owner/` and the root `core.md` go into the system prompt of each new model process.

Everything else belongs to the agent, which reads and edits memory with its native file tools and commits changes with git. The prompt tells it to leave `owner/` untouched, propose fixes to you, and keep `core.md` under 8 KB. These rules are prompt instructions, and the harness does not enforce them.

At startup, noctiluca writes and commits missing defaults for `procedures/consolidate.md` and `procedures/hillclimb.md`. To use newer shipped text, delete the corresponding procedure before restarting. You can also write `procedures/heartbeat.md` for the heartbeat to follow. It is outside `owner/`, so the agent may edit it later. Without that file, the heartbeat does nothing.

## Conversation events

Before channel rules apply, the Slack adapter selects these events:

- Top-level messages, including edits and deletions.
- Thread messages that mention the agent or belong to a thread where it posted the root or a reply. Edits, deletions, and thread broadcasts follow the same rule.
- Reactions on messages the agent posted.

The configured sender, bot, and mention restrictions still apply to these events. In DMs, every message and reaction reaches the configured `direct_messages` rules.

The adapter checks thread participation through Slack, including posts from before noctiluca started. If that lookup fails, it logs a warning and forwards the event to the channel rules.

## Run

```sh
noctiluca --config config.yaml mcp-login <server>   # once per MCP server: opens the browser (or open the printed URL) and waits for the login
noctiluca --config config.yaml run
```

SIGTERM stops the agent and kills Claude and `watch` process groups.

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
```

## Release

Push a `v*` tag. CI builds linux/amd64, linux/arm64, and darwin/arm64 archives with checksums and creates the GitHub Release.
