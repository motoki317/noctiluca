package discord

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"unicode/utf8"

	sdk "github.com/disgoorg/disgo/discord"
	"github.com/disgoorg/snowflake/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/motoki317/noctiluca/pkg/domain"
)

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

func TestSplitText(t *testing.T) {
	for _, text := range []string{strings.Repeat("a", 2000), strings.Repeat("🙂", 1001), strings.Repeat("line\n", 1000), strings.Repeat("paragraph\n\n", 300), "```go\n" + strings.Repeat("x", 6000) + "\n```", "~~~text\n" + strings.Repeat("code\n", 600) + "~~~", "```go\nunfinished", "```go\n" + strings.Repeat("x", 1990) + "```\n```", "```go\n```" + strings.Repeat(" ", 2000) + "z\n```"} {
		chunks, err := splitText(text)
		require.NoError(t, err)
		require.NotEmpty(t, chunks)
		for _, chunk := range chunks {
			assert.NotEqual(t, "```go\n\n```", chunk)
			assert.LessOrEqual(t, textUnits(chunk), 2000)
			assert.True(t, utf8.ValidString(chunk))
			state := codeFence{}
			for _, line := range strings.SplitAfter(chunk, "\n") {
				state = fenceAfter(state, line)
			}
			assert.Empty(t, state.marker)
		}
		if !strings.Contains(text, "```") && !strings.Contains(text, "~~~") {
			assert.Equal(t, text, strings.Join(chunks, ""))
		}
	}
	paragraph := strings.Repeat("a", 1200) + "\n\n"
	next := strings.Repeat("b", 900)
	chunks, err := splitText(paragraph + next)
	require.NoError(t, err)
	assert.Equal(t, []string{paragraph, next}, chunks)
	_, err = splitText("\xff")
	require.Error(t, err)
	_, err = splitText("```" + strings.Repeat("a", 2000) + "\n")
	require.ErrorContains(t, err, "2000")
}

func TestSendCreatesAndReusesThread(t *testing.T) {
	for _, race := range []bool{false, true} {
		t.Run(fmt.Sprint(race), func(t *testing.T) {
			created := false
			starts := 0
			var posts []sdk.MessageCreate
			a := testAdapter(t, Config{}, func(w http.ResponseWriter, r *http.Request) bool {
				switch r.URL.Path {
				case "/channels/300":
					if !created {
						w.WriteHeader(404)
						writeJSON(t, w, map[string]any{"code": 10003})
					} else {
						writeJSON(t, w, map[string]any{"id": "300", "type": 11, "parent_id": "400", "guild_id": "500"})
					}
					return true
				case "/channels/400/messages/300":
					m := message(300, 400)
					m.Content = "<@100> Original request"
					writeJSON(t, w, m)
					return true
				case "/channels/400/messages/300/threads":
					starts++
					created = true
					var body sdk.ThreadCreateFromMessage
					require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
					assert.Equal(t, "Original request", body.Name)
					if race {
						w.WriteHeader(400)
						writeJSON(t, w, map[string]any{"code": 160004})
					} else {
						writeJSON(t, w, map[string]any{"id": "300", "type": 11, "parent_id": "400"})
					}
					return true
				case "/channels/300/messages":
					var body sdk.MessageCreate
					require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
					posts = append(posts, body)
					writeJSON(t, w, message(snowflake.ID(800+len(posts)), 300))
					return true
				}
				return false
			})
			id, err := a.Send(t.Context(), "chat:400:300", domain.OutboundMessage{Text: strings.Repeat("🙂", 1100)})
			require.NoError(t, err)
			assert.Equal(t, "802", id)
			_, err = a.Send(t.Context(), "chat:400:300", domain.OutboundMessage{Text: "<@200> <@&700> @everyone"})
			require.NoError(t, err)
			assert.Equal(t, 1, starts)
			require.Len(t, posts, 3)
			for _, p := range posts {
				assert.LessOrEqual(t, textUnits(p.Content), 2000)
				require.NotNil(t, p.AllowedMentions)
				assert.Equal(t, []sdk.AllowedMentionType{sdk.AllowedMentionTypeUsers}, p.AllowedMentions.Parse)
				assert.Empty(t, p.AllowedMentions.Roles)
			}
			_, err = a.Send(t.Context(), "other:400:300", domain.OutboundMessage{Text: "no"})
			require.Error(t, err)
			_, err = a.Send(t.Context(), "chat:401:300", domain.OutboundMessage{Text: "no"})
			require.ErrorContains(t, err, "parent")
		})
	}
}

func TestToolsHistorySearchReactionsAndRecording(t *testing.T) {
	var paths []string
	a := testAdapter(t, Config{}, func(w http.ResponseWriter, r *http.Request) bool {
		paths = append(paths, r.URL.EscapedPath())
		switch {
		case strings.Contains(r.URL.Path, "/reactions/"):
			assert.Equal(t, "PUT", r.Method)
			w.WriteHeader(204)
			return true
		case r.URL.Path == "/guilds/500/messages/search":
			assert.Equal(t, "needle", r.URL.Query().Get("content"))
			assert.Equal(t, "25", r.URL.Query().Get("limit"))
			assert.Equal(t, "25", r.URL.Query().Get("offset"))
			writeJSON(t, w, map[string]any{"total_results": 1, "messages": [][]sdk.Message{{message(600, 400)}}})
			return true
		case r.URL.Path == "/channels/400/messages" && r.Method == "POST":
			writeJSON(t, w, message(800, 400))
			return true
		case r.URL.Path == "/channels/400/messages" || r.URL.Path == "/channels/300/messages":
			assert.Equal(t, "2", r.URL.Query().Get("limit"))
			assert.Equal(t, "700", r.URL.Query().Get("before"))
			writeJSON(t, w, []sdk.Message{message(602, 400), message(601, 400)})
			return true
		case r.URL.Path == "/channels/400/messages/300":
			writeJSON(t, w, message(300, 400))
			return true
		case r.URL.Path == "/channels/300":
			writeJSON(t, w, map[string]any{"id": "300", "type": 11, "parent_id": "400"})
			return true
		}
		return false
	})
	require.Len(t, a.Tools(), 7)
	for _, name := range []string{"read_history", "read_thread"} {
		in := map[string]any{"channel": "400", "limit": 2, "before": "700"}
		if name == "read_thread" {
			in["thread"] = "300"
		}
		raw, err := callTool(t, a, name, in)
		require.NoError(t, err)
		assert.Contains(t, string(raw), `"next_before":"601"`)
		if name == "read_thread" {
			assert.Contains(t, string(raw), `"starter"`)
		}
	}
	raw, err := callTool(t, a, "search", map[string]any{"channel": "400", "query": "needle", "limit": 25, "offset": 25})
	require.NoError(t, err)
	assert.Contains(t, string(raw), "hello")
	for _, emoji := range []string{"👍", "#️⃣", "custom:700"} {
		_, err = callTool(t, a, "react", map[string]string{"channel": "400", "message": "600", "emoji": emoji})
		require.NoError(t, err)
		assert.Equal(t, "/channels/400/messages/600/reactions/"+url.PathEscape(emoji)+"/@me", paths[len(paths)-1])
	}
	_, err = callTool(t, a, "send", map[string]string{"channel": "400", "text": "hello"})
	require.NoError(t, err)
	sent, err := a.history.ListOutbound(t.Context(), "chat:400:", 10)
	require.NoError(t, err)
	require.Len(t, sent, 1)
	assert.Equal(t, "800", sent[0].ID)
	for _, tc := range []struct {
		name string
		in   any
	}{
		{"send", map[string]any{"channel": "400", "text": "x", "broadcast": true}},
		{"search", map[string]any{"guild": "500", "query": "x"}},
		{"search", map[string]any{"query": "x"}},
		{"search", map[string]any{"channel": "400", "query": "x", "limit": 26}},
		{"react", map[string]any{"channel": "400", "message": "600", "emoji": "x:invalid"}},
		{"read_history", map[string]any{"channel": "bad"}},
		{"read_history", map[string]any{"channel": "400", "limit": 101}},
		{"send", map[string]any{"channel": "400"}},
	} {
		before := len(paths)
		_, err = callTool(t, a, tc.name, tc.in)
		require.Error(t, err)
		assert.Len(t, paths, before)
	}
}

func TestSearchResolvesChannelGuild(t *testing.T) {
	for _, tc := range []struct {
		name string
		typ  int
		fail bool
		want string
	}{
		{"guild", 0, false, ""}, {"thread", 11, false, ""},
		{"dm", 1, false, "DM channels cannot be searched"}, {"group dm", 3, false, "DM channels cannot be searched"},
		{"forbidden", 0, true, "Discord HTTP 403"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var channels, searches int
			a := testAdapter(t, Config{}, func(w http.ResponseWriter, r *http.Request) bool {
				switch r.URL.Path {
				case "/channels/300":
					channels++
					if tc.fail {
						w.WriteHeader(http.StatusForbidden)
						writeJSON(t, w, map[string]any{"code": 50013, "message": "private response"})
					} else {
						writeJSON(t, w, map[string]any{"id": "300", "type": tc.typ, "parent_id": "400", "guild_id": "500"})
					}
				case "/guilds/500/messages/search":
					searches++
					writeJSON(t, w, map[string]any{"total_results": 1, "messages": [][]sdk.Message{{message(600, 400)}}})
				default:
					return false
				}
				return true
			})
			for _, tool := range a.Tools() {
				if tool.Name == "chat_search" {
					var schema struct {
						Properties map[string]any `json:"properties"`
						Required   []string       `json:"required"`
					}
					require.NoError(t, json.Unmarshal(tool.InputSchema, &schema))
					require.Contains(t, schema.Properties, "channel")
					require.NotContains(t, schema.Properties, "guild")
					require.Contains(t, schema.Required, "channel")
				}
			}
			for range 2 {
				raw, err := callTool(t, a, "search", map[string]string{"channel": "300", "query": "needle"})
				if tc.want == "" {
					require.NoError(t, err)
					assert.Contains(t, string(raw), "hello")
				} else {
					require.ErrorContains(t, err, tc.want)
					assert.NotContains(t, err.Error(), "private response")
				}
			}
			if tc.fail {
				assert.Equal(t, 2, channels)
			} else {
				assert.Equal(t, 1, channels)
			}
			if tc.want == "" {
				assert.Equal(t, 2, searches)
			} else {
				assert.Zero(t, searches)
			}
		})
	}
}

func TestSearchSharesLocationCacheWithEvents(t *testing.T) {
	a := testAdapter(t, Config{}, func(w http.ResponseWriter, r *http.Request) bool {
		if r.URL.Path != "/guilds/500/messages/search" {
			return false
		}
		writeJSON(t, w, map[string]any{"total_results": 0, "messages": []any{}})
		return true
	})
	var workers sync.WaitGroup
	for i := range 20 {
		workers.Go(func() {
			_, err := callTool(t, a, "search", map[string]string{"channel": "400", "query": "needle"})
			assert.NoError(t, err)
		})
		workers.Go(func() {
			_, err := a.message(t.Context(), message(600, snowflake.ID(400+i)), false)
			assert.NoError(t, err)
		})
	}
	workers.Wait()
}

type transportFunc func(*http.Request) (*http.Response, error)

func (f transportFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestFiles(t *testing.T) {
	bytes := []byte{0, 1, 2, 255}
	cdnFail := false
	cdn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Empty(t, r.Header.Get("Authorization"))
		if cdnFail {
			w.WriteHeader(403)
			return
		}
		_, _ = w.Write(bytes)
	}))
	defer cdn.Close()
	cdnBase, err := url.Parse(cdn.URL)
	require.NoError(t, err)
	a := testAdapter(t, Config{}, func(w http.ResponseWriter, r *http.Request) bool {
		if r.URL.Path == "/channels/400/messages/600" {
			m := message(600, 400)
			m.Attachments = []sdk.Attachment{{ID: 700, Filename: "binary.dat", URL: "https://cdn.discordapp.com/attachments/fresh?signature=secret"}}
			writeJSON(t, w, m)
			return true
		}
		if r.URL.Path == "/channels/400/messages" && r.Method == "POST" {
			require.NoError(t, r.ParseMultipartForm(1<<20))
			defer r.MultipartForm.RemoveAll()
			var payload sdk.MessageCreate
			require.NoError(t, json.Unmarshal([]byte(r.FormValue("payload_json")), &payload))
			require.NotNil(t, payload.AllowedMentions)
			assert.Equal(t, []sdk.AllowedMentionType{sdk.AllowedMentionTypeUsers}, payload.AllowedMentions.Parse)
			require.Len(t, r.MultipartForm.File, 1)
			for _, files := range r.MultipartForm.File {
				file, err := files[0].Open()
				require.NoError(t, err)
				data, err := io.ReadAll(file)
				require.NoError(t, err)
				require.NoError(t, file.Close())
				assert.Equal(t, bytes, data)
			}
			m := message(800, 400)
			m.Attachments = []sdk.Attachment{{ID: 701, Filename: "binary.dat"}}
			writeJSON(t, w, m)
			return true
		}
		return false
	})
	transport := a.client.HTTPClient().Transport
	a.client.HTTPClient().Transport = transportFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Host == "cdn.discordapp.com" {
			clone := r.Clone(r.Context())
			u := *r.URL
			u.Scheme, u.Host = cdnBase.Scheme, cdnBase.Host
			clone.URL = &u
			return transport.RoundTrip(clone)
		}
		return transport.RoundTrip(r)
	})
	path := filepath.Join(t.TempDir(), "nested", "binary.dat")
	in := map[string]string{"file_ref": "chat:400:600:700", "path": path}
	_, err = callTool(t, a, "download_file", in)
	require.NoError(t, err)
	actual, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, bytes, actual)
	stat, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0600), stat.Mode().Perm())
	_, err = callTool(t, a, "download_file", in)
	require.Error(t, err)
	cdnFail = true
	in["path"] = filepath.Join(t.TempDir(), "failed.dat")
	_, err = callTool(t, a, "download_file", in)
	require.Error(t, err)
	assert.NoFileExists(t, in["path"])
	in["file_ref"] = "other:400:600:700"
	_, err = callTool(t, a, "download_file", in)
	require.Error(t, err)
	raw, err := callTool(t, a, "upload_file", map[string]string{"channel": "400", "path": path, "text": "caption"})
	require.NoError(t, err)
	assert.Contains(t, string(raw), "chat:400:800:701")
	sent, err := a.history.ListOutbound(t.Context(), "chat:400:", 1)
	require.NoError(t, err)
	require.Len(t, sent, 1)
	assert.Equal(t, "chat:400:800:701", sent[0].Message.Attachments[0].Ref)
	_, err = callTool(t, a, "upload_file", map[string]string{"channel": "400", "path": t.TempDir()})
	require.ErrorContains(t, err, "regular file")
}

func TestPermissionAndCancellationErrors(t *testing.T) {
	a := testAdapter(t, Config{}, func(w http.ResponseWriter, r *http.Request) bool {
		w.WriteHeader(403)
		writeJSON(t, w, map[string]any{"code": 50013, "message": "fake-token"})
		return true
	})
	_, err := a.Send(t.Context(), "chat:400:", domain.OutboundMessage{Text: "hello"})
	require.ErrorContains(t, err, "HTTP 403")
	assert.NotContains(t, err.Error(), "fake-token")
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = a.Send(ctx, "chat:400:", domain.OutboundMessage{Text: "hello"})
	require.ErrorIs(t, err, context.Canceled)
}
