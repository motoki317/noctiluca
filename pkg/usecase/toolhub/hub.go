package toolhub

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/motoki317/noctiluca/pkg/domain"
)

type Config struct {
	Port int
}

type grant struct {
	session domain.ToolSession
	token   string
	ctx     context.Context
	cancel  context.CancelFunc
}

type grantKey struct{}

type hub struct {
	mu              sync.Mutex
	server          *mcp.Server
	http            *http.Server
	listener        net.Listener
	sources         map[string]struct{}
	tools           map[string]string
	sessions        map[domain.SessionKey]*grant
	tokens          map[string]*grant
	started, closed bool
}

// New binds the loopback listener so OpenSession can return the actual port before Start.
func New(config Config) (domain.ToolHub, error) {
	listener, err := net.Listen("tcp4", fmt.Sprintf("127.0.0.1:%d", config.Port))
	if err != nil {
		return nil, fmt.Errorf("listen for tool hub: %w", err)
	}
	h := &hub{listener: listener, server: mcp.NewServer(&mcp.Implementation{Name: "noctiluca", Version: "0.1.0"}, nil), sources: make(map[string]struct{}), tools: make(map[string]string), sessions: make(map[domain.SessionKey]*grant), tokens: make(map[string]*grant)}
	// The bearer identifies the model session. Transport connections need no separate lifetime.
	transport := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return h.server }, &mcp.StreamableHTTPOptions{Stateless: true, JSONResponse: true, PropagateRequestCancellation: true})
	h.http = &http.Server{Handler: h.authorize(http.NewCrossOriginProtection().Handler(transport)), ReadHeaderTimeout: 10 * time.Second}
	return h, nil
}

// Start blocks until Shutdown or cancellation, as other services do under an errgroup.
func (h *hub) Start(ctx context.Context) error {
	h.mu.Lock()
	// A shutdown that wins the race with Start leaves nothing to serve; that is not an error.
	if h.closed {
		h.mu.Unlock()
		return nil
	}
	if h.started {
		h.mu.Unlock()
		return errors.New("tool hub is already started")
	}
	h.started = true
	h.mu.Unlock()
	shutdownDone := make(chan struct{})
	var shutdownErr error
	stop := context.AfterFunc(ctx, func() {
		defer close(shutdownDone)
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		shutdownErr = h.Shutdown(shutdownCtx)
	})
	err := h.http.Serve(h.listener)
	if !stop() {
		<-shutdownDone
		if shutdownErr != nil {
			return shutdownErr
		}
	}
	if errors.Is(err, http.ErrServerClosed) || errors.Is(err, net.ErrClosed) {
		return nil
	}
	return err
}

func (h *hub) Shutdown(ctx context.Context) error {
	h.mu.Lock()
	h.closed = true
	for key, g := range h.sessions {
		g.cancel()
		delete(h.tokens, g.token)
		delete(h.sessions, key)
	}
	h.mu.Unlock()
	// Shutdown owns active listeners. Close ours afterward if Start was never called.
	defer h.listener.Close()
	if err := h.http.Shutdown(ctx); err != nil {
		_ = h.http.Close()
		return err
	}
	return nil
}

func (h *hub) OpenSession(key domain.SessionKey) (domain.MCPEndpoint, error) {
	if key == "" {
		return domain.MCPEndpoint{}, errors.New("tool session key is required")
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return domain.MCPEndpoint{}, errors.New("tool hub is closed")
	}
	if _, ok := h.sessions[key]; ok {
		return domain.MCPEndpoint{}, errors.New("tool session already open")
	}
	var secret [32]byte
	if _, err := rand.Read(secret[:]); err != nil {
		return domain.MCPEndpoint{}, fmt.Errorf("generate tool session token: %w", err)
	}
	token := base64.RawURLEncoding.EncodeToString(secret[:])
	ctx, cancel := context.WithCancel(context.Background())
	g := &grant{session: domain.ToolSession{Key: key}, token: token, ctx: ctx, cancel: cancel}
	h.sessions[key], h.tokens[token] = g, g
	return domain.MCPEndpoint{URL: "http://" + h.listener.Addr().String() + "/mcp", BearerToken: token}, nil
}

// CloseSession revokes access independently of model turns and transport connections.
func (h *hub) CloseSession(key domain.SessionKey) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if g, ok := h.sessions[key]; ok {
		delete(h.sessions, key)
		delete(h.tokens, g.token)
		g.cancel()
	}
}

func (h *hub) authorize(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/mcp" {
			http.NotFound(w, r)
			return
		}
		headers := r.Header.Values("Authorization")
		var token string
		if len(headers) == 1 {
			scheme, value, ok := strings.Cut(headers[0], " ")
			if ok && strings.EqualFold(scheme, "Bearer") {
				token = value
			}
		}
		h.mu.Lock()
		g := h.tokens[token]
		h.mu.Unlock()
		if g == nil || g.ctx.Err() != nil {
			w.Header().Set("WWW-Authenticate", `Bearer realm="noctiluca"`)
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}
		ctx := context.WithValue(r.Context(), grantKey{}, g)
		ctx = domain.WithToolSession(ctx, g.session)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}
