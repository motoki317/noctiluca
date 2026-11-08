// Claude CLI protocol reference: OpenClaw (MIT), https://github.com/openclaw/openclaw.
// Go reimplementation. Provenance and license notice: THIRD_PARTY_NOTICES.md.

package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	sdk "github.com/slack-go/slack"
	"github.com/stretchr/testify/require"

	"github.com/motoki317/noctiluca/pkg/domain"
	"github.com/motoki317/noctiluca/pkg/infrastructure/repository"
	"github.com/motoki317/noctiluca/pkg/infrastructure/slack"
	"github.com/motoki317/noctiluca/pkg/usecase/consolidate"
	"github.com/motoki317/noctiluca/pkg/usecase/reconcile"
	"github.com/motoki317/noctiluca/pkg/usecase/watch"
)

type runCapture struct {
	PID, ChildPID, WatchPID              int
	URL, MCPFile, Input, Workdir, Prompt string
}

type bearerTransport string

func (b bearerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", string(b))
	return http.DefaultTransport.RoundTrip(r)
}

func TestRunCLIHelper(t *testing.T) {
	if os.Getenv("NOCTILUCA_RUN_TEST_BINARY") == "" {
		return
	}
	if os.Getenv("NOCTILUCA_RUN_CHILD") == "1" {
		signal.Ignore(syscall.SIGTERM)
		for {
			_ = syscall.Pause()
		}
	}
	args := os.Args
	for i, arg := range args {
		if arg == "--" {
			args = args[i+1:]
			break
		}
	}
	if len(args) == 1 && args[0] == "--version" {
		fmt.Println("2.1.316 (Claude Code)")
		os.Exit(0)
	}
	capture := runCapture{PID: os.Getpid()}
	capture.Workdir, _ = os.Getwd()
	for i, arg := range args {
		if arg == "--append-system-prompt-file" {
			data, err := os.ReadFile(args[i+1])
			require.NoError(t, err)
			capture.Prompt = string(data)
		}
		if arg == "--mcp-config" {
			capture.MCPFile = args[i+1]
		}
	}
	data, err := os.ReadFile(capture.MCPFile)
	require.NoError(t, err)
	var config struct {
		Servers map[string]struct {
			URL     string
			Headers map[string]string
		} `json:"mcpServers"`
	}
	require.NoError(t, json.Unmarshal(data, &config))
	endpoint := config.Servers["noctiluca"]
	capture.URL = endpoint.URL
	child := exec.Command(os.Getenv("NOCTILUCA_RUN_TEST_BINARY"), "-test.run=TestRunCLIHelper")
	child.Env = append(os.Environ(), "NOCTILUCA_RUN_CHILD=1")
	require.NoError(t, child.Start())
	capture.ChildPID = child.Process.Pid
	scanner := bufio.NewScanner(os.Stdin)
	for scanner.Scan() {
		var frame struct {
			Type    string
			UUID    string
			Message struct{ Content string }
		}
		require.NoError(t, json.Unmarshal(scanner.Bytes(), &frame))
		if frame.Type == "control_request" {
			fmt.Println(`{"type":"control_response","response":{"request_id":"initialize","subtype":"success"}}`)
			fmt.Println(`{"type":"system","subtype":"init","session_id":"run-session"}`)
		}
		if frame.Type != "user" {
			continue
		}
		capture.Input = frame.Message.Content
		fmt.Printf("{\"type\":\"command_lifecycle\",\"command_uuid\":%q,\"state\":\"started\"}\n", frame.UUID)
		recovery := os.Getenv("NOCTILUCA_RUN_RECOVERY") != ""
		if recovery {
			record, err := os.OpenFile(filepath.Join(os.Getenv("NOCTILUCA_RUN_TEST_DIR"), "recovery.jsonl"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
			require.NoError(t, err)
			require.NoError(t, json.NewEncoder(record).Encode(map[string]any{"pid": os.Getpid(), "child": capture.ChildPID, "input": capture.Input, "args": args}))
			require.NoError(t, record.Close())
			if strings.Contains(capture.Input, "model-error") {
				fmt.Println(`{"type":"result","subtype":"error_during_execution","is_error":true,"errors":["scripted model failure"],"session_id":"run-session"}`)
				continue
			}
			if strings.Contains(capture.Input, "hang-task") {
				continue
			}
		}

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		client := mcp.NewClient(&mcp.Implementation{Name: "fake-claude", Version: "1"}, nil)
		session, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: endpoint.URL, HTTPClient: &http.Client{Transport: bearerTransport(endpoint.Headers["Authorization"])}}, nil)
		require.NoError(t, err)
		listed, err := session.ListTools(ctx, nil)
		require.NoError(t, err)
		names := map[string]bool{}
		for _, tool := range listed.Tools {
			names[tool.Name] = true
			require.False(t, strings.HasPrefix(tool.Name, "memory_"), tool.Name)
		}
		for _, name := range []string{"reply", "watch", "wake_at", "schedule_add", "slack_read_history"} {
			require.True(t, names[name], name)
		}
		_, targets, ok := strings.Cut(capture.Input, "reply → ")
		require.True(t, ok)
		target, _, _ := strings.Cut(targets, " | work → ")
		if !recovery {
			result, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "watch", Arguments: map[string]any{
				"target": target, "command": `NOCTILUCA_RUN_CHILD=1 exec "$NOCTILUCA_RUN_TEST_BINARY" -test.run=TestRunCLIHelper`, "cwd": os.Getenv("NOCTILUCA_RUN_TEST_DIR"),
			}})
			require.NoError(t, err)
			require.False(t, result.IsError, "%+v", result.Content)
			var watched struct{ PID int }
			require.NoError(t, json.Unmarshal([]byte(result.Content[0].(*mcp.TextContent).Text), &watched))
			capture.WatchPID = watched.PID
		}

		result, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "reply", Arguments: map[string]any{"target": target, "text": "**Hello** from the hub"}})
		require.NoError(t, err)
		require.False(t, result.IsError, "%+v", result.Content)
		require.NoError(t, session.Close())
		cancel()
		fmt.Println(`{"type":"result","subtype":"success","session_id":"run-session"}`)
		data, err := json.Marshal(capture)
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(filepath.Join(os.Getenv("NOCTILUCA_RUN_TEST_DIR"), "capture.json"), data, 0600))
	}
	os.Exit(0)
}

func TestRunAssemblyReplyAndShutdown(t *testing.T) {
	dir := t.TempDir()
	binary, err := os.Executable()
	require.NoError(t, err)
	t.Setenv("NOCTILUCA_RUN_TEST_BINARY", binary)
	t.Setenv("NOCTILUCA_RUN_TEST_DIR", dir)
	t.Setenv("TEST_APP_TOKEN", "xapp-fake")
	t.Setenv("TEST_BOT_TOKEN", "xoxb-fake")
	require.NoError(t, os.WriteFile(filepath.Join(dir, "claude"), []byte("#!/bin/sh\nexec \"$NOCTILUCA_RUN_TEST_BINARY\" -test.run=TestRunCLIHelper -- \"$@\"\n"), 0700))
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "memory", "owner"), 0700))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "memory", "owner", "persona.md"), []byte("Owner instructions"), 0600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "memory", "core.md"), []byte("Core instructions"+strings.Repeat("x", 8192)), 0600))
	posts := make(chan url.Values, 4)
	socketClosed := make(chan struct{})
	var socketClose sync.Once
	var groupReads atomic.Int32
	var app *application
	acks := make(chan string, 6)
	upgrader := websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return r.Header.Get("Origin") == "https://api.slack.com" }}
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/users.info":
			fmt.Fprint(w, `{"ok":true,"user":{"profile":{"display_name":"Owner"}}}`)
		case "/conversations.info":
			fmt.Fprint(w, `{"ok":true,"channel":{"name":"test"}}`)
		case "/conversations.replies":
			fmt.Fprint(w, `{"ok":true,"messages":[]}`)
		case "/auth.test":
			_, _ = io.WriteString(w, `{"ok":true,"user_id":"U_BOT","bot_id":"B_BOT"}`)
		case "/usergroups.users.list":
			groupReads.Add(1)
			_, _ = io.WriteString(w, `{"ok":true,"users":["U_OWNER","U_MEMBER"]}`)
		case "/apps.connections.open":
			require.Equal(t, "Bearer xapp-fake", r.Header.Get("Authorization"))
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "url": "ws://" + r.Host + "/ws"})
		case "/ws":
			conn, err := upgrader.Upgrade(w, r, nil)
			if err != nil {
				t.Error(err)
				return
			}
			defer conn.Close()
			defer socketClose.Do(func() { close(socketClosed) })
			require.NoError(t, conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"hello"}`)))
			for i, user := range []string{"U_DENIED", "U_OWNER", "U_OWNER"} {
				envelope := fmt.Sprintf(`{"type":"events_api","envelope_id":"event-%d","payload":{"type":"event_callback","event_id":"source-%d","event":{"type":"message","channel":"D_DIRECT","channel_type":"im","user":%q,"text":"hello","ts":"1.2"}}}`, i, min(i, 1), user)
				require.NoError(t, conn.WriteMessage(websocket.TextMessage, []byte(envelope)))
			}
			require.NoError(t, conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"events_api","envelope_id":"reaction","payload":{"type":"event_callback","event_id":"reaction-source","event":{"type":"reaction_added","user":"U_MEMBER","item_user":"U_BOT","reaction":"thumbsdown","item":{"type":"message","channel":"C_UNLISTED","ts":"1.2"}}}}`)))
			require.NoError(t, conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"events_api","envelope_id":"unjoined","payload":{"type":"event_callback","event_id":"unjoined-source","event":{"type":"message","channel":"C_CHANNEL","user":"U_MEMBER","text":"unjoined reply","ts":"8.1","thread_ts":"8.0"}}}`)))
			require.NoError(t, conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"events_api","envelope_id":"other-reaction","payload":{"type":"event_callback","event_id":"other-reaction-source","event":{"type":"reaction_added","user":"U_MEMBER","item_user":"U_OTHER","reaction":"eyes","item":{"type":"message","channel":"C_CHANNEL","ts":"8.2"}}}}`)))
			for {
				_, raw, err := conn.ReadMessage()
				if err != nil {
					return
				}
				var ack struct {
					EnvelopeID string `json:"envelope_id"`
				}
				require.NoError(t, json.Unmarshal(raw, &ack))
				if ack.EnvelopeID == "event-0" {
					require.Eventually(t, func() bool {
						var n int
						return app.db.QueryRow("SELECT count(*) FROM inbox WHERE source_key LIKE 'schedule:heartbeat:%'").Scan(&n) == nil && n > 0
					}, 2*time.Second, time.Millisecond)
				}
				var events, inbox int
				require.NoError(t, app.db.QueryRow("SELECT (SELECT count(*) FROM events), (SELECT count(*) FROM inbox WHERE json_extract(payload, '$.Event') IS NOT NULL)").Scan(&events, &inbox))
				require.Positive(t, events)
				require.Equal(t, events, inbox)
				acks <- ack.EnvelopeID
			}
		case "/chat.postMessage":
			require.NoError(t, r.ParseForm())
			require.Equal(t, "xoxb-fake", r.Form.Get("token"))
			posts <- r.Form
			_, _ = io.WriteString(w, `{"ok":true,"channel":"D_DIRECT","ts":"2.3"}`)
		default:
			t.Errorf("unexpected network request: %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer api.Close()
	configPath := filepath.Join(dir, "config.yaml")
	require.NoError(t, os.WriteFile(configPath, []byte(fmt.Sprintf(`messaging:
  - name: slack
    type: slack
    owner: U_OWNER
    slack:
      app_token: {env: TEST_APP_TOKEN}
      bot_token: {env: TEST_BOT_TOKEN}
      group_refresh_interval: 10ms
    conversations:
      rules:
        - {id: C_CHANNEL, groups: [S_GROUP], require_mention: true}
model: {executable: ./claude, name: haiku}
heartbeat: {enabled: true, interval: 10ms}
cron:
  - {name: daily, cron: '0 9 * * *', tz: UTC, prompt: Check tasks}
mcp_servers:
  - {name: unconfigured, url: '%s/mcp'}
`, api.URL)), 0600))
	c, err := loadConfig(configPath)
	require.NoError(t, err)
	var logs bytes.Buffer
	app, err = newApplication(t.Context(), c, slog.New(slog.NewTextHandler(&logs, nil)), sdk.OptionAPIURL(api.URL+"/"), sdk.OptionHTTPClient(api.Client()))
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- app.Start(ctx) }()
	t.Cleanup(func() {
		cancel()
		stop, finish := context.WithTimeout(context.Background(), 5*time.Second)
		defer finish()
		require.NoError(t, app.Shutdown(stop))
	})
	var posted url.Values
	select {
	case posted = <-posts:
	case err := <-done:
		t.Fatalf("run stopped: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("no reply")
	}
	require.Equal(t, "D_DIRECT", posted.Get("channel"))
	require.Equal(t, "*Hello* from the hub", posted.Get("text"))
	for range 6 {
		select {
		case <-acks:
		case <-time.After(time.Second):
			t.Fatal("missing durable acknowledgement")
		}
	}
	var capture runCapture
	require.Eventually(t, func() bool {
		data, err := os.ReadFile(filepath.Join(dir, "capture.json"))
		return err == nil && json.Unmarshal(data, &capture) == nil
	}, time.Second, time.Millisecond)
	require.NotContains(t, capture.Input, "Owner instructions")
	require.Contains(t, capture.Prompt, "Owner instructions")
	require.NotContains(t, capture.Input, "Core instructions")
	require.Contains(t, capture.Prompt, "Core instructions")
	require.NotContains(t, capture.Input, "slack_read")
	require.Contains(t, capture.Prompt, "slack_read")
	require.Equal(t, c.Model.Workdir, capture.Workdir)
	require.Eventually(t, func() bool { return groupReads.Load() >= 2 }, time.Second, time.Millisecond)
	cancel()
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("run did not stop")
	}
	select {
	case <-socketClosed:
	case <-time.After(time.Second):
		t.Fatal("socket still open")
	}
	require.Error(t, app.db.Ping())
	require.NoError(t, app.Shutdown(t.Context()))
	require.Contains(t, logs.String(), "Skipping MCP server without login")
	require.NotContains(t, logs.String(), "xoxb-fake")
	require.NoFileExists(t, capture.MCPFile)
	endpoint, err := url.Parse(capture.URL)
	require.NoError(t, err)
	conn, err := net.DialTimeout("tcp", endpoint.Host, 100*time.Millisecond)
	if conn != nil {
		conn.Close()
	}
	require.Error(t, err)
	for _, pid := range []int{capture.PID, capture.ChildPID, capture.WatchPID} {
		require.Positive(t, pid)
		require.Eventually(t, func() bool {
			data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
			return os.IsNotExist(err) || (err == nil && strings.Contains(string(data), ") Z "))
		}, time.Second, time.Millisecond, "process %d remains alive", pid)
	}
	db, err := repository.New(t.Context(), c.Database)
	require.NoError(t, err)
	defer db.Close()
	history := repository.NewEventRepository(db)
	var droppedEvents, droppedInbox int
	require.NoError(t, db.QueryRow("SELECT count(*) FROM events WHERE message_ref IN ('slack:C_CHANNEL:8.1', 'slack:C_CHANNEL:8.2')").Scan(&droppedEvents))
	require.NoError(t, db.QueryRow("SELECT count(*) FROM inbox WHERE source_key IN ('slack:unjoined-source', 'slack:other-reaction-source')").Scan(&droppedInbox))
	require.Zero(t, droppedEvents)
	require.Zero(t, droppedInbox)
	inbound, err := history.ListInbound(t.Context(), slack.Conversation("slack", "D_DIRECT", ""), 10)
	require.NoError(t, err)
	require.Len(t, inbound, 2)
	reactions, err := history.ListInbound(t.Context(), slack.Conversation("slack", "C_UNLISTED", ""), 10)
	require.NoError(t, err)
	require.Len(t, reactions, 1)
	require.Equal(t, domain.InboundEventReaction, reactions[0].Kind)
	require.Empty(t, reactions[0].Parent)
	require.NotNil(t, reactions[0].Accepted)
	require.False(t, *reactions[0].Accepted)
	episodes, err := consolidate.EpisodesSince(t.Context(), repository.NewEpisodeRepository(db), time.Time{}, time.Now(), []string{"thumbsdown"})
	require.NoError(t, err)
	var reactionEpisode *consolidate.Episode
	for i := range episodes.Items {
		if episodes.Items[i].SessionKey == "slack:C_UNLISTED:" {
			reactionEpisode = &episodes.Items[i]
			break
		}
	}
	require.NotNil(t, reactionEpisode)
	require.Contains(t, reactionEpisode.Input, ":thumbsdown: on message 1.2")
	require.Contains(t, reactionEpisode.Signals, "negative_reaction")
	require.True(t, reactionEpisode.ContextOnly)
	_, err = repository.NewSessionRepository(db).Get(t.Context(), "slack:C_UNLISTED:")
	require.ErrorIs(t, err, domain.ErrNotFound)
	outbound, err := history.ListOutbound(t.Context(), slack.Conversation("slack", "D_DIRECT", ""), 10)
	require.NoError(t, err)
	require.Len(t, outbound, 1)
	require.Equal(t, "**Hello** from the hub", outbound[0].Message.Text)
	session, err := repository.NewSessionRepository(db).Get(t.Context(), "slack:D_DIRECT:")
	require.NoError(t, err)
	require.Equal(t, "run-session", session.ResumeToken)
	schedules, err := repository.NewScheduleRepository(db).List(t.Context())
	require.NoError(t, err)
	require.Len(t, schedules, 2)
	for _, schedule := range schedules {
		require.Equal(t, c.Model.Workdir, schedule.Workdir)
	}
	require.DirExists(t, filepath.Join(c.Memory.Dir, ".git"))
	watches := repository.NewWatchRepository(db)
	unfinished, err := watches.Unfinished(t.Context())
	require.NoError(t, err)
	require.Len(t, unfinished, 1)
	restarted := make(chan struct{}, 1)
	watcher := watch.New(watches, repository.NewInboxRepository(db), nil, func() { restarted <- struct{}{} })
	watchCtx, stopWatch := context.WithCancel(t.Context())
	watchDone := make(chan error, 1)
	go func() { watchDone <- watcher.Start(watchCtx) }()
	select {
	case <-restarted:
	case <-time.After(time.Second):
		t.Fatal("watch recovery did not finish")
	}
	stopWatch()
	require.NoError(t, <-watchDone)
	unfinished, err = watches.Unfinished(t.Context())
	require.NoError(t, err)
	require.Empty(t, unfinished)
	var interruptions int
	require.NoError(t, db.QueryRowContext(t.Context(), `SELECT count(*) FROM inbox WHERE source_key LIKE 'watch:%' AND payload LIKE '%interrupted by restart; exit unknown%'`).Scan(&interruptions))
	require.Equal(t, 1, interruptions)

}

func TestRunStartupFailureClosesResources(t *testing.T) {
	t.Setenv("TEST_APP_TOKEN", "xapp-fake")
	t.Setenv("TEST_BOT_TOKEN", "xoxb-fake")
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"ok":false,"error":"invalid_auth"}`)
	}))
	defer api.Close()
	c, err := loadConfig(writeConfig(t, "messaging:\n  - name: slack\n    type: slack\n    owner: U_OWNER\n    slack:\n      app_token: {env: TEST_APP_TOKEN}\n      bot_token: {env: TEST_BOT_TOKEN}\n"))
	require.NoError(t, err)
	app, err := newApplication(t.Context(), c, nil, sdk.OptionAPIURL(api.URL+"/"))
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	require.NoError(t, app.Start(ctx))
	require.Error(t, app.db.Ping())
	require.NoError(t, app.Shutdown(t.Context()))
}

type blockingStartupTransport struct{ entered chan struct{} }

func (b blockingStartupTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	close(b.entered)
	<-r.Context().Done()
	return nil, r.Context().Err()
}

func TestRunCancellationDuringStartup(t *testing.T) {
	for _, groups := range []bool{false, true} {
		t.Run(fmt.Sprint(groups), func(t *testing.T) {
			t.Setenv("TEST_APP_TOKEN", "xapp-fake")
			t.Setenv("TEST_BOT_TOKEN", "xoxb-fake")
			yaml := "messaging:\n  - name: slack\n    type: slack\n    owner: U_OWNER\n    slack:\n      app_token: {env: TEST_APP_TOKEN}\n      bot_token: {env: TEST_BOT_TOKEN}\n"
			if groups {
				yaml += "    conversations:\n      rules:\n        - {id: C_CHANNEL, groups: [S_GROUP]}\n"
			}
			c, err := loadConfig(writeConfig(t, yaml))
			require.NoError(t, err)
			entered := make(chan struct{})
			app, err := newApplication(t.Context(), c, nil, sdk.OptionHTTPClient(&http.Client{Transport: blockingStartupTransport{entered}}))
			require.NoError(t, err)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			done := make(chan error, 1)
			go func() { done <- app.Start(ctx) }()
			select {
			case <-entered:
			case <-time.After(time.Second):
				t.Fatal("startup did not reach HTTP request")
			}
			cancel()
			select {
			case err := <-done:
				require.NoError(t, err)
			case <-time.After(time.Second):
				t.Fatal("startup cancellation blocked")
			}
			require.Error(t, app.db.Ping())
		})
	}
}

func TestRunRecoversInboxAndSurvivesModelFailures(t *testing.T) {
	const recoveryToken = "22222222-2222-4222-8222-222222222222"
	dir := t.TempDir()
	binary, err := os.Executable()
	require.NoError(t, err)
	t.Setenv("NOCTILUCA_RUN_TEST_BINARY", binary)
	t.Setenv("NOCTILUCA_RUN_TEST_DIR", dir)
	t.Setenv("NOCTILUCA_RUN_RECOVERY", "1")
	// The race runtime's default one-second exit delay would consume the fake CLI's turn deadline.
	t.Setenv("GORACE", os.Getenv("GORACE")+" atexit_sleep_ms=0")
	t.Setenv("TEST_APP_TOKEN", "xapp-fake")
	t.Setenv("TEST_BOT_TOKEN", "xoxb-fake")
	require.NoError(t, os.WriteFile(filepath.Join(dir, "claude"), []byte("#!/bin/sh\nexec \"$NOCTILUCA_RUN_TEST_BINARY\" -test.run=TestRunCLIHelper -- \"$@\"\n"), 0700))
	slackAPI := newRunSlack(t, nil)
	api := slackAPI.Server
	path := filepath.Join(dir, "config.yaml")
	// The hung task's model must start on its input within the task timeout before
	// its turn can hit the timeout, and a loaded runner can take seconds to start the fake CLI.
	require.NoError(t, os.WriteFile(path, []byte("messaging:\n  - name: slack\n    type: slack\n    owner: U_OWNER\n    slack:\n      app_token: {env: TEST_APP_TOKEN}\n      bot_token: {env: TEST_BOT_TOKEN}\nmodel: {executable: ./claude}\nheartbeat: {enabled: false}\ntasks: {timeout: 3s}\n"), 0600))
	config, err := loadConfig(path)
	require.NoError(t, err)
	app, err := newApplication(t.Context(), config, nil, sdk.OptionAPIURL(api.URL+"/"))
	require.NoError(t, err)
	inbox := repository.NewInboxRepository(app.db)
	makeItem := func(name, text string) domain.InboxItem {
		ref := slack.Conversation("slack", "D_"+name, "")
		return domain.InboxItem{SourceKey: name, SessionKey: domain.SessionKey(ref), Lane: "conv", ReadyAt: time.Now(), Payload: domain.InboxPayload{Input: domain.UserInput{Text: text}, Context: domain.SessionContext{Conversation: ref, Direct: true, Workdir: dir}}}
	}
	pending := makeItem("PENDING", "pending-success")
	recovered := makeItem("RECOVERED", "recover-success")
	failed := makeItem("FAILED", "model-error")
	hung := reconcile.TaskItem("hung", "hang-task", dir)
	next := reconcile.TaskItem("next", "next-task", dir)
	for _, item := range []domain.InboxItem{pending, recovered, failed, hung, next} {
		id, err := inbox.Enqueue(t.Context(), item)
		require.NoError(t, err)
		if item.SourceKey == recovered.SourceKey {
			_, err = inbox.Claim(t.Context(), id, time.Now())
			require.NoError(t, err)
			require.NoError(t, repository.NewSessionRepository(app.db).Save(t.Context(), domain.Session{Key: item.SessionKey, ResumeToken: recoveryToken}))
		}
	}
	_, err = repository.NewWatchRepository(app.db).Add(t.Context(), domain.Watch{Command: "must never restart", CWD: dir, Item: makeItem("WATCH", "unused")})
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- app.Start(ctx) }()
	t.Cleanup(func() {
		cancel()
		stop, finish := context.WithTimeout(context.Background(), 5*time.Second)
		defer finish()
		require.NoError(t, app.Shutdown(stop))
	})
	require.Eventually(t, func() bool {
		var finished int
		err := app.db.QueryRow(`SELECT count(*) FROM inbox WHERE status='done' AND (source_key IN (?,?,?) OR source_key LIKE 'watch:%')`, pending.SourceKey, recovered.SourceKey, next.SourceKey).Scan(&finished)
		return err == nil && finished == 4
	}, 15*time.Second, 5*time.Millisecond)
	for _, key := range []string{failed.SourceKey, hung.SourceKey} {
		var attempts int
		require.NoError(t, app.db.QueryRow("SELECT attempts FROM inbox WHERE source_key=?", key).Scan(&attempts))
		require.Zero(t, attempts)
	}
	select {
	case err := <-done:
		t.Fatalf("session failure stopped run: %v", err)
	default:
	}
	cancel()
	require.NoError(t, <-done)
	data, err := os.ReadFile(filepath.Join(dir, "recovery.jsonl"))
	require.NoError(t, err)
	require.Contains(t, string(data), "recover-success")
	require.Contains(t, string(data), recoveryToken)
	require.Contains(t, string(data), "interrupted by restart; exit unknown")
	for _, line := range bytes.Split(bytes.TrimSpace(data), []byte("\n")) {
		var record struct{ PID, Child int }
		require.NoError(t, json.Unmarshal(line, &record))
		for _, pid := range []int{record.PID, record.Child} {
			require.Eventually(t, func() bool {
				data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
				return os.IsNotExist(err) || strings.Contains(string(data), ") Z ")
			}, time.Second, time.Millisecond)
		}
	}
}
