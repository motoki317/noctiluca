package mcpclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/modelcontextprotocol/go-sdk/oauthex"
	"golang.org/x/oauth2"
	"golang.org/x/sync/errgroup"

	"github.com/motoki317/noctiluca/pkg/domain"
	"github.com/motoki317/noctiluca/pkg/util/lifecycle"
)

// The SDK exposes no second-401 hook. Keep the retry local to each request so
// concurrent calls cannot clear or inherit another request's authentication failure.
func authorizedTransport(endpoint string, source *tokenSource, base http.RoundTripper) http.RoundTripper {
	origin, _ := url.Parse(endpoint)
	authed := &oauth2.Transport{Source: source, Base: base}
	return roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.URL.Scheme != origin.Scheme || dialAddress(request.URL) != dialAddress(origin) {
			return nil, errors.New("cross-origin MCP redirect")
		}
		for attempt := 0; ; attempt++ {
			response, err := authed.RoundTrip(request)
			if err != nil {
				return nil, err
			}
			if response.StatusCode == http.StatusUnauthorized {
				closeAuthResponse(response)
				if attempt > 0 {
					return nil, source.fail("authorization rejected after refresh (401)")
				}
				if err := source.refreshRejected(response.Request.Header.Get("Authorization")); err != nil {
					return nil, err
				}
				request = request.Clone(request.Context())
				if request.Body != nil {
					if request.GetBody == nil {
						return nil, errors.New("MCP request body cannot be retried")
					}
					request.Body, err = request.GetBody()
					if err != nil {
						return nil, err
					}
				}
				continue
			}
			if response.StatusCode == http.StatusForbidden {
				challenges, _ := oauthex.ParseWWWAuthenticate(response.Header.Values("WWW-Authenticate"))
				for _, challenge := range challenges {
					if challenge.Scheme == "bearer" && challenge.Params["error"] == "insufficient_scope" {
						closeAuthResponse(response)
						return nil, source.fail(fmt.Sprintf("additional authorization required (403 insufficient_scope); check mcp_servers.scopes for %q", challenge.Params["scope"]))
					}
				}
			}
			return response, nil
		}
	})
}

func closeAuthResponse(response *http.Response) {
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
}

type manager struct {
	life       lifecycle.Group
	configs    []ServerConfig
	repository domain.OAuthTokenRepository
	inbox      domain.InboxRepository
	hub        domain.ToolHub
	logger     *slog.Logger
	remotes    []*remote
}

type remote struct {
	config    ServerConfig
	hub       domain.ToolHub
	inbox     domain.InboxRepository
	logger    *slog.Logger
	client    *mcp.Client
	tokens    *tokenSource
	loginID   string
	session   *mcp.ClientSession
	stopClose func() bool
	changed   chan struct{}
}

func New(configs []ServerConfig, repository domain.OAuthTokenRepository, inbox domain.InboxRepository, hub domain.ToolHub, logger *slog.Logger) (domain.ExternalTools, error) {
	if err := Validate(configs); err != nil {
		return nil, err
	}
	if repository == nil || inbox == nil || hub == nil {
		return nil, errors.New("external MCP dependencies are required")
	}
	if logger == nil {
		logger = slog.Default()
	}
	configs = slices.Clone(configs)
	for i := range configs {
		configs[i].Scopes = slices.Clone(configs[i].Scopes)
		configs[i].Tools = slices.Clone(configs[i].Tools)
	}
	return &manager{configs: configs, repository: repository, inbox: inbox, hub: hub, logger: logger}, nil
}

func (m *manager) Connect(ctx context.Context) error {
	ctx, finish, err := m.life.Begin(ctx)
	if err != nil {
		return err
	}
	defer finish()

	for _, config := range m.configs {
		tokens, err := restoreTokens(ctx, config, m.repository)
		if errors.Is(err, domain.ErrNotFound) {
			m.logger.WarnContext(ctx, "Skipping MCP server without login", "server", config.Name, "command", "noctiluca mcp-login "+config.Name)
			continue
		}
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return errors.New("could not load MCP login")
		}
		r := &remote{config: config, hub: m.hub, inbox: m.inbox, logger: m.logger, tokens: tokens, loginID: tokens.registration.LoginID, changed: make(chan struct{}, 1)}
		if expiry := r.tokens.expiresWithoutRefresh(); !expiry.IsZero() && !expiry.After(time.Now().Add(30*24*time.Hour)) {
			m.logger.WarnContext(ctx, "MCP login expires without a refresh token", "server", config.Name, "expiry", expiry.UTC().Format(time.RFC3339), "command", "noctiluca mcp-login "+config.Name)
		}
		r.client = mcp.NewClient(&mcp.Implementation{Name: "noctiluca", Version: "0.1.0"}, &mcp.ClientOptions{KeepAlive: time.Minute, ToolListChangedHandler: func(context.Context, *mcp.ToolListChangedRequest) {
			select {
			case r.changed <- struct{}{}:
			default:
			}
		}})
		if err := m.hub.AddSource("external:"+config.Name, nil); err != nil {
			return err
		}
		m.remotes = append(m.remotes, r)
		if err := r.connect(ctx); err != nil {
			if r.tokens.err() != nil {
				if err := r.disable(ctx); err != nil {
					return err
				}
				continue
			}
			r.logger.WarnContext(ctx, "MCP server connection failed; will retry", "server", config.Name, "error", err)
		}
	}
	return ctx.Err()
}

func (m *manager) Start(ctx context.Context) error {
	ctx, finish, beginErr := m.life.Begin(ctx)
	if beginErr != nil {
		return beginErr
	}
	defer finish()

	g, ctx := errgroup.WithContext(ctx)
	for _, r := range m.remotes {
		g.Go(func() error { return r.run(ctx) })
	}
	if len(m.remotes) == 0 {
		<-ctx.Done()
	}
	return g.Wait()
}

func (r *remote) connect(ctx context.Context) error {
	connectCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	// The SSE connection has no response deadline. Each caller owns its request deadline.
	client := oauthHTTPClient(r.config.URL)
	client.Transport = authorizedTransport(r.config.URL, r.tokens, client.Transport)
	client.Timeout = 0
	session, err := r.client.Connect(connectCtx, &mcp.StreamableClientTransport{Endpoint: r.config.URL, HTTPClient: client}, nil)
	if err != nil {
		return fmt.Errorf("MCP connection failed: %w", err)
	}
	stop := context.AfterFunc(ctx, func() { _ = session.Close() })
	if err := r.register(connectCtx, session); err != nil {
		stop()
		_ = session.Close()
		return err
	}
	r.session = session
	r.stopClose = stop
	return nil
}

func (r *remote) register(ctx context.Context, session *mcp.ClientSession) error {
	var tools []domain.Tool
	for tool, err := range session.Tools(ctx, nil) {
		if err != nil {
			return fmt.Errorf("MCP tool discovery failed: %w", err)
		}
		if r.config.Tools != nil && !slices.Contains(r.config.Tools, tool.Name) {
			continue
		}
		name := r.config.Name + "_" + tool.Name
		if !domain.ValidToolName(name) {
			r.logger.WarnContext(ctx, "Skipping invalid MCP tool name", "server", r.config.Name, "tool", name)
			continue
		}
		schema, err := json.Marshal(tool.InputSchema)
		if err != nil {
			return errors.New("MCP tool schema is invalid")
		}
		tools = append(tools, domain.Tool{Name: name, Description: tool.Description, InputSchema: schema, ReadOnly: tool.Annotations != nil && tool.Annotations.ReadOnlyHint, OnDemand: true, TextResult: true, Handler: func(ctx context.Context, input json.RawMessage) (json.RawMessage, error) {
			if err := r.tokens.err(); err != nil {
				return nil, err
			}
			result, err := session.CallTool(ctx, &mcp.CallToolParams{Name: tool.Name, Arguments: input})
			// Calls can have side effects. Reconnection only affects subsequent calls.
			if err != nil {
				if authErr := r.tokens.err(); authErr != nil {
					return nil, authErr
				}
				r.logger.WarnContext(ctx, "MCP tool call failed", "server", r.config.Name, "tool", name)
				return nil, errors.New("external MCP call failed")
			}
			var parts []string
			for _, content := range result.Content {
				if text, ok := content.(*mcp.TextContent); ok {
					parts = append(parts, text.Text)
				}
			}
			text := strings.Join(parts, "\n")
			if result.IsError {
				return nil, errors.New(text)
			}
			return json.Marshal(text)
		}})
	}
	return r.hub.ReplaceSource("external:"+r.config.Name, tools)
}

func (r *remote) run(ctx context.Context) error {
	defer r.close()
	var expiry <-chan time.Time
	if at := r.tokens.expiresWithoutRefresh(); !at.IsZero() {
		timer := time.NewTimer(time.Until(at))
		defer timer.Stop()
		expiry = timer.C
	}
	delay := time.Second
	for ctx.Err() == nil {
		if r.tokens.err() != nil {
			return r.disable(ctx)
		}
		if r.session == nil {
			if err := r.connect(ctx); err != nil {
				if r.tokens.err() != nil {
					return r.disable(ctx)
				}
				r.logger.WarnContext(ctx, "MCP server connection failed; will retry", "server", r.config.Name, "error", err)
				if !retryAfter(ctx, delay) {
					break
				}
				delay = min(delay*2, 30*time.Second)
				continue
			}
		}
		delay = time.Second
		closed := make(chan struct{})
		session := r.session
		go func() { _ = session.Wait(); close(closed) }()
		connected := true
		for connected {
			select {
			case <-ctx.Done():
				return nil
			case <-r.tokens.failed:
				return r.disable(ctx)
			case <-expiry:
				r.tokens.fail("access token expired without a refresh token")
				return r.disable(ctx)
			case <-closed:
				connected = false
			case <-r.changed:
				listCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
				err := r.register(listCtx, session)
				cancel()
				if err != nil {
					r.logger.WarnContext(ctx, "MCP tool refresh failed", "server", r.config.Name, "error", err)
				}
			}
		}
		_ = r.close()
		if ctx.Err() != nil {
			return nil
		}
		if err := r.hub.ReplaceSource("external:"+r.config.Name, nil); err != nil {
			return err
		}
	}
	return nil
}

func (r *remote) close() error {
	if r.session == nil {
		return nil
	}
	r.stopClose()
	err := r.session.Close()
	r.session = nil
	// A revoked token can also reject the session's DELETE request.
	if r.tokens.err() != nil {
		return nil
	}
	return err
}

func (r *remote) disable(ctx context.Context) error {
	if err := r.hub.ReplaceSource("external:"+r.config.Name, nil); err != nil {
		return err
	}
	reason := r.tokens.err().Error()
	r.logger.WarnContext(ctx, "Skipping MCP server until re-login", "server", r.config.Name, "error", reason)
	key := "mcp-auth:" + r.config.Name + ":" + r.loginID
	_, err := r.inbox.Enqueue(ctx, domain.NewOwnerNotice(key, reason))
	return err
}

func retryAfter(ctx context.Context, delay time.Duration) bool {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-timer.C:
		return true
	case <-ctx.Done():
		return false
	}
}

func (m *manager) Shutdown(ctx context.Context) error {
	if err := m.life.Shutdown(ctx); err != nil {
		return err
	}
	var result error
	for _, r := range m.remotes {
		result = errors.Join(result, r.close())
	}
	return result
}
