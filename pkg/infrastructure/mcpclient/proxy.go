package mcpclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"golang.org/x/oauth2"
	"golang.org/x/sync/errgroup"

	"github.com/motoki317/noctiluca/pkg/domain"
	"github.com/motoki317/noctiluca/pkg/util/lifecycle"
)

type storedAuth struct{ source oauth2.TokenSource }

func (a storedAuth) TokenSource(context.Context) (oauth2.TokenSource, error) { return a.source, nil }
func (a storedAuth) Authorize(_ context.Context, _ *http.Request, response *http.Response) error {
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
	return errors.New("MCP authorization failed; run mcp-login again")
}

type manager struct {
	life       lifecycle.Group
	configs    []ServerConfig
	repository domain.OAuthTokenRepository
	hub        domain.ToolHub
	logger     *slog.Logger
	remotes    []*remote
}

type remote struct {
	config    ServerConfig
	hub       domain.ToolHub
	logger    *slog.Logger
	client    *mcp.Client
	tokens    oauth2.TokenSource
	session   *mcp.ClientSession
	stopClose func() bool
	changed   chan struct{}
}

func New(configs []ServerConfig, repository domain.OAuthTokenRepository, hub domain.ToolHub, logger *slog.Logger) (domain.ExternalTools, error) {
	if err := Validate(configs); err != nil {
		return nil, err
	}
	if repository == nil || hub == nil {
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
	return &manager{configs: configs, repository: repository, hub: hub, logger: logger}, nil
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
		r := &remote{config: config, hub: m.hub, logger: m.logger, tokens: tokens, changed: make(chan struct{}, 1)}
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
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	session, err := r.client.Connect(connectCtx, &mcp.StreamableClientTransport{Endpoint: r.config.URL, HTTPClient: client, OAuthHandler: storedAuth{r.tokens}}, nil)
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
		tools = append(tools, domain.Tool{Name: name, Description: tool.Description, InputSchema: schema, OwnerOnly: !r.config.Members, ReadOnly: tool.Annotations != nil && tool.Annotations.ReadOnlyHint, TextResult: true, Handler: func(ctx context.Context, input json.RawMessage) (json.RawMessage, error) {
			result, err := session.CallTool(ctx, &mcp.CallToolParams{Name: tool.Name, Arguments: input})
			// Calls can have side effects. Reconnection only affects subsequent calls.
			if err != nil {
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
	defer func() {
		if r.session != nil {
			r.stopClose()
			_ = r.session.Close()
		}
	}()
	delay := time.Second
	for ctx.Err() == nil {
		if r.session == nil {
			if err := r.connect(ctx); err != nil {
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
		_ = session.Close()
		r.stopClose()
		r.session = nil
		if ctx.Err() != nil {
			return nil
		}
		if err := r.hub.ReplaceSource("external:"+r.config.Name, nil); err != nil {
			return err
		}
	}
	return nil
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
		if r.session != nil {
			r.stopClose()
			result = errors.Join(result, r.session.Close())
		}
	}
	return result
}
