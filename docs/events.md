# Events

A local program can push an event to the agent. It sends an HTTP POST to noctiluca, and the body reaches the agent as one input, as a chat message does.

## Configure

Add this to the config and restart noctiluca:

```yaml
events:
  listen: 127.0.0.1:8790
  token: {file: events-token}
```

- `listen` must be `127.0.0.1:<port>`.
- `token` is the one secret that every sender presents. Create the file beside the config: `(umask 077; openssl rand -hex 32 > ~/.config/noctiluca/events-token)`.
- `{env: NAME}` works too, but not with the name `NOCTILUCA_EVENTS_TOKEN`. noctiluca reads each `NOCTILUCA_` variable as a config key, so that variable hides the `events.token` setting, and noctiluca stops at startup with `events.token: token source is missing or empty`.

## Connect a command

```bash
curl -fsS --data-binary 'nightly build failed' \
  -H @<(printf 'Authorization: Bearer %s' "$(cat ~/.config/noctiluca/events-token)") \
  http://127.0.0.1:8790/events/build
```

The `-H @<(…)` form, which needs bash, keeps the token out of the process list, where other Unix users can read it.

- The path is `/events/<name>`. The name starts with a lowercase letter and holds only lowercase letters, digits, and `_`.
- noctiluca passes a UTF-8 body on as it is, JSON or text. It keeps the first 16 KiB of a longer body and tells the agent that the body was cut.
- noctiluca must receive the whole request within 3 seconds, so send a finished body, not a stream.
- noctiluca answers 202 after it saves the input. It answers 401 for a missing or wrong token, 400 for an invalid name or a malformed request, and 404 or 405 for any other path or method: fix the sender. On any other answer, such as 408 for a request that took too long, send the event again later.
- A sender that retries must send an `Idempotency-Key` header. If an earlier POST to the same event name used the same key, noctiluca saves nothing and still answers 202. noctiluca keeps every key for good, so use a key that names one event, such as a message ID, not a date.

noctiluca turns each POST into an input and does not filter or check the body. What a sender pushes, and whether that content is safe to hand to the agent, is the sender's responsibility. The prompt tells the agent that an event body is data, not instructions. Still, text that strangers can write, such as an email subject, can steer the agent, and every turn has a shell ([Trust](design.md#trust)).

## What the agent sees

```
Event build | external | 09:12:03 JST

nightly build failed
```

The prompt also tells the agent that nobody waits for a reply. Tell the agent what to do with each event name in a file under `owner/` in the memory directory ([Running](running.md#6-set-up-memory)).

## Example: Gmail with gog

A systemd user timer for each Google account runs a script every minute. The script keeps a cursor, the Gmail history ID up to which it has posted. It asks [gog](https://github.com/openclaw/gogcli) for the messages added since the cursor, posts their IDs, and moves the cursor only after noctiluca answers 2xx. The agent then reads each message with gog, so this example lets mail from strangers reach a turn.

1. Authorize each account read-only, and give it an alias for the unit names:

   ```sh
   gog auth add you@example.com --services gmail --readonly
   gog auth alias set personal you@example.com
   gog auth add you@work.example.com --services gmail --readonly
   gog auth alias set work you@work.example.com
   ```

2. Save this script as `~/.local/bin/gmail-events` and make it executable:

   ```bash
   #!/bin/bash
   # Usage: gmail-events <gog account or alias>
   set -euo pipefail
   account=$1
   cursor=${XDG_STATE_HOME:-$HOME/.local/state}/gmail-events/$account
   token=$(cat ~/.config/noctiluca/events-token)
   if [[ ! -s $cursor ]]; then
     # The first run starts from the newest message and posts nothing.
     mkdir -p "${cursor%/*}"
     id=$(gog gmail messages search in:anywhere --max 1 --json -a "$account" | jq -r '.messages[0].id')
     gog gmail get "$id" --format metadata --json -a "$account" | jq -r .message.historyId > "$cursor"
     exit
   fi
   history=$(gog gmail history --since "$(cat "$cursor")" --all --json -a "$account")
   next=$(jq -r .historyId <<<"$history")
   if [[ $(jq '.messages | length' <<<"$history") -gt 0 ]]; then
     jq -c --arg account "$account" '{account: $account, messages}' <<<"$history" |
       curl -fsS --data-binary @- \
         -H "Idempotency-Key: $account:$next" \
         -H @<(printf 'Authorization: Bearer %s' "$token") \
         http://127.0.0.1:8790/events/gmail
   fi
   echo "$next" > "$cursor"
   ```

3. Add two template units in `~/.config/systemd/user/`. `gmail-events@.service`:

   ```ini
   [Service]
   Type=oneshot
   # gog, jq, and curl must be on this PATH.
   Environment="PATH=%h/.local/bin:/usr/local/bin:/usr/bin:/bin"
   ExecStart=%h/.local/bin/gmail-events %i
   ```

   `gmail-events@.timer`:

   ```ini
   [Timer]
   OnActiveSec=1min
   OnUnitActiveSec=1min

   [Install]
   WantedBy=timers.target
   ```

4. Start one timer for each account. Two accounts are two units:

   ```sh
   systemctl --user daemon-reload
   systemctl --user enable --now gmail-events@personal.timer gmail-events@work.timer
   ```

The agent receives `{"account":"personal","messages":["<id>",…]}`. The list holds every message added to the mailbox, your own sent mail included. In `owner/`, tell the agent to read a message with `gog gmail get <id> -a <account>`, and what to do with it.

Gmail usually keeps a cursor valid for at least a week, and sometimes for only a few hours. Once a cursor expires, `gog gmail history` fails on every run. Delete the cursor file, such as `~/.local/state/gmail-events/personal`, to start over from the newest message.
