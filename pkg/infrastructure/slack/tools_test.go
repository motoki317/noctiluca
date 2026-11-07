package slack

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/motoki317/noctiluca/pkg/domain"
	"github.com/motoki317/noctiluca/pkg/infrastructure/repository"
)

func invoke(t *testing.T, a *adapter, name string, args any) json.RawMessage {
	t.Helper()
	data, err := json.Marshal(args)
	require.NoError(t, err)
	for _, tool := range a.Tools() {
		if tool.Name == name {
			result, err := tool.Handler(t.Context(), data)
			require.NoError(t, err)
			return result
		}
	}
	t.Fatal("tool not found", name)
	return nil
}

func TestReadTools(t *testing.T) {
	a := apiAdapter(t, func(w http.ResponseWriter, r *http.Request) {
		require.NoError(t, r.ParseForm())
		if r.URL.Path == "/search.messages" {
			require.Equal(t, "xoxp-test", r.Form.Get("token"))
		} else {
			require.Equal(t, "xoxb-test", r.Form.Get("token"))
		}
		switch r.URL.Path {
		case "/conversations.replies":
			require.Equal(t, "CTEST", r.Form.Get("channel"))
			require.Equal(t, "123.000001", r.Form.Get("ts"))
			require.Equal(t, "next", r.Form.Get("cursor"))
			_, _ = w.Write([]byte(`{"ok":true,"messages":[{"user":"UMEMBER","text":"thread","ts":"124.000001"}],"has_more":true,"response_metadata":{"next_cursor":"later"}}`))
		case "/conversations.history":
			require.Equal(t, "20", r.Form.Get("limit"))
			_, _ = w.Write([]byte(`{"ok":true,"messages":[{"text":"history","ts":"123.000001"}],"response_metadata":{"next_cursor":"next"}}`))
		case "/search.messages":
			require.Equal(t, "query", r.Form.Get("query"))
			require.Equal(t, "2", r.Form.Get("page"))
			_, _ = w.Write([]byte(`{"ok":true,"messages":{"total":1,"matches":[{"channel":{"id":"CTEST"},"text":"match","ts":"123.000001","permalink":"https://example.test/message"}]}}`))
		default:
			t.Errorf("unexpected API %s", r.URL.Path)
		}
	}, true)
	require.Len(t, a.Tools(), 7)
	result := invoke(t, a, "slack_read_thread", map[string]any{"channel": "CTEST", "thread_ts": "123.000001", "cursor": "next"})
	require.Contains(t, string(result), `"next_cursor":"later"`)
	require.Contains(t, string(result), `"text":"thread"`)
	require.Contains(t, string(invoke(t, a, "slack_read_history", map[string]string{"channel": "CTEST"})), `"text":"history"`)
	require.Contains(t, string(invoke(t, a, "slack_search", map[string]any{"query": "query", "page": 2})), `"text":"match"`)
}

func TestReadToolsReactions(t *testing.T) {
	for _, tc := range []struct {
		tool string
		path string
	}{
		{tool: "slack_read_thread", path: "/conversations.replies"},
		{tool: "slack_read_history", path: "/conversations.history"},
	} {
		t.Run(tc.tool, func(t *testing.T) {
			a := apiAdapter(t, func(w http.ResponseWriter, r *http.Request) {
				require.Equal(t, tc.path, r.URL.Path)
				_, _ = w.Write([]byte(`{"ok":true,"messages":[
					{"text":"reacted","ts":"123.000001","reactions":[{"name":"white_check_mark","count":3,"users":["UMEMBER"]},{"name":"eyes","count":1,"users":["UMEMBER"]}]},
					{"text":"unreacted","ts":"124.000001"},
					{"text":"empty reactions","ts":"125.000001","reactions":[]}
				]}`))
			}, false)
			result := invoke(t, a, tc.tool, map[string]string{"channel": "CTEST", "thread_ts": "123.000001"})
			require.JSONEq(t, `{"has_more":false,"next_cursor":"","messages":[
				{"text":"reacted","ts":"123.000001","reactions":[{"name":"white_check_mark","count":3},{"name":"eyes","count":1}]},
				{"text":"unreacted","ts":"124.000001"},
				{"text":"empty reactions","ts":"125.000001"}
			]}`, string(result))
		})
	}
}

func TestDownloadRawBytes(t *testing.T) {
	body := []byte{0, 255, 0x89, 'P', 'N', 'G', '\r', '\n'}
	fail := false
	a := apiAdapter(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/files.info":
			require.NoError(t, r.ParseForm())
			require.Equal(t, "xoxp-test", r.Form.Get("token"))
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "file": map[string]string{"id": "FTEST", "url_private_download": "http://" + r.Host + "/download"}})
		case "/download":
			require.Equal(t, "Bearer xoxp-test", r.Header.Get("Authorization"))
			if fail {
				w.WriteHeader(403)
				return
			}
			_, _ = w.Write(body)
		default:
			t.Errorf("unexpected API %s", r.URL.Path)
		}
	}, true)
	path := filepath.Join(t.TempDir(), "raw.png")
	invoke(t, a, "slack_download_file", map[string]string{"file_id": "FTEST", "path": path})
	got, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, body, got)
	_, err = a.download(t.Context(), toolInput{FileID: "FTEST", Path: path})
	require.ErrorIs(t, err, os.ErrExist)
	t.Run("missing parent directories", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "a", "b", "file")
		invoke(t, a, "slack_download_file", map[string]string{"file_id": "FTEST", "path": path})
		got, err := os.ReadFile(path)
		require.NoError(t, err)
		require.Equal(t, body, got)
		for _, parent := range []string{filepath.Join(dir, "a"), filepath.Dir(path)} {
			info, err := os.Stat(parent)
			require.NoError(t, err)
			require.True(t, info.IsDir())
			require.Equal(t, os.FileMode(0o700), info.Mode().Perm())
		}
	})
	fail = true
	path = filepath.Join(t.TempDir(), "denied.png")
	_, err = a.download(t.Context(), toolInput{FileID: "FTEST", Path: path})
	require.Error(t, err)
	_, err = os.Stat(path)
	require.ErrorIs(t, err, os.ErrNotExist)
}

func TestUploadBroadcastReactionAndHistory(t *testing.T) {
	body := []byte{0, 255, 'p', 'i', 'c'}
	path := filepath.Join(t.TempDir(), "image.png")
	require.NoError(t, os.WriteFile(path, body, 0o600))
	missingInfo := false
	a := apiAdapter(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/upload" {
			require.Equal(t, "Bearer xoxb-test", r.Header.Get("Authorization"))
			file, _, err := r.FormFile("file")
			require.NoError(t, err)
			defer file.Close()
			got, err := io.ReadAll(file)
			require.NoError(t, err)
			require.Equal(t, body, got)
			_, _ = w.Write([]byte(`ok`))
			return
		}
		require.NoError(t, r.ParseForm())
		if r.URL.Path != "/files.info" {
			require.Equal(t, "xoxb-test", r.Form.Get("token"))
		}
		switch r.URL.Path {
		case "/files.getUploadURLExternal":
			require.Equal(t, "5", r.Form.Get("length"))
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "file_id": "FTEST", "upload_url": "http://" + r.Host + "/upload"})
		case "/files.completeUploadExternal":
			require.Equal(t, "CTEST", r.Form.Get("channel_id"))
			require.Equal(t, "123.000001", r.Form.Get("thread_ts"))
			require.Equal(t, "*photo*", r.Form.Get("initial_comment"))
			require.Empty(t, r.Form.Get("reply_broadcast"))
			_, _ = w.Write([]byte(`{"ok":true,"files":[{"id":"FTEST","title":"Photo"}]}`))
		case "/files.info":
			require.Equal(t, "xoxp-test", r.Form.Get("token"))
			if missingInfo {
				_, _ = w.Write([]byte(`{"ok":false,"error":"file_not_found"}`))
				return
			}
			_, _ = w.Write([]byte(`{"ok":true,"file":{"id":"FTEST","permalink":"https://example.test/file"}}`))
		case "/chat.postMessage":
			require.Equal(t, "true", r.Form.Get("reply_broadcast"))
			require.Equal(t, "123.000001", r.Form.Get("thread_ts"))
			require.Equal(t, "<https://example.test/file|Photo>", r.Form.Get("text"))
			_, _ = w.Write([]byte(`{"ok":true,"ts":"124.000001"}`))
		case "/reactions.add":
			require.Equal(t, "CTEST", r.Form.Get("channel"))
			require.Equal(t, "eyes", r.Form.Get("name"))
			_, _ = w.Write([]byte(`{"ok":true}`))
		default:
			t.Errorf("unexpected API %s", r.URL.Path)
		}
	}, true)
	db, err := repository.New(t.Context(), repository.Config{Path: filepath.Join(t.TempDir(), "history.db")})
	require.NoError(t, err)
	defer db.Close()
	a.history = repository.NewEventRepository(db)
	args := map[string]string{"channel": "CTEST", "thread_ts": "123.000001", "path": path, "text": "**photo**"}
	result := invoke(t, a, "slack_upload_file", args)
	require.Contains(t, string(result), `"permalink":"https://example.test/file"`)
	invoke(t, a, "slack_send", map[string]any{"channel": "CTEST", "thread_ts": "123.000001", "text": "[Photo](https://example.test/file)", "broadcast": true})
	invoke(t, a, "slack_react", map[string]string{"channel": "CTEST", "timestamp": "124.000001", "emoji": ":eyes:"})
	sends, err := a.history.ListOutbound(t.Context(), Conversation("CTEST", "123.000001"), 10)
	require.NoError(t, err)
	require.Len(t, sends, 2)
	require.Equal(t, "FTEST", sends[1].Message.Attachments[0].Ref)
	missingInfo = true
	result = invoke(t, a, "slack_upload_file", args)
	require.Contains(t, string(result), `"warning"`)
	sends, err = a.history.ListOutbound(t.Context(), Conversation("CTEST", "123.000001"), 10)
	require.NoError(t, err)
	require.Len(t, sends, 3)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	require.NoError(t, a.record(ctx, "already-posted", Conversation("CTEST", ""), domain.OutboundMessage{Text: "posted"}))
}

func TestHistoryTokenFallbackAndErrors(t *testing.T) {
	for _, method := range []string{"slack_read_history", "slack_read_thread"} {
		for _, code := range []string{"", "not_in_channel", "channel_not_found", "missing_scope", "invalid_auth"} {
			t.Run(method+"/"+code, func(t *testing.T) {
				var tokens []string
				a := apiAdapter(t, func(w http.ResponseWriter, r *http.Request) {
					require.NoError(t, r.ParseForm())
					token := r.Form.Get("token")
					tokens = append(tokens, token)
					if token == "xoxb-test" && code != "" {
						_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": code})
						return
					}
					_, _ = w.Write([]byte(`{"ok":true,"messages":[]}`))
				}, true)
				for _, tool := range a.Tools() {
					if tool.Name == method {
						_, err := tool.Handler(t.Context(), json.RawMessage(`{"channel":"DTEST","thread_ts":"123.000001"}`))
						if code == "missing_scope" || code == "invalid_auth" {
							require.ErrorContains(t, err, code)
						} else {
							require.NoError(t, err)
						}
					}
				}
				expected := []string{"xoxb-test"}
				if code == "not_in_channel" || code == "channel_not_found" {
					expected = append(expected, "xoxp-test")
				}
				require.Equal(t, expected, tokens)
			})
		}
	}
}
