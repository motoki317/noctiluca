<!-- Claude CLI protocol reference: OpenClaw (MIT), https://github.com/openclaw/openclaw.
Original measurements and probe script. See ../THIRD_PARTY_NOTICES.md for provenance and license. -->

# Claude CLI protocol

The Claude adapter requires Claude Code 2.1.316 or later. Measurements on 2026-11-04 used version 2.1.316, the subscription login, `--model haiku`, and these arguments:

```sh
claude --print \
  --input-format stream-json --output-format stream-json --verbose \
  --include-partial-messages --replay-user-messages \
  --permission-prompt-tool stdio --permission-mode default \
  --setting-sources user,project,local --model haiku
```

The probe removed `ANTHROPIC_API_KEY` from the child environment. It never passed `--bare`. User settings, skills, and rules remained available.

The protocol follows OpenClaw 2026.10.10. The relevant installed sources are `dist/cli-runtime-args-*.mjs`, `dist/cli-transport-*.mjs`, and `dist/extensions/anthropic/cli.runtime.js`. The last file implements follow-up input injection from openclaw/openclaw#159816.

## Input and initialization

Each stdin line contains one JSON object followed by a newline. The initial handshake is:

```json
{"type":"control_request","request_id":"initialize","request":{"subtype":"initialize"}}
```

Wait for a `control_response` whose `response.request_id` matches and whose `response.subtype` is `success`. Then send a user message:

```json
{"type":"user","message":{"role":"user","content":"Reply exactly ALPHA."},"parent_tool_use_id":null,"uuid":"00000000-0000-4000-8000-000000000001"}
```

Generate a fresh UUID for each message. Once known, `session_id` can accompany the message. The same line shape carries input during a running turn.

A CLI permission request has `type: control_request`, a `request_id`, and `request.subtype: can_use_tool`. A denial uses:

```json
{"type":"control_response","response":{"subtype":"success","request_id":"permission-request","response":{"behavior":"deny","message":"No interactive permission approval is available."}}}
```

The adapter denies requests that need interactive approval. Native tools already allowed by the user's settings remain available. The adapter allows `mcp__noctiluca__*` because the hub checks session permissions.

## Observed behavior

| Probe | Observation |
|---|---|
| First input | `Reply exactly ALPHA.` returned `ALPHA`. |
| Input during a turn | On the first `stream_event` with `event.type: message_start`, the probe sent `Reply exactly BETA.`. The CLI queued it. |
| Queue order | The first `result` contained `ALPHA`. The second command then started, and its `result` contained `BETA`. The second input neither merged into the first response nor failed. |
| Resume | A new process with `--resume <session_id>` recalled both earlier instructions and returned `ALPHA, BETA`. |
| Turn completion | Each turn emitted `result` with `subtype: success`. `stream_event.event.type: message_stop` also occurred between tool calls, so it does not mark turn completion. |
| Background agent | The Agent tool ran with `model: haiku` and `run_in_background: true`. The stream included `system/task_started`, `system/background_tasks_changed`, and `system/task_notification`. |
| Background completion | The notification had `status: completed` and `summary: BACKGROUND_OK`. It arrived before the parent returned `LAUNCHED` in this run. |

The two ordinary results arrived about 3.9 and 6.4 seconds after startup. The resumed result arrived after 4.3 seconds. Background completion arrived after 6.6 seconds, before the parent result at 7.1 seconds. These timings describe one run, not a latency guarantee.

`command_lifecycle` frames report `queued`, `started`, and `completed`, with a `command_uuid` that matches the input UUID. The adapter writes follow-up inputs immediately and lets this measured CLI queue them. This satisfies the design's fallback when a model cannot merge input into the active turn.

Replayed user messages have `type: user`, `isReplay: true`, and string `message.content`. Assistant messages use content arrays. The parser decodes assistant blocks separately and ignores user replays. It emits text from partial deltas and complete tool inputs from assistant blocks, without duplicating the final assistant text.

`system/task_notification` becomes `background_done`, with the task ID, status, and summary in the event text. The parser accepts completion before or after `result`. Child agent text does not become parent text.

A live run on 2026-11-05 used Claude Code 2.1.318, Haiku, and Bash with `run_in_background: true` to run `sleep 20; echo BGDONE`. The initial turn ended within a few seconds, and another thread received a reply while the command ran. After completion, the CLI emitted `task_notification`, then another assistant turn and `result`, without another user input. The adapter emits `turn_started` for this turn, and the reconciler applies the conversation turn deadline. Reproduce with `go test -tags live -run Live -v ./cmd/noctiluca`.

A failed `result` emits `error` followed by `turn_finished`. Malformed frames, oversized frames, and unexpected process exit terminate the session with an error. Unknown frame types are ignored. Error messages omit raw CLI bodies and stderr because they can contain credentials or tool output.

## Reproduce the measurements

Run `claude --version` and record the installed version. Version 2.1.316 produced the observations above. Save the following as `probe.py` inside a scratch directory in the project. Run `python3 probe.py`. It makes subscription requests, so it is separate from unit tests.

```python
import json
import os
import pathlib
import selectors
import signal
import subprocess
import time
import uuid

base = [
    "claude", "--print", "--input-format", "stream-json",
    "--output-format", "stream-json", "--verbose",
    "--include-partial-messages", "--replay-user-messages",
    "--permission-prompt-tool", "stdio", "--permission-mode", "default",
    "--setting-sources", "user,project,local", "--model", "haiku",
]
env = os.environ.copy()
env.pop("ANTHROPIC_API_KEY", None)
env.pop("CLAUDECODE", None)


def probe(prompt, extra, second=None, background=False):
    process = subprocess.Popen(
        base + extra, stdin=subprocess.PIPE, stdout=subprocess.PIPE,
        stderr=subprocess.DEVNULL, env=env,
        cwd=pathlib.Path(__file__).resolve().parent, start_new_session=True,
    )
    selector = selectors.DefaultSelector()
    selector.register(process.stdout, selectors.EVENT_READ)
    pending = b""
    results = 0
    target = 2 if second else 1
    notified = False
    session = None
    deadline = time.monotonic() + 100

    def send(value):
        process.stdin.write((json.dumps(value) + "\n").encode())
        process.stdin.flush()

    def user(text):
        send({"type": "user", "message": {"role": "user", "content": text},
              "parent_tool_use_id": None, "uuid": str(uuid.uuid4())})

    send({"type": "control_request", "request_id": "initialize",
          "request": {"subtype": "initialize"}})
    try:
        while time.monotonic() < deadline:
            if not selector.select(max(0, deadline - time.monotonic())):
                raise TimeoutError("CLI probe timed out")
            data = os.read(process.stdout.fileno(), 65536)
            if not data:
                raise RuntimeError("CLI exited before the probe completed")
            pending += data
            while b"\n" in pending:
                line, pending = pending.split(b"\n", 1)
                frame = json.loads(line)
                kind = frame.get("type")
                subtype = frame.get("subtype")
                if kind == "control_response":
                    response = frame["response"]
                    if response["request_id"] == "initialize":
                        assert response["subtype"] == "success"
                        user(prompt)
                if kind == "system" and subtype == "init":
                    session = frame["session_id"]
                if second and frame.get("event", {}).get("type") == "message_start":
                    user(second)
                    second = None
                    print("second input written during the first turn")
                if kind == "command_lifecycle":
                    print("command", frame["state"])
                if kind == "control_request":
                    request = frame["request"]
                    allow = (request.get("subtype") == "can_use_tool"
                             and request.get("tool_name") == "Agent")
                    reply = {"behavior": "allow", "updatedInput": request["input"]} if allow else {
                        "behavior": "deny", "message": "Only Agent is authorized in this probe."}
                    send({"type": "control_response", "response": {
                        "subtype": "success", "request_id": frame["request_id"],
                        "response": reply}})
                if kind == "system" and subtype == "task_notification":
                    notified = True
                    print("background", frame["status"], frame.get("summary", ""))
                if kind == "result":
                    assert not frame.get("is_error"), "CLI turn failed"
                    results += 1
                    print("result", frame.get("result", ""))
            if results >= target and (not background or notified):
                return session
        raise TimeoutError("CLI probe timed out")
    finally:
        process.stdin.close()
        try:
            process.wait(timeout=5)
        except subprocess.TimeoutExpired:
            os.killpg(process.pid, signal.SIGTERM)
            try:
                process.wait(timeout=3)
            except subprocess.TimeoutExpired:
                os.killpg(process.pid, signal.SIGKILL)
                process.wait()
        selector.close()


session = probe("Reply exactly ALPHA.", ["--tools", ""], second="Reply exactly BETA.")
probe("What were the two exact words I asked for earlier? Return them only, comma separated.",
      ["--tools", "", "--resume", session])
probe("Use the Agent tool once with model haiku and run_in_background true. "
      "Ask it to reply exactly BACKGROUND_OK without using tools. "
      "Once launched reply exactly LAUNCHED. Do not wait for the result.",
      ["--tools", "Agent", "--allowedTools", "Agent"], background=True)
```

The first two probes disable native tools to keep the requests small. The background probe exposes only Agent. The adapter itself retains native tools and delegates their permissions to Claude settings.

## Adapter startup and shutdown

`claude.New(executable, model)` returns the domain adapter. An empty executable selects `claude`, and an empty model uses the CLI's configured model. `Open` rejects versions below 2.1.316. Later patch versions start without a warning. A newer major or minor version starts with a warning to check protocol compatibility. An unreadable version fails the check.

This minimum keeps CLI self-updates from taking the bot offline. The adapter retains explicit settings-source flags and never passes `--bare`. Recheck subscription login, settings discovery, and the probes above when the CLI changes its startup defaults.

The adapter also removes `ANTHROPIC_AUTH_TOKEN`, `ANTHROPIC_BASE_URL`, `CLAUDECODE`, and `CLAUDE_CODE_SIMPLE` from its child environment. It appends the session prompt to Claude's native prompt through a private file.

The MCP config has this shape:

```json
{"mcpServers":{"noctiluca":{"type":"http","url":"http://127.0.0.1:1234/mcp","headers":{"Authorization":"Bearer <session-token>"}}}}
```

The adapter creates a directory with mode `0700` and files with mode `0600`. It passes the path with `--mcp-config` and uses `--strict-mcp-config` so the model receives only the hub's MCP servers. Without a hub endpoint, the file contains an empty server map. T4 supplies the server and token validation.

The session keeps its config files through background work and removes them on close or process failure. `Close` is idempotent. Cancellation sends SIGTERM to the process group and escalates to SIGKILL after two seconds. If the parent exits first, the adapter kills remaining descendants immediately and drains buffered output. The implementation targets Unix process groups. Tests for orphan cleanup use Linux `/proc`.

Offline tests use a fake executable and never call a model service:

```sh
go test -shuffle=on ./pkg/infrastructure/claude
go test -race -shuffle=on ./pkg/infrastructure/claude
```

A separate real adapter smoke run used `go run bin/.t3-probe/smoke.go` with Haiku. The scratch harness opened a session, sent `ALPHA` and mid-turn `BETA`, closed it, and resumed its token. It observed `ALPHA`, `BETA`, and `ALPHA, BETA`. The harness was local verification material, not a network-dependent unit test.
