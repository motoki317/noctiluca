package traq

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	sdk "github.com/traPtitech/go-traq"

	"github.com/motoki317/noctiluca/pkg/domain"
)

type historyFake struct {
	domain.EventRepository
	mu       sync.Mutex
	inbound  []domain.InboundEvent
	outbound []domain.SentMessage
	err      error
}

func (h *historyFake) SaveOutbound(_ context.Context, sent domain.SentMessage) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.outbound = append(h.outbound, sent)
	return h.err
}
func (h *historyFake) LatestMessageTime(_ context.Context, ref domain.ConversationRef) (time.Time, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.err != nil {
		return time.Time{}, h.err
	}
	var last time.Time
	for _, event := range h.inbound {
		if event.Conversation == ref && !event.Deleted &&
			(event.Kind == domain.InboundEventMessage || event.Kind == domain.InboundEventMessageEdited) && event.ReceivedAt.After(last) {
			last = event.ReceivedAt
		}
	}
	return last, nil
}
func (h *historyFake) GetInbound(_ context.Context, ref domain.ConversationRef) (domain.InboundEvent, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.err != nil {
		return domain.InboundEvent{}, h.err
	}
	for _, event := range h.inbound {
		if event.MessageRef == ref {
			return event, nil
		}
	}
	return domain.InboundEvent{}, domain.ErrNotFound
}

func apiAdapter(t *testing.T, handler http.HandlerFunc) *adapter {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "Bearer test-token", r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "application/json")
		handler(w, r)
	}))
	t.Cleanup(server.Close)
	a, err := newAdapter("team", Config{Origin: server.URL, AccessToken: "test-token"}, &historyFake{})
	require.NoError(t, err)
	return a
}
func respond(t *testing.T, w http.ResponseWriter, value any) {
	t.Helper()
	require.NoError(t, json.NewEncoder(w).Encode(value))
}
func restMessage(id, channel, text string, at time.Time) sdk.Message {
	return sdk.Message{Id: id, ChannelId: channel, UserId: "human", Content: text, CreatedAt: at, UpdatedAt: at, Stamps: []sdk.MessageStamp{}}
}
func callTool(t *testing.T, a *adapter, name string, input any) (json.RawMessage, error) {
	t.Helper()
	raw, err := json.Marshal(input)
	require.NoError(t, err)
	for _, tool := range a.Tools() {
		if tool.Name == a.name+"_"+name {
			return tool.Handler(t.Context(), raw)
		}
	}
	t.Fatalf("missing tool %s", name)
	return nil, nil
}

func TestSendQuotesAndLimits(t *testing.T) {
	var posted []string
	a := apiAdapter(t, func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/api/v3/channels/channel/messages", r.URL.Path)
		require.Equal(t, http.MethodPost, r.Method)
		var request sdk.PostMessageRequest
		require.NoError(t, json.NewDecoder(r.Body).Decode(&request))
		require.True(t, request.GetEmbed())
		posted = append(posted, request.Content)
		respond(t, w, restMessage("sent", "channel", request.Content, time.Now()))
	})
	for _, target := range []string{"", "request"} {
		id, err := a.Send(t.Context(), Conversation("team", "channel", target), domain.OutboundMessage{Text: "**hello** @someone", Attachments: []domain.Attachment{{Ref: "file"}}})
		require.NoError(t, err)
		require.Equal(t, "sent", id)
	}
	require.Equal(t, "**hello** @someone\n"+a.config.Origin+"/files/file", posted[0])
	require.Equal(t, posted[0]+"\n"+a.config.Origin+"/messages/request", posted[1])
	_, err := a.Send(t.Context(), Conversation("team", "channel", ""), domain.OutboundMessage{Text: strings.Repeat("界", 10000)})
	require.NoError(t, err)
	for _, text := range []string{strings.Repeat("界", 10001), string([]byte{0xff})} {
		_, err := a.Send(t.Context(), Conversation("team", "channel", ""), domain.OutboundMessage{Text: text})
		require.ErrorContains(t, err, "10000")
	}
	_, err = a.Send(t.Context(), Conversation("team", "channel", "request"), domain.OutboundMessage{Text: strings.Repeat("a", 10000)})
	require.ErrorContains(t, err, "10000")
	for _, ref := range []domain.ConversationRef{"other:channel:", "team:channel", "team:../bad:", "team:channel:bad/message"} {
		_, err := a.Send(t.Context(), ref, domain.OutboundMessage{Text: "hi"})
		require.Error(t, err)
	}
	require.Len(t, posted, 3)
}

func TestProviders(t *testing.T) {
	a := apiAdapter(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v3/users/owner/dm-channel":
			respond(t, w, sdk.DMChannel{Id: "dm", UserId: "owner"})
		case "/api/v3/users/owner":
			respond(t, w, sdk.UserDetail{Id: "owner", Name: "owner", DisplayName: "Owner"})
		case "/api/v3/groups/group/members":
			respond(t, w, []sdk.UserGroupMember{{Id: "member"}})
		case "/api/v3/channels/channel":
			respond(t, w, sdk.Channel{Id: "channel", Name: "child", ParentId: *sdk.NewNullableString(ptr("parent"))})
		case "/api/v3/channels/parent":
			respond(t, w, sdk.Channel{Id: "parent", Name: "parent"})
		case "/api/v3/messages/root":
			respond(t, w, restMessage("root", "channel", "request", time.Unix(100, 0)))
		default:
			t.Errorf("unexpected %s", r.URL.Path)
			w.WriteHeader(500)
		}
	})
	ref, err := a.DirectMessage(t.Context(), "owner")
	require.NoError(t, err)
	require.Equal(t, Conversation("team", "dm", ""), ref)
	info := a.SourceInfo(t.Context(), Conversation("team", "channel", "root"), "owner", true)
	require.Equal(t, "#parent/child", info.Conversation)
	require.Equal(t, "Owner", info.Sender)
	require.Equal(t, "request", info.ThreadText)
	require.Equal(t, int64(100), info.ThreadAt.Unix())
	require.Equal(t, "Owner", a.SourceInfo(t.Context(), ref, "owner", false).Conversation)
	groups, err := a.UserGroups(t.Context(), []string{"group"})
	require.NoError(t, err)
	require.Equal(t, []string{"member"}, groups["group"])
}
func ptr[T any](value T) *T { return &value }

func TestToolsReadSendAndReact(t *testing.T) {
	at := time.Now()
	var calls []string
	a := apiAdapter(t, func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, r.Method+" "+r.URL.Path)
		switch r.URL.Path {
		case "/api/v3/channels/channel/messages":
			if r.Method == http.MethodPost {
				respond(t, w, restMessage("sent", "channel", "hi", at))
				return
			}
			require.Equal(t, "2", r.URL.Query().Get("offset"))
			require.Equal(t, "1", r.URL.Query().Get("limit"))
			require.Equal(t, "desc", r.URL.Query().Get("order"))
			respond(t, w, []sdk.Message{restMessage("found", "channel", "hello", at)})
		case "/api/v3/messages/found":
			respond(t, w, restMessage("found", "channel", "hello", at))
		case "/api/v3/messages":
			require.Equal(t, "hello", r.URL.Query().Get("word"))
			require.Equal(t, "channel", r.URL.Query().Get("in"))
			respond(t, w, sdk.MessageSearchResult{TotalHits: 1, Hits: []sdk.Message{restMessage("found", "channel", "hello", at)}})
		case "/api/v3/stamps":
			respond(t, w, []sdk.StampWithThumbnail{{Id: "stamp", Name: "eyes"}})
		case "/api/v3/messages/found/stamps/stamp":
			var body sdk.PostMessageStampRequest
			require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
			require.EqualValues(t, 1, body.Count)
			w.WriteHeader(204)
		default:
			t.Errorf("unexpected %s", r.URL.Path)
			w.WriteHeader(500)
		}
	})
	require.Len(t, a.Tools(), 7)
	for _, tool := range a.Tools() {
		require.NotContains(t, string(tool.InputSchema), "broadcast")
	}
	for _, tc := range []struct {
		name     string
		input    map[string]any
		contains string
	}{
		{"read_history", map[string]any{"channel": "channel", "limit": 1, "offset": 2}, `"next_offset":3`},
		{"read_message", map[string]any{"message_id": "found"}, `"id":"found"`},
		{"search", map[string]any{"query": "hello", "channel": "channel"}, `"total":1`},
		{"react", map[string]any{"message_id": "found", "stamp": ":eyes:"}, `"ok":true`},
		{"send", map[string]any{"channel": "channel", "text": "hi", "quote_message_id": "root"}, `"id":"sent"`},
	} {
		raw, err := callTool(t, a, tc.name, tc.input)
		require.NoError(t, err)
		require.Contains(t, string(raw), tc.contains)
	}
	history := a.history.(*historyFake)
	require.Len(t, history.outbound, 1)
	require.Equal(t, Conversation("team", "channel", "root"), history.outbound[0].Conversation)
	_, err := callTool(t, a, "react", map[string]any{"message_id": "found", "stamp": "missing"})
	require.ErrorContains(t, err, "unknown traQ stamp")
	before := len(calls)
	for _, in := range []map[string]any{{"channel": "team:channel:", "text": "x"}, {"channel": "channel", "broadcast": true}, {"text": "x"}} {
		_, err := callTool(t, a, "send", in)
		require.Error(t, err)
	}
	_, err = callTool(t, a, "read_history", map[string]any{"channel": "channel", "limit": 101})
	require.Error(t, err)
	require.Len(t, calls, before)
	require.Contains(t, a.ToolNotes(), "team_read_message")
}

func TestFileTransfer(t *testing.T) {
	content := []byte{0, 255, 128, 10, 13, 1}
	var posted string
	a := apiAdapter(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v3/files/file":
			w.Header().Set("Content-Type", "application/octet-stream")
			_, _ = w.Write(content)
		case "/api/v3/files":
			require.NoError(t, r.ParseMultipartForm(1<<20))
			defer r.MultipartForm.RemoveAll()
			require.Equal(t, "channel", r.FormValue("channelId"))
			file, header, err := r.FormFile("file")
			require.NoError(t, err)
			defer file.Close()
			require.Equal(t, "upload.bin", header.Filename)
			bytes, err := io.ReadAll(file)
			require.NoError(t, err)
			require.Equal(t, content, bytes)
			respond(t, w, sdk.FileInfo{Id: "file", Name: "upload.bin", Mime: "application/octet-stream"})
		case "/api/v3/channels/channel/messages":
			var request sdk.PostMessageRequest
			require.NoError(t, json.NewDecoder(r.Body).Decode(&request))
			posted = request.Content
			respond(t, w, restMessage("sent", "channel", posted, time.Now()))
		default:
			t.Errorf("unexpected %s", r.URL.Path)
			w.WriteHeader(500)
		}
	})
	path := filepath.Join(t.TempDir(), "sub", "download.bin")
	_, err := callTool(t, a, "download_file", map[string]any{"file_id": "file", "path": path})
	require.NoError(t, err)
	bytes, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, content, bytes)
	stat, err := os.Stat(path)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0600), stat.Mode().Perm())
	_, err = callTool(t, a, "download_file", map[string]any{"file_id": "file", "path": path})
	require.Error(t, err)
	upload := filepath.Join(t.TempDir(), "upload.bin")
	require.NoError(t, os.WriteFile(upload, content, 0600))
	_, err = callTool(t, a, "upload_file", map[string]any{"channel": "channel", "path": upload, "text": "caption", "quote_message_id": "root"})
	require.NoError(t, err)
	require.Equal(t, "caption\n"+a.config.Origin+"/files/file\n"+a.config.Origin+"/messages/root", posted)
	require.Len(t, a.history.(*historyFake).outbound, 1)
}

func TestDownloadFailureAndRedirect(t *testing.T) {
	for _, mode := range []string{"truncated", "redirect", "denied"} {
		t.Run(mode, func(t *testing.T) {
			a := apiAdapter(t, func(w http.ResponseWriter, r *http.Request) {
				switch mode {
				case "truncated":
					w.Header().Set("Content-Length", "100")
					_, _ = w.Write([]byte("partial"))
				case "redirect":
					w.Header().Set("Location", "https://invalid.example/file")
					w.WriteHeader(302)
				case "denied":
					w.WriteHeader(403)
				}
			})
			path := filepath.Join(t.TempDir(), "file")
			_, err := callTool(t, a, "download_file", map[string]any{"file_id": "file", "path": path})
			require.Error(t, err)
			require.NoFileExists(t, path)
		})
	}
}

func TestErrorsDoNotExposeResponseSecrets(t *testing.T) {
	a := apiAdapter(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(403)
		_, _ = w.Write([]byte("test-token private body"))
	})
	_, err := a.Send(t.Context(), Conversation("team", "channel", ""), domain.OutboundMessage{Text: "hi"})
	require.ErrorContains(t, err, "403")
	require.NotContains(t, err.Error(), "test-token")
	require.Equal(t, domain.SenderBot, a.user(t.Context(), "unknown").Kind)
	require.Equal(t, "channel", a.SourceInfo(t.Context(), Conversation("team", "channel", ""), "", false).Conversation)
}

func TestHistoryFailureAfterPost(t *testing.T) {
	a := apiAdapter(t, func(w http.ResponseWriter, r *http.Request) {
		respond(t, w, restMessage("sent", "channel", "hi", time.Now()))
	})
	a.history.(*historyFake).err = errors.New("storage failed")
	_, err := callTool(t, a, "send", map[string]any{"channel": "channel", "text": "hi"})
	require.EqualError(t, err, "posted to traQ but history recording failed")
}

func TestConstructorValidation(t *testing.T) {
	for _, origin := range []string{"", "ftp://example.org", "https://user:pass@example.org", "https://example.org/path", "https://example.org?query", "https://example.org#fragment"} {
		_, err := newAdapter("team", Config{Origin: origin, AccessToken: "token"}, &historyFake{})
		require.Error(t, err)
	}
	for _, tc := range []struct {
		name, token string
		history     domain.EventRepository
	}{
		{"Bad", "token", &historyFake{}}, {"team", "", &historyFake{}}, {"team", "token", nil},
	} {
		_, err := newAdapter(tc.name, Config{Origin: "https://example.org", AccessToken: tc.token}, tc.history)
		require.Error(t, err)
	}
}
