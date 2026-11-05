<p align="center"><img src="assets/logo.svg" width="160" alt="noctiluca"></p>

# noctiluca

noctiluca is a minimal, self-hosted agent harness that replaces OpenClaw. It stores memory in Git. An optional nightly job consolidates memory. The name comes from the sea sparkle, *Noctiluca scintillans*.

The Go implementation runs one agent through Slack and the Claude CLI.

- [docs/requirements.md](docs/requirements.md): requirements
- [docs/lessons-from-openclaw.md](docs/lessons-from-openclaw.md): problems from running OpenClaw, and how noctiluca handles each one
- [docs/design.md](docs/design.md): the design and its open questions
- [docs/memory-research.md](docs/memory-research.md): existing research and tools for memory consolidation
- [docs/go-stack.md](docs/go-stack.md): Go libraries and the project layout
- [docs/brand.md](docs/brand.md): the logo and colors

## Build

With Nix flakes enabled, build the Linux package:

```sh
nix build
./result/bin/noctiluca version
```

The package disables cgo and embeds the Git revision as its version. `flake.lock` pins nixpkgs-unstable.
`nix flake check` builds the package and runs its Go tests.
Use `nix develop` for a shell with Go, golangci-lint, mise, and Git.

Without Nix, install Go 1.26.10 or newer and build:

```sh
go build -o bin/noctiluca ./cmd/noctiluca
```

## Run

v0 requires a fresh database. Prototype databases have no upgrade path.

Install Git and Claude CLI 2.1.316 or newer. Log in to Claude with your subscription.
Use a Slack app with Socket Mode and message/reaction event subscriptions enabled.

Copy the example config:

```sh
cp config.example.yaml config.yaml
```

Edit `config.yaml`. Replace the placeholder owner, channel, and user-group IDs, or remove rules you do not need.
Set `SLACK_APP_TOKEN` and `SLACK_BOT_TOKEN` in the process environment.
Each token field accepts `{env: VARIABLE_NAME}` or `{file: path}`. Token files contain only the token, with an optional trailing newline.
An optional user token supplies reads. Posts always use the bot token.

Start the agent:

```sh
bin/noctiluca --config config.yaml run
```

For a Nix build, use `./result/bin/noctiluca` in place of `bin/noctiluca` in these commands.

Paths resolve relative to the config file. Conversation turns use `model.workdir`.
A cron job can override its working directory with `workdir`.
An executable name without a slash uses `PATH`.
Scalar config fields accept environment overrides, such as `NOCTILUCA_LOG_LEVEL=DEBUG`.

`conversations.rules` uses Slack channel IDs, including DM channel IDs. Threads inherit their channel's rule.
Explicit rules override `conversations.direct_messages`.
The default DM rule accepts only the owner. Set `direct_messages: {users: []}` to disable that default.
Unlisted channels and group DMs do not start turns.
A session receives owner tools only when its rule permits the owner alone.
Rules that permit several users receive member tools, including in DMs.

Startup initializes SQLite and the memory Git repository.
Put persona and preferences in `memory/owner/`, short shared rules in `memory/core.md`, and procedures in `memory/procedures/`.
The heartbeat reads `memory/procedures/heartbeat.md` each hour by default. A missing or empty file skips the turn.
Set `heartbeat.enabled: false` to disable it.
Config-defined cron jobs persist in SQLite and return on restart if an agent removes them through a tool.

Set `consolidate.enabled: true` to enable memory consolidation. It shares the background worker with heartbeat and cron jobs.
The default schedule is 04:00 UTC. Set `consolidate.tz` to your IANA timezone and `consolidate.cron` to change the schedule.
The shipped procedure commits taught facts and clear factual corrections, then sends behavioral proposals to the owner's DM.
Override it with `memory/procedures/consolidate.md`. An empty override skips the turn without advancing the checkpoint.
The shipped `procedures/hillclimb.md` is available through memory tools for owner-requested behavior experiments.
Defaults remain embedded until overridden and do not consume memory size caps.

All incoming work persists in SQLite before acknowledgement. One reconciler dispatches conversation sessions concurrently and task sessions one at a time.
`tasks.timeout` bounds background turns (default `30m`). `model.turn_timeout` bounds conversation turns (default `2h`).
A hung task releases the worker after its process exits.
Inputs retry only before successful delivery to the model. After three delivery failures, the owner receives a failure notice.
Successful delivery marks the input done. Later turn failures never replay it.
Pending inputs survive restart. Claimed inputs without a committed delivery return to pending, including owner notices.
A crash between `Send` and its database commit can repeat delivery.
A task turn deadline stops the task and sends one owner notice. Other interruptions allow one continuation with a short interruption note.
Watches record their command before spawn and their result before delivery. An interrupted watch produces a restart notice and is never rerun.

Memory tools reserve `owner/`, `core.md`, and `procedures/` edits for owner sessions and tasks.
The core-size cap applies to writes. Oversized files still load, and `memory_sizes` reports their size.

`model.workdir` defaults to `work/` beside the database and is created with mode `0700`.
Keep config, database, token files, and memory outside it. Config validation rejects overlapping paths.

External MCP tools require the owner unless their server sets `members: true`.
Add servers to `mcp_servers`, then authorize each one:

```sh
bin/noctiluca --config config.yaml mcp-login tracker
```

Open the printed authorization URL in your browser. The command stores the login in SQLite.
Restart `run` after login. Servers without a saved login are skipped with a warning.
The tool hub listens on `127.0.0.1` only. Port `0` selects a free port.

Send a message from an accepted user to start a turn. Only a tool call posts a reply.
Press Ctrl-C or send SIGTERM to stop the agent, close its connections, and kill model and watch process groups.

### systemd user service

After `nix build`, install the binary, config, and [example unit](contrib/noctiluca.service):

```sh
install -Dm755 result/bin/noctiluca ~/.local/bin/noctiluca
install -Dm600 config.yaml ~/.config/noctiluca/config.yaml
install -Dm644 contrib/noctiluca.service ~/.config/systemd/user/noctiluca.service
```

Review paths in the installed config. Relative paths now resolve under `~/.config/noctiluca/`.
Use absolute paths to keep an existing database or memory repository.
For tokens configured through environment variables, put `NAME=value` entries in `~/.config/noctiluca/environment` and set its permissions to `0600`.
File-based token config also works.
The unit includes `~/.local/bin` and `~/.nix-profile/bin` in `PATH`. Edit its `PATH` if Git or Claude lives elsewhere.

Start the service at login and now:

```sh
systemctl --user daemon-reload
systemctl --user enable --now noctiluca.service
journalctl --user -u noctiluca.service -f
```

The service restarts on failure. `KillMode=control-group` stops Claude and watch children when the service stops.
Use `systemctl --user stop noctiluca.service` to stop it.
Repeat the binary installation and run `systemctl --user restart noctiluca.service` after an upgrade.

## Live smoke test

Install `claude` on `PATH` and log in with your subscription, then run:

```sh
go test -tags live -run Live -v ./cmd/noctiluca
```

This test spends subscription requests using Haiku and keeps your Claude CLI settings.
It removes `ANTHROPIC_API_KEY` from the child environment and uses temporary memory and database files.
Slack uses a local fake Web API and Socket Mode server.
The test covers replies, queued input, thread history, watch completion, restart recall, and process cleanup.
Normal `go test ./...` excludes it. It also skips when `CI` is set.

## License

noctiluca is licensed under the [MIT License](LICENSE). See [THIRD_PARTY_NOTICES.md](THIRD_PARTY_NOTICES.md) for source credits and upstream license notices.
