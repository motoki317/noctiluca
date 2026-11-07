package slack

import (
	"bytes"
	"context"
	"encoding/json"
	"image"
	"image/gif"
	"image/jpeg"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	sdk "github.com/slack-go/slack"
	"github.com/stretchr/testify/require"

	"github.com/motoki317/noctiluca/pkg/domain"
	"github.com/motoki317/noctiluca/pkg/infrastructure/repository"
)

type uploadTransport http.HandlerFunc

func (handler uploadTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if err := r.Context().Err(); err != nil {
		return nil, err
	}
	w := httptest.NewRecorder()
	handler(w, r)
	return w.Result(), nil
}

func TestBroadcastUpload(t *testing.T) {
	for _, tc := range []struct {
		name        string
		extension   string
		text        string
		wantText    string
		filename    string
		failures    []string
		cancelAfter time.Duration
		wantError   string
		wantPosts   int
		wantWait    time.Duration
	}{
		{name: "png", extension: "png", text: "**photo**", wantText: "*photo*", wantPosts: 1},
		{name: "jpg", extension: "jpg", text: "**photo**", wantText: "*photo*", wantPosts: 1},
		{name: "jpeg", extension: "jpeg", text: "**photo**", wantText: "*photo*", wantPosts: 1},
		{name: "gif without caption", extension: "gif", wantPosts: 1},
		{name: "literal filename", extension: "png", filename: "<!channel>&.png", wantText: "&lt;!channel&gt;&amp;.png", wantPosts: 1},
		{name: "caption at limit", extension: "png", text: strings.Repeat("界", 3000), wantText: strings.Repeat("界", 3000), wantPosts: 1},
		{name: "processing", extension: "png", text: "**photo**", wantText: "*photo*", failures: []string{"invalid_blocks"}, wantPosts: 2, wantWait: 2 * time.Second},
		{name: "processing exhausted", extension: "png", failures: []string{"invalid_blocks", "invalid_blocks", "invalid_blocks", "invalid_blocks", "invalid_blocks", "invalid_blocks"}, wantError: "invalid_blocks", wantPosts: 6, wantWait: 10 * time.Second},
		{name: "permission denied", extension: "png", failures: []string{"not_in_channel"}, wantError: "not_in_channel", wantPosts: 1},
		{name: "ambiguous server failure", extension: "png", failures: []string{"http_error"}, wantError: "broadcast Slack image failed", wantPosts: 1},
		{name: "canceled during backoff", extension: "png", failures: []string{"invalid_blocks"}, cancelAfter: time.Second, wantError: "context deadline exceeded", wantPosts: 1, wantWait: time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				var body bytes.Buffer
				picture := image.NewRGBA(image.Rect(0, 0, 1, 1))
				switch tc.extension {
				case "jpg", "jpeg":
					require.NoError(t, jpeg.Encode(&body, picture, nil))
				case "gif":
					require.NoError(t, gif.Encode(&body, picture, nil))
				default:
					require.NoError(t, png.Encode(&body, picture))
				}
				filename := "photo." + tc.extension
				if tc.filename != "" {
					filename = tc.filename
				}
				path := filepath.Join(t.TempDir(), filename)
				require.NoError(t, os.WriteFile(path, body.Bytes(), 0o600))
				var calls []string
				posts := 0
				handler := func(w http.ResponseWriter, r *http.Request) {
					calls = append(calls, r.URL.Path)
					if r.URL.Path == "/upload" {
						require.Equal(t, "Bearer xoxb-test", r.Header.Get("Authorization"))
						file, _, err := r.FormFile("file")
						require.NoError(t, err)
						defer file.Close()
						got, err := io.ReadAll(file)
						require.NoError(t, err)
						require.Equal(t, body.Bytes(), got)
						_, _ = w.Write([]byte("ok"))
						return
					}
					require.NoError(t, r.ParseForm())
					require.Equal(t, "xoxb-test", r.Form.Get("token"))
					switch r.URL.Path {
					case "/files.getUploadURLExternal":
						require.Equal(t, filename, r.Form.Get("filename"))
						require.Equal(t, strconv.Itoa(body.Len()), r.Form.Get("length"))
						_, _ = w.Write([]byte(`{"ok":true,"file_id":"FTEST","upload_url":"https://example.test/upload"}`))
					case "/files.completeUploadExternal":
						for _, field := range []string{"channel_id", "channels", "thread_ts", "initial_comment", "blocks", "reply_broadcast"} {
							require.NotContains(t, r.Form, field)
						}
						require.JSONEq(t, `[{"id":"FTEST","title":"Photo"}]`, r.Form.Get("files"))
						_, _ = w.Write([]byte(`{"ok":true,"files":[{"id":"FTEST","title":"Photo"}]}`))
					case "/chat.postMessage":
						posts++
						require.Equal(t, "CTEST", r.Form.Get("channel"))
						require.Equal(t, "123.000001", r.Form.Get("thread_ts"))
						require.Equal(t, "true", r.Form.Get("reply_broadcast"))
						var blocks []map[string]any
						if tc.text != "" {
							blocks = append(blocks, map[string]any{"type": "section", "text": map[string]any{"type": "mrkdwn", "text": tc.wantText}})
						}
						if tc.wantText == "" {
							require.Equal(t, filename, r.Form.Get("text"))
						} else {
							require.Equal(t, tc.wantText, r.Form.Get("text"))
						}
						blocks = append(blocks, map[string]any{"type": "image", "slack_file": map[string]string{"id": "FTEST"}, "alt_text": filename})
						want, err := json.Marshal(blocks)
						require.NoError(t, err)
						require.JSONEq(t, string(want), r.Form.Get("blocks"))
						if posts <= len(tc.failures) {
							if tc.failures[posts-1] == "http_error" {
								w.WriteHeader(http.StatusInternalServerError)
							} else {
								_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": tc.failures[posts-1]})
							}
							return
						}
						_, _ = w.Write([]byte(`{"ok":true,"ts":"124.000001"}`))
					case "/files.info":
						require.Equal(t, "FTEST", r.Form.Get("file"))
						_, _ = w.Write([]byte(`{"ok":true,"file":{"id":"FTEST","permalink":"https://example.test/file"}}`))
					default:
						t.Fatalf("unexpected API %s", r.URL.Path)
					}
				}
				db, err := repository.New(t.Context(), repository.Config{Path: filepath.Join(t.TempDir(), "history.db")})
				require.NoError(t, err)
				defer db.Close()
				history := repository.NewEventRepository(db)
				a, err := newAdapter("slack", Config{AppToken: "xapp-test", BotToken: "xoxb-test", UserToken: "xoxp-test"}, history, sdk.OptionAPIURL("https://example.test/"), sdk.OptionHTTPClient(&http.Client{Transport: uploadTransport(handler)}))
				require.NoError(t, err)
				ctx := t.Context()
				if tc.cancelAfter != 0 {
					var cancel context.CancelFunc
					ctx, cancel = context.WithTimeout(ctx, tc.cancelAfter)
					defer cancel()
				}
				var upload domain.Tool
				for _, tool := range a.Tools() {
					if tool.Name == "slack_upload_file" {
						upload = tool
					}
				}
				require.NotNil(t, upload.Handler)
				require.Contains(t, string(upload.InputSchema), `"broadcast":{"type":"boolean"}`)
				args, err := json.Marshal(map[string]any{"channel": "CTEST", "thread_ts": "123.000001", "path": path, "title": "Photo", "text": tc.text, "broadcast": true})
				require.NoError(t, err)
				start := time.Now()
				result, err := upload.Handler(ctx, args)
				require.Equal(t, tc.wantWait, time.Since(start))
				require.Equal(t, tc.wantPosts, posts)
				expectedCalls := []string{"/files.getUploadURLExternal", "/upload", "/files.completeUploadExternal"}
				for range tc.wantPosts {
					expectedCalls = append(expectedCalls, "/chat.postMessage")
				}
				sent, historyErr := history.ListOutbound(t.Context(), Conversation("slack", "CTEST", "123.000001"), 10)
				require.NoError(t, historyErr)
				if tc.wantError != "" {
					require.ErrorContains(t, err, tc.wantError)
					require.Empty(t, sent)
				} else {
					require.NoError(t, err)
					require.JSONEq(t, `{"id":"FTEST","permalink":"https://example.test/file"}`, string(result))
					expectedCalls = append(expectedCalls, "/files.info")
					require.Len(t, sent, 1)
					require.Equal(t, "124.000001", sent[0].ID)
					require.Equal(t, domain.OutboundMessage{Text: tc.text, Attachments: []domain.Attachment{{Ref: "FTEST", Name: filename}}}, sent[0].Message)
				}
				require.Equal(t, expectedCalls, calls)
			})
		})
	}
}

func TestBroadcastUploadRejectsInvalidInput(t *testing.T) {
	for _, tc := range []struct {
		name   string
		thread string
		body   []byte
		text   string
		want   string
	}{
		{name: "missing thread", body: []byte("\x89PNG\r\n\x1a\n"), want: "broadcast requires thread_ts"},
		{name: "text named png", thread: "123.000001", body: []byte("not an image"), want: "PNG, JPEG, or GIF"},
		{name: "unsupported webp", thread: "123.000001", body: []byte("RIFF1234WEBPVP8 "), want: "PNG, JPEG, or GIF"},
		{name: "empty file", thread: "123.000001", want: "nonempty regular file"},
		{name: "caption over limit", thread: "123.000001", body: []byte("\x89PNG\r\n\x1a\n"), text: strings.Repeat("界", 3001), want: "caption must be at most 3000 characters"},
		{name: "caption expands over limit", thread: "123.000001", body: []byte("\x89PNG\r\n\x1a\n"), text: strings.Repeat("&", 601), want: "caption must be at most 3000 characters"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "image.png")
			require.NoError(t, os.WriteFile(path, tc.body, 0o600))
			a := apiAdapter(t, func(w http.ResponseWriter, r *http.Request) {
				t.Errorf("invalid input called Slack API %s", r.URL.Path)
				w.WriteHeader(http.StatusBadRequest)
			}, true)
			_, err := a.upload(t.Context(), toolInput{Channel: "CTEST", Thread: tc.thread, Path: path, Text: tc.text, Broadcast: true})
			require.ErrorContains(t, err, tc.want)
		})
	}
}
