// Package claude drives the official Claude Code CLI as a noctiluca model adapter.
//
// The stream-json protocol follows OpenClaw (MIT, https://github.com/openclaw/openclaw);
// see THIRD_PARTY_NOTICES.md. The behaviour below was measured against CLI 2.1.316–2.1.319
// with a subscription login; the live test re-checks it
// (go test -tags live -run Live -v ./cmd/noctiluca).
//
//   - One long-lived `claude --print --input-format stream-json --output-format stream-json`
//     process serves one session. Input starts after an `initialize` control request succeeds.
//   - Input written during a turn is queued for the next turn, and several queued inputs can start
//     together in one turn. The CLI reports each input in `command_lifecycle` frames keyed by its
//     uuid: `queued`, then `started` when its turn begins. The adapter reports `started` as
//     input_started, and `refused`, `discarded`, or `cancelled` before a start as input_refused.
//   - `result` ends a turn. `message_stop` also appears between tool calls, so it does not.
//   - Background Bash or Agent work reports `system/task_notification` when it finishes, and the
//     CLI then starts a turn by itself, without input. The adapter emits turn_started for that
//     turn so the reconciler marks the session busy and applies the turn deadline.
//   - `--resume <session_id>` restores the conversation in a new process.
//   - Idle sessions with at least 200000 context tokens and a one-hour cache compact internally after 54 minutes.
//
// CLI 2.1.316 and 2.1.321 never defer tools with _meta {"anthropic/alwaysLoad": true}, so their schemas survive compaction.
// A server-level alwaysLoad: false in --mcp-config overrides the tool metadata.
//
// The adapter never passes --bare and keeps user, project, and local settings: bare mode skips
// the subscription login and ~/.claude. It removes API-key and base-URL variables from the child
// environment so turns bill to the subscription. Versions below 2.1.316 are refused; a major or
// minor version newer than 2.1 only logs a warning, because the CLI updates itself.
package claude
