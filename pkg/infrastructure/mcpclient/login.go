package mcpclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/modelcontextprotocol/go-sdk/oauthex"
	"golang.org/x/oauth2"

	"github.com/motoki317/noctiluca/pkg/domain"
)

func Login(ctx context.Context, config ServerConfig, repository domain.OAuthTokenRepository, output io.Writer) error {
	if err := Validate([]ServerConfig{config}); err != nil {
		return err
	}
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return fmt.Errorf("listen for OAuth redirect: %w", err)
	}
	// One callback server serves the whole login: the SDK can ask for authorization more than
	// once (for example after a scope challenge), and a per-request server would close the listener.
	callbacks := serveCallbacks(listener)
	defer callbacks.close()
	redirect := "http://" + listener.Addr().String() + "/callback"
	handler, err := auth.NewAuthorizationCodeHandler(&auth.AuthorizationCodeHandlerConfig{
		RedirectURL:                     redirect,
		DynamicClientRegistrationConfig: &auth.DynamicClientRegistrationConfig{Metadata: &oauthex.ClientRegistrationMetadata{ClientName: "noctiluca", RedirectURIs: []string{redirect}, GrantTypes: []string{"authorization_code", "refresh_token"}, ResponseTypes: []string{"code"}, TokenEndpointAuthMethod: "none", Scope: strings.Join(config.Scopes, " ")}},
		ScopeFilter: func(discovered []string) []string {
			if config.Scopes != nil {
				return append([]string(nil), config.Scopes...)
			}
			return discovered
		},
		// Explicit scopes must match registration. Include offline_access in config if the server requires it.
		Client: oauthHTTPClient(),
		AuthorizationCodeFetcher: func(ctx context.Context, args *auth.AuthorizationArgs) (*auth.AuthorizationResult, error) {
			return callbacks.receive(ctx, args.URL, output)
		},
		NewTokenSource: func(_ context.Context, cfg *oauth2.Config, token *oauth2.Token) (oauth2.TokenSource, error) {
			// Some servers (ClickUp) issue long-lived access tokens without a refresh token. Such a
			// login lasts until the access token expires; the token source then asks for mcp-login.
			reg, err := json.Marshal(registration{Resource: config.URL, OAuth: *cfg})
			if err != nil {
				return nil, err
			}
			refreshCtx := context.WithValue(ctx, oauth2.HTTPClient, oauthHTTPClient())
			source := &tokenSource{ctx: refreshCtx, repository: repository, server: config.Name, registration: reg, source: cfg.TokenSource(refreshCtx, token)}
			_, err = source.Token()
			return source, err
		},
	})
	if err != nil {
		return err
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "noctiluca", Version: "0.1.0"}, nil)
	session, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: config.URL, OAuthHandler: handler, HTTPClient: oauthHTTPClient()}, nil)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		// The cause names the failed OAuth step (registration, consent, token exchange).
		// OAuth error responses carry no credentials, so it is safe to show.
		return fmt.Errorf("MCP login failed: %w", err)
	}
	defer session.Close()
	source, _ := handler.TokenSource(ctx)
	if source == nil {
		return errors.New("MCP server did not request OAuth authorization")
	}
	return nil
}

type callbackResult struct {
	result *auth.AuthorizationResult
	err    error
}

type callbackServer struct {
	server  *http.Server
	serve   chan error
	mu      sync.Mutex
	state   string              // state of the authorization request being waited for
	results chan callbackResult // capacity 1: the browser can return before receive starts waiting
}

func serveCallbacks(listener net.Listener) *callbackServer {
	c := &callbackServer{serve: make(chan error, 1), results: make(chan callbackResult, 1)}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /callback", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		c.mu.Lock()
		valid := c.state != "" && q.Get("state") == c.state
		if valid {
			c.state = ""
		}
		c.mu.Unlock()
		if !valid {
			http.Error(w, "Invalid authorization state", http.StatusBadRequest)
			return
		}
		var result callbackResult
		if q.Get("error") != "" || q.Get("code") == "" {
			result.err = fmt.Errorf("authorization denied: %s", q.Get("error"))
		} else {
			result.result = &auth.AuthorizationResult{Code: q.Get("code"), State: q.Get("state"), Iss: q.Get("iss")}
		}
		c.results <- result
		fmt.Fprintln(w, "Authorization response received. Check the terminal for the login result.")
	})
	c.server = &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() { c.serve <- c.server.Serve(listener) }()
	return c
}

// receive prints the authorization URL and waits for the browser's redirect for that request.
func (c *callbackServer) receive(ctx context.Context, authorizationURL string, output io.Writer) (*auth.AuthorizationResult, error) {
	u, err := url.Parse(authorizationURL)
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	c.state = u.Query().Get("state")
	c.mu.Unlock()
	if _, err := fmt.Fprintln(output, authorizationURL); err != nil {
		return nil, err
	}
	select {
	case r := <-c.results:
		return r.result, r.err
	case err := <-c.serve:
		return nil, err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (c *callbackServer) close() {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if c.server.Shutdown(ctx) != nil {
		_ = c.server.Close()
	}
}
