//go:build live

package main

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	sdk "github.com/slack-go/slack"
	"github.com/stretchr/testify/require"

	"github.com/motoki317/noctiluca/pkg/domain"
	"github.com/motoki317/noctiluca/pkg/infrastructure/repository"
)

// This test spends subscription requests and retains the user's CLI settings.
func TestLiveRun(t *testing.T) {
	if os.Getenv("CI") != "" {
		t.Skip("live subscription test is disabled in CI")
	}
	executable, err := exec.LookPath("claude")
	require.NoError(t, err)
	dir := t.TempDir()
	t.Setenv("TEST_APP_TOKEN", "xapp-fake")
	t.Setenv("TEST_BOT_TOKEN", "xoxb-fake")
	t.Setenv("NOCTILUCA_LIVE_CLAUDE", executable)
	t.Setenv("NOCTILUCA_LIVE_PROCESSES", filepath.Join(dir, "processes"))
	wrapper := filepath.Join(dir, "claude")
	require.NoError(t, os.WriteFile(wrapper, []byte(`#!/bin/sh
[ "${ANTHROPIC_API_KEY+x}" != x ] || exit 90
if [ "$1" != --version ]; then
 printf '%s %s\n' "$$" "$*" >> "$NOCTILUCA_LIVE_PROCESSES"
fi
exec "$NOCTILUCA_LIVE_CLAUDE" "$@"
`), 0700))
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "memory", "owner"), 0700))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "memory", "owner", "smoke.md"), []byte("This is a smoke test. Follow each user request literally. Post only through the reply tool. Do not read or change files, use external services, or persist test words in memory. When asked to watch a command, call watch, end the turn immediately, and reply only after its exit notification.\n"), 0600))
	path := filepath.Join(dir, "config.yaml")
	require.NoError(t, os.WriteFile(path, []byte("owner: U_OWNER\nslack:\n  app_token: {env: TEST_APP_TOKEN}\n  bot_token: {env: TEST_BOT_TOKEN}\nmodel: {executable: ./claude, name: haiku, turn_timeout: 2m}\nheartbeat: {enabled: false}\nconversations:\n  rules:\n    - {id: C_ROOM, users: [U_OWNER], require_mention: true}\n"), 0600))
	config, err := loadConfig(path)
	require.NoError(t, err)
	entered, release := make(chan struct{}), make(chan struct{})
	var enteredOnce, releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	defer unblock()
	slack := newRunSlack(t, func(ctx context.Context, post url.Values) {
		if strings.TrimSpace(post.Get("text")) == "QUEUE_FIRST" {
			enteredOnce.Do(func() { close(entered) })
			select {
			case <-release:
			case <-ctx.Done():
			}
		}
	})
	var app *application
	var socket *websocket.Conn
	var stop func()
	start := func() {
		app, err = newApplication(t.Context(), config, nil, sdk.OptionAPIURL(slack.URL+"/"))
		require.NoError(t, err)
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan error, 1)
		go func() { done <- app.Start(ctx) }()
		var once sync.Once
		stop = func() {
			once.Do(func() {
				cancel()
				select {
				case err := <-done:
					require.NoError(t, err)
				case <-time.After(15 * time.Second):
					t.Error("assembly shutdown timed out")
				}
			})
		}
		t.Cleanup(stop)
		socket = liveTake(t, slack.connections)
	}
	sequence := 0
	send := func(t *testing.T, channel, thread, text string) string {
		t.Helper()
		sequence++
		id := fmt.Sprintf("live-%d", sequence)
		event := map[string]any{"type": "message", "channel": channel, "user": "U_OWNER", "text": text, "ts": fmt.Sprintf("%d.000001", sequence+100)}
		if thread != "" {
			event["thread_ts"] = thread
		}
		require.NoError(t, socket.WriteJSON(map[string]any{"type": "events_api", "envelope_id": id, "payload": map[string]any{"type": "event_callback", "event_id": id, "event": event}}))
		require.Equal(t, id, liveTake(t, slack.acks))
		return "slack:" + id
	}
	reply := func(t *testing.T, channel, thread, word string) {
		t.Helper()
		post := liveTake(t, slack.posts)
		require.Equal(t, channel, post.Get("channel"))
		require.Equal(t, thread, post.Get("thread_ts"))
		require.Contains(t, post.Get("text"), word)
	}
	idle := func(t *testing.T, key domain.SessionKey) domain.Session {
		t.Helper()
		var saved domain.Session
		require.Eventually(t, func() bool {
			var err error
			saved, err = repository.NewSessionRepository(app.db).Get(t.Context(), key)
			return err == nil && saved.Turn == nil && saved.ResumeToken != ""
		}, 90*time.Second, 20*time.Millisecond)
		return saved
	}
	run := func(name string, fn func(*testing.T)) {
		if !t.Run(name, fn) {
			t.FailNow()
		}
	}
	start()
	var token string
	run("1_DM_PONG", func(t *testing.T) {
		send(t, "D_OWNER", "", "reply with the word PONG using the reply tool")
		reply(t, "D_OWNER", "", "PONG")
		token = idle(t, "slack:D_OWNER:").ResumeToken
	})
	stop()
	liveNoProcesses(t, dir)
	start()
	run("5_restart_resume", func(t *testing.T) {
		send(t, "D_OWNER", "", "Recall the single word I asked you to reply with in my first message in this conversation. Reply with only that word using the reply tool. Do not read files.")
		reply(t, "D_OWNER", "", "PONG")
		require.Equal(t, token, idle(t, "slack:D_OWNER:").ResumeToken)
		data, err := os.ReadFile(filepath.Join(dir, "processes"))
		require.NoError(t, err)
		require.Contains(t, string(data), "--resume "+token)
	})
	run("2_queued_input", func(t *testing.T) {
		send(t, "D_OWNER", "", "Reply with exactly QUEUE_FIRST using the reply tool.")
		liveTake(t, entered)
		var finished int
		require.NoError(t, app.db.QueryRow("SELECT count(*) FROM model_events WHERE session_key='slack:D_OWNER:' AND kind='turn_finished'").Scan(&finished))
		id := send(t, "D_OWNER", "", "Reply with exactly QUEUE_SECOND using the reply tool.")
		require.Eventually(t, func() bool {
			var status string
			return app.db.QueryRow("SELECT status FROM inbox WHERE source_key=?", id).Scan(&status) == nil && status == "done"
		}, 10*time.Second, 10*time.Millisecond)
		var after int
		require.NoError(t, app.db.QueryRow("SELECT count(*) FROM model_events WHERE session_key='slack:D_OWNER:' AND kind='turn_finished'").Scan(&after))
		require.Equal(t, finished, after, "second message must reach the running turn")
		unblock()
		reply(t, "D_OWNER", "", "QUEUE_FIRST")
		reply(t, "D_OWNER", "", "QUEUE_SECOND")
		idle(t, "slack:D_OWNER:")
	})
	run("3_thread_parent_history", func(t *testing.T) {
		marker := fmt.Sprintf("PARENT-%d", time.Now().UnixNano())
		send(t, "C_ROOM", "", "Parent-only code: "+marker)
		thread := fmt.Sprintf("%d.000001", sequence+100)
		send(t, "C_ROOM", thread, "<@U_BOT> Reply with only the Parent-only code from the parent_history attached to this input, using the reply tool. Do not fetch history or read files.")
		reply(t, "C_ROOM", thread, marker)
		idle(t, domain.SessionKey("slack:C_ROOM:"+thread))
	})
	run("4_watch_completion", func(t *testing.T) {
		send(t, "D_WATCH", "", fmt.Sprintf("Call the watch tool exactly once with command `sleep 5; echo DONE` and cwd %q. Then end this turn immediately without replying or waiting. When the exit notification arrives, use the reply tool to report its output.", config.Model.Workdir))
		reply(t, "D_WATCH", "", "DONE")
		idle(t, "slack:D_WATCH:")
		var finishedBeforeExit, finished int
		require.NoError(t, app.db.QueryRow(`SELECT count(*) FROM model_events WHERE session_key='slack:D_WATCH:' AND kind='turn_finished' AND recorded_at < (SELECT ready_at FROM inbox WHERE source_key LIKE 'watch:%' LIMIT 1)`).Scan(&finishedBeforeExit))
		require.Positive(t, finishedBeforeExit, "watch turn must finish before command exit")
		require.NoError(t, app.db.QueryRow(`SELECT count(*) FROM model_events WHERE session_key='slack:D_WATCH:' AND kind='turn_finished'`).Scan(&finished))
		require.GreaterOrEqual(t, finished, 2)
		var command, outcome string
		require.NoError(t, app.db.QueryRow("SELECT command,outcome FROM watches").Scan(&command, &outcome))
		require.Equal(t, "sleep 5; echo DONE", command)
		require.Contains(t, outcome, "DONE")
	})
	run("6_shutdown", func(t *testing.T) { stop(); liveNoProcesses(t, dir) })
}

func liveTake[T any](t *testing.T, ch <-chan T) T {
	t.Helper()
	select {
	case value := <-ch:
		return value
	case <-time.After(90 * time.Second):
		t.Fatal("live operation timed out")
		var zero T
		return zero
	}
}
func liveNoProcesses(t *testing.T, dir string) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, "processes"))
	require.NoError(t, err)
	require.NotEmpty(t, data)
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		pid, err := strconv.Atoi(strings.Fields(line)[0])
		require.NoError(t, err)
		require.Eventually(t, func() bool { return syscall.Kill(pid, 0) == syscall.ESRCH }, 5*time.Second, 20*time.Millisecond, "claude process %d survived shutdown", pid)
	}
}
