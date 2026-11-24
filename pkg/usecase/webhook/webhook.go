package webhook

import (
	"bufio"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/motoki317/noctiluca/pkg/domain"
)

const maxBodyBytes = 16 * 1024

type server struct {
	http     *http.Server
	listener net.Listener
	token    [sha256.Size]byte
	inbox    domain.InboxRepository
	notify   func()
}

func New(listen, token string, inbox domain.InboxRepository, notify func()) (domain.Background, error) {
	listener, err := net.Listen("tcp4", listen)
	if err != nil {
		return nil, fmt.Errorf("listen for events (events.listen): %w", err)
	}
	s := &server{listener: listener, token: sha256.Sum256([]byte(token)), inbox: inbox, notify: notify}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /events/{name}", s.receive)
	s.http = &http.Server{
		Handler: mux,
		// Senders post a complete body. A stalled upload must end before the shutdown deadline.
		ReadTimeout: 3 * time.Second,
	}
	return s, nil
}

func (s *server) Start(ctx context.Context) error {
	shutdown := make(chan error, 1)
	stop := context.AfterFunc(ctx, func() {
		stopCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		shutdown <- s.Shutdown(stopCtx)
	})
	err := s.http.Serve(s.listener)
	if errors.Is(err, http.ErrServerClosed) || errors.Is(err, net.ErrClosed) {
		err = nil
	}
	if !stop() {
		err = errors.Join(err, <-shutdown)
	}
	return err
}

func (s *server) Shutdown(ctx context.Context) error {
	// http.Server.Shutdown closes only a listener that Serve registered. This closes ours if Start never ran.
	defer s.listener.Close()
	if err := s.http.Shutdown(ctx); err != nil {
		return errors.Join(err, s.http.Close())
	}
	return nil
}

func (s *server) Tools() []domain.Tool { return nil }

func (s *server) authorized(headers []string) bool {
	if len(headers) != 1 {
		return false
	}
	scheme, value, ok := strings.Cut(headers[0], " ")
	if !ok || value == "" || !strings.EqualFold(scheme, "Bearer") {
		return false
	}
	digest := sha256.Sum256([]byte(value))
	return subtle.ConstantTimeCompare(s.token[:], digest[:]) == 1
}

func (s *server) receive(w http.ResponseWriter, r *http.Request) {
	if !s.authorized(r.Header.Values("Authorization")) {
		w.Header().Set("WWW-Authenticate", `Bearer realm="noctiluca"`)
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	name := r.PathValue("name")
	if !domain.ValidMessagingName(name) {
		http.Error(w, "event name must start with a lowercase letter and hold only a-z, 0-9, and _", http.StatusBadRequest)
		return
	}
	body, truncated, err := readBody(r.Body)
	if err != nil {
		status := http.StatusBadRequest
		var netErr net.Error
		if errors.As(err, &netErr) && netErr.Timeout() {
			status = http.StatusRequestTimeout
		}
		http.Error(w, "Cannot read event body", status)
		return
	}
	key := r.Header.Get("Idempotency-Key")
	if key == "" {
		key = uuid.NewString()
	}
	id, err := s.inbox.Enqueue(r.Context(), domain.InboxItem{
		SourceKey:  "events/" + name + "/" + key,
		SessionKey: domain.MainSessionKey,
		Lane:       domain.LaneConversation,
		ReadyAt:    time.Now(),
		Payload: domain.InboxPayload{
			Input:    domain.UserInput{Text: body},
			External: &domain.ExternalEvent{Name: name, Truncated: truncated},
		},
	})
	if err != nil {
		http.Error(w, "Cannot save event", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusAccepted)
	if id != 0 {
		s.notify()
	}
}

func readBody(body io.Reader) (string, bool, error) {
	reader := bufio.NewReader(body)
	var text strings.Builder
	inInvalidRun := false
	for {
		char, size, err := reader.ReadRune()
		if err == io.EOF {
			return text.String(), false, nil
		}
		if err != nil {
			return "", false, err
		}
		invalid := char == utf8.RuneError && size == 1
		if invalid && inInvalidRun {
			continue
		}
		inInvalidRun = invalid
		if text.Len()+utf8.RuneLen(char) > maxBodyBytes {
			// Hide bufio.Reader.WriteTo from io.Copy: WriteTo skips a read error that ReadRune already buffered.
			_, err := io.Copy(io.Discard, struct{ io.Reader }{reader})
			return text.String(), true, err
		}
		text.WriteRune(char)
	}
}
