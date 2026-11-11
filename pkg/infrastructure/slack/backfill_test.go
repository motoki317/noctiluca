package slack

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"path/filepath"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	sdk "github.com/slack-go/slack"
	"github.com/slack-go/slack/slackevents"
	"github.com/slack-go/slack/socketmode"
	"github.com/stretchr/testify/require"

	"github.com/motoki317/noctiluca/pkg/domain"
	"github.com/motoki317/noctiluca/pkg/infrastructure/repository"
)

func historyMessage(at time.Time, text string) sdk.Message {
	return sdk.Message{Msg: sdk.Msg{User: "UOWNER", Timestamp: formatTimestamp(at), Text: text}}
}

func backfillPage(t *testing.T, w http.ResponseWriter, messages []sdk.Message, more bool, cursor string) {
	t.Helper()
	require.NoError(t, json.NewEncoder(w).Encode(map[string]any{
		"ok": true, "messages": messages, "has_more": more, "response_metadata": map[string]string{"next_cursor": cursor},
	}))
}

func saveBackfillMessage(t *testing.T, a *adapter, ref domain.ConversationRef, at time.Time, accepted *bool) {
	t.Helper()
	require.NoError(t, a.history.SaveInbound(t.Context(), domain.InboundEvent{
		Kind: domain.InboundEventMessage, Conversation: ref, ReceivedAt: at, Accepted: accepted,
		Sender: domain.Sender{ID: "UOWNER", Kind: domain.SenderHuman},
	}))
}

func TestBackfillTargets(t *testing.T) {
	var opens int
	a := apiAdapter(t, func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/conversations.open", r.URL.Path)
		require.NoError(t, r.ParseForm())
		require.Equal(t, "UOWNER", r.Form.Get("users"))
		require.Equal(t, "xoxb-test", r.Form.Get("token"))
		opens++
		fmt.Fprint(w, `{"ok":true,"channel":{"id":"DOWNER"}}`)
	}, true)
	a.owner = "UOWNER"
	now := time.Now().Truncate(time.Microsecond)
	accepted, rejected := true, false
	for _, entry := range []struct {
		ref      domain.ConversationRef
		at       time.Time
		accepted *bool
	}{
		{"slack:DOWNER:", now.Add(-time.Hour), &accepted},
		{"slack:COLD:", now.Add(-25 * time.Hour), &accepted},
		{"slack:COLD:", now.Add(-26 * time.Hour), &accepted},
		{"slack:DPEER:", now.Add(-2 * time.Hour), &accepted},
		{"slack:COLD:1.000001", now.Add(-3 * time.Hour), &accepted},
		{"slack:COLD:old", now.Add(-25 * time.Hour), &accepted},
		{"slack:CREJECTED:", now, &rejected},
		{"slack:CUNKNOWN:", now, nil},
		{"other:COTHER:", now, &accepted},
	} {
		saveBackfillMessage(t, a, entry.ref, entry.at, entry.accepted)
	}
	require.NoError(t, a.history.SaveOutbound(t.Context(), domain.SentMessage{Conversation: "slack:CSENT:2.000001", SentAt: now}))
	require.NoError(t, a.history.SaveOutbound(t.Context(), domain.SentMessage{Conversation: "slack:COLD:1.000001", SentAt: now.Add(-4 * time.Hour)}))
	before := time.Now().Add(-24 * time.Hour)
	targets := a.backfillTargets(t.Context())
	after := time.Now().Add(-24 * time.Hour)
	require.Equal(t, 1, opens)
	require.Len(t, targets, 5)
	require.Equal(t, "DOWNER", targets[0].channel)
	want := map[domain.ConversationRef]time.Time{
		"slack:DOWNER:": now.Add(-time.Hour), "slack:COLD:": {}, "slack:DPEER:": now.Add(-2 * time.Hour),
		"slack:COLD:1.000001": now.Add(-3 * time.Hour), "slack:CSENT:2.000001": now,
	}
	for _, target := range targets {
		ref := Conversation(a.name, target.channel, target.thread)
		at, ok := want[ref]
		require.True(t, ok, ref)
		if at.IsZero() {
			require.False(t, target.since.Before(before))
			require.False(t, target.since.After(after))
		} else {
			require.True(t, at.Equal(target.since))
		}
		require.Equal(t, targets[0].until, target.until)
	}
}

type backfillLookupFailure struct {
	domain.EventRepository
	operation string
	err       error
}

func (h *backfillLookupFailure) ListConversations(ctx context.Context, name string) ([]domain.Conversation, error) {
	if h.operation == "conversations" {
		return nil, h.err
	}
	return h.EventRepository.ListConversations(ctx, name)
}

func (h *backfillLookupFailure) ListThreads(ctx context.Context, name string, since time.Time) ([]domain.ConversationRef, error) {
	if h.operation == "threads" {
		return nil, h.err
	}
	return h.EventRepository.ListThreads(ctx, name, since)
}

func (h *backfillLookupFailure) LatestMessageTime(ctx context.Context, ref domain.ConversationRef) (time.Time, error) {
	if h.operation == "inbound cursor" {
		return time.Time{}, h.err
	}
	return h.EventRepository.LatestMessageTime(ctx, ref)
}

func (h *backfillLookupFailure) ListOutbound(ctx context.Context, ref domain.ConversationRef, limit int) ([]domain.SentMessage, error) {
	if h.operation == "outbound cursor" {
		return nil, h.err
	}
	return h.EventRepository.ListOutbound(ctx, ref, limit)
}

func TestBackfillLookupWarnings(t *testing.T) {
	for _, tc := range []struct{ operation, warning, errorText string }{
		{"conversations", "List Slack backfill conversations failed", "lookup failed"},
		{"owner DM", "Resolve Slack backfill owner DM failed", "open owner direct message failed"},
		{"threads", "List Slack backfill threads failed", "lookup failed"},
		{"inbound cursor", "Read Slack backfill cursor failed", "lookup failed"},
		{"outbound cursor", "Read Slack backfill cursor failed", "lookup failed"},
	} {
		t.Run(tc.operation, func(t *testing.T) {
			var logs bytes.Buffer
			logger := slog.Default()
			slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, nil)))
			t.Cleanup(func() { slog.SetDefault(logger) })
			a := apiAdapter(t, func(w http.ResponseWriter, r *http.Request) {
				require.Equal(t, "/conversations.open", r.URL.Path)
				fmt.Fprint(w, `{"ok":false,"error":"invalid_auth"}`)
			}, false)
			if tc.operation == "owner DM" {
				a.owner = "UOWNER"
			}
			if tc.operation == "inbound cursor" || tc.operation == "outbound cursor" {
				require.NoError(t, a.history.SaveOutbound(t.Context(), domain.SentMessage{Conversation: "slack:CTEST:1.000001", SentAt: time.Now()}))
			}
			a.history = &backfillLookupFailure{EventRepository: a.history, operation: tc.operation, err: errors.New("lookup failed")}
			require.Empty(t, a.backfillTargets(t.Context()))
			var entry map[string]any
			require.NoError(t, json.Unmarshal(logs.Bytes(), &entry))
			require.Equal(t, tc.warning, entry["msg"])
			require.Equal(t, tc.errorText, entry["error"])
		})
	}
}

func TestBackfillPaginationAndCap(t *testing.T) {
	for _, thread := range []string{"", "1.000001"} {
		t.Run(thread, func(t *testing.T) {
			at := time.Now().Add(-time.Hour).Truncate(time.Second)
			var source []sdk.Message
			for i := 1; i <= 240; i++ {
				message := historyMessage(at.Add(time.Duration(i)*time.Second), fmt.Sprint(i))
				message.ThreadTimestamp = thread
				source = append(source, message)
			}
			if thread == "" {
				slices.Reverse(source)
			}
			var calls, offset int
			var previous string
			a := apiAdapter(t, func(w http.ResponseWriter, r *http.Request) {
				require.NoError(t, r.ParseForm())
				method := "/conversations.history"
				if thread != "" {
					method = "/conversations.replies"
					require.Equal(t, thread, r.Form.Get("ts"))
				}
				require.Equal(t, method, r.URL.Path)
				require.Equal(t, "0", r.Form.Get("inclusive"))
				require.Equal(t, "CTEST", r.Form.Get("channel"))
				require.Equal(t, previous, r.Form.Get("cursor"))
				end := min(offset+17, len(source))
				page := source[offset:end]
				more := end < len(source)
				cursor := ""
				if more {
					cursor = fmt.Sprint(end)
					previous = cursor
				}
				offset = end
				calls++
				backfillPage(t, w, page, more, cursor)
			}, false)
			var got []domain.InboundEvent
			a.selfUser = "USELF"
			a.SetConversationFilter(func(domain.ConversationRef) bool { return false })
			require.NoError(t, a.backfill(t.Context(), []backfillTarget{{channel: "CTEST", thread: thread, since: at, until: at.Add(time.Hour)}}, func(_ context.Context, event domain.InboundEvent) error {
				got = append(got, event)
				return nil
			}))
			require.Len(t, got, 200)
			for i, event := range got {
				require.Equal(t, fmt.Sprint(i+41), event.Text)
				require.Equal(t, "slack:m:CTEST:"+formatTimestamp(at.Add(time.Duration(i+41)*time.Second)), event.SourceKey)
			}
			if thread == "" {
				require.Equal(t, 12, calls)
			} else {
				require.Equal(t, 15, calls)
			}
		})
	}
}

func TestBackfillWindow(t *testing.T) {
	for _, thread := range []string{"", "1.000001"} {
		t.Run(thread, func(t *testing.T) {
			since := time.Now().Add(-time.Hour).Truncate(time.Microsecond)
			until := since.Add(time.Hour)
			var calls int
			a := apiAdapter(t, func(w http.ResponseWriter, r *http.Request) {
				require.NoError(t, r.ParseForm())
				require.Equal(t, formatTimestamp(since), r.Form.Get("oldest"))
				require.Equal(t, formatTimestamp(until), r.Form.Get("latest"))
				require.Equal(t, "0", r.Form.Get("inclusive"))
				calls++
				messages := []sdk.Message{
					historyMessage(since.Add(-time.Microsecond), "too old"), historyMessage(since, "last stored"),
					historyMessage(since.Add(time.Microsecond), "new"), historyMessage(until.Add(time.Microsecond), "future"),
					{Msg: sdk.Msg{User: "UOWNER", Text: "missing timestamp"}},
				}
				if thread != "" {
					messages = append(messages, sdk.Message{Msg: sdk.Msg{User: "UOWNER", Timestamp: thread, Text: "root"}})
				}
				backfillPage(t, w, messages, false, "")
			}, false)
			var got []string
			require.NoError(t, a.backfill(t.Context(), []backfillTarget{{channel: "DTEST", thread: thread, since: since, until: until}}, func(_ context.Context, event domain.InboundEvent) error {
				got = append(got, event.Text)
				return nil
			}))
			require.Equal(t, []string{"new"}, got)
			require.Equal(t, 1, calls)
		})
	}
}

func TestBackfillThreadFilters(t *testing.T) {
	for _, tc := range []struct {
		name, channel, text        string
		joined, outboundOnly, want bool
	}{
		{"joined", "CTEST", "reply", true, false, true},
		{"outbound only", "CTEST", "reply", true, true, true},
		{"unjoined", "CTEST", "reply", false, false, false},
		{"mentioned", "CTEST", "hi <@USELF>", false, false, true},
		{"DM", "DTEST", "reply", false, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			at := time.Now().Add(-time.Hour).Truncate(time.Microsecond)
			root := formatTimestamp(at.Add(-time.Hour))
			message := historyMessage(at.Add(time.Minute), tc.text)
			message.ThreadTimestamp = root
			var reads, lookups int
			a := apiAdapter(t, func(w http.ResponseWriter, r *http.Request) {
				require.Equal(t, "/conversations.replies", r.URL.Path)
				require.NoError(t, r.ParseForm())
				require.Equal(t, root, r.Form.Get("ts"))
				if r.Form.Get("oldest") == "" {
					lookups++
					user := "UOTHER"
					if tc.joined {
						user = "USELF"
					}
					backfillPage(t, w, []sdk.Message{{Msg: sdk.Msg{User: user, Timestamp: root}}}, false, "")
				} else {
					reads++
					backfillPage(t, w, []sdk.Message{{Msg: sdk.Msg{User: "UOTHER", Timestamp: root, ThreadTimestamp: root, Text: "root"}}, message}, false, "")
				}
			}, false)
			a.selfUser = "USELF"
			ref := Conversation(a.name, tc.channel, root)
			require.NoError(t, a.history.SaveOutbound(t.Context(), domain.SentMessage{Conversation: ref, SentAt: at}))
			if !tc.outboundOnly {
				accepted := true
				saveBackfillMessage(t, a, ref, at, &accepted)
			}
			var got []domain.InboundEvent
			require.NoError(t, a.backfill(t.Context(), a.backfillTargets(t.Context()), func(_ context.Context, event domain.InboundEvent) error {
				got = append(got, event)
				return nil
			}))
			require.Equal(t, 1, reads)
			if tc.want {
				require.Len(t, got, 1)
				require.Equal(t, ref, got[0].Conversation)
				require.Equal(t, Conversation(a.name, tc.channel, message.Timestamp), got[0].MessageRef)
				require.Equal(t, "slack:m:"+tc.channel+":"+message.Timestamp, got[0].SourceKey)
				require.Equal(t, tc.channel == "DTEST", got[0].Direct)
			} else {
				require.Empty(t, got)
			}
			if tc.channel == "DTEST" || tc.text == "hi <@USELF>" {
				require.Zero(t, lookups)
			} else {
				require.Equal(t, 1, lookups)
			}
		})
	}
}

func TestBackfillDoesNotReplayRepliesBeforeLastPost(t *testing.T) {
	at := time.Now().Add(-time.Hour).Truncate(time.Microsecond)
	root := formatTimestamp(at)
	postedAt := at.Add(10 * time.Minute)
	early := historyMessage(at.Add(time.Minute), "reply before the agent joined")
	early.ThreadTimestamp = root
	later := historyMessage(postedAt.Add(time.Minute), "reply after the result")
	later.ThreadTimestamp = root
	var posted bool
	var oldest string
	a := apiAdapter(t, func(w http.ResponseWriter, r *http.Request) {
		require.NoError(t, r.ParseForm())
		switch r.URL.Path {
		case "/chat.postMessage":
			posted = true
			fmt.Fprintf(w, `{"ok":true,"channel":"CTEST","ts":%q}`, formatTimestamp(postedAt))
		case "/conversations.history":
			backfillPage(t, w, nil, false, "")
		case "/conversations.replies":
			if r.Form.Get("oldest") != "" {
				oldest = r.Form.Get("oldest")
				backfillPage(t, w, []sdk.Message{early, later}, false, "")
			} else if posted {
				backfillPage(t, w, []sdk.Message{{Msg: sdk.Msg{User: "USELF", Timestamp: formatTimestamp(postedAt), ThreadTimestamp: root}}}, false, "")
			} else {
				backfillPage(t, w, nil, false, "")
			}
		default:
			t.Errorf("unexpected API %s", r.URL.Path)
		}
	}, false)
	a.selfUser = "USELF"
	accepted := true
	saveBackfillMessage(t, a, Conversation(a.name, "CTEST", ""), at, &accepted)
	_, ok := a.inbound(t.Context(), &slackevents.MessageEvent{Channel: "CTEST", Message: &early.Msg})
	require.False(t, ok)
	ref := Conversation(a.name, "CTEST", root)
	stored, err := a.history.ListInbound(t.Context(), ref, 1)
	require.NoError(t, err)
	require.Empty(t, stored)
	id, err := a.Send(t.Context(), ref, domain.OutboundMessage{Text: "result"})
	require.NoError(t, err)
	require.NoError(t, a.history.SaveOutbound(t.Context(), domain.SentMessage{ID: id, Conversation: ref, SentAt: postedAt}))
	var got []string
	require.NoError(t, a.backfill(t.Context(), a.backfillTargets(t.Context()), func(_ context.Context, event domain.InboundEvent) error {
		got = append(got, event.Text)
		return nil
	}))
	require.Equal(t, []string{later.Text}, got)
	require.Equal(t, formatTimestamp(postedAt), oldest)
}

func TestBackfillUsesLiveMessageFilters(t *testing.T) {
	at := time.Now().Add(-time.Hour).Truncate(time.Microsecond)
	var messages []sdk.Message
	for _, subtype := range []string{"", "bot_message", "file_share", "me_message", "thread_broadcast", "channel_join", "message_changed", "message_deleted"} {
		message := historyMessage(at.Add(time.Duration(len(messages)+1)*time.Second), subtype)
		message.SubType = subtype
		message.Files = []sdk.File{{ID: "FTEST", Name: "photo.png", Mimetype: "image/png"}}
		if subtype == "thread_broadcast" {
			message.ThreadTimestamp = formatTimestamp(at)
		}
		messages = append(messages, message)
	}
	for _, self := range []sdk.Msg{{User: "USELF"}, {BotID: "BSELF"}} {
		self.Timestamp = formatTimestamp(at.Add(time.Duration(len(messages)+1) * time.Second))
		messages = append(messages, sdk.Message{Msg: self})
	}
	a := apiAdapter(t, func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/conversations.history", r.URL.Path)
		backfillPage(t, w, messages, false, "")
	}, false)
	a.selfUser, a.selfBot = "USELF", "BSELF"
	var got []domain.InboundEvent
	require.NoError(t, a.backfill(t.Context(), []backfillTarget{{channel: "DTEST", since: at, until: time.Now()}}, func(_ context.Context, event domain.InboundEvent) error {
		got = append(got, event)
		return nil
	}))
	require.Len(t, got, 5)
	for i, event := range got {
		live, ok := a.inbound(t.Context(), &slackevents.MessageEvent{Channel: "DTEST", SubType: messages[i].SubType, Message: &messages[i].Msg})
		require.True(t, ok)
		require.Equal(t, live, event)
		require.Equal(t, "FTEST", event.Attachments[0].Ref)
	}
}

func TestBackfillRateLimit(t *testing.T) {
	for _, thread := range []string{"", "1.000001"} {
		t.Run(thread, func(t *testing.T) {
			var calls int
			at := time.Now().Add(-time.Hour)
			a := apiAdapter(t, func(w http.ResponseWriter, r *http.Request) {
				calls++
				if calls == 1 {
					w.Header().Set("Retry-After", "1")
					w.WriteHeader(http.StatusTooManyRequests)
					return
				}
				backfillPage(t, w, []sdk.Message{historyMessage(at.Add(time.Minute), "retried")}, false, "")
			}, false)
			start := time.Now()
			messages, err := a.readBackfill(t.Context(), backfillTarget{channel: "DTEST", thread: thread, since: at, until: time.Now()})
			require.NoError(t, err)
			require.GreaterOrEqual(t, time.Since(start), time.Second)
			require.Len(t, messages, 1)
			require.Equal(t, 2, calls)
		})
	}
	t.Run("context bounds retry", func(t *testing.T) {
		a := apiAdapter(t, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Retry-After", "60")
			w.WriteHeader(http.StatusTooManyRequests)
		}, false)
		ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
		defer cancel()
		start := time.Now()
		_, err := a.readBackfill(ctx, backfillTarget{channel: "DTEST", since: time.Now().Add(-time.Hour), until: time.Now()})
		require.ErrorIs(t, err, context.DeadlineExceeded)
		require.Less(t, time.Since(start), time.Second)
	})
}

func TestBackfillReaderFallback(t *testing.T) {
	for _, thread := range []string{"", "1.000001"} {
		t.Run(thread, func(t *testing.T) {
			var botCalls, readerCalls int
			at := time.Now().Add(-time.Hour)
			a := apiAdapter(t, func(w http.ResponseWriter, r *http.Request) {
				require.NoError(t, r.ParseForm())
				if r.Form.Get("token") == "xoxb-test" {
					botCalls++
					fmt.Fprint(w, `{"ok":false,"error":"not_in_channel"}`)
					return
				}
				require.Equal(t, "xoxp-test", r.Form.Get("token"))
				readerCalls++
				if r.Form.Get("cursor") == "" {
					backfillPage(t, w, []sdk.Message{historyMessage(at.Add(time.Minute), "first")}, true, "next")
				} else {
					backfillPage(t, w, []sdk.Message{historyMessage(at.Add(2*time.Minute), "second")}, false, "")
				}
			}, true)
			messages, err := a.readBackfill(t.Context(), backfillTarget{channel: "CTEST", thread: thread, since: at, until: time.Now()})
			require.NoError(t, err)
			require.Len(t, messages, 2)
			require.Equal(t, 1, botCalls)
			require.Equal(t, 2, readerCalls)
		})
	}
}

type observedCursors struct {
	domain.EventRepository
	reads []domain.ConversationRef
}

func (h *observedCursors) LatestMessageTime(ctx context.Context, ref domain.ConversationRef) (time.Time, error) {
	h.reads = append(h.reads, ref)
	return h.EventRepository.LatestMessageTime(ctx, ref)
}

func TestConnectedBackfillPrecedesLiveAndDeduplicates(t *testing.T) {
	var logs bytes.Buffer
	logger := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(logger) })
	at := time.Now().Add(-time.Hour).Truncate(time.Microsecond)
	missed := historyMessage(at.Add(time.Minute), "missed DM")
	started, release := make(chan struct{}), make(chan struct{})
	var opens, reads atomic.Int32
	a := apiAdapter(t, func(w http.ResponseWriter, r *http.Request) {
		require.NoError(t, r.ParseForm())
		switch r.URL.Path {
		case "/auth.test":
			fmt.Fprint(w, `{"ok":true,"user_id":"USELF","bot_id":"BSELF"}`)
		case "/conversations.open":
			opens.Add(1)
			fmt.Fprint(w, `{"ok":true,"channel":{"id":"DOWNER"}}`)
		case "/conversations.history":
			if r.Form.Get("channel") == "DOWNER" && reads.Add(1) == 1 {
				close(started)
				select {
				case <-release:
				case <-r.Context().Done():
					return
				}
			}
			switch r.Form.Get("channel") {
			case "CFAIL":
				fmt.Fprint(w, `{"ok":false,"error":"channel_not_found"}`)
			case "CGOOD":
				backfillPage(t, w, []sdk.Message{historyMessage(at.Add(2*time.Minute), "missed channel")}, false, "")
			default:
				if opens.Load() == 2 {
					require.Equal(t, missed.Timestamp, r.Form.Get("oldest"))
				}
				backfillPage(t, w, []sdk.Message{missed}, false, "")
			}
		default:
			t.Errorf("unexpected API %s", r.URL.Path)
		}
	}, false)
	a.owner = "UOWNER"
	accepted := true
	for _, channel := range []string{"CFAIL", "CGOOD"} {
		saveBackfillMessage(t, a, Conversation(a.name, channel, ""), at, &accepted)
	}
	history := &observedCursors{EventRepository: a.history}
	a.history = history
	db, err := repository.New(t.Context(), repository.Config{Path: filepath.Join(t.TempDir(), "inbox.db")})
	require.NoError(t, err)
	defer db.Close()
	inbox := repository.NewInboxRepository(db)
	stream := make(chan socketmode.Event, 8)
	acks := make(chan string, 4)
	a.socketEvents = stream
	a.runSocket = func(ctx context.Context) error { <-ctx.Done(); return nil }
	a.ack = func(_ context.Context, id string, _ any) error { acks <- id; return nil }
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	var got []string
	done := make(chan error, 1)
	go func() {
		done <- a.Start(ctx, func(ctx context.Context, event domain.InboundEvent) error {
			if len(history.reads) != 3*int(opens.Load()) {
				return errors.New("consumed before all cursors were fixed")
			}
			got = append(got, event.Text)
			if err := inbox.Receive(ctx, event, domain.InboxItem{SourceKey: event.SourceKey, SessionKey: domain.SessionKey(event.Conversation), Lane: "conv", ReadyAt: event.ReceivedAt, Payload: domain.InboxPayload{Event: &event}}); err != nil {
				return err
			}
			event.Accepted = &accepted
			return a.history.SaveInbound(ctx, event)
		})
	}()
	stopped := false
	defer func() {
		cancel()
		if !stopped {
			require.NoError(t, <-done)
		}
	}()
	stream <- socketmode.Event{Type: socketmode.EventTypeConnected}
	select {
	case <-started:
	case <-ctx.Done():
		t.Fatal("backfill did not start")
	}
	payload, err := slackevents.ParseEvent(json.RawMessage(fmt.Sprintf(`{"type":"event_callback","event_id":"live-id","event":{"type":"message","channel":"DOWNER","user":"UOWNER","text":"missed DM","ts":%q}}`, missed.Timestamp)), slackevents.OptionNoVerifyToken())
	require.NoError(t, err)
	stream <- socketmode.Event{Type: socketmode.EventTypeEventsAPI, Data: payload, Request: &socketmode.Request{EnvelopeID: "live"}}
	select {
	case <-acks:
		t.Fatal("live event acknowledged before backfill completed")
	default:
	}
	items, err := inbox.Active(t.Context())
	require.NoError(t, err)
	require.Empty(t, items)
	close(release)
	select {
	case id := <-acks:
		require.Equal(t, "live", id)
	case <-ctx.Done():
		t.Fatal("live event did not follow backfill")
	}
	require.Equal(t, []string{"missed DM", "missed channel", "missed DM"}, got)
	items, err = inbox.Active(t.Context())
	require.NoError(t, err)
	require.Len(t, items, 2)
	var count int
	require.NoError(t, db.QueryRowContext(t.Context(), "SELECT COUNT(*) FROM events").Scan(&count))
	require.Equal(t, 2, count)
	stream <- socketmode.Event{Type: socketmode.EventTypeConnected}
	stream <- socketmode.Event{Type: socketmode.EventTypeEventsAPI, Data: payload, Request: &socketmode.Request{EnvelopeID: "after-reconnect"}}
	select {
	case <-acks:
	case <-ctx.Done():
		t.Fatal("reconnect blocked live events")
	}
	cancel()
	require.NoError(t, <-done)
	stopped = true
	require.Equal(t, int32(2), opens.Load())
	require.Contains(t, logs.String(), "WARN")
	require.Contains(t, logs.String(), "Slack backfill read failed")
	require.Contains(t, logs.String(), "CFAIL")
	items, err = inbox.Active(t.Context())
	require.NoError(t, err)
	require.Len(t, items, 2)
}

func TestBackfillConsumerFailure(t *testing.T) {
	at := time.Now().Add(-time.Hour)
	a := apiAdapter(t, func(w http.ResponseWriter, r *http.Request) {
		backfillPage(t, w, []sdk.Message{historyMessage(at.Add(time.Minute), "missed")}, false, "")
	}, false)
	want := errors.New("persistence unavailable")
	err := a.backfill(t.Context(), []backfillTarget{{channel: "DTEST", since: at, until: time.Now()}}, func(context.Context, domain.InboundEvent) error { return want })
	require.ErrorIs(t, err, want)
}
