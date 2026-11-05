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
- `memory.dir`: the memory repository, created as a Git repository if missing. Put persona and preferences in `owner/` and rules for every turn in `core.md`. The heartbeat follows `procedures/heartbeat.md`, which you write. Without it the heartbeat does nothing. `procedures/consolidate.md` and `procedures/hillclimb.md` ship as defaults that a file of the same name overrides.
- `heartbeat`, `cron`, `consolidate`: background tasks. Consolidation is off until `consolidate.enabled: true`.
- `mcp_servers`: external MCP servers. Their tools are owner-only unless `members: true`.


## Run

```sh
noctiluca --config config.yaml mcp-login <server>   # once per MCP server: open the printed URL, the command waits for the login
noctiluca --config config.yaml run
```

SIGTERM stops the agent and kills Claude and `watch` process groups.

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
