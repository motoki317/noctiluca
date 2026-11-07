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
- A Slack app with Socket Mode, subscribed to channel and DM message events and `reaction_added`. It needs an app token (`connections:write`) and a bot token. An optional user token (`channels:history`, `channels:read`, `search:read`, `files:read`) lets the agent read any public channel the user joined and search the workspace. Posts always use the bot token.

## Configure

Start from [config.example.yaml](../config.example.yaml). Each token is `{env: NAME}` or `{file: path}`. Relative paths resolve against the config file.

- `conversations.rules`: one rule per Slack channel ID — allowed users and user groups, `allow_bots`, `require_mention`. DMs accept only the owner unless `direct_messages` says otherwise. Unlisted channels start no turns.
- `model.workdir`: where Claude runs. Keep config, database, tokens, and memory outside it. Config validation rejects overlaps.
- `memory.dir`: the memory repository, created as a Git repository if missing. Write your own files in `owner/`, for example `soul.md` for the persona and `user.md` about you; the names are free. Every file in `owner/` and the root `core.md` go into the system prompt of each Claude process. The memory tools refuse to change `owner/`. Everything else belongs to the agent, which edits it with the memory tools. You may seed files there, such as `procedures/heartbeat.md`, which the heartbeat follows; without it the heartbeat does nothing. Commit what you write by hand: the memory tools refuse to write a file with uncommitted edits. `procedures/consolidate.md` and `procedures/hillclimb.md` ship as defaults that a file of the same name overrides.
- `heartbeat`, `cron`, `consolidate`: background tasks. Consolidation is off until `consolidate.enabled: true`.
- `mcp_servers`: external MCP servers. Every session can use the tools they expose; `tools` limits which ones load.

## Conversation events

Before channel rules apply, the Slack adapter selects these events:

- Top-level messages, including edits and deletions.
- Thread messages that mention the agent or belong to a thread where it posted the root or a reply. Edits, deletions, and thread broadcasts follow the same rule.
- Reactions on messages the agent posted.

The configured sender, bot, and mention restrictions still apply to these events. In DMs, every message and reaction reaches the configured `direct_messages` rules.

The adapter checks thread participation through Slack, including posts from before this instance started. If that lookup fails, it logs a warning and forwards the event to the channel rules.

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
