package webhook

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"testing/iotest"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/motoki317/noctiluca/pkg/domain"
	"github.com/motoki317/noctiluca/pkg/infrastructure/repository"
)

const testToken = "test-event-token"

func newTestServer(t *testing.T, inbox domain.InboxRepository, notify func()) *server {
	t.Helper()
	background, err := New("127.0.0.1:0", testToken, inbox, notify)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, background.Shutdown(context.Background())) })
	return background.(*server)
}

func TestListenerBindsBeforeStart(t *testing.T) {
	s := newTestServer(t, nil, nil)
	addr := s.listener.Addr().String()
	_, err := New(addr, testToken, nil, nil)
	require.ErrorContains(t, err, "listen for events (events.listen)")
	require.ErrorIs(t, err, syscall.EADDRINUSE)

	require.NoError(t, s.Shutdown(t.Context()))
	listener, err := net.Listen("tcp4", addr)
	require.NoError(t, err)
	require.NoError(t, listener.Close())
}

func TestStalledUploadDoesNotBlockShutdown(t *testing.T) {
	inbox := newInbox(t)
	s := newTestServer(t, inbox, func() { t.Error("stalled upload triggered notification") })
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- s.Start(ctx) }()

	addr := s.listener.Addr().String()
	conn, err := net.DialTimeout("tcp4", addr, time.Second)
	require.NoError(t, err)
	defer conn.Close()
	require.NoError(t, conn.SetDeadline(time.Now().Add(5*time.Second)))
	_, err = fmt.Fprintf(conn, "POST /events/gmail HTTP/1.1\r\nHost: %s\r\nAuthorization: Bearer %s\r\nContent-Length: 4\r\nExpect: 100-continue\r\n\r\n", addr, testToken)
	require.NoError(t, err)
	reader := bufio.NewReader(conn)
	response, err := http.ReadResponse(reader, nil)
	require.NoError(t, err)
	require.Equal(t, http.StatusContinue, response.StatusCode)
	require.NoError(t, response.Body.Close())

	cancel()
	response, err = http.ReadResponse(reader, nil)
	require.NoError(t, err)
	require.Equal(t, http.StatusRequestTimeout, response.StatusCode)
	require.NoError(t, response.Body.Close())
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("server did not stop")
	}
	rows, err := inbox.Active(t.Context())
	require.NoError(t, err)
	require.Empty(t, rows)
}

func newInbox(t *testing.T) domain.InboxRepository {
	t.Helper()
	db, err := repository.New(t.Context(), repository.Config{Path: filepath.Join(t.TempDir(), "events.db")})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	return repository.NewInboxRepository(db)
}

func post(name string, body io.Reader) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/events/"+name, body)
	r.Header.Set("Authorization", "Bearer "+testToken)
	return r
}

func TestRejectEvent(t *testing.T) {
	inbox := newInbox(t)
	s := newTestServer(t, inbox, func() { t.Error("rejected event triggered notification") })
	for _, tc := range []struct {
		name, method, path string
		auth               []string
		status             int
	}{
		{"no auth before name check", "POST", "/events/Gmail", nil, http.StatusUnauthorized},
		{"wrong token", "POST", "/events/gmail", []string{"Bearer wrong-token"}, http.StatusUnauthorized},
		{"duplicate auth", "POST", "/events/gmail", []string{"Bearer " + testToken, "Bearer " + testToken}, http.StatusUnauthorized},
		{"combined auth", "POST", "/events/gmail", []string{"Bearer " + testToken + ", Bearer " + testToken}, http.StatusUnauthorized},
		{"wrong scheme", "POST", "/events/gmail", []string{"Basic " + testToken}, http.StatusUnauthorized},
		{"uppercase name", "POST", "/events/Gmail", []string{"Bearer " + testToken}, http.StatusBadRequest},
		{"numeric prefix", "POST", "/events/1x", []string{"Bearer " + testToken}, http.StatusBadRequest},
		{"hyphenated name", "POST", "/events/a-b", []string{"Bearer " + testToken}, http.StatusBadRequest},
		{"GET", "GET", "/events/gmail", nil, http.StatusMethodNotAllowed},
		{"unmatched path", "POST", "/other", nil, http.StatusNotFound},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := strings.NewReader("unread body")
			r := httptest.NewRequest(tc.method, tc.path, body)
			r.Header["Authorization"] = tc.auth
			w := httptest.NewRecorder()
			s.http.Handler.ServeHTTP(w, r)
			require.Equal(t, tc.status, w.Code)
			if tc.status == http.StatusUnauthorized {
				require.Equal(t, `Bearer realm="noctiluca"`, w.Header().Get("WWW-Authenticate"))
			}
			if tc.status == http.StatusBadRequest {
				require.Equal(t, "event name must start with a lowercase letter and hold only a-z, 0-9, and _\n", w.Body.String())
			}
			require.NotContains(t, w.Body.String(), testToken)
			require.Equal(t, body.Size(), int64(body.Len()))
			rows, err := inbox.Active(t.Context())
			require.NoError(t, err)
			require.Empty(t, rows)
		})
	}
}

func TestAcceptEventAndDeduplicate(t *testing.T) {
	inbox := newInbox(t)
	var notifications int
	var response *httptest.ResponseRecorder
	s := newTestServer(t, inbox, func() {
		notifications++
		rows, err := inbox.Active(t.Context())
		require.NoError(t, err)
		require.Len(t, rows, notifications)
		require.Equal(t, http.StatusAccepted, response.Code)
	})
	const body = "\n A body with 日本語.\r\n "
	for _, tc := range []struct {
		name, key string
		setKey    bool
		wantRows  int
	}{
		{"gmail", "one", true, 1},
		{"gmail", "one", true, 1},
		{"calendar", "one", true, 2},
		{"gmail", "", false, 3},
		{"gmail", "", false, 4},
		{"gmail", "", true, 5},
		{"gmail", "", true, 6},
	} {
		sent := body
		duplicate := tc.wantRows == notifications
		if duplicate {
			sent = "duplicate body"
		}
		r := post(tc.name, strings.NewReader(sent))
		r.Header.Set("Content-Type", "application/octet-stream")
		if tc.setKey {
			r.Header.Set("Idempotency-Key", tc.key)
		}
		response = httptest.NewRecorder()
		start := time.Now()
		s.http.Handler.ServeHTTP(response, r)
		require.Equal(t, http.StatusAccepted, response.Code)
		require.Empty(t, response.Body.String())
		rows, err := inbox.Active(t.Context())
		require.NoError(t, err)
		require.Len(t, rows, tc.wantRows)
		require.Equal(t, tc.wantRows, notifications)
		row := rows[len(rows)-1]
		require.Equal(t, domain.MainSessionKey, row.SessionKey)
		require.Equal(t, domain.LaneConversation, row.Lane)
		require.Equal(t, domain.InboxPayload{Input: domain.UserInput{Text: body}, External: &domain.ExternalEvent{Name: tc.name}}, row.Payload)
		if !duplicate {
			require.WithinRange(t, row.ReadyAt, start, time.Now())
		}
		if tc.key != "" {
			require.Equal(t, "events/"+tc.name+"/"+tc.key, row.SourceKey)
		} else {
			key, ok := strings.CutPrefix(row.SourceKey, "events/"+tc.name+"/")
			require.True(t, ok)
			require.NoError(t, uuid.Validate(key))
		}
	}
}

func TestEventSourceKeyDoesNotCollideWithMessaging(t *testing.T) {
	inbox := newInbox(t)
	_, err := inbox.Enqueue(t.Context(), domain.InboxItem{
		SourceKey:  "event:m:600",
		SessionKey: domain.MainSessionKey,
		Lane:       domain.LaneConversation,
		ReadyAt:    time.Unix(1, 0),
		Payload:    domain.InboxPayload{Input: domain.UserInput{Text: "chat message"}},
	})
	require.NoError(t, err)

	s := newTestServer(t, inbox, func() {})
	r := post("m", strings.NewReader("local event"))
	r.Header.Set("Idempotency-Key", "600")
	w := httptest.NewRecorder()
	s.http.Handler.ServeHTTP(w, r)
	require.Equal(t, http.StatusAccepted, w.Code)

	rows, err := inbox.Active(t.Context())
	require.NoError(t, err)
	require.Len(t, rows, 2)
	require.Equal(t, "event:m:600", rows[0].SourceKey)
	require.Equal(t, "chat message", rows[0].Payload.Input.Text)
	require.Equal(t, "events/m/600", rows[1].SourceKey)
	require.Equal(t, "local event", rows[1].Payload.Input.Text)
}

func TestEventBodyLimit(t *testing.T) {
	const limit = 16384
	for _, tc := range []struct {
		name      string
		body      string
		want      string
		truncated bool
	}{
		{"empty", "", "", false},
		{"at limit", strings.Repeat("x", limit), strings.Repeat("x", limit), false},
		{"cut inside rune", strings.Repeat("x", limit-1) + "日", strings.Repeat("x", limit-1), true},
		{"cut after rune", strings.Repeat("x", limit-3) + "日" + strings.Repeat("x", 512*1024), strings.Repeat("x", limit-3) + "日", true},
		{"one oversized invalid run", strings.Repeat("\xff", limit+1), "�", false},
		{"valid suffix after long invalid run", strings.Repeat("\xff", limit+1) + "tail", "�tail", false},
		{"replacement expands past limit", strings.Repeat("\xffx", limit/4+1), strings.Repeat("�x", limit/4), true},
		{"literal replacement between invalid runs", "\xff�\xff", "���", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			inbox := newInbox(t)
			s := newTestServer(t, inbox, func() {})
			body := strings.NewReader(tc.body)
			w := httptest.NewRecorder()
			s.http.Handler.ServeHTTP(w, post("gmail", body))
			require.Equal(t, http.StatusAccepted, w.Code)
			require.Zero(t, body.Len())
			rows, err := inbox.Active(t.Context())
			require.NoError(t, err)
			require.Len(t, rows, 1)
			require.Equal(t, tc.want, rows[0].Payload.Input.Text)
			require.True(t, utf8.ValidString(rows[0].Payload.Input.Text))
			require.Equal(t, tc.truncated, rows[0].Payload.External.Truncated)
		})
	}
}

func TestEventBodyReadFailure(t *testing.T) {
	readErr := errors.New("read failed")
	for _, tc := range []struct {
		body   io.Reader
		status int
	}{
		{io.MultiReader(strings.NewReader("partial"), iotest.ErrReader(readErr)), http.StatusBadRequest},
		{io.MultiReader(strings.NewReader(strings.Repeat("x", maxBodyBytes+1)), iotest.ErrReader(readErr)), http.StatusBadRequest},
		{io.MultiReader(strings.NewReader(strings.Repeat("x", maxBodyBytes)), iotest.DataErrReader(iotest.TimeoutReader(strings.NewReader("x")))), http.StatusBadRequest},
		{io.MultiReader(strings.NewReader("partial"), iotest.ErrReader(fmt.Errorf("read body: %w", os.ErrDeadlineExceeded))), http.StatusRequestTimeout},
	} {
		inbox := newInbox(t)
		s := newTestServer(t, inbox, func() { t.Error("unread event triggered notification") })
		w := httptest.NewRecorder()
		s.http.Handler.ServeHTTP(w, post("gmail", tc.body))
		require.Equal(t, tc.status, w.Code)
		rows, err := inbox.Active(t.Context())
		require.NoError(t, err)
		require.Empty(t, rows)
	}
}

type failingInbox struct{ domain.InboxRepository }

func (failingInbox) Enqueue(context.Context, domain.InboxItem) (int64, error) {
	return 0, errors.New("save failed")
}

func TestEventSaveFailure(t *testing.T) {
	s := newTestServer(t, failingInbox{}, func() { t.Error("failed save triggered notification") })
	w := httptest.NewRecorder()
	s.http.Handler.ServeHTTP(w, post("gmail", strings.NewReader("body")))
	require.Equal(t, http.StatusInternalServerError, w.Code)
	require.NotContains(t, w.Body.String(), "save failed")
}
