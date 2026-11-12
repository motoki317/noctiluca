package slack

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"slices"
	"testing"
	"testing/synctest"
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
	accepted := true
	for _, entry := range []struct {
		ref domain.ConversationRef
		at  time.Time
	}{
		{"slack:DOWNER:", now.Add(-time.Hour)},
		{"slack:COLD:", now.Add(-25 * time.Hour)},
		{"slack:COLD:1.000001", now.Add(-3 * time.Hour)},
	} {
		require.NoError(t, a.history.SaveInbound(t.Context(), domain.InboundEvent{
			Kind: domain.InboundEventMessage, Conversation: entry.ref, ReceivedAt: entry.at, Accepted: &accepted,
			Sender: domain.Sender{ID: "UOWNER", Kind: domain.SenderHuman},
		}))
	}
	require.NoError(t, a.history.SaveOutbound(t.Context(), domain.SentMessage{Conversation: "slack:CSENT:2.000001", SentAt: now}))
	require.NoError(t, a.history.SaveOutbound(t.Context(), domain.SentMessage{Conversation: "slack:COLD:1.000001", SentAt: now.Add(-2 * time.Hour)}))
	before := time.Now().Add(-24 * time.Hour)
	targets := a.backfillTargets(t.Context())
	after := time.Now().Add(-24 * time.Hour)
	require.Equal(t, 1, opens)
	require.Len(t, targets, 4)
	require.Equal(t, "DOWNER", targets[0].channel)
	want := map[domain.ConversationRef]time.Time{
		"slack:DOWNER:": now.Add(-time.Hour), "slack:COLD:": {},
		"slack:COLD:1.000001": now.Add(-2 * time.Hour), "slack:CSENT:2.000001": now,
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

func TestBackfillSkipsUnreadableCursors(t *testing.T) {
	for _, operation := range []string{"inbound cursor", "outbound cursor"} {
		t.Run(operation, func(t *testing.T) {
			a := apiAdapter(t, func(_ http.ResponseWriter, r *http.Request) {
				t.Errorf("unexpected API %s", r.URL.Path)
			}, false)
			require.NoError(t, a.history.SaveOutbound(t.Context(), domain.SentMessage{Conversation: "slack:CTEST:1.000001", SentAt: time.Now()}))
			a.history = &backfillLookupFailure{EventRepository: a.history, operation: operation, err: errors.New("lookup failed")}
			require.Empty(t, a.backfillTargets(t.Context()))
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
	for _, joined := range []bool{true, false} {
		t.Run(fmt.Sprintf("joined=%t", joined), func(t *testing.T) {
			at := time.Now().Add(-time.Hour).Truncate(time.Microsecond)
			root := formatTimestamp(at.Add(-time.Hour))
			message := historyMessage(at.Add(time.Minute), "reply")
			message.ThreadTimestamp = root
			var reads, lookups int
			a := apiAdapter(t, func(w http.ResponseWriter, r *http.Request) {
				require.Equal(t, "/conversations.replies", r.URL.Path)
				require.NoError(t, r.ParseForm())
				require.Equal(t, root, r.Form.Get("ts"))
				if r.Form.Get("oldest") == "" {
					lookups++
					user := "UOTHER"
					if joined {
						user = "USELF"
					}
					backfillPage(t, w, []sdk.Message{{Msg: sdk.Msg{User: user, Timestamp: root}}}, false, "")
				} else {
					reads++
					backfillPage(t, w, []sdk.Message{{Msg: sdk.Msg{User: "UOTHER", Timestamp: root, ThreadTimestamp: root, Text: "root"}}, message}, false, "")
				}
			}, false)
			a.selfUser = "USELF"
			ref := Conversation(a.name, "CTEST", root)
			require.NoError(t, a.history.SaveOutbound(t.Context(), domain.SentMessage{Conversation: ref, SentAt: at}))
			var got []domain.InboundEvent
			require.NoError(t, a.backfill(t.Context(), a.backfillTargets(t.Context()), func(_ context.Context, event domain.InboundEvent) error {
				got = append(got, event)
				return nil
			}))
			require.Equal(t, 1, reads)
			require.Equal(t, 1, lookups)
			if joined {
				require.Len(t, got, 1)
				require.Equal(t, ref, got[0].Conversation)
				require.Equal(t, Conversation(a.name, "CTEST", message.Timestamp), got[0].MessageRef)
				require.Equal(t, "slack:m:CTEST:"+message.Timestamp, got[0].SourceKey)
			} else {
				require.Empty(t, got)
			}
		})
	}
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
		require.Equal(t, "slack:m:DTEST:"+messages[i].Timestamp, event.SourceKey)
		require.Equal(t, "FTEST", event.Attachments[0].Ref)
	}
}

func TestBackfillRateLimit(t *testing.T) {
	for _, tc := range []struct {
		name, retryAfter string
		timeout, wait    time.Duration
		calls            int
	}{
		{name: "retry", retryAfter: "1", wait: time.Second, calls: 2},
		{name: "context bounds retry", retryAfter: "60", timeout: 100 * time.Millisecond, wait: 100 * time.Millisecond, calls: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				var calls int
				at := time.Now().Add(-time.Hour)
				bot := sdk.New("xoxb-test", sdk.OptionAPIURL("https://example.test/"),
					sdk.OptionHTTPClient(&http.Client{Transport: uploadTransport(func(w http.ResponseWriter, r *http.Request) {
						calls++
						if calls == 1 {
							w.Header().Set("Retry-After", tc.retryAfter)
							w.WriteHeader(http.StatusTooManyRequests)
							return
						}
						backfillPage(t, w, []sdk.Message{historyMessage(at.Add(time.Minute), "retried")}, false, "")
					})}))
				a := &adapter{bot: bot, reader: bot}
				ctx := t.Context()
				if tc.timeout != 0 {
					var cancel context.CancelFunc
					ctx, cancel = context.WithTimeout(ctx, tc.timeout)
					defer cancel()
				}
				start := time.Now()
				messages, err := a.readBackfill(ctx, backfillTarget{channel: "DTEST", since: at, until: time.Now()})
				if tc.timeout != 0 {
					require.ErrorIs(t, err, context.DeadlineExceeded)
				} else {
					require.NoError(t, err)
					require.Len(t, messages, 1)
				}
				require.Equal(t, tc.wait, time.Since(start))
				require.Equal(t, tc.calls, calls)
			})
		})
	}
}

func TestBackfillReaderFallback(t *testing.T) {
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
	messages, err := a.readBackfill(t.Context(), backfillTarget{channel: "CTEST", since: at, until: time.Now()})
	require.NoError(t, err)
	require.Len(t, messages, 2)
	require.Equal(t, 1, botCalls)
	require.Equal(t, 2, readerCalls)
}

func TestConnectedBackfillPrecedesLive(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		at := time.Now().Add(-time.Hour).Truncate(time.Microsecond)
		missed := historyMessage(at.Add(time.Minute), "missed DM")
		started, release := make(chan struct{}), make(chan struct{})
		db, err := repository.New(t.Context(), repository.Config{Path: filepath.Join(t.TempDir(), "history.db")})
		require.NoError(t, err)
		defer db.Close()
		a, err := newAdapter("slack", Config{AppToken: "xapp-test", BotToken: "xoxb-test", Owner: "UOWNER"}, repository.NewEventRepository(db),
			sdk.OptionAPIURL("https://example.test/"),
			sdk.OptionHTTPClient(&http.Client{Transport: uploadTransport(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/auth.test":
					fmt.Fprint(w, `{"ok":true,"user_id":"USELF","bot_id":"BSELF"}`)
				case "/conversations.open":
					fmt.Fprint(w, `{"ok":true,"channel":{"id":"DOWNER"}}`)
				case "/conversations.history":
					close(started)
					select {
					case <-release:
					case <-r.Context().Done():
						return
					}
					backfillPage(t, w, []sdk.Message{missed}, false, "")
				default:
					t.Errorf("unexpected API %s", r.URL.Path)
				}
			})}))
		require.NoError(t, err)
		stream := make(chan socketmode.Event, 1)
		acks := make(chan string, 1)
		a.socketEvents = stream
		a.runSocket = func(ctx context.Context) error { <-ctx.Done(); return nil }
		a.ack = func(_ context.Context, id string, _ any) error { acks <- id; return nil }
		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		defer cancel()
		events := make(chan domain.InboundEvent, 2)
		done := make(chan error, 1)
		go func() {
			done <- a.Start(ctx, func(_ context.Context, event domain.InboundEvent) error { events <- event; return nil })
		}()
		defer func() { cancel(); require.NoError(t, <-done) }()
		stream <- socketmode.Event{Type: socketmode.EventTypeConnected}
		select {
		case <-started:
		case <-ctx.Done():
			t.Fatal("backfill did not start")
		}
		payload, err := slackevents.ParseEvent(json.RawMessage(`{"type":"event_callback","event":{"type":"message","channel":"DOWNER","user":"UOWNER","text":"live DM","ts":"2.3"}}`), slackevents.OptionNoVerifyToken())
		require.NoError(t, err)
		stream <- socketmode.Event{Type: socketmode.EventTypeEventsAPI, Data: payload, Request: &socketmode.Request{EnvelopeID: "live"}}
		synctest.Wait()
		require.Empty(t, acks)
		require.Empty(t, events)
		close(release)
		for _, want := range []string{"missed DM", "live DM"} {
			select {
			case event := <-events:
				require.Equal(t, want, event.Text)
			case <-ctx.Done():
				t.Fatal("event was not delivered")
			}
		}
		select {
		case id := <-acks:
			require.Equal(t, "live", id)
		case <-ctx.Done():
			t.Fatal("live event was not acknowledged")
		}
	})
}

func TestBackfillContinuesAfterReadFailure(t *testing.T) {
	at := time.Now().Add(-time.Hour)
	a := apiAdapter(t, func(w http.ResponseWriter, r *http.Request) {
		require.NoError(t, r.ParseForm())
		if r.Form.Get("channel") == "CFAIL" {
			fmt.Fprint(w, `{"ok":false,"error":"channel_not_found"}`)
		} else {
			backfillPage(t, w, []sdk.Message{historyMessage(at.Add(time.Minute), "missed channel")}, false, "")
		}
	}, false)
	var got []string
	require.NoError(t, a.backfill(t.Context(), []backfillTarget{
		{channel: "CFAIL", since: at, until: time.Now()},
		{channel: "CGOOD", since: at, until: time.Now()},
	}, func(_ context.Context, event domain.InboundEvent) error {
		got = append(got, event.Text)
		return nil
	}))
	require.Equal(t, []string{"missed channel"}, got)
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
