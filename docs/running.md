# Running noctiluca

Anyone who can message the agent can make it run shell commands as your Unix user. Read [Trust](design.md#trust) before you connect it to a shared workspace.

## 1. Install

Download an archive for Linux or macOS from [Releases](https://github.com/motoki317/noctiluca/releases), or build the binary:

```sh
nix build            # Linux only. Writes ./result/bin/noctiluca
# or, with Go 1.26.10 or newer:
go build -o bin/noctiluca ./cmd/noctiluca
```

## 2. Install Git and a model CLI

noctiluca needs Git and one model CLI:

- Claude Code CLI 2.1.316 or newer, logged in with a Claude subscription, or
- Codex CLI 0.163.0 or newer, logged in with a ChatGPT account (`codex login`).

The agent uses the CLI's usual home, `~/.claude` or `~/.codex`, and shares its login, settings, and history with you.
To give the agent its own home, set `CLAUDE_CONFIG_DIR` or `CODEX_HOME` in noctiluca's environment.
The agent does not get the MCP servers of the CLI's settings. It gets only the servers under `mcp_servers` in noctiluca's config.

## 3. Create a chat bot

Each platform gives you tokens. You put them in the config in step 4.

**Slack.** Create an app from [contrib/slack-app-manifest.yaml](../contrib/slack-app-manifest.yaml) and install it to your workspace. Keep its bot token. In the app's settings, generate an app-level token with the `connections:write` scope.

**Discord.** Create an application with a bot, and keep the bot's token.

- Turn on the Message Content intent. Turn on the Server Members intent too if you plan to trust Discord roles (`trust.groups` in the config).
- Turn off Public Bot, so that others cannot add the bot to their servers.
- Invite the bot with the `bot` scope and these permissions: View Channels, Send Messages, Send Messages in Threads, Create Public Threads, Read Message History, Add Reactions, Attach Files, and Embed Links.
- After the invite, restrict View Channels to the channels where the bot works, because a message in any channel that the bot sees can start a turn.

**traQ.** Create a bot in WebSocket mode, and keep its access token. Subscribe the bot to these events:

```
MESSAGE_CREATED  MENTION_MESSAGE_CREATED  MESSAGE_UPDATED  MESSAGE_DELETED
DIRECT_MESSAGE_CREATED  DIRECT_MESSAGE_UPDATED  DIRECT_MESSAGE_DELETED
BOT_MESSAGE_STAMPS_UPDATED
```

Use each Discord or traQ bot token in one `messaging` entry of one running noctiluca only. Both platforms send each event to every connection of a token, so a second connection answers every message again.

## 4. Configure

Copy [config.example.yaml](../config.example.yaml) to `config.yaml` and edit it. Its comments explain every key.

## 5. Run

```sh
noctiluca --config config.yaml run
```

To run noctiluca as a systemd user service, put these in place:

| What | Where |
|---|---|
| the binary | `~/.local/bin/noctiluca` |
| `config.yaml` | `~/.config/noctiluca/config.yaml` |
| [contrib/noctiluca.service](../contrib/noctiluca.service) | `~/.config/systemd/user/noctiluca.service` |
| an environment file, mode 0600, with a `NAME=value` line for each token that the config reads from the environment | `~/.config/noctiluca/environment` |

Relative paths in the config resolve against the config file, so the database, the memory directory, and the model's working directory land in `~/.config/noctiluca/` too.

```sh
systemctl --user daemon-reload
systemctl --user enable --now noctiluca.service
journalctl --user -u noctiluca.service -f
```

## 6. Set up memory

The memory directory (`memory.dir`) is a Git repository that noctiluca creates on the first run.

- **Your files.** Write who the agent is and who you are in `owner/`, for example `owner/soul.md` and `owner/user.md`. Every file in `owner/` loads into the system prompt. An edit takes effect after you restart noctiluca. noctiluca tells the agent to leave `owner/` alone, and the agent owns every other file.
- **Backup.** Add a private Git remote that the Unix user of noctiluca can push to without a prompt: in the memory directory, run `git remote add origin <url>` and `git push -u origin HEAD`. The agent then pushes after each commit. When the remote rejects a push, the agent overwrites the remote with its local history, so a commit that anything else pushed is lost. Edit and commit `owner/` in the memory directory, not in another clone or a web editor.
- **Your own steps.** The heartbeat makes the agent check its open work every hour. To add steps to the heartbeat or to the nightly rest, write them in `procedures/heartbeat.md` or `procedures/rest.md` in the memory directory. Say where each step fits: name a section of the shipped procedure ([heartbeat.md](../pkg/usecase/memory/heartbeat.md), [rest.md](../pkg/usecase/memory/rest.md)), or quote the sentence that your step replaces.
